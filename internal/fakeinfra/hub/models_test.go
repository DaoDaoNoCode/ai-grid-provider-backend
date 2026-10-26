package hub

import (
	"context"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	testManifestWorksGVR = schema.GroupVersionResource{Group: mwrsAPIGroup, Version: manifestWorkVersion, Resource: manifestWorksResource}
	testMWRSGVR          = schema.GroupVersionResource{Group: mwrsAPIGroup, Version: mwrsVersion, Resource: mwrsResource}
	testDecisionsGVR     = schema.GroupVersionResource{Group: placementAPIGroup, Version: placementVersion, Resource: placementDecisionsResource}
)

func fixtureObject(apiVersion, kind, namespace, name, rv string, labels map[string]string) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "resourceVersion": rv,
			"uid": "00000000-0000-4000-8000-0000000000" + rv, "generation": int64(1),
			"creationTimestamp": "2026-10-20T09:00:00Z",
		},
	}}
	u.SetLabels(labels)
	return u
}

func modelsFixtures() *Fixtures {
	fx := testFixtures()
	owner := func(mwrs string) map[string]string {
		return map[string]string{LabelMWRSOwner: "ai-grid-models." + mwrs, "work.open-cluster-management.io/placementname": mwrs}
	}
	fx.ManifestWorkReplicaSets = []unstructured.Unstructured{
		fixtureObject(mwrsAPIVersion, mwrsKind, "ai-grid-models", "model-a", "10", nil),
		fixtureObject(mwrsAPIVersion, mwrsKind, "ai-grid-models", "model-b", "11", nil),
	}
	fx.ManifestWorks = []unstructured.Unstructured{
		fixtureObject(manifestWorkAPIVersion, manifestWorkKind, "site-b", "model-abbbbb", "12", owner("model-a")),
		fixtureObject(manifestWorkAPIVersion, manifestWorkKind, "site-a", "model-aaaaaa", "13", owner("model-a")),
		fixtureObject(manifestWorkAPIVersion, manifestWorkKind, "site-a", "model-bccccc", "14", owner("model-b")),
	}
	fx.PlacementDecisions = []unstructured.Unstructured{
		fixtureObject(placementAPIVersion, placementDecisionKind, "ai-grid-models", "model-a-decision-1", "15",
			map[string]string{"cluster.open-cluster-management.io/placement": "model-a"}),
		fixtureObject(placementAPIVersion, placementDecisionKind, "ai-grid-models", "model-b-decision-1", "16",
			map[string]string{"cluster.open-cluster-management.io/placement": "model-b"}),
	}
	return fx
}

func dynamicClient(t *testing.T, th *testHub, token string) dynamic.Interface {
	t.Helper()
	c, err := dynamic.NewForConfig(&rest.Config{Host: th.srv.URL, BearerToken: token, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatalf("dynamic.NewForConfig: %v", err)
	}
	return c
}

func names(list *unstructured.UnstructuredList) []string {
	var out []string
	for _, item := range list.Items {
		out = append(out, item.GetNamespace()+"/"+item.GetName())
	}
	return out
}

func TestManifestWorksListedAcrossNamespacesBySelector(t *testing.T) {
	th := startHub(t, modelsFixtures())
	client := dynamicClient(t, th, testToken)
	ctx := context.Background()

	all, err := client.Resource(testManifestWorksGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if got := names(all); len(got) != 3 || got[0] != "site-a/model-aaaaaa" || got[1] != "site-a/model-bccccc" || got[2] != "site-b/model-abbbbb" {
		t.Fatalf("all works = %v, want namespace then name order", got)
	}
	owned, err := client.Resource(testManifestWorksGVR).Namespace("").List(ctx, metav1.ListOptions{
		LabelSelector: LabelMWRSOwner + "=ai-grid-models.model-a",
	})
	if err != nil {
		t.Fatalf("List by owner: %v", err)
	}
	if got := names(owned); len(got) != 2 || got[0] != "site-a/model-aaaaaa" || got[1] != "site-b/model-abbbbb" {
		t.Fatalf("model-a works = %v", got)
	}
	inSite, err := client.Resource(testManifestWorksGVR).Namespace("site-a").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List site-a: %v", err)
	}
	if got := names(inSite); len(got) != 2 {
		t.Fatalf("site-a works = %v", got)
	}
	if _, err := client.Resource(testManifestWorksGVR).Namespace("site-b").Get(ctx, "model-abbbbb", metav1.GetOptions{}); err != nil {
		t.Fatalf("Get work: %v", err)
	}
}

func TestNamespacedListHonoursLabelSelector(t *testing.T) {
	th := startHub(t, modelsFixtures())
	list, err := dynamicClient(t, th, testToken).Resource(testDecisionsGVR).Namespace("ai-grid-models").List(context.Background(),
		metav1.ListOptions{LabelSelector: "cluster.open-cluster-management.io/placement=model-b"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := names(list); len(got) != 1 || got[0] != "ai-grid-models/model-b-decision-1" {
		t.Fatalf("decisions = %v", got)
	}
	code, body := th.do(t, http.MethodGet, manifestWorksPath+"?labelSelector=a%20in%20(", testToken, "", "")
	if code != http.StatusBadRequest || body["reason"] != string(metav1.StatusReasonBadRequest) {
		t.Fatalf("bad selector = %d %v, want 400 BadRequest", code, body)
	}
}

func TestManifestWorksForbiddenAtClusterScope(t *testing.T) {
	th := startHub(t, modelsFixtures())
	_, err := dynamicClient(t, th, "viewer-token").Resource(testManifestWorksGVR).Namespace("").List(context.Background(), metav1.ListOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("List(viewer) = %v, want Forbidden", err)
	}
	want := `manifestworks.work.open-cluster-management.io is forbidden: User "dev-user" cannot list resource "manifestworks" in API group "work.open-cluster-management.io" at the cluster scope`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

func TestDeletingMWRSDeletesItsWorks(t *testing.T) {
	th := startHub(t, modelsFixtures())
	client := dynamicClient(t, th, testToken)
	ctx := context.Background()
	if err := client.Resource(testMWRSGVR).Namespace("ai-grid-models").Delete(ctx, "model-a", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete MWRS: %v", err)
	}
	all, err := client.Resource(testManifestWorksGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := names(all); len(got) != 1 || got[0] != "site-a/model-bccccc" {
		t.Fatalf("works after deleting model-a's MWRS = %v, want only model-b's", got)
	}
}
