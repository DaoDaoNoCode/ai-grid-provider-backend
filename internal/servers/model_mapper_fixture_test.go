package servers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

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
	if len(envelope.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(envelope.Items))
	}
	m := modelFromLLMInferenceService(envelope.Items[0])
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
