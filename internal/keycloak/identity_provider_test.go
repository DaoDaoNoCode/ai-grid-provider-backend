package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	t.Run("create all four when missing", func(t *testing.T) {
		t.Parallel()
		created := map[string]IdentityProviderMapper{}
		server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mappers"):
				_ = json.NewEncoder(w).Encode([]IdentityProviderMapper{})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mappers"):
				var m IdentityProviderMapper
				_ = json.NewDecoder(r.Body).Decode(&m)
				created[m.Name] = m
				w.WriteHeader(http.StatusCreated)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		if err := testClient(server).UpsertIdentityProviderMapper(context.Background(), "acme", "uuid-1", "Acme"); err != nil {
			t.Fatalf("Upsert: %v", err)
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
		if len(created) != 4 {
			t.Fatalf("created %d mappers, want 4", len(created))
		}
	})
	t.Run("update existing, create the rest", func(t *testing.T) {
		t.Parallel()
		var updatePath string
		var updateBody IdentityProviderMapper
		posts := 0
		server := httptest.NewServer(tokenThen(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mappers"):
				_ = json.NewEncoder(w).Encode([]IdentityProviderMapper{{
					ID: "mapper-1", Name: "AI Grid org id", IdentityProviderAlias: "acme",
				}})
			case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/mappers/mapper-1"):
				updatePath = r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&updateBody)
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mappers"):
				posts++
				w.WriteHeader(http.StatusCreated)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		if err := testClient(server).UpsertIdentityProviderMapper(context.Background(), "acme", "uuid-2", "Acme"); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if !strings.HasSuffix(updatePath, "/mappers/mapper-1") || updateBody.Config["attribute.value"] != "uuid-2" || posts != 3 {
			t.Fatalf("path=%s body=%+v posts=%d", updatePath, updateBody, posts)
		}
	})
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
