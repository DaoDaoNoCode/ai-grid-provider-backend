package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// listServer answers the token request and hands every other request to
// page, recording its query. page returns the status and the JSON body.
// transport, when set, wraps the server's transport.
func listServer(t *testing.T, page func(query url.Values) (int, any), transport func(http.RoundTripper) http.RoundTripper) (*Client, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = io.WriteString(w, `{"access_token":"t"}`)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		status, body := page(r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	httpClient := server.Client()
	if transport != nil {
		httpClient = &http.Client{Transport: transport(httpClient.Transport)}
	}
	client := New(Config{
		BaseURL: server.URL, AdminRealm: "master", Realm: "ai-grid",
		Username: "admin", Password: "admin", HTTPClient: httpClient,
	})
	return client, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(queries)
	}
}

// keycloakPage pages items the way the Admin API does for first and max
// (PaginationUtils.paginateQuery): skip first, return at most max.
func keycloakPage[T any](items []T, query url.Values) []T {
	first, _ := strconv.Atoi(query.Get("first"))
	size, _ := strconv.Atoi(query.Get("max"))
	start := min(max(first, 0), len(items))
	end := min(start+max(size, 0), len(items))
	return items[start:end]
}

func numberedOrganizations(n int) []Organization {
	orgs := make([]Organization, 0, n)
	for i := range n {
		orgs = append(orgs, Organization{ID: fmt.Sprintf("org-%03d", i), Name: fmt.Sprintf("tenant-%03d", i)})
	}
	return orgs
}

func firsts(queries []url.Values) []string {
	out := make([]string, 0, len(queries))
	for _, q := range queries {
		out = append(out, q.Get("first"))
	}
	return out
}

// TestListOrganizationsReadsEveryPage: the list asks for pages of listPageSize
// until one comes back short, so a realm with more organizations than one
// page lists all of them, in Keycloak's order.
func TestListOrganizationsReadsEveryPage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		count      int
		wantFirsts []string
	}{
		{name: "none", count: 0, wantFirsts: []string{"0"}},
		{name: "one page exactly full, then an empty one", count: 100, wantFirsts: []string{"0", "100"}},
		{name: "250 across three pages", count: 250, wantFirsts: []string{"0", "100", "200"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			all := numberedOrganizations(tc.count)
			client, queries := listServer(t, func(query url.Values) (int, any) {
				return http.StatusOK, keycloakPage(all, query)
			}, nil)

			got, err := client.ListOrganizations(context.Background())
			if err != nil {
				t.Fatalf("ListOrganizations() error = %v", err)
			}
			if len(got) != tc.count {
				t.Fatalf("ListOrganizations() returned %d organizations, want %d", len(got), tc.count)
			}
			for i, org := range got {
				if org.ID != all[i].ID {
					t.Fatalf("organization %d = %q, want %q", i, org.ID, all[i].ID)
				}
			}
			if got := firsts(queries()); !slices.Equal(got, tc.wantFirsts) {
				t.Fatalf("requested first = %v, want %v", got, tc.wantFirsts)
			}
			for _, q := range queries() {
				if q.Get("max") != strconv.Itoa(listPageSize) || q.Get("briefRepresentation") != "false" {
					t.Fatalf("list query = %v, want max=%d and briefRepresentation=false", q, listPageSize)
				}
			}
		})
	}
}

// TestListOrganizationsServerIgnoringFirst: a server that answers every
// request with the same full page makes no progress, and the list fails
// after the second page instead of asking forever.
func TestListOrganizationsServerIgnoringFirst(t *testing.T) {
	t.Parallel()

	all := numberedOrganizations(250)
	client, queries := listServer(t, func(url.Values) (int, any) {
		return http.StatusOK, all[:listPageSize]
	}, nil)

	got, err := client.ListOrganizations(context.Background())
	if !errors.Is(err, errPagingStalled) {
		t.Fatalf("ListOrganizations() = %d organizations, %v; want errPagingStalled", len(got), err)
	}
	if got := firsts(queries()); !slices.Equal(got, []string{"0", "100"}) {
		t.Fatalf("requested first = %v, want [0 100]", got)
	}
}

// TestListOrganizationsLaterPageFails: an error on any page fails the whole
// list; a partial list would be read as complete.
func TestListOrganizationsLaterPageFails(t *testing.T) {
	t.Parallel()

	all := numberedOrganizations(250)
	client, queries := listServer(t, func(query url.Values) (int, any) {
		if query.Get("first") == "100" {
			return http.StatusServiceUnavailable, map[string]string{"error": "unavailable"}
		}
		return http.StatusOK, keycloakPage(all, query)
	}, nil)

	got, err := client.ListOrganizations(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ListOrganizations() = %d organizations, %v; want a 503 APIError", len(got), err)
	}
	if got != nil {
		t.Fatalf("ListOrganizations() returned %d organizations with its error", len(got))
	}
	if got := firsts(queries()); !slices.Equal(got, []string{"0", "100"}) {
		t.Fatalf("requested first = %v, want [0 100]", got)
	}
}

// TestListOrganizationsStopsWhenCanceled: a context canceled while a page is
// read stops the list before it asks for the next page.
func TestListOrganizationsStopsWhenCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	all := numberedOrganizations(250)
	client, queries := listServer(t, func(query url.Values) (int, any) {
		return http.StatusOK, keycloakPage(all, query)
	}, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := next.RoundTrip(r)
			if err != nil || !strings.HasSuffix(r.URL.Path, "/organizations") {
				return resp, err
			}
			// Hand back the page whole, then cancel: the page decodes,
			// and the next one must not be requested.
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(body))
			cancel()
			return resp, err
		})
	})

	_, err := client.ListOrganizations(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListOrganizations() error = %v, want context.Canceled", err)
	}
	if got := firsts(queries()); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("requested first = %v, want [0]", got)
	}
}

// TestListOrganizationMembersReadsEveryPage: members page the same way as
// organizations, so an organization with more members than one page lists
// all of them.
func TestListOrganizationMembersReadsEveryPage(t *testing.T) {
	t.Parallel()

	all := make([]OrganizationMember, 0, 250)
	for i := range 250 {
		all = append(all, OrganizationMember{ID: fmt.Sprintf("user-%03d", i), Username: fmt.Sprintf("user%03d", i)})
	}
	client, queries := listServer(t, func(query url.Values) (int, any) {
		return http.StatusOK, keycloakPage(all, query)
	}, nil)

	got, err := client.ListOrganizationMembers(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListOrganizationMembers() error = %v", err)
	}
	if len(got) != len(all) || got[0].ID != all[0].ID || got[249].ID != all[249].ID {
		t.Fatalf("ListOrganizationMembers() returned %d members, want %d", len(got), len(all))
	}
	if got := firsts(queries()); !slices.Equal(got, []string{"0", "100", "200"}) {
		t.Fatalf("requested first = %v, want [0 100 200]", got)
	}
}

// TestListOrganizationMembersServerIgnoringFirst: as for organizations, a
// server that repeats a full page fails the list instead of looping.
func TestListOrganizationMembersServerIgnoringFirst(t *testing.T) {
	t.Parallel()

	all := make([]OrganizationMember, 0, listPageSize)
	for i := range listPageSize {
		all = append(all, OrganizationMember{ID: fmt.Sprintf("user-%03d", i)})
	}
	client, _ := listServer(t, func(url.Values) (int, any) {
		return http.StatusOK, all
	}, nil)

	if _, err := client.ListOrganizationMembers(context.Background(), "org-1"); !errors.Is(err, errPagingStalled) {
		t.Fatalf("ListOrganizationMembers() error = %v, want errPagingStalled", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
