package keycloak

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		"alias": "acme", "providerId": "oidc", "enabled": true,
		"config": map[string]string{"clientId": "c", "clientSecret": "secret-value"},
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
	if cfg["clientSecret"] != "" {
		t.Fatalf("get leaked client secret: %#v", cfg)
	}

	linkBody, _ := json.Marshal("acme")
	res = doAdmin(t, ts, token, http.MethodPost, "/admin/realms/ai-grid/organizations/"+orgID+"/identity-providers", linkBody)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("link: %d %s", res.StatusCode, readBody(res))
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
