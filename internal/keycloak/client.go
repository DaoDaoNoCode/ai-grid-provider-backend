package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client is the small part of the Keycloak Admin API used by ai-grid-provider-backend.
// The local Kind deployment uses the bootstrap admin password grant; a
// production deployment should use a service account with manage-organization
// (and related) roles instead of these credentials.
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

// OrganizationDomain is a domain on a Keycloak Organization.
type OrganizationDomain struct {
	Name     string `json:"name"`
	Verified bool   `json:"verified,omitempty"`
}

// OrganizationMember is a Keycloak user belonging to an Organization.
// Fields match the Admin API brief/full user representation used by
// GET /admin/realms/{realm}/organizations/{id}/members.
type OrganizationMember struct {
	ID        string `json:"id,omitempty"`
	Username  string `json:"username,omitempty"`
	Email     string `json:"email,omitempty"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	Enabled   bool   `json:"enabled,omitempty"`
}

// Organization is a Keycloak Organization in the configured realm.
// Extra fields (Enabled, Description, RedirectURL, Attributes) are preserved
// through GET→modify→PUT so Keycloak replacement semantics do not clear them.
type Organization struct {
	ID          string               `json:"id,omitempty"`
	Name        string               `json:"name"`
	Alias       string               `json:"alias,omitempty"`
	Enabled     *bool                `json:"enabled,omitempty"`
	Description string               `json:"description,omitempty"`
	RedirectURL string               `json:"redirectUrl,omitempty"`
	Attributes  map[string][]string  `json:"attributes,omitempty"`
	Domains     []OrganizationDomain `json:"domains,omitempty"`
}

// APIError is a non-2xx response from the Keycloak Admin API.
type APIError struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("Keycloak %s returned %s", e.Op, http.StatusText(e.StatusCode))
	}
	return fmt.Sprintf("Keycloak %s returned %s: %s", e.Op, http.StatusText(e.StatusCode), e.Body)
}

// IsNotFound reports whether err is a Keycloak 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsConflict reports whether err is a Keycloak 409.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// IsBadRequest reports whether err is a Keycloak 400.
func IsBadRequest(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest
}

// IsForbidden reports whether err is a Keycloak 401 or 403.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

// ListOrganizations lists Keycloak Organizations in the configured realm.
func (c *Client) ListOrganizations(ctx context.Context) ([]Organization, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := c.orgsURL() + "?first=0&max=" + strconv.Itoa(100)
	var organizations []Organization
	if err := c.doJSON(ctx, http.MethodGet, endpoint, token, nil, &organizations, "list organizations"); err != nil {
		return nil, err
	}
	return organizations, nil
}

// GetOrganization returns one Keycloak Organization by id.
func (c *Client) GetOrganization(ctx context.Context, id string) (Organization, error) {
	token, err := c.token(ctx)
	if err != nil {
		return Organization{}, err
	}
	var organization Organization
	if err := c.doJSON(ctx, http.MethodGet, c.orgURL(id), token, nil, &organization, "get organization"); err != nil {
		return Organization{}, err
	}
	return organization, nil
}

// CreateOrganization creates a Keycloak Organization and returns the stored representation.
func (c *Client) CreateOrganization(ctx context.Context, org Organization) (Organization, error) {
	token, err := c.token(ctx)
	if err != nil {
		return Organization{}, err
	}
	if org.Alias == "" {
		org.Alias = org.Name
	}
	payload, err := json.Marshal(org)
	if err != nil {
		return Organization{}, fmt.Errorf("marshal Keycloak organization: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.orgsURL(), bytes.NewReader(payload))
	if err != nil {
		return Organization{}, fmt.Errorf("create Keycloak organization request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Organization{}, fmt.Errorf("request Keycloak create organization: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Organization{}, fmt.Errorf("read Keycloak create organization response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Organization{}, &APIError{Op: "create organization", StatusCode: resp.StatusCode, Body: truncateDiagnostic(body)}
	}

	id := organizationIDFromLocation(resp.Header.Get("Location"))
	if id == "" {
		// Some Keycloak versions omit Location; fall back to exact name search.
		found, err := c.findOrganizationByName(ctx, token, org.Name)
		if err != nil {
			return Organization{}, err
		}
		return found, nil
	}
	return c.GetOrganization(ctx, id)
}

// UpdateOrganization replaces a Keycloak Organization representation.
func (c *Client) UpdateOrganization(ctx context.Context, org Organization) (Organization, error) {
	if org.ID == "" {
		return Organization{}, fmt.Errorf("organization id is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return Organization{}, err
	}
	// Do not invent Alias from Name on update: Keycloak treats alias as immutable
	// once set. Callers should pass the representation from GetOrganization.
	payload, err := json.Marshal(org)
	if err != nil {
		return Organization{}, fmt.Errorf("marshal Keycloak organization: %w", err)
	}
	if err := c.doJSON(ctx, http.MethodPut, c.orgURL(org.ID), token, payload, nil, "update organization"); err != nil {
		return Organization{}, err
	}
	return c.GetOrganization(ctx, org.ID)
}

// DeleteOrganization deletes a Keycloak Organization by id.
func (c *Client) DeleteOrganization(ctx context.Context, id string) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodDelete, c.orgURL(id), token, nil, nil, "delete organization")
}

// ListOrganizationMembers lists users that belong to a Keycloak Organization.
// Uses first/max pagination against the Admin API (default first=0, max=100).
func (c *Client) ListOrganizationMembers(ctx context.Context, orgID string) ([]OrganizationMember, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("organization id is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := c.orgMembersURL(orgID) + "?first=0&max=" + strconv.Itoa(100)
	var members []OrganizationMember
	if err := c.doJSON(ctx, http.MethodGet, endpoint, token, nil, &members, "list organization members"); err != nil {
		return nil, err
	}
	return members, nil
}

// DomainNames returns the domain name strings for an organization.
func (o Organization) DomainNames() []string {
	domains := make([]string, 0, len(o.Domains))
	for _, domain := range o.Domains {
		domains = append(domains, domain.Name)
	}
	return domains
}

func (c *Client) token(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {c.username}, "password": {c.password},
	}
	tokenRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/realms/"+url.PathEscape(c.adminRealm)+"/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create Keycloak token request: %w", err)
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResponseBody, err := c.httpClient.Do(tokenRequest)
	if err != nil {
		return "", fmt.Errorf("request Keycloak token: %w", err)
	}
	defer tokenResponseBody.Body.Close()
	body, err := io.ReadAll(tokenResponseBody.Body)
	if err != nil {
		return "", fmt.Errorf("read Keycloak token response: %w", err)
	}
	if tokenResponseBody.StatusCode/100 != 2 {
		return "", fmt.Errorf("keycloak token request returned %s: %s", tokenResponseBody.Status, truncateDiagnostic(body))
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("decode Keycloak token: %w", err)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("keycloak token response missing access_token")
	}
	return token.AccessToken, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint, token string, body []byte, out any, op string) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("create Keycloak %s request: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request Keycloak %s: %w", op, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read Keycloak %s response: %w", op, err)
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Op: op, StatusCode: resp.StatusCode, Body: truncateDiagnostic(respBody)}
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode Keycloak %s: %w", op, err)
	}
	return nil
}

// truncateDiagnostic caps error-body text used in APIError / log messages.
const diagnosticBodyLimit = 4096

func truncateDiagnostic(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) <= diagnosticBodyLimit {
		return s
	}
	return s[:diagnosticBodyLimit]
}

func (c *Client) findOrganizationByName(ctx context.Context, token, name string) (Organization, error) {
	endpoint := c.orgsURL() + "?exact=true&search=" + url.QueryEscape(name) + "&first=0&max=2"
	var organizations []Organization
	if err := c.doJSON(ctx, http.MethodGet, endpoint, token, nil, &organizations, "search organizations"); err != nil {
		return Organization{}, err
	}
	for _, org := range organizations {
		if org.Name == name {
			return org, nil
		}
	}
	return Organization{}, &APIError{Op: "search organizations", StatusCode: http.StatusNotFound, Body: "organization " + name + " not found after create"}
}

func (c *Client) orgsURL() string {
	return c.baseURL + "/admin/realms/" + url.PathEscape(c.realm) + "/organizations"
}

func (c *Client) orgURL(id string) string {
	return c.orgsURL() + "/" + url.PathEscape(id)
}

func (c *Client) orgMembersURL(id string) string {
	return c.orgURL(id) + "/members"
}

func organizationIDFromLocation(location string) string {
	location = strings.TrimSpace(location)
	if location == "" {
		return ""
	}
	location = strings.TrimRight(location, "/")
	if i := strings.LastIndex(location, "/"); i >= 0 && i+1 < len(location) {
		return location[i+1:]
	}
	return location
}
