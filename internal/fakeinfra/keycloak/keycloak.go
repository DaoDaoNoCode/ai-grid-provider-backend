// Package keycloak is a fake of the part of Keycloak 26.8.0 that
// ai-grid-provider-backend calls: the OpenID Connect password grant on
// /realms/{admin realm}/protocol/openid-connect/token and the Organizations
// Admin REST API under /admin/realms/{realm}/organizations. It answers with
// Keycloak's status codes, body shapes and messages (validation.go names the
// Keycloak source line of each), so the backend's client code runs unchanged.
package keycloak

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token lifetimes of a bootstrapped master realm (ApplianceBootstrap.java,
// mock/research/keycloak.md 3.5): access tokens live 60 seconds, the SSO
// session 1800.
const (
	accessTokenLifespan  = 60 * time.Second
	refreshTokenLifespan = 1800 * time.Second
	// adminCLI is the public client every realm has and the backend uses.
	adminCLI = "admin-cli"
	// defaultMax is OrganizationsResource's default page size
	// (OrganizationsResource.java L157-158).
	defaultMax = 10
	// maxBodyBytes bounds request bodies; Keycloak's own limit is larger,
	// nothing the backend sends comes close.
	maxBodyBytes = 1 << 20
)

// Config configures the fake.
type Config struct {
	// AdminRealm is the realm the admin user lives in (`KEYCLOAK_ADMIN_REALM`,
	// default master).
	AdminRealm string
	// Realm is the realm with Organizations enabled (`KEYCLOAK_REALM`,
	// default ai-grid).
	Realm string
	// Username and Password are the only credentials the password grant
	// accepts.
	Username string
	Password string
	// Clock replaces time.Now, for tests.
	Clock func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Server is the fake Keycloak. It is safe for concurrent use.
type Server struct {
	cfg    Config
	clock  func() time.Time
	logger *slog.Logger

	mu     sync.Mutex
	orgs   map[string]*organization
	tokens map[string]time.Time
}

// organization is the stored model of an Organization.
type organization struct {
	id          string
	name        string
	alias       string
	enabled     bool
	description *string
	redirectURL *string
	attributes  map[string][]string
	domains     []Domain
}

// New builds a fake Keycloak seeded with organizations. Every seed is
// checked the way Keycloak would have accepted it.
func New(cfg Config, seed []Organization) (*Server, error) {
	if cfg.AdminRealm == "" || cfg.Realm == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("admin realm, realm, username and password are required")
	}
	s := &Server{cfg: cfg, clock: cfg.Clock, logger: cfg.Logger, orgs: map[string]*organization{}, tokens: map[string]time.Time{}}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	for _, rep := range seed {
		if err := s.seed(rep); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) seed(rep Organization) error {
	if !uuidPattern.MatchString(rep.ID) {
		return fmt.Errorf("fixture organization %q: id %q is not a lower-case UUID as Keycloak assigns them", rep.Name, rep.ID)
	}
	if _, dup := s.orgs[rep.ID]; dup {
		return fmt.Errorf("fixture organization id %q listed twice", rep.ID)
	}
	org, err := s.build(rep, nil)
	if err != nil {
		return fmt.Errorf("fixture organization %q: %s", rep.Name, err.msg)
	}
	org.id = rep.ID
	s.orgs[org.id] = org
	return nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/{realm}/protocol/openid-connect/token", s.token)
	orgs := "/admin/realms/{realm}/organizations"
	mux.HandleFunc("GET "+orgs, s.admin(s.list))
	mux.HandleFunc("GET "+orgs+"/count", s.admin(s.count))
	mux.HandleFunc("POST "+orgs, s.admin(s.create))
	mux.HandleFunc("GET "+orgs+"/{id}", s.admin(s.get))
	mux.HandleFunc("PUT "+orgs+"/{id}", s.admin(s.update))
	mux.HandleFunc("DELETE "+orgs+"/{id}", s.admin(s.delete))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Everything else is a resource this fake does not serve
		// (members, identity providers, groups, invitations, other admin
		// endpoints). Keycloak's error handler writes a JAX-RS 404 as
		// below.
		writeError(w, http.StatusNotFound, "HTTP 404 Not Found")
	})
	return mux
}

// token is the password grant of the OpenID Connect token endpoint, in the
// order Keycloak's TokenEndpoint checks a request: the realm, the grant type
// parameter, the client, then the grant itself.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	realm := r.PathValue("realm")
	if realm != s.cfg.AdminRealm && realm != s.cfg.Realm {
		writeError(w, http.StatusNotFound, MsgRealmDoesNotExist)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	grantType := r.PostForm.Get("grant_type")
	switch grantType {
	case "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", MsgMissingGrantType)
		return
	case "password", "client_credentials", "refresh_token":
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "Unsupported grant_type")
		return
	}
	if r.PostForm.Get("client_id") != adminCLI {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", MsgInvalidClient)
		return
	}
	if grantType == "client_credentials" {
		writeOAuthError(w, http.StatusUnauthorized, "unauthorized_client", "Public client not allowed to retrieve service account")
		return
	}
	if grantType == "refresh_token" {
		// The backend never refreshes; a refresh token is not kept.
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Invalid refresh token")
		return
	}
	username := r.PostForm.Get("username")
	if username == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_request", MsgMissingUsername)
		return
	}
	// Only the admin realm has the admin user.
	if realm != s.cfg.AdminRealm || username != s.cfg.Username || r.PostForm.Get("password") != s.cfg.Password {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", MsgInvalidCredentials)
		return
	}
	now := s.clock()
	access := randomToken()
	s.mu.Lock()
	for t, exp := range s.tokens {
		if !now.Before(exp) {
			delete(s.tokens, t)
		}
	}
	s.tokens[access] = now.Add(accessTokenLifespan)
	s.mu.Unlock()
	s.logger.Debug("keycloak token issued", "realm", realm, "username", username)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":       access,
		"expires_in":         int(accessTokenLifespan.Seconds()),
		"refresh_expires_in": int(refreshTokenLifespan.Seconds()),
		"refresh_token":      randomToken(),
		"token_type":         "Bearer",
		"not-before-policy":  0,
		"session_state":      newUUID(),
		"scope":              "profile email",
	})
}

// admin wraps an Organizations handler with what Keycloak's admin root does
// first: authenticate the bearer token (401 before anything else), then find
// the realm (404 "Realm not found."), then the Organizations feature of that
// realm (404 "Organizations not enabled for this realm."; the admin realm is
// bootstrapped without it). Handlers run under s.mu.
func (s *Server) admin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		defer s.mu.Unlock()
		if exp, issued := s.tokens[token]; !ok || !issued || !s.clock().Before(exp) {
			writeError(w, http.StatusUnauthorized, MsgUnauthorized)
			return
		}
		switch r.PathValue("realm") {
		case s.cfg.Realm:
			next(w, r)
		case s.cfg.AdminRealm:
			writeErrorMessage(w, http.StatusNotFound, MsgOrgsNotEnabled)
		default:
			writeError(w, http.StatusNotFound, MsgRealmMissing)
		}
	}
}

// filtered returns the organizations a list or count request selects,
// ordered by name (JpaOrganizationProvider.java L454, `ORDER BY name ASC`;
// the realm database's collation, which the fake takes to be byte order).
//
// `search` is a case-insensitive LIKE '%term%' on the name or on any domain
// name, or with `exact=true` a case-sensitive equality on either; a blank
// term selects everything (JpaOrganizationProvider.java L462-489). `q` is
// `key1:value1 key2:value2`, every attribute named must hold that value;
// when `q` is present `search` is ignored, as OrganizationsResource does.
func (s *Server) filtered(r *http.Request) []*organization {
	query := r.URL.Query()
	search := query.Get("search")
	exact := strings.EqualFold(query.Get("exact"), "true")
	attrs, hasQ := parseQ(query)
	out := make([]*organization, 0, len(s.orgs))
	for _, o := range s.orgs {
		switch {
		case hasQ:
			if !matchesAttributes(o, attrs) {
				continue
			}
		case javaIsBlank(search):
		case exact:
			if o.name != search && !slices.ContainsFunc(o.domains, func(d Domain) bool { return d.Name == search }) {
				continue
			}
		default:
			pattern := "%" + strings.ToLower(search) + "%"
			if !likeMatch(strings.ToLower(o.name), pattern) && !slices.ContainsFunc(o.domains, func(d Domain) bool { return likeMatch(d.Name, pattern) }) {
				continue
			}
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func parseQ(query url.Values) (map[string]string, bool) {
	if !query.Has("q") {
		return nil, false
	}
	attrs := map[string]string{}
	for _, field := range strings.Fields(query.Get("q")) {
		if k, v, ok := strings.Cut(field, ":"); ok {
			attrs[k] = v
		}
	}
	return attrs, true
}

func matchesAttributes(o *organization, attrs map[string]string) bool {
	for k, v := range attrs {
		if !slices.Contains(o.attributes[k], v) {
			return false
		}
	}
	return true
}

// list is GET /organizations: `first` (default 0) and `max` (default 10) page
// the result, a negative value of either is ignored (OrganizationsResource
// L138-139, PaginationUtils.paginateQuery L26-41), and
// `briefRepresentation` (default true) leaves `attributes` out.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	first, okFirst := intParam(query, "first", 0)
	max, okMax := intParam(query, "max", defaultMax)
	if !okFirst || !okMax {
		writeError(w, http.StatusNotFound, "HTTP 404 Not Found")
		return
	}
	brief := !query.Has("briefRepresentation") || strings.EqualFold(query.Get("briefRepresentation"), "true")
	all := s.filtered(r)
	if first > 0 {
		all = all[min(first, len(all)):]
	}
	if max >= 0 && max < len(all) {
		all = all[:max]
	}
	out := make([]Organization, 0, len(all))
	for _, o := range all {
		out = append(out, o.representation(brief))
	}
	writeJSON(w, http.StatusOK, out)
}

// count is GET /organizations/count with the same filters as list.
func (s *Server) count(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, len(s.filtered(r)))
}

// intParam reads an Integer query parameter the way JAX-RS does: absent is
// the default, unparseable is a failed request (JAX-RS answers 404).
func intParam(query url.Values, name string, def int) (int, bool) {
	if !query.Has(name) {
		return def, true
	}
	v, err := strconv.Atoi(strings.TrimSpace(query.Get(name)))
	return v, err == nil
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	o, ok := s.orgs[r.PathValue("id")]
	if !ok {
		writeErrorMessage(w, http.StatusNotFound, MsgOrganizationMissing)
		return
	}
	// GET by id always returns the full representation.
	writeJSON(w, http.StatusOK, o.representation(false))
}

// create is POST /organizations: 201 with an empty body and a Location
// header naming the new id. A client-supplied id is ignored.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	rep, ok := readRepresentation(w, r)
	if !ok {
		return
	}
	org, kcErr := s.build(*rep, nil)
	if kcErr != nil {
		kcErr.write(w)
		return
	}
	org.id = newUUID()
	s.orgs[org.id] = org
	s.logger.Debug("organization created", "id", org.id, "name", org.name, "alias", org.alias)
	w.Header().Set("Location", absoluteURL(r, r.URL.Path+"/"+org.id))
	w.WriteHeader(http.StatusCreated)
}

// update is PUT /organizations/{id} with the full-representation semantics
// of RepresentationToModel.toModel (L1939-1958): name, alias, enabled,
// redirectUrl and description are always replaced (absent `enabled` is
// true, absent `description` and `redirectUrl` are cleared), `attributes`
// and `domains` only when the body carries them (null or absent leaves
// them as stored; `[]` and `{}` clear them).
func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	cur, ok := s.orgs[r.PathValue("id")]
	if !ok {
		writeErrorMessage(w, http.StatusNotFound, MsgOrganizationMissing)
		return
	}
	rep, ok := readRepresentation(w, r)
	if !ok {
		return
	}
	next, kcErr := s.build(*rep, cur)
	if kcErr != nil {
		kcErr.write(w)
		return
	}
	next.id = cur.id
	s.orgs[cur.id] = next
	s.logger.Debug("organization updated", "id", next.id, "name", next.name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.orgs[id]; !ok {
		writeErrorMessage(w, http.StatusNotFound, MsgOrganizationMissing)
		return
	}
	delete(s.orgs, id)
	s.logger.Debug("organization deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// httpError is a Keycloak error answer: `errorMessage` for an ErrorResponse,
// `error` for a JAX-RS exception (KeycloakErrorHandler).
type httpError struct {
	code      int
	msg       string
	framework bool
}

func (e *httpError) write(w http.ResponseWriter) {
	if e.framework {
		writeError(w, e.code, e.msg)
		return
	}
	writeErrorMessage(w, e.code, e.msg)
}

// build applies a representation the way Keycloak does, in its order, to a
// new organization (cur nil: OrganizationsResource.create L112-118, then
// JpaOrganizationProvider.create L102-121, then RepresentationToModel.toModel
// and OrganizationAdapter.setDomains L184-229) or to a stored one (PUT:
// OrganizationResource.update L120-125, then toModel). Must be called under
// s.mu.
func (s *Server) build(rep Organization, cur *organization) (*organization, *httpError) {
	org := &organization{name: rep.Name}
	if cur == nil {
		// OrganizationsResource.java L112: an explicit alias is checked
		// first; the validator throws a BadRequestException, so the body
		// has the framework `error` shape.
		if rep.Alias != "" {
			if err := validateAlias(rep.Alias); err != nil {
				return nil, &httpError{code: http.StatusBadRequest, msg: err.Error(), framework: true}
			}
		}
		if javaIsBlank(rep.Name) { // JpaOrganizationProvider.java L103-104
			return nil, &httpError{code: http.StatusBadRequest, msg: MsgNameRequired}
		}
		org.alias = rep.Alias
		if javaIsBlank(org.alias) { // JpaOrganizationProvider.java L107-113
			org.alias = org.name
			if err := validateAlias(org.alias); err != nil {
				return nil, &httpError{code: http.StatusBadRequest, msg: MsgNameNotAliasPrefix + err.Error()}
			}
		}
		for _, other := range s.orgs {
			if other.name == org.name {
				return nil, &httpError{code: http.StatusConflict, msg: MsgDuplicateName}
			}
		}
		for _, other := range s.orgs {
			if other.alias == org.alias {
				return nil, &httpError{code: http.StatusConflict, msg: MsgDuplicateAlias}
			}
		}
	} else {
		// OrganizationResource.java L123-125: a new name is looked up with
		// getAllStream(name, exact=true), which matches names and domain
		// names, does not exclude this organization, and matches every
		// organization for a blank name (JpaOrganizationProvider.java
		// L462-470), so an omitted name is a 409 too.
		if rep.Name != cur.name {
			if javaIsBlank(rep.Name) {
				return nil, &httpError{code: http.StatusConflict, msg: MsgDuplicateName}
			}
			for _, other := range s.orgs {
				if other.name == rep.Name || slices.ContainsFunc(other.domains, func(d Domain) bool { return d.Name == rep.Name }) {
					return nil, &httpError{code: http.StatusConflict, msg: MsgDuplicateName}
				}
			}
		}
		// toModel sets the name before the alias, and
		// OrganizationAdapter.setAlias (L100-110) defaults a blank alias to
		// that new name and refuses any value but the stored one.
		alias := rep.Alias
		if javaIsBlank(alias) {
			alias = rep.Name
		}
		if alias != cur.alias {
			return nil, &httpError{code: http.StatusBadRequest, msg: MsgAliasImmutable}
		}
		org.alias = cur.alias
	}

	// OrganizationRepresentation.enabled is a Boolean that defaults to true
	// (OrganizationRepresentation.java L33), so a body without it enables.
	org.enabled = rep.Enabled == nil || *rep.Enabled
	if rep.RedirectURL != nil && !javaIsBlank(*rep.RedirectURL) && !validRedirectURL(*rep.RedirectURL) {
		return nil, &httpError{code: http.StatusBadRequest, msg: MsgRedirectURLInvalid}
	}
	org.redirectURL = cloneString(rep.RedirectURL)
	org.description = cloneString(rep.Description)

	// setAttributes and setDomains return early on null
	// (OrganizationAdapter L144-147, L184-187).
	switch {
	case rep.Attributes != nil:
		org.attributes = cloneAttributes(*rep.Attributes)
	case cur != nil:
		org.attributes = cloneAttributes(cur.attributes)
	default:
		org.attributes = map[string][]string{}
	}
	switch {
	case rep.Domains != nil:
		domains, err := normalizeDomains(*rep.Domains)
		if err != nil {
			return nil, &httpError{code: http.StatusBadRequest, msg: err.Error()}
		}
		org.domains = domains
	case cur != nil:
		org.domains = slices.Clone(cur.domains)
	}
	selfID := ""
	if cur != nil {
		selfID = cur.id
	}
	for _, d := range org.domains {
		for id, other := range s.orgs {
			if id != selfID && slices.ContainsFunc(other.domains, func(o Domain) bool { return o.Name == d.Name }) {
				return nil, &httpError{code: http.StatusBadRequest, msg: fmt.Sprintf(MsgDomainLinkedFmt, d.Name, other.name, s.cfg.Realm)}
			}
		}
	}
	if err := checkIdentityProviders(org.domains, s.cfg.Realm); err != nil {
		return nil, &httpError{code: http.StatusBadRequest, msg: err.Error()}
	}
	return org, nil
}

// validRedirectURL approximates Keycloak's built-in URI validator as
// OrganizationsValidation uses it: an absolute http or https URL with a host.
func validRedirectURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// readRepresentation decodes an OrganizationRepresentation body.
func readRepresentation(w http.ResponseWriter, r *http.Request) (*Organization, bool) {
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "HTTP 415 Unsupported Media Type")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "HTTP 400 Bad Request")
		return nil, false
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed == "" || trimmed == "null" {
		writeErrorMessage(w, http.StatusBadRequest, MsgOrganizationNull)
		return nil, false
	}
	var rep Organization
	if err := json.Unmarshal(body, &rep); err != nil {
		writeError(w, http.StatusBadRequest, "HTTP 400 Bad Request")
		return nil, false
	}
	return &rep, true
}

// absoluteURL is the request's absolute URI with path, as JAX-RS
// UriInfo.getAbsolutePathBuilder builds the Location header.
func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: path}).String()
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

func cloneAttributes(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErrorMessage writes an ErrorResponse body.
func writeErrorMessage(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"errorMessage": msg})
}

// writeError writes the body KeycloakErrorHandler gives a JAX-RS exception.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeOAuthError writes an OAuth 2.0 token endpoint error.
func writeOAuthError(w http.ResponseWriter, code int, oauthErr, description string) {
	writeJSON(w, code, map[string]string{"error": oauthErr, "error_description": description})
}
