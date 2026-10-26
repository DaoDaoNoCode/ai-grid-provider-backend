package servers

import (
	"context"
	"encoding/json"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// externalPlacement is a Placement written outside the API (GitOps, oc
// apply) with a claim selector and tolerations.
func externalPlacement(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cluster.open-cluster-management.io/v1beta1",
		"kind":       "Placement",
		"metadata":   map[string]interface{}{"name": name, "namespace": "ai-grid-models", "resourceVersion": "3"},
		"spec": map[string]interface{}{
			"clusterSets": []interface{}{"global"},
			"predicates": []interface{}{map[string]interface{}{
				"requiredClusterSelector": map[string]interface{}{
					"labelSelector": map[string]interface{}{
						"matchLabels": map[string]interface{}{"ai-grid/enabled": "true", "ai-grid/provisioned": "true"},
					},
					"claimSelector": map[string]interface{}{
						"matchExpressions": []interface{}{map[string]interface{}{
							"key": "platform.open-cluster-management.io", "operator": "In", "values": []interface{}{"AWS", "BareMetal"},
						}},
					},
				},
			}},
			"tolerations": []interface{}{
				map[string]interface{}{
					"key": "cluster.open-cluster-management.io/unreachable", "operator": "Exists",
					"effect": "NoSelect", "tolerationSeconds": int64(300),
				},
			},
		},
	}}
}

func TestModelPlacementClaimsAndTolerationsRoundTrip(t *testing.T) {
	t.Parallel()
	stored := externalPlacement("model-rt")
	placement := modelPlacementFromUnstructured(*stored)

	claims := placement.GetClaimExpressions()
	if len(claims) != 1 || claims[0].GetKey() != "platform.open-cluster-management.io" ||
		claims[0].GetOperator() != "In" || len(claims[0].GetValues()) != 2 {
		t.Fatalf("claim_expressions = %v", claims)
	}
	tol := placement.GetTolerations()
	if len(tol) != 1 || tol[0].GetKey() != "cluster.open-cluster-management.io/unreachable" ||
		tol[0].GetOperator() != "Exists" || tol[0].GetEffect() != "NoSelect" ||
		tol[0].TolerationSeconds == nil || tol[0].GetTolerationSeconds() != 300 {
		t.Fatalf("tolerations = %v", tol)
	}

	model := sampleModel("rt")
	model.Spec.Placement = placement
	written := placementFromModel(model, "ai-grid-models")
	for _, field := range [][]string{
		{"spec", "clusterSets"},
		{"spec", "tolerations"},
	} {
		want, _, _ := unstructured.NestedFieldNoCopy(stored.Object, field...)
		got, _, _ := unstructured.NestedFieldNoCopy(written.Object, field...)
		if !jsonEqualForTest(t, want, got) {
			t.Fatalf("%v = %v, want %v", field, got, want)
		}
	}
	wantPreds, _, _ := unstructured.NestedSlice(stored.Object, "spec", "predicates")
	gotPreds, _, _ := unstructured.NestedSlice(written.Object, "spec", "predicates")
	if !jsonEqualForTest(t, wantPreds, gotPreds) {
		t.Fatalf("predicates = %v, want %v", gotPreds, wantPreds)
	}
}

func TestModelPlacementWithoutClaimsOrTolerationsWritesNeither(t *testing.T) {
	t.Parallel()
	model := sampleModel("plain")
	model.Spec.Placement = &aigridv1.ModelPlacement{}
	written := placementFromModel(model, "ai-grid-models")
	if _, found, _ := unstructured.NestedFieldNoCopy(written.Object, "spec", "tolerations"); found {
		t.Fatal("tolerations written for a placement without any")
	}
	preds, _, _ := unstructured.NestedSlice(written.Object, "spec", "predicates")
	if _, found, _ := unstructured.NestedFieldNoCopy(preds[0].(map[string]interface{}), "requiredClusterSelector", "claimSelector"); found {
		t.Fatal("claimSelector written for a placement without claim expressions")
	}
}

// TestModelsUpdateWholePlacementKeepsClaimsAndTolerations: a client that
// loads a model whose Placement was written outside the API and sends the
// placement back with one change keeps the claim selector and tolerations;
// sending it without them removes them.
func TestModelsUpdateWholePlacementKeepsClaimsAndTolerations(t *testing.T) {
	t.Parallel()
	llm := llmInferenceServiceFromModel(sampleModel("gitops-llm"), "ai-grid-models")
	llm.SetResourceVersion("1")
	client := newModelsClient(llm, externalPlacement("model-gitops-llm"))
	srv := mustModelsServer(t, client)
	ctx := context.Background()

	got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "gitops-llm"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	object := got.GetObject()
	if len(object.GetSpec().GetPlacement().GetTolerations()) != 1 || len(object.GetSpec().GetPlacement().GetClaimExpressions()) != 1 {
		t.Fatalf("Get placement = %v", object.GetSpec().GetPlacement())
	}
	object.Spec.Placement.NumberOfClusters = proto.Int32(1)
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     object,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	stored, err := client.Resource(testPlacementGVR).Namespace("ai-grid-models").Get(ctx, "model-gitops-llm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Placement: %v", err)
	}
	if n, _, _ := unstructured.NestedInt64(stored.Object, "spec", "numberOfClusters"); n != 1 {
		t.Fatalf("numberOfClusters = %d", n)
	}
	reread := modelPlacementFromUnstructured(*stored)
	if len(reread.GetTolerations()) != 1 || len(reread.GetClaimExpressions()) != 1 || len(reread.GetClusterSets()) != 1 {
		t.Fatalf("whole-placement update dropped fields: %v", reread)
	}

	object.Spec.Placement.Tolerations = nil
	object.Spec.Placement.ClaimExpressions = nil
	object.Spec.Placement.ClusterSets = nil
	object.Spec.Placement.NumberOfClusters = nil
	object.Metadata.Version = 0
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     object,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
	}); err != nil {
		t.Fatalf("Update() clearing error = %v", err)
	}
	stored, err = client.Resource(testPlacementGVR).Namespace("ai-grid-models").Get(ctx, "model-gitops-llm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Placement: %v", err)
	}
	for _, key := range []string{"tolerations", "clusterSets", "numberOfClusters"} {
		if _, found, _ := unstructured.NestedFieldNoCopy(stored.Object, "spec", key); found {
			t.Fatalf("spec.%s kept after the placement was replaced without it: %v", key, stored.Object["spec"])
		}
	}
	if reread := modelPlacementFromUnstructured(*stored); len(reread.GetClaimExpressions()) != 0 {
		t.Fatalf("claim expressions kept: %v", reread.GetClaimExpressions())
	}
}

func TestModelsPlacementValidation(t *testing.T) {
	t.Parallel()
	srv := mustModelsServer(t, newModelsClient())
	tests := []struct {
		name      string
		placement *aigridv1.ModelPlacement
	}{
		{"claim operator", &aigridv1.ModelPlacement{ClaimExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: "k", Operator: "Gt"}}}},
		{"toleration operator", &aigridv1.ModelPlacement{Tolerations: []*aigridv1.ModelToleration{{Key: "k", Operator: "In"}}}},
		{"empty key without Exists", &aigridv1.ModelPlacement{Tolerations: []*aigridv1.ModelToleration{{Effect: "NoSelect"}}}},
		{"Exists with value", &aigridv1.ModelPlacement{Tolerations: []*aigridv1.ModelToleration{{Key: "k", Operator: "Exists", Value: "v"}}}},
		{"toleration effect", &aigridv1.ModelPlacement{Tolerations: []*aigridv1.ModelToleration{{Key: "k", Effect: "NoSchedule"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := sampleModel("invalid-placement")
			obj.Spec.Placement = tt.placement
			_, err := srv.Create(context.Background(), &aigridv1.ModelsCreateRequest{Object: obj})
			assertConnectCode(t, err, connect.CodeInvalidArgument)
		})
	}

	ok := sampleModel("valid-placement")
	ok.Spec.Placement = &aigridv1.ModelPlacement{
		ClaimExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: "platform.open-cluster-management.io", Operator: "Exists"}},
		Tolerations: []*aigridv1.ModelToleration{
			{Operator: "Exists"},
			{Key: "cluster.open-cluster-management.io/unavailable", Value: "", Effect: "NoSelectIfNew", TolerationSeconds: proto.Int64(60)},
		},
	}
	if _, err := srv.Create(context.Background(), &aigridv1.ModelsCreateRequest{Object: ok}); err != nil {
		t.Fatalf("Create() valid placement error = %v", err)
	}
}

func jsonEqualForTest(t *testing.T, a, b interface{}) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(ja) == string(jb)
}
