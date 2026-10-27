package client_test

import (
	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"context"
	"errors"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"net"
	"net/http"
	"testing"

	clientpkg "github.com/osac-project/ai-grid-provider-backend/client"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
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

var maasModelRefsResource = schema.GroupVersionResource{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasmodelrefs",
}
var maasSubscriptionsResource = schema.GroupVersionResource{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maassubscriptions",
}
var maasAuthPoliciesResource = schema.GroupVersionResource{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasauthpolicies",
}
var llmInferenceServicesResource = schema.GroupVersionResource{
	Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices",
}
var placementsResource = schema.GroupVersionResource{
	Group: "cluster.open-cluster-management.io", Version: "v1beta1", Resource: "placements",
}
var placementDecisionsResource = schema.GroupVersionResource{
	Group: "cluster.open-cluster-management.io", Version: "v1beta1", Resource: "placementdecisions",
}
var manifestWorkReplicaSetsResource = schema.GroupVersionResource{
	Group: "work.open-cluster-management.io", Version: "v1alpha1", Resource: "manifestworkreplicasets",
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
			Labels: map[string]string{"location": "staging", "ai-grid/enabled": "true"},
		},
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: false},
	}, nil)
	if err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}
	if updated.GetMetadata().GetLabels()["location"] != "staging" {
		t.Fatalf("updated location = %q", updated.GetMetadata().GetLabels()["location"])
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
			Labels: map[string]string{"location": "prod"},
		},
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
	}, &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}})
	if err != nil {
		t.Fatalf("UpdateCluster(mask) error = %v", err)
	}
	if masked.GetMetadata().GetLabels()["location"] != "prod" {
		t.Fatalf("masked location = %q", masked.GetMetadata().GetLabels()["location"])
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
		managedCluster("existing", map[string]string{"location": "prod"}, true),
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

func TestClientModelsCRUD(t *testing.T) {
	t.Parallel()

	c, cleanup := startTestClient(t, nil, fakeOrganizationLister{})
	defer cleanup()
	ctx := context.Background()

	items, err := c.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ListModels() len = %d, want 0", len(items))
	}

	replicas := int32(1)
	created, err := c.CreateModel(ctx, &aigridv1.Model{
		Id: "client-opt",
		Metadata: &aigridv1.Metadata{
			Name: "client-opt",
			Annotations: map[string]string{
				"openshift.io/display-name": "Client OPT",
			},
		},
		Spec: &aigridv1.ModelSpec{
			Model: &aigridv1.ModelSource{
				Uri:  "hf://facebook/opt-125m",
				Name: "facebook/opt-125m",
			},
			Replicas: &replicas,
			Main: &aigridv1.ModelContainer{
				Image:    "quay.io/example/vllm:test",
				Requests: map[string]string{"cpu": "100m", "memory": "1Gi"},
				Limits:   map[string]string{"cpu": "1", "memory": "2Gi"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}
	if created.GetId() != "client-opt" {
		t.Fatalf("CreateModel id = %q", created.GetId())
	}

	got, err := c.GetModel(ctx, "client-opt")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got.GetSpec().GetModel().GetUri() != "hf://facebook/opt-125m" {
		t.Fatalf("uri = %q", got.GetSpec().GetModel().GetUri())
	}

	replicas = 2
	got.Spec.Replicas = &replicas
	updated, err := c.UpdateModel(ctx, got, &fieldmaskpb.FieldMask{Paths: []string{"spec.replicas"}})
	if err != nil {
		t.Fatalf("UpdateModel() error = %v", err)
	}
	if updated.GetSpec().GetReplicas() != 2 {
		t.Fatalf("updated replicas = %v", updated.GetSpec().Replicas)
	}

	if err := c.DeleteModel(ctx, "client-opt"); err != nil {
		t.Fatalf("DeleteModel() error = %v", err)
	}
	_, err = c.GetModel(ctx, "client-opt")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Get after delete code = %v (%v)", status.Code(err), err)
	}
}

func TestClientSubscriptionsCRUD(t *testing.T) {
	t.Parallel()

	c, cleanup := startTestClient(t, nil, fakeOrganizationLister{})
	defer cleanup()
	ctx := context.Background()

	items, err := c.ListSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListSubscriptions() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ListSubscriptions() len = %d, want 0", len(items))
	}

	created, err := c.CreateSubscription(ctx, &aigridv1.Subscription{
		Id:       "free-subscription",
		Metadata: &aigridv1.Metadata{Name: "free-subscription"},
		Spec: &aigridv1.SubscriptionSpec{
			Owner: &aigridv1.SubscriptionOwner{Groups: []string{"system:authenticated"}},
			ModelRefs: []*aigridv1.SubscriptionModelRef{
				{Name: "sim-model", TokenRateLimits: []*aigridv1.TokenRateLimit{{Limit: 100, Window: "1m"}}},
			},
			Priority: 10,
		},
	})
	if err != nil {
		t.Fatalf("CreateSubscription() error = %v", err)
	}
	if created.GetId() != "free-subscription" {
		t.Fatalf("CreateSubscription id = %q", created.GetId())
	}

	got, err := c.GetSubscription(ctx, "free-subscription")
	if err != nil {
		t.Fatalf("GetSubscription() error = %v", err)
	}
	if got.GetSpec().GetPriority() != 10 {
		t.Fatalf("priority = %d", got.GetSpec().GetPriority())
	}

	got.Spec.Priority = 20
	updated, err := c.UpdateSubscription(ctx, got, nil)
	if err != nil {
		t.Fatalf("UpdateSubscription() error = %v", err)
	}
	if updated.GetSpec().GetPriority() != 20 {
		t.Fatalf("updated priority = %d", updated.GetSpec().GetPriority())
	}

	if err := c.DeleteSubscription(ctx, "free-subscription"); err != nil {
		t.Fatalf("DeleteSubscription() error = %v", err)
	}
	_, err = c.GetSubscription(ctx, "free-subscription")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Get after delete code = %v (%v)", status.Code(err), err)
	}
}

func TestClientTenantCRUD(t *testing.T) {
	t.Parallel()

	store := &mutableOrganizationStore{byID: map[string]keycloak.Organization{}}
	c, cleanup := startTestClient(t, nil, store)
	defer cleanup()
	ctx := context.Background()

	created, err := c.CreateTenant(ctx, &aigridv1.Tenant{
		Metadata: &aigridv1.Metadata{Name: "acme"},
		Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}},
	})
	if err != nil {
		t.Fatalf("CreateTenant() error = %v", err)
	}
	if created.GetId() == "" || created.GetMetadata().GetName() != "acme" {
		t.Fatalf("created = %+v", created)
	}

	got, err := c.GetTenant(ctx, created.GetId())
	if err != nil {
		t.Fatalf("GetTenant() error = %v", err)
	}
	if got.GetMetadata().GetName() != "acme" {
		t.Fatalf("GetTenant name = %q", got.GetMetadata().GetName())
	}

	updated, err := c.UpdateTenant(ctx, &aigridv1.Tenant{
		Id:       created.GetId(),
		Metadata: &aigridv1.Metadata{Name: "acme", Version: got.GetMetadata().GetVersion()},
		Spec:     &aigridv1.TenantSpec{Domains: []string{"updated.example.com"}},
	}, nil)
	if err != nil {
		t.Fatalf("UpdateTenant() error = %v", err)
	}
	if domains := updated.GetSpec().GetDomains(); len(domains) != 1 || domains[0] != "updated.example.com" {
		t.Fatalf("domains = %v", domains)
	}

	if err := c.DeleteTenant(ctx, created.GetId(), updated.GetMetadata().GetVersion()); err != nil {
		t.Fatalf("DeleteTenant() error = %v", err)
	}
	_, err = c.GetTenant(ctx, created.GetId())
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Get after delete code = %v (%v)", status.Code(err), err)
	}
}

type organizationClient interface {
	ListOrganizations(ctx context.Context) ([]keycloak.Organization, error)
	GetOrganization(ctx context.Context, id string) (keycloak.Organization, error)
	CreateOrganization(ctx context.Context, org keycloak.Organization) (keycloak.Organization, error)
	UpdateOrganization(ctx context.Context, org keycloak.Organization) (keycloak.Organization, error)
	DeleteOrganization(ctx context.Context, id string) error
	ListOrganizationMembers(ctx context.Context, orgID string) ([]keycloak.OrganizationMember, error)
}

type mutableOrganizationStore struct {
	byID map[string]keycloak.Organization
	next int
}

func (f *mutableOrganizationStore) ListOrganizations(context.Context) ([]keycloak.Organization, error) {
	out := make([]keycloak.Organization, 0, len(f.byID))
	for _, org := range f.byID {
		out = append(out, org)
	}
	return out, nil
}

func (f *mutableOrganizationStore) GetOrganization(_ context.Context, id string) (keycloak.Organization, error) {
	org, ok := f.byID[id]
	if !ok {
		return keycloak.Organization{}, &keycloak.APIError{Op: "get organization", StatusCode: http.StatusNotFound}
	}
	return org, nil
}

func (f *mutableOrganizationStore) CreateOrganization(_ context.Context, org keycloak.Organization) (keycloak.Organization, error) {
	for _, existing := range f.byID {
		if existing.Name == org.Name {
			return keycloak.Organization{}, &keycloak.APIError{Op: "create organization", StatusCode: http.StatusConflict}
		}
	}
	f.next++
	org.ID = "org-" + org.Name
	if org.Alias == "" {
		org.Alias = org.Name
	}
	f.byID[org.ID] = org
	return org, nil
}

func (f *mutableOrganizationStore) UpdateOrganization(_ context.Context, org keycloak.Organization) (keycloak.Organization, error) {
	existing, ok := f.byID[org.ID]
	if !ok {
		return keycloak.Organization{}, &keycloak.APIError{Op: "update organization", StatusCode: http.StatusNotFound}
	}
	if org.Alias == "" {
		org.Alias = existing.Alias
	}
	f.byID[org.ID] = org
	return org, nil
}

func (f *mutableOrganizationStore) DeleteOrganization(_ context.Context, id string) error {
	if _, ok := f.byID[id]; !ok {
		return &keycloak.APIError{Op: "delete organization", StatusCode: http.StatusNotFound}
	}
	delete(f.byID, id)
	return nil
}

func (f *mutableOrganizationStore) ListOrganizationMembers(context.Context, string) ([]keycloak.OrganizationMember, error) {
	return nil, nil
}

func startTestClient(t *testing.T, objects []runtime.Object, orgs organizationClient) (*clientpkg.Client, func()) {
	t.Helper()

	if objects == nil {
		objects = []runtime.Object{}
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			managedClustersResource:         "ManagedClusterList",
			maasSubscriptionsResource:       "MaaSSubscriptionList",
			maasAuthPoliciesResource:        "MaaSAuthPolicyList",
			maasModelRefsResource:           "MaaSModelRefList",
			llmInferenceServicesResource:    "LLMInferenceServiceList",
			placementsResource:              "PlacementList",
			placementDecisionsResource:      "PlacementDecisionList",
			manifestWorkReplicaSetsResource: "ManifestWorkReplicaSetList",
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
		SetOrganizationClient(orgs).
		Build()
	if err != nil {
		t.Fatalf("build Tenants server: %v", err)
	}
	modelsServer, err := servers.NewModelsServer().
		SetDynamicClient(dynamicClient).
		SetManifestWorkReplicaSetEnabled(true).
		Build()
	if err != nil {
		t.Fatalf("build Models server: %v", err)
	}
	subscriptionsServer, err := servers.NewSubscriptionsServer().
		SetDynamicClient(dynamicClient).
		Build()
	if err != nil {
		t.Fatalf("build Subscriptions server: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// client/ is a grpc-go client; it reaches the connect handlers over plaintext
	// HTTP/2, exactly as it reaches the deployed service.
	server := connect.NewServer()
	aigridv1connect.RegisterAiGridHandler(server, aiGridServer)
	aigridv1connect.RegisterClustersHandler(server, clustersServer)
	aigridv1connect.RegisterTenantsHandler(server, tenantsServer)
	aigridv1connect.RegisterModelsHandler(server, modelsServer)
	aigridv1connect.RegisterSubscriptionsHandler(server, subscriptionsServer)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	apiServer := h2cServer(mux)
	go func() {
		_ = apiServer.Serve(listener)
	}()

	client, err := clientpkg.NewBuilder().
		SetAddress(listener.Addr().String()).
		SetPlaintext(true).
		Build(context.Background())
	if err != nil {
		_ = apiServer.Close()
		_ = listener.Close()
		t.Fatalf("build client: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
		_ = apiServer.Close()
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

func (f fakeOrganizationLister) GetOrganization(_ context.Context, id string) (keycloak.Organization, error) {
	for _, org := range f.orgs {
		if org.ID == id {
			return org, nil
		}
	}
	return keycloak.Organization{}, &keycloak.APIError{Op: "get organization", StatusCode: http.StatusNotFound}
}

func (f fakeOrganizationLister) CreateOrganization(context.Context, keycloak.Organization) (keycloak.Organization, error) {
	return keycloak.Organization{}, errors.New("CreateOrganization not implemented in list-only fake")
}

func (f fakeOrganizationLister) UpdateOrganization(context.Context, keycloak.Organization) (keycloak.Organization, error) {
	return keycloak.Organization{}, errors.New("UpdateOrganization not implemented in list-only fake")
}

func (f fakeOrganizationLister) DeleteOrganization(context.Context, string) error {
	return errors.New("DeleteOrganization not implemented in list-only fake")
}

func (f fakeOrganizationLister) ListOrganizationMembers(context.Context, string) ([]keycloak.OrganizationMember, error) {
	return nil, errors.New("ListOrganizationMembers not implemented in list-only fake")
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

// h2cServer serves plaintext HTTP/2, which is what a gRPC client speaks and what
// the deployed service listens for.
func h2cServer(handler http.Handler) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{Handler: handler, Protocols: protocols}
}
