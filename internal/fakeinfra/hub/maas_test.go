package hub

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	testSubscriptionsGVR = schema.GroupVersionResource{
		Group: maasAPIGroup, Version: maasVersion, Resource: maasSubscriptionsResource,
	}
	testAuthPoliciesGVR = schema.GroupVersionResource{
		Group: maasAPIGroup, Version: maasVersion, Resource: maasAuthPoliciesResource,
	}
	testModelRefsGVR = schema.GroupVersionResource{
		Group: maasAPIGroup, Version: maasVersion, Resource: maasModelRefsResource,
	}
)

func TestMAASNamespacedCRUD(t *testing.T) {
	fx := loadShipped(t, "default")
	fx.Access.ServiceAccount.Token = testToken
	th := startHub(t, fx)
	cfg := &rest.Config{Host: th.srv.URL, BearerToken: testToken, QPS: 1000, Burst: 1000}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic.NewForConfig: %v", err)
	}
	ctx := context.Background()
	subs := client.Resource(testSubscriptionsGVR).Namespace("models-as-a-service")

	list, err := subs.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list.Items))
	}

	got, err := client.Resource(testModelRefsGVR).Namespace("llm").Get(ctx, "granite-8b-instruct", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get modelref: %v", err)
	}
	if got.GetAnnotations()["openshift.io/display-name"] != "Granite 8B Instruct" {
		t.Fatalf("modelref annotations = %v", got.GetAnnotations())
	}

	created := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": maasAPIVersion,
		"kind":       maasSubscriptionKind,
		"metadata": map[string]interface{}{
			"name":      "smoke-sub",
			"namespace": "models-as-a-service",
		},
		"spec": map[string]interface{}{
			"owner": map[string]interface{}{
				"groups": []interface{}{map[string]interface{}{"name": "smoke"}},
				"users":  []interface{}{},
			},
			"modelRefs": []interface{}{
				map[string]interface{}{"name": "tiny-llm", "namespace": "llm"},
			},
			"priority": int64(5),
		},
	}}
	out, err := subs.Create(ctx, created, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if out.GetUID() == "" || out.GetResourceVersion() == "" {
		t.Fatalf("Create() meta = %#v", out.Object["metadata"])
	}

	patch := []byte(`{"spec":{"priority":9},"metadata":{"resourceVersion":"` + out.GetResourceVersion() + `"}}`)
	patched, err := subs.Patch(ctx, "smoke-sub", types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("Patch() error = %v", err)
	}
	prio, _, _ := unstructured.NestedInt64(patched.Object, "spec", "priority")
	if prio != 9 {
		t.Fatalf("patched priority = %d", prio)
	}

	stale := []byte(`{"spec":{"priority":1},"metadata":{"resourceVersion":"1"}}`)
	if _, err := subs.Patch(ctx, "smoke-sub", types.MergePatchType, stale, metav1.PatchOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("stale Patch() = %v, want Conflict", err)
	}

	if err := subs.Delete(ctx, "smoke-sub", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := subs.Get(ctx, "smoke-sub", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Get after delete = %v, want NotFound", err)
	}

	policies, err := client.Resource(testAuthPoliciesGVR).Namespace("models-as-a-service").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List policies: %v", err)
	}
	if len(policies.Items) != 2 {
		t.Fatalf("List policies len = %d, want 2", len(policies.Items))
	}
}

func TestShippedDefaultHasMAASFixtures(t *testing.T) {
	fx := loadShipped(t, "default")
	if len(fx.MaaSSubscriptions) != 2 || len(fx.MaaSAuthPolicies) != 2 || len(fx.MaaSModelRefs) != 2 {
		t.Fatalf("default MaaS counts: subs=%d policies=%d modelrefs=%d",
			len(fx.MaaSSubscriptions), len(fx.MaaSAuthPolicies), len(fx.MaaSModelRefs))
	}
}
