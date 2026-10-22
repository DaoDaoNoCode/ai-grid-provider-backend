package servers

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/restgateway"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// TestEveryProtocol drives one set of handlers over all four protocols the
// service speaks: Connect (what a browser uses), gRPC, gRPC-Web and the REST
// surface vanguard derives from the google.api.http annotations.
func TestEveryProtocol(t *testing.T) {
	t.Parallel()

	baseURL, httpClient := startBackend(t, managedCluster("cluster-a", map[string]string{"environment": "production"}, true))

	rpcProtocols := []struct {
		name string
		opts []connecthttp.Option
	}{
		{name: "connect"},
		{name: "grpc", opts: []connecthttp.Option{connecthttp.WithGRPC()}},
		{name: "grpc-web", opts: []connecthttp.Option{connecthttp.WithGRPCWeb()}},
	}

	for _, protocol := range rpcProtocols {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()

			client := connect.NewClient(connecthttp.NewTransport(httpClient, baseURL, protocol.opts...))

			ping, err := aigridv1connect.NewAiGridClient(client).Ping(context.Background(), &aigridv1.AiGridPingRequest{Name: "Ada"})
			if err != nil {
				t.Fatalf("Ping() error = %v", err)
			}
			if ping.GetMessage() != "Welcome, Ada!" {
				t.Fatalf("Ping() message = %q, want %q", ping.GetMessage(), "Welcome, Ada!")
			}

			clusters := aigridv1connect.NewClustersClient(client)
			got, err := clusters.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: "cluster-a"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.GetObject().GetMetadata().GetLabels()["environment"] != "production" {
				t.Fatalf("labels = %v", got.GetObject().GetMetadata().GetLabels())
			}

			// The code and the message have to survive the wire: connect-go
			// sends an uncoded error as "unknown" with no message at all.
			_, err = clusters.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: "missing"})
			assertConnectCode(t, err, connect.CodeNotFound)
			if want := `ManagedCluster "missing" not found`; !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want substring %q", err, want)
			}

			_, err = clusters.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: "INVALID_NAME"})
			assertConnectCode(t, err, connect.CodeInvalidArgument)
		})
	}

	t.Run("rest", func(t *testing.T) {
		t.Parallel()

		body, status := get(t, httpClient, baseURL+"/api/v1/clusters/cluster-a")
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %s", status, body)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, body)
		}
		if payload["id"] != "cluster-a" {
			t.Fatalf("id = %v (%s)", payload["id"], body)
		}
		// Proto field names, as the grpc-gateway marshaler answered with.
		spec, _ := payload["spec"].(map[string]any)
		if spec["hub_accepts_client"] != true {
			t.Fatalf("spec.hub_accepts_client = %v (%s)", spec["hub_accepts_client"], body)
		}

		body, status = get(t, httpClient, baseURL+"/api/v1/clusters/missing")
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body = %s", status, body)
		}
		if want := `ManagedCluster \"missing\" not found`; !strings.Contains(body, want) {
			t.Fatalf("body = %s, want substring %q", body, want)
		}
	})
}

func startBackend(t *testing.T, objects ...runtime.Object) (string, connecthttp.HTTPClient) {
	t.Helper()
	return startBackendWithTenants(t, newFakeOrganizationStore(), objects...)
}

// startBackendWithTenants registers every service the binary does. Tenants is
// registered too: without it nothing here exercises the Tenants handlers over
// the wire, which is how a plain Go error reaching a client went unnoticed.
func startBackendWithTenants(t *testing.T, tenants organizationClient, objects ...runtime.Object) (string, connecthttp.HTTPClient) {
	t.Helper()

	aiGridServer, err := NewAiGridServer().Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	server := connect.NewServer()
	aigridv1connect.RegisterAiGridHandler(server, aiGridServer)
	aigridv1connect.RegisterClustersHandler(server, mustClustersServer(t, newManagedClusterClient(objects...)))
	aigridv1connect.RegisterTenantsHandler(server, mustTenantsServer(t, tenants))

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	if err := restgateway.Mount(mux, server); err != nil {
		t.Fatalf("restgateway.Mount() error = %v", err)
	}

	backend := httptest.NewUnstartedServer(mux)
	backend.EnableHTTP2 = true
	backend.Config.Protocols = new(http.Protocols)
	backend.Config.Protocols.SetHTTP1(true)
	backend.Config.Protocols.SetUnencryptedHTTP2(true)
	backend.Start()
	t.Cleanup(backend.Close)

	// Plaintext HTTP/2, which is what the deployed service listens for. The
	// standard library handles this now; x/net/http2.Transport is deprecated.
	clientProtocols := new(http.Protocols)
	clientProtocols.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{Transport: &http.Transport{Protocols: clientProtocols}}
	return backend.URL, httpClient
}

func get(t *testing.T, client connecthttp.HTTPClient, url string) (string, int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body), response.StatusCode
}

// An unmapped Kubernetes failure must still reach the caller with its message.
//
// connect-go v2 scrubs an uncoded handler error to "unknown" with an empty wire message,
// where grpc-go serialized the text. mapManagedClusterAPIError's default therefore carries
// CodeInternal: without it an etcd timeout or a dropped API server arrives in the console
// blank, on exactly the path someone is debugging when something surprising breaks.
func TestUnmappedKubernetesErrorKeepsItsMessage(t *testing.T) {
	t.Parallel()

	dynamicClient := newManagedClusterClient()
	dynamicClient.PrependReactor("get", "managedclusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServerTimeout(
			schema.GroupResource{Group: "cluster.open-cluster-management.io", Resource: "managedclusters"}, "get", 1)
	})

	server := connect.NewServer()
	aigridv1connect.RegisterClustersHandler(server, mustClustersServer(t, dynamicClient))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)

	backend := httptest.NewUnstartedServer(mux)
	backend.EnableHTTP2 = true
	backend.Config.Protocols = new(http.Protocols)
	backend.Config.Protocols.SetHTTP1(true)
	backend.Config.Protocols.SetUnencryptedHTTP2(true)
	backend.Start()
	t.Cleanup(backend.Close)

	client := aigridv1connect.NewClustersClient(
		connect.NewClient(connecthttp.NewTransport(backend.Client(), backend.URL)))

	_, err := client.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: "cluster-a"})
	if err == nil {
		t.Fatal("Get() error = nil, want the server timeout to surface")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("code = %v, want %v", got, connect.CodeInternal)
	}
	// The assertion that matters: an uncoded error would arrive empty here.
	if !strings.Contains(err.Error(), "ManagedCluster") {
		t.Errorf("error = %q, want it to name the resource; an uncoded error is scrubbed to an empty message", err)
	}
}

// A backend failure on a list path must reach the caller with its message.
// connect-go v2 scrubs an uncoded handler error to "unknown" with an empty wire
// message, so a plain fmt.Errorf here showed the console a blank error. Covers
// both list paths, which had no wire-level coverage at all.
func TestListFailuresKeepTheirMessage(t *testing.T) {
	t.Parallel()

	store := newFakeOrganizationStore()
	store.listErr = errors.New("keycloak is unreachable")
	baseURL, httpClient := startBackendWithTenants(t, store)
	client := connect.NewClient(connecthttp.NewTransport(httpClient, baseURL))

	_, err := aigridv1connect.NewTenantsClient(client).List(context.Background(), &aigridv1.TenantsListRequest{})
	if err == nil {
		t.Fatal("Tenants.List() error = nil, want the Keycloak failure to surface")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("code = %v, want %v", got, connect.CodeInternal)
	}
	if !strings.Contains(err.Error(), "keycloak is unreachable") {
		t.Errorf("error = %q, want the cause in the message; an uncoded error arrives empty", err)
	}
}

// grpc-gateway skipped a query parameter it could not map. vanguard rejects one
// unless told otherwise, so a cache-buster or a tracking parameter would turn a
// working request into a 400.
func TestRESTIgnoresUnknownQueryParameters(t *testing.T) {
	t.Parallel()

	baseURL, httpClient := startBackend(t, managedCluster("cluster-a", nil, true))

	body, status := get(t, httpClient, baseURL+"/api/v1/clusters?_=1730000000&utm_source=test")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown query parameters must be ignored), body = %s", status, body)
	}
	if !strings.Contains(body, "cluster-a") {
		t.Errorf("body = %s, want it to list cluster-a", body)
	}
}

// A custom verb (":listMembers") is a path shape net/http cannot route and a
// transcoder has to handle itself. It is the one route form in this contract
// that is not plain CRUD.
func TestRESTCustomVerbRoutes(t *testing.T) {
	t.Parallel()

	store := newFakeOrganizationStore(keycloak.Organization{ID: "11111111-1111-4111-8111-111111111111", Name: "acme"})
	baseURL, httpClient := startBackendWithTenants(t, store)

	body, status := get(t, httpClient, baseURL+"/api/v1/tenants/11111111-1111-4111-8111-111111111111:listMembers")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the custom verb, body = %s", status, body)
	}
}

// A REST PATCH that names only some fields must leave the rest alone.
// grpc-gateway inferred the field mask from the JSON body; vanguard does not,
// so the handler sees an empty mask and has to infer it the way the Clusters
// handler already does. Without that, a name-only PATCH wipes the domains.
func TestRESTPatchWithoutMaskKeepsUnnamedFields(t *testing.T) {
	t.Parallel()

	const id = "11111111-1111-4111-8111-111111111111"
	store := newFakeOrganizationStore(keycloak.Organization{
		ID: id, Name: "acme", Domains: []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
	})
	baseURL, httpClient := startBackendWithTenants(t, store)

	body, status := patch(t, httpClient, baseURL+"/api/v1/tenants/"+id, `{"metadata":{"name":"acme-two"}}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200, body = %s", status, body)
	}
	if !strings.Contains(body, "acme.example.com") {
		t.Errorf("PATCH response dropped the domains: %s", body)
	}

	body, status = get(t, httpClient, baseURL+"/api/v1/tenants/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body = %s", status, body)
	}
	if !strings.Contains(body, "acme.example.com") {
		t.Errorf("a name-only PATCH cleared the stored domains: %s", body)
	}
	if !strings.Contains(body, "acme-two") {
		t.Errorf("the PATCH did not apply the new name: %s", body)
	}
}

func patch(t *testing.T, client connecthttp.HTTPClient, url, body string) (string, int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	defer response.Body.Close()
	out, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(out), response.StatusCode
}

// A browser sends Connect over HTTP/1.1, not the plaintext HTTP/2 the other
// cases here use. Connect is a plain POST with a JSON body, so this is the
// shape the console actually produces.
func TestConnectOverHTTP1(t *testing.T) {
	t.Parallel()

	baseURL, _ := startBackend(t)

	request, err := http.NewRequest(http.MethodPost,
		baseURL+"/ai_grid.v1.AiGrid/Ping", strings.NewReader(`{"name":"browser"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	// Default transport: HTTP/1.1, no prior knowledge of HTTP/2.
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 1 {
		t.Fatalf("served over HTTP/%d, want HTTP/1.x", response.ProtoMajor)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "Welcome, browser!") {
		t.Errorf("body = %s, want the greeting", body)
	}
}
