package client_test

import (
	"context"
	"net"
	"testing"

	clientpkg "github.com/osac-project/ai-grid-provider-backend/client"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

var managedClustersResource = schema.GroupVersionResource{
	Group: "cluster.open-cluster-management.io", Version: "v1", Resource: "managedclusters",
}

func TestBuilderValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if _, err := clientpkg.NewBuilder().SetPlaintext(true).Build(ctx); err == nil {
		t.Fatal("Build() without address succeeded, want error")
	}
	if _, err := clientpkg.NewBuilder().SetAddress("localhost:1").Build(ctx); err == nil {
		t.Fatal("Build() without plaintext succeeded, want error")
	}
}

func TestClientPing(t *testing.T) {
	t.Parallel()

	c, cleanup := startTestClient(t, nil, fakeOrganizationLister{})
	defer cleanup()

	got, err := c.Ping(context.Background(), "Ada")
	if err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if got != "Welcome, Ada!" {
		t.Fatalf("Ping() = %q, want Welcome, Ada!", got)
	}
}

func TestClientClusterCRUD(t *testing.T) {
	t.Parallel()

	c, cleanup := startTestClient(t, nil, fakeOrganizationLister{})
	defer cleanup()
	ctx := context.Background()
	const id = "client-crud-demo"

	created, err := c.CreateCluster(ctx, &aigridv1.ManagedCluster{
		Id: id,
		Metadata: &aigridv1.Metadata{
			Name:   id,
			Labels: map[string]string{"environment": "dev"},
		},
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
	})
	if err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if created.GetId() != id {
		t.Fatalf("created id = %q, want %q", created.GetId(), id)
	}
	if created.GetMetadata().GetLabels()["environment"] != "dev" {
		t.Fatalf("created labels = %v", created.GetMetadata().GetLabels())
	}
	if !created.GetSpec().GetHubAcceptsClient() {
		t.Fatal("created hubAcceptsClient = false, want true")
	}

	listed, err := c.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters() error = %v", err)
	}
	if !containsManagedCluster(listed, id) {
		t.Fatalf("ListClusters() missing %q; got %v", id, managedClusterIDs(listed))
	}

	got, err := c.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster() error = %v", err)
	}
	if got.GetMetadata().GetLabels()["environment"] != "dev" {
		t.Fatalf("get labels = %v", got.GetMetadata().GetLabels())
	}

	updated, err := c.UpdateCluster(ctx, &aigridv1.ManagedCluster{
		Id: id,
		Metadata: &aigridv1.Metadata{
			Labels: map[string]string{"environment": "staging", "ai-grid/enabled": "true"},
		},
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: false},
	}, nil)
	if err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}
	if updated.GetMetadata().GetLabels()["environment"] != "staging" {
		t.Fatalf("updated environment = %q", updated.GetMetadata().GetLabels()["environment"])
	}
	if updated.GetMetadata().GetLabels()["ai-grid/enabled"] != "true" {
		t.Fatalf("updated ai-grid/enabled = %q", updated.GetMetadata().GetLabels()["ai-grid/enabled"])
	}
	if updated.GetSpec().GetHubAcceptsClient() {
		t.Fatal("updated hubAcceptsClient = true, want false")
	}

	masked, err := c.UpdateCluster(ctx, &aigridv1.ManagedCluster{
		Id: id,
		Metadata: &aigridv1.Metadata{
			Labels: map[string]string{"environment": "prod"},
		},
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
	}, &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}})
	if err != nil {
		t.Fatalf("UpdateCluster(mask) error = %v", err)
	}
	if masked.GetMetadata().GetLabels()["environment"] != "prod" {
		t.Fatalf("masked environment = %q", masked.GetMetadata().GetLabels()["environment"])
	}
	if masked.GetSpec().GetHubAcceptsClient() {
		t.Fatal("hubAcceptsClient changed despite labels-only mask")
	}

	if err := c.DeleteCluster(ctx, id); err != nil {
		t.Fatalf("DeleteCluster() error = %v", err)
	}
	_, err = c.GetCluster(ctx, id)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Get after delete code = %v, want NotFound (%v)", status.Code(err), err)
	}
}

func TestClientClusterErrors(t *testing.T) {
	t.Parallel()

	c, cleanup := startTestClient(t, []runtime.Object{
		managedCluster("existing", map[string]string{"environment": "prod"}, true),
	}, fakeOrganizationLister{})
	defer cleanup()
	ctx := context.Background()

	_, err := c.GetCluster(ctx, "missing")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Get missing code = %v, want NotFound (%v)", status.Code(err), err)
	}

	_, err = c.CreateCluster(ctx, &aigridv1.ManagedCluster{
		Id:   "existing",
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("Create duplicate code = %v, want AlreadyExists (%v)", status.Code(err), err)
	}

	_, err = c.CreateCluster(ctx, &aigridv1.ManagedCluster{
		Id:   "INVALID_NAME",
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Create invalid id code = %v, want InvalidArgument (%v)", status.Code(err), err)
	}
}

func TestClientListTenants(t *testing.T) {
	t.Parallel()

	c, cleanup := startTestClient(t, nil, fakeOrganizationLister{
		orgs: []keycloak.Organization{
			{ID: "org-1", Name: "tenant-a", Domains: []keycloak.OrganizationDomain{{Name: "a.example.com"}}},
			{ID: "org-2", Name: "tenant-b", Domains: []keycloak.OrganizationDomain{{Name: "b.example.com"}}},
		},
	})
	defer cleanup()

	tenants, err := c.ListTenants(context.Background())
	if err != nil {
		t.Fatalf("ListTenants() error = %v", err)
	}
	if len(tenants) != 2 {
		t.Fatalf("got %d tenants, want 2", len(tenants))
	}
	byName := map[string]*aigridv1.Tenant{}
	for _, tenant := range tenants {
		byName[tenant.GetMetadata().GetName()] = tenant
	}
	if byName["tenant-a"].GetId() != "org-1" {
		t.Fatalf("tenant-a id = %q", byName["tenant-a"].GetId())
	}
	if got := byName["tenant-b"].GetSpec().GetDomains(); len(got) != 1 || got[0] != "b.example.com" {
		t.Fatalf("tenant-b domains = %v", got)
	}
}

func startTestClient(t *testing.T, objects []runtime.Object, orgs fakeOrganizationLister) (*clientpkg.Client, func()) {
	t.Helper()

	if objects == nil {
		objects = []runtime.Object{}
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			managedClustersResource: "ManagedClusterList",
		},
		objects...,
	)

	aiGridServer, err := servers.NewAiGridServer().Build()
	if err != nil {
		t.Fatalf("build AiGrid server: %v", err)
	}
	clustersServer, err := servers.NewClustersServer().
		SetDynamicClient(dynamicClient).
		Build()
	if err != nil {
		t.Fatalf("build Clusters server: %v", err)
	}
	tenantsServer, err := servers.NewTenantsServer().
		SetOrganizationLister(orgs).
		Build()
	if err != nil {
		t.Fatalf("build Tenants server: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	aigridv1.RegisterAiGridServer(grpcServer, aiGridServer)
	aigridv1.RegisterClustersServer(grpcServer, clustersServer)
	aigridv1.RegisterTenantsServer(grpcServer, tenantsServer)
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	client, err := clientpkg.NewBuilder().
		SetAddress(listener.Addr().String()).
		SetPlaintext(true).
		Build(context.Background())
	if err != nil {
		grpcServer.Stop()
		_ = listener.Close()
		t.Fatalf("build client: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
		grpcServer.Stop()
		_ = listener.Close()
	}
	return client, cleanup
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

func containsManagedCluster(clusters []*aigridv1.ManagedCluster, id string) bool {
	for _, cluster := range clusters {
		if cluster.GetId() == id {
			return true
		}
	}
	return false
}

func managedClusterIDs(clusters []*aigridv1.ManagedCluster) []string {
	ids := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		ids = append(ids, cluster.GetId())
	}
	return ids
}
