package hub

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var testPoliciesGVR = schema.GroupVersionResource{Group: "policy.open-cluster-management.io", Version: "v1", Resource: "policies"}

// The shipped status Policies load, and the hub serves the roots and the
// per-cluster copies by namespace and name.
func TestShippedStatusPolicies(t *testing.T) {
	t.Parallel()
	fx := loadShipped(t, "default")
	fx.Access.ServiceAccount.Token = testToken
	th := startHub(t, fx)
	client, err := dynamic.NewForConfig(&rest.Config{Host: th.srv.URL, BearerToken: testToken})
	if err != nil {
		t.Fatalf("dynamic.NewForConfig: %v", err)
	}
	for _, ref := range []struct{ namespace, name string }{
		{"ai-grid-policies", "ai-grid-models-status"},
		{"ai-grid-policies", "ai-grid-catalog-status"},
		{"site-paris-1", "ai-grid-policies.ai-grid-models-status"},
		{"site-ashburn-1", "ai-grid-policies.ai-grid-catalog-status"},
	} {
		got, err := client.Resource(testPoliciesGVR).Namespace(ref.namespace).Get(context.Background(), ref.name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get(%s/%s) error = %v", ref.namespace, ref.name, err)
		}
		if got.GetName() != ref.name || got.GetResourceVersion() == "" {
			t.Fatalf("Get(%s/%s) = %s, resourceVersion %q", ref.namespace, ref.name, got.GetName(), got.GetResourceVersion())
		}
	}
	if _, err := client.Resource(testPoliciesGVR).Namespace("site-oslo-1").Get(context.Background(), "ai-grid-policies.ai-grid-models-status", metav1.GetOptions{}); err == nil {
		t.Fatal("Get(a copy the fixtures don't have) succeeded")
	}
}

func TestPolicyFixturesRejectTheReadyPolicyAndDuplicates(t *testing.T) {
	t.Parallel()
	policy := func(namespace, name string) unstructured.Unstructured {
		p := unstructured.Unstructured{}
		p.SetNamespace(namespace)
		p.SetName(name)
		return p
	}
	for name, policies := range map[string][]unstructured.Unstructured{
		"ready":     {policy(ReadyPolicyNamespace, ReadyPolicyName)},
		"duplicate": {policy("a", "p"), policy("a", "p")},
		"no name":   {policy("a", "")},
	} {
		if _, err := New(&Fixtures{Policies: policies}); err == nil {
			t.Errorf("%s: New() succeeded", name)
		}
	}
}
