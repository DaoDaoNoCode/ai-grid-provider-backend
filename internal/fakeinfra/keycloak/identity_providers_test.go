package keycloak

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIdentityProviderRoutes(t *testing.T) {
	t.Parallel()
	srv, err := New(Config{
		AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin",
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token := grantToken(t, ts)

	createOrg(t, ts, token, map[string]any{"name": "acme", "alias": "acme"})
	orgID := firstOrgID(t, ts, token)

	createBody, _ := json.Marshal(map[string]any{
		"alias": "acme", "providerId": "oidc", "enabled": true, "trustEmail": true,
		"firstBrokerLoginFlowAlias": "ai-grid-first-broker-login",
		"config":                    map[string]string{"clientId": "c", "clientSecret": "secret-value"},
	})
	res := doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/identity-provider/instances", createBody)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create idp: %d %s", res.StatusCode, readBody(res))
	}

	res = doAdmin(t, ts, token, http.MethodGet, "/admin/realms/ai-grid/identity-provider/instances/acme", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get idp: %d", res.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(res.Body).Decode(&got)
	cfg := got["config"].(map[string]any)
	if cfg["clientSecret"] != "**********" {
		t.Fatalf("get leaked client secret: %#v", cfg)
	}
	if got["trustEmail"] != true {
		t.Fatalf("get trustEmail = %#v, want true", got["trustEmail"])
	}
	if got["firstBrokerLoginFlowAlias"] != "ai-grid-first-broker-login" {
		t.Fatalf("get firstBrokerLoginFlowAlias = %#v", got["firstBrokerLoginFlowAlias"])
	}

	orgIdP := "/admin/realms/ai-grid/organizations/" + orgID + "/identity-providers/acme"
	if res := doAdmin(t, ts, token, http.MethodGet, orgIdP, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("get unlinked org idp: %d %s, want 404", res.StatusCode, readBody(res))
	}

	linkBody, _ := json.Marshal("acme")
	res = doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/organizations/"+orgID+"/identity-providers", linkBody)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("link: %d %s", res.StatusCode, readBody(res))
	}

	if res := doAdmin(t, ts, token, http.MethodGet, orgIdP, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("get linked org idp: %d %s, want 200", res.StatusCode, readBody(res))
	}

	cfgBody, _ := json.Marshal(map[string]any{"autoMembership": true, "membershipType": "MANAGED"})
	res = doAdmin(t, ts, token, http.MethodPut, "/admin/realms/ai-grid/organizations/"+orgID+"/identity-providers/acme", cfgBody)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("configure: %d %s", res.StatusCode, readBody(res))
	}

	mapperBody, _ := json.Marshal(map[string]any{
		"name": "AI Grid org id", "identityProviderAlias": "acme",
		"identityProviderMapper": "hardcoded-attribute-idp-mapper",
		"config":                 map[string]string{"attribute": "maas_org_id", "attribute.value": orgID, "syncMode": "FORCE"},
	})
	res = doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/identity-provider/instances/acme/mappers", mapperBody)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create mapper: %d %s", res.StatusCode, readBody(res))
	}
	var mapper map[string]any
	_ = json.NewDecoder(res.Body).Decode(&mapper)
	mapperID, _ := mapper["id"].(string)

	res = doAdmin(t, ts, token, http.MethodGet, "/admin/realms/ai-grid/identity-provider/instances/acme/mappers", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list mappers: %d", res.StatusCode)
	}

	updateBody, _ := json.Marshal(mapper)
	res = doAdmin(t, ts, token, http.MethodPut, "/admin/realms/ai-grid/identity-provider/instances/acme/mappers/"+mapperID, updateBody)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("update mapper: %d %s", res.StatusCode, readBody(res))
	}

	res = doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/identity-provider/instances", createBody)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate idp: %d", res.StatusCode)
	}

	res = doAdmin(t, ts, token, http.MethodDelete, "/admin/realms/ai-grid/identity-provider/instances/acme", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete idp: %d", res.StatusCode)
	}
	res = doAdmin(t, ts, token, http.MethodGet, "/admin/realms/ai-grid/identity-provider/instances/acme", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: %d", res.StatusCode)
	}
}

// TestIdentityProviderUpdateAndChecks: PUT keeps the stored secret for the
// masked value and refuses a new alias; import-config and reload-keys fetch
// the IdP's documents as Keycloak does, and answer 500 when they can't.
func TestIdentityProviderUpdateAndChecks(t *testing.T) {
	t.Parallel()
	idpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": "https://idp.example.com", "authorization_endpoint": "https://idp.example.com/auth",
				"token_endpoint": "https://idp.example.com/token", "jwks_uri": "https://idp.example.com/keys",
			})
		case "/keys":
			_, _ = io.WriteString(w, `{"keys":[]}`)
		case "/moved":
			http.Redirect(w, r, "/.well-known/openid-configuration", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idpServer.Close)
	srv, err := New(Config{AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token := grantToken(t, ts)
	const instance = "/admin/realms/ai-grid/identity-provider/instances/acme"

	create := func(jwksURL string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"alias": "acme", "providerId": "oidc", "enabled": true,
			"config": map[string]string{"clientId": "c", "clientSecret": "s1", "useJwksUrl": "true", "jwksUrl": jwksURL},
		})
		if res := doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/identity-provider/instances", body); res.StatusCode != http.StatusCreated {
			t.Fatalf("create idp: %d %s", res.StatusCode, readBody(res))
		}
	}
	create(idpServer.URL + "/keys")

	res := doAdmin(t, ts, token, http.MethodGet, instance+"/reload-keys", nil)
	if got := readBody(res); res.StatusCode != http.StatusOK || strings.TrimSpace(got) != "true" {
		t.Fatalf("reload-keys = %d %s, want 200 true", res.StatusCode, got)
	}

	put, _ := json.Marshal(map[string]any{
		"alias": "acme", "providerId": "oidc", "enabled": true,
		"config": map[string]string{"clientId": "c2", "clientSecret": "**********", "useJwksUrl": "true", "jwksUrl": idpServer.URL + "/missing"},
	})
	if res := doAdmin(t, ts, token, http.MethodPut, instance, put); res.StatusCode != http.StatusNoContent {
		t.Fatalf("update idp: %d %s", res.StatusCode, readBody(res))
	}
	srv.mu.Lock()
	stored := srv.idps["acme"].Config
	srv.mu.Unlock()
	if stored["clientId"] != "c2" || stored["clientSecret"] != "s1" {
		t.Fatalf("stored config = %v, want c2 with the secret kept", stored)
	}
	res = doAdmin(t, ts, token, http.MethodGet, instance+"/reload-keys", nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("reload-keys with a missing JWKS = %d %s, want 500", res.StatusCode, readBody(res))
	}
	countUsers := func() string {
		res := doAdmin(t, ts, token, http.MethodGet, "/admin/realms/ai-grid/users/count?idpAlias=acme", nil)
		return strings.TrimSpace(readBody(res))
	}
	if got := countUsers(); got != "0" {
		t.Fatalf("users linked before a sign-in = %s, want 0", got)
	}
	srv.LinkFederatedIdentity("acme", "user-1", "sub-1")
	srv.LinkFederatedIdentity("acme", "user-2", "sub-2")
	if got := countUsers(); got != "2" {
		t.Fatalf("users linked = %s, want 2", got)
	}

	renamed, _ := json.Marshal(map[string]any{"alias": "other", "providerId": "oidc", "config": map[string]string{}})
	if res := doAdmin(t, ts, token, http.MethodPut, instance, renamed); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename idp: %d %s, want 400", res.StatusCode, readBody(res))
	}

	importConfig := func(from string) *http.Response {
		body, _ := json.Marshal(map[string]string{"providerId": "oidc", "fromUrl": from})
		return doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/identity-provider/import-config", body)
	}
	res = importConfig(idpServer.URL + "/.well-known/openid-configuration")
	var config map[string]any
	_ = json.NewDecoder(res.Body).Decode(&config)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || config["issuer"] != "https://idp.example.com" || config["jwksUrl"] != "https://idp.example.com/keys" || config["userInfoUrl"] != nil {
		t.Fatalf("import-config = %d %v", res.StatusCode, config)
	}
	for _, from := range []string{idpServer.URL + "/missing", idpServer.URL + "/moved"} {
		if res := importConfig(from); res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("import-config %s = %d %s, want 500", from, res.StatusCode, readBody(res))
		}
	}
}

// TestIdentityProviderKeepsFieldsItDoesNotModel: GET, change, PUT, GET keeps the
// representation's other fields, as Keycloak stores them.
func TestIdentityProviderKeepsFieldsItDoesNotModel(t *testing.T) {
	t.Parallel()
	srv, err := New(Config{AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token := grantToken(t, ts)
	const instance = "/admin/realms/ai-grid/identity-provider/instances/acme"
	created, _ := json.Marshal(map[string]any{
		"alias": "acme", "providerId": "oidc", "enabled": true, "hideOnLogin": true, "firstBrokerLoginFlowAlias": "custom flow",
		"config": map[string]string{"clientId": "c", "clientSecret": "s", "pkceEnabled": "true"},
	})
	if res := doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/identity-provider/instances", created); res.StatusCode != http.StatusCreated {
		t.Fatalf("create idp: %d %s", res.StatusCode, readBody(res))
	}
	get := func() map[string]any {
		t.Helper()
		res := doAdmin(t, ts, token, http.MethodGet, instance, nil)
		var rep map[string]any
		if err := json.Unmarshal([]byte(readBody(res)), &rep); err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("get idp: %d %v", res.StatusCode, err)
		}
		return rep
	}
	rep := get()
	rep["config"].(map[string]any)["clientId"] = "c2"
	body, _ := json.Marshal(rep)
	if res := doAdmin(t, ts, token, http.MethodPut, instance, body); res.StatusCode != http.StatusNoContent {
		t.Fatalf("update idp: %d %s", res.StatusCode, readBody(res))
	}
	rep = get()
	cfg := rep["config"].(map[string]any)
	if rep["hideOnLogin"] != true || rep["firstBrokerLoginFlowAlias"] != "custom flow" || cfg["pkceEnabled"] != "true" || cfg["clientId"] != "c2" {
		t.Fatalf("idp after update = %v, want hideOnLogin, the flow and pkceEnabled kept with clientId c2", rep)
	}
}

func grantToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	res, err := http.PostForm(ts.URL+"/realms/master/protocol/openid-connect/token", map[string][]string{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {"admin"}, "password": {"admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(res.Body).Decode(&tok)
	if tok.AccessToken == "" {
		t.Fatal("empty token")
	}
	return tok.AccessToken
}

func doAdmin(t *testing.T, ts *httptest.Server, token, method, path string, body []byte) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func createOrg(t *testing.T, ts *httptest.Server, token string, org map[string]any) {
	t.Helper()
	body, _ := json.Marshal(org)
	res := doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/organizations", body)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create org: %d %s", res.StatusCode, readBody(res))
	}
	res.Body.Close()
}

func firstOrgID(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	res := doAdmin(t, ts, token, http.MethodGet, "/admin/realms/ai-grid/organizations?briefRepresentation=false&first=0&max=100", nil)
	defer res.Body.Close()
	var orgs []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&orgs)
	if len(orgs) == 0 {
		t.Fatal("no orgs")
	}
	id, _ := orgs[0]["id"].(string)
	return id
}

func readBody(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return string(b)
}
