package servers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
)

// organizationLister is the Keycloak Organizations surface used by Tenants.
// Tests inject fakes; production wires *keycloak.Client.
type organizationLister interface {
	ListOrganizations(ctx context.Context) ([]keycloak.Organization, error)
}

// TenantsServerBuilder configures a TenantsServer.
type TenantsServerBuilder struct {
	logger  *slog.Logger
	tenants organizationLister
}

// TenantsServer implements ai_grid.v1.Tenants.
type TenantsServer struct {
	aigridv1.UnimplementedTenantsServer
	logger  *slog.Logger
	tenants organizationLister
}

var _ aigridv1.TenantsServer = (*TenantsServer)(nil)

// NewTenantsServer creates a builder for TenantsServer.
func NewTenantsServer() *TenantsServerBuilder {
	return &TenantsServerBuilder{}
}

// SetLogger sets the logger. Optional.
func (b *TenantsServerBuilder) SetLogger(logger *slog.Logger) *TenantsServerBuilder {
	b.logger = logger
	return b
}

// SetOrganizationLister sets the Keycloak organization client. Mandatory.
func (b *TenantsServerBuilder) SetOrganizationLister(lister organizationLister) *TenantsServerBuilder {
	b.tenants = lister
	return b
}

// Build creates the TenantsServer.
func (b *TenantsServerBuilder) Build() (*TenantsServer, error) {
	if b.tenants == nil {
		return nil, errors.New("keycloak tenant client is mandatory")
	}
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	return &TenantsServer{logger: logger, tenants: b.tenants}, nil
}

// List returns Keycloak Organizations from the configured realm.
func (s *TenantsServer) List(ctx context.Context, req *aigridv1.TenantsListRequest) (*aigridv1.TenantsListResponse, error) {
	if s.tenants == nil {
		return nil, fmt.Errorf("keycloak tenant client is not configured")
	}
	organizations, err := s.tenants.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]*aigridv1.Tenant, 0, len(organizations))
	for _, organization := range organizations {
		items = append(items, tenantFromOrganization(organization))
	}
	page, total := applyListWindow(items, req.Offset, req.Limit)
	return &aigridv1.TenantsListResponse{
		Size:  int32(len(page)),
		Total: total,
		Items: page,
	}, nil
}

func tenantFromOrganization(organization keycloak.Organization) *aigridv1.Tenant {
	return &aigridv1.Tenant{
		Id: organization.ID,
		Metadata: &aigridv1.Metadata{
			Name: organization.Name,
		},
		Spec: &aigridv1.TenantSpec{
			Domains: organization.DomainNames(),
		},
		Status: &aigridv1.TenantStatus{},
	}
}
