package aigrid

import (
	"context"
	"errors"
	"strings"
	"testing"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// API unit tests call Service methods directly with fakes.
// Kind integration tests cover the gRPC wire path (integration/kind_test.go).
// When Create/Get/Update/Delete RPCs are added, extend the table cases below
// using the same pattern: fake deps → call RPC → assert response/error.

func TestPing(t *testing.T) {
	t.Parallel()

	service := Service{}
	tests := []struct {
		name    string
		request *aigridv1.PingRequest
		want    string
	}{
		{name: "empty name defaults to world", request: &aigridv1.PingRequest{}, want: "Welcome, world!"},
		{name: "named greeting", request: &aigridv1.PingRequest{Name: "Ada"}, want: "Welcome, Ada!"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := service.Ping(context.Background(), tc.request)
			if err != nil {
				t.Fatalf("Ping() error = %v", err)
			}
			if got.GetMessage() != tc.want {
				t.Fatalf("Ping() message = %q, want %q", got.GetMessage(), tc.want)
			}
		})
	}
}

func TestListManagedClusters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		objects   []runtime.Object
		reactor   func(*fake.FakeDynamicClient)
		wantNames []string
		wantEnv   map[string]string
		wantHub   map[string]bool
		wantErr   bool
	}{
		{
			name: "maps cluster metadata",
			objects: []runtime.Object{
				managedCluster("cluster-a", map[string]string{"environment": "production"}, true),
				managedCluster("cluster-b", map[string]string{"environment": "development"}, false),
			},
			wantNames: []string{"cluster-a", "cluster-b"},
			wantEnv: map[string]string{
				"cluster-a": "production",
				"cluster-b": "development",
			},
			wantHub: map[string]bool{
				"cluster-a": true,
				"cluster-b": false,
			},
		},
		{
			name:      "empty list",
			objects:   nil,
			wantNames: []string{},
		},
		{
			name: "not found CRD returns empty list",
			reactor: func(client *fake.FakeDynamicClient) {
				client.PrependReactor("list", "managedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{
						Group: "cluster.open-cluster-management.io", Resource: "managedclusters",
					}, "")
				})
			},
			wantNames: []string{},
		},
		{
			name: "list error is returned",
			reactor: func(client *fake.FakeDynamicClient) {
				client.PrependReactor("list", "managedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("api unavailable")
				})
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			objects := tc.objects
			if objects == nil {
				objects = []runtime.Object{}
			}
			client := newManagedClusterClient(objects...)
			if tc.reactor != nil {
				tc.reactor(client)
			}
			service := Service{client: client}

			response, err := service.ListManagedClusters(context.Background(), &aigridv1.ListManagedClustersRequest{})
			if tc.wantErr {
				if err == nil {
					t.Fatal("ListManagedClusters() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ListManagedClusters() error = %v", err)
			}

			clusters := response.GetClusters()
			if len(clusters) != len(tc.wantNames) {
				t.Fatalf("got %d clusters, want %d", len(clusters), len(tc.wantNames))
			}
			byName := make(map[string]*aigridv1.ManagedCluster, len(clusters))
			for _, cluster := range clusters {
				byName[cluster.GetName()] = cluster
			}
			for _, name := range tc.wantNames {
				cluster, ok := byName[name]
				if !ok {
					t.Fatalf("missing cluster %q; got %v", name, byName)
				}
				if env, ok := tc.wantEnv[name]; ok && cluster.GetLabels()["environment"] != env {
					t.Fatalf("cluster %q environment = %q, want %q", name, cluster.GetLabels()["environment"], env)
				}
				if hub, ok := tc.wantHub[name]; ok && cluster.GetHubAcceptsClient() != hub {
					t.Fatalf("cluster %q hubAcceptsClient = %v, want %v", name, cluster.GetHubAcceptsClient(), hub)
				}
			}
		})
	}
}

func TestListTenants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lister    organizationLister
		wantIDs   map[string]string
		wantNames []string
		wantDom   map[string][]string
		wantErr   string
	}{
		{
			name: "maps organizations to tenants",
			lister: fakeOrganizationLister{
				orgs: []keycloak.Organization{
					{ID: "org-1", Name: "tenant-a", Domains: []keycloak.OrganizationDomain{{Name: "a.example.com"}}},
					{ID: "org-2", Name: "tenant-b", Domains: []keycloak.OrganizationDomain{
						{Name: "b.example.com"}, {Name: "b.alt.example.com"},
					}},
				},
			},
			wantIDs: map[string]string{
				"tenant-a": "org-1",
				"tenant-b": "org-2",
			},
			wantNames: []string{"tenant-a", "tenant-b"},
			wantDom: map[string][]string{
				"tenant-a": {"a.example.com"},
				"tenant-b": {"b.example.com", "b.alt.example.com"},
			},
		},
		{
			name:      "empty organizations",
			lister:    fakeOrganizationLister{},
			wantNames: []string{},
		},
		{
			name:    "nil client",
			lister:  nil,
			wantErr: "Keycloak tenant client is not configured",
		},
		{
			name:    "upstream error",
			lister:  fakeOrganizationLister{err: errors.New("token rejected")},
			wantErr: "token rejected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			service := Service{tenants: tc.lister}
			response, err := service.ListTenants(context.Background(), &aigridv1.ListTenantsRequest{})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("ListTenants() error = nil, want error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ListTenants() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListTenants() error = %v", err)
			}

			tenants := response.GetTenants()
			if len(tenants) != len(tc.wantNames) {
				t.Fatalf("got %d tenants, want %d", len(tenants), len(tc.wantNames))
			}
			byName := make(map[string]*aigridv1.Tenant, len(tenants))
			for _, tenant := range tenants {
				byName[tenant.GetName()] = tenant
			}
			for _, name := range tc.wantNames {
				tenant, ok := byName[name]
				if !ok {
					t.Fatalf("missing tenant %q; got %v", name, byName)
				}
				if id, ok := tc.wantIDs[name]; ok && tenant.GetId() != id {
					t.Fatalf("tenant %q id = %q, want %q", name, tenant.GetId(), id)
				}
				if want, ok := tc.wantDom[name]; ok {
					assertStringSlicesEqual(t, tenant.GetDomains(), want)
				}
			}
		})
	}
}

type fakeOrganizationLister struct {
	orgs []keycloak.Organization
	err  error
}

func (f fakeOrganizationLister) ListOrganizations(context.Context) ([]keycloak.Organization, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.orgs, nil
}

func newManagedClusterClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			managedClustersResource: "ManagedClusterList",
		},
		objects...,
	)
}

func managedCluster(name string, labels map[string]string, accepts bool) *unstructured.Unstructured {
	jsonLabels := make(map[string]interface{}, len(labels))
	for key, value := range labels {
		jsonLabels[key] = value
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cluster.open-cluster-management.io/v1",
		"kind":       "ManagedCluster",
		"metadata": map[string]interface{}{
			"name":   name,
			"labels": jsonLabels,
		},
		"spec": map[string]interface{}{
			"hubAcceptsClient": accepts,
		},
	}}
}

func assertStringSlicesEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("domains length = %d, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("domains[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
