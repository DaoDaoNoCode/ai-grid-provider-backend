package draft

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/draft/celfilter"
)

// Tenants are Keycloak Organizations behind the backend's mapping
// (docs/draft/research/keycloak.md sections 3, 5 and 8; backend-conventions.md 6.6).
// The store is a map of organizations keyed by UUID; every RPC does what the
// real backend does with Keycloak 26.8: the same uniqueness rules, the same
// validation messages, the same defaults, and the gRPC code the error table
// in keycloak.md 8.2 assigns to each Keycloak outcome.

// TenantVersionAttribute is the organization attribute the backend keeps a
// tenant's version in (the console's proposal, docs/draft/areas/tenants.md,
// "Version"): Keycloak Organizations have none, so the backend writes this
// attribute with every PUT it makes, starting at 1, and maps it to
// `Tenant.resource_version` instead of `attributes`. The fake keeps the value
// in the stored Tenant's `resource_version` and answers as that backend would:
// the key never appears in `attributes`, and a request that sets it is
// InvalidArgument.
const TenantVersionAttribute = "ai-grid/resource-version"

// TenantCreateTimeAttribute is the organization attribute the backend keeps
// a tenant's creation time in (proposal P3), in RFC 3339: Keycloak keeps
// none, so the backend stamps it on create and maps it to
// `Tenant.create_time` instead of `attributes`. Like the version, the fake
// keeps the value in the stored Tenant's `create_time`, and a request that
// sets the attribute is InvalidArgument.
const TenantCreateTimeAttribute = "ai-grid/create-time"

// checkReservedAttributes refuses the attributes the backend owns.
func checkReservedAttributes(attrs map[string]string) error {
	if _, ok := attrs[TenantVersionAttribute]; ok {
		return status.Errorf(codes.InvalidArgument, "attribute %q is reserved for the tenant's version; it cannot be set", TenantVersionAttribute)
	}
	if _, ok := attrs[TenantCreateTimeAttribute]; ok {
		return status.Errorf(codes.InvalidArgument, "attribute %q is reserved for the tenant's creation time; it cannot be set", TenantCreateTimeAttribute)
	}
	return nil
}

// checkTenantVersion is the precondition of UpdateTenant and DeleteTenant:
// a non-empty expected version must equal the stored one.
func checkTenantVersion(t *aigriddraftv1.Tenant, expected string) error {
	if expected != "" && expected != t.GetResourceVersion() {
		return status.Errorf(codes.Aborted, "object with identifier '%s' has resource version %s, the request carries %q; fetch it again and retry", t.GetId(), t.GetResourceVersion(), expected)
	}
	return nil
}

// bumpTenantVersion is what the backend writes into TenantVersionAttribute
// with each PUT: the stored version plus one.
func bumpTenantVersion(t *aigriddraftv1.Tenant) {
	n, _ := strconv.ParseInt(t.GetResourceVersion(), 10, 64)
	t.ResourceVersion = strconv.FormatInt(n+1, 10)
}

// beginTenants is what every Tenants RPC does after authentication and the
// round trip: fail the way the backend fails when Keycloak is unreachable or
// the realm is missing, before touching the store. Must be called under s.mu.
func (s *Server) beginTenants() error {
	if s.TenantsErr != nil {
		return s.TenantsErr
	}
	// Keycloak answers 404 {"error":"Realm not found."} or
	// {"errorMessage":"Organizations not enabled for this realm."}; the
	// conventions make both FailedPrecondition (deployment misconfiguration,
	// not a caller error). The wrapping text `Keycloak realm "<realm>": ` is
	// the console's proposal (keycloak.md 8.2), not something Keycloak says.
	if s.realmMissing {
		return status.Errorf(codes.FailedPrecondition, "Keycloak realm %q: %s", KeycloakRealm, MsgRealmMissing)
	}
	if s.orgsDisabled {
		return status.Errorf(codes.FailedPrecondition, "Keycloak realm %q: %s", KeycloakRealm, MsgOrgsNotEnabled)
	}
	return nil
}

// ListTenants implements Tenants. `offset`/`limit` travel to Keycloak as
// `first`/`max`, but their meaning is the contract's, not Keycloak's: an
// omitted limit means all, zero means count only and a negative value is
// InvalidArgument, whereas Keycloak defaults `first` to 0 and `max` to 10
// (OrganizationsResource.java L157-158) and ignores a negative value of
// either (L138-139; PaginationUtils.paginateQuery L26-41), so the backend
// must send an explicit `max` for an omitted limit and reject negatives
// itself. Disabled organizations are included like Keycloak includes them,
// and results are ordered by name (JpaOrganizationProvider.java L454,
// `ORDER BY name ASC`). That order is the realm database's collation for
// the name column, which the console has not checked on the hub; the fake
// uses Go byte order, which equals it for the all-lower-case fixtures.
//
// `filter` is CEL over `this` (a Tenant) and `now` (celfilter), evaluated in
// memory over the whole realm list before paging, as the List rule of
// docs/draft/research/backend-conventions.md 5.3 says (a realm's organizations are
// few), so `total` counts the filtered set. The evaluation is strict CEL:
// `lowerAscii()` folds A-Z only and `contains` is a literal substring, so a
// caller that wants a case-insensitive search lowers its term itself. A
// backend may instead push a simple term down to Keycloak's `search`, a SQL
// LIKE built without escaping and without an ESCAPE clause
// (JpaOrganizationProvider.java L482-483, so `%`, `_` and `\` must be
// escaped first), but only where that returns what the CEL expression
// means; the fake's in-memory evaluation is the reference.
func (s *Server) ListTenants(ctx context.Context, req *aigriddraftv1.ListTenantsRequest) (*aigriddraftv1.ListTenantsResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	filter, err := celfilter.Compile(req.GetFilter(), &aigriddraftv1.Tenant{})
	if err != nil {
		return nil, err
	}
	if req.GetOrder() != "" {
		return nil, status.Error(codes.InvalidArgument, "order is not supported by this server yet; Keycloak returns organizations by name")
	}
	if req.GetOffset() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "offset must not be negative, got %d", req.GetOffset())
	}
	if req.Limit != nil && req.GetLimit() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "limit must not be negative, got %d", req.GetLimit())
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.beginTenants(); err != nil {
		return nil, err
	}
	// Sort and filter references under the lock, in name order so that an
	// evaluation error names the same tenant every time; only the page that
	// leaves the lock is cloned, so a count-only or small-page request does
	// not copy the whole realm.
	sorted := make([]*aigriddraftv1.Tenant, 0, len(s.tenants))
	for _, t := range s.tenants {
		sorted = append(sorted, t)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetName() < sorted[j].GetName() })
	now := s.clock()
	all := make([]*aigriddraftv1.Tenant, 0, len(sorted))
	for _, t := range sorted {
		ok, err := filter.Match(ctx, t, t.GetId(), now)
		if err != nil {
			return nil, err
		}
		if ok {
			all = append(all, t)
		}
	}
	total := len(all)
	offset := min(int(req.GetOffset()), total)
	window := all[offset:]
	if req.Limit != nil && int(req.GetLimit()) < len(window) {
		window = window[:req.GetLimit()]
	}
	page := make([]*aigriddraftv1.Tenant, 0, len(window))
	for _, t := range window {
		page = append(page, cloneTenant(t))
	}
	return &aigriddraftv1.ListTenantsResponse{Tenants: page, Size: int32(len(page)), Total: proto.Int32(int32(total))}, nil
}

// GetTenant implements Tenants. Keycloak answers 404 "Organization not
// found."; the backend maps that to NotFound with the conventions' message
// and keeps Keycloak's text in its log, as this does.
func (s *Server) GetTenant(ctx context.Context, req *aigriddraftv1.GetTenantRequest) (*aigriddraftv1.GetTenantResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		// Request-shape check before Keycloak is called; the text is the
		// console's (Keycloak would say "Id cannot be null.",
		// OrganizationsResource.java L206-207).
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.beginTenants(); err != nil {
		return nil, err
	}
	t, ok := s.tenants[req.GetId()]
	if !ok {
		return nil, s.tenantNotFound(req.GetId())
	}
	return &aigriddraftv1.GetTenantResponse{Object: cloneTenant(t)}, nil
}

func (s *Server) tenantNotFound(id string) error {
	s.logger.Debug("keycloak", "status", 404, "errorMessage", MsgOrganizationMissing, "id", id)
	return notFound(id)
}

// CreateTenant implements Tenants in the order Keycloak checks a POST
// (OrganizationsResource.java L112-118, then JpaOrganizationProvider.create
// L102-121, then RepresentationToModel.toModel L1939-1958 and
// OrganizationAdapter.setDomains L184-229): explicit alias characters,
// blank name, alias defaulting to the name and the name's characters,
// duplicate name, duplicate alias, and only then the domains (format, other
// owners, identity provider aliases). Everything
// Keycloak decides comes after the Keycloak gate (beginTenants), because the
// real backend learns those answers from Keycloak and answers Unavailable
// without it; only the request-shape checks precede it. The real backend
// follows the 201's Location with a GET and returns the object; the fake
// returns the stored object directly.
func (s *Server) CreateTenant(ctx context.Context, req *aigriddraftv1.CreateTenantRequest) (*aigriddraftv1.CreateTenantResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "tenants"); err != nil {
		return nil, err
	}
	obj := req.GetObject()
	if obj == nil {
		// Console wording; Keycloak's own is "Organization cannot be null."
		return nil, status.Error(codes.InvalidArgument, "object is required")
	}
	if obj.GetId() != "" {
		// Console rule (see CreateTenantRequest in the proto): Keycloak
		// itself ignores a client id on POST.
		return nil, status.Error(codes.InvalidArgument, "object.id must be empty; Keycloak assigns it")
	}
	if err := checkReservedAttributes(obj.GetAttributes()); err != nil {
		return nil, err
	}
	// The identity provider (P1) is the console's rule, checked before
	// Keycloak is called; an organization may have none.
	var idp *aigriddraftv1.IdentityProvider
	if obj.GetIdentityProvider() != nil {
		var fields map[string]string
		idp, fields = checkIdentityProvider(obj.GetIdentityProvider())
		if err := identityProviderError(fields); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginTenants(); err != nil {
		return nil, err
	}
	// OrganizationsResource.java L112: an explicit alias, even a blank one,
	// is checked first (validateNoSpace skips only null, which the proto
	// sends as "").
	if obj.GetAlias() != "" {
		if err := validateAlias(obj.GetAlias()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if javaIsBlank(obj.GetName()) { // JpaOrganizationProvider.java L103-104
		return nil, status.Error(codes.InvalidArgument, MsgNameRequired)
	}
	t := cloneTenant(obj)
	t.IdentityProvider = idp
	if javaIsBlank(t.GetAlias()) { // JpaOrganizationProvider.java L107-113
		t.Alias = t.GetName()
		if err := validateAlias(t.GetAlias()); err != nil {
			return nil, status.Error(codes.InvalidArgument, MsgNameNotAliasPrefix+err.Error())
		}
	}
	for _, other := range s.tenants {
		if other.GetName() == t.GetName() {
			return nil, status.Error(codes.AlreadyExists, MsgDuplicateName)
		}
	}
	for _, other := range s.tenants {
		if other.GetAlias() == t.GetAlias() {
			return nil, status.Error(codes.AlreadyExists, MsgDuplicateAlias)
		}
	}
	if err := normalizeDomains(t); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.checkDomainsFree(t, ""); err != nil {
		return nil, err
	}
	if err := checkIdentityProviders(t); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if t.Enabled == nil {
		t.Enabled = proto.Bool(true) // OrganizationRepresentation.java L33: enabled defaults to true
	}
	if t.Attributes == nil {
		t.Attributes = map[string]string{}
	}
	t.CreateTime = timestamppb.New(s.clock().UTC().Truncate(time.Second)) // output only; stamped here
	t.Id = s.newID()
	t.ResourceVersion = "" // output only; the first write makes it 1
	bumpTenantVersion(t)
	s.tenants[t.Id] = t
	s.logger.Debug("organization created", "id", t.GetId(), "name", t.GetName(), "alias", t.GetAlias())
	return &aigriddraftv1.CreateTenantResponse{Object: cloneTenant(t)}, nil
}

// checkDomainsFree is the realm-wide domain uniqueness Keycloak enforces
// when it claims domains for an organization (OrganizationAdapter.java
// L304-309: a conflict is another organization holding exactly that name).
// Must be called under s.mu.
func (s *Server) checkDomainsFree(t *aigriddraftv1.Tenant, selfID string) error {
	for _, d := range t.GetDomains() {
		for id, other := range s.tenants {
			if id != selfID && slices.Contains(other.GetDomains(), d) {
				return status.Errorf(codes.InvalidArgument, MsgDomainLinkedFmt, d, other.GetName(), KeycloakRealm)
			}
		}
	}
	return nil
}

// UpdateTenant implements Tenants as the backend's GET, merge, PUT (the
// console's proposal, keycloak.md 8.1): the backend reads the full
// representation, overwrites the fields the mask names with the request's
// values and PUTs the result, so everything else, `alias` and
// `redirectUrl` included, goes back as stored. Without a mask `name`,
// `enabled` (true when unset, OrganizationRepresentation.java L33) and
// `description` are taken from the request and `attributes` and the domains
// are kept, which is what a Keycloak PUT does with a body that lacks those
// two keys (RepresentationToModel.toModel L1949-1955; OrganizationAdapter
// setAttributes L144-147 and setDomains L184-187 return on null).
//
// The alias is never taken from an omitted request field: the PUT carries
// the stored alias unless the request names a non-blank one. That matters,
// because a PUT straight to Keycloak without `alias` does not keep it:
// toModel sets the new name first (L1944) and setAlias then defaults a blank
// alias to that name (OrganizationAdapter.java L100-110), which is 400
// "Cannot change the alias" whenever the new name differs from the stored
// alias (for globex-research, whose alias is globex, always).
//
// Keycloak's own checks follow, in its order. First the name
// (OrganizationResource.java L123-125): a name different from the stored
// one is looked up with exact=true, which matches organization names and
// domain names and does not exclude the organization itself, and any hit is
// 409; a blank name always hits, because a blank search is no filter at all
// (JpaOrganizationProvider.java L468-470) and the organization being
// updated is in the realm, so a blank name is AlreadyExists "A organization
// with the same name already exists.", not "Name can not be null". Then
// toModel: a different alias is 400 "Cannot change the alias", then the
// domains as on create.
//
// The mask path `alias` exists for a client that sends back the alias it
// loaded: it writes nothing (the PUT always carries the stored alias) and a
// different non-blank alias is the same InvalidArgument as above.
//
// A merge that leaves the organization as stored is not PUT (console
// proposal: the backend compares its GET with the merged representation and
// skips a PUT that changes nothing, so Keycloak writes no admin event and the
// version attribute is not bumped; Keycloak itself would accept the PUT and
// write the event). The tenant comes back with its stored resource_version;
// the version precondition and Keycloak's checks run before, so a stale or
// invalid request still fails.
func (s *Server) UpdateTenant(ctx context.Context, req *aigriddraftv1.UpdateTenantRequest) (*aigriddraftv1.UpdateTenantResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "tenants"); err != nil {
		return nil, err
	}
	obj := req.GetObject()
	if obj.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "object.id is required") // console wording
	}
	paths := req.GetUpdateMask().GetPaths()
	for _, p := range paths {
		if !slices.Contains(tenantPaths, p) {
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not supported; supported paths: %s", p, strings.Join(tenantPaths, ", "))
		}
	}
	if slices.Contains(paths, PathAttributes) {
		if err := checkReservedAttributes(obj.GetAttributes()); err != nil {
			return nil, err
		}
	}
	has := func(p string) bool { return len(paths) == 0 || slices.Contains(paths, p) }
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginTenants(); err != nil {
		return nil, err
	}
	cur, ok := s.tenants[obj.GetId()]
	if !ok {
		return nil, s.tenantNotFound(obj.GetId())
	}
	// The backend's precondition, checked on its GET before Keycloak's own
	// checks: a stale version is Aborted whatever else the request says.
	if err := checkTenantVersion(cur, obj.GetResourceVersion()); err != nil {
		return nil, err
	}
	// The identity provider (P1) moves only when its path is in the mask,
	// and must then come with a value that passes the console's rules. The
	// check is the console's own, so it runs after the version precondition.
	var idp *aigriddraftv1.IdentityProvider
	if slices.Contains(paths, PathIdentityProvider) {
		var fields map[string]string
		idp, fields = checkIdentityProvider(obj.GetIdentityProvider())
		if err := identityProviderError(fields); err != nil {
			return nil, err
		}
	}
	next := cloneTenant(cur)
	if has(PathName) {
		next.Name = obj.GetName()
	}
	if next.GetName() != cur.GetName() {
		// OrganizationResource.java L123-125 with
		// JpaOrganizationProvider.getAllStream(name, exact=true) (L345-359,
		// L462-489): blank matches everything, otherwise names and domains
		// are compared for equality, the organization's own included.
		if javaIsBlank(next.GetName()) {
			return nil, status.Error(codes.AlreadyExists, MsgDuplicateName)
		}
		for id, other := range s.tenants {
			if (id != cur.GetId() && other.GetName() == next.GetName()) || slices.Contains(other.GetDomains(), next.GetName()) {
				return nil, status.Error(codes.AlreadyExists, MsgDuplicateName)
			}
		}
	}
	// The PUT carries the request's alias when it is not blank, the stored
	// one otherwise; OrganizationAdapter.setAlias (L100-110) refuses any
	// other value than the stored one.
	if !javaIsBlank(obj.GetAlias()) && obj.GetAlias() != cur.GetAlias() {
		return nil, status.Error(codes.InvalidArgument, MsgAliasImmutable)
	}
	switch {
	case slices.Contains(paths, PathEnabled):
		next.Enabled = proto.Bool(obj.GetEnabled())
	case len(paths) == 0:
		// A PUT body without `enabled` re-enables (Java default true).
		next.Enabled = proto.Bool(obj.Enabled == nil || obj.GetEnabled())
	}
	if has(PathDescription) {
		next.Description = obj.GetDescription()
	}
	// Collections only move when their path is in the mask: a Keycloak PUT
	// leaves `attributes` and `domains` unchanged when the key is absent.
	if slices.Contains(paths, PathAttributes) {
		next.Attributes = copyMap(obj.GetAttributes())
	}
	switch {
	case slices.Contains(paths, PathDomainDetails):
		next.Domains = nil
		next.DomainDetails = obj.GetDomainDetails()
	case slices.Contains(paths, PathDomains):
		// The names are replaced; a domain that stays keeps its metadata
		// because the backend's merge copies the stored `verified`,
		// identity provider alias and `autoRedirect` of every name that
		// stays into the PUT body. That is the console's proposal, not
		// Keycloak's behaviour: OrganizationAdapter.setDomains overwrites
		// all three on a kept domain from the body (L200-205), so a body
		// with names only would reset them.
		kept := map[string]*aigriddraftv1.TenantDomain{}
		for _, d := range cur.GetDomainDetails() {
			kept[d.GetName()] = d
		}
		next.Domains = nil
		next.DomainDetails = nil
		for _, name := range obj.GetDomains() {
			if d, ok := kept[canonicalDomain(name)]; ok {
				next.DomainDetails = append(next.DomainDetails, d)
			} else {
				next.DomainDetails = append(next.DomainDetails, &aigriddraftv1.TenantDomain{Name: name})
			}
		}
	}
	if idp != nil {
		next.IdentityProvider = idp
	}
	if err := normalizeDomains(next); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.checkDomainsFree(next, cur.GetId()); err != nil {
		return nil, err
	}
	if err := checkIdentityProviders(next); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if proto.Equal(next, cur) {
		s.logger.Debug("organization update changes nothing; no PUT", "id", cur.GetId(), "paths", paths, "resourceVersion", cur.GetResourceVersion())
		return &aigriddraftv1.UpdateTenantResponse{Object: cloneTenant(cur)}, nil
	}
	bumpTenantVersion(next)
	s.tenants[cur.GetId()] = next
	s.logger.Debug("organization updated", "id", next.GetId(), "name", next.GetName(), "paths", paths, "resourceVersion", next.GetResourceVersion())
	return &aigriddraftv1.UpdateTenantResponse{Object: cloneTenant(next)}, nil
}

// DeleteTenant implements Tenants: Keycloak's DELETE answers 204 and removes
// the organization with its memberships; 404 when it is already gone. A
// non-empty `resource_version` is compared first (the backend's GET), and a
// stale one is Aborted.
func (s *Server) DeleteTenant(ctx context.Context, req *aigriddraftv1.DeleteTenantRequest) (*aigriddraftv1.DeleteTenantResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "tenants"); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required") // console wording
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginTenants(); err != nil {
		return nil, err
	}
	t, ok := s.tenants[req.GetId()]
	if !ok {
		return nil, s.tenantNotFound(req.GetId())
	}
	if err := checkTenantVersion(t, req.GetResourceVersion()); err != nil {
		return nil, err
	}
	delete(s.tenants, req.GetId())
	s.logger.Debug("organization deleted", "id", t.GetId(), "name", t.GetName())
	return &aigriddraftv1.DeleteTenantResponse{}, nil
}

// seedTenant validates one fixture organization the way Keycloak would have
// accepted it, so the shipped fixtures can only contain organizations that
// could exist in a realm without identity providers (the fake has none, so
// a domain `identityProviderAlias` is rejected as on create). Must be
// called under s.mu, before Serve.
func (s *Server) seedTenant(t *aigriddraftv1.Tenant) error {
	if !uuidPattern.MatchString(t.GetId()) {
		return fmt.Errorf("fixture tenant %q: id %q is not a lower-case UUID as Keycloak assigns them", t.GetName(), t.GetId())
	}
	if javaIsBlank(t.GetName()) {
		return fmt.Errorf("fixture tenant %s: %s", t.GetId(), MsgNameRequired)
	}
	t = cloneTenant(t)
	if javaIsBlank(t.GetAlias()) {
		t.Alias = t.GetName()
	}
	if t.Enabled == nil {
		t.Enabled = proto.Bool(true)
	}
	if err := validateAlias(t.GetAlias()); err != nil {
		return fmt.Errorf("fixture tenant %q: alias %q: %w", t.GetName(), t.GetAlias(), err)
	}
	if err := normalizeDomains(t); err != nil {
		return fmt.Errorf("fixture tenant %q: %w", t.GetName(), err)
	}
	if err := checkIdentityProviders(t); err != nil {
		return fmt.Errorf("fixture tenant %q: %w", t.GetName(), err)
	}
	if _, ok := t.GetAttributes()[TenantVersionAttribute]; ok {
		return fmt.Errorf("fixture tenant %q: attribute %q is the backend's version; set resourceVersion instead", t.GetName(), TenantVersionAttribute)
	}
	if _, ok := t.GetAttributes()[TenantCreateTimeAttribute]; ok {
		return fmt.Errorf("fixture tenant %q: attribute %q is the backend's creation time; set createTime instead", t.GetName(), TenantCreateTimeAttribute)
	}
	switch n, err := strconv.ParseInt(t.GetResourceVersion(), 10, 64); {
	case t.GetResourceVersion() == "":
		t.ResourceVersion = "1" // the backend wrote the attribute when it created the organization
	case err != nil || n < 1:
		return fmt.Errorf("fixture tenant %q: resourceVersion %q is not a positive integer", t.GetName(), t.GetResourceVersion())
	}
	if t.GetIdentityProvider() != nil {
		idp, fields := checkIdentityProvider(t.GetIdentityProvider())
		if len(fields) > 0 {
			return fmt.Errorf("fixture tenant %q: identity provider: %s", t.GetName(), joinMessages(fields))
		}
		t.IdentityProvider = idp
	}
	if t.Attributes == nil {
		t.Attributes = map[string]string{}
	}
	if _, dup := s.tenants[t.GetId()]; dup {
		return fmt.Errorf("fixture tenant id %q listed twice", t.GetId())
	}
	for _, other := range s.tenants {
		if other.GetName() == t.GetName() {
			return fmt.Errorf("fixture tenant %q: %s", t.GetName(), MsgDuplicateName)
		}
		if other.GetAlias() == t.GetAlias() {
			return fmt.Errorf("fixture tenant %q: %s", t.GetName(), MsgDuplicateAlias)
		}
	}
	if err := s.checkDomainsFree(t, ""); err != nil {
		return fmt.Errorf("fixture tenant %q: %s", t.GetName(), status.Convert(err).Message())
	}
	s.tenants[t.GetId()] = t
	return nil
}
