package aigrid

import (
	"context"
	"fmt"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

// ListTenants returns Keycloak Organizations from the configured realm.
func (s Service) ListTenants(ctx context.Context, _ *aigridv1.ListTenantsRequest) (*aigridv1.ListTenantsResponse, error) {
	if s.tenants == nil {
		return nil, fmt.Errorf("Keycloak tenant client is not configured")
	}
	organizations, err := s.tenants.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	tenants := make([]*aigridv1.Tenant, 0, len(organizations))
	for _, organization := range organizations {
		tenants = append(tenants, &aigridv1.Tenant{Id: organization.ID, Name: organization.Name, Domains: organization.DomainNames()})
	}
	return &aigridv1.ListTenantsResponse{Tenants: tenants}, nil
}
