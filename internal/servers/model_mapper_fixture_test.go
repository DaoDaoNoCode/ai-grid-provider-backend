package servers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestFixtureTemplatesMatchMWRSFromModel keeps the hand-authored placed-model
// fixtures honest: the template of every fixture MWRS and the spec of every
// work it generated are what mwrsFromModel writes for the fixture's
// LLMInferenceService today.
func TestFixtureTemplatesMatchMWRSFromModel(t *testing.T) {
	t.Parallel()
	read := func(preset, file string) []unstructured.Unstructured {
		raw, err := os.ReadFile(filepath.Join("..", "..", "fixtures", preset, file))
		if err != nil {
			t.Fatalf("read %s/%s: %v", preset, file, err)
		}
		var envelope struct {
			Items []unstructured.Unstructured `json:"items"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("unmarshal %s/%s: %v", preset, file, err)
		}
		return envelope.Items
	}
	for _, preset := range []string{"default", "poc"} {
		llms := map[string]unstructured.Unstructured{}
		for _, item := range read(preset, "llminferenceservices.json") {
			llms[item.GetName()] = item
		}
		templates := map[string]interface{}{}
		for _, set := range read(preset, "manifestworkreplicasets.json") {
			id := strings.TrimPrefix(set.GetName(), "model-")
			deployment, ok := llms[id]
			if !ok {
				t.Fatalf("%s: MWRS %s has no LLMInferenceService", preset, set.GetName())
			}
			model := modelFromLLMInferenceService(deployment)
			want, _, _ := unstructured.NestedFieldNoCopy(mwrsFromModel(model, "ai-grid-models", &deployment).Object, "spec", "manifestWorkTemplate")
			got, _, _ := unstructured.NestedFieldNoCopy(set.Object, "spec", "manifestWorkTemplate")
			if !jsonEqualForTest(t, want, got) {
				t.Errorf("%s: %s template differs from mwrsFromModel\n got %v\nwant %v", preset, set.GetName(), got, want)
			}
			templates["ai-grid-models."+set.GetName()] = got
		}
		for _, work := range read(preset, "manifestworks.json") {
			owner := work.GetLabels()["work.open-cluster-management.io/manifestworkreplicaset"]
			spec, _, _ := unstructured.NestedFieldNoCopy(work.Object, "spec")
			if tmpl, ok := templates[owner]; !ok || !jsonEqualForTest(t, tmpl, spec) {
				t.Errorf("%s: work %s/%s spec is not its MWRS %q template", preset, work.GetNamespace(), work.GetName(), owner)
			}
		}
	}
}

func TestModelSourceNameDefaultsToID(t *testing.T) {
	t.Parallel()
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "gitops-llm"},
		"spec":     map[string]interface{}{"model": map[string]interface{}{"uri": "hf://facebook/opt-125m"}},
	}}
	if got := modelFromLLMInferenceService(item).GetSpec().GetModel(); got.GetName() != "gitops-llm" || got.GetUri() != "hf://facebook/opt-125m" {
		t.Fatalf("spec.model = %v, want name gitops-llm", got)
	}
	_ = unstructured.SetNestedField(item.Object, "facebook/opt-125m", "spec", "model", "name")
	if got := modelFromLLMInferenceService(item).GetSpec().GetModel().GetName(); got != "facebook/opt-125m" {
		t.Fatalf("stored name = %q, want it kept", got)
	}
	unstructured.RemoveNestedField(item.Object, "spec", "model")
	if got := modelFromLLMInferenceService(item).GetSpec().GetModel(); got != nil {
		t.Fatalf("spec.model = %v, want nil without a stored spec.model", got)
	}
}

// TestModelFromFixtureLLMInferenceService checks the mapper against the
// shipped fake-hub fixture shape (fixtures/default/llminferenceservices.json).
func TestModelFromFixtureLLMInferenceService(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "default", "llminferenceservices.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var envelope struct {
		Items []unstructured.Unstructured `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var item *unstructured.Unstructured
	for i := range envelope.Items {
		if envelope.Items[i].GetName() == "fixture-cpu-llm" {
			item = &envelope.Items[i]
		}
	}
	if item == nil {
		t.Fatalf("fixture-cpu-llm not among %d items", len(envelope.Items))
	}
	m := modelFromLLMInferenceService(*item)
	if m.GetId() != "fixture-cpu-llm" {
		t.Fatalf("id = %q", m.GetId())
	}
	if m.GetMetadata().GetAnnotations()["openshift.io/display-name"] != "Fixture CPU LLM" {
		t.Fatalf("display-name = %v", m.GetMetadata().GetAnnotations())
	}
	if m.GetMetadata().GetVersion() != 1 {
		t.Fatalf("version = %d", m.GetMetadata().GetVersion())
	}
	if m.GetSpec().GetModel().GetUri() != "hf://facebook/opt-125m" {
		t.Fatalf("uri = %q", m.GetSpec().GetModel().GetUri())
	}
	if m.GetSpec().Replicas == nil || m.GetSpec().GetReplicas() != 1 {
		t.Fatalf("replicas = %v", m.GetSpec().Replicas)
	}
	if len(m.GetSpec().GetBaseRefs()) != 1 || m.GetSpec().GetBaseRefs()[0] != "v3-6-0-kserve-config-llm-template-cpu" {
		t.Fatalf("baseRefs = %v", m.GetSpec().GetBaseRefs())
	}
	main := m.GetSpec().GetMain()
	if main == nil || main.GetImage() != "quay.io/example/vllm:latest" {
		t.Fatalf("main = %+v", main)
	}
	if main.GetRequests()["cpu"] != "100m" || main.GetLimits()["memory"] != "10Gi" {
		t.Fatalf("resources = req=%v lim=%v", main.GetRequests(), main.GetLimits())
	}
	router := m.GetSpec().GetRouter()
	if router == nil || !router.GetManagedRoute() {
		t.Fatalf("router = %+v", router)
	}
	if len(router.GetGatewayRefs()) != 1 || router.GetGatewayRefs()[0].GetName() != "openshift-ai-inference" {
		t.Fatalf("gateway_refs = %+v", router.GetGatewayRefs())
	}
	if m.GetStatus() == nil || m.GetStatus().GetUrl() != "https://fixture-cpu-llm.example.com" {
		t.Fatalf("status = %+v", m.GetStatus())
	}
}
