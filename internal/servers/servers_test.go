package servers

import (
	"context"
	"errors"
	"strings"
	"testing"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestPing(t *testing.T) {
	t.Parallel()

	server, err := NewAiGridServer().Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	tests := []struct {
		name    string
		request *aigridv1.AiGridPingRequest
		want    string
	}{
		{name: "empty name defaults to world", request: &aigridv1.AiGridPingRequest{}, want: "Welcome, world!"},
		{name: "named greeting", request: &aigridv1.AiGridPingRequest{Name: "Ada"}, want: "Welcome, Ada!"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := server.Ping(context.Background(), tc.request)
			if err != nil {
				t.Fatalf("Ping() error = %v", err)
			}
			if got.GetMessage() != tc.want {
				t.Fatalf("Ping() message = %q, want %q", got.GetMessage(), tc.want)
			}
		})
	}
}

func TestListClusters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		objects   []runtime.Object
		reactor   func(*fake.FakeDynamicClient)
		wantNames []string
		wantEnv   map[string]string
		wantHub   map[string]bool
		offset    *int32
		limit     *int32
		wantSize  int32
		wantTotal int32
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
			wantSize:  2,
			wantTotal: 2,
		},
		{
			name:      "empty list",
			objects:   nil,
			wantNames: []string{},
			wantSize:  0,
			wantTotal: 0,
		},
		{
			name: "pagination",
			objects: []runtime.Object{
				managedCluster("cluster-a", nil, true),
				managedCluster("cluster-b", nil, true),
				managedCluster("cluster-c", nil, true),
			},
			offset:    int32Ptr(1),
			limit:     int32Ptr(1),
			wantNames: nil, // order from the fake client is not guaranteed; assert size/total only
			wantSize:  1,
			wantTotal: 3,
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
			wantSize:  0,
			wantTotal: 0,
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
			server := mustClustersServer(t, client)

			response, err := server.List(context.Background(), &aigridv1.ClustersListRequest{
				Offset: tc.offset,
				Limit:  tc.limit,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("List() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if response.GetSize() != tc.wantSize || response.GetTotal() != tc.wantTotal {
				t.Fatalf("size/total = %d/%d, want %d/%d", response.GetSize(), response.GetTotal(), tc.wantSize, tc.wantTotal)
			}

			clusters := response.GetItems()
			if tc.wantNames != nil {
				if len(clusters) != len(tc.wantNames) {
					t.Fatalf("got %d clusters, want %d", len(clusters), len(tc.wantNames))
				}
				byName := make(map[string]*aigridv1.ManagedCluster, len(clusters))
				for _, cluster := range clusters {
					byName[cluster.GetId()] = cluster
				}
				for _, name := range tc.wantNames {
					cluster, ok := byName[name]
					if !ok {
						t.Fatalf("missing cluster %q; got %v", name, byName)
					}
					if cluster.GetMetadata().GetName() != name {
						t.Fatalf("metadata.name = %q, want %q", cluster.GetMetadata().GetName(), name)
					}
					if env, ok := tc.wantEnv[name]; ok && cluster.GetMetadata().GetLabels()["environment"] != env {
						t.Fatalf("cluster %q environment = %q, want %q", name, cluster.GetMetadata().GetLabels()["environment"], env)
					}
					if hub, ok := tc.wantHub[name]; ok && cluster.GetSpec().GetHubAcceptsClient() != hub {
						t.Fatalf("cluster %q hubAcceptsClient = %v, want %v", name, cluster.GetSpec().GetHubAcceptsClient(), hub)
					}
				}
			} else if int32(len(clusters)) != tc.wantSize {
				t.Fatalf("got %d clusters, want size %d", len(clusters), tc.wantSize)
			}
		})
	}
}

func TestGetCluster(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		objects []runtime.Object
		reqID   string
		wantEnv string
		wantHub bool
		wantErr codes.Code
	}{
		{
			name:    "found",
			objects: []runtime.Object{managedCluster("cluster-a", map[string]string{"environment": "production"}, true)},
			reqID:   "cluster-a",
			wantEnv: "production",
			wantHub: true,
		},
		{
			name:    "not found",
			objects: []runtime.Object{},
			reqID:   "missing",
			wantErr: codes.NotFound,
		},
		{
			name:    "invalid id",
			objects: []runtime.Object{},
			reqID:   "INVALID_NAME",
			wantErr: codes.InvalidArgument,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := mustClustersServer(t, newManagedClusterClient(tc.objects...))
			got, err := server.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: tc.reqID})
			if tc.wantErr != codes.OK {
				assertGRPCCode(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.GetObject().GetId() != tc.reqID {
				t.Fatalf("id = %q, want %q", got.GetObject().GetId(), tc.reqID)
			}
			if got.GetObject().GetMetadata().GetLabels()["environment"] != tc.wantEnv {
				t.Fatalf("environment = %q, want %q", got.GetObject().GetMetadata().GetLabels()["environment"], tc.wantEnv)
			}
			if got.GetObject().GetSpec().GetHubAcceptsClient() != tc.wantHub {
				t.Fatalf("hubAcceptsClient = %v, want %v", got.GetObject().GetSpec().GetHubAcceptsClient(), tc.wantHub)
			}
		})
	}
}

func TestCreateCluster(t *testing.T) {
	t.Parallel()

	t.Run("creates cluster", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient())
		got, err := server.Create(context.Background(), &aigridv1.ClustersCreateRequest{
			Object: &aigridv1.ManagedCluster{
				Id: "new-cluster",
				Metadata: &aigridv1.Metadata{
					Name:   "new-cluster",
					Labels: map[string]string{"environment": "dev"},
				},
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
			},
		})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got.GetObject().GetId() != "new-cluster" {
			t.Fatalf("id = %q", got.GetObject().GetId())
		}
		if !got.GetObject().GetSpec().GetHubAcceptsClient() {
			t.Fatal("hubAcceptsClient = false, want true")
		}
		if got.GetObject().GetMetadata().GetLabels()["environment"] != "dev" {
			t.Fatalf("labels = %v", got.GetObject().GetMetadata().GetLabels())
		}
	})

	t.Run("already exists", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(managedCluster("dup", nil, true)))
		_, err := server.Create(context.Background(), &aigridv1.ClustersCreateRequest{
			Object: &aigridv1.ManagedCluster{
				Id:   "dup",
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
			},
		})
		assertGRPCCode(t, err, codes.AlreadyExists)
	})

	t.Run("nil object", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient())
		_, err := server.Create(context.Background(), &aigridv1.ClustersCreateRequest{})
		assertGRPCCode(t, err, codes.InvalidArgument)
	})
}

func TestUpdateCluster(t *testing.T) {
	t.Parallel()

	t.Run("merges labels and sets hubAcceptsClient", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(
			managedCluster("cluster-a", map[string]string{"environment": "production", "keep": "yes"}, true),
		))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{
				Id: "cluster-a",
				Metadata: &aigridv1.Metadata{
					Labels: map[string]string{"environment": "staging", "ai-grid/enabled": "true"},
				},
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: false},
			},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		labels := got.GetObject().GetMetadata().GetLabels()
		if labels["environment"] != "staging" {
			t.Fatalf("environment = %q, want staging", labels["environment"])
		}
		if labels["keep"] != "yes" {
			t.Fatalf("keep label missing: %v", labels)
		}
		if labels["ai-grid/enabled"] != "true" {
			t.Fatalf("ai-grid/enabled = %q", labels["ai-grid/enabled"])
		}
		if got.GetObject().GetSpec().GetHubAcceptsClient() {
			t.Fatal("hubAcceptsClient = true, want false")
		}
	})

	t.Run("field mask labels only", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(
			managedCluster("cluster-a", map[string]string{"environment": "production"}, true),
		))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{
				Id: "cluster-a",
				Metadata: &aigridv1.Metadata{
					Labels: map[string]string{"environment": "staging"},
				},
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: false},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetLabels()["environment"] != "staging" {
			t.Fatalf("labels = %v", got.GetObject().GetMetadata().GetLabels())
		}
		if !got.GetObject().GetSpec().GetHubAcceptsClient() {
			t.Fatal("hubAcceptsClient changed despite labels-only mask")
		}
	})

	t.Run("empty labels leaves existing labels", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(
			managedCluster("cluster-a", map[string]string{"environment": "production"}, true),
		))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{
				Id:   "cluster-a",
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
			},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetLabels()["environment"] != "production" {
			t.Fatalf("labels = %v", got.GetObject().GetMetadata().GetLabels())
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient())
		_, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{
				Id:   "missing",
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
			},
		})
		assertGRPCCode(t, err, codes.NotFound)
	})
}

func TestDeleteCluster(t *testing.T) {
	t.Parallel()

	t.Run("deletes existing", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(managedCluster("cluster-a", nil, true)))
		_, err := server.Delete(context.Background(), &aigridv1.ClustersDeleteRequest{Id: "cluster-a"})
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		_, err = server.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: "cluster-a"})
		assertGRPCCode(t, err, codes.NotFound)
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient())
		_, err := server.Delete(context.Background(), &aigridv1.ClustersDeleteRequest{Id: "missing"})
		assertGRPCCode(t, err, codes.NotFound)
	})
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
			name:    "upstream error",
			lister:  fakeOrganizationLister{err: errors.New("token rejected")},
			wantErr: "token rejected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, err := NewTenantsServer().SetOrganizationLister(tc.lister).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			response, err := server.List(context.Background(), &aigridv1.TenantsListRequest{})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("List() error = nil, want error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("List() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}

			tenants := response.GetItems()
			if len(tenants) != len(tc.wantNames) {
				t.Fatalf("got %d tenants, want %d", len(tenants), len(tc.wantNames))
			}
			if int(response.GetSize()) != len(tenants) || int(response.GetTotal()) != len(tc.wantNames) {
				t.Fatalf("size/total = %d/%d, want %d/%d", response.GetSize(), response.GetTotal(), len(tenants), len(tc.wantNames))
			}
			byName := make(map[string]*aigridv1.Tenant, len(tenants))
			for _, tenant := range tenants {
				byName[tenant.GetMetadata().GetName()] = tenant
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
					assertStringSlicesEqual(t, tenant.GetSpec().GetDomains(), want)
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

func mustClustersServer(t *testing.T, client *fake.FakeDynamicClient) *ClustersServer {
	t.Helper()
	server, err := NewClustersServer().SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return server
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

func assertGRPCCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want gRPC code %v", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error = %v, want gRPC status %v", err, want)
	}
	if st.Code() != want {
		t.Fatalf("gRPC code = %v, want %v (%v)", st.Code(), want, err)
	}
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

func int32Ptr(v int32) *int32 { return &v }
