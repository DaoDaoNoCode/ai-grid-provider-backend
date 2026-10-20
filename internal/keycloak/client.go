package keycloak

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client is the small part of the Keycloak Admin API used by ai-grid-provider-backend.
// The local Kind deployment uses the bootstrap admin password grant; a
// production deployment should use a service account with query-organizations.
type Client struct {
	baseURL    string
	adminRealm string
	realm      string
	username   string
	password   string
	httpClient *http.Client
}

// Config configures a Keycloak Admin API client.
// HTTPClient is optional; when nil, http.DefaultClient is used.
type Config struct {
	BaseURL    string
	AdminRealm string
	Realm      string
	Username   string
	Password   string
	HTTPClient *http.Client
}

func New(config Config) *Client {
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL: strings.TrimRight(config.BaseURL, "/"), adminRealm: config.AdminRealm,
		realm: config.Realm, username: config.Username, password: config.Password,
		httpClient: httpClient,
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

// OrganizationDomain is a verified domain on a Keycloak Organization.
type OrganizationDomain struct {
	Name string `json:"name"`
}

// Organization is a Keycloak Organization in the configured realm.
type Organization struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Domains []OrganizationDomain `json:"domains"`
}

// ListOrganizations lists Keycloak Organizations in the configured realm.
func (c *Client) ListOrganizations(ctx context.Context) ([]Organization, error) {
	form := url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {c.username}, "password": {c.password},
	}
	tokenRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/realms/"+url.PathEscape(c.adminRealm)+"/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create Keycloak token request: %w", err)
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResponseBody, err := c.httpClient.Do(tokenRequest)
	if err != nil {
		return nil, fmt.Errorf("request Keycloak token: %w", err)
	}
	defer tokenResponseBody.Body.Close()
	if tokenResponseBody.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(tokenResponseBody.Body, 4096))
		return nil, fmt.Errorf("keycloak token request returned %s: %s", tokenResponseBody.Status, strings.TrimSpace(string(body)))
	}
	var token tokenResponse
	if err := json.NewDecoder(tokenResponseBody.Body).Decode(&token); err != nil {
		return nil, fmt.Errorf("decode Keycloak token: %w", err)
	}

	endpoint := c.baseURL + "/admin/realms/" + url.PathEscape(c.realm) + "/organizations?first=0&max=" + strconv.Itoa(100)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create Keycloak organizations request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request Keycloak organizations: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("keycloak organizations request returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var organizations []Organization
	if err := json.NewDecoder(response.Body).Decode(&organizations); err != nil {
		return nil, fmt.Errorf("decode Keycloak organizations: %w", err)
	}
	return organizations, nil
}

// DomainNames returns the domain name strings for an organization.
func (o Organization) DomainNames() []string {
	domains := make([]string, 0, len(o.Domains))
	for _, domain := range o.Domains {
		domains = append(domains, domain.Name)
	}
	return domains
}
