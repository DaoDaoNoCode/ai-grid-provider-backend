package hub

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func searchFixtures() *Fixtures {
	fx := testFixtures()
	fx.SearchNodes = []SearchNode{
		{Cluster: "site-a", Name: "a-gpu", Ready: true, Labels: map[string]string{"nvidia.com/gpu.present": "true", "nvidia.com/gpu.product": "NVIDIA-L4"}},
		{Cluster: "site-b", Name: "b-cpu", Labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
	}
	return fx
}

const gpuQuery = `{"query":"query mySearch($input: [SearchInput]) { searchResult: search(input: $input) { items } }",` +
	`"variables":{"input":[{"filters":[{"property":"kind","values":["Node"]},{"property":"label","values":["nvidia.com/gpu.present=true"]}],"limit":-1}]}}`

func searchItems(t *testing.T, body map[string]any) []any {
	t.Helper()
	data, _ := body["data"].(map[string]any)
	results, _ := data["searchResult"].([]any)
	if len(results) != 1 {
		t.Fatalf("searchResult = %v", body)
	}
	items, _ := results[0].(map[string]any)["items"].([]any)
	return items
}

func TestSearchAnswersLikeSearchV2(t *testing.T) {
	th := startHub(t, searchFixtures())

	// search-v2-api answers these with http.Error, so as text/plain with a
	// JSON message; the shared do helper insists on application/json.
	status := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, th.srv.URL+SearchPath, strings.NewReader(gpuQuery))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := status(""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code := status("expired-token"); code != http.StatusForbidden {
		t.Fatalf("rejected token: %d, want 403", code)
	}
	// A caller who may see no managed cluster gets no items, not an error.
	code, body := th.do(t, http.MethodPost, SearchPath, "viewer-token", "application/json", gpuQuery)
	if code != http.StatusOK || len(searchItems(t, body)) != 0 {
		t.Fatalf("denied token: %d %v", code, body)
	}

	code, body = th.do(t, http.MethodPost, SearchPath, testToken, "application/json", gpuQuery)
	items := searchItems(t, body)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("GPU nodes: %d %v", code, body)
	}
	item := items[0].(map[string]any)
	if item["cluster"] != "site-a" || item["name"] != "a-gpu" ||
		item["label"] != "nvidia.com/gpu.present=true; nvidia.com/gpu.product=NVIDIA-L4" ||
		item["condition"] != "DiskPressure=False; MemoryPressure=False; PIDPressure=False; Ready=True" {
		t.Fatalf("item = %v", item)
	}

	// A malformed label filter is Search's GraphQL error.
	bad := `{"query":"q","variables":{"input":[{"filters":[{"property":"label","values":["no-equals"]}]}]}}`
	_, body = th.do(t, http.MethodPost, SearchPath, testToken, "application/json", bad)
	if errs, _ := body["errors"].([]any); len(errs) != 1 {
		t.Fatalf("bad label filter: %v", body)
	}

	// Search forgets a cluster's resources once the cluster is gone.
	if err := th.client(t, testToken).Delete(context.Background(), "site-a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	th.clock.Advance(DefaultDetachAfter + 1)
	_, body = th.do(t, http.MethodPost, SearchPath, testToken, "application/json", gpuQuery)
	if items := searchItems(t, body); len(items) != 0 {
		t.Fatalf("after site-a was deleted: %v", items)
	}
}

func TestSearchNodesMustNameAFixtureCluster(t *testing.T) {
	fx := searchFixtures()
	fx.SearchNodes = append(fx.SearchNodes, SearchNode{Cluster: "nope", Name: "x"})
	if _, err := New(fx); err == nil {
		t.Fatal("New() accepted a node of an unknown cluster")
	}
}
