package servers

import (
	"context"
	"log/slog"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
)

const subscriptionsNotImplemented = "Subscriptions storage is not implemented yet"

// SubscriptionsServerBuilder configures a SubscriptionsServer.
type SubscriptionsServerBuilder struct {
	logger *slog.Logger
}

// SubscriptionsServer implements ai_grid.v1.Subscriptions.
// Placeholder: List returns an empty page; Get/Create/Update/Delete are Unimplemented.
type SubscriptionsServer struct {
	aigridv1connect.UnimplementedSubscriptionsHandler
	logger *slog.Logger
}

var _ aigridv1connect.SubscriptionsHandler = (*SubscriptionsServer)(nil)

// NewSubscriptionsServer creates a builder for SubscriptionsServer.
func NewSubscriptionsServer() *SubscriptionsServerBuilder {
	return &SubscriptionsServerBuilder{}
}

// SetLogger sets the logger. Optional; a default is used when unset.
func (b *SubscriptionsServerBuilder) SetLogger(logger *slog.Logger) *SubscriptionsServerBuilder {
	b.logger = logger
	return b
}

// Build creates the SubscriptionsServer.
func (b *SubscriptionsServerBuilder) Build() (*SubscriptionsServer, error) {
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	return &SubscriptionsServer{logger: logger}, nil
}

// List returns an empty subscription list. Storage is not implemented yet.
func (s *SubscriptionsServer) List(_ context.Context, _ *aigridv1.SubscriptionsListRequest) (*aigridv1.SubscriptionsListResponse, error) {
	return &aigridv1.SubscriptionsListResponse{
		Size:  0,
		Total: 0,
		Items: []*aigridv1.Subscription{},
	}, nil
}

// Get is not implemented yet.
func (s *SubscriptionsServer) Get(_ context.Context, _ *aigridv1.SubscriptionsGetRequest) (*aigridv1.SubscriptionsGetResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, subscriptionsNotImplemented)
}

// Create is not implemented yet.
func (s *SubscriptionsServer) Create(_ context.Context, _ *aigridv1.SubscriptionsCreateRequest) (*aigridv1.SubscriptionsCreateResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, subscriptionsNotImplemented)
}

// Update is not implemented yet.
func (s *SubscriptionsServer) Update(_ context.Context, _ *aigridv1.SubscriptionsUpdateRequest) (*aigridv1.SubscriptionsUpdateResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, subscriptionsNotImplemented)
}

// Delete is not implemented yet.
func (s *SubscriptionsServer) Delete(_ context.Context, _ *aigridv1.SubscriptionsDeleteRequest) (*aigridv1.SubscriptionsDeleteResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, subscriptionsNotImplemented)
}
