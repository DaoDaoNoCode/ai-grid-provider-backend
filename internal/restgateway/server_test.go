package restgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/restgateway"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

var managedClustersResource = schema.GroupVersionResource{
	Group: "cluster.open-cluster-management.io", Version: "v1", Resource: "managedclusters",
}

var maasSubscriptionsResource = schema.GroupVersionResource{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maassubscriptions",
}

var maasAuthPoliciesResource = schema.GroupVersionResource{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasauthpolicies",
}

var maasModelRefsResource = schema.GroupVersionResource{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasmodelrefs",
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

func TestRESTGatewayRoutes(t *testing.T) {
	t.Parallel()

	baseURL := startGateway(t,
		[]runtime.Object{
			managedCluster("seed-cluster", map[string]string{"environment": "dev"}, true),
		},
		fakeOrganizationLister{
			orgs: []keycloak.Organization{
				{ID: "org-1", Name: "tenant-a", Domains: []keycloak.OrganizationDomain{{Name: "a.example.com"}}},
			},
		},
	)

	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	t.Run("ping", func(t *testing.T) {
		body := mustGET(t, client, baseURL+"/api/v1/ping?name=Ada")
		if !strings.Contains(body, "Welcome, Ada!") {
			t.Fatalf("ping body = %q", body)
		}
	})

	t.Run("list clusters", func(t *testing.T) {
		body := mustGET(t, client, baseURL+"/api/v1/clusters")
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("unmarshal list: %v (%s)", err, body)
		}
		if _, ok := payload["items"]; !ok {
			t.Fatalf("missing items: %s", body)
		}
		if payload["size"] == nil || payload["total"] == nil {
			t.Fatalf("missing size/total: %s", body)
		}
		if !strings.Contains(body, `"seed-cluster"`) {
			t.Fatalf("list missing seed-cluster: %s", body)
		}
	})

	t.Run("get cluster", func(t *testing.T) {
		body := mustGET(t, client, baseURL+"/api/v1/clusters/seed-cluster")
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("unmarshal get: %v (%s)", err, body)
		}
		if payload["id"] != "seed-cluster" {
			t.Fatalf("id = %v, want seed-cluster (%s)", payload["id"], body)
		}
		metadata, _ := payload["metadata"].(map[string]any)
		if metadata["name"] != "seed-cluster" {
			t.Fatalf("metadata.name = %v (%s)", metadata["name"], body)
		}
		spec, _ := payload["spec"].(map[string]any)
		if spec["hub_accepts_client"] != true {
			t.Fatalf("spec.hub_accepts_client = %v (%s)", spec["hub_accepts_client"], body)
		}
	})

	t.Run("list tenants", func(t *testing.T) {
		body := mustGET(t, client, baseURL+"/api/v1/tenants")
		if !strings.Contains(body, `"tenant-a"`) || !strings.Contains(body, `"org-1"`) {
			t.Fatalf("tenants body = %s", body)
		}
		if !strings.Contains(body, `"items"`) {
			t.Fatalf("missing items envelope: %s", body)
		}
	})

	t.Run("list subscriptions", func(t *testing.T) {
		body := mustGET(t, client, baseURL+"/api/v1/subscriptions")
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("unmarshal list: %v (%s)", err, body)
		}
		if payload["size"] != float64(0) || payload["total"] != float64(0) {
			t.Fatalf("size/total = %v/%v, want 0/0 (%s)", payload["size"], payload["total"], body)
		}
		items, _ := payload["items"].([]any)
		if len(items) != 0 {
			t.Fatalf("items len = %d, want 0 (%s)", len(items), body)
		}
	})

	t.Run("get subscription not found", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/api/v1/subscriptions/free-subscription")
		if err != nil {
			t.Fatalf("GET subscription: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, body)
		}
	})

	t.Run("list models", func(t *testing.T) {
		body := mustGET(t, client, baseURL+"/api/v1/models")
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("unmarshal list: %v (%s)", err, body)
		}
		if payload["size"] != float64(0) || payload["total"] != float64(0) {
			t.Fatalf("size/total = %v/%v, want 0/0 (%s)", payload["size"], payload["total"], body)
		}
		items, _ := payload["items"].([]any)
		if len(items) != 0 {
			t.Fatalf("items len = %d, want 0 (%s)", len(items), body)
		}
	})

	t.Run("get model not found", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/api/v1/models/missing-model")
		if err != nil {
			t.Fatalf("GET model: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, body)
		}
	})

	t.Run("openapi", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/openapi.json")
		if err != nil {
			t.Fatalf("GET openapi.json: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("Content-Type = %q", ct)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !bytes.Contains(body, []byte(`"swagger"`)) {
			t.Fatalf("openapi missing swagger: %s", body)
		}
	})

	t.Run("docs", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/docs/")
		if err != nil {
			t.Fatalf("GET /docs/: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !bytes.Contains(bytes.ToLower(body), []byte("html")) {
			t.Fatalf("docs body not html: %s", body)
		}

		redirect, err := client.Get(baseURL + "/docs")
		if err != nil {
			t.Fatalf("GET /docs: %v", err)
		}
		defer redirect.Body.Close()
		if redirect.StatusCode != http.StatusFound && redirect.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("/docs status = %d, want redirect", redirect.StatusCode)
		}
		if loc := redirect.Header.Get("Location"); !strings.HasSuffix(loc, "/docs/") {
			t.Fatalf("Location = %q, want .../docs/", loc)
		}
	})
}

func TestRESTClusterCreateDelete(t *testing.T) {
	t.Parallel()

	baseURL := startGateway(t, nil, fakeOrganizationLister{})
	client := &http.Client{Timeout: 10 * time.Second}
	const id = "rest-crud-demo"

	createBody := `{
		"id":"` + id + `",
		"metadata":{"name":"` + id + `","labels":{"environment":"smoke"}},
		"spec":{"hub_accepts_client":true}
	}`
	resp, err := client.Post(baseURL+"/api/v1/clusters", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	defer resp.Body.Close()
	created, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read create body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d body = %s", resp.StatusCode, created)
	}
	if !strings.Contains(string(created), `"`+id+`"`) {
		t.Fatalf("create body missing id: %s", created)
	}

	got := mustGET(t, client, baseURL+"/api/v1/clusters/"+id)
	if !strings.Contains(got, `"environment"`) || !strings.Contains(got, `"smoke"`) {
		t.Fatalf("get after create = %s", got)
	}

	delReq, err := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/clusters/"+id, nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(delResp.Body)
		t.Fatalf("delete status = %d body = %s", delResp.StatusCode, body)
	}

	missing, err := client.Get(baseURL + "/api/v1/clusters/" + id)
	if err != nil {
		t.Fatalf("GET after delete: %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(missing.Body)
		t.Fatalf("get after delete status = %d body = %s", missing.StatusCode, body)
	}
}

func TestRESTTenantCRUD(t *testing.T) {
	t.Parallel()

	baseURL := startGateway(t, nil, newFakeOrganizationStore())
	client := &http.Client{Timeout: 10 * time.Second}

	createBody := `{
		"metadata":{"name":"rest-tenant"},
		"spec":{"domains":["rest.example.com"]}
	}`
	resp, err := client.Post(baseURL+"/api/v1/tenants", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	defer resp.Body.Close()
	createdBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read create body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d body = %s", resp.StatusCode, createdBytes)
	}
	var created map[string]any
	if err := json.Unmarshal(createdBytes, &created); err != nil {
		t.Fatalf("unmarshal create: %v (%s)", err, createdBytes)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("missing id in create response: %s", createdBytes)
	}

	got := mustGET(t, client, baseURL+"/api/v1/tenants/"+id)
	if !strings.Contains(got, `"rest-tenant"`) || !strings.Contains(got, `"rest.example.com"`) {
		t.Fatalf("get after create = %s", got)
	}

	// The body sends back the version and creation time it read, as a client
	// round-tripping the GET response does.
	createdMeta, _ := created["metadata"].(map[string]any)
	if createdMeta["version"] != float64(1) || createdMeta["creation_timestamp"] == nil {
		t.Fatalf("create metadata = %v", createdMeta)
	}
	patchBody := `{
		"id":"` + id + `",
		"metadata":{"name":"rest-tenant","version":1,"creation_timestamp":"` + createdMeta["creation_timestamp"].(string) + `"},
		"spec":{"domains":["updated.example.com"]}
	}`
	patchReq, err := http.NewRequest(http.MethodPatch, baseURL+"/api/v1/tenants/"+id, strings.NewReader(patchBody))
	if err != nil {
		t.Fatalf("new patch request: %v", err)
	}
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp, err := client.Do(patchReq)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer patchResp.Body.Close()
	patched, err := io.ReadAll(patchResp.Body)
	if err != nil {
		t.Fatalf("read patch body: %v", err)
	}
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d body = %s", patchResp.StatusCode, patched)
	}
	if !strings.Contains(string(patched), `"updated.example.com"`) {
		t.Fatalf("patch body = %s", patched)
	}

	if !strings.Contains(string(patched), `"version":2`) {
		t.Fatalf("patch body = %s, want version 2", patched)
	}

	// A stale version: the PATCH again, and a DELETE at the read version.
	stale, err := http.NewRequest(http.MethodPatch, baseURL+"/api/v1/tenants/"+id, strings.NewReader(patchBody))
	if err != nil {
		t.Fatalf("new patch request: %v", err)
	}
	stale.Header.Set("Content-Type", "application/json")
	staleDel, err := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/tenants/"+id+"?version=1", nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	for _, req := range []*http.Request{stale, staleDel} {
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", req.Method, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s at a stale version: status = %d, want 409", req.Method, resp.StatusCode)
		}
	}

	delReq, err := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/tenants/"+id+"?version=2", nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(delResp.Body)
		t.Fatalf("delete status = %d body = %s", delResp.StatusCode, body)
	}

	missing, err := client.Get(baseURL + "/api/v1/tenants/" + id)
	if err != nil {
		t.Fatalf("GET after delete: %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(missing.Body)
		t.Fatalf("get after delete status = %d body = %s", missing.StatusCode, body)
	}
}

// organizationClient mirrors servers' Keycloak Organizations dependency for tests.
type organizationClient interface {
	ListOrganizations(ctx context.Context) ([]keycloak.Organization, error)
	GetOrganization(ctx context.Context, id string) (keycloak.Organization, error)
	CreateOrganization(ctx context.Context, org keycloak.Organization) (keycloak.Organization, error)
	UpdateOrganization(ctx context.Context, org keycloak.Organization) (keycloak.Organization, error)
	DeleteOrganization(ctx context.Context, id string) error
	ListOrganizationMembers(ctx context.Context, orgID string) ([]keycloak.OrganizationMember, error)
}

func startGateway(t *testing.T, objects []runtime.Object, orgs organizationClient) string {
	t.Helper()

	if objects == nil {
		objects = []runtime.Object{}
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			managedClustersResource:         "ManagedClusterList",
			maasSubscriptionsResource:       "MaaSSubscriptionList",
			maasModelRefsResource:           "MaaSModelRefList",
			maasAuthPoliciesResource:        "MaaSAuthPolicyList",
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

	server := connect.NewServer()
	aigridv1connect.RegisterAiGridHandler(server, aiGridServer)
	aigridv1connect.RegisterClustersHandler(server, clustersServer)
	aigridv1connect.RegisterTenantsHandler(server, tenantsServer)
	aigridv1connect.RegisterModelsHandler(server, modelsServer)
	aigridv1connect.RegisterSubscriptionsHandler(server, subscriptionsServer)

	gateway, err := restgateway.New("127.0.0.1:0", server)
	if err != nil {
		t.Fatalf("restgateway.New: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- gateway.Serve()
	}()

	baseURL := "http://" + gateway.Addr()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/openapi.json")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway did not become ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gateway.Shutdown(shutdownCtx)
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	})
	return baseURL
}

func mustGET(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d body = %s", url, resp.StatusCode, body)
	}
	return string(body)
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

type fakeOrganizationStore struct {
	byID map[string]keycloak.Organization
	next int
}

func newFakeOrganizationStore(orgs ...keycloak.Organization) *fakeOrganizationStore {
	store := &fakeOrganizationStore{byID: make(map[string]keycloak.Organization, len(orgs))}
	for _, org := range orgs {
		store.byID[org.ID] = org
	}
	return store
}

func (f *fakeOrganizationStore) ListOrganizations(context.Context) ([]keycloak.Organization, error) {
	out := make([]keycloak.Organization, 0, len(f.byID))
	for _, org := range f.byID {
		out = append(out, org)
	}
	return out, nil
}

func (f *fakeOrganizationStore) GetOrganization(_ context.Context, id string) (keycloak.Organization, error) {
	org, ok := f.byID[id]
	if !ok {
		return keycloak.Organization{}, &keycloak.APIError{Op: "get organization", StatusCode: http.StatusNotFound}
	}
	return org, nil
}

func (f *fakeOrganizationStore) CreateOrganization(_ context.Context, org keycloak.Organization) (keycloak.Organization, error) {
	for _, existing := range f.byID {
		if existing.Name == org.Name {
			return keycloak.Organization{}, &keycloak.APIError{Op: "create organization", StatusCode: http.StatusConflict}
		}
	}
	f.next++
	org.ID = "org-" + strings.TrimSpace(org.Name)
	if org.Alias == "" {
		org.Alias = org.Name
	}
	f.byID[org.ID] = org
	return org, nil
}

func (f *fakeOrganizationStore) UpdateOrganization(_ context.Context, org keycloak.Organization) (keycloak.Organization, error) {
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

func (f *fakeOrganizationStore) DeleteOrganization(_ context.Context, id string) error {
	if _, ok := f.byID[id]; !ok {
		return &keycloak.APIError{Op: "delete organization", StatusCode: http.StatusNotFound}
	}
	delete(f.byID, id)
	return nil
}

func (f *fakeOrganizationStore) ListOrganizationMembers(context.Context, string) ([]keycloak.OrganizationMember, error) {
	return nil, nil
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
