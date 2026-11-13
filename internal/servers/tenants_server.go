package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/names"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Keycloak organization attributes this backend owns. Keycloak keeps no
// version or creation time for an organization, so the backend stores them.
const (
	// tenantVersionAttribute is metadata.version, bumped on every write the
	// backend makes. An organization without it (created in Keycloak
	// directly) reads as version 1.
	tenantVersionAttribute = "ai-grid/resource-version"
	// tenantCreateTimeAttribute is metadata.creation_timestamp in RFC 3339,
	// stamped on Create.
	tenantCreateTimeAttribute = "ai-grid/create-time"
)

// Approximate IPv4 / IPv6 rejection for tenant domains (OSAC parity); not a full IP parser.
var (
	ipv4RE      = regexp.MustCompile(`^[0-9.]+$`)
	ipv6LooseRE = regexp.MustCompile(`^[0-9a-fA-F:]+$`)
)

// organizationClient is the Keycloak Organizations + IdP surface used by Tenants.
// Tests inject fakes; production wires *keycloak.Client.
type organizationClient interface {
	ListOrganizations(ctx context.Context) ([]keycloak.Organization, error)
	GetOrganization(ctx context.Context, id string) (keycloak.Organization, error)
	CreateOrganization(ctx context.Context, org keycloak.Organization) (keycloak.Organization, error)
	UpdateOrganization(ctx context.Context, org keycloak.Organization) (keycloak.Organization, error)
	DeleteOrganization(ctx context.Context, id string) error
	ListOrganizationMembers(ctx context.Context, orgID string) ([]keycloak.OrganizationMember, error)
	CreateIdentityProvider(ctx context.Context, idp keycloak.IdentityProvider) error
	GetIdentityProvider(ctx context.Context, alias string) (keycloak.IdentityProvider, error)
	GetOrganizationIdentityProvider(ctx context.Context, orgID, alias string) (keycloak.IdentityProvider, error)
	UpdateIdentityProvider(ctx context.Context, idp keycloak.IdentityProvider) error
	DeleteIdentityProvider(ctx context.Context, alias string) error
	ImportIdentityProviderConfig(ctx context.Context, fromURL string) (map[string]string, error)
	ReloadIdentityProviderKeys(ctx context.Context, alias string) (bool, error)
	LinkOrganizationIdentityProvider(ctx context.Context, orgID, alias string) error
	ConfigureOrganizationIdentityProvider(ctx context.Context, orgID, alias string) error
	UpsertIdentityProviderMapper(ctx context.Context, spec keycloak.IdentityProviderMapperSpec) error
	ListIdentityProviderMappers(ctx context.Context, alias string) ([]keycloak.IdentityProviderMapper, error)
	CountIdentityProviderUsers(ctx context.Context, alias string) (int, error)
}

// TenantsServerBuilder configures a TenantsServer.
type TenantsServerBuilder struct {
	logger  *slog.Logger
	tenants organizationClient
	secrets SecretReader
}

// TenantsServer implements ai_grid.v1.Tenants.
type TenantsServer struct {
	aigridv1connect.UnimplementedTenantsHandler
	logger  *slog.Logger
	tenants organizationClient
	secrets SecretReader
	now     func() time.Time

	// writeLocks serializes writes per organization id. Update and Delete
	// hold one from their read to their write, so two writes through this
	// backend can't both pass the version check; Keycloak has no conditional
	// write. It works because the backend runs one replica and only it bumps
	// versions. Revisit it before running more than one. A fixed set, picked
	// by a hash of the id, keeps memory bounded whatever ids callers send; two
	// tenants that share a lock only wait for each other. Each lock is a
	// one-slot channel, so a waiting request gives up when its context ends.
	writeLocks [64]chan struct{}
}

// lockTenant serializes this backend's writes to one organization and
// returns the unlock function, or an error when ctx ends first.
func (s *TenantsServer) lockTenant(ctx context.Context, id string) (func(), error) {
	h := fnv.New32a()
	h.Write([]byte(id))
	lock := s.writeLocks[h.Sum32()%uint32(len(s.writeLocks))]
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		code := connect.CodeCanceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = connect.CodeDeadlineExceeded
		}
		return nil, connect.Errorf(code, "wait for tenant %q: %v", id, ctx.Err())
	}
}

var _ aigridv1connect.TenantsHandler = (*TenantsServer)(nil)

// NewTenantsServer creates a builder for TenantsServer.
func NewTenantsServer() *TenantsServerBuilder {
	return &TenantsServerBuilder{}
}

// SetLogger sets the logger. Optional; slog.Default() when unset.
func (b *TenantsServerBuilder) SetLogger(logger *slog.Logger) *TenantsServerBuilder {
	b.logger = logger
	return b
}

// SetOrganizationClient sets the Keycloak organization client. Mandatory.
func (b *TenantsServerBuilder) SetOrganizationClient(client organizationClient) *TenantsServerBuilder {
	b.tenants = client
	return b
}

// SetSecretReader sets the reader for IdP client secrets. Required only when a
// Create or Update request writes an identity provider.
func (b *TenantsServerBuilder) SetSecretReader(reader SecretReader) *TenantsServerBuilder {
	b.secrets = reader
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
	server := &TenantsServer{logger: logger, tenants: b.tenants, secrets: b.secrets, now: time.Now}
	for i := range server.writeLocks {
		server.writeLocks[i] = make(chan struct{}, 1)
	}
	return server, nil
}

// List returns Keycloak Organizations from the configured realm.
func (s *TenantsServer) List(ctx context.Context, req *aigridv1.TenantsListRequest) (*aigridv1.TenantsListResponse, error) {
	if s.tenants == nil {
		return nil, connect.Errorf(connect.CodeInternal, "keycloak tenant client is not configured")
	}
	organizations, err := s.tenants.ListOrganizations(ctx)
	if err != nil {
		return nil, mapTenantKeycloakError("list", "", err)
	}
	items := make([]*aigridv1.Tenant, 0, len(organizations))
	for _, organization := range organizations {
		items = append(items, s.projectTenant(ctx, organization))
	}
	page, total := applyListWindow(items, req.Offset, req.Limit)
	return &aigridv1.TenantsListResponse{
		Size:  int32(len(page)),
		Total: total,
		Items: page,
	}, nil
}

// Get returns a single tenant by Keycloak Organization id.
func (s *TenantsServer) Get(ctx context.Context, req *aigridv1.TenantsGetRequest) (*aigridv1.TenantsGetResponse, error) {
	id := req.GetId()
	if err := validateTenantID(id); err != nil {
		return nil, err
	}
	organization, err := s.tenants.GetOrganization(ctx, id)
	if err != nil {
		return nil, mapTenantKeycloakError("get", id, err)
	}
	return &aigridv1.TenantsGetResponse{Object: s.projectTenant(ctx, organization)}, nil
}

// projectTenant maps an organization to a Tenant. A stored identity provider
// that does not decode is logged and left off the response, so one bad
// attribute does not fail List or Get for every organization.
func (s *TenantsServer) projectTenant(ctx context.Context, organization keycloak.Organization) *aigridv1.Tenant {
	tenant, err := tenantFromOrganization(organization)
	if err == nil {
		return tenant
	}
	s.logger.WarnContext(ctx, "tenant identity provider attribute ignored", "id", organization.ID, "error", err)
	stripped := organization
	if stripped.Attributes != nil {
		attrs := maps.Clone(stripped.Attributes)
		delete(attrs, tenantIdentityProviderAttribute)
		stripped.Attributes = attrs
	}
	tenant, err = tenantFromOrganization(stripped)
	if err != nil {
		// tenantFromOrganization only fails while decoding the attribute.
		s.logger.ErrorContext(ctx, "tenant projection failed after dropping identity provider", "id", organization.ID, "error", err)
		return &aigridv1.Tenant{Id: organization.ID}
	}
	return tenant
}

// Create creates a Keycloak Organization for the tenant.
// metadata.name is required (RFC 1123). Keycloak assigns the organization id.
// When spec.identity_provider is set, Keycloak first checks the issuer's discovery
// document, then the realm IdP is created, its signing keys checked, and it is
// linked with its mappers; any failure removes what was created.
func (s *TenantsServer) Create(ctx context.Context, req *aigridv1.TenantsCreateRequest) (*aigridv1.TenantsCreateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "object is required")
	}
	name := tenantDisplayName(object)
	if err := validateTenantName(name); err != nil {
		return nil, err
	}
	domains, err := domainsFromTenantSpec(object.GetSpec())
	if err != nil {
		return nil, err
	}
	if err := validateTenantDomains(domains); err != nil {
		return nil, err
	}

	var normalizedIdP *aigridv1.IdentityProvider
	if object.GetSpec().GetIdentityProvider() != nil {
		normalizedIdP, err = normalizeIdentityProvider(name, object.GetSpec().GetIdentityProvider())
		if err != nil {
			return nil, err
		}
		if s.secrets == nil {
			return nil, connect.Errorf(connect.CodeInternal, "identity provider secret reader is not configured")
		}
		if err := s.checkIssuer(ctx, normalizedIdP.GetConfig()); err != nil {
			return nil, err
		}
	}

	attrs := map[string][]string{
		tenantVersionAttribute:    {"1"},
		tenantCreateTimeAttribute: {s.now().UTC().Format(time.RFC3339)},
	}
	if normalizedIdP != nil {
		chunks, err := identityProviderToAttributeValues(normalizedIdP)
		if err != nil {
			return nil, connect.Errorf(connect.CodeInternal, "encode identity provider: %v", err)
		}
		attrs[tenantIdentityProviderAttribute] = chunks
	}

	org := keycloak.Organization{
		Name:       name,
		Alias:      name,
		Domains:    domains,
		Attributes: attrs,
	}
	created, err := s.tenants.CreateOrganization(ctx, org)
	if err != nil {
		return nil, mapTenantKeycloakError("create", name, err)
	}
	if normalizedIdP != nil {
		idpCreated, setupErr := s.setupIdentityProvider(ctx, created, normalizedIdP)
		if setupErr == nil {
			// Only now that the IdP is linked may a domain name it.
			created.Domains = routeDomainsToIdentityProvider(created.Domains, identityProviderAliasOf(created))
			created, setupErr = s.updateRoutedOrganization(ctx, created)
		}
		if setupErr != nil {
			return nil, s.cleanupIdentityProviderCreate(ctx, created.ID, created.Alias, idpCreated, setupErr)
		}
	}
	tenant, err := tenantFromOrganization(created)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "project tenant: %v", err)
	}
	return &aigridv1.TenantsCreateResponse{Object: tenant}, nil
}

// Update patches tenant name, domains and/or identity provider according to
// update_mask; an empty mask is inferred from the object (tenantUpdatePaths).
// An identity provider is added to an organization without one, or changed in
// place under the same alias, so the organization, its id, its members and
// their links to the IdP stay. Removing it is refused.
func (s *TenantsServer) Update(ctx context.Context, req *aigridv1.TenantsUpdateRequest) (*aigridv1.TenantsUpdateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "object is required")
	}
	id := object.GetId()
	if err := validateTenantID(id); err != nil {
		return nil, err
	}

	version := object.GetMetadata().GetVersion()
	if version == 0 {
		return nil, errTenantVersionRequired
	}

	unlock, err := s.lockTenant(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := s.tenants.GetOrganization(ctx, id)
	if err != nil {
		return nil, mapTenantKeycloakError("get", id, err)
	}
	// A stale version is Aborted before anything else is checked.
	if version != organizationVersion(current) {
		return nil, staleTenantError("update", id)
	}

	updateName, updateDomains, updateIdP, nameRequired, err := tenantUpdatePaths(req.GetUpdateMask(), req.GetObject())
	if err != nil {
		return nil, err
	}
	storedIdP, err := identityProviderFromOrganizationAttrs(current.Attributes)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read identity provider: %v", err)
	}
	// nextIdP is the identity provider to write: set when the request adds or
	// changes it, nil when it leaves it as stored.
	var nextIdP *aigridv1.IdentityProvider
	if updateIdP {
		nextIdP, err = identityProviderToWrite(current.Name, storedIdP, object.GetSpec().GetIdentityProvider())
		if err != nil {
			return nil, err
		}
	}
	if !updateName && !updateDomains && nextIdP == nil {
		// An unchanged IdP (or an identity/version-only mask): nothing to write.
		tenant, err := tenantFromOrganization(current)
		if err != nil {
			return nil, connect.Errorf(connect.CodeInternal, "project tenant: %v", err)
		}
		return &aigridv1.TenantsUpdateResponse{Object: tenant}, nil
	}
	next := current
	if updateName {
		name := tenantDisplayName(object)
		if name == "" {
			if nameRequired {
				return nil, connect.Errorf(connect.CodeInvalidArgument, "metadata.name is required")
			}
			name = current.Name
		}
		if err := validateTenantName(name); err != nil {
			return nil, err
		}
		// Name is mutable; Alias is immutable in Keycloak once set — keep current.
		next.Name = name
	}
	if updateDomains {
		if object.GetSpec() == nil {
			next.Domains = nil
		} else {
			domains, err := domainsFromTenantSpec(object.GetSpec())
			if err != nil {
				return nil, err
			}
			next.Domains = keepDomainDetails(domains, current.Domains)
		}
		if err := validateTenantDomains(next.Domains); err != nil {
			return nil, err
		}
		if storedIdP != nil {
			next.Domains = routeDomainsToIdentityProvider(next.Domains, identityProviderAliasOf(current))
		}
	}

	// The rest of the representation, attributes this backend doesn't own
	// included, goes back as read.
	next.Attributes = maps.Clone(current.Attributes)
	if next.Attributes == nil {
		next.Attributes = map[string][]string{}
	}
	next.Attributes[tenantVersionAttribute] = []string{strconv.FormatInt(nextVersion(organizationVersion(current)), 10)}

	// The identity provider, its mappers included, is written in Keycloak before
	// the organization, the commit point, and undone if anything up to the
	// commit fails, so the organization never names an IdP that isn't there, or
	// one other than the IdP Keycloak signs its users in with.
	undoIdP := func(err error) error { return err }
	if nextIdP != nil {
		if s.secrets == nil {
			return nil, connect.Errorf(connect.CodeInternal, "identity provider secret reader is not configured")
		}
		// The issuer is checked when it is set: here when the IdP is added; for a change,
		// replaceIdentityProvider checks it against the issuer Keycloak's IdP has now.
		alias := identityProviderAliasOf(current)
		if storedIdP == nil {
			if err := s.checkIssuer(ctx, nextIdP.GetConfig()); err != nil {
				return nil, err
			}
		}
		if storedIdP == nil {
			// A realm IdP under the organization's alias and linked to it, which the
			// organization doesn't name, is what an add leaves when the organization's
			// save failed and couldn't be read again: write the request over it, as a
			// change does, so the retry converges. The organization doesn't name it, so
			// it is undone like a fresh add: deleted, and a retry starts clean. Any other
			// IdP under the alias makes Create below answer AlreadyExists, as before.
			_, err := s.tenants.GetOrganizationIdentityProvider(ctx, current.ID, alias)
			switch {
			case err == nil:
				// Only this backend's own leftover is taken over: an organization made
				// outside this API (imported with its own IdP, say) has a working IdP
				// that this API must neither overwrite nor delete. This backend's IdPs
				// carry its org id mapper, which it applies before it saves the
				// organization; it is matched by name, since another mapper may hold
				// the same org id.
				if err := s.ensureOwnIdentityProvider(ctx, alias); err != nil {
					return nil, err
				}
				// The requested Secret is read before the undo is set, so a request that
				// fails before writing anything deletes nothing.
				if _, err := s.readIdentityProviderSecret(ctx, nextIdP); err != nil {
					return nil, err
				}
				undoIdP = func(err error) error { return s.cleanupIdentityProviderAdd(ctx, alias, true, err) }
				if _, err := s.replaceIdentityProvider(ctx, current, next.Name, nil, nextIdP); err != nil {
					return nil, undoIdP(err)
				}
				if err := s.tenants.ConfigureOrganizationIdentityProvider(ctx, current.ID, alias); err != nil {
					return nil, undoIdP(mapTenantKeycloakError("configure identity provider", alias, err))
				}
			case keycloak.IsNotFound(err):
				// next carries the organization's id and alias, and the name it is
				// about to have, which the mappers hold.
				idpCreated, err := s.setupIdentityProvider(ctx, next, nextIdP)
				if err != nil {
					return nil, s.cleanupIdentityProviderAdd(ctx, alias, idpCreated, err)
				}
				undoIdP = func(err error) error { return s.cleanupIdentityProviderAdd(ctx, alias, true, err) }
			default:
				return nil, mapTenantKeycloakError("get organization identity provider", alias, err)
			}
			// Only now that the IdP is linked may a domain name it.
			next.Domains = routeDomainsToIdentityProvider(next.Domains, alias)
		} else {
			previous, err := s.replaceIdentityProvider(ctx, current, next.Name, storedIdP, nextIdP)
			if err != nil {
				return nil, err
			}
			undoIdP = func(err error) error { return s.restoreIdentityProvider(ctx, current, previous, err) }
		}
		chunks, err := identityProviderToAttributeValues(nextIdP)
		if err != nil {
			return nil, undoIdP(connect.Errorf(connect.CodeInternal, "encode identity provider: %v", err))
		}
		next.Attributes[tenantIdentityProviderAttribute] = chunks
	}

	updated, err := s.tenants.UpdateOrganization(ctx, next)
	if readBack := (*keycloak.ReadBackError)(nil); errors.As(err, &readBack) {
		// The write landed; answer with what was written rather than make
		// the caller retry into a 409 for its own change.
		s.logger.WarnContext(ctx, "tenant updated, but reading it back failed", "id", id, "error", readBack.Err)
		updated, err = readBack.Written, nil
	}
	if err != nil && nextIdP != nil {
		// Undo the IdP only when the organization surely wasn't saved: if it was (its
		// answer lost), undoing would leave it naming an IdP config Keycloak no longer
		// has. This backend alone bumps the version, under the tenant's lock, so the
		// version it wrote says whether the write landed. When that can't be read, nothing
		// is undone: Keycloak has the new IdP and the organization is old or new, and a
		// retry of the same request writes the rest.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), identityProviderCleanupTimeout)
		stored, readErr := s.tenants.GetOrganization(readCtx, id)
		cancel()
		switch {
		case readErr != nil:
			// Nothing is undone, so Keycloak already signs users in with the new IdP config.
			issuerNote := ""
			if storedIdP != nil && nextIdP.GetConfig().GetIssuer() != storedIdP.GetConfig().GetIssuer() {
				issuerNote = fmt.Sprintf("; sign-in already uses the new issuer %q, so don't test sign-in until a reload shows the tenant with it", nextIdP.GetConfig().GetIssuer())
			}
			return nil, connect.Errorf(connect.CodeUnavailable, "update tenant %q: %v; the change may have been saved, and reading the tenant again failed (%v): reload it, and retry if it doesn't show the change%s", id, err, readErr, issuerNote).WithCause(err)
		case organizationVersion(stored) == organizationVersion(next):
			s.logger.WarnContext(ctx, "tenant update answered with an error, but it was saved", "id", id, "error", err)
			updated, err = stored, nil
		}
	}
	if err != nil {
		return nil, undoIdP(mapTenantKeycloakError("update", id, err))
	}
	if updateName && nextIdP == nil {
		// The org name mapper is hardcoded per IdP: refresh it so new logins get the new
		// org_name. Done on every name update, not only a change, so a retry repairs a
		// failed refresh. An added or changed IdP got its mappers, with the new name,
		// before the organization was written.
		if err := s.refreshIdentityProviderMappers(ctx, updated); err != nil {
			return nil, err
		}
	}
	tenant, err := tenantFromOrganization(updated)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "project tenant: %v", err)
	}
	return &aigridv1.TenantsUpdateResponse{Object: tenant}, nil
}

// identityProviderAliasOf is the alias of the IdP linked to an organization:
// the organization's alias, or its name when it has none.
func identityProviderAliasOf(org keycloak.Organization) string {
	if org.Alias != "" {
		return org.Alias
	}
	return org.Name
}

// routeDomainsToIdentityProvider points every domain at the organization's
// identity provider with autoRedirect, so Keycloak's identity-first login sends
// a user whose email domain matches straight to the tenant's login server
// instead of showing a button.
func routeDomainsToIdentityProvider(domains []keycloak.OrganizationDomain, alias string) []keycloak.OrganizationDomain {
	routed := make([]keycloak.OrganizationDomain, len(domains))
	for i, d := range domains {
		d.IdentityProviderAlias = alias
		d.AutoRedirect = true
		routed[i] = d
	}
	return routed
}

// updateRoutedOrganization writes the organization back after Create routed its
// domains, accepting a write whose read-back failed.
func (s *TenantsServer) updateRoutedOrganization(ctx context.Context, org keycloak.Organization) (keycloak.Organization, error) {
	updated, err := s.tenants.UpdateOrganization(ctx, org)
	if readBack := (*keycloak.ReadBackError)(nil); errors.As(err, &readBack) {
		s.logger.WarnContext(ctx, "tenant domains routed, but reading it back failed", "id", org.ID, "error", readBack.Err)
		return readBack.Written, nil
	}
	if err != nil {
		return org, mapTenantKeycloakError("route domains to identity provider", org.ID, err)
	}
	return updated, nil
}

// refreshIdentityProviderMappers upserts the broker mappers of an organization's linked
// identity provider with its current id and name. An organization without one is left alone.
func (s *TenantsServer) refreshIdentityProviderMappers(ctx context.Context, org keycloak.Organization) error {
	storedIdP, err := identityProviderFromOrganizationAttrs(org.Attributes)
	if err != nil {
		return connect.Errorf(connect.CodeInternal, "read identity provider: %v", err)
	}
	if storedIdP == nil {
		return nil
	}
	alias := identityProviderAliasOf(org)
	if err := s.tenants.UpsertIdentityProviderMapper(ctx, keycloak.IdentityProviderMapperSpec{
		Alias: alias, OrganizationID: org.ID, OrganizationName: org.Name,
	}); err != nil {
		return mapTenantKeycloakError("update identity provider mappers", alias, err)
	}
	return nil
}

// errTenantVersionRequired answers an Update or Delete without a version.
// Every organization reads as version 1 or more, so a caller that read the
// tenant always has one.
var errTenantVersionRequired = connect.Errorf(connect.CodeInvalidArgument, "a version is required: send the organization's metadata.version")

// Delete deletes a Keycloak Organization by id.
func (s *TenantsServer) Delete(ctx context.Context, req *aigridv1.TenantsDeleteRequest) (*aigridv1.TenantsDeleteResponse, error) {
	id := req.GetId()
	if err := validateTenantID(id); err != nil {
		return nil, err
	}
	version := req.GetVersion()
	if version == 0 {
		return nil, errTenantVersionRequired
	}
	unlock, err := s.lockTenant(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := s.tenants.GetOrganization(ctx, id)
	if err != nil {
		return nil, mapTenantKeycloakError("get", id, err)
	}
	if version != organizationVersion(current) {
		return nil, staleTenantError("delete", id)
	}
	storedIdP, err := identityProviderFromOrganizationAttrs(current.Attributes)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read identity provider: %v", err)
	}
	if storedIdP != nil {
		alias := current.Alias
		if alias == "" {
			alias = current.Name
		}
		if err := s.tenants.DeleteIdentityProvider(ctx, alias); err != nil {
			return nil, mapTenantKeycloakError("delete identity provider", alias, err)
		}
	}
	if err := s.tenants.DeleteOrganization(ctx, id); err != nil {
		return nil, mapTenantKeycloakError("delete", id, err)
	}
	return &aigridv1.TenantsDeleteResponse{}, nil
}

// ListMembers returns Keycloak users that belong to the tenant Organization.
func (s *TenantsServer) ListMembers(ctx context.Context, req *aigridv1.TenantsListMembersRequest) (*aigridv1.TenantsListMembersResponse, error) {
	id := req.GetId()
	if err := validateTenantID(id); err != nil {
		return nil, err
	}
	// Ensure the tenant exists so missing orgs map to NotFound (not an empty list).
	if _, err := s.tenants.GetOrganization(ctx, id); err != nil {
		return nil, mapTenantKeycloakError("get", id, err)
	}
	members, err := s.tenants.ListOrganizationMembers(ctx, id)
	if err != nil {
		return nil, mapTenantKeycloakError("list members", id, err)
	}
	items := make([]*aigridv1.TenantMember, 0, len(members))
	for _, member := range members {
		items = append(items, tenantMemberFromOrganizationMember(member))
	}
	page, total := applyListWindow(items, req.Offset, req.Limit)
	return &aigridv1.TenantsListMembersResponse{
		Size:  int32(len(page)),
		Total: total,
		Items: page,
	}, nil
}

func tenantFromOrganization(organization keycloak.Organization) (*aigridv1.Tenant, error) {
	idp, err := identityProviderFromOrganizationAttrs(organization.Attributes)
	if err != nil {
		return nil, err
	}
	return &aigridv1.Tenant{
		Id: organization.ID,
		Metadata: &aigridv1.Metadata{
			Name:              organization.Name,
			Version:           organizationVersion(organization),
			CreationTimestamp: organizationCreateTime(organization),
		},
		Spec: &aigridv1.TenantSpec{
			Domains:          organization.DomainNames(),
			IdentityProvider: idp,
		},
		Status: &aigridv1.TenantStatus{},
	}, nil
}

// organizationVersion reads the version attribute. An organization without
// one, or with a value that isn't a positive int32, is version 1, so a client
// can lock even its first update.
func organizationVersion(organization keycloak.Organization) int32 {
	values := organization.Attributes[tenantVersionAttribute]
	if len(values) == 0 {
		return 1
	}
	v, err := strconv.ParseInt(values[0], 10, 32)
	if err != nil || v < 1 {
		return 1
	}
	return int32(v)
}

// nextVersion is the version after v, wrapping to 2 past the int32 maximum
// so it never returns 0 (no check) or 1 (an unstamped organization).
func nextVersion(v int32) int64 {
	if v == math.MaxInt32 {
		return 2
	}
	return int64(v) + 1
}

// organizationCreateTime reads the creation-time attribute; nil when it's
// missing, not RFC 3339, or outside the range a protobuf Timestamp holds.
func organizationCreateTime(organization keycloak.Organization) *timestamppb.Timestamp {
	values := organization.Attributes[tenantCreateTimeAttribute]
	if len(values) == 0 {
		return nil
	}
	t, err := time.Parse(time.RFC3339, values[0])
	if err != nil {
		return nil
	}
	ts := timestamppb.New(t)
	if ts.CheckValid() != nil {
		return nil
	}
	return ts
}

// keepDomainDetails carries each kept domain's stored representation
// (verified, identity provider link, keys the client doesn't model) over to
// the new list, so an update that only adds or removes domains leaves the
// others as they were.
func keepDomainDetails(next, current []keycloak.OrganizationDomain) []keycloak.OrganizationDomain {
	for i, d := range next {
		for _, c := range current {
			if c.Name == d.Name {
				next[i] = c
				break
			}
		}
	}
	return next
}

// staleTenantError is the Aborted error for a write against an old version.
func staleTenantError(op, id string) *connect.Error {
	return connect.Errorf(connect.CodeAborted, "%s tenant %q: the tenant changed since it was read; reload and retry", op, id)
}

func tenantMemberFromOrganizationMember(member keycloak.OrganizationMember) *aigridv1.TenantMember {
	preferredUsername := ""
	if values := member.Attributes["maas_preferred_username"]; len(values) > 0 {
		preferredUsername = values[0]
	}
	return &aigridv1.TenantMember{
		Id:                member.ID,
		Username:          member.Username,
		Email:             member.Email,
		FirstName:         member.FirstName,
		LastName:          member.LastName,
		Enabled:           member.Enabled,
		PreferredUsername: preferredUsername,
	}
}

// tenantDisplayName returns metadata.name only. Callers must not synthesize a
// name from the organization id — that defeats empty-name preservation on Update.
func tenantDisplayName(object *aigridv1.Tenant) string {
	if object.GetMetadata() == nil {
		return ""
	}
	return object.GetMetadata().GetName()
}

func domainsFromTenantSpec(spec *aigridv1.TenantSpec) ([]keycloak.OrganizationDomain, error) {
	if spec == nil {
		return nil, nil
	}
	domains := make([]keycloak.OrganizationDomain, 0, len(spec.GetDomains()))
	for _, domain := range spec.GetDomains() {
		trimmed := strings.TrimSpace(domain)
		if trimmed == "" {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.domains entries must be non-empty")
		}
		domains = append(domains, keycloak.OrganizationDomain{Name: trimmed})
	}
	return domains, nil
}

// tenantUpdatePaths reports which mutable fields an Update changes.
// Supported paths: metadata, metadata.name, spec, spec.domains (optional object. prefix).
// id is accepted as a no-op (REST PATCH body masks often include it).
// Unsupported paths are rejected. nameRequired is true when an explicit mask
// selects the name (empty mask updates name but may preserve when absent).
// tenantUpdatePaths decides which fields an Update writes.
//
// An empty mask is inferred from the object, as the Clusters handler does: a
// REST PATCH arrives without one, because vanguard does not synthesise a mask
// from the JSON body the way grpc-gateway did. Treating an empty mask as "write
// everything" made a name-only PATCH clear the tenant's domains.
func tenantUpdatePaths(mask *fieldmaskpb.FieldMask, object *aigridv1.Tenant) (name, domains, idp, nameRequired bool, err error) {
	if mask == nil || len(mask.GetPaths()) == 0 {
		// Only a name counts: metadata holding just the version or creation
		// time names nothing to write, so it stays a no-op.
		//
		// spec != nil used to mean "replace domains". A REST PATCH that sends
		// only spec.identityProvider still has an empty domains list (proto3
		// has no presence for it), so that cleared the domains and never
		// checked the identity provider. An identity provider with no domains
		// is an IdP check, not a domain write. Domains are still replaced
		// when the body has no identity provider, including an empty list.
		spec := object.GetSpec()
		idp := spec.GetIdentityProvider() != nil
		domains := spec != nil && (spec.GetIdentityProvider() == nil || len(spec.GetDomains()) > 0)
		return object.GetMetadata().GetName() != "", domains, idp, false, nil
	}
	for _, path := range mask.GetPaths() {
		p := strings.TrimPrefix(path, "object.")
		switch p {
		case "id", "metadata.version", "metadata.creation_timestamp":
			// Identity is taken from the path / object.id, the version is a
			// precondition, and the creation time is output only.
		case "metadata", "metadata.name":
			name = true
			nameRequired = true
		case "spec":
			domains = true
			idp = true
		case "spec.domains":
			domains = true
		case "spec.identity_provider":
			idp = true
		default:
			return false, false, false, false, connect.Errorf(connect.CodeInvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	return name, domains, idp, nameRequired, nil
}

func validateTenantID(id string) error {
	if strings.TrimSpace(id) == "" {
		return connect.Errorf(connect.CodeInvalidArgument, "id is required")
	}
	return nil
}

func validateTenantName(name string) error {
	if name == "" {
		return connect.Errorf(connect.CodeInvalidArgument, "metadata.name is required")
	}
	if !names.ValidDNSLabel(name) {
		return connect.Errorf(connect.CodeInvalidArgument, "metadata.name %q is not a valid RFC 1123 DNS label", name)
	}
	return nil
}

// validateTenantDomains checks domains toward OSAC rules: DNS hostname (not IP),
// at least two labels, lowercase DNS labels, max lengths, no duplicates. An
// organization needs at least one domain: Keycloak finds a user's organization,
// and so its identity provider, from the domain of the email they sign in with.
func validateTenantDomains(domains []keycloak.OrganizationDomain) error {
	if len(domains) == 0 {
		return connect.Errorf(connect.CodeInvalidArgument, "spec.domains must contain at least one domain")
	}
	seen := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.domains entries must be non-empty")
		}
		if len(name) > 253 {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.domains entry %q exceeds 253 characters", name)
		}
		if ipv4RE.MatchString(name) || ipv6LooseRE.MatchString(name) {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.domains entry %q must be a DNS hostname, not an IP address", name)
		}
		if !strings.Contains(name, ".") {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.domains entry %q must have at least two labels (e.g. example.com)", name)
		}
		labels := strings.Split(name, ".")
		for _, label := range labels {
			if !names.ValidDNSLabel(label) {
				return connect.Errorf(connect.CodeInvalidArgument, "spec.domains entry %q has invalid DNS label %q (lowercase a-z/0-9/hyphen only)", name, label)
			}
		}
		if _, ok := seen[name]; ok {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.domains contains duplicate %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func mapTenantKeycloakError(op, id string, err error) error {
	var mapperConflict *keycloak.UsernameMapperConflictError
	switch {
	case errors.As(err, &mapperConflict):
		return connect.Errorf(connect.CodeFailedPrecondition, "%s tenant %q: %v", op, id, err).WithCause(err)
	case keycloak.IsNotFound(err):
		if id == "" {
			return connect.Errorf(connect.CodeNotFound, "tenant not found").WithCause(err)
		}
		return connect.Errorf(connect.CodeNotFound, "tenant %q not found", id).WithCause(err)
	case keycloak.IsConflict(err) && op == "update":
		// On an update Keycloak's 409 means the new name or a domain belongs
		// to another organization; id is this tenant's own.
		return connect.Errorf(connect.CodeAlreadyExists, "update tenant %q: the name or a domain is already used by another organization", id).WithCause(err)
	case keycloak.IsConflict(err):
		if id == "" {
			return connect.Errorf(connect.CodeAlreadyExists, "tenant already exists").WithCause(err)
		}
		return connect.Errorf(connect.CodeAlreadyExists, "tenant %q already exists", id).WithCause(err)
	case keycloak.IsBadRequest(err):
		return connect.Errorf(connect.CodeInvalidArgument, "%s tenant %q: %v", op, id, err).WithCause(err)
	case keycloak.IsForbidden(err):
		return connect.Errorf(connect.CodePermissionDenied, "%s tenant %q: %v", op, id, err).WithCause(err)
	default:
		// Coded, not bare: connect-go v2 scrubs an uncoded error to "unknown"
		// with an empty message, so a Keycloak outage would reach the console
		// blank.
		if id == "" {
			return connect.Errorf(connect.CodeInternal, "%s tenants: %v", op, err).WithCause(err)
		}
		return connect.Errorf(connect.CodeInternal, "%s tenant %q: %v", op, id, err).WithCause(err)
	}
}

// refuseIssuerChangeWithLinkedUsers refuses an issuer change once anyone has signed in through
// the IdP. Keycloak links a member to the IdP by its alias and the subject the IdP sends, not by
// issuer, so after an issuer change a user of the new issuer whose subject matches a member's
// link would sign in as that member. A wrong issuer lets nobody sign in, so it has no linked
// users and can still be corrected.
func (s *TenantsServer) refuseIssuerChangeWithLinkedUsers(ctx context.Context, alias string) error {
	linked, err := s.tenants.CountIdentityProviderUsers(ctx, alias)
	if err != nil {
		return mapTenantKeycloakError("count identity provider users", alias, err)
	}
	if linked > 0 {
		return connect.Errorf(connect.CodeFailedPrecondition, "spec.identity_provider.config.issuer can't change after members have signed in through identity provider %q (%d linked): Keycloak links a member by the provider alias and the subject the provider sends, so a user of the new issuer with a matching subject would sign in as that member. To correct it, create a new organization, or remove the members' links to the identity provider in Keycloak first", alias, linked)
	}
	return nil
}

// ensureOwnIdentityProvider refuses an IdP this backend didn't create: one without the org id
// mapper every IdP it creates gets.
func (s *TenantsServer) ensureOwnIdentityProvider(ctx context.Context, alias string) error {
	mappers, err := s.tenants.ListIdentityProviderMappers(ctx, alias)
	if err != nil {
		return mapTenantKeycloakError("list identity provider mappers", alias, err)
	}
	for _, m := range mappers {
		if m.Name == keycloak.IdentityProviderOrgIDMapperName {
			return nil
		}
	}
	return connect.Errorf(connect.CodeFailedPrecondition, "the organization already has identity provider %q, which this API didn't create; manage it in Keycloak, or remove it there before adding one here", alias)
}

// readIdentityProviderSecret reads the client secret an IdP's config names.
func (s *TenantsServer) readIdentityProviderSecret(ctx context.Context, idp *aigridv1.IdentityProvider) ([]byte, error) {
	secret, err := s.secrets.Read(ctx, idp.GetConfig().GetClientSecretSecretName())
	if err != nil {
		if IsSecretNotFound(err) {
			return nil, connect.Errorf(connect.CodeNotFound, "identity provider secret %q not found", idp.GetConfig().GetClientSecretSecretName()).WithCause(err)
		}
		if IsSecretWithoutClientSecret(err) {
			return nil, connect.Errorf(connect.CodeFailedPrecondition, "identity provider secret %q has no client secret: %v; put the IdP client's secret under key %q", idp.GetConfig().GetClientSecretSecretName(), err, idpClientSecretKey).WithCause(err)
		}
		return nil, connect.Errorf(connect.CodeInternal, "read identity provider secret: %v", err).WithCause(err)
	}
	return secret, nil
}

// checkIssuer has Keycloak read the issuer's OIDC discovery document, from Keycloak's own
// network as it does at sign-in, and refuses an issuer the document doesn't name: Keycloak
// rejects every ID token whose iss isn't the configured issuer. It writes nothing.
func (s *TenantsServer) checkIssuer(ctx context.Context, cfg *aigridv1.OidcConfig) error {
	issuer := cfg.GetIssuer()
	// OpenID Connect Discovery 1.0, section 4: a terminating "/" is removed before appending.
	discovery := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	imported, err := s.tenants.ImportIdentityProviderConfig(ctx, discovery)
	if err != nil {
		var apiErr *keycloak.APIError
		if errors.As(err, &apiErr) && !keycloak.IsForbidden(err) {
			return connect.Errorf(connect.CodeFailedPrecondition, "spec.identity_provider.config.issuer: Keycloak could not read the OIDC discovery document %s; check the issuer URL and that Keycloak can reach it (%v)", discovery, err).WithCause(err)
		}
		return mapTenantKeycloakError("check identity provider issuer", issuer, err)
	}
	if got := imported["issuer"]; got != issuer {
		return connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider.config.issuer %q does not match the issuer %q that its discovery document %s names", issuer, got, discovery)
	}
	return nil
}

// checkSigningKeys has Keycloak load the IdP's signing keys from its jwks_url now, as it does
// at sign-in. The IdP must already be saved in Keycloak; nothing is checked without a jwks_url.
// Only a reload that fails (an unreachable or malformed JWKS) refuses the write. Keycloak
// answering false means it didn't try: the IdP is disabled or has no JWKS URL, or it loaded the
// keys less than minTimeBetweenRequests (10 s) ago, on this node or another. That isn't a bad
// JWKS, so the write stands and the keys load on the next sign-in.
func (s *TenantsServer) checkSigningKeys(ctx context.Context, alias string, cfg *aigridv1.OidcConfig) error {
	if cfg.GetJwksUrl() == "" {
		return nil
	}
	if _, err := s.tenants.ReloadIdentityProviderKeys(ctx, alias); err != nil {
		var apiErr *keycloak.APIError
		if errors.As(err, &apiErr) && !keycloak.IsForbidden(err) {
			return connect.Errorf(connect.CodeFailedPrecondition, "spec.identity_provider.config.jwks_url: Keycloak could not load signing keys from %s; check the JWKS URL and that Keycloak can reach it (%v)", cfg.GetJwksUrl(), err).WithCause(err)
		}
		return mapTenantKeycloakError("check identity provider signing keys", alias, err)
	}
	return nil
}

// keycloakIdentityProvider is the realm IdP this backend writes for a tenant's IdP.
//
// With TrustEmail on, Keycloak (26.8) sets a brokered user's email_verified from this IdP's
// email_verified claim when the claim is present. When it's absent, Keycloak marks the email
// verified for a new user or a changed email. With TrustEmail off, email_verified is false and
// the MaaS Portal's oauth2-proxy refuses the token. The IdP's sync mode is FORCE
// (oidcConfigForKeycloak), so this applies on every login, not only the first.
// So a tenant IdP must send email_verified=true for its users: in the ID token, and also in
// the userinfo response when the IdP has a userinfo URL. If userinfo returns an email without
// email_verified, Keycloak reads the claim as false and TrustEmail doesn't help.
// TrustEmail doesn't change account linking: the realm's first broker login flow matches a
// brokered login to an existing user by email or username without reading email_verified,
// so what a tenant IdP can do by asserting another user's email or username is the same
// with it on or off.
func keycloakIdentityProvider(alias string, idp *aigridv1.IdentityProvider, clientSecret []byte) keycloak.IdentityProvider {
	return keycloak.IdentityProvider{
		Alias:       alias,
		DisplayName: idp.GetTitle(),
		ProviderID:  "oidc",
		Enabled:     idp.GetEnabled(),
		TrustEmail:  true,
		Config:      oidcConfigForKeycloak(idp.GetConfig(), clientSecret),
	}
}

// setupIdentityProvider creates the organization's realm IdP, checks Keycloak can load its
// signing keys, links it to the organization with managed membership and applies the
// mappers. idpCreated reports whether the IdP exists in Keycloak, for the caller's cleanup.
func (s *TenantsServer) setupIdentityProvider(ctx context.Context, org keycloak.Organization, idp *aigridv1.IdentityProvider) (idpCreated bool, err error) {
	secret, err := s.readIdentityProviderSecret(ctx, idp)
	if err != nil {
		return false, err
	}
	alias := identityProviderAliasOf(org)
	kcIdP := keycloakIdentityProvider(alias, idp, secret)
	kcIdP.Extra = map[string]json.RawMessage{
		"firstBrokerLoginFlowAlias": json.RawMessage(strconv.Quote(keycloak.FirstBrokerLoginFlowAlias)),
	}
	if err := s.tenants.CreateIdentityProvider(ctx, kcIdP); err != nil {
		return false, mapTenantKeycloakError("create identity provider", alias, err)
	}
	idpCreated = true
	if err := s.checkSigningKeys(ctx, alias, idp.GetConfig()); err != nil {
		return idpCreated, err
	}
	if err := s.tenants.LinkOrganizationIdentityProvider(ctx, org.ID, alias); err != nil {
		return idpCreated, mapTenantKeycloakError("link identity provider", alias, err)
	}
	if err := s.tenants.ConfigureOrganizationIdentityProvider(ctx, org.ID, alias); err != nil {
		return idpCreated, mapTenantKeycloakError("configure identity provider", alias, err)
	}
	if err := s.tenants.UpsertIdentityProviderMapper(ctx, keycloak.IdentityProviderMapperSpec{
		Alias: alias, OrganizationID: org.ID, OrganizationName: org.Name,
	}); err != nil {
		return idpCreated, mapTenantKeycloakError("upsert identity provider mapper", alias, err)
	}
	return idpCreated, nil
}

// identityProviderCleanupTimeout bounds rollback after the caller's context
// is already canceled. It matches the Keycloak admin client's own timeout.
const identityProviderCleanupTimeout = 30 * time.Second

func (s *TenantsServer) cleanupIdentityProviderCreate(ctx context.Context, orgID, alias string, idpCreated bool, setupErr error) error {
	// The caller may have canceled mid-create. Cleanup still has to remove the
	// organization and identity provider, or a retry collides with what was left behind.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), identityProviderCleanupTimeout)
	defer cancel()
	var idpCleanupErr, orgCleanupErr error
	if idpCreated {
		idpCleanupErr = s.tenants.DeleteIdentityProvider(ctx, alias)
	}
	orgCleanupErr = s.tenants.DeleteOrganization(ctx, orgID)
	if idpCleanupErr == nil && orgCleanupErr == nil {
		return setupErr
	}
	if idpCleanupErr != nil && orgCleanupErr != nil {
		return connect.Errorf(connect.CodeInternal, "identity provider setup failed (%v); cleanup also failed (identity provider: %v; organization: %v)", setupErr, idpCleanupErr, orgCleanupErr)
	}
	if idpCleanupErr != nil {
		return connect.Errorf(connect.CodeInternal, "identity provider setup failed (%v); identity provider cleanup also failed: %v", setupErr, idpCleanupErr)
	}
	return connect.Errorf(connect.CodeInternal, "identity provider setup failed (%v); organization cleanup also failed: %v", setupErr, orgCleanupErr)
}

// cleanupIdentityProviderAdd deletes the IdP an Update added when a later step failed.
// Keycloak drops the IdP's organization link and mappers with it; the organization stays.
func (s *TenantsServer) cleanupIdentityProviderAdd(ctx context.Context, alias string, idpCreated bool, setupErr error) error {
	if !idpCreated {
		return setupErr
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), identityProviderCleanupTimeout)
	defer cancel()
	if err := s.tenants.DeleteIdentityProvider(ctx, alias); err != nil {
		return connect.Errorf(connect.CodeInternal, "identity provider setup failed (%v); identity provider cleanup also failed: %v", setupErr, err)
	}
	return setupErr
}

// replaceIdentityProvider writes a changed IdP over the organization's realm IdP, under the
// same alias, so Keycloak keeps its organization link and its users' federated links. Keys
// this backend doesn't own are written back as read. It re-reads the client secret the new
// config names, checks Keycloak can load the new signing keys and re-applies the mappers with
// the organization's id and new name, restoring the previous IdP when any of that fails. It
// returns the IdP as it was, with its client secret, for restoreIdentityProvider.
//
// stored is nil when an add takes over an IdP the organization doesn't name: there is no
// previous IdP of the tenant to restore, so a failure is returned as it is, and the caller
// deletes the IdP as it does a fresh add's.
func (s *TenantsServer) replaceIdentityProvider(ctx context.Context, current keycloak.Organization, name string, stored, idp *aigridv1.IdentityProvider) (keycloak.IdentityProvider, error) {
	alias := identityProviderAliasOf(current)
	previous, err := s.tenants.GetIdentityProvider(ctx, alias)
	if keycloak.IsNotFound(err) {
		return keycloak.IdentityProvider{}, connect.Errorf(connect.CodeFailedPrecondition, "identity provider %q is missing in Keycloak; it was deleted outside this API", alias).WithCause(err)
	}
	if err != nil {
		return keycloak.IdentityProvider{}, mapTenantKeycloakError("get identity provider", alias, err)
	}
	// Whether the issuer changes is judged against the issuer Keycloak's IdP has now, not the
	// one the organization names: they differ after an answer that left the write unconfirmed,
	// a failed restore, or an edit in Keycloak, and Keycloak signs users in with its own. A
	// change that keeps that issuer isn't checked again, so an issuer whose discovery document
	// stopped answering doesn't block a correction of the rest. An add checked it already.
	if idp.GetConfig().GetIssuer() != previous.Config["issuer"] {
		if err := s.refuseIssuerChangeWithLinkedUsers(ctx, alias); err != nil {
			return keycloak.IdentityProvider{}, err
		}
		if stored != nil {
			if err := s.checkIssuer(ctx, idp.GetConfig()); err != nil {
				return keycloak.IdentityProvider{}, err
			}
		}
	}
	secret, err := s.readIdentityProviderSecret(ctx, idp)
	if err != nil {
		return keycloak.IdentityProvider{}, err
	}
	// Keycloak never returns the client secret, so the one to restore on failure is read now,
	// from the Secret the stored IdP names, before anything is written.
	var previousSecret []byte
	if stored != nil {
		previousSecret, err = s.secrets.Read(ctx, stored.GetConfig().GetClientSecretSecretName())
		if err != nil {
			code := connect.CodeInternal
			if IsSecretNotFound(err) || IsSecretWithoutClientSecret(err) {
				code = connect.CodeFailedPrecondition
			}
			// Not the requested Secret (that one missing is NotFound): the one the stored IdP
			// names, which holds the previous client secret a failed change restores.
			return keycloak.IdentityProvider{}, connect.Errorf(code, "the previous client secret, needed to restore the identity provider if this change fails, can't be read from Secret %q, which the stored identity provider names: %v; nothing was changed. Recreate Secret %q with the client secret Keycloak has now, under key %q, then retry", stored.GetConfig().GetClientSecretSecretName(), err, stored.GetConfig().GetClientSecretSecretName(), idpClientSecretKey).WithCause(err)
		}
	}
	owned := keycloakIdentityProvider(alias, idp, secret)
	next := previous
	next.DisplayName, next.ProviderID, next.Enabled, next.TrustEmail = owned.DisplayName, owned.ProviderID, owned.Enabled, owned.TrustEmail
	next.Config = maps.Clone(previous.Config)
	if next.Config == nil {
		next.Config = map[string]string{}
	}
	for _, key := range oidcOptionalConfigKeys {
		delete(next.Config, key)
	}
	maps.Copy(next.Config, owned.Config)
	previous.Config = maps.Clone(previous.Config)
	if previous.Config == nil {
		previous.Config = map[string]string{}
	}
	previous.Config["clientSecret"] = string(previousSecret)
	restore := func(err error) error {
		if stored == nil {
			return err
		}
		return s.restoreIdentityProvider(ctx, current, previous, err)
	}
	if err := s.tenants.UpdateIdentityProvider(ctx, next); err != nil {
		return keycloak.IdentityProvider{}, mapTenantKeycloakError("update identity provider", alias, err)
	}
	if err := s.checkSigningKeys(ctx, alias, idp.GetConfig()); err != nil {
		return keycloak.IdentityProvider{}, restore(err)
	}
	if err := s.tenants.UpsertIdentityProviderMapper(ctx, keycloak.IdentityProviderMapperSpec{
		Alias: alias, OrganizationID: current.ID, OrganizationName: name,
	}); err != nil {
		return keycloak.IdentityProvider{}, restore(mapTenantKeycloakError("upsert identity provider mapper", alias, err))
	}
	return previous, nil
}

// restoreIdentityProvider writes the IdP an Update replaced back as it was, with the client
// secret replaceIdentityProvider read before the change, and its mappers with the
// organization's current id and name, when a later step failed.
func (s *TenantsServer) restoreIdentityProvider(ctx context.Context, current keycloak.Organization, previous keycloak.IdentityProvider, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), identityProviderCleanupTimeout)
	defer cancel()
	if err := s.tenants.UpdateIdentityProvider(ctx, previous); err != nil {
		return connect.Errorf(connect.CodeInternal, "identity provider update failed (%v); restoring the previous identity provider also failed (%v), so the realm identity provider %q is left on the new config while the organization wasn't saved: retry the update, or revert the identity provider in Keycloak", cause, err, previous.Alias)
	}
	if err := s.tenants.UpsertIdentityProviderMapper(ctx, keycloak.IdentityProviderMapperSpec{
		Alias: previous.Alias, OrganizationID: current.ID, OrganizationName: current.Name,
	}); err != nil {
		return connect.Errorf(connect.CodeInternal, "identity provider update failed (%v); the previous identity provider was restored, but restoring its mappers failed: %v", cause, err)
	}
	return cause
}

// identityProviderToWrite returns the requested IdP, validated and normalized as Create does,
// when Update must write it: when it adds one or changes the stored one. It returns nil when
// the request repeats the stored IdP or neither has one.
//
// Removing an IdP is refused: Keycloak would delete every member's link to it with it, and
// the organization's members, who have no other way to sign in, would be locked out.
// Correct it in place instead.
func identityProviderToWrite(tenantName string, stored, request *aigridv1.IdentityProvider) (*aigridv1.IdentityProvider, error) {
	if request == nil {
		if stored != nil {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider cannot be removed: the organization's members sign in through it; change it instead")
		}
		return nil, nil
	}
	normalized, err := normalizeIdentityProvider(tenantName, request)
	if err != nil {
		return nil, err
	}
	if stored != nil && proto.Equal(stored, normalized) {
		return nil, nil
	}
	return normalized, nil
}

// hideWhenOrganizationUnknown is the IdP config key of Keycloak's "Hide on login page when
// organization not resolved" (OrganizationModel.HIDE_IDP_ON_LOGIN_WHEN_ORGANIZATION_UNKNOWN,
// Keycloak 26). Without it, the realm's sign-in page lists every organization's IdP to anyone
// whose email domain belongs to no organization, so every tenant's IdP title is shown to every
// visitor. Users at an organization's domains still reach its IdP: domain routing, configured
// on the organization's domains, doesn't read this key.
const hideWhenOrganizationUnknown = "kc.org.broker.login.hide-when-org-unknown"

// oidcOptionalConfigKeys are the keys oidcConfigForKeycloak writes only when the tenant's IdP
// has a value for them, so a changed IdP without one must drop them.
var oidcOptionalConfigKeys = []string{"userInfoUrl", "jwksUrl", "useJwksUrl", "logoutUrl"}

func oidcConfigForKeycloak(cfg *aigridv1.OidcConfig, clientSecret []byte) map[string]string {
	out := map[string]string{
		"clientId":                  cfg.GetClientId(),
		"clientSecret":              string(clientSecret),
		"issuer":                    cfg.GetIssuer(),
		"authorizationUrl":          cfg.GetAuthorizationUrl(),
		"tokenUrl":                  cfg.GetTokenUrl(),
		"defaultScope":              cfg.GetDefaultScopes(),
		"validateSignature":         boolString(cfg.GetValidateSignature()),
		"syncMode":                  "FORCE",
		hideWhenOrganizationUnknown: "true",
		"clientAuthMethod":          "client_secret_post",
	}
	if v := cfg.GetUserInfoUrl(); v != "" {
		out["userInfoUrl"] = v
	}
	if v := cfg.GetJwksUrl(); v != "" {
		out["jwksUrl"] = v
		out["useJwksUrl"] = "true"
	}
	if v := cfg.GetLogoutUrl(); v != "" {
		out["logoutUrl"] = v
	}
	return out
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
