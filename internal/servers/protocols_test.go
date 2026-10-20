package servers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/restgateway"
	"golang.org/x/net/http2"
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

	aiGridServer, err := NewAiGridServer().Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	server := connect.NewServer()
	aigridv1connect.RegisterAiGridHandler(server, aiGridServer)
	aigridv1connect.RegisterClustersHandler(server, mustClustersServer(t, newManagedClusterClient(objects...)))

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

	// Plaintext HTTP/2, which is what the deployed service listens for.
	httpClient := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
	}}
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
