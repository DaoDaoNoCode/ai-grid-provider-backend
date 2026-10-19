package keycloak

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// OrganizationsFile is the fixture file the fake reads: a JSON array of
// OrganizationRepresentation, exactly what
// GET /admin/realms/{realm}/organizations?briefRepresentation=false returns.
const OrganizationsFile = "organizations.json"

// Organization is Keycloak's OrganizationRepresentation
// (core/src/main/java/org/keycloak/representations/idm/OrganizationRepresentation.java)
// as the Admin API reads and writes it. Keycloak's admin ObjectMapper omits
// null fields (NON_NULL, mock/research/keycloak.md 4.1), which the omitempty
// tags reproduce: `description` and `redirectUrl` appear only when set,
// `attributes` only in the full representation, `domains` only when the
// organization has one. On input, a nil Attributes or Domains means the key
// was absent or null, which a PUT treats as "leave unchanged".
type Organization struct {
	ID          string               `json:"id,omitempty"`
	Name        string               `json:"name,omitempty"`
	Alias       string               `json:"alias,omitempty"`
	Enabled     *bool                `json:"enabled,omitempty"`
	Description *string              `json:"description,omitempty"`
	RedirectURL *string              `json:"redirectUrl,omitempty"`
	Attributes  *map[string][]string `json:"attributes,omitempty"`
	Domains     *[]Domain            `json:"domains,omitempty"`
}

// Domain is OrganizationDomainRepresentation. `verified` and `autoRedirect`
// are primitives and always present; `identityProviderAlias` only when set.
type Domain struct {
	Name                  string `json:"name"`
	Verified              bool   `json:"verified"`
	IdentityProviderAlias string `json:"identityProviderAlias,omitempty"`
	AutoRedirect          bool   `json:"autoRedirect"`
}

// representation is ModelToRepresentation.toRepresentation(organization,
// brief): the brief form (the list default) leaves out the attributes.
func (o *organization) representation(brief bool) Organization {
	enabled := o.enabled
	rep := Organization{
		ID: o.id, Name: o.name, Alias: o.alias, Enabled: &enabled,
		Description: cloneString(o.description), RedirectURL: cloneString(o.redirectURL),
	}
	if !brief {
		attrs := cloneAttributes(o.attributes)
		rep.Attributes = &attrs
	}
	if len(o.domains) > 0 {
		domains := slices.Clone(o.domains)
		rep.Domains = &domains
	}
	return rep
}

// LoadFixtures reads organizations.json from dir; a missing file means a
// realm without organizations. The file is decoded strictly.
func LoadFixtures(dir string) ([]Organization, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("fixtures directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fixtures directory %s is not a directory", dir)
	}
	raw, err := os.ReadFile(filepath.Join(dir, OrganizationsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var orgs []Organization
	if err := dec.Decode(&orgs); err != nil {
		return nil, fmt.Errorf("%s: %w", OrganizationsFile, err)
	}
	return orgs, nil
}
