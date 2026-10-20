package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListOrganizations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantNames  []string
		wantDomain string
		wantErr    string
	}{
		{
			name: "happy path",
			handler: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
					if err := r.ParseForm(); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if r.Form.Get("username") != "admin" || r.Form.Get("password") != "secret" {
						http.Error(w, "bad credentials", http.StatusUnauthorized)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-token"})
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/organizations"):
					if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
						http.Error(w, "missing bearer", http.StatusUnauthorized)
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]any{
						{
							"id": "org-1", "name": "tenant-a",
							"domains": []map[string]string{{"name": "a.example.com"}},
						},
					})
				default:
					http.NotFound(w, r)
				}
			},
			wantNames:  []string{"tenant-a"},
			wantDomain: "a.example.com",
		},
		{
			name: "token failure",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/token") {
					http.Error(w, "invalid_grant", http.StatusUnauthorized)
					return
				}
				http.NotFound(w, r)
			},
			wantErr: "keycloak token request returned",
		},
		{
			name: "organizations failure",
			handler: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/token"):
					_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-token"})
				case strings.Contains(r.URL.Path, "/organizations"):
					http.Error(w, "forbidden", http.StatusForbidden)
				default:
					http.NotFound(w, r)
				}
			},
			wantErr: "keycloak organizations request returned",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(tc.handler)
			t.Cleanup(server.Close)

			client := New(Config{
				BaseURL:    server.URL,
				AdminRealm: "master",
				Realm:      "ai-grid",
				Username:   "admin",
				Password:   "secret",
				HTTPClient: server.Client(),
			})

			orgs, err := client.ListOrganizations(context.Background())
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("ListOrganizations() error = nil, want error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ListOrganizations() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListOrganizations() error = %v", err)
			}
			if len(orgs) != len(tc.wantNames) {
				t.Fatalf("got %d orgs, want %d", len(orgs), len(tc.wantNames))
			}
			for i, name := range tc.wantNames {
				if orgs[i].Name != name {
					t.Fatalf("orgs[%d].Name = %q, want %q", i, orgs[i].Name, name)
				}
			}
			if tc.wantDomain != "" && (len(orgs[0].DomainNames()) != 1 || orgs[0].DomainNames()[0] != tc.wantDomain) {
				t.Fatalf("DomainNames() = %v, want [%q]", orgs[0].DomainNames(), tc.wantDomain)
			}
		})
	}
}

func TestOrganizationDomainNames(t *testing.T) {
	t.Parallel()

	org := Organization{
		Domains: []OrganizationDomain{{Name: "a.example.com"}, {Name: "b.example.com"}},
	}
	got := org.DomainNames()
	want := []string{"a.example.com", "b.example.com"}
	if len(got) != len(want) {
		t.Fatalf("DomainNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DomainNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestListOrganizationsClosesBodies(t *testing.T) {
	t.Parallel()

	// Ensures the client does not leak response bodies on the happy path.
	var bodies int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies++
		if strings.Contains(r.URL.Path, "/token") {
			_, _ = io.WriteString(w, `{"access_token":"t"}`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(server.Close)

	client := New(Config{
		BaseURL: server.URL, AdminRealm: "master", Realm: "ai-grid",
		Username: "admin", Password: "admin", HTTPClient: server.Client(),
	})
	if _, err := client.ListOrganizations(context.Background()); err != nil {
		t.Fatalf("ListOrganizations() error = %v", err)
	}
	if bodies != 2 {
		t.Fatalf("expected 2 HTTP round-trips, got %d", bodies)
	}
}
