package keycloak

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
			wantErr: "Keycloak list organizations returned",
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

func TestOrganizationCRUD(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	orgs := map[string]Organization{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-token"})
			return
		}
		if !strings.Contains(r.URL.Path, "/organizations") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			if strings.HasSuffix(r.URL.Path, "/organizations") {
				items := make([]Organization, 0, len(orgs))
				for _, org := range orgs {
					items = append(items, org)
				}
				_ = json.NewEncoder(w).Encode(items)
				return
			}
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			org, ok := orgs[id]
			if !ok {
				http.Error(w, "missing", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(org)
		case http.MethodPost:
			var org Organization
			if err := json.NewDecoder(r.Body).Decode(&org); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for _, existing := range orgs {
				if existing.Name == org.Name {
					http.Error(w, "conflict", http.StatusConflict)
					return
				}
			}
			org.ID = "org-created"
			orgs[org.ID] = org
			w.Header().Set("Location", r.URL.Path+"/"+org.ID)
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if _, ok := orgs[id]; !ok {
				http.Error(w, "missing", http.StatusNotFound)
				return
			}
			var org Organization
			if err := json.NewDecoder(r.Body).Decode(&org); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			org.ID = id
			orgs[id] = org
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if _, ok := orgs[id]; !ok {
				http.Error(w, "missing", http.StatusNotFound)
				return
			}
			delete(orgs, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)

	client := New(Config{
		BaseURL: server.URL, AdminRealm: "master", Realm: "ai-grid",
		Username: "admin", Password: "admin", HTTPClient: server.Client(),
	})
	ctx := context.Background()

	created, err := client.CreateOrganization(ctx, Organization{
		Name: "acme", Domains: []OrganizationDomain{{Name: "acme.example.com"}},
	})
	if err != nil {
		t.Fatalf("CreateOrganization() error = %v", err)
	}
	if created.ID != "org-created" || created.Name != "acme" || created.Alias != "acme" {
		t.Fatalf("created = %+v", created)
	}
	if len(created.DomainNames()) != 1 || created.DomainNames()[0] != "acme.example.com" {
		t.Fatalf("domains = %v", created.DomainNames())
	}

	got, err := client.GetOrganization(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetOrganization() error = %v", err)
	}
	if got.Name != "acme" {
		t.Fatalf("GetOrganization name = %q", got.Name)
	}

	enabled := false
	created.Enabled = &enabled
	created.Description = "keep-me"
	created.RedirectURL = "https://acme.example.com/login"
	created.Name = "acme-renamed"
	created.Domains = []OrganizationDomain{{Name: "renamed.example.com"}}
	// Alias stays at the create-time value (immutable in Keycloak).
	updated, err := client.UpdateOrganization(ctx, created)
	if err != nil {
		t.Fatalf("UpdateOrganization() error = %v", err)
	}
	if updated.Name != "acme-renamed" || updated.Alias != "acme" {
		t.Fatalf("updated name/alias = %+v", updated)
	}
	if updated.Enabled == nil || *updated.Enabled || updated.Description != "keep-me" || updated.RedirectURL != "https://acme.example.com/login" {
		t.Fatalf("updated lost Keycloak fields: %+v", updated)
	}
	if len(updated.DomainNames()) != 1 || updated.DomainNames()[0] != "renamed.example.com" {
		t.Fatalf("updated domains = %v", updated.DomainNames())
	}

	if err := client.DeleteOrganization(ctx, created.ID); err != nil {
		t.Fatalf("DeleteOrganization() error = %v", err)
	}
	_, err = client.GetOrganization(ctx, created.ID)
	if !IsNotFound(err) {
		t.Fatalf("Get after delete error = %v, want NotFound", err)
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

func TestListOrganizationMembers(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-token"})
			return
		}
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/members") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]OrganizationMember{
			{ID: "u1", Username: "alice", Email: "alice@example.com", Enabled: true},
			{ID: "u2", Username: "bob", FirstName: "Bob", LastName: "Builder"},
		})
	}))
	t.Cleanup(server.Close)

	client := New(Config{
		BaseURL: server.URL, AdminRealm: "master", Realm: "ai-grid",
		Username: "admin", Password: "admin", HTTPClient: server.Client(),
	})
	got, err := client.ListOrganizationMembers(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListOrganizationMembers() error = %v", err)
	}
	if len(got) != 2 || got[0].Username != "alice" || got[1].Username != "bob" {
		t.Fatalf("ListOrganizationMembers() = %+v", got)
	}
}

func TestUpdateOrganizationPreservesAttributes(t *testing.T) {
	t.Parallel()

	stored := Organization{
		ID:    "org-1",
		Name:  "acme",
		Alias: "acme",
		Attributes: map[string][]string{
			"billing-id": {"keep-me"},
		},
		Domains: []OrganizationDomain{{Name: "acme.example.com"}},
	}
	var putBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-token"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(stored)
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			putBody = body
			var org Organization
			if err := json.Unmarshal(body, &org); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			stored = org
			stored.ID = "org-1"
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)

	client := New(Config{
		BaseURL: server.URL, AdminRealm: "master", Realm: "ai-grid",
		Username: "admin", Password: "admin", HTTPClient: server.Client(),
	})
	ctx := context.Background()

	current, err := client.GetOrganization(ctx, "org-1")
	if err != nil {
		t.Fatalf("GetOrganization() error = %v", err)
	}
	current.Name = "acme-renamed"
	current.Domains = []OrganizationDomain{{Name: "renamed.example.com"}}
	if _, err := client.UpdateOrganization(ctx, current); err != nil {
		t.Fatalf("UpdateOrganization() error = %v", err)
	}
	if len(putBody) == 0 {
		t.Fatal("expected PUT body")
	}
	var sent Organization
	if err := json.Unmarshal(putBody, &sent); err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	if got := sent.Attributes["billing-id"]; len(got) != 1 || got[0] != "keep-me" {
		t.Fatalf("PUT omitted attributes: %+v (raw=%s)", sent.Attributes, putBody)
	}
}

func TestListOrganizationsLargeBody(t *testing.T) {
	t.Parallel()

	// Responses larger than the old 4 KiB LimitReader must still decode.
	orgs := make([]Organization, 0, 80)
	for i := 0; i < 80; i++ {
		orgs = append(orgs, Organization{
			ID:    fmt.Sprintf("org-%03d-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", i),
			Name:  fmt.Sprintf("tenant-%03d-with-a-longer-name-for-payload", i),
			Alias: fmt.Sprintf("tenant-%03d-with-a-longer-name-for-payload", i),
			Domains: []OrganizationDomain{
				{Name: fmt.Sprintf("tenant-%03d.example.com", i)},
			},
		})
	}
	payload, err := json.Marshal(orgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(payload) <= 4096 {
		t.Fatalf("test payload too small (%d bytes); need >4096", len(payload))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t"})
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	client := New(Config{
		BaseURL: server.URL, AdminRealm: "master", Realm: "ai-grid",
		Username: "admin", Password: "admin", HTTPClient: server.Client(),
	})
	got, err := client.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("ListOrganizations() error = %v", err)
	}
	if len(got) != 80 {
		t.Fatalf("len = %d, want 80", len(got))
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

func TestOrganizationIDFromLocation(t *testing.T) {
	t.Parallel()
	if got := organizationIDFromLocation("http://kc/admin/realms/ai-grid/organizations/abc-123"); got != "abc-123" {
		t.Fatalf("got %q", got)
	}
	if got := organizationIDFromLocation(""); got != "" {
		t.Fatalf("empty got %q", got)
	}
}
