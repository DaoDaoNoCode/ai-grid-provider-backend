package client

import (
	"context"
	"errors"
	"fmt"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Builder configures a client connection to ai-grid-provider-backend.
type Builder struct {
	address   string
	plaintext bool
}

// NewBuilder creates a client builder.
func NewBuilder() *Builder { return &Builder{} }

// SetAddress sets the server address, for example localhost:8080.
func (b *Builder) SetAddress(address string) *Builder {
	b.address = address
	return b
}

// SetPlaintext enables a connection without TLS, intended for local development.
func (b *Builder) SetPlaintext(plaintext bool) *Builder {
	b.plaintext = plaintext
	return b
}

// Build creates a connected client.
func (b *Builder) Build(ctx context.Context) (*Client, error) {
	if b.address == "" {
		return nil, errors.New("server address is mandatory")
	}
	if !b.plaintext {
		return nil, errors.New("TLS is not configured; call SetPlaintext(true) for a local server")
	}

	connection, err := grpc.DialContext(ctx, b.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to ai-grid-provider-backend: %w", err)
	}
	return &Client{
		connection:      connection,
		aiGrid:          aigridv1.NewAiGridClient(connection),
		managedClusters: aigridv1.NewManagedClustersClient(connection),
		tenants:         aigridv1.NewTenantsClient(connection),
	}, nil
}

// Client calls the ai-grid-provider-backend gRPC API.
type Client struct {
	connection      *grpc.ClientConn
	aiGrid          aigridv1.AiGridClient
	managedClusters aigridv1.ManagedClustersClient
	tenants         aigridv1.TenantsClient
}

// Ping invokes ai_grid.v1.AiGrid/Ping.
func (c *Client) Ping(ctx context.Context, name string) (string, error) {
	response, err := c.aiGrid.Ping(ctx, &aigridv1.PingRequest{Name: name})
	if err != nil {
		return "", err
	}
	return response.GetMessage(), nil
}

// ListManagedClusters returns the sample or ACM-backed ManagedCluster objects.
func (c *Client) ListManagedClusters(ctx context.Context) ([]*aigridv1.ManagedCluster, error) {
	response, err := c.managedClusters.ListManagedClusters(ctx, &aigridv1.ListManagedClustersRequest{})
	if err != nil {
		return nil, err
	}
	return response.GetClusters(), nil
}

// ListTenants returns Keycloak Organizations exposed as ai-grid tenants.
func (c *Client) ListTenants(ctx context.Context) ([]*aigridv1.Tenant, error) {
	response, err := c.tenants.ListTenants(ctx, &aigridv1.ListTenantsRequest{})
	if err != nil {
		return nil, err
	}
	return response.GetTenants(), nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error { return c.connection.Close() }
