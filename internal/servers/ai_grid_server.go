package servers

import (
	"context"
	"fmt"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
)

// AiGridServerBuilder configures an AiGridServer.
type AiGridServerBuilder struct{}

// AiGridServer implements ai_grid.v1.AiGrid.
type AiGridServer struct {
	aigridv1connect.UnimplementedAiGridHandler
}

var _ aigridv1connect.AiGridHandler = (*AiGridServer)(nil)

// NewAiGridServer creates a builder for AiGridServer.
func NewAiGridServer() *AiGridServerBuilder {
	return &AiGridServerBuilder{}
}

// Build creates the AiGridServer.
func (b *AiGridServerBuilder) Build() (*AiGridServer, error) {
	return &AiGridServer{}, nil
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
