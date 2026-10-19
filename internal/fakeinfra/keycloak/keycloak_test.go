package keycloak

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	acmeID   = "3f1c9a72-5b6e-4d2a-8c01-2d3e4f5a6b7c"
	globexID = "8d2e7b31-0c4f-4a9d-b5e6-7f8a9b0c1d2e"
	initID   = "c5a4d3e2-f1b0-4987-a6c5-d4e3f2a1b0c9"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func ptr[T any](v T) *T { return &v }

func seedOrgs() []Organization {
	return []Organization{
		{
			ID: acmeID, Name: "acme-robotics", Alias: "acme-robotics", Enabled: ptr(true), Description: ptr("Acme Robotics, EU region"),
			Attributes: &map[string][]string{"ai-grid/region": {"europe-west"}},
			Domains:    &[]Domain{{Name: "acme-robotics.example.com", Verified: true, AutoRedirect: true}, {Name: "acme.example.org"}},
		},
		{ID: globexID, Name: "globex-research", Alias: "globex", Enabled: ptr(true), Domains: &[]Domain{{Name: "globex.example.com", Verified: true}}},
		{ID: initID, Name: "initech-labs", Enabled: ptr(false)},
	}
}

type fixture struct {
	srv   *httptest.Server
	clock *clock
	token string
}

func start(t *testing.T) *fixture {
	t.Helper()
	c := &clock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	s, err := New(Config{AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "secret", Clock: c.Now, Logger: slog.New(slog.DiscardHandler)}, seedOrgs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	f := &fixture{srv: srv, clock: c}
	code, body, _ := f.tokenRequest(t, "master", url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"secret"}})
	if code != http.StatusOK {
		t.Fatalf("token = %d %s", code, body)
	}
	var tok map[string]any
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatal(err)
	}
	f.token = tok["access_token"].(string)
	return f
}

func (f *fixture) tokenRequest(t *testing.T, realm string, form url.Values) (int, []byte, http.Header) {
	t.Helper()
	resp, err := http.PostForm(f.srv.URL+"/realms/"+realm+"/protocol/openid-connect/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header
}

func (f *fixture) do(t *testing.T, method, path, body string) (int, string, http.Header) {
	t.Helper()
	return f.doToken(t, f.token, method, path, body)
}

func (f *fixture) doToken(t *testing.T, token, method, path, body string) (int, string, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(raw)), resp.Header
}

const orgs = "/admin/realms/ai-grid/organizations"

func names(t *testing.T, body string) []string {
	t.Helper()
	var list []Organization
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("list body %s: %v", body, err)
	}
	out := []string{}
	for _, o := range list {
		out = append(out, o.Name)
	}
	return out
}

func TestTokenEndpoint(t *testing.T) {
	f := start(t)
	ok := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"secret"}}
	code, body, hdr := f.tokenRequest(t, "master", ok)
	if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("token = %d %v", code, hdr)
	}
	var tok map[string]any
	_ = json.Unmarshal(body, &tok)
	if tok["token_type"] != "Bearer" || tok["expires_in"] != float64(60) || tok["access_token"] == "" || tok["refresh_token"] == "" {
		t.Fatalf("token body = %s", body)
	}

	with := func(k, v string) url.Values {
		form := url.Values{}
		for key, val := range ok {
			form[key] = val
		}
		if v == "" {
			form.Del(k)
		} else {
			form.Set(k, v)
		}
		return form
	}
	for _, tc := range []struct {
		name  string
		realm string
		form  url.Values
		code  int
		want  string
	}{
		{"wrong password", "master", with("password", "nope"), 400, `{"error":"invalid_grant","error_description":"Invalid user credentials"}`},
		{"unknown user", "master", with("username", "root"), 400, `{"error":"invalid_grant","error_description":"Invalid user credentials"}`},
		{"admin not in the tenant realm", "ai-grid", ok, 400, `{"error":"invalid_grant","error_description":"Invalid user credentials"}`},
		{"missing username", "master", with("username", ""), 401, `{"error":"invalid_request","error_description":"Missing parameter: username"}`},
		{"other client", "master", with("client_id", "ai-grid-backend"), 401, `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`},
		{"missing grant type", "master", with("grant_type", ""), 400, `{"error":"invalid_request","error_description":"Missing form parameter: grant_type"}`},
		{"unknown realm", "nowhere", ok, 404, `{"error":"Realm does not exist"}`},
	} {
		code, body, _ := f.tokenRequest(t, tc.realm, tc.form)
		if code != tc.code || strings.TrimSpace(string(body)) != tc.want {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, body, tc.code, tc.want)
		}
	}
}

func TestAdminAuthAndRealms(t *testing.T) {
	f := start(t)
	for _, token := range []string{"", "not-issued"} {
		code, body, _ := f.doToken(t, token, http.MethodGet, orgs, "")
		if code != http.StatusUnauthorized || body != `{"error":"HTTP 401 Unauthorized"}` {
			t.Fatalf("token %q: %d %s", token, code, body)
		}
	}
	// Auth comes before the realm lookup.
	if code, _, _ := f.doToken(t, "", http.MethodGet, "/admin/realms/nowhere/organizations", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown realm = %d", code)
	}
	if code, body, _ := f.do(t, http.MethodGet, "/admin/realms/nowhere/organizations", ""); code != 404 || body != `{"error":"Realm not found."}` {
		t.Fatalf("unknown realm = %d %s", code, body)
	}
	if code, body, _ := f.do(t, http.MethodGet, "/admin/realms/master/organizations", ""); code != 404 || body != `{"errorMessage":"Organizations not enabled for this realm."}` {
		t.Fatalf("master realm = %d %s", code, body)
	}
	// Access tokens expire after 60 seconds.
	f.clock.Advance(60 * time.Second)
	if code, _, _ := f.do(t, http.MethodGet, orgs, ""); code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d", code)
	}
}

func TestListParameters(t *testing.T) {
	f := start(t)
	// Twelve more organizations, to see the default page of ten.
	for i := range 12 {
		code, body, _ := f.do(t, http.MethodPost, orgs, fmt.Sprintf(`{"name":"zz-%02d"}`, i))
		if code != http.StatusCreated {
			t.Fatalf("create = %d %s", code, body)
		}
	}
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"", "acme-robotics,globex-research,initech-labs,zz-00,zz-01,zz-02,zz-03,zz-04,zz-05,zz-06"},
		{"?first=0&max=100", "acme-robotics,globex-research,initech-labs,zz-00,zz-01,zz-02,zz-03,zz-04,zz-05,zz-06,zz-07,zz-08,zz-09,zz-10,zz-11"},
		{"?first=2&max=2", "initech-labs,zz-00"},
		{"?first=-5&max=-1", "acme-robotics,globex-research,initech-labs,zz-00,zz-01,zz-02,zz-03,zz-04,zz-05,zz-06,zz-07,zz-08,zz-09,zz-10,zz-11"},
		{"?search=ROBOT", "acme-robotics"},
		{"?search=example.org", "acme-robotics"},
		{"?search=glob_x", "globex-research"},
		{"?search=%25labs", "initech-labs"},
		{"?search=globex.example.com&exact=true", "globex-research"},
		{"?search=GLOBEX-RESEARCH&exact=true", ""},
		{"?search=globex-research&exact=true&first=0&max=2", "globex-research"},
		{"?search=%20&max=3", "acme-robotics,globex-research,initech-labs"},
		{"?q=ai-grid/region:europe-west&search=zz", "acme-robotics"},
	} {
		code, body, _ := f.do(t, http.MethodGet, orgs+tc.query, "")
		if code != http.StatusOK || strings.Join(names(t, body), ",") != tc.want {
			t.Errorf("GET %s = %d %v, want %s", tc.query, code, names(t, body), tc.want)
		}
	}
	if _, body, _ := f.do(t, http.MethodGet, orgs+"/count?search=zz", ""); body != "12" {
		t.Errorf("count = %s", body)
	}
	if code, _, _ := f.do(t, http.MethodGet, orgs+"?max=ten", ""); code != http.StatusNotFound {
		t.Errorf("bad max = %d", code)
	}
}

func TestRepresentations(t *testing.T) {
	f := start(t)
	_, brief, _ := f.do(t, http.MethodGet, orgs+"?search=acme", "")
	want := `[{"id":"` + acmeID + `","name":"acme-robotics","alias":"acme-robotics","enabled":true,"description":"Acme Robotics, EU region","domains":[{"name":"acme-robotics.example.com","verified":true,"autoRedirect":true},{"name":"acme.example.org","verified":false,"autoRedirect":false}]}]`
	if brief != want {
		t.Fatalf("brief\n got %s\nwant %s", brief, want)
	}
	_, full, _ := f.do(t, http.MethodGet, orgs+"?search=acme&briefRepresentation=false", "")
	if !strings.Contains(full, `"attributes":{"ai-grid/region":["europe-west"]}`) {
		t.Fatalf("full = %s", full)
	}
	// GET by id is always full; no domains means no key; alias defaults to name.
	code, one, _ := f.do(t, http.MethodGet, orgs+"/"+initID, "")
	if code != 200 || one != `{"id":"`+initID+`","name":"initech-labs","alias":"initech-labs","enabled":false,"attributes":{}}` {
		t.Fatalf("get = %d %s", code, one)
	}
	code, missing, _ := f.do(t, http.MethodGet, orgs+"/00000000-0000-4000-8000-000000000000", "")
	if code != 404 || missing != `{"errorMessage":"Organization not found."}` {
		t.Fatalf("get missing = %d %s", code, missing)
	}
}

func TestCreate(t *testing.T) {
	f := start(t)
	code, body, hdr := f.do(t, http.MethodPost, orgs, `{"id":"ignored","name":"wayne-aerospace","domains":[{"name":" Wayne-Aero.Example.com "}]}`)
	if code != http.StatusCreated || body != "" {
		t.Fatalf("create = %d %q", code, body)
	}
	loc := hdr.Get("Location")
	prefix := f.srv.URL + orgs + "/"
	id := strings.TrimPrefix(loc, prefix)
	if !strings.HasPrefix(loc, prefix) || !uuidPattern.MatchString(id) {
		t.Fatalf("Location = %q", loc)
	}
	_, got, _ := f.do(t, http.MethodGet, orgs+"/"+id, "")
	if got != `{"id":"`+id+`","name":"wayne-aerospace","alias":"wayne-aerospace","enabled":true,"attributes":{},"domains":[{"name":"wayne-aero.example.com","verified":false,"autoRedirect":false}]}` {
		t.Fatalf("created = %s", got)
	}

	for _, tc := range []struct {
		name, body string
		code       int
		want       string
	}{
		{"null", `null`, 400, `{"errorMessage":"Organization cannot be null."}`},
		{"blank name", `{"name":"  "}`, 400, `{"errorMessage":"Name can not be null"}`},
		{"explicit alias with space", `{"name":"x","alias":"a b"}`, 400, `{"error":"Empty Space not allowed."}`},
		{"name not usable as alias", `{"name":"a/b"}`, 400, `{"errorMessage":"Name cannot be used as alias: Character '/' not allowed."}`},
		{"duplicate name", `{"name":"acme-robotics","alias":"other"}`, 409, `{"errorMessage":"A organization with the same name already exists."}`},
		{"duplicate alias", `{"name":"globex-2","alias":"globex"}`, 409, `{"errorMessage":"A organization with the same alias already exists"}`},
		{"bad domain", `{"name":"x","domains":[{"name":"Not A Domain"}]}`, 400, `{"errorMessage":"Invalid domain format: not a domain"}`},
		{"wildcard base", `{"name":"x","domains":[{"name":"*.com"}]}`, 400, `{"errorMessage":"Domain must have at least 2 parts (e.g. 'example.com'): com"}`},
		{"linked domain", `{"name":"x","domains":[{"name":"GLOBEX.example.com"}]}`, 400, `{"errorMessage":"Domain globex.example.com is already linked to organization globex-research in realm ai-grid"}`},
		{"identity provider", `{"name":"x","domains":[{"name":"x.example.com","identityProviderAlias":"x-oidc"}]}`, 400, `{"errorMessage":"Identity provider with alias 'x-oidc' does not exist in realm ai-grid"}`},
		{"redirect url", `{"name":"x","redirectUrl":"not a url"}`, 400, `{"errorMessage":"Organization redirect URL is not valid."}`},
	} {
		code, body, _ := f.do(t, http.MethodPost, orgs, tc.body)
		if code != tc.code || body != tc.want {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, body, tc.code, tc.want)
		}
	}
	// Nothing from the failed requests was stored.
	if _, n, _ := f.do(t, http.MethodGet, orgs+"/count", ""); n != "4" {
		t.Fatalf("count after failures = %s", n)
	}
}

func TestUpdateFullRepresentation(t *testing.T) {
	f := start(t)
	// A body without description, enabled or attributes: the description
	// is cleared, enabled becomes true, attributes and domains stay.
	code, body, _ := f.do(t, http.MethodPut, orgs+"/"+acmeID, `{"name":"acme-robotics","alias":"acme-robotics","enabled":false}`)
	if code != http.StatusNoContent || body != "" {
		t.Fatalf("PUT = %d %s", code, body)
	}
	_, got, _ := f.do(t, http.MethodGet, orgs+"/"+acmeID, "")
	want := `{"id":"` + acmeID + `","name":"acme-robotics","alias":"acme-robotics","enabled":false,"attributes":{"ai-grid/region":["europe-west"]},"domains":[{"name":"acme-robotics.example.com","verified":true,"autoRedirect":true},{"name":"acme.example.org","verified":false,"autoRedirect":false}]}`
	if got != want {
		t.Fatalf("after PUT\n got %s\nwant %s", got, want)
	}
	// Empty collections clear; a domain that stays takes the body's fields.
	f.do(t, http.MethodPut, orgs+"/"+acmeID, `{"name":"acme-robotics-eu","alias":"acme-robotics","attributes":{},"domains":[{"name":"acme-robotics.example.com"}]}`)
	_, got, _ = f.do(t, http.MethodGet, orgs+"/"+acmeID, "")
	want = `{"id":"` + acmeID + `","name":"acme-robotics-eu","alias":"acme-robotics","enabled":true,"attributes":{},"domains":[{"name":"acme-robotics.example.com","verified":false,"autoRedirect":false}]}`
	if got != want {
		t.Fatalf("after second PUT\n got %s\nwant %s", got, want)
	}

	for _, tc := range []struct {
		name, id, body string
		code           int
		want           string
	}{
		{"name taken", globexID, `{"name":"initech-labs","alias":"globex"}`, 409, `{"errorMessage":"A organization with the same name already exists."}`},
		{"name equals a domain", globexID, `{"name":"acme-robotics.example.com","alias":"globex"}`, 409, `{"errorMessage":"A organization with the same name already exists."}`},
		{"no name", globexID, `{"alias":"globex"}`, 409, `{"errorMessage":"A organization with the same name already exists."}`},
		{"alias omitted", globexID, `{"name":"globex-research"}`, 400, `{"errorMessage":"Cannot change the alias"}`},
		{"alias changed", globexID, `{"name":"globex-research","alias":"gx"}`, 400, `{"errorMessage":"Cannot change the alias"}`},
		{"domain of another org", globexID, `{"name":"globex-research","alias":"globex","domains":[{"name":"acme-robotics.example.com"}]}`, 400, `{"errorMessage":"Domain acme-robotics.example.com is already linked to organization acme-robotics-eu in realm ai-grid"}`},
		{"missing", "00000000-0000-4000-8000-000000000000", `{"name":"x"}`, 404, `{"errorMessage":"Organization not found."}`},
	} {
		code, body, _ := f.do(t, http.MethodPut, orgs+"/"+tc.id, tc.body)
		if code != tc.code || body != tc.want {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, body, tc.code, tc.want)
		}
	}
	// Keeping its own domain is fine; alias omitted is fine when the name is the alias.
	if code, body, _ := f.do(t, http.MethodPut, orgs+"/"+initID, `{"name":"initech-labs","domains":[{"name":"initech.example.com"}]}`); code != 204 {
		t.Fatalf("PUT initech = %d %s", code, body)
	}
	if code, body, _ := f.do(t, http.MethodPut, orgs+"/"+initID, `{"name":"initech-labs","domains":[{"name":"initech.example.com","verified":true}]}`); code != 204 {
		t.Fatalf("PUT initech again = %d %s", code, body)
	}
}

func TestDelete(t *testing.T) {
	f := start(t)
	if code, body, _ := f.do(t, http.MethodDelete, orgs+"/"+globexID, ""); code != http.StatusNoContent || body != "" {
		t.Fatalf("DELETE = %d %s", code, body)
	}
	if code, body, _ := f.do(t, http.MethodDelete, orgs+"/"+globexID, ""); code != 404 || body != `{"errorMessage":"Organization not found."}` {
		t.Fatalf("DELETE again = %d %s", code, body)
	}
	// Its domain is free again.
	if code, body, _ := f.do(t, http.MethodPost, orgs, `{"name":"globex-2","domains":[{"name":"globex.example.com"}]}`); code != http.StatusCreated {
		t.Fatalf("reuse domain = %d %s", code, body)
	}
}

func TestSeedValidation(t *testing.T) {
	cfg := Config{AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin"}
	for name, seed := range map[string][]Organization{
		"non-uuid id":     {{ID: "acme", Name: "acme"}},
		"duplicate name":  {{ID: acmeID, Name: "acme"}, {ID: globexID, Name: "acme", Alias: "b"}},
		"duplicate alias": {{ID: acmeID, Name: "a", Alias: "x"}, {ID: globexID, Name: "b", Alias: "x"}},
		"shared domain":   {{ID: acmeID, Name: "a", Domains: &[]Domain{{Name: "a.example.com"}}}, {ID: globexID, Name: "b", Domains: &[]Domain{{Name: "a.example.com"}}}},
		"bad domain":      {{ID: acmeID, Name: "a", Domains: &[]Domain{{Name: "not a domain"}}}},
	} {
		if _, err := New(cfg, seed); err == nil {
			t.Errorf("%s: New() accepted the seed", name)
		}
	}
}

func TestLikeMatch(t *testing.T) {
	for _, tc := range []struct {
		value, pattern string
		want           bool
	}{
		{"acme-robotics", "%robot%", true},
		{"acme-robotics", "%rob_t%", true},
		{"acme-robotics", "%rob\\_t%", false},
		{"rob_t", "%rob\\_t%", true},
		{"a.b", "%a.b%", true},
		{"axb", "%a.b%", false},
	} {
		if got := likeMatch(tc.value, tc.pattern); got != tc.want {
			t.Errorf("likeMatch(%q, %q) = %v", tc.value, tc.pattern, got)
		}
	}
}
