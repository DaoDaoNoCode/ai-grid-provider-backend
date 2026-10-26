package servers

import (
	"fmt"
	"testing"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/jsonpath"
)

func TestMWRSTemplateCarriesManifestConfig(t *testing.T) {
	t.Parallel()
	deployment := llmInferenceServiceFromModel(sampleModel("tmpl-llm"), "ai-grid-models")
	mwrs := mwrsFromModel(sampleModel("tmpl-llm"), "ai-grid-models", deployment)
	configs, found, err := unstructured.NestedSlice(mwrs.Object, "spec", "manifestWorkTemplate", "manifestConfigs")
	if err != nil || !found || len(configs) != 1 {
		t.Fatalf("manifestConfigs = %v (found %v, err %v)", configs, found, err)
	}
	cfg := configs[0].(map[string]interface{})
	id, _, _ := unstructured.NestedStringMap(cfg, "resourceIdentifier")
	want := map[string]string{
		"group": "serving.kserve.io", "resource": "llminferenceservices",
		"namespace": "ai-grid-models", "name": "tmpl-llm",
	}
	for k, v := range want {
		if id[k] != v {
			t.Fatalf("resourceIdentifier = %v, want %v", id, want)
		}
	}
	rules, _, _ := unstructured.NestedSlice(cfg, "feedbackRules")
	if len(rules) != 1 {
		t.Fatalf("feedbackRules = %v", rules)
	}
	rule := rules[0].(map[string]interface{})
	paths, _, _ := unstructured.NestedSlice(rule, "jsonPaths")
	if rule["type"] != "JSONPaths" || len(paths) != 1 ||
		paths[0].(map[string]interface{})["name"] != "ready" ||
		paths[0].(map[string]interface{})["path"] != llmReadyFeedbackPath {
		t.Fatalf("feedback rule = %v", rule)
	}
	condRules, _, _ := unstructured.NestedSlice(cfg, "conditionRules")
	got := map[string]string{}
	for _, raw := range condRules {
		r := raw.(map[string]interface{})
		if r["type"] != "CEL" {
			t.Fatalf("condition rule type = %v", r["type"])
		}
		got[r["condition"].(string)] = r["message"].(string)
	}
	if got["Progressing"] != messageRuleProgressing || got["Degraded"] != messageRuleDegraded || len(got) != 2 {
		t.Fatalf("condition rules = %v", got)
	}
}

// spokeLLM is a spoke LLMInferenceService with the given Ready condition;
// status "" leaves status out entirely.
func spokeLLM(status, reason string) map[string]interface{} {
	obj := map[string]interface{}{
		"apiVersion": "serving.kserve.io/v1alpha2",
		"kind":       "LLMInferenceService",
		"metadata":   map[string]interface{}{"name": "m", "namespace": "ai-grid-models"},
	}
	if status == "" {
		return obj
	}
	ready := map[string]interface{}{"type": "Ready", "status": status}
	if reason != "" {
		ready["reason"] = reason
	}
	obj["status"] = map[string]interface{}{"conditions": []interface{}{
		map[string]interface{}{"type": "PresetsCombined", "status": "True"},
		ready,
	}}
	return obj
}

// TestReadyFeedbackPath evaluates the feedback path the way the work agent
// does (ocm statusfeedback reader.go getValueByJsonPath): "{path}" against
// the whole object, missing keys allowed, one value expected.
func TestReadyFeedbackPath(t *testing.T) {
	t.Parallel()
	eval := func(obj map[string]interface{}) []interface{} {
		j := jsonpath.New("ready").AllowMissingKeys(true)
		if err := j.Parse(fmt.Sprintf("{%s}", llmReadyFeedbackPath)); err != nil {
			t.Fatalf("parse %s: %v", llmReadyFeedbackPath, err)
		}
		results, err := j.FindResults(obj)
		if err != nil {
			t.Fatalf("FindResults: %v", err)
		}
		var out []interface{}
		for _, r := range results {
			for _, v := range r {
				out = append(out, v.Interface())
			}
		}
		return out
	}
	if got := eval(spokeLLM("True", "")); len(got) != 1 || got[0] != "True" {
		t.Fatalf("Ready True -> %v", got)
	}
	if got := eval(spokeLLM("False", "MinimumReplicasUnavailable")); len(got) != 1 || got[0] != "False" {
		t.Fatalf("Ready False -> %v", got)
	}
	if got := eval(spokeLLM("", "")); len(got) != 0 {
		t.Fatalf("no status -> %v, want no value", got)
	}
}

// TestConditionRulesCEL evaluates the template's CEL rules with the work
// agent's environment (ocm conditions/reader.go: one `object` variable of
// DynType bound to the unstructured object).
func TestConditionRulesCEL(t *testing.T) {
	t.Parallel()
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType))
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	compile := func(expr string) cel.Program {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("compile %s: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatalf("program %s: %v", expr, err)
		}
		return prg
	}
	progressing, degraded := compile(llmProgressingCEL), compile(llmDegradedCEL)
	eval := func(prg cel.Program, obj map[string]interface{}) bool {
		out, _, err := prg.Eval(map[string]interface{}{"object": obj})
		if err != nil {
			t.Fatalf("eval on %v: %v", obj, err)
		}
		return out.Value().(bool)
	}
	tests := []struct {
		name                  string
		obj                   map[string]interface{}
		progressing, degraded bool
	}{
		{"no status yet", spokeLLM("", ""), true, false},
		{"starting", spokeLLM("False", "MinimumReplicasUnavailable"), true, false},
		{"Ready without reason", spokeLLM("False", ""), true, false},
		{"Unknown", spokeLLM("Unknown", ""), true, false},
		{"terminal ConfigNotFound", spokeLLM("False", "ConfigNotFound"), true, true},
		{"terminal GatewaysNotReady", spokeLLM("False", "GatewaysNotReady"), true, true},
		{"ready", spokeLLM("True", ""), false, false},
	}
	for _, tt := range tests {
		if got := eval(progressing, tt.obj); got != tt.progressing {
			t.Errorf("%s: Progressing = %v, want %v", tt.name, got, tt.progressing)
		}
		if got := eval(degraded, tt.obj); got != tt.degraded {
			t.Errorf("%s: Degraded = %v, want %v", tt.name, got, tt.degraded)
		}
	}
}
