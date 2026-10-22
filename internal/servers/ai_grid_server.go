package servers

import (
	connect "connectrpc.com/connect/v2"
	"context"
	"fmt"
	"log/slog"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
)

// AiGridServerBuilder configures an AiGridServer.
type AiGridServerBuilder struct {
	logger *slog.Logger
}

// AiGridServer implements ai_grid.v1.AiGrid.
type AiGridServer struct {
	aigridv1connect.UnimplementedAiGridHandler
	logger *slog.Logger
}

var _ aigridv1connect.AiGridHandler = (*AiGridServer)(nil)

// NewAiGridServer creates a builder for AiGridServer.
func NewAiGridServer() *AiGridServerBuilder {
	return &AiGridServerBuilder{}
}

// SetLogger sets the logger. Optional; a default is used when unset.
func (b *AiGridServerBuilder) SetLogger(logger *slog.Logger) *AiGridServerBuilder {
	b.logger = logger
	return b
}

// Build creates the AiGridServer.
func (b *AiGridServerBuilder) Build() (*AiGridServer, error) {
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AiGridServer{logger: logger}, nil
}

// Ping returns a greeting for the requested name.
func (s *AiGridServer) Ping(_ context.Context, request *aigridv1.AiGridPingRequest) (*aigridv1.AiGridPingResponse, error) {
	if request == nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "request is required")
	}
	name := request.GetName()
	if name == "" {
		name = "world"
	}
	return &aigridv1.AiGridPingResponse{Message: fmt.Sprintf("Welcome, %s!", name)}, nil
}
