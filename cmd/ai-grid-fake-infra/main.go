// Command ai-grid-fake-infra runs the fake infrastructure the real
// ai-grid-provider-backend needs on a laptop: a fake hub API (ManagedClusters
// and SelfSubjectAccessReview) and a fake Keycloak (token endpoint and
// Organizations Admin API), both seeded from a fixtures directory, and writes
// a kubeconfig that points the backend's --kubeconfig at the fake hub with
// the backend's fake ServiceAccount token. See "Dev mode with fake
// infrastructure" in README.md.
//
// It also serves the console's draft API, ai_grid.draft.v1, on
// --draft-address: the fake for the services this backend does not implement
// yet (internal/fakeinfra/draft, docs/draft/README.md). The draft fake has its
// own state, seeded from --draft-fixtures; it does not read the fake hub's
// clusters.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/devserve"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/draft"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/hub"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/keycloak"
)

func main() {
	fixtures := flag.String("fixtures", "fixtures/default", "fixtures directory (fixtures/default or fixtures/poc)")
	hubAddress := flag.String("hub-address", "127.0.0.1:9091", "fake hub API listen address")
	keycloakAddress := flag.String("keycloak-address", "127.0.0.1:9093", "fake Keycloak listen address")
	draftAddress := flag.String("draft-address", "127.0.0.1:9094", "gRPC listen address of the fake ai_grid.draft.v1 services; empty disables them")
	draftFixtures := flag.String("draft-fixtures", "fixtures/draft/default", "fixtures directory of the draft fake (fixtures/draft/default or fixtures/draft/poc)")
	kubeconfigOut := flag.String("kubeconfig-out", "bin/fake-kubeconfig", "where to write the kubeconfig for the backend's --kubeconfig")
	hubCAOut := flag.String("hub-ca-out", "bin/fake-hub-ca.crt", "where to write the fake hub's CA certificate, for the backend's SEARCH_CA_FILE (the fake Search shares the hub's port); empty writes none")
	adminRealm := flag.String("keycloak-admin-realm", "master", "realm of the Keycloak admin user (the backend's KEYCLOAK_ADMIN_REALM)")
	realm := flag.String("keycloak-realm", "ai-grid", "realm with Organizations (the backend's KEYCLOAK_REALM)")
	username := flag.String("keycloak-admin-username", "admin", "Keycloak admin user name the password grant accepts (KEYCLOAK_ADMIN_USERNAME)")
	password := flag.String("keycloak-admin-password", "admin", "Keycloak admin password the password grant accepts (KEYCLOAK_ADMIN_PASSWORD); local fake only")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "--log-level: %v\n", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	if err := run(logger, config{
		fixtures: *fixtures, hubAddress: *hubAddress, keycloakAddress: *keycloakAddress, kubeconfigOut: *kubeconfigOut, hubCAOut: *hubCAOut,
		draftAddress: *draftAddress, draftFixtures: *draftFixtures,
		keycloak: keycloak.Config{AdminRealm: *adminRealm, Realm: *realm, Username: *username, Password: *password},
	}); err != nil {
		logger.Error("ai-grid-fake-infra failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// Server timeouts: a request body must arrive, and a response be written,
// within these, so no slow client can hold a connection or a fake's lock
// for long. Every fake request is answered from memory in microseconds.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 30 * time.Second
	idleTimeout  = 2 * time.Minute
)

type config struct {
	fixtures        string
	hubAddress      string
	keycloakAddress string
	kubeconfigOut   string
	hubCAOut        string
	draftAddress    string
	draftFixtures   string
	keycloak        keycloak.Config
}

func run(logger *slog.Logger, cfg config) error {
	hubFixtures, err := hub.LoadFixtures(cfg.fixtures)
	if err != nil {
		return fmt.Errorf("load hub fixtures: %w", err)
	}
	fakeHub, err := hub.New(hubFixtures, hub.WithLogger(logger.With("component", "hub")))
	if err != nil {
		return fmt.Errorf("build fake hub: %w", err)
	}
	organizations, err := keycloak.LoadFixtures(cfg.fixtures)
	if err != nil {
		return fmt.Errorf("load Keycloak fixtures: %w", err)
	}
	cfg.keycloak.Logger = logger.With("component", "keycloak")
	fakeKeycloak, err := keycloak.New(cfg.keycloak, organizations)
	if err != nil {
		return fmt.Errorf("build fake Keycloak: %w", err)
	}

	var fakeDraft *draft.Server
	if cfg.draftAddress != "" {
		draftFx, err := draft.Load(cfg.draftFixtures)
		if err != nil {
			return fmt.Errorf("load draft fixtures from %s: %w", cfg.draftFixtures, err)
		}
		fakeDraft, err = draft.New(draftFx, logger.With("component", "draft"))
		if err != nil {
			return fmt.Errorf("draft fixtures in %s: %w", cfg.draftFixtures, err)
		}
		logger.Info("draft fixtures loaded", slog.String("dir", cfg.draftFixtures),
			slog.Int("clusters", len(draftFx.Clusters)), slog.Int("tenants", len(draftFx.Tenants)),
			slog.Bool("tenantsUnavailable", draftFx.TenantsUnavailable != ""), slog.Duration("latency", draftFx.Latency.Duration()),
			slog.Int("scenarios", len(draftFx.Scenarios)), slog.Float64("speed", draftFx.Speed))
	}

	hubHost, _, err := net.SplitHostPort(cfg.hubAddress)
	if err != nil {
		return fmt.Errorf("--hub-address: %w", err)
	}
	cert, caPEM, err := devserve.SelfSignedCert(hubHost)
	if err != nil {
		return fmt.Errorf("fake hub certificate: %w", err)
	}
	hubTCP, err := net.Listen("tcp", cfg.hubAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.hubAddress, err)
	}
	// TLS for the backend (client-go sends kubeconfig credentials only over
	// TLS), plain HTTP on the same port for the console BFF.
	hubListener := devserve.NewDualListener(hubTCP, &tls.Config{
		Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12,
	})
	keycloakListener, err := net.Listen("tcp", cfg.keycloakAddress)
	if err != nil {
		_ = hubListener.Close()
		return fmt.Errorf("listen on %s: %w", cfg.keycloakAddress, err)
	}
	// The draft port is bound before the kubeconfig is written too, so the
	// kubeconfig appearing (what hack/dev-fake.sh waits for) means every fake
	// is listening.
	var draftListener net.Listener
	if fakeDraft != nil {
		draftListener, err = net.Listen("tcp", cfg.draftAddress)
		if err != nil {
			_ = hubListener.Close()
			_ = keycloakListener.Close()
			return fmt.Errorf("listen on %s: %w", cfg.draftAddress, err)
		}
	}
	closeListeners := func() {
		_ = hubListener.Close()
		_ = keycloakListener.Close()
		if draftListener != nil {
			_ = draftListener.Close()
		}
	}
	hubURL := "https://" + hubListener.Addr().String()
	// The CA file first: the kubeconfig appearing is what hack/dev-fake.sh
	// waits for, so the CA must exist by then.
	if cfg.hubCAOut != "" {
		if err := os.WriteFile(cfg.hubCAOut, caPEM, 0o600); err != nil {
			closeListeners()
			return fmt.Errorf("write %s: %w", cfg.hubCAOut, err)
		}
		defer func() { _ = os.Remove(cfg.hubCAOut) }()
	}
	written, err := devserve.WriteKubeconfig(cfg.kubeconfigOut, hubURL, caPEM, fakeHub.ServiceAccountToken())
	if err != nil {
		closeListeners()
		return err
	}
	defer func() {
		// Only the file this process wrote, if nobody replaced it since.
		if err := devserve.RemoveKubeconfig(cfg.kubeconfigOut, written); err != nil {
			logger.Warn("remove kubeconfig", slog.Any("error", err))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	servers := []struct {
		name     string
		listener net.Listener
		server   *http.Server
	}{
		{"hub", hubListener, &http.Server{Handler: logRequests(logger, "hub", fakeHub.Handler()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: readTimeout, WriteTimeout: writeTimeout, IdleTimeout: idleTimeout}},
		{"keycloak", keycloakListener, &http.Server{Handler: logRequests(logger, "keycloak", fakeKeycloak.Handler()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: readTimeout, WriteTimeout: writeTimeout, IdleTimeout: idleTimeout}},
	}
	errs := make(chan error, len(servers)+1)
	var wg sync.WaitGroup
	// The draft gRPC server stops when draftCtx ends: on a signal, or when
	// another server fails.
	draftCtx, stopDraft := context.WithCancel(ctx)
	defer stopDraft()
	if fakeDraft != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fakeDraft.ServeListener(draftCtx, draftListener); err != nil {
				errs <- fmt.Errorf("draft server: %w", err)
			}
		}()
	}
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("%s server: %w", s.name, err)
			}
		}()
	}
	logger.Info("fake infrastructure listening",
		slog.String("fixtures", cfg.fixtures),
		slog.String("hub", hubURL),
		slog.String("hubPlainHTTP", "http://"+hubListener.Addr().String()),
		slog.String("keycloak", "http://"+keycloakListener.Addr().String()),
		slog.String("draftGRPC", addrOf(draftListener)),
		slog.String("draftFixtures", cfg.draftFixtures),
		slog.String("kubeconfig", cfg.kubeconfigOut),
		slog.String("search", hubURL+hub.SearchPath),
		slog.String("keycloakAdminRealm", cfg.keycloak.AdminRealm),
		slog.String("keycloakRealm", cfg.keycloak.Realm),
	)

	var serveErr error
	select {
	case <-ctx.Done():
		logger.Info("shutting down fake infrastructure")
	case serveErr = <-errs:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopDraft()
	for _, s := range servers {
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown", slog.String("server", s.name), slog.Any("error", err))
		}
	}
	wg.Wait()
	return serveErr
}

// addrOf is the listener's address, or "disabled" for none.
func addrOf(l net.Listener) string {
	if l == nil {
		return "disabled"
	}
	return l.Addr().String()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests logs one line per request. It never logs headers, so bearer
// tokens and credentials stay out of the log.
func logRequests(logger *slog.Logger, component string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.InfoContext(r.Context(), "request",
			slog.String("component", component),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}
