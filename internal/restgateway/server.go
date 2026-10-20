package restgateway

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/vanguard"
)

//go:embed swagger.html
var swaggerHTML []byte

//go:embed openapi/ai_grid.swagger.json
var openAPISpec []byte

//go:embed openapi
var openAPIFS embed.FS

// Server is the HTTP REST gateway + Swagger UI listener.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
}

// New builds an HTTP gateway that serves the registered services over REST and
// Swagger UI at /docs. vanguard reads the google.api.http annotations off the
// descriptors at runtime, so there is no generated gateway code.
func New(address string, server *connect.Server) (*Server, error) {
	root := http.NewServeMux()
	// vanguard installs a catch-all "/" handler; the more specific patterns
	// below still win, so the docs paths are unaffected.
	if err := Mount(root, server); err != nil {
		return nil, err
	}

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

// Mount adds the REST surface to mux: vanguard derives it from the
// google.api.http annotations on the services registered on server. Every
// listener that offers REST goes through here, so they all answer the same
// JSON.
func Mount(mux connecthttp.ServeMux, server *connect.Server) error {
	if err := vanguard.Mount(mux, server, vanguard.WithCodec(newRESTCodec)); err != nil {
		return fmt.Errorf("mount REST handlers: %w", err)
	}
	return nil
}

// newRESTCodec keeps the JSON the REST surface has always answered with.
// vanguard's default emits camelCase names; the grpc-gateway marshaler this
// replaces was configured for proto names, and unpopulated fields are emitted
// either way.
func newRESTCodec(resolver connectproto.TypeResolver) vanguard.RESTCodec {
	codec := vanguard.NewJSONCodec(resolver)
	codec.MarshalOptions.UseProtoNames = true
	return codec
}
