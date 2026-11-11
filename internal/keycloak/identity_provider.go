package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

const (
	// IdentityProviderOrgIDMapperName names the mapper this backend puts on every IdP it
	// creates; an IdP without it was made outside this API.
	IdentityProviderOrgIDMapperName   = "AI Grid org id"
	identityProviderOrgNameMapperName = "AI Grid org name"
	identityProviderGroupsMapperName  = "AI Grid groups"
	identityProviderRolesMapperName   = "AI Grid roles"
)

// IdentityProvider is a realm-level identity provider instance. TrustEmail is Keycloak's
// trustEmail: with it on, Keycloak takes a brokered user's email_verified from the IdP instead
// of setting it to false. Keys this type doesn't model (hideOnLogin, the first broker login
// flow and others) are kept in Extra, so a GET→modify→PUT writes them back unchanged.
type IdentityProvider struct {
	Alias       string                     `json:"alias"`
	DisplayName string                     `json:"displayName,omitempty"`
	ProviderID  string                     `json:"providerId"`
	Enabled     bool                       `json:"enabled"`
	TrustEmail  bool                       `json:"trustEmail"`
	Config      map[string]string          `json:"config"`
	Extra       map[string]json.RawMessage `json:"-"`
}

type identityProviderFields IdentityProvider

// UnmarshalJSON keeps the keys IdentityProvider doesn't model in Extra.
func (p *IdentityProvider) UnmarshalJSON(data []byte) error {
	var fields identityProviderFields
	extra, err := unmarshalWithExtra(data, &fields)
	if err != nil {
		return err
	}
	*p = IdentityProvider(fields)
	p.Extra = extra
	return nil
}

// MarshalJSON writes Extra back beside the modelled keys.
func (p IdentityProvider) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(identityProviderFields(p), p.Extra)
}

// IdentityProviderMapper is an IdP mapper representation.
type IdentityProviderMapper struct {
	ID                     string            `json:"id,omitempty"`
	Name                   string            `json:"name"`
	IdentityProviderAlias  string            `json:"identityProviderAlias"`
	IdentityProviderMapper string            `json:"identityProviderMapper"`
	Config                 map[string]string `json:"config"`
}

// OrganizationIdentityProviderLink configures an org↔IdP association.
type OrganizationIdentityProviderLink struct {
	AutoMembership bool   `json:"autoMembership"`
	MembershipType string `json:"membershipType"`
}

// CreateIdentityProvider creates a realm-level identity provider.
func (c *Client) CreateIdentityProvider(ctx context.Context, idp IdentityProvider) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(idp)
	if err != nil {
		return fmt.Errorf("marshal identity provider: %w", err)
	}
	return c.doJSON(ctx, http.MethodPost, c.idpInstancesURL(), token, payload, nil, "create identity provider")
}

// DeleteIdentityProvider deletes a realm-level identity provider by alias.
// A 404 is treated as success.
func (c *Client) DeleteIdentityProvider(ctx context.Context, alias string) error {
	if alias == "" {
		return fmt.Errorf("identity provider alias is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	err = c.doJSON(ctx, http.MethodDelete, c.idpInstanceURL(alias), token, nil, nil, "delete identity provider")
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

// GetIdentityProvider returns a realm-level identity provider by alias. Keycloak masks the
// client secret in the answer as "**********" (StripSecretsUtils); a PUT that sends that value
// back keeps the stored secret (IdentityProviderResource.updateIdpFromRep).
func (c *Client) GetIdentityProvider(ctx context.Context, alias string) (IdentityProvider, error) {
	if alias == "" {
		return IdentityProvider{}, fmt.Errorf("identity provider alias is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return IdentityProvider{}, err
	}
	var idp IdentityProvider
	if err := c.doJSON(ctx, http.MethodGet, c.idpInstanceURL(alias), token, nil, &idp, "get identity provider"); err != nil {
		return IdentityProvider{}, err
	}
	return idp, nil
}

// GetOrganizationIdentityProvider returns the realm IdP with alias when it is linked to the
// organization (GET .../organizations/{id}/identity-providers/{alias}); Keycloak answers 404
// for an alias that names no IdP or one not linked to this organization.
func (c *Client) GetOrganizationIdentityProvider(ctx context.Context, orgID, alias string) (IdentityProvider, error) {
	if orgID == "" || alias == "" {
		return IdentityProvider{}, fmt.Errorf("organization id and identity provider alias are required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return IdentityProvider{}, err
	}
	var idp IdentityProvider
	if err := c.doJSON(ctx, http.MethodGet, c.orgIdpAliasURL(orgID, alias), token, nil, &idp, "get organization identity provider"); err != nil {
		return IdentityProvider{}, err
	}
	return idp, nil
}

// UpdateIdentityProvider replaces a realm-level identity provider's representation
// (PUT .../identity-provider/instances/{alias}). Keycloak keeps the provider's internal id,
// its organization link and its mappers, and drops the signing keys it cached for it.
func (c *Client) UpdateIdentityProvider(ctx context.Context, idp IdentityProvider) error {
	if idp.Alias == "" {
		return fmt.Errorf("identity provider alias is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(idp)
	if err != nil {
		return fmt.Errorf("marshal identity provider: %w", err)
	}
	return c.doJSON(ctx, http.MethodPut, c.idpInstanceURL(idp.Alias), token, payload, nil, "update identity provider")
}

// ImportIdentityProviderConfig asks Keycloak to read an OIDC discovery document
// (POST .../identity-provider/import-config with providerId oidc and fromUrl) and returns the
// identity provider config Keycloak derives from it: issuer, authorizationUrl, tokenUrl,
// jwksUrl and the other endpoints. Keycloak fetches the document itself, with its own HTTP
// client (no redirects, bounded response size), and answers 500 when it can't read it.
func (c *Client) ImportIdentityProviderConfig(ctx context.Context, fromURL string) (map[string]string, error) {
	if fromURL == "" {
		return nil, fmt.Errorf("discovery document URL is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{"providerId": "oidc", "fromUrl": fromURL})
	if err != nil {
		return nil, fmt.Errorf("marshal import-config request: %w", err)
	}
	var config map[string]string
	if err := c.doJSON(ctx, http.MethodPost, c.idpImportConfigURL(), token, payload, &config, "import identity provider config"); err != nil {
		return nil, err
	}
	return config, nil
}

// ReloadIdentityProviderKeys asks Keycloak to load an identity provider's signing keys from
// its jwksUrl now (GET .../instances/{alias}/reload-keys), as it does at sign-in. Keycloak
// answers 500 when it can't load them, and false when it didn't try: the provider has no
// jwksUrl, or Keycloak loaded its keys less than minTimeBetweenRequests (10 s) ago.
func (c *Client) ReloadIdentityProviderKeys(ctx context.Context, alias string) (bool, error) {
	if alias == "" {
		return false, fmt.Errorf("identity provider alias is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return false, err
	}
	var reloaded bool
	if err := c.doJSON(ctx, http.MethodGet, c.idpInstanceURL(alias)+"/reload-keys", token, nil, &reloaded, "reload identity provider keys"); err != nil {
		return false, err
	}
	return reloaded, nil
}

// ListIdentityProviderMappers lists an identity provider's mappers
// (GET .../identity-provider/instances/{alias}/mappers).
func (c *Client) ListIdentityProviderMappers(ctx context.Context, alias string) ([]IdentityProviderMapper, error) {
	if alias == "" {
		return nil, fmt.Errorf("identity provider alias is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	var mappers []IdentityProviderMapper
	if err := c.doJSON(ctx, http.MethodGet, c.idpMappersURL(alias), token, nil, &mappers, "list identity provider mappers"); err != nil {
		return nil, err
	}
	return mappers, nil
}

// CountIdentityProviderUsers counts the realm's users with a federated identity link to an
// identity provider (GET /admin/realms/{realm}/users/count?idpAlias={alias},
// UsersResource.getUsersCount). It needs query-users, and counts every such user, not only
// those the caller may view, unless Keycloak's fine-grained admin permissions are enabled.
func (c *Client) CountIdentityProviderUsers(ctx context.Context, alias string) (int, error) {
	if alias == "" {
		return 0, fmt.Errorf("identity provider alias is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return 0, err
	}
	endpoint := c.baseURL + "/admin/realms/" + url.PathEscape(c.realm) + "/users/count?idpAlias=" + url.QueryEscape(alias)
	var count int
	if err := c.doJSON(ctx, http.MethodGet, endpoint, token, nil, &count, "count identity provider users"); err != nil {
		return 0, err
	}
	return count, nil
}

// LinkOrganizationIdentityProvider associates a realm IdP with an organization.
func (c *Client) LinkOrganizationIdentityProvider(ctx context.Context, orgID, alias string) error {
	if orgID == "" || alias == "" {
		return fmt.Errorf("organization id and identity provider alias are required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(alias)
	if err != nil {
		return fmt.Errorf("marshal identity provider alias: %w", err)
	}
	return c.doJSON(ctx, http.MethodPost, c.orgIdpURL(orgID), token, payload, nil, "link organization identity provider")
}

// ConfigureOrganizationIdentityProvider sets MANAGED auto-membership on the org IdP link.
func (c *Client) ConfigureOrganizationIdentityProvider(ctx context.Context, orgID, alias string) error {
	if orgID == "" || alias == "" {
		return fmt.Errorf("organization id and identity provider alias are required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(OrganizationIdentityProviderLink{
		AutoMembership: true,
		MembershipType: "MANAGED",
	})
	if err != nil {
		return fmt.Errorf("marshal organization identity provider link: %w", err)
	}
	return c.doJSON(ctx, http.MethodPut, c.orgIdpAliasURL(orgID, alias), token, payload, nil, "configure organization identity provider")
}

// UpsertIdentityProviderMapper creates or updates the broker mappers that put the AI Grid
// identity claims on a tenant user: the organization id and name (fixed per IdP), and the
// tenant IdP's groups and roles claims, copied on every login (FORCE). The realm's client
// scopes turn the maas_* user attributes into the flat org_id, org_name, groups and roles
// token claims (ODH-ADR-MS-0007).
func (c *Client) UpsertIdentityProviderMapper(ctx context.Context, alias, organizationID, organizationName string) error {
	if alias == "" || organizationID == "" {
		return fmt.Errorf("identity provider alias and organization id are required")
	}
	if organizationName == "" {
		organizationName = alias
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	var existing []IdentityProviderMapper
	if err := c.doJSON(ctx, http.MethodGet, c.idpMappersURL(alias), token, nil, &existing, "list identity provider mappers"); err != nil {
		return err
	}
	for _, mapper := range identityProviderMappers(alias, organizationID, organizationName) {
		method, url, op := http.MethodPost, c.idpMappersURL(alias), "create identity provider mapper"
		for _, e := range existing {
			if e.Name == mapper.Name {
				mapper.ID = e.ID
				method, url, op = http.MethodPut, c.idpMapperURL(alias, e.ID), "update identity provider mapper"
				break
			}
		}
		payload, err := json.Marshal(mapper)
		if err != nil {
			return fmt.Errorf("marshal identity provider mapper: %w", err)
		}
		if err := c.doJSON(ctx, method, url, token, payload, nil, op); err != nil {
			return err
		}
	}
	return nil
}

func identityProviderMappers(alias, organizationID, organizationName string) []IdentityProviderMapper {
	hardcoded := func(name, attribute, value string) IdentityProviderMapper {
		return IdentityProviderMapper{
			Name: name, IdentityProviderAlias: alias, IdentityProviderMapper: "hardcoded-attribute-idp-mapper",
			Config: map[string]string{"attribute": attribute, "attribute.value": value, "syncMode": "FORCE"},
		}
	}
	fromClaim := func(name, claim, attribute string) IdentityProviderMapper {
		return IdentityProviderMapper{
			Name: name, IdentityProviderAlias: alias, IdentityProviderMapper: "oidc-user-attribute-idp-mapper",
			Config: map[string]string{"claim": claim, "user.attribute": attribute, "syncMode": "FORCE"},
		}
	}
	return []IdentityProviderMapper{
		hardcoded(IdentityProviderOrgIDMapperName, "maas_org_id", organizationID),
		hardcoded(identityProviderOrgNameMapperName, "maas_org_name", organizationName),
		fromClaim(identityProviderGroupsMapperName, "groups", "maas_groups"),
		fromClaim(identityProviderRolesMapperName, "roles", "maas_roles"),
	}
}

func (c *Client) idpInstancesURL() string {
	return c.baseURL + "/admin/realms/" + url.PathEscape(c.realm) + "/identity-provider/instances"
}

func (c *Client) idpImportConfigURL() string {
	return c.baseURL + "/admin/realms/" + url.PathEscape(c.realm) + "/identity-provider/import-config"
}

func (c *Client) idpInstanceURL(alias string) string {
	return c.idpInstancesURL() + "/" + url.PathEscape(alias)
}

func (c *Client) idpMappersURL(alias string) string {
	return c.idpInstanceURL(alias) + "/mappers"
}

func (c *Client) idpMapperURL(alias, id string) string {
	return c.idpMappersURL(alias) + "/" + url.PathEscape(id)
}

func (c *Client) orgIdpURL(orgID string) string {
	return c.orgURL(orgID) + "/identity-providers"
}

func (c *Client) orgIdpAliasURL(orgID, alias string) string {
	return c.orgIdpURL(orgID) + "/" + url.PathEscape(alias)
}
