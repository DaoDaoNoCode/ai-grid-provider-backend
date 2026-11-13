package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
			// ParseFloat reads these; status.location doesn't, so neither does Update.
			{map[string]string{"ai-grid/latitude": "4e1"}, false},
			{map[string]string{"ai-grid/longitude": "0x1p4"}, false},
			{map[string]string{"ai-grid/latitude": "40,7"}, false},
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
		wantCode  connect.Code
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
			name:     "upstream error",
			client:   &fakeOrganizationStore{listErr: errors.New("token rejected")},
			wantCode: connect.CodeInternal,
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
			if tc.wantCode != 0 {
				assertConnectCode(t, err, tc.wantCode)
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
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}}},
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

	t.Run("with identity provider", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		secrets := &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("s3cret")}}
		server := mustTenantsServerWithSecrets(t, store, secrets)
		got, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec: &aigridv1.TenantSpec{
					Domains:          []string{"acme.example.com"},
					IdentityProvider: validCreateIdP(),
				},
			},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		idp := got.GetObject().GetSpec().GetIdentityProvider()
		if idp == nil || idp.GetConfig().GetClientSecretSecretName() != "acme-idp-secret" {
			t.Fatalf("response idp = %+v", idp)
		}
		if strings.Contains(fmt.Sprintf("%v", got), "s3cret") {
			t.Fatal("response leaked secret value")
		}
		stored := store.byID[got.GetObject().GetId()]
		chunks := stored.Attributes[tenantIdentityProviderAttribute]
		if len(chunks) == 0 {
			t.Fatalf("missing idp attribute: %+v", stored.Attributes)
		}
		for i, c := range chunks {
			if len(c) > keycloakGroupAttributeMax {
				t.Fatalf("idp attribute chunk %d length %d exceeds Keycloak limit %d", i, len(c), keycloakGroupAttributeMax)
			}
		}
		gotIdP, err := identityProviderFromOrganizationAttrs(stored.Attributes)
		if err != nil || gotIdP == nil || gotIdP.GetConfig().GetClientSecretSecretName() != "acme-idp-secret" {
			t.Fatalf("idp attribute round-trip = %+v, err=%v", gotIdP, err)
		}
		if _, ok := store.idps["acme"]; !ok {
			t.Fatal("idp alias not created")
		}
		if store.idps["acme"].Config["useJwksUrl"] != "true" || store.idps["acme"].Config["jwksUrl"] == "" {
			t.Fatalf("keycloak idp config = %+v, want jwksUrl and useJwksUrl", store.idps["acme"].Config)
		}
		// Brokered users' emails must read as verified, or the MaaS Portal's oauth2-proxy fails.
		if !store.idps["acme"].TrustEmail {
			t.Fatal("keycloak idp trustEmail = false, want true")
		}
		// The sign-in page must not list this organization's IdP to users of no organization.
		if store.idps["acme"].Config["kc.org.broker.login.hide-when-org-unknown"] != "true" {
			t.Fatalf("keycloak idp config = %+v, want kc.org.broker.login.hide-when-org-unknown true", store.idps["acme"].Config)
		}
		if store.mappers["acme"].Config["attribute.value"] != got.GetObject().GetId() {
			t.Fatalf("mapper value = %q, want org id", store.mappers["acme"].Config["attribute.value"])
		}
		domains := store.byID[got.GetObject().GetId()].Domains
		if len(domains) != 1 || domains[0].Name != "acme.example.com" || domains[0].IdentityProviderAlias != "acme" || !domains[0].AutoRedirect {
			t.Fatalf("domains = %+v, want acme.example.com routed to IdP acme with autoRedirect", domains)
		}
		if len(store.imports) != 1 || store.imports[0] != "https://idp.example.com/.well-known/openid-configuration" || store.reloadKeysCalls != 1 {
			t.Fatalf("discovery checks = %v, signing key checks = %d; want one each", store.imports, store.reloadKeysCalls)
		}
	})

	t.Run("issuer with a trailing slash", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.discoveredIssuers = map[string]string{"https://idp.example.com/.well-known/openid-configuration": "https://idp.example.com/"}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		idp := validCreateIdP()
		idp.Config.Issuer = "https://idp.example.com/"
		if _, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: idp}},
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	})

	t.Run("issuer its discovery document doesn't name is refused before anything is created", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.discoveredIssuers = map[string]string{"https://idp.example.com/.well-known/openid-configuration": "https://idp.example.com/realms/other"}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()}},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("created orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("unreachable discovery document is refused before anything is created", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.importErr = &keycloak.APIError{Op: "import identity provider config", StatusCode: http.StatusInternalServerError}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()}},
		})
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("created orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("unreadable signing keys clean up", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.reloadKeysErr = &keycloak.APIError{Op: "reload identity provider keys", StatusCode: http.StatusInternalServerError}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()}},
		})
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("cleanup failed: orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("no domains is refused before anything is created", func(t *testing.T) {
		t.Parallel()
		for name, spec := range map[string]*aigridv1.TenantSpec{
			"no spec":                          nil,
			"empty domains":                    {Domains: []string{}},
			"identity provider and no domains": {IdentityProvider: validCreateIdP()},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				store := newFakeOrganizationStore()
				server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("s3cret")}})
				_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
					Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: spec},
				})
				assertConnectCode(t, err, connect.CodeInvalidArgument)
				if !strings.Contains(err.Error(), "spec.domains must contain at least one domain") {
					t.Fatalf("Create() error = %v, want the domains message", err)
				}
				if len(store.byID) != 0 || len(store.idps) != 0 {
					t.Fatalf("created orgs=%v idps=%v", store.byID, store.idps)
				}
			})
		}
	})

	t.Run("missing secret", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
		assertConnectCode(t, err, connect.CodeNotFound)
		if len(store.byID) != 0 {
			t.Fatalf("org left behind: %+v", store.byID)
		}
	})

	t.Run("secret without a client secret", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": {}}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("left behind: orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("idp create failure cleans up", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.createIdPErr = &keycloak.APIError{Op: "create identity provider", StatusCode: http.StatusBadGateway}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("cleanup failed: orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("link failure cleans up idp and org", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.linkIdPErr = &keycloak.APIError{Op: "link", StatusCode: http.StatusBadGateway}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("cleanup failed: orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("cleanup runs after the caller cancels", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.linkIdPErr = &keycloak.APIError{Op: "link", StatusCode: http.StatusBadGateway}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store.linkHook = cancel
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(ctx, &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("canceled caller left orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("mapper failure cleans up", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.mapperErr = &keycloak.APIError{Op: "mapper", StatusCode: http.StatusBadGateway}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if len(store.byID) != 0 || len(store.idps) != 0 {
			t.Fatalf("cleanup failed: orgs=%v idps=%v", store.byID, store.idps)
		}
	})

	t.Run("cleanup failure names both errors", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		store.createIdPErr = errors.New("setup boom")
		store.deleteOrgErr = errors.New("cleanup boom")
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret": []byte("x")}})
		_, err := server.Create(context.Background(), &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
		assertConnectCode(t, err, connect.CodeInternal)
		if !strings.Contains(err.Error(), "setup boom") || !strings.Contains(err.Error(), "cleanup boom") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestListSkipsUnreadableIdentityProvider(t *testing.T) {
	t.Parallel()
	normalized, err := normalizeIdentityProvider("good-org", validCreateIdP())
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	chunks, err := identityProviderToAttributeValues(normalized)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	store := newFakeOrganizationStore(
		keycloak.Organization{
			ID: "bad", Name: "bad-org", Alias: "bad-org",
			Domains: []keycloak.OrganizationDomain{{Name: "bad.example.com"}},
			Attributes: map[string][]string{
				tenantVersionAttribute:          {"1"},
				tenantIdentityProviderAttribute: {"{"},
			},
		},
		keycloak.Organization{
			ID: "good", Name: "good-org", Alias: "good-org",
			Attributes: map[string][]string{
				tenantVersionAttribute:          {"1"},
				tenantIdentityProviderAttribute: chunks,
			},
		},
	)
	server := mustTenantsServer(t, store)
	listed, err := server.List(context.Background(), &aigridv1.TenantsListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if listed.GetTotal() != 2 || len(listed.GetItems()) != 2 {
		t.Fatalf("List size = %d items %d, want both tenants", listed.GetTotal(), len(listed.GetItems()))
	}
	byID := map[string]*aigridv1.Tenant{}
	for _, item := range listed.GetItems() {
		byID[item.GetId()] = item
	}
	bad := byID["bad"]
	if bad == nil || bad.GetSpec().GetIdentityProvider() != nil || len(bad.GetSpec().GetDomains()) != 1 {
		t.Fatalf("bad tenant = %+v", bad)
	}
	good := byID["good"]
	if good.GetSpec().GetIdentityProvider().GetConfig().GetClientSecretSecretName() != "acme-idp-secret" {
		t.Fatalf("good tenant idp = %+v", good.GetSpec().GetIdentityProvider())
	}
	got, err := server.Get(context.Background(), &aigridv1.TenantsGetRequest{Id: "bad"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetObject().GetSpec().GetIdentityProvider() != nil || got.GetObject().GetMetadata().GetName() != "bad-org" {
		t.Fatalf("Get bad = %+v", got.GetObject())
	}
}

func TestTenantIdentityProviderLoginIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	createTenant := func(t *testing.T, store *fakeOrganizationStore) (*aigridv1.TenantsCreateResponse, error) {
		t.Helper()
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{
			secrets: map[string][]byte{"acme-idp-secret": []byte("s3cret")},
		})
		return server.Create(ctx, &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{
				Metadata: &aigridv1.Metadata{Name: "acme"},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}, IdentityProvider: validCreateIdP()},
			},
		})
	}

	t.Run("uses the built-in flow and installs six mappers", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		got, err := createTenant(t, store)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		idp := store.idps["acme"]
		var flow string
		if err := json.Unmarshal(idp.Extra["firstBrokerLoginFlowAlias"], &flow); err != nil || flow != keycloak.FirstBrokerLoginFlowAlias {
			t.Fatalf("firstBrokerLoginFlowAlias = %q, want %q", idp.Extra["firstBrokerLoginFlowAlias"], keycloak.FirstBrokerLoginFlowAlias)
		}
		wantMappers := []string{
			"AI Grid org id",
			"AI Grid org name",
			"AI Grid groups",
			"AI Grid roles",
			"AI Grid local username",
			"AI Grid preferred username",
		}
		if !slices.Equal(store.mapperNames["acme"], wantMappers) {
			t.Fatalf("mappers = %v, want %v", store.mapperNames["acme"], wantMappers)
		}
		if store.mapperCalls != 1 {
			t.Fatalf("mapper calls = %d, want 1", store.mapperCalls)
		}
		if got.GetObject().GetSpec().GetIdentityProvider() == nil {
			t.Fatal("response omitted identity provider")
		}
	})

	t.Run("new mapper failure cleans up", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"AI Grid local username", "AI Grid preferred username"} {
			t.Run(name, func(t *testing.T) {
				store := newFakeOrganizationStore()
				store.failMapperName = name
				_, err := createTenant(t, store)
				assertConnectCode(t, err, connect.CodeInternal)
				if len(store.byID) != 0 || len(store.idps) != 0 || len(store.links) != 0 {
					t.Fatalf("cleanup failed: orgs=%v idps=%v links=%v", store.byID, store.idps, store.links)
				}
			})
		}
	})

	t.Run("tenant without an idp does not touch idp state", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		server := mustTenantsServer(t, store)
		if _, err := server.Create(ctx, &aigridv1.TenantsCreateRequest{
			Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}}},
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(store.idps) != 0 || store.mapperCalls != 0 {
			t.Fatalf("idps=%v mapperCalls=%d, want no idp writes", store.idps, store.mapperCalls)
		}
	})

	t.Run("existing tenant update refreshes mappers", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore()
		got, err := createTenant(t, store)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		server := mustTenantsServerWithSecrets(t, store, &fakeSecretReader{
			secrets: map[string][]byte{"acme-idp-secret": []byte("s3cret")},
		})
		if _, err := server.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id: got.GetObject().GetId(), Metadata: &aigridv1.Metadata{Version: 1, Name: "acme-corp"},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.name"}},
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if store.mapperCalls != 2 {
			t.Fatalf("mapper calls = %d, want 2", store.mapperCalls)
		}
		if got := store.mapperOrgNames["acme"]; got != "acme-corp" {
			t.Fatalf("mapper org name = %q, want acme-corp", got)
		}
	})
}

func TestUpdateTenantIdentityProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	normalized, err := normalizeIdentityProvider("acme", validCreateIdP())
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	raw, err := identityProviderToAttribute(normalized)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	org := keycloak.Organization{
		ID: "org-1", Name: "acme", Alias: "acme",
		Attributes: map[string][]string{
			tenantVersionAttribute:          {"1"},
			tenantIdentityProviderAttribute: {raw},
		},
	}

	t.Run("maskless identity provider patch changes it and keeps domains", func(t *testing.T) {
		t.Parallel()
		stored := org
		stored.Domains = []keycloak.OrganizationDomain{{Name: "acme.example.com", Verified: true, IdentityProviderAlias: "acme", AutoRedirect: true}}
		store := idpStore(t, stored)
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		changed := validCreateIdP()
		changed.Title = "other"
		_, err := server.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1},
				Spec: &aigridv1.TenantSpec{IdentityProvider: changed},
			},
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		got := store.byID["org-1"]
		if len(got.Domains) != 1 || got.Domains[0].Name != "acme.example.com" || !got.Domains[0].Verified {
			t.Fatalf("domains = %+v, want acme.example.com kept", got.Domains)
		}
		if store.idps["acme"].DisplayName != "other" {
			t.Fatalf("keycloak idp displayName = %q, want other", store.idps["acme"].DisplayName)
		}
	})

	t.Run("same idp succeeds without idp write", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(org)
		server := mustTenantsServer(t, store)
		_, err := server.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1},
				Spec: &aigridv1.TenantSpec{IdentityProvider: validCreateIdP()},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.identity_provider"}},
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if store.puts != 0 || store.idpUpdates != 0 || len(store.imports) != 0 {
			t.Fatalf("unexpected write or check: puts=%d idpUpdates=%d imports=%v", store.puts, store.idpUpdates, store.imports)
		}
	})

	t.Run("rename refreshes the org name mapper", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(org)
		store.idps["acme"] = keycloak.IdentityProvider{Alias: "acme"}
		server := mustTenantsServer(t, store)
		_, err := server.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1, Name: "acme-corp"},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.name"}},
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got := store.mapperOrgNames["acme"]; got != "acme-corp" {
			t.Fatalf("mapper org name = %q, want %q", got, "acme-corp")
		}
		if got := store.mappers["acme"].Config["attribute.value"]; got != "org-1" {
			t.Fatalf("mapper org id = %q, want org-1", got)
		}
	})

	t.Run("added domain is routed to the linked idp", func(t *testing.T) {
		t.Parallel()
		linked := org
		linked.Domains = []keycloak.OrganizationDomain{{Name: "acme.example.com", IdentityProviderAlias: "acme", AutoRedirect: true}}
		store := newFakeOrganizationStore(linked)
		store.idps["acme"] = keycloak.IdentityProvider{Alias: "acme"}
		store.links["org-1"] = "acme"
		server := mustTenantsServer(t, store)
		_, err := server.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object: &aigridv1.Tenant{
				Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1},
				Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com", "acme.example.org"}},
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		domains := store.byID["org-1"].Domains
		if len(domains) != 2 {
			t.Fatalf("domains = %+v", domains)
		}
		for _, d := range domains {
			if d.IdentityProviderAlias != "acme" || !d.AutoRedirect {
				t.Fatalf("domain %s = %+v, want routed to IdP acme with autoRedirect", d.Name, d)
			}
		}
	})

	t.Run("rename without an idp writes no mappers", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(keycloak.Organization{
			ID: "org-1", Name: "acme", Alias: "acme",
			Attributes: map[string][]string{tenantVersionAttribute: {"1"}},
		})
		server := mustTenantsServer(t, store)
		_, err := server.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1, Name: "acme-corp"}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.name"}},
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if len(store.mapperOrgNames) != 0 {
			t.Fatalf("unexpected mapper upsert: %v", store.mapperOrgNames)
		}
	})

	t.Run("remove idp rejected", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(org)
		server := mustTenantsServer(t, store)
		_, err := server.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.identity_provider"}},
		})
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
				Metadata: &aigridv1.Metadata{Version: 1, Name: "acme-renamed"},
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
				Metadata: &aigridv1.Metadata{Version: 1, Name: "should-not-apply"},
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
				Metadata: &aigridv1.Metadata{Version: 1, Name: "unexpected"},
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
				Id:       "org-1",
				Metadata: &aigridv1.Metadata{Version: 1},
				Spec:     &aigridv1.TenantSpec{Domains: []string{"new.example.com"}},
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
				Metadata: &aigridv1.Metadata{Version: 1, Name: ""},
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
				Metadata: &aigridv1.Metadata{Version: 1, Name: "acme-renamed"},
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
				Metadata: &aigridv1.Metadata{Version: 1, Name: "missing"},
			},
		})
		assertConnectCode(t, err, connect.CodeNotFound)
	})
}

func TestTenantMemberSeparatesIdentityAndUsernames(t *testing.T) {
	t.Parallel()

	store := newFakeOrganizationStore(
		keycloak.Organization{ID: "org-1", Name: "acme"},
	)
	store.members["org-1"] = []keycloak.OrganizationMember{
		{
			ID: "user-alice-a", Username: "11111111-2222-4333-8444-555555555555", Enabled: true,
			Attributes: map[string][]string{"maas_preferred_username": {"alice"}},
		},
		{
			ID: "user-alice-b", Username: "66666666-7777-4888-9999-aaaaaaaaaaaa", Enabled: true,
			Attributes: map[string][]string{"maas_preferred_username": {"alice"}},
		},
	}
	server := mustTenantsServer(t, store)
	got, err := server.ListMembers(context.Background(), &aigridv1.TenantsListMembersRequest{Id: "org-1"})
	if err != nil {
		t.Fatalf("ListMembers() error = %v", err)
	}
	if len(got.GetItems()) != 2 {
		t.Fatalf("ListMembers() items = %+v", got.GetItems())
	}
	members := []struct {
		id                string
		username          string
		preferredUsername string
	}{
		{id: "user-alice-a", username: "11111111-2222-4333-8444-555555555555", preferredUsername: "alice"},
		{id: "user-alice-b", username: "66666666-7777-4888-9999-aaaaaaaaaaaa", preferredUsername: "alice"},
	}
	for i, want := range members {
		item := got.GetItems()[i]
		if item.GetId() != want.id || item.GetUsername() != want.username {
			t.Fatalf("member %d identity = %+v", i, item)
		}
		if item.GetPreferredUsername() != want.preferredUsername {
			t.Fatalf("member %d preferred username = %q, want %q", i, item.GetPreferredUsername(), want.preferredUsername)
		}
	}

	missing := tenantMemberFromOrganizationMember(keycloak.OrganizationMember{
		ID: "user-missing", Username: "11111111-2222-4333-8444-555555555555",
	})
	if missing.GetUsername() != "11111111-2222-4333-8444-555555555555" || missing.GetPreferredUsername() != "" {
		t.Fatalf("missing profile usernames = %q and %q", missing.GetUsername(), missing.GetPreferredUsername())
	}
	legacy := tenantMemberFromOrganizationMember(keycloak.OrganizationMember{
		ID: "user-legacy", Username: "bob",
	})
	if legacy.GetUsername() != "bob" || legacy.GetPreferredUsername() != "" {
		t.Fatalf("legacy usernames = %q and %q, want bob and empty", legacy.GetUsername(), legacy.GetPreferredUsername())
	}
	renamedLegacy := tenantMemberFromOrganizationMember(keycloak.OrganizationMember{
		ID: "user-renamed-legacy", Username: "bob",
		Attributes: map[string][]string{"maas_preferred_username": {"bob-renamed"}},
	})
	if renamedLegacy.GetUsername() != "bob" || renamedLegacy.GetPreferredUsername() != "bob-renamed" {
		t.Fatalf("renamed legacy usernames = %q and %q, want bob and bob-renamed", renamedLegacy.GetUsername(), renamedLegacy.GetPreferredUsername())
	}

	before := keycloak.OrganizationMember{
		ID: "user-renamed", Username: "bbbbbbbb-2222-4333-8444-555555555555",
		Attributes: map[string][]string{"maas_preferred_username": {"alice"}},
	}
	after := before
	after.Attributes = map[string][]string{"maas_preferred_username": {"alice-renamed"}}
	beforeMember := tenantMemberFromOrganizationMember(before)
	afterMember := tenantMemberFromOrganizationMember(after)
	if beforeMember.GetId() != afterMember.GetId() || beforeMember.GetUsername() != afterMember.GetUsername() {
		t.Fatalf("rename changed identity: before=%+v after=%+v", beforeMember, afterMember)
	}
	if beforeMember.GetPreferredUsername() != "alice" || afterMember.GetPreferredUsername() != "alice-renamed" {
		t.Fatalf("renamed profile username = %q, want alice-renamed", afterMember.GetPreferredUsername())
	}
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
		_, err := server.Delete(context.Background(), &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 1})
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		_, err = server.Get(context.Background(), &aigridv1.TenantsGetRequest{Id: "org-1"})
		assertConnectCode(t, err, connect.CodeNotFound)
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore())
		_, err := server.Delete(context.Background(), &aigridv1.TenantsDeleteRequest{Id: "missing", Version: 1})
		assertConnectCode(t, err, connect.CodeNotFound)
	})

	t.Run("deletes idp then organization", func(t *testing.T) {
		t.Parallel()
		normalized, err := normalizeIdentityProvider("acme", validCreateIdP())
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		raw, err := identityProviderToAttribute(normalized)
		if err != nil {
			t.Fatalf("attribute: %v", err)
		}
		store := newFakeOrganizationStore(keycloak.Organization{
			ID: "org-1", Name: "acme", Alias: "acme",
			Attributes: map[string][]string{tenantIdentityProviderAttribute: {raw}},
		})
		store.idps["acme"] = keycloak.IdentityProvider{Alias: "acme"}
		server := mustTenantsServer(t, store)
		if _, err := server.Delete(context.Background(), &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 1}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, ok := store.idps["acme"]; ok {
			t.Fatal("idp not deleted")
		}
		if _, ok := store.byID["org-1"]; ok {
			t.Fatal("org not deleted")
		}
	})

	t.Run("idp already gone is ok", func(t *testing.T) {
		t.Parallel()
		normalized, err := normalizeIdentityProvider("acme", validCreateIdP())
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		raw, _ := identityProviderToAttribute(normalized)
		store := newFakeOrganizationStore(keycloak.Organization{
			ID: "org-1", Name: "acme", Alias: "acme",
			Attributes: map[string][]string{tenantIdentityProviderAttribute: {raw}},
		})
		server := mustTenantsServer(t, store)
		if _, err := server.Delete(context.Background(), &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 1}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("org kept when idp delete fails", func(t *testing.T) {
		t.Parallel()
		normalized, err := normalizeIdentityProvider("acme", validCreateIdP())
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		raw, _ := identityProviderToAttribute(normalized)
		store := newFakeOrganizationStore(keycloak.Organization{
			ID: "org-1", Name: "acme", Alias: "acme",
			Attributes: map[string][]string{tenantIdentityProviderAttribute: {raw}},
		})
		store.idps["acme"] = keycloak.IdentityProvider{Alias: "acme"}
		store.deleteIdPErr = errors.New("idp locked")
		server := mustTenantsServer(t, store)
		_, err = server.Delete(context.Background(), &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 1})
		if err == nil {
			t.Fatal("expected error")
		}
		if _, ok := store.byID["org-1"]; !ok {
			t.Fatal("organization was deleted despite idp failure")
		}
	})
}

type fakeOrganizationStore struct {
	mu      sync.Mutex
	byID    map[string]keycloak.Organization
	members map[string][]keycloak.OrganizationMember
	idps    map[string]keycloak.IdentityProvider
	links   map[string]string // orgID -> alias
	mappers map[string]keycloak.IdentityProviderMapper
	// mapperNames records the names the fake upsert would write.
	mapperNames map[string][]string
	mapperCalls int
	// mapperUpserts counts every mapper upsert.
	mapperUpserts int
	// failMapperName makes the fake fail when it reaches that mapper.
	failMapperName string
	// mapperOrgNames is the org name each alias's mappers were last upserted with.
	mapperOrgNames map[string]string
	next           int
	listErr        error
	// updateDelay widens the window between an Update's read and its write.
	updateDelay time.Duration
	// readBackErr makes UpdateOrganization store the change, then fail the
	// read-back as the real client does.
	readBackErr error
	puts        int

	createIdPErr   error
	linkIdPErr     error
	configureErr   error
	mapperErr      error
	deleteIdPErr   error
	deleteOrgErr   error
	deleteIdPCalls int
	// linkHook runs at the start of LinkOrganizationIdentityProvider, before the error.
	linkHook func()

	updateIdPErr error
	idpUpdates   int
	// importErr fails ImportIdentityProviderConfig; discoveredIssuers maps a
	// discovery URL to the issuer its document names (default: the URL
	// without /.well-known/openid-configuration).
	importErr         error
	discoveredIssuers map[string]string
	imports           []string
	reloadKeysErr     error
	reloadKeysFalse   bool
	reloadKeysCalls   int
	// mapperErrOnce fails the next mapper upsert only.
	mapperErrOnce error
	// lostAnswerErr makes UpdateOrganization store the change, then fail with it,
	// as when Keycloak's answer is lost; orgWriteErr fails it without storing.
	// getErrAfterWrite then fails the next GetOrganization, once.
	lostAnswerErr    error
	orgWriteErr      error
	orgWrites        int
	getErrAfterWrite error
	// restoreIdPErr fails every UpdateIdentityProvider after the first.
	restoreIdPErr error
	// linkedUsers is how many users have a federated identity link to each alias.
	linkedUsers map[string]int
}

func (f *fakeOrganizationStore) CountIdentityProviderUsers(_ context.Context, alias string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.linkedUsers[alias], nil
}

func newFakeOrganizationStore(orgs ...keycloak.Organization) *fakeOrganizationStore {
	store := &fakeOrganizationStore{
		byID:           make(map[string]keycloak.Organization, len(orgs)),
		members:        make(map[string][]keycloak.OrganizationMember),
		idps:           make(map[string]keycloak.IdentityProvider),
		links:          make(map[string]string),
		mappers:        make(map[string]keycloak.IdentityProviderMapper),
		mapperNames:    make(map[string][]string),
		mapperOrgNames: make(map[string]string),
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
	if err := f.getErrAfterWrite; err != nil && f.orgWrites > 0 {
		f.getErrAfterWrite = nil
		return keycloak.Organization{}, err
	}
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
	f.orgWrites++
	if err := f.orgWriteErr; err != nil {
		f.orgWriteErr = nil
		return keycloak.Organization{}, err
	}
	existing, ok := f.byID[org.ID]
	if !ok {
		return keycloak.Organization{}, &keycloak.APIError{Op: "update organization", StatusCode: http.StatusNotFound}
	}
	for id, other := range f.byID {
		if id != org.ID && other.Name == org.Name {
			return keycloak.Organization{}, &keycloak.APIError{Op: "update organization", StatusCode: http.StatusConflict}
		}
	}
	// Keycloak rejects a domain that names an IdP not linked to this organization.
	for _, d := range org.Domains {
		if d.IdentityProviderAlias != "" && f.links[org.ID] != d.IdentityProviderAlias {
			return keycloak.Organization{}, &keycloak.APIError{Op: "update organization", StatusCode: http.StatusBadRequest}
		}
	}
	f.puts++
	// Keycloak alias is immutable once set; preserve when omitted.
	if org.Alias == "" {
		org.Alias = existing.Alias
	}
	f.byID[org.ID] = org
	if err := f.lostAnswerErr; err != nil {
		f.lostAnswerErr = nil
		return keycloak.Organization{}, err
	}
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

func (f *fakeOrganizationStore) DeleteOrganization(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteOrgErr != nil {
		return f.deleteOrgErr
	}
	if _, ok := f.byID[id]; !ok {
		return &keycloak.APIError{Op: "delete organization", StatusCode: http.StatusNotFound}
	}
	delete(f.byID, id)
	delete(f.links, id)
	return nil
}

func (f *fakeOrganizationStore) CreateIdentityProvider(_ context.Context, idp keycloak.IdentityProvider) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createIdPErr != nil {
		return f.createIdPErr
	}
	if _, ok := f.idps[idp.Alias]; ok {
		return &keycloak.APIError{Op: "create identity provider", StatusCode: http.StatusConflict}
	}
	f.idps[idp.Alias] = idp
	return nil
}

// GetIdentityProvider masks the client secret as Keycloak's StripSecretsUtils does.
func (f *fakeOrganizationStore) GetIdentityProvider(_ context.Context, alias string) (keycloak.IdentityProvider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idp, ok := f.idps[alias]
	if !ok {
		return keycloak.IdentityProvider{}, &keycloak.APIError{Op: "get identity provider", StatusCode: http.StatusNotFound}
	}
	idp.Config = maps.Clone(idp.Config)
	if _, ok := idp.Config["clientSecret"]; ok {
		idp.Config["clientSecret"] = "**********"
	}
	return idp, nil
}

// GetOrganizationIdentityProvider answers 404 unless alias is linked to orgID, as
// Keycloak's OrganizationIdentityProvidersResource.getIdentityProvider does.
func (f *fakeOrganizationStore) GetOrganizationIdentityProvider(ctx context.Context, orgID, alias string) (keycloak.IdentityProvider, error) {
	f.mu.Lock()
	linked := f.links[orgID] == alias
	f.mu.Unlock()
	if !linked {
		return keycloak.IdentityProvider{}, &keycloak.APIError{Op: "get organization identity provider", StatusCode: http.StatusNotFound}
	}
	return f.GetIdentityProvider(ctx, alias)
}

// UpdateIdentityProvider keeps the stored secret when the masked value comes back, as
// Keycloak's IdentityProviderResource.updateIdpFromRep does.
func (f *fakeOrganizationStore) UpdateIdentityProvider(ctx context.Context, idp keycloak.IdentityProvider) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateIdPErr != nil {
		return f.updateIdPErr
	}
	if f.restoreIdPErr != nil && f.idpUpdates > 0 {
		return f.restoreIdPErr
	}
	existing, ok := f.idps[idp.Alias]
	if !ok {
		return &keycloak.APIError{Op: "update identity provider", StatusCode: http.StatusNotFound}
	}
	idp.Config = maps.Clone(idp.Config)
	if idp.Config["clientSecret"] == "**********" {
		idp.Config["clientSecret"] = existing.Config["clientSecret"]
	}
	f.idpUpdates++
	f.idps[idp.Alias] = idp
	return nil
}

func (f *fakeOrganizationStore) ImportIdentityProviderConfig(_ context.Context, fromURL string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, fromURL)
	if f.importErr != nil {
		return nil, f.importErr
	}
	issuer, ok := f.discoveredIssuers[fromURL]
	if !ok {
		issuer = strings.TrimSuffix(fromURL, "/.well-known/openid-configuration")
	}
	return map[string]string{"issuer": issuer}, nil
}

func (f *fakeOrganizationStore) ReloadIdentityProviderKeys(_ context.Context, alias string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadKeysCalls++
	if f.reloadKeysErr != nil {
		return false, f.reloadKeysErr
	}
	if _, ok := f.idps[alias]; !ok {
		return false, &keycloak.APIError{Op: "reload identity provider keys", StatusCode: http.StatusNotFound}
	}
	return !f.reloadKeysFalse, nil
}

func (f *fakeOrganizationStore) DeleteIdentityProvider(ctx context.Context, alias string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteIdPCalls++
	if f.deleteIdPErr != nil {
		return f.deleteIdPErr
	}
	if _, ok := f.idps[alias]; !ok {
		return nil // 404 treated as success by real client; tests may still call this
	}
	delete(f.idps, alias)
	delete(f.mappers, alias)
	// Keycloak drops the IdP's organization link with it.
	for orgID, linked := range f.links {
		if linked == alias {
			delete(f.links, orgID)
		}
	}
	return nil
}

func (f *fakeOrganizationStore) LinkOrganizationIdentityProvider(ctx context.Context, orgID, alias string) error {
	if f.linkHook != nil {
		f.linkHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil && f.linkIdPErr == nil {
		return err
	}
	if f.linkIdPErr != nil {
		return f.linkIdPErr
	}
	if _, ok := f.byID[orgID]; !ok {
		return &keycloak.APIError{Op: "link organization identity provider", StatusCode: http.StatusNotFound}
	}
	if _, ok := f.idps[alias]; !ok {
		return &keycloak.APIError{Op: "link organization identity provider", StatusCode: http.StatusBadRequest}
	}
	f.links[orgID] = alias
	return nil
}

func (f *fakeOrganizationStore) ConfigureOrganizationIdentityProvider(_ context.Context, orgID, alias string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configureErr != nil {
		return f.configureErr
	}
	if f.links[orgID] != alias {
		return &keycloak.APIError{Op: "configure organization identity provider", StatusCode: http.StatusNotFound}
	}
	return nil
}

func (f *fakeOrganizationStore) UpsertIdentityProviderMapper(_ context.Context, spec keycloak.IdentityProviderMapperSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mapperErr != nil {
		return f.mapperErr
	}
	if err := f.mapperErrOnce; err != nil {
		f.mapperErrOnce = nil
		return err
	}
	if _, ok := f.idps[spec.Alias]; !ok {
		return &keycloak.APIError{Op: "upsert identity provider mapper", StatusCode: http.StatusNotFound}
	}
	f.mapperCalls++
	f.mapperUpserts++
	names := []string{
		"AI Grid org id",
		"AI Grid org name",
		"AI Grid groups",
		"AI Grid roles",
		"AI Grid local username",
		"AI Grid preferred username",
	}
	f.mapperNames[spec.Alias] = nil
	for _, name := range names {
		if name == f.failMapperName {
			return &keycloak.APIError{Op: "upsert identity provider mapper", StatusCode: http.StatusBadGateway}
		}
		f.mapperNames[spec.Alias] = append(f.mapperNames[spec.Alias], name)
	}
	f.mappers[spec.Alias] = keycloak.IdentityProviderMapper{
		Name: "AI Grid org id", IdentityProviderAlias: spec.Alias,
		IdentityProviderMapper: "hardcoded-attribute-idp-mapper",
		Config:                 map[string]string{"attribute": "maas_org_id", "attribute.value": spec.OrganizationID, "syncMode": "FORCE"},
	}
	f.mapperOrgNames[spec.Alias] = spec.OrganizationName
	return nil
}

// ListIdentityProviderMappers lists the alias's mappers; the fake keeps the org id one.
func (f *fakeOrganizationStore) ListIdentityProviderMappers(_ context.Context, alias string) ([]keycloak.IdentityProviderMapper, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.idps[alias]; !ok {
		return nil, &keycloak.APIError{Op: "list identity provider mappers", StatusCode: http.StatusNotFound}
	}
	if m, ok := f.mappers[alias]; ok {
		return []keycloak.IdentityProviderMapper{m}, nil
	}
	return []keycloak.IdentityProviderMapper{}, nil
}

type fakeSecretReader struct {
	secrets map[string][]byte
	err     error
	// reads is every Secret name read, in order.
	reads []string
}

func (f *fakeSecretReader) Read(_ context.Context, name string) ([]byte, error) {
	f.reads = append(f.reads, name)
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.secrets[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	if len(v) == 0 {
		// As clientSecretFromSecret answers a Secret without a client-secret value.
		return nil, fmt.Errorf("secret %q key %q is empty: %w", name, idpClientSecretKey, errNoClientSecret)
	}
	return v, nil
}

func mustTenantsServer(t *testing.T, client organizationClient) *TenantsServer {
	t.Helper()
	server, err := NewTenantsServer().SetOrganizationClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return server
}

func mustTenantsServerWithSecrets(t *testing.T, client organizationClient, secrets SecretReader) *TenantsServer {
	t.Helper()
	server, err := NewTenantsServer().SetOrganizationClient(client).SetSecretReader(secrets).Build()
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

// assertConnectMessage is the documented exception to code-only error
// asserts: the console shows the backend's Connect message to admins
// (describeError), so the few tests whose purpose is "the cause survives to
// the wire" may match message text. errors.As failing is a test failure,
// never a skip.
func assertConnectMessage(t *testing.T, err error, want string) {
	t.Helper()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("error = %v (%T), want *connect.Error with message containing %q", err, err, want)
	}
	if !strings.Contains(connectErr.Message(), want) {
		t.Errorf("Connect message = %q, want it to contain %q", connectErr.Message(), want)
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
		got, err := server.Create(ctx, &aigridv1.TenantsCreateRequest{Object: &aigridv1.Tenant{Metadata: &aigridv1.Metadata{Name: "acme"}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}}}})
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

	t.Run("a create time outside the Timestamp range is nil", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore(
			keycloak.Organization{ID: "org-1", Name: "acme", Attributes: map[string][]string{tenantCreateTimeAttribute: {"0000-01-01T00:00:00Z"}}},
		))
		list, err := server.List(ctx, &aigridv1.TenantsListRequest{})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if ts := list.GetItems()[0].GetMetadata().GetCreationTimestamp(); ts != nil {
			t.Fatalf("creation timestamp = %v, want nil", ts)
		}
	})

	t.Run("a write waiting on the tenant lock gives up when its context ends", func(t *testing.T) {
		t.Parallel()
		server := mustTenantsServer(t, newFakeOrganizationStore(stamped("4")))
		unlock, err := server.lockTenant(ctx, "org-1")
		if err != nil {
			t.Fatalf("lockTenant() error = %v", err)
		}
		defer unlock()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = server.Update(canceled, update(4, true))
		assertConnectCode(t, err, connect.CodeCanceled)
		deadline, cancelDeadline := context.WithTimeout(ctx, time.Millisecond)
		defer cancelDeadline()
		_, err = server.Delete(deadline, &aigridv1.TenantsDeleteRequest{Id: "org-1", Version: 4})
		assertConnectCode(t, err, connect.CodeDeadlineExceeded)
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

	t.Run("an update or delete without a version is InvalidArgument and writes nothing", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		server := mustTenantsServer(t, store)
		for _, lock := range []bool{true, false} {
			_, err := server.Update(ctx, update(0, lock))
			assertConnectCode(t, err, connect.CodeInvalidArgument)
		}
		_, err := server.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: "org-1"})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		if stored, ok := store.byID["org-1"]; !ok || stored.Name != "acme" {
			t.Fatalf("a write without a version changed the store: %+v", stored)
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

	t.Run("a REST body that sends back version and creation time is accepted", func(t *testing.T) {
		t.Parallel()
		req := update(4, false)
		req.UpdateMask.Paths = append(req.UpdateMask.Paths, "metadata.version", "metadata.creation_timestamp")
		if _, err := mustTenantsServer(t, newFakeOrganizationStore(stamped("4"))).Update(ctx, req); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	})

	t.Run("removing the last domain is refused", func(t *testing.T) {
		t.Parallel()
		store := newFakeOrganizationStore(stamped("4"))
		before := slices.Clone(store.byID["org-1"].Domains)
		_, err := mustTenantsServer(t, store).Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: "org-1", Metadata: &aigridv1.Metadata{Version: 4}, Spec: &aigridv1.TenantSpec{}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		if store.puts != 0 || !reflect.DeepEqual(store.byID["org-1"].Domains, before) {
			t.Fatalf("puts=%d domains=%v, want no write and %v", store.puts, store.byID["org-1"].Domains, before)
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
			Object:     &aigridv1.Tenant{Id: "org-1", Metadata: &aigridv1.Metadata{Version: 4}, Spec: &aigridv1.TenantSpec{Domains: []string{"acme.example.com", "new.example.com"}}},
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
		assertConnectMessage(t, err, "already used by another organization")
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

func TestTenantIdentityProviderMessages(t *testing.T) {
	t.Parallel()

	enabled := true
	validate := false
	idp := &aigridv1.IdentityProvider{
		Title:       "Acme IdP",
		Description: "broker",
		Enabled:     &enabled,
		Config: &aigridv1.OidcConfig{
			ClientId:               "client",
			ClientSecretSecretName: "acme-idp-secret",
			Issuer:                 "https://idp.example.com",
			AuthorizationUrl:       "https://idp.example.com/auth",
			TokenUrl:               "https://idp.example.com/token",
			DefaultScopes:          "openid profile email",
			UserInfoUrl:            "https://idp.example.com/userinfo",
			JwksUrl:                "https://idp.example.com/jwks",
			LogoutUrl:              "https://idp.example.com/logout",
			ValidateSignature:      &validate,
		},
	}

	spec := &aigridv1.TenantSpec{IdentityProvider: idp}
	got := spec.GetIdentityProvider()
	if got == nil {
		t.Fatal("TenantSpec.IdentityProvider is nil")
	}
	if got.GetTitle() != "Acme IdP" || got.GetDescription() != "broker" {
		t.Fatalf("IdentityProvider title/description = %q / %q", got.GetTitle(), got.GetDescription())
	}
	if !got.GetEnabled() {
		t.Fatal("IdentityProvider.Enabled = false, want true via optional field")
	}
	cfg := got.GetConfig()
	if cfg == nil {
		t.Fatal("IdentityProvider.Config is nil")
	}
	if cfg.GetClientId() != "client" ||
		cfg.GetClientSecretSecretName() != "acme-idp-secret" ||
		cfg.GetIssuer() != "https://idp.example.com" ||
		cfg.GetAuthorizationUrl() != "https://idp.example.com/auth" ||
		cfg.GetTokenUrl() != "https://idp.example.com/token" ||
		cfg.GetDefaultScopes() != "openid profile email" ||
		cfg.GetUserInfoUrl() != "https://idp.example.com/userinfo" ||
		cfg.GetJwksUrl() != "https://idp.example.com/jwks" ||
		cfg.GetLogoutUrl() != "https://idp.example.com/logout" {
		t.Fatalf("OidcConfig = %+v", cfg)
	}
	if cfg.GetValidateSignature() {
		t.Fatal("OidcConfig.ValidateSignature = true, want false via optional field")
	}
}

func TestTenantIdentityProviderValidation(t *testing.T) {
	t.Parallel()

	valid := func() *aigridv1.IdentityProvider {
		return &aigridv1.IdentityProvider{
			Config: &aigridv1.OidcConfig{
				ClientId:               "client",
				ClientSecretSecretName: "acme-idp-secret",
				Issuer:                 "https://idp.example.com",
				AuthorizationUrl:       "https://idp.example.com/auth",
				TokenUrl:               "https://idp.example.com/token",
				JwksUrl:                "https://idp.example.com/jwks",
			},
		}
	}

	t.Run("normalizes defaults", func(t *testing.T) {
		t.Parallel()
		got, err := normalizeIdentityProvider("acme", valid())
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if got.GetTitle() != "acme" || !got.GetEnabled() || got.GetConfig().GetDefaultScopes() != defaultOIDCScopes || !got.GetConfig().GetValidateSignature() {
			t.Fatalf("normalized = %+v", got)
		}
	})

	t.Run("missing config", func(t *testing.T) {
		t.Parallel()
		_, err := normalizeIdentityProvider("acme", &aigridv1.IdentityProvider{})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("missing client id", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.ClientId = ""
		_, err := normalizeIdentityProvider("acme", idp)
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("missing secret name", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.ClientSecretSecretName = ""
		_, err := normalizeIdentityProvider("acme", idp)
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("invalid secret name", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.ClientSecretSecretName = "NOT_VALID"
		_, err := normalizeIdentityProvider("acme", idp)
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("missing required url", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.Issuer = ""
		_, err := normalizeIdentityProvider("acme", idp)
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("non-http url", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.Issuer = "ftp://idp.example.com"
		_, err := normalizeIdentityProvider("acme", idp)
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("overlong title", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Title = strings.Repeat("a", 254)
		_, err := normalizeIdentityProvider("acme", idp)
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("optional urls", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.UserInfoUrl = "https://idp.example.com/userinfo"
		idp.Config.LogoutUrl = "https://idp.example.com/logout"
		got, err := normalizeIdentityProvider("acme", idp)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if got.GetConfig().GetUserInfoUrl() == "" || got.GetConfig().GetLogoutUrl() == "" {
			t.Fatalf("optional urls = %+v", got.GetConfig())
		}
	})

	t.Run("jwks required when validating signatures", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.JwksUrl = ""
		_, err := normalizeIdentityProvider("acme", idp)
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		if err == nil || !strings.Contains(err.Error(), "jwks_url") {
			t.Fatalf("error = %v, want jwks_url", err)
		}
	})

	t.Run("jwks optional when signature validation is off", func(t *testing.T) {
		t.Parallel()
		idp := valid()
		idp.Config.JwksUrl = "  "
		validate := false
		idp.Config.ValidateSignature = &validate
		got, err := normalizeIdentityProvider("acme", idp)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if got.GetConfig().GetValidateSignature() || got.GetConfig().GetJwksUrl() != "" {
			t.Fatalf("config = %+v", got.GetConfig())
		}
	})
}

func TestTenantIdentityProviderPersistence(t *testing.T) {
	t.Parallel()
	enabled := true
	validate := true
	longBase := "https://idp.example.com/" + strings.Repeat("path/", 40) + "realm"
	idp := &aigridv1.IdentityProvider{
		Title: "acme", Description: "d", Enabled: &enabled,
		Config: &aigridv1.OidcConfig{
			ClientId: "c", ClientSecretSecretName: "sec",
			Issuer: longBase, AuthorizationUrl: longBase + "/auth", TokenUrl: longBase + "/token",
			JwksUrl: longBase + "/jwks", LogoutUrl: longBase + "/logout",
			DefaultScopes: defaultOIDCScopes, ValidateSignature: &validate,
		},
	}
	raw, err := identityProviderToAttribute(idp)
	if err != nil {
		t.Fatalf("to attribute: %v", err)
	}
	if len(raw) <= keycloakGroupAttributeMax {
		t.Fatalf("test IdP JSON too short (%d) to exercise Keycloak attribute chunking", len(raw))
	}
	if !strings.Contains(raw, "clientSecretSecretName") && !strings.Contains(raw, "client_secret_secret_name") {
		t.Fatalf("attribute missing secret name field: %s", raw)
	}
	if strings.Contains(raw, `"s3cret"`) {
		t.Fatalf("attribute leaked a secret value: %s", raw)
	}
	chunks, err := identityProviderToAttributeValues(idp)
	if err != nil {
		t.Fatalf("to attribute values: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected chunked attribute for long IdP JSON, got %d chunks (%d bytes)", len(chunks), len(raw))
	}
	for i, c := range chunks {
		if len(c) > keycloakGroupAttributeMax {
			t.Fatalf("chunk %d length %d exceeds %d", i, len(c), keycloakGroupAttributeMax)
		}
	}
	got, err := identityProviderFromOrganizationAttrs(map[string][]string{
		tenantIdentityProviderAttribute: chunks,
	})
	if err != nil {
		t.Fatalf("from attribute: %v", err)
	}
	if got.GetTitle() != idp.GetTitle() || got.GetConfig().GetClientSecretSecretName() != "sec" {
		t.Fatalf("round-trip = %+v", got)
	}
	nilIdP, err := identityProviderFromAttribute("")
	if err != nil || nilIdP != nil {
		t.Fatalf("absent = %v, %v", nilIdP, err)
	}
	if _, err := identityProviderFromAttribute("{"); err == nil {
		t.Fatal("malformed JSON expected error")
	}

	// Keycloak may return multi-valued attributes in any order. The index prefix
	// is what puts the JSON back together.
	reversed := append([]string(nil), chunks...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	reordered, err := identityProviderFromOrganizationAttrs(map[string][]string{
		tenantIdentityProviderAttribute: reversed,
	})
	if err != nil || reordered.GetConfig().GetClientSecretSecretName() != "sec" {
		t.Fatalf("reordered round-trip = %+v, err=%v", reordered, err)
	}

	// Writes from before the index prefix are still joined in stored order.
	legacy := chunkString(raw, 100)
	for _, c := range legacy {
		if len(c) >= chunkIndexPrefixLen && c[chunkIndexPrefixLen-1] == '|' && c[0] >= '0' && c[0] <= '9' {
			t.Fatalf("legacy chunk looks indexed: %q", c)
		}
	}
	legacyGot, err := identityProviderFromOrganizationAttrs(map[string][]string{
		tenantIdentityProviderAttribute: legacy,
	})
	if err != nil || legacyGot.GetTitle() != idp.GetTitle() {
		t.Fatalf("legacy round-trip = %+v, err=%v", legacyGot, err)
	}
}

func TestChunkStringKeepsRunesWhole(t *testing.T) {
	t.Parallel()
	const in = "aaa世bbb世"
	chunks := chunkString(in, 4)
	if strings.Join(chunks, "") != in {
		t.Fatalf("chunks %q joined to %q", chunks, strings.Join(chunks, ""))
	}
	for i, c := range chunks {
		if len(c) > 4 {
			t.Fatalf("chunk %d %q is %d bytes", i, c, len(c))
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is not valid UTF-8: %q", i, c)
		}
	}
	if strings.Contains(strings.Join(chunks, ""), "\uFFFD") {
		t.Fatal("chunk split a rune")
	}
}

func validCreateIdP() *aigridv1.IdentityProvider {
	return &aigridv1.IdentityProvider{
		Config: &aigridv1.OidcConfig{
			ClientId:               "client",
			ClientSecretSecretName: "acme-idp-secret",
			Issuer:                 "https://idp.example.com",
			AuthorizationUrl:       "https://idp.example.com/auth",
			TokenUrl:               "https://idp.example.com/token",
			JwksUrl:                "https://idp.example.com/jwks",
		},
	}
}
