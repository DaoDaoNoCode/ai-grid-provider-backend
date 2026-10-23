package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
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
		wantErr connect.Code
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
			wantErr: connect.CodeNotFound,
		},
		{
			name:    "invalid id",
			objects: []runtime.Object{},
			reqID:   "INVALID_NAME",
			wantErr: connect.CodeInvalidArgument,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := mustClustersServer(t, newManagedClusterClient(tc.objects...))
			got, err := server.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: tc.reqID})
			if tc.wantErr != 0 {
				assertConnectCode(t, err, tc.wantErr)
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

// TestClusterProvisioningUnreadablePolicy: a ready Policy that is missing or
// forbidden leaves provisioning empty instead of failing the request.
func TestClusterProvisioningUnreadablePolicy(t *testing.T) {
	t.Parallel()
	for name, policyErr := range map[string]error{
		"not found": apierrors.NewNotFound(policiesResource.GroupResource(), "ai-grid-ready"),
		"forbidden": apierrors.NewForbidden(policiesResource.GroupResource(), "ai-grid-ready", errors.New("no access")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := newManagedClusterClient(managedCluster("cluster-a", map[string]string{"ai-grid/enabled": "true"}, true))
			client.PrependReactor("get", "policies", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, policyErr
			})
			server := mustClustersServer(t, client)
			got, err := server.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: "cluster-a"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if p := got.GetObject().GetStatus().GetProvisioning(); p != "" {
				t.Fatalf("Get() provisioning = %q, want empty", p)
			}
			list, err := server.List(context.Background(), &aigridv1.ClustersListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if p := list.GetItems()[0].GetStatus().GetProvisioning(); p != "" {
				t.Fatalf("List() provisioning = %q, want empty", p)
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
		assertConnectCode(t, err, connect.CodeAlreadyExists)
	})

	t.Run("nil object", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient())
		_, err := server.Create(context.Background(), &aigridv1.ClustersCreateRequest{})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})
}

func TestUpdateCluster(t *testing.T) {
	t.Parallel()

	t.Run("merges labels and sets hubAcceptsClient", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(
			managedCluster("cluster-a", map[string]string{"location": "us-east", "keep": "yes"}, true),
		))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{
				Id: "cluster-a",
				Metadata: &aigridv1.Metadata{
					Labels: map[string]string{"location": "eu-west", "ai-grid/enabled": "true"},
				},
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: false},
			},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		labels := got.GetObject().GetMetadata().GetLabels()
		if labels["location"] != "eu-west" {
			t.Fatalf("location = %q, want eu-west", labels["location"])
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
			managedCluster("cluster-a", map[string]string{"location": "us-east"}, true),
		))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{
				Id: "cluster-a",
				Metadata: &aigridv1.Metadata{
					Labels: map[string]string{"location": "eu-west"},
				},
				Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: false},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetLabels()["location"] != "eu-west" {
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

	t.Run("labels without spec or mask keep hubAcceptsClient", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(
			managedCluster("cluster-a", map[string]string{"environment": "production"}, true),
		))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{
				Id:       "cluster-a",
				Metadata: &aigridv1.Metadata{Labels: map[string]string{"ai-grid/enabled": "true"}},
			},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetLabels()["ai-grid/enabled"] != "true" {
			t.Fatalf("labels = %v", got.GetObject().GetMetadata().GetLabels())
		}
		if !got.GetObject().GetSpec().GetHubAcceptsClient() {
			t.Fatal("hubAcceptsClient = false, want true: a request without spec must not change it")
		}
	})

	t.Run("refuses changes to keys the console doesn't own", func(t *testing.T) {
		t.Parallel()
		cluster := managedCluster("cluster-a", map[string]string{"name": "cluster-a", "vendor": "OpenShift"}, true)
		cluster.SetAnnotations(map[string]string{"open-cluster-management/created-via": "other"})
		server := mustClustersServer(t, newManagedClusterClient(cluster))
		labels := &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}}
		annotations := &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}}
		for _, tc := range []struct {
			name string
			meta *aigridv1.Metadata
			mask *fieldmaskpb.FieldMask
		}{
			{"changed label", &aigridv1.Metadata{Labels: map[string]string{"name": "cluster-a", "vendor": "Other"}}, labels},
			{"new label", &aigridv1.Metadata{Labels: map[string]string{"cluster.open-cluster-management.io/clusterset": "other"}}, labels},
			{"changed annotation", &aigridv1.Metadata{Annotations: map[string]string{"open-cluster-management/created-via": "console"}}, annotations},
			{"location annotation", &aigridv1.Metadata{Annotations: map[string]string{"location": "us-east"}}, annotations},
		} {
			_, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
				Object:     &aigridv1.ManagedCluster{Id: "cluster-a", Metadata: tc.meta},
				UpdateMask: tc.mask,
			})
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("%s: error = %v, want InvalidArgument", tc.name, err)
			}
		}
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{Id: "cluster-a", Metadata: &aigridv1.Metadata{
				Labels: map[string]string{"name": "cluster-a", "vendor": "OpenShift", "ai-grid/enabled": "true"},
			}},
			UpdateMask: labels,
		})
		if err != nil {
			t.Fatalf("unchanged hub labels sent back: error = %v", err)
		}
		if got.GetObject().GetMetadata().GetLabels()["ai-grid/enabled"] != "true" {
			t.Fatalf("labels = %v", got.GetObject().GetMetadata().GetLabels())
		}
	})

	t.Run("checks coordinate annotations", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(managedCluster("cluster-a", nil, true)))
		for _, tc := range []struct {
			annotations map[string]string
			ok          bool
		}{
			{map[string]string{"ai-grid/latitude": "91"}, false},
			{map[string]string{"ai-grid/latitude": "north"}, false},
			{map[string]string{"ai-grid/latitude": "NaN"}, false},
			{map[string]string{"ai-grid/longitude": "-180.5"}, false},
			{map[string]string{"ai-grid/latitude": " 40.4 ", "ai-grid/longitude": "-180"}, true},
			{map[string]string{"ai-grid/latitude": ""}, true},
		} {
			_, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
				Object:     &aigridv1.ManagedCluster{Id: "cluster-a", Metadata: &aigridv1.Metadata{Annotations: tc.annotations}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}},
			})
			if tc.ok && err != nil {
				t.Fatalf("%v: error = %v, want nil", tc.annotations, err)
			}
			if !tc.ok && connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("%v: error = %v, want InvalidArgument", tc.annotations, err)
			}
		}
	})

	t.Run("annotations are the whole console set", func(t *testing.T) {
		t.Parallel()
		cluster := managedCluster("cluster-a", nil, true)
		cluster.SetAnnotations(map[string]string{
			"ai-grid/latitude":                    "1",
			"ai-grid/site-name":                   "Site A",
			"open-cluster-management/created-via": "other",
		})
		server := mustClustersServer(t, newManagedClusterClient(cluster))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object:     &aigridv1.ManagedCluster{Id: "cluster-a", Metadata: &aigridv1.Metadata{Annotations: map[string]string{"ai-grid/latitude": "2"}}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		want := map[string]string{"ai-grid/latitude": "2", "open-cluster-management/created-via": "other"}
		if got := got.GetObject().GetMetadata().GetAnnotations(); !maps.Equal(got, want) {
			t.Fatalf("annotations = %v, want %v", got, want)
		}
	})

	t.Run("an empty label set clears the last console label", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient(
			managedCluster("cluster-a", map[string]string{"ai-grid/enabled": "true", "name": "cluster-a"}, true),
		))
		got, err := server.Update(context.Background(), &aigridv1.ClustersUpdateRequest{
			Object:     &aigridv1.ManagedCluster{Id: "cluster-a", Metadata: &aigridv1.Metadata{}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got, want := got.GetObject().GetMetadata().GetLabels(), map[string]string{"name": "cluster-a"}; !maps.Equal(got, want) {
			t.Fatalf("labels = %v, want %v", got, want)
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
		assertConnectCode(t, err, connect.CodeNotFound)
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
		assertConnectCode(t, err, connect.CodeNotFound)
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := mustClustersServer(t, newManagedClusterClient())
		_, err := server.Delete(context.Background(), &aigridv1.ClustersDeleteRequest{Id: "missing"})
		assertConnectCode(t, err, connect.CodeNotFound)
	})
}

func TestListTenants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		client    organizationClient
		wantIDs   map[string]string
		wantNames []string
		wantDom   map[string][]string
		wantErr   string
	}{
		{
			name: "maps organizations to tenants",
			client: newFakeOrganizationStore(
				keycloak.Organization{ID: "org-1", Name: "tenant-a", Domains: []keycloak.OrganizationDomain{{Name: "a.example.com"}}},
				keycloak.Organization{ID: "org-2", Name: "tenant-b", Domains: []keycloak.OrganizationDomain{
					{Name: "b.example.com"}, {Name: "b.alt.example.com"},
				}},
			),
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
			client:    newFakeOrganizationStore(),
			wantNames: []string{},
		},
		{
			name:    "upstream error",
			client:  &fakeOrganizationStore{listErr: errors.New("token rejected")},
			wantErr: "token rejected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, err := NewTenantsServer().SetOrganizationClient(tc.client).Build()
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

func TestGetTenant(t *testing.T) {
	t.Parallel()

	store := newFakeOrganizationStore(
		keycloak.Organization{ID: "org-1", Name: "tenant-a", Domains: []keycloak.OrganizationDomain{{Name: "a.example.com"}}},
	)
	server := mustTenantsServer(t, store)

	got, err := server.Get(context.Background(), &aigridv1.TenantsGetRequest{Id: "org-1"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetObject().GetMetadata().GetName() != "tenant-a" {
		t.Fatalf("name = %q", got.GetObject().GetMetadata().GetName())
	}

	_, err = server.Get(context.Background(), &aigridv1.TenantsGetRequest{Id: "missing"})
	assertConnectCode(t, err, connect.CodeNotFound)

	_, err = server.Get(context.Background(), &aigridv1.TenantsGetRequest{})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

func TestCreateTenant(t *testing.T) {
	t.Parallel()

	t.Run("creates tenant", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		got, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}},
			},
		})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got.GetObject().GetId() == "" {
			t.Fatal("expected Keycloak-assigned id")
		}
		if got.GetObject().GetMetadata().GetName() != "acme" {
			t.Fatalf("name = %q", got.GetObject().GetMetadata().GetName())
		}
		assertStringSlicesEqual(t, got.GetObject().GetSpec().GetDomains(), []string{"acme.example.com"})
	})

	t.Run("already exists", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore(
			keycloak.Organization{ID: "org-1", Name: "acme"},
		))
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}},
		})
		assertConnectCode(t, err, connect.CodeAlreadyExists)
	})

	t.Run("invalid name", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "INVALID"}},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("invalid domain single label", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"localhost"}},
			},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("invalid domain ip", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"1.2.3.4"}},
			},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("invalid domain uppercase", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"Acme.Example.COM"}},
			},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("duplicate domains", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com", "acme.example.com"}},
			},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("blank domain entry is rejected", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{" "}},
			},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("nil object", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})
}

func TestUpdateTenant(t *testing.T) {
	t.Parallel()

	t.Run("updates name and domains", func(t *testing.T) {
		t.Parallel()
		enabled := false
		store := newFakeOrganizationStore(
			keycloak.Organization{
				ID: "org-1", Name: "acme", Alias: "acme",
				Enabled: &enabled, Description: "keep-me", RedirectURL: "https://acme.example.com",
				Domains: []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
			},
		)
		server := mustTenantsServer(t, store)
		got, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:       "org-1",
				Metadata: &aigridv1.Metadata{Name: "acme-renamed"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"renamed.example.com"}},
			},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetName() != "acme-renamed" {
			t.Fatalf("name = %q", got.GetObject().GetMetadata().GetName())
		}
		assertStringSlicesEqual(t, got.GetObject().GetSpec().GetDomains(), []string{"renamed.example.com"})
		stored, err := store.GetOrganization(context.Background(), "org-1")
		if err != nil {
			t.Fatalf("GetOrganization: %v", err)
		}
		if stored.Alias != "acme" {
			t.Fatalf("alias changed on rename: %q", stored.Alias)
		}
		if stored.Enabled == nil || *stored.Enabled || stored.Description != "keep-me" || stored.RedirectURL != "https://acme.example.com" {
			t.Fatalf("Keycloak fields not preserved: %+v", stored)
		}
	})

	t.Run("field mask domains only", func(t *testing.T) {
		t.Parallel()
		enabled := false
		store := newFakeOrganizationStore(
			keycloak.Organization{
				ID: "org-1", Name: "acme", Alias: "acme",
				Enabled: &enabled, Description: "keep-me",
				Domains: []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
			},
		)
		server := mustTenantsServer(t, store)
		got, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:       "org-1",
				Metadata: &aigridv1.Metadata{Name: "should-not-apply"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"only-domains.example.com"}},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetName() != "acme" {
			t.Fatalf("name changed despite domains-only mask: %q", got.GetObject().GetMetadata().GetName())
		}
		assertStringSlicesEqual(t, got.GetObject().GetSpec().GetDomains(), []string{"only-domains.example.com"})
		stored, err := store.GetOrganization(context.Background(), "org-1")
		if err != nil {
			t.Fatalf("GetOrganization: %v", err)
		}
		if stored.Alias != "acme" || stored.Description != "keep-me" || stored.Enabled == nil || *stored.Enabled {
			t.Fatalf("domains-only update cleared Keycloak fields: %+v", stored)
		}
	})

	t.Run("rejects unsupported field mask paths", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(
			keycloak.Organization{
				ID: "org-1", Name: "acme", Alias: "acme",
				Domains: []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
			},
		)
		server := mustTenantsServer(t, store)
		_, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:       "org-1",
				Metadata: &aigridv1.Metadata{Name: "unexpected"},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		stored, err := store.GetOrganization(context.Background(), "org-1")
		if err != nil {
			t.Fatalf("GetOrganization: %v", err)
		}
		if stored.Name != "acme" {
			t.Fatalf("unsupported mask mutated name to %q", stored.Name)
		}
	})

	t.Run("empty mask without name preserves display name", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(
			keycloak.Organization{
				ID: "org-1", Name: "acme", Alias: "acme",
				Domains: []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
			},
		)
		server := mustTenantsServer(t, store)
		got, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:   "org-1",
				Spec: &aigridv1.TenantSpec{Domains: []string{"new.example.com"}},
			},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetName() != "acme" {
			t.Fatalf("name became %q; want preserved acme (not org id)", got.GetObject().GetMetadata().GetName())
		}
		assertStringSlicesEqual(t, got.GetObject().GetSpec().GetDomains(), []string{"new.example.com"})
	})

	t.Run("explicit empty name is rejected", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(
			keycloak.Organization{ID: "org-1", Name: "acme", Alias: "acme"},
		)
		server := mustTenantsServer(t, store)
		_, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:       "org-1",
				Metadata: &aigridv1.Metadata{Name: ""},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.name"}},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("blank domain entry is rejected", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(
			keycloak.Organization{
				ID: "org-1", Name: "acme", Alias: "acme",
				Domains: []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
			},
		)
		server := mustTenantsServer(t, store)
		_, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:   "org-1",
				Spec: &aigridv1.TenantSpec{Domains: []string{" "}},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		stored, err := store.GetOrganization(context.Background(), "org-1")
		if err != nil {
			t.Fatalf("GetOrganization: %v", err)
		}
		if len(stored.Domains) != 1 || stored.Domains[0].Name != "acme.example.com" {
			t.Fatalf("blank domain mutated store: %+v", stored.Domains)
		}
	})

	t.Run("preserves attributes on update", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(
			keycloak.Organization{
				ID: "org-1", Name: "acme", Alias: "acme",
				Attributes: map[string][]string{"billing-id": {"keep-me"}},
				Domains:    []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
			},
		)
		server := mustTenantsServer(t, store)
		_, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:       "org-1",
				Metadata: &aigridv1.Metadata{Name: "acme-renamed"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"renamed.example.com"}},
			},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		stored, err := store.GetOrganization(context.Background(), "org-1")
		if err != nil {
			t.Fatalf("GetOrganization: %v", err)
		}
		if got := stored.Attributes["billing-id"]; len(got) != 1 || got[0] != "keep-me" {
			t.Fatalf("attributes dropped: %+v", stored.Attributes)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Update(context.Background(), &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id:       "missing",
				Metadata: &aigridv1.Metadata{Name: "missing"},
			},
		})
		assertConnectCode(t, err, connect.CodeNotFound)
	})
}

func TestListTenantMembers(t *testing.T) {
	t.Parallel()

	t.Run("lists members", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(
			keycloak.Organization{ID: "org-1", Name: "acme"},
		)
		store.members["org-1"] = []keycloak.OrganizationMember{
			{ID: "u1", Username: "alice", Email: "alice@acme.example.com", Enabled: true},
			{ID: "u2", Username: "bob", FirstName: "Bob", LastName: "Builder", Enabled: false},
		}
		server := mustTenantsServer(t, store)
		got, err := server.ListMembers(context.Background(), &aigridv1.TenantsListMembersRequest{Id: "org-1"})
		if err != nil {
			t.Fatalf("ListMembers() error = %v", err)
		}
		if got.GetTotal() != 2 || got.GetSize() != 2 || len(got.GetItems()) != 2 {
			t.Fatalf("ListMembers() = %+v", got)
		}
		if got.GetItems()[0].GetUsername() != "alice" || got.GetItems()[1].GetUsername() != "bob" {
			t.Fatalf("items = %+v", got.GetItems())
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.ListMembers(context.Background(), &aigridv1.TenantsListMembersRequest{Id: "missing"})
		assertConnectCode(t, err, connect.CodeNotFound)
	})

	t.Run("requires id", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.ListMembers(context.Background(), &aigridv1.TenantsListMembersRequest{})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})
}

func TestDeleteTenant(t *testing.T) {
	t.Parallel()

	t.Run("deletes existing", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore(
			keycloak.Organization{ID: "org-1", Name: "acme"},
		))
		_, err := server.Delete(context.Background(), &aigridv1.TenantsDeleteRequest{Id: "org-1"})
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		_, err = server.Get(context.Background(), &aigridv1.TenantsGetRequest{Id: "org-1"})
		assertConnectCode(t, err, connect.CodeNotFound)
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Delete(context.Background(), &aigridv1.TenantsDeleteRequest{Id: "missing"})
		assertConnectCode(t, err, connect.CodeNotFound)
	})
}

type fakeOrganizationStore struct {
	mu      sync.Mutex
	byID    map[string]keycloak.Organization
	members map[string][]keycloak.OrganizationMember
	next    int
	listErr error
	// updateDelay widens the window between an Update's read and its write.
	updateDelay time.Duration
	// readBackErr makes UpdateOrganization store the change, then fail the
	// read-back as the real client does.
	readBackErr error
	puts        int
}

func newFakeOrganizationStore(orgs ...keycloak.Organization) *fakeOrganizationStore {
	store := &fakeOrganizationStore{
		byID:    make(map[string]keycloak.Organization, len(orgs)),
		members: make(map[string][]keycloak.OrganizationMember),
	}
	for _, org := range orgs {
		store.byID[org.ID] = org
	}
	return store
}

func (f *fakeOrganizationStore) ListOrganizations(context.Context) ([]keycloak.Organization, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keycloak.Organization, 0, len(f.byID))
	for _, org := range f.byID {
		out = append(out, org)
	}
	return out, nil
}

func (f *fakeOrganizationStore) GetOrganization(_ context.Context, id string) (keycloak.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	org, ok := f.byID[id]
	if !ok {
		return keycloak.Organization{}, &keycloak.APIError{Op: "get organization", StatusCode: http.StatusNotFound}
	}
	return org, nil
}

func (f *fakeOrganizationStore) CreateOrganization(_ context.Context, org keycloak.Organization) (keycloak.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if existing.Name == org.Name {
			return keycloak.Organization{}, &keycloak.APIError{Op: "create organization", StatusCode: http.StatusConflict}
		}
	}
	f.next++
	org.ID = fmt.Sprintf("org-%d", f.next)
	if org.Alias == "" {
		org.Alias = org.Name
	}
	f.byID[org.ID] = org
	return org, nil
}

func (f *fakeOrganizationStore) UpdateOrganization(_ context.Context, org keycloak.Organization) (keycloak.Organization, error) {
	time.Sleep(f.updateDelay)
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.byID[org.ID]
	if !ok {
		return keycloak.Organization{}, &keycloak.APIError{Op: "update organization", StatusCode: http.StatusNotFound}
	}
	for id, other := range f.byID {
		if id != org.ID && other.Name == org.Name {
			return keycloak.Organization{}, &keycloak.APIError{Op: "update organization", StatusCode: http.StatusConflict}
		}
	}
	f.puts++
	// Keycloak alias is immutable once set; preserve when omitted.
	if org.Alias == "" {
		org.Alias = existing.Alias
	}
	f.byID[org.ID] = org
	if f.readBackErr != nil {
		return keycloak.Organization{}, &keycloak.ReadBackError{Written: org, Err: f.readBackErr}
	}
	return org, nil
}

func (f *fakeOrganizationStore) ListOrganizationMembers(_ context.Context, orgID string) ([]keycloak.OrganizationMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[orgID]; !ok {
		return nil, &keycloak.APIError{Op: "list organization members", StatusCode: http.StatusNotFound}
	}
	out := make([]keycloak.OrganizationMember, len(f.members[orgID]))
	copy(out, f.members[orgID])
	return out, nil
}

func (f *fakeOrganizationStore) DeleteOrganization(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[id]; !ok {
		return &keycloak.APIError{Op: "delete organization", StatusCode: http.StatusNotFound}
	}
	delete(f.byID, id)
	return nil
}

func mustTenantsServer(t *testing.T, client organizationClient) *TenantsServer {
	t.Helper()
	server, err := NewTenantsServer().SetOrganizationClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return server
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

// assertConnectCode requires a *connect.Error, not just a matching code:
// connect.CodeOf answers CodeUnknown for any uncoded error, so checking the
// code alone would pass for a plain Go error.
func assertConnectCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want connect code %v", want)
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("error = %v (%T), want *connect.Error with code %v", err, err, want)
	}
	if connectErr.Code() != want {
		t.Fatalf("connect code = %v, want %v (%v)", connectErr.Code(), want, err)
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

func TestTenantVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stamped := func(v string) keycloak.Organization {
		return keycloak.Organization{
			ID: "org-1", Name: "acme", Alias: "acme",
			Attributes: map[string][]string{tenantVersionAttribute: {v}, "billing-id": {"keep-me"}},
			Domains:    []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
		}
	}
	update := func(version int32, lock bool) *aigridv1.TenantsUpdateRequest {
		return &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: "org-1", Metadata: &aigridv1.Metadata{Name: "acme-renamed", Version: version}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.name"}},
			Lock:       lock,
		}
	}

	t.Run("create stamps version 1 and the create time", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		server.now = func() time.Time { return time.Date(2026, 10, 22, 9, 30, 0, 0, time.FixedZone("ET", -4*3600)) }
		got, err := server.Create(ctx, &aigridv1.TenantsCreateRequest{Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}}})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		meta := got.GetObject().GetMetadata()
		if meta.GetVersion() != 1 || meta.GetCreationTimestamp().AsTime().Format(time.RFC3339) != "2026-10-22T13:30:00Z" {
			t.Fatalf("Create() metadata = %v", meta)
		}
	})

	t.Run("an organization made outside the backend is version 1 with no create time", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore(
			keycloak.Organization{ID: "org-1", Name: "acme"},
			keycloak.Organization{ID: "org-2", Name: "beta", Attributes: map[string][]string{tenantVersionAttribute: {"junk"}, tenantCreateTimeAttribute: {"yesterday"}}},
		))
		list, err := server.List(ctx, &aigridv1.TenantsListRequest{})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		for _, item := range list.GetItems() {
			if item.GetMetadata().GetVersion() != 1 || item.GetMetadata().GetCreationTimestamp() != nil {
				t.Fatalf("%s metadata = %v", item.GetId(), item.GetMetadata())
			}
		}
	})

	t.Run("update at the current version bumps it and keeps the rest", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		store.byID["org-1"].Domains[0].Verified = true
		got, err := mustTenantsServer(t, store).Update(ctx, update(4, true))
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetVersion() != 5 {
			t.Fatalf("version = %d, want 5", got.GetObject().GetMetadata().GetVersion())
		}
		stored := store.byID["org-1"]
		if stored.Attributes["billing-id"][0] != "keep-me" || !stored.Domains[0].Verified {
			t.Fatalf("update dropped stored fields: %+v", stored)
		}
	})

	t.Run("an unstamped organization takes version 1 and becomes 2", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(keycloak.Organization{ID: "org-1", Name: "acme", Alias: "acme"})
		got, err := mustTenantsServer(t, store).Update(ctx, update(1, true))
		if err != nil || got.GetObject().GetMetadata().GetVersion() != 2 {
			t.Fatalf("Update() = %v, %v; want version 2", got, err)
		}
	})

	t.Run("an update without a version is unchecked and still bumps", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		got, err := mustTenantsServer(t, store).Update(ctx, update(0, false))
		if err != nil || got.GetObject().GetMetadata().GetVersion() != 5 {
			t.Fatalf("Update() = %v, %v; want version 5", got, err)
		}
	})

	t.Run("a stale version is Aborted and writes nothing", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		server := mustTenantsServer(t, store)
		for _, lock := range []bool{true, false} {
			_, err := server.Update(ctx, update(3, lock))
			assertConnectCode(t, err, connect.CodeAborted)
		}
		if store.byID["org-1"].Name != "acme" {
			t.Fatalf("stale update wrote %+v", store.byID["org-1"])
		}
	})

	t.Run("lock without a version is InvalidArgument", func(t *testing.T) {
		t.Parallel()
		_, err := mustTenantsServer(t, newFakeOrganizationStore(stamped("4"))).Update(ctx, update(0, true))
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("a REST body that sends back version and creation time is accepted", func(t *testing.T) {
		t.Parallel()
		req := update(4, false)
		req.UpdateMask.Paths = append(req.UpdateMask.Paths, "metadata.version", "metadata.creation_timestamp")
		if _, err := mustTenantsServer(t, newFakeOrganizationStore(stamped("4"))).Update(ctx, req); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	})

	t.Run("removing the last domain clears domains", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		_, err := mustTenantsServer(t, store).Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: "org-1", Spec: &aigridv1.TenantSpec{}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got := store.byID["org-1"].Domains; len(got) != 0 {
			t.Fatalf("domains = %v, want none", got)
		}
	})

	t.Run("a kept domain keeps its stored details", func(t *testing.T) {
		t.Parallel()
		org := stamped("4")
		org.Domains = []keycloak.OrganizationDomain{{
			Name: "acme.example.com", Verified: true,
			Extra: map[string]json.RawMessage{"identityProviderAlias": json.RawMessage(`"acme-idp"`)},
		}}
		store := newFakeOrganizationStore(org)
		_, err := mustTenantsServer(t, store).Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: "org-1", Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com", "new.example.com"}}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
		})
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		got := store.byID["org-1"].Domains
		if len(got) != 2 || !got[0].Verified || string(got[0].Extra["identityProviderAlias"]) != `"acme-idp"` || got[1].Name != "new.example.com" || got[1].Verified {
			t.Fatalf("domains = %+v", got)
		}
	})

	t.Run("two concurrent edits at one version: the second is Aborted", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		store.updateDelay = 50 * time.Millisecond
		server := mustTenantsServer(t, store)
		rename := update(4, true)
		domains := &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: "org-1", Metadata: &aigridv1.Metadata{Version: 4}, Spec: &aigridv1.TenantSpec{Domains: []string{"other.example.com"}}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
			Lock:       true,
		}
		errs := make(chan error, 2)
		for _, req := range []*aigridv1.TenantsUpdateRequest{rename, domains} {
			go func() {
				_, err := server.Update(ctx, req)
				errs <- err
			}()
		}
		var ok, aborted int
		for range 2 {
			switch err := <-errs; {
			case err == nil:
				ok++
			case connect.CodeOf(err) == connect.CodeAborted:
				aborted++
			default:
				t.Fatalf("Update() error = %v", err)
			}
		}
		if ok != 1 || aborted != 1 || store.puts != 1 {
			t.Fatalf("ok = %d, aborted = %d, puts = %d; want 1, 1, 1", ok, aborted, store.puts)
		}
	})

	t.Run("a mask with only id, version and creation time writes nothing", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		req := update(4, true)
		req.UpdateMask.Paths = []string{"id", "metadata.version", "metadata.creation_timestamp"}
		got, err := mustTenantsServer(t, store).Update(ctx, req)
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetVersion() != 4 || got.GetObject().GetMetadata().GetName() != "acme" || store.puts != 0 {
			t.Fatalf("Update() = %v after %d writes; want version 4, name acme, no write", got.GetObject(), store.puts)
		}
		// Still checked: a stale version is Aborted even when nothing would change.
		req.Object.Metadata.Version = 3
		_, err = mustTenantsServer(t, store).Update(ctx, req)
		assertConnectCode(t, err, connect.CodeAborted)
	})

	t.Run("a name used by another organization is AlreadyExists, naming the cause", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"), keycloak.Organization{ID: "org-2", Name: "acme-renamed"})
		_, err := mustTenantsServer(t, store).Update(ctx, update(4, true))
		assertConnectCode(t, err, connect.CodeAlreadyExists)
		if !strings.Contains(err.Error(), "already used by another organization") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("a failed read-back answers with what was written", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		store.readBackErr = errors.New("connection reset")
		got, err := mustTenantsServer(t, store).Update(ctx, update(4, true))
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got.GetObject().GetMetadata().GetVersion() != 5 || got.GetObject().GetMetadata().GetName() != "acme-renamed" {
			t.Fatalf("Update() = %v; want the written tenant at version 5", got.GetObject())
		}
	})

	t.Run("delete checks a non-zero version", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		server := mustTenantsServer(t, store)
		_, err := server.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 3})
		assertConnectCode(t, err, connect.CodeAborted)
		if _, ok := store.byID["org-1"]; !ok {
			t.Fatal("stale delete removed the tenant")
		}
		if _, err := server.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 4}); err != nil {
			t.Fatalf("Delete(current version) error = %v", err)
		}
		_, err = server.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 4})
		assertConnectCode(t, err, connect.CodeNotFound)
	})
}
