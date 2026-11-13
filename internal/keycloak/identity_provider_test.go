package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"strings"
	"testing"
)

func TestCreateIdentityProvider(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/identity-provider/instances"):
			gotMethod, gotPath = r.Method, r.URL.Path
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBody)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := testClient(server)
	err := client.CreateIdentityProvider(context.Background(), IdentityProvider{
		Alias: "acme", DisplayName: "Acme", ProviderID: "oidc", Enabled: true, TrustEmail: true,
		Config: map[string]string{"clientId": "c", "clientSecret": "s"},
	})
	if err != nil {
		t.Fatalf("CreateIdentityProvider: %v", err)
	}
	if gotMethod != http.MethodPost || !strings.HasSuffix(gotPath, "/identity-provider/instances") {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotBody["alias"] != "acme" || gotBody["providerId"] != "oidc" || gotBody["trustEmail"] != true {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestCreateIdentityProviderUsesBuiltInFirstBrokerLoginFlow(t *testing.T) {
	t.Parallel()
	var flowLookups int
	var gotBody map[string]any
	server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/authentication/flows") {
			flowLookups++
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/identity-provider/instances") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	err := testClient(server).CreateIdentityProvider(context.Background(), IdentityProvider{
		Alias: "acme", ProviderID: "oidc",
		Extra: map[string]json.RawMessage{
			"firstBrokerLoginFlowAlias": json.RawMessage(`"first broker login"`),
		},
	})
	if err != nil {
		t.Fatalf("CreateIdentityProvider: %v", err)
	}
	if flowLookups != 0 {
		t.Fatalf("authentication-flow lookups = %d, want 0", flowLookups)
	}
	if gotBody["firstBrokerLoginFlowAlias"] != "first broker login" {
		t.Fatalf("firstBrokerLoginFlowAlias = %#v", gotBody["firstBrokerLoginFlowAlias"])
	}
}

func TestDeleteIdentityProvider(t *testing.T) {
	t.Parallel()
	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		var gotMethod, gotPath string
		server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)
		if err := testClient(server).DeleteIdentityProvider(context.Background(), "acme"); err != nil {
			t.Fatalf("DeleteIdentityProvider: %v", err)
		}
		if gotMethod != http.MethodDelete || !strings.HasSuffix(gotPath, "/identity-provider/instances/acme") {
			t.Fatalf("request = %s %s", gotMethod, gotPath)
		}
	})
	t.Run("404 is success", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "missing", http.StatusNotFound)
		}))
		t.Cleanup(server.Close)
		if err := testClient(server).DeleteIdentityProvider(context.Background(), "acme"); err != nil {
			t.Fatalf("DeleteIdentityProvider 404: %v", err)
		}
	})
}

func TestLinkOrganizationIdentityProvider(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotBody string
	server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	if err := testClient(server).LinkOrganizationIdentityProvider(context.Background(), "org-1", "acme"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if gotMethod != http.MethodPost || !strings.HasSuffix(gotPath, "/organizations/org-1/identity-providers") {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotBody != `"acme"` {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestConfigureOrganizationIdentityProvider(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	var gotBody OrganizationIdentityProviderLink
	server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	if err := testClient(server).ConfigureOrganizationIdentityProvider(context.Background(), "org-1", "acme"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if gotMethod != http.MethodPut || !strings.HasSuffix(gotPath, "/organizations/org-1/identity-providers/acme") {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if !gotBody.AutoMembership || gotBody.MembershipType != "MANAGED" {
		t.Fatalf("body = %+v", gotBody)
	}
}

func TestUpsertIdentityProviderMapper(t *testing.T) {
	t.Parallel()
	t.Run("create all six when missing", func(t *testing.T) {
		t.Parallel()
		recorder := newMapperRecorder(t, nil)
		if err := testClient(recorder.server).UpsertIdentityProviderMapper(context.Background(), IdentityProviderMapperSpec{
			Alias: "acme", OrganizationID: "uuid-1", OrganizationName: "Acme",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		created := map[string]IdentityProviderMapper{}
		for _, mapper := range recorder.mappers {
			created[mapper.Name] = mapper
		}
		want := map[string][2]string{
			"AI Grid org id":   {"maas_org_id", "uuid-1"},
			"AI Grid org name": {"maas_org_name", "Acme"},
		}
		for name, w := range want {
			m := created[name]
			if m.IdentityProviderMapper != "hardcoded-attribute-idp-mapper" || m.Config["attribute"] != w[0] || m.Config["attribute.value"] != w[1] || m.Config["syncMode"] != "FORCE" {
				t.Fatalf("%s = %+v", name, m)
			}
		}
		for name, claim := range map[string]string{"AI Grid groups": "groups", "AI Grid roles": "roles"} {
			m := created[name]
			if m.IdentityProviderMapper != "oidc-user-attribute-idp-mapper" || m.Config["claim"] != claim || m.Config["user.attribute"] != "maas_"+claim || m.Config["syncMode"] != "FORCE" {
				t.Fatalf("%s = %+v", name, m)
			}
		}
		localUsername := created["AI Grid local username"]
		wantLocalUsername := IdentityProviderMapper{
			Name: "AI Grid local username", IdentityProviderAlias: "acme",
			IdentityProviderMapper: "oidc-username-idp-mapper",
			Config:                 map[string]string{"template": "${UUID}", "target": "LOCAL", "syncMode": "IMPORT"},
		}
		localUsername.ID = ""
		if !reflect.DeepEqual(localUsername, wantLocalUsername) {
			t.Fatalf("AI Grid local username = %+v, want %+v", localUsername, wantLocalUsername)
		}
		preferredUsername := created["AI Grid preferred username"]
		if preferredUsername.IdentityProviderMapper != "oidc-user-attribute-idp-mapper" ||
			preferredUsername.Config["claim"] != "preferred_username" ||
			preferredUsername.Config["user.attribute"] != "maas_preferred_username" ||
			preferredUsername.Config["syncMode"] != "FORCE" {
			t.Fatalf("AI Grid preferred username = %+v", preferredUsername)
		}
		if len(created) != 6 {
			t.Fatalf("created %d mappers, want 6", len(created))
		}
	})
	t.Run("update new mappers by id and create the rest", func(t *testing.T) {
		t.Parallel()
		existing := []IdentityProviderMapper{
			{ID: "org-id", Name: "AI Grid org id", IdentityProviderAlias: "acme"},
			{ID: "local-username", Name: "AI Grid local username", IdentityProviderAlias: "acme"},
			{ID: "preferred-username", Name: "AI Grid preferred username", IdentityProviderAlias: "acme"},
		}
		recorder := newMapperRecorder(t, existing)
		if err := testClient(recorder.server).UpsertIdentityProviderMapper(context.Background(), IdentityProviderMapperSpec{
			Alias: "acme", OrganizationID: "uuid-2", OrganizationName: "Acme",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		_, updatedOrgID := recorder.puts["org-id"]
		_, updatedLocalUsername := recorder.puts["local-username"]
		_, updatedPreferredUsername := recorder.puts["preferred-username"]
		if recorder.posts != 3 || !updatedOrgID || !updatedLocalUsername || !updatedPreferredUsername {
			t.Fatalf("posts=%d puts=%v", recorder.posts, recorder.puts)
		}
		if recorder.puts["local-username"].Config["syncMode"] != "IMPORT" || recorder.puts["preferred-username"].Config["syncMode"] != "FORCE" {
			t.Fatalf("sync modes = local %+v preferred %+v", recorder.puts["local-username"], recorder.puts["preferred-username"])
		}
	})
	t.Run("repeated upsert does not duplicate mappers", func(t *testing.T) {
		t.Parallel()
		recorder := newMapperRecorder(t, nil)
		client := testClient(recorder.server)
		for range 2 {
			if err := client.UpsertIdentityProviderMapper(context.Background(), IdentityProviderMapperSpec{
				Alias: "acme", OrganizationID: "uuid-1", OrganizationName: "Acme",
			}); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
		}
		if recorder.posts != 6 || len(recorder.puts) != 6 || len(recorder.mappers) != 6 {
			t.Fatalf("posts=%d puts=%d mappers=%d", recorder.posts, len(recorder.puts), len(recorder.mappers))
		}
	})
	t.Run("rejects an ambiguous username preprocessor before writes", func(t *testing.T) {
		t.Parallel()
		existing := []IdentityProviderMapper{{
			ID: "legacy", Name: "Legacy username", IdentityProviderAlias: "acme",
			IdentityProviderMapper: "oidc-username-idp-mapper",
		}}
		recorder := newMapperRecorder(t, existing)
		err := testClient(recorder.server).UpsertIdentityProviderMapper(context.Background(), IdentityProviderMapperSpec{
			Alias: "acme", OrganizationID: "uuid-1", OrganizationName: "Acme",
		})
		var conflict *UsernameMapperConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("Upsert error = %v, want UsernameMapperConflictError", err)
		}
		if conflict.Alias != "acme" || conflict.MapperName != "Legacy username" {
			t.Fatalf("conflict = %+v, want acme and Legacy username", conflict)
		}
		if recorder.posts != 0 || len(recorder.puts) != 0 || len(recorder.mappers) != 1 {
			t.Fatalf("writes occurred: posts=%d puts=%d mappers=%d", recorder.posts, len(recorder.puts), len(recorder.mappers))
		}
	})
}

type mapperRecorder struct {
	server  *httptest.Server
	mappers map[string]IdentityProviderMapper
	puts    map[string]IdentityProviderMapper
	posts   int
}

func newMapperRecorder(t *testing.T, existing []IdentityProviderMapper) *mapperRecorder {
	t.Helper()
	recorder := &mapperRecorder{
		mappers: map[string]IdentityProviderMapper{},
		puts:    map[string]IdentityProviderMapper{},
	}
	for _, mapper := range existing {
		recorder.mappers[mapper.ID] = mapper
	}
	recorder.server = httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mappers"):
			mappers := make([]IdentityProviderMapper, 0, len(recorder.mappers))
			for _, mapper := range recorder.mappers {
				mappers = append(mappers, mapper)
			}
			_ = json.NewEncoder(w).Encode(mappers)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mappers"):
			var mapper IdentityProviderMapper
			_ = json.NewDecoder(r.Body).Decode(&mapper)
			mapper.ID = fmt.Sprintf("mapper-%d", len(recorder.mappers)+1)
			recorder.mappers[mapper.ID] = mapper
			recorder.posts++
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut:
			id := path.Base(r.URL.Path)
			var mapper IdentityProviderMapper
			_ = json.NewDecoder(r.Body).Decode(&mapper)
			mapper.ID = id
			recorder.mappers[id] = mapper
			recorder.puts[id] = mapper
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

// TestGetAndUpdateIdentityProviderKeepUnmodelledKeys: a GET→PUT writes back the
// representation's keys IdentityProvider doesn't model.
func TestGetAndUpdateIdentityProviderKeepUnmodelledKeys(t *testing.T) {
	t.Parallel()
	var putMethod, putPath string
	var putBody map[string]any
	server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"alias":"acme","internalId":"i-1","providerId":"oidc","enabled":true,"trustEmail":true,"hideOnLogin":true,"config":{"clientId":"c","clientSecret":"**********"}}`)
		case http.MethodPut:
			putMethod, putPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	client := testClient(server)
	idp, err := client.GetIdentityProvider(context.Background(), "acme")
	if err != nil {
		t.Fatalf("GetIdentityProvider: %v", err)
	}
	idp.Config["clientId"] = "c2"
	if err := client.UpdateIdentityProvider(context.Background(), idp); err != nil {
		t.Fatalf("UpdateIdentityProvider: %v", err)
	}
	if putMethod != http.MethodPut || !strings.HasSuffix(putPath, "/identity-provider/instances/acme") {
		t.Fatalf("request = %s %s", putMethod, putPath)
	}
	cfg, _ := putBody["config"].(map[string]any)
	if putBody["hideOnLogin"] != true || putBody["internalId"] != "i-1" || cfg["clientId"] != "c2" || cfg["clientSecret"] != "**********" {
		t.Fatalf("body = %#v", putBody)
	}
}

func TestImportIdentityProviderConfig(t *testing.T) {
	t.Parallel()
	t.Run("returns the config Keycloak read", func(t *testing.T) {
		t.Parallel()
		var gotMethod, gotPath string
		var gotBody map[string]string
		server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = io.WriteString(w, `{"issuer":"https://idp.example.com","jwksUrl":"https://idp.example.com/keys","logoutUrl":null}`)
		}))
		t.Cleanup(server.Close)
		got, err := testClient(server).ImportIdentityProviderConfig(context.Background(), "https://idp.example.com/.well-known/openid-configuration")
		if err != nil {
			t.Fatalf("ImportIdentityProviderConfig: %v", err)
		}
		if gotMethod != http.MethodPost || gotPath != "/admin/realms/ai-grid/identity-provider/import-config" {
			t.Fatalf("request = %s %s", gotMethod, gotPath)
		}
		if gotBody["providerId"] != "oidc" || gotBody["fromUrl"] != "https://idp.example.com/.well-known/openid-configuration" {
			t.Fatalf("body = %v", gotBody)
		}
		if got["issuer"] != "https://idp.example.com" || got["jwksUrl"] != "https://idp.example.com/keys" {
			t.Fatalf("config = %v", got)
		}
	})
	t.Run("an unreadable document is Keycloak's 500", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"unknown_error"}`)
		}))
		t.Cleanup(server.Close)
		_, err := testClient(server).ImportIdentityProviderConfig(context.Background(), "https://idp.example.com/.well-known/openid-configuration")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("error = %v, want APIError 500", err)
		}
	})
}

func TestReloadIdentityProviderKeys(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		body string
		want bool
	}{{"true", true}, {"false", false}} {
		t.Run(tt.body, func(t *testing.T) {
			t.Parallel()
			var gotMethod, gotPath string
			server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)
			got, err := testClient(server).ReloadIdentityProviderKeys(context.Background(), "acme")
			if err != nil || got != tt.want {
				t.Fatalf("ReloadIdentityProviderKeys = %v, %v; want %v", got, err, tt.want)
			}
			if gotMethod != http.MethodGet || gotPath != "/admin/realms/ai-grid/identity-provider/instances/acme/reload-keys" {
				t.Fatalf("request = %s %s", gotMethod, gotPath)
			}
		})
	}
}

func TestCountIdentityProviderUsers(t *testing.T) {
	t.Parallel()
	var gotPath, gotAlias string
	server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAlias = r.URL.Path, r.URL.Query().Get("idpAlias")
		_, _ = io.WriteString(w, "3")
	}))
	t.Cleanup(server.Close)
	got, err := testClient(server).CountIdentityProviderUsers(context.Background(), "acme corp")
	if err != nil || got != 3 {
		t.Fatalf("CountIdentityProviderUsers = %d, %v; want 3", got, err)
	}
	if gotPath != "/admin/realms/ai-grid/users/count" || gotAlias != "acme corp" {
		t.Fatalf("request = %s idpAlias=%q", gotPath, gotAlias)
	}
}

func TestListIdentityProviderMappers(t *testing.T) {
	t.Parallel()
	var gotPath string
	server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode([]IdentityProviderMapper{{ID: "m-1", Name: IdentityProviderOrgIDMapperName}})
	}))
	t.Cleanup(server.Close)
	got, err := testClient(server).ListIdentityProviderMappers(context.Background(), "acme")
	if err != nil || len(got) != 1 || got[0].Name != IdentityProviderOrgIDMapperName {
		t.Fatalf("ListIdentityProviderMappers = %+v, %v", got, err)
	}
	if gotPath != "/admin/realms/ai-grid/identity-provider/instances/acme/mappers" {
		t.Fatalf("request path = %s", gotPath)
	}
}

func testClient(server *httptest.Server) *Client {
	return New(Config{
		BaseURL: server.URL, AdminRealm: "master", Realm: "ai-grid",
		Username: "admin", Password: "secret", HTTPClient: server.Client(),
	})
}

func tokenThen(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
			return
		}
		next(w, r)
	}
}
