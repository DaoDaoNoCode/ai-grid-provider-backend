package keycloak

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
)

// identityProvider is IdentityProviderRepresentation. The fake checks the fields it models;
// the others (hideOnLogin, firstBrokerLoginFlowAlias, storeToken, linkOnly and the rest) are
// kept in extra and returned as written, as Keycloak stores and returns them.
type identityProvider struct {
	Alias       string            `json:"alias"`
	DisplayName string            `json:"displayName,omitempty"`
	ProviderID  string            `json:"providerId"`
	Enabled     bool              `json:"enabled"`
	TrustEmail  bool              `json:"trustEmail"`
	Config      map[string]string `json:"config"`
	extra       map[string]json.RawMessage
}

type identityProviderFields identityProvider

func (p *identityProvider) UnmarshalJSON(data []byte) error {
	var fields identityProviderFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for _, key := range jsonFieldNames(fields) {
		delete(all, key)
	}
	*p = identityProvider(fields)
	p.extra = all
	return nil
}

func (p identityProvider) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(identityProviderFields(p))
	if err != nil || len(p.extra) == 0 {
		return data, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	modelled := jsonFieldNames(identityProviderFields(p))
	for key, value := range p.extra {
		if !slices.Contains(modelled, key) {
			all[key] = value
		}
	}
	return json.Marshal(all)
}

// jsonFieldNames lists the JSON names of a struct's fields, including those omitted as empty,
// as the backend's client does (internal/keycloak/client.go), so the keys kept in extra can't
// drift from the fields the fake models.
func jsonFieldNames(v any) []string {
	t := reflect.TypeOf(v)
	var names []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

type identityProviderMapper struct {
	ID                     string            `json:"id,omitempty"`
	Name                   string            `json:"name"`
	IdentityProviderAlias  string            `json:"identityProviderAlias"`
	IdentityProviderMapper string            `json:"identityProviderMapper"`
	Config                 map[string]string `json:"config"`
}

type orgIdPLink struct {
	AutoMembership bool   `json:"autoMembership"`
	MembershipType string `json:"membershipType"`
}

type orgIdPLinkState struct {
	alias string
	link  orgIdPLink
}

func (s *Server) createIdP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var idp identityProvider
	if err := json.Unmarshal(body, &idp); err != nil || idp.Alias == "" || idp.ProviderID == "" {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid identity provider representation")
		return
	}
	if _, ok := s.idps[idp.Alias]; ok {
		writeErrorMessage(w, http.StatusConflict, "Identity Provider with alias already exists")
		return
	}
	if idp.Config == nil {
		idp.Config = map[string]string{}
	}
	s.idps[idp.Alias] = &idp
	s.mappers[idp.Alias] = nil
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) getIdP(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	idp, ok := s.idps[alias]
	if !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	writeJSON(w, http.StatusOK, redactIdP(*idp))
}

// updateIdP is IdentityProviderResource.update (services/.../resources/admin/
// IdentityProviderResource.java L173-234, 26.8.0): 404 for an unknown alias
// (L176-178), 400 for a changed alias (L210-212, the IllegalArgumentException
// answered as an ErrorResponse, L186-197), and the masked client secret keeps
// the stored one (L219-221). The organization link and the mappers stay.
func (s *Server) updateIdP(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	existing, ok := s.idps[alias]
	if !ok {
		writeError(w, http.StatusNotFound, "HTTP 404 Not Found")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var idp identityProvider
	if err := json.Unmarshal(body, &idp); err != nil {
		writeError(w, http.StatusBadRequest, "HTTP 400 Bad Request")
		return
	}
	if idp.Alias != "" && idp.Alias != alias {
		writeErrorMessage(w, http.StatusBadRequest, "Identity Provider alias cannot be changed")
		return
	}
	idp.Alias = alias
	if idp.Config == nil {
		idp.Config = map[string]string{}
	}
	if idp.Config["clientSecret"] == secretMask {
		idp.Config["clientSecret"] = existing.Config["clientSecret"]
	}
	s.idps[alias] = &idp
	w.WriteHeader(http.StatusNoContent)
}

// importIdPConfig is IdentityProvidersResource.importFrom with a JSON body
// (services/.../resources/admin/IdentityProvidersResource.java L182-203,
// 26.8.0): 400 without providerId and fromUrl (L189-191); Keycloak fetches
// fromUrl itself (L197) and parses it as an OIDC discovery document
// (OIDCIdentityProviderFactory.parseOIDCConfig, L60-85). A fetch or parse
// failure is an uncaught exception, which KeycloakErrorHandler answers 500
// unknown_error.
func (s *Server) importIdPConfig(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req["providerId"] == nil || req["fromUrl"] == nil {
		writeError(w, http.StatusBadRequest, "HTTP 400 Bad Request")
		return
	}
	from, _ := req["fromUrl"].(string)
	var rep struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		UserinfoEndpoint      string `json:"userinfo_endpoint"`
		EndSessionEndpoint    string `json:"end_session_endpoint"`
		JwksURI               string `json:"jwks_uri"`
		IntrospectionEndpoint string `json:"introspection_endpoint"`
	}
	if err := s.fetchJSONUnlocked(from, &rep); err != nil {
		writeError(w, http.StatusInternalServerError, "unknown_error")
		return
	}
	// The setters store what the document has, null for a missing endpoint.
	orNull := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	config := map[string]any{
		"issuer": orNull(rep.Issuer), "authorizationUrl": orNull(rep.AuthorizationEndpoint), "tokenUrl": orNull(rep.TokenEndpoint),
		"userInfoUrl": orNull(rep.UserinfoEndpoint), "logoutUrl": orNull(rep.EndSessionEndpoint),
		// IdentityProvidersResource.java L201.
		"metadataDescriptorUrl": from,
	}
	if rep.JwksURI != "" {
		config["validateSignature"], config["useJwksUrl"], config["jwksUrl"] = "true", "true", rep.JwksURI
	}
	if rep.IntrospectionEndpoint != "" {
		config["tokenIntrospectionUrl"] = rep.IntrospectionEndpoint
	}
	writeJSON(w, http.StatusOK, config)
}

// reloadIdPKeys is IdentityProviderResource.reloadKeys (IdentityProviderResource.java
// L532-547, 26.8.0) for an OIDC provider (OIDCIdentityProvider.reloadKeys, L1093-1100):
// false unless the provider is enabled with useJwksUrl, else Keycloak loads the JWK set
// from jwksUrl (OIDCIdentityProviderPublicKeyLoader) and a failure is 500 unknown_error.
// Keycloak skips a reload within minTimeBetweenRequests (10 s) of the last; an update
// drops the cached keys, and the fake caches none, so it always loads them.
func (s *Server) reloadIdPKeys(w http.ResponseWriter, r *http.Request) {
	idp, ok := s.idps[r.PathValue("alias")]
	if !ok {
		writeError(w, http.StatusNotFound, "HTTP 404 Not Found")
		return
	}
	if !idp.Enabled || idp.Config["useJwksUrl"] != "true" {
		writeJSON(w, http.StatusOK, false)
		return
	}
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := s.fetchJSONUnlocked(idp.Config["jwksUrl"], &jwks); err != nil || jwks.Keys == nil {
		writeError(w, http.StatusInternalServerError, "unknown_error")
		return
	}
	writeJSON(w, http.StatusOK, true)
}

// outboundClient is how the fake fetches what Keycloak would fetch: like Keycloak's
// DefaultHttpClientFactory, it follows no redirects (allow-redirects defaults to false).
var outboundClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// maxConsumedResponseSize is HttpClientProvider.DEFAULT_MAX_CONSUMED_RESPONSE_SIZE.
const maxConsumedResponseSize = 10_000_000

// fetchJSONUnlocked GETs url and decodes a 2xx JSON answer into out. It releases the
// server's lock, which the admin wrapper holds, for the fetch, so a slow remote doesn't
// block other requests.
func (s *Server) fetchJSONUnlocked(url string, out any) error {
	s.mu.Unlock()
	defer s.mu.Lock()
	resp, err := outboundClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConsumedResponseSize+1))
	if err != nil {
		return err
	}
	if len(body) > maxConsumedResponseSize {
		return fmt.Errorf("GET %s: response larger than %d bytes", url, maxConsumedResponseSize)
	}
	return json.Unmarshal(body, out)
}

// LinkFederatedIdentity records that user signed in through the identity provider alias as
// subject: the link Keycloak's first broker login makes (FederatedIdentityModel: identity
// provider alias, user id at the IdP, Keycloak user). The fake has no broker login, so tests
// call this instead.
func (s *Server) LinkFederatedIdentity(alias, user, subject string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.federated[alias] == nil {
		s.federated[alias] = map[string]string{}
	}
	s.federated[alias][user] = subject
}

// countUsers is UsersResource.getUsersCount (services/.../resources/admin/UsersResource.java,
// 26.8.0) for the query the backend sends, idpAlias: the users with a federated identity link
// to that identity provider, 0 for an alias nobody is linked to.
func (s *Server) countUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, len(s.federated[r.URL.Query().Get("idpAlias")]))
}

func (s *Server) deleteIdP(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	delete(s.idps, alias)
	delete(s.mappers, alias)
	// Keycloak removes the users' links to a deleted IdP (UserProvider.preRemove(realm, idp)).
	delete(s.federated, alias)
	for orgID, link := range s.orgIdPs {
		if link.alias == alias {
			delete(s.orgIdPs, orgID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) linkOrgIdP(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	if _, ok := s.orgs[orgID]; !ok {
		writeError(w, http.StatusNotFound, "HTTP 404 Not Found")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var alias string
	if err := json.Unmarshal(body, &alias); err != nil || alias == "" {
		writeErrorMessage(w, http.StatusBadRequest, "Identity provider not found with the given alias")
		return
	}
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusBadRequest, "Identity provider not found with the given alias")
		return
	}
	if _, ok := s.orgIdPs[orgID]; ok {
		writeErrorMessage(w, http.StatusConflict, "Identity provider already associated to the organization")
		return
	}
	s.orgIdPs[orgID] = orgIdPLinkState{alias: alias, link: orgIdPLink{}}
	w.WriteHeader(http.StatusNoContent)
}

// getOrgIdP is OrganizationIdentityProvidersResource.getIdentityProvider
// (services/.../organization/admin/resource/OrganizationIdentityProvidersResource.java
// L138-161, 26.8.0): the IdP when it is linked to the organization, else 404
// "Identity provider not associated with the organization" (L156-158).
func (s *Server) getOrgIdP(w http.ResponseWriter, r *http.Request) {
	orgID, alias := r.PathValue("id"), r.PathValue("alias")
	if _, ok := s.orgs[orgID]; !ok {
		writeError(w, http.StatusNotFound, "HTTP 404 Not Found")
		return
	}
	idp, ok := s.idps[alias]
	if link, linked := s.orgIdPs[orgID]; !ok || !linked || link.alias != alias {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not associated with the organization")
		return
	}
	writeJSON(w, http.StatusOK, redactIdP(*idp))
}

func (s *Server) configureOrgIdP(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	alias := r.PathValue("alias")
	state, ok := s.orgIdPs[orgID]
	if !ok || state.alias != alias {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not associated with the organization")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var link orgIdPLink
	if err := json.Unmarshal(body, &link); err != nil {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid membership type: ")
		return
	}
	if link.MembershipType == "MANAGED" && !link.AutoMembership {
		writeErrorMessage(w, http.StatusBadRequest, "Auto-membership must be enabled when membership type is MANAGED")
		return
	}
	if link.MembershipType != "MANAGED" && link.MembershipType != "UNMANAGED" {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid membership type: "+link.MembershipType)
		return
	}
	state.link = link
	s.orgIdPs[orgID] = state
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listIdPMappers(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	out := s.mappers[alias]
	if out == nil {
		out = []*identityProviderMapper{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createIdPMapper(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var mapper identityProviderMapper
	if err := json.Unmarshal(body, &mapper); err != nil || mapper.Name == "" {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid identity provider mapper")
		return
	}
	mapper.ID = newMapperID()
	mapper.IdentityProviderAlias = alias
	s.mappers[alias] = append(s.mappers[alias], &mapper)
	writeJSON(w, http.StatusCreated, mapper)
}

func (s *Server) updateIdPMapper(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	id := r.PathValue("mapperId")
	list := s.mappers[alias]
	body, _ := io.ReadAll(r.Body)
	for i, m := range list {
		if m.ID != id {
			continue
		}
		var mapper identityProviderMapper
		if err := json.Unmarshal(body, &mapper); err != nil {
			writeErrorMessage(w, http.StatusBadRequest, "Invalid identity provider mapper")
			return
		}
		mapper.ID = id
		mapper.IdentityProviderAlias = alias
		list[i] = &mapper
		s.mappers[alias] = list
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeErrorMessage(w, http.StatusNotFound, "Identity provider mapper not found")
}

// secretMask is ComponentRepresentation.SECRET_VALUE, what Keycloak's StripSecretsUtils
// writes in place of a client secret it returns.
const secretMask = "**********"

func redactIdP(idp identityProvider) identityProvider {
	out := idp
	if out.Config != nil {
		cfg := make(map[string]string, len(out.Config))
		for k, v := range out.Config {
			if strings.EqualFold(k, "clientSecret") {
				cfg[k] = secretMask
				continue
			}
			cfg[k] = v
		}
		out.Config = cfg
	}
	return out
}

func newMapperID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
