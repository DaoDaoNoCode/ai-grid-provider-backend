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

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/ai_grid"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/restgateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"k8s.io/client-go/rest"
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
	var service *aigrid.Service
	if *kubeconfig == "" {
		service, err = aigrid.NewServiceFromConfig(mustInClusterConfig())
	} else {
		service, err = aigrid.NewServiceFromKubeconfig(*kubeconfig)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "load managed clusters: %v\n", err)
		os.Exit(1)
	}
	service.SetKeycloakClient(keycloak.New(keycloak.Config{
		BaseURL:    getEnv("KEYCLOAK_URL", "http://keycloak.ai-grid.svc:8080"),
		AdminRealm: getEnv("KEYCLOAK_ADMIN_REALM", "master"),
		Realm:      getEnv("KEYCLOAK_REALM", "ai-grid"),
		Username:   getEnv("KEYCLOAK_ADMIN_USERNAME", "admin"),
		Password:   getEnv("KEYCLOAK_ADMIN_PASSWORD", "admin"),
	}))

	grpcServer := grpc.NewServer()
	aigridv1.RegisterAiGridServer(grpcServer, service)
	aigridv1.RegisterManagedClustersServer(grpcServer, service)
	aigridv1.RegisterTenantsServer(grpcServer, service)

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(grpcServer, healthServer)
	reflection.Register(grpcServer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gateway, err := restgateway.New(ctx, *httpAddress, restgateway.Servers{
		AiGrid:          service,
		ManagedClusters: service,
		Tenants:         service,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "REST gateway: %v\n", err)
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down ai-grid-provider-backend")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = gateway.Shutdown(shutdownCtx)
		grpcServer.GracefulStop()
	}()

	go func() {
		if err := gateway.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "REST gateway: %v\n", err)
			os.Exit(1)
		}
	}()

	slog.Info("ai-grid-provider-backend listening", "grpc", listener.Addr().String(), "http", gateway.Addr())
	if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		fmt.Fprintf(os.Stderr, "gRPC server: %v\n", err)
		os.Exit(1)
	}
}

func getEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func mustInClusterConfig() *rest.Config {
	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load in-cluster Kubernetes config: %v\n", err)
		os.Exit(1)
	}
	return config
}
