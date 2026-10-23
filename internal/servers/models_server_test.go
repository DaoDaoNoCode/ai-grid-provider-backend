package servers

import (
	"context"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

var (
	testLLMGVR = schema.GroupVersionResource{
		Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices",
	}
	testPlacementGVR = schema.GroupVersionResource{
		Group: "cluster.open-cluster-management.io", Version: "v1beta1", Resource: "placements",
	}
	testPlacementDecisionGVR = schema.GroupVersionResource{
		Group: "cluster.open-cluster-management.io", Version: "v1beta1", Resource: "placementdecisions",
	}
	testMWRSGVR = schema.GroupVersionResource{
		Group: "work.open-cluster-management.io", Version: "v1alpha1", Resource: "manifestworkreplicasets",
	}
)

func TestModelsServerRequiresClient(t *testing.T) {
	t.Parallel()
	if _, err := NewModelsServer().Build(); err == nil {
		t.Fatal("Build() without client succeeded")
	}
}

func TestModelsCRUDHubOnly(t *testing.T) {
	t.Parallel()
	client := newModelsClient()
	srv := mustModelsServer(t, client)
	ctx := context.Background()

	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel("tiny-llm")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.GetObject().GetId() != "tiny-llm" {
		t.Fatalf("Create id = %q", created.GetObject().GetId())
	}

	llm, err := client.Resource(testLLMGVR).Namespace("ai-grid-internal-models").Get(ctx, "tiny-llm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get LLMInferenceService: %v", err)
	}
	uri, _, _ := unstructured.NestedString(llm.Object, "spec", "model", "uri")
	if uri != "hf://facebook/opt-125m" {
		t.Fatalf("uri = %q", uri)
	}
	ref, err := client.Resource(testModelRefsGVR).Namespace("ai-grid-internal-models").Get(ctx, "tiny-llm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get MaaSModelRef: %v", err)
	}
	kind, _, _ := unstructured.NestedString(ref.Object, "spec", "modelRef", "kind")
	if kind != "LLMInferenceService" {
		t.Fatalf("modelRef.kind = %q", kind)
	}

	got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "tiny-llm"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetObject().GetSpec().GetModel().GetUri() != "hf://facebook/opt-125m" {
		t.Fatalf("Get uri = %q", got.GetObject().GetSpec().GetModel().GetUri())
	}

	list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.GetTotal() != 1 || list.GetSize() != 1 {
		t.Fatalf("List size/total = %d/%d", list.GetSize(), list.GetTotal())
	}

	replicas := int32(2)
	updatedObj := sampleModel("tiny-llm")
	updatedObj.Spec.Replicas = &replicas
	updatedObj.Metadata = &aigridv1.Metadata{Name: "tiny-llm"}
	updated, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     updatedObj,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.replicas"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.GetObject().GetSpec().GetReplicas() != 2 {
		t.Fatalf("updated replicas = %v", updated.GetObject().GetSpec().Replicas)
	}

	if _, err := srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "tiny-llm"}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := client.Resource(testLLMGVR).Namespace("ai-grid-internal-models").Get(ctx, "tiny-llm", metav1.GetOptions{}); err == nil {
		t.Fatal("LLMInferenceService still present after Delete")
	}
	if _, err := client.Resource(testModelRefsGVR).Namespace("ai-grid-internal-models").Get(ctx, "tiny-llm", metav1.GetOptions{}); err == nil {
		t.Fatal("MaaSModelRef still present after Delete")
	}
}

func TestModelsCreateWithPlacement(t *testing.T) {
	t.Parallel()
	client := newModelsClient()
	srv := mustModelsServer(t, client)
	ctx := context.Background()

	obj := sampleModel("placed-llm")
	obj.Spec.Placement = &aigridv1.ModelPlacement{
		LabelSelector: &aigridv1.ModelLabelSelector{
			MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{
				{Key: "location", Operator: "In", Values: []string{"site-paris"}},
			},
		},
	}
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: obj})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.GetObject().GetSpec().GetPlacement() == nil {
		t.Fatal("expected placement on created model")
	}

	if _, err := client.Resource(testPlacementGVR).Namespace("ai-grid-internal-models").Get(ctx, "model-placed-llm", metav1.GetOptions{}); err != nil {
		t.Fatalf("get Placement: %v", err)
	}
	if _, err := client.Resource(testMWRSGVR).Namespace("ai-grid-internal-models").Get(ctx, "model-placed-llm", metav1.GetOptions{}); err != nil {
		t.Fatalf("get ManifestWorkReplicaSet: %v", err)
	}
}

func TestModelsCreateMWRSDisabled(t *testing.T) {
	t.Parallel()
	client := newModelsClient()
	srv, err := NewModelsServer().
		SetDynamicClient(client).
		SetManifestWorkReplicaSetEnabled(false).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	obj := sampleModel("blocked")
	obj.Spec.Placement = &aigridv1.ModelPlacement{
		LabelSelector: &aigridv1.ModelLabelSelector{
			MatchLabels: map[string]string{"ai-grid/enabled": "true"},
		},
	}
	_, err = srv.Create(context.Background(), &aigridv1.ModelsCreateRequest{Object: obj})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}

func TestModelsDeleteGuardedBySubscription(t *testing.T) {
	t.Parallel()
	client := newModelsClient(
		subscriptionUnstructured("sub-a", "models-as-a-service"),
	)
	// Point subscription at our model namespace/name.
	sub := subscriptionUnstructured("sub-a", "models-as-a-service")
	_ = unstructured.SetNestedSlice(sub.Object, []interface{}{
		map[string]interface{}{
			"name":      "guarded",
			"namespace": "ai-grid-internal-models",
			"tokenRateLimits": []interface{}{
				map[string]interface{}{"limit": int64(100), "window": "1m"},
			},
		},
	}, "spec", "modelRefs")
	client = newModelsClient(sub)

	srv := mustModelsServer(t, client)
	ctx := context.Background()
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel("guarded")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err := srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "guarded"})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}

func TestModelsValidation(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	ctx := context.Background()

	bad := sampleModel("bad")
	bad.Spec.Model.Uri = "file:///tmp/model"
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: bad})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	both := sampleModel("both")
	both.Spec.Scaling = &aigridv1.ModelScaling{MaxReplicas: 2, Keda: true, Triggers: []*aigridv1.ModelKedaTrigger{{Type: "cpu"}}}
	_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: both})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

func TestModelsGetNotFound(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	_, err := srv.Get(context.Background(), &aigridv1.ModelsGetRequest{Id: "missing"})
	assertConnectCode(t, err, connect.CodeNotFound)
}

func TestModelsCreateAlreadyExists(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	ctx := context.Background()
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel("dup")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel("dup")})
	assertConnectCode(t, err, connect.CodeAlreadyExists)
}

func TestModelsCreateScalingOnly(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	obj := sampleModel("scaled")
	obj.Spec.Replicas = nil
	obj.Spec.Scaling = &aigridv1.ModelScaling{
		MinReplicas: 1,
		MaxReplicas: 3,
		Keda:        true,
		Triggers: []*aigridv1.ModelKedaTrigger{{
			Type:     "cpu",
			Metadata: map[string]string{"value": "70"},
		}},
	}
	created, err := srv.Create(context.Background(), &aigridv1.ModelsCreateRequest{Object: obj})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.GetObject().GetSpec().Replicas != nil {
		t.Fatalf("replicas = %v, want unset", created.GetObject().GetSpec().Replicas)
	}
	if created.GetObject().GetSpec().GetScaling().GetMaxReplicas() != 3 {
		t.Fatalf("scaling.max = %d", created.GetObject().GetSpec().GetScaling().GetMaxReplicas())
	}
}

func TestModelsUpdateStaleVersion(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	ctx := context.Background()
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel("lock-me")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	obj := created.GetObject()
	obj.Metadata.Version = 999
	replicas := int32(2)
	obj.Spec.Replicas = &replicas
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     obj,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.replicas"}},
		Lock:       true,
	})
	assertConnectCode(t, err, connect.CodeAborted)
}

func TestModelsUpdateLockRequiresVersion(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	ctx := context.Background()
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel("nolock")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	obj := created.GetObject()
	obj.Metadata.Version = 0
	replicas := int32(2)
	obj.Spec.Replicas = &replicas
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     obj,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.replicas"}},
		Lock:       true,
	})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

func TestModelsDeleteGuardedByAuthPolicy(t *testing.T) {
	t.Parallel()
	pol := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSAuthPolicy",
		"metadata": map[string]interface{}{
			"name":            "guard-policy",
			"namespace":       "models-as-a-service",
			"resourceVersion": "1",
		},
		"spec": map[string]interface{}{
			"subjects": map[string]interface{}{
				"groups": []interface{}{map[string]interface{}{"name": "g"}},
				"users":  []interface{}{},
			},
			"modelRefs": []interface{}{
				map[string]interface{}{
					"name":      "pol-guarded",
					"namespace": "ai-grid-internal-models",
				},
			},
		},
	}}
	srv := mustModelsServer(t, newModelsClient(pol))
	ctx := context.Background()
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel("pol-guarded")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err := srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "pol-guarded"})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}

func TestModelsListFilterOrderPaging(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	ctx := context.Background()
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: sampleModel(id)}); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}

	filter := `this.id == "bravo"`
	filtered, err := srv.List(ctx, &aigridv1.ModelsListRequest{Filter: &filter})
	if err != nil {
		t.Fatalf("List filter error = %v", err)
	}
	if filtered.GetTotal() != 1 || len(filtered.GetItems()) != 1 || filtered.GetItems()[0].GetId() != "bravo" {
		t.Fatalf("filter = size/total %d/%d items=%v", filtered.GetSize(), filtered.GetTotal(), idsOf(filtered.GetItems()))
	}

	order := "-id"
	ordered, err := srv.List(ctx, &aigridv1.ModelsListRequest{Order: &order})
	if err != nil {
		t.Fatalf("List order error = %v", err)
	}
	if got := idsOf(ordered.GetItems()); len(got) != 3 || got[0] != "charlie" || got[2] != "alpha" {
		t.Fatalf("order -id = %v", got)
	}

	offset, limit := int32(1), int32(1)
	page, err := srv.List(ctx, &aigridv1.ModelsListRequest{Offset: &offset, Limit: &limit})
	if err != nil {
		t.Fatalf("List page error = %v", err)
	}
	if page.GetTotal() != 3 || page.GetSize() != 1 || page.GetItems()[0].GetId() != "bravo" {
		t.Fatalf("page = size/total %d/%d id=%v", page.GetSize(), page.GetTotal(), idsOf(page.GetItems()))
	}

	badOrder := "spec.replicas"
	_, err = srv.List(ctx, &aigridv1.ModelsListRequest{Order: &badOrder})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

func idsOf(items []*aigridv1.Model) []string {
	out := make([]string, 0, len(items))
	for _, m := range items {
		out = append(out, m.GetId())
	}
	return out
}

func mustModelsServer(t *testing.T, client *fake.FakeDynamicClient) *ModelsServer {
	t.Helper()
	srv, err := NewModelsServer().
		SetDynamicClient(client).
		SetManifestWorkReplicaSetEnabled(true).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return srv
}

func newModelsClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			testLLMGVR:               "LLMInferenceServiceList",
			testPlacementGVR:         "PlacementList",
			testPlacementDecisionGVR: "PlacementDecisionList",
			testMWRSGVR:              "ManifestWorkReplicaSetList",
			testModelRefsGVR:         "MaaSModelRefList",
			testSubscriptionsGVR:     "MaaSSubscriptionList",
			testAuthPoliciesGVR:      "MaaSAuthPolicyList",
		},
		objects...,
	)
}

func sampleModel(id string) *aigridv1.Model {
	replicas := int32(1)
	return &aigridv1.Model{
		Id: id,
		Metadata: &aigridv1.Metadata{
			Name: id,
			Annotations: map[string]string{
				"openshift.io/display-name": id,
			},
		},
		Spec: &aigridv1.ModelSpec{
			Model: &aigridv1.ModelSource{
				Uri:  "hf://facebook/opt-125m",
				Name: "facebook/opt-125m",
			},
			Replicas: &replicas,
			BaseRefs: []string{"v3-6-0-kserve-config-llm-template-cpu"},
			Main: &aigridv1.ModelContainer{
				Image: "quay.io/example/vllm:latest",
				Requests: map[string]string{
					"cpu":    "100m",
					"memory": "8Gi",
				},
				Limits: map[string]string{
					"cpu":    "1",
					"memory": "10Gi",
				},
			},
		},
	}
}
