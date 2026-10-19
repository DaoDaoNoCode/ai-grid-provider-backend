// Command ai-grid-fake-infra runs the fake infrastructure the real
// ai-grid-provider-backend needs on a laptop: a fake hub API (ManagedClusters
// and SelfSubjectAccessReview) and a fake Keycloak (token endpoint and
// Organizations Admin API), both seeded from a fixtures directory, and writes
// a kubeconfig that points the backend's --kubeconfig at the fake hub with
// the backend's fake ServiceAccount token. See "Dev mode with fake
// infrastructure" in README.md.
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
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/hub"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/keycloak"
)

func main() {
	fixtures := flag.String("fixtures", "fixtures/default", "fixtures directory (fixtures/default or fixtures/poc)")
	hubAddress := flag.String("hub-address", "127.0.0.1:9091", "fake hub API listen address")
	keycloakAddress := flag.String("keycloak-address", "127.0.0.1:9093", "fake Keycloak listen address")
	kubeconfigOut := flag.String("kubeconfig-out", "bin/fake-kubeconfig", "where to write the kubeconfig for the backend's --kubeconfig")
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
		fixtures: *fixtures, hubAddress: *hubAddress, keycloakAddress: *keycloakAddress, kubeconfigOut: *kubeconfigOut,
		keycloak: keycloak.Config{AdminRealm: *adminRealm, Realm: *realm, Username: *username, Password: *password},
	}); err != nil {
		logger.Error("ai-grid-fake-infra failed", slog.Any("error", err))
		os.Exit(1)
	}
}

type config struct {
	fixtures        string
	hubAddress      string
	keycloakAddress string
	kubeconfigOut   string
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
	hubURL := "https://" + hubListener.Addr().String()
	written, err := devserve.WriteKubeconfig(cfg.kubeconfigOut, hubURL, caPEM, fakeHub.ServiceAccountToken())
	if err != nil {
		_ = hubListener.Close()
		_ = keycloakListener.Close()
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
		{"hub", hubListener, &http.Server{Handler: logRequests(logger, "hub", fakeHub.Handler()), ReadHeaderTimeout: 5 * time.Second}},
		{"keycloak", keycloakListener, &http.Server{Handler: logRequests(logger, "keycloak", fakeKeycloak.Handler()), ReadHeaderTimeout: 5 * time.Second}},
	}
	errs := make(chan error, len(servers))
	var wg sync.WaitGroup
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
		slog.String("kubeconfig", cfg.kubeconfigOut),
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
	for _, s := range servers {
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown", slog.String("server", s.name), slog.Any("error", err))
		}
	}
	wg.Wait()
	return serveErr
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
