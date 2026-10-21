package client

import (
	"context"
	"errors"
	"fmt"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
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
		return nil, errors.New("tls is not configured; call SetPlaintext(true) for a local server")
	}

	// NewClient dials lazily; ctx is kept on Build for API compatibility.
	connection, err := grpc.NewClient(b.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to ai-grid-provider-backend: %w", err)
	}
	return &Client{
		connection: connection,
		aiGrid:     aigridv1.NewAiGridClient(connection),
		clusters:   aigridv1.NewClustersClient(connection),
		tenants:    aigridv1.NewTenantsClient(connection),
	}, nil
}

// Client calls the ai-grid-provider-backend gRPC API.
type Client struct {
	connection *grpc.ClientConn
	aiGrid     aigridv1.AiGridClient
	clusters   aigridv1.ClustersClient
	tenants    aigridv1.TenantsClient
}

// Ping invokes ai_grid.v1.AiGrid/Ping.
func (c *Client) Ping(ctx context.Context, name string) (string, error) {
	response, err := c.aiGrid.Ping(ctx, &aigridv1.AiGridPingRequest{Name: name})
	if err != nil {
		return "", err
	}
	return response.GetMessage(), nil
}

// ListClusters returns ManagedCluster objects from the hub.
func (c *Client) ListClusters(ctx context.Context) ([]*aigridv1.ManagedCluster, error) {
	response, err := c.clusters.List(ctx, &aigridv1.ClustersListRequest{})
	if err != nil {
		return nil, err
	}
	return response.GetItems(), nil
}

// GetCluster returns a single ManagedCluster by id.
func (c *Client) GetCluster(ctx context.Context, id string) (*aigridv1.ManagedCluster, error) {
	response, err := c.clusters.Get(ctx, &aigridv1.ClustersGetRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return response.GetObject(), nil
}

// CreateCluster creates a hub-side ManagedCluster CR.
func (c *Client) CreateCluster(ctx context.Context, object *aigridv1.ManagedCluster) (*aigridv1.ManagedCluster, error) {
	response, err := c.clusters.Create(ctx, &aigridv1.ClustersCreateRequest{Object: object})
	if err != nil {
		return nil, err
	}
	return response.GetObject(), nil
}

// UpdateCluster patches an existing ManagedCluster. A nil mask updates all mutable fields.
func (c *Client) UpdateCluster(ctx context.Context, object *aigridv1.ManagedCluster, mask *fieldmaskpb.FieldMask) (*aigridv1.ManagedCluster, error) {
	response, err := c.clusters.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     object,
		UpdateMask: mask,
	})
	if err != nil {
		return nil, err
	}
	return response.GetObject(), nil
}

// DeleteCluster deletes a ManagedCluster by id.
func (c *Client) DeleteCluster(ctx context.Context, id string) error {
	_, err := c.clusters.Delete(ctx, &aigridv1.ClustersDeleteRequest{Id: id})
	return err
}

// ListTenants returns Keycloak Organizations exposed as ai-grid tenants.
func (c *Client) ListTenants(ctx context.Context) ([]*aigridv1.Tenant, error) {
	response, err := c.tenants.List(ctx, &aigridv1.TenantsListRequest{})
	if err != nil {
		return nil, err
	}
	return response.GetItems(), nil
}

// GetTenant returns a single tenant by Keycloak Organization id.
func (c *Client) GetTenant(ctx context.Context, id string) (*aigridv1.Tenant, error) {
	response, err := c.tenants.Get(ctx, &aigridv1.TenantsGetRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return response.GetObject(), nil
}

// CreateTenant creates a Keycloak Organization for the tenant.
func (c *Client) CreateTenant(ctx context.Context, object *aigridv1.Tenant) (*aigridv1.Tenant, error) {
	response, err := c.tenants.Create(ctx, &aigridv1.TenantsCreateRequest{Object: object})
	if err != nil {
		return nil, err
	}
	return response.GetObject(), nil
}

// UpdateTenant patches an existing tenant. A nil mask updates all mutable fields.
func (c *Client) UpdateTenant(ctx context.Context, object *aigridv1.Tenant, mask *fieldmaskpb.FieldMask) (*aigridv1.Tenant, error) {
	response, err := c.tenants.Update(ctx, &aigridv1.TenantsUpdateRequest{
		Object:     object,
		UpdateMask: mask,
	})
	if err != nil {
		return nil, err
	}
	return response.GetObject(), nil
}

// DeleteTenant deletes a tenant (Keycloak Organization) by id.
func (c *Client) DeleteTenant(ctx context.Context, id string) error {
	_, err := c.tenants.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: id})
	return err
}

// ListTenantMembers returns Keycloak users that belong to the tenant Organization.
func (c *Client) ListTenantMembers(ctx context.Context, id string) ([]*aigridv1.TenantMember, error) {
	response, err := c.tenants.ListMembers(ctx, &aigridv1.TenantsListMembersRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return response.GetItems(), nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error { return c.connection.Close() }
