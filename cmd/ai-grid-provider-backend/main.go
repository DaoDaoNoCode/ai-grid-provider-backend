package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	grpchealth "connectrpc.com/grpchealth/v2"
	grpcreflect "connectrpc.com/grpcreflect/v2"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/restgateway"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	address := flag.String("listen-address", ":8080", "gRPC listen address")
	httpAddress := flag.String("http-listen-address", ":8081", "REST gateway and Swagger UI listen address")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster service-account credentials")
	flag.Parse()

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen on %s: %v\n", *address, err)
		os.Exit(1)
	}

	kubeClient, err := newDynamicClient(*kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	aiGridServer, err := servers.NewAiGridServer().Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build AiGrid server: %v\n", err)
		os.Exit(1)
	}
	clustersServer, err := servers.NewClustersServer().
		SetDynamicClient(kubeClient).
		SetReadyPolicy(getEnv("AI_GRID_READY_POLICY", servers.DefaultReadyPolicy)).
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Clusters server: %v\n", err)
		os.Exit(1)
	}
	tenantsServer, err := servers.NewTenantsServer().
		SetOrganizationClient(keycloak.New(keycloak.Config{
			BaseURL:    getEnv("KEYCLOAK_URL", "http://keycloak.ai-grid.svc:8080"),
			AdminRealm: getEnv("KEYCLOAK_ADMIN_REALM", "master"),
			Realm:      getEnv("KEYCLOAK_REALM", "ai-grid"),
			Username:   getEnv("KEYCLOAK_ADMIN_USERNAME", "admin"),
			Password:   getEnv("KEYCLOAK_ADMIN_PASSWORD", "admin"),
		})).
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Tenants server: %v\n", err)
		os.Exit(1)
	}

	// One *connect.Server holds every service. connecthttp serves Connect,
	// gRPC and gRPC-Web from it; vanguard serves the google.api.http
	// annotations as REST. Both transports dispatch the same handlers.
	server := connect.NewServer()
	aigridv1connect.RegisterAiGridHandler(server, aiGridServer)
	aigridv1connect.RegisterClustersHandler(server, clustersServer)
	aigridv1connect.RegisterTenantsHandler(server, tenantsServer)

	grpchealth.Register(server, grpchealth.NewStaticChecker(
		aigridv1connect.AiGridName,
		aigridv1connect.ClustersName,
		aigridv1connect.TenantsName,
	))
	// Reflection lists the three API services. It does not list grpc.health.v1.Health:
	// grpchealth/v2 registers its methods without a protobuf Schema and grpcreflect
	// skips schema-less methods. Health/Check by name and `grpcurl describe` both work,
	// which is what hack/smoke-apis.sh relies on.
	grpcreflect.Register(server)

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	if err := restgateway.Mount(mux, server); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Plaintext HTTP/2 so gRPC clients (grpcurl, client/, integration/) reach
	// the same address they always have, alongside HTTP/1.1 Connect callers.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	apiServer := &http.Server{Handler: mux, Protocols: protocols}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gateway, err := restgateway.New(*httpAddress, server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "REST gateway: %v\n", err)
		os.Exit(1)
	}

	// http.Server.Shutdown makes Serve return ErrServerClosed immediately, so main
	// must wait for the drain itself: "Make sure the program doesn't exit and waits
	// instead for Shutdown to return." grpc-go's Serve used to block until
	// GracefulStop finished, so without this a pod termination cuts off live RPCs.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		slog.Info("shutting down ai-grid-provider-backend")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = gateway.Shutdown(shutdownCtx)
		_ = apiServer.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := gateway.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "REST gateway: %v\n", err)
			os.Exit(1)
		}
	}()

	slog.Info("ai-grid-provider-backend listening", "grpc", listener.Addr().String(), "http", gateway.Addr())
	if err := apiServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "RPC server: %v\n", err)
		os.Exit(1)
	}
	<-drained
}

func getEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func newDynamicClient(kubeconfig string) (dynamic.Interface, error) {
	var (
		config *rest.Config
		err    error
	)
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("load in-cluster Kubernetes config: %w", err)
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return client, nil
}
