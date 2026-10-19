package restgateway

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

//go:embed swagger.html
var swaggerHTML []byte

//go:embed openapi/ai_grid.swagger.json
var openAPISpec []byte

//go:embed openapi
var openAPIFS embed.FS

// Servers are the gRPC service implementations exposed over HTTP.
type Servers struct {
	AiGrid   aigridv1.AiGridServer
	Clusters aigridv1.ClustersServer
	Tenants  aigridv1.TenantsServer
}

// Server is the HTTP REST gateway + Swagger UI listener.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
}

// New builds an HTTP gateway that calls the in-process gRPC service impls
// (OSAC-style grpc-gateway) and serves Swagger UI at /docs.
func New(ctx context.Context, address string, servers Servers) (*Server, error) {
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:   true,
				EmitUnpopulated: true,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{
				DiscardUnknown: true,
			},
		}),
	)

	if err := aigridv1.RegisterAiGridHandlerServer(ctx, mux, servers.AiGrid); err != nil {
		return nil, fmt.Errorf("register AiGrid HTTP handlers: %w", err)
	}
	if err := aigridv1.RegisterClustersHandlerServer(ctx, mux, servers.Clusters); err != nil {
		return nil, fmt.Errorf("register Clusters HTTP handlers: %w", err)
	}
	if err := aigridv1.RegisterTenantsHandlerServer(ctx, mux, servers.Tenants); err != nil {
		return nil, fmt.Errorf("register Tenants HTTP handlers: %w", err)
	}

	root := http.NewServeMux()
	root.Handle("/", mux)
	root.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAPISpec)
	})
	root.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusFound)
	})
	root.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(swaggerHTML)
	})
	// Keep the generated file available under a stable docs path as well.
	if sub, err := fs.Sub(openAPIFS, "openapi"); err == nil {
		root.Handle("/openapi/", http.StripPrefix("/openapi/", http.FileServer(http.FS(sub))))
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}

	return &Server{
		listener: listener,
		httpServer: &http.Server{
			Handler: root,
		},
	}, nil
}

// Addr returns the bound listen address.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve blocks until the HTTP server stops.
func (s *Server) Serve() error {
	slog.Info("ai-grid-provider-backend REST gateway listening", "address", s.Addr())
	return s.httpServer.Serve(s.listener)
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
