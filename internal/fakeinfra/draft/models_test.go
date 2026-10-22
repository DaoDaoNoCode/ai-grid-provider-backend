package draft

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

const testNS = "ai-grid-models"

var testStart = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// hubModel is a hub deployment shaped like the hub's instance (models-maas.md 2.1).
func hubModel(name string) *aigriddraftv1.Model {
	return &aigriddraftv1.Model{
		Name:     name,
		Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS, Annotations: map[string]string{AnnotationDisplayName: "Model " + name}},
		Spec: &aigriddraftv1.ModelSpec{
			Model:    &aigriddraftv1.ModelSource{Uri: "hf://example-org/" + name, Name: "example-org/" + name},
			Replicas: proto.Int32(1),
			BaseRefs: []string{"v3-6-0-kserve-config-llm-template-cpu"},
			Router:   &aigriddraftv1.Router{GatewayRefs: []*aigriddraftv1.GatewayRef{{Name: GatewayAIInference, Namespace: GatewayNamespace}}, ManagedRoute: true, Scheduler: true},
			Main:     &aigriddraftv1.Container{Requests: map[string]string{"cpu": "4", "memory": "16Gi"}, Env: []*aigriddraftv1.EnvVar{{Name: "VLLM_CPU_KVCACHE_SPACE", Value: "8"}}},
		},
	}
}

// placedModel selects the clusters labelled ai-grid/enabled=true.
func placedModel(name string, rollout *aigriddraftv1.Rollout) *aigriddraftv1.Model {
	m := hubModel(name)
	m.Spec.Placement = &aigriddraftv1.Placement{ClusterSets: []string{ClusterSetGlobal}, LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: map[string]string{"ai-grid/enabled": "true"}}}
	m.Spec.Rollout = rollout
	return m
}

// enrolled is a ready cluster labelled for placement.
func enrolled(name string) *aigriddraftv1.ManagedCluster {
	c := readyCluster(name)
	c.Labels["ai-grid/enabled"] = "true"
	c.Labels[LabelClusterSet] = ClusterSetDefault
	return c
}

// modelServer is a server with a fixed, controllable clock and predictable
// work names. The returned function moves the clock.
func modelServer(t *testing.T, fx *Fixtures) (*Server, func(time.Duration)) {
	t.Helper()
	s := newServer(t, fx)
	now := testStart
	s.SetClock(func() time.Time { return now })
	n := 0
	s.SetWorkSuffixGenerator(func() string { n++; return fmt.Sprintf("w%04d", n) })
	return s, func(d time.Duration) { now = testStart.Add(d) }
}

func getModel(t *testing.T, s *Server, name string) *aigriddraftv1.Model {
	t.Helper()
	resp, err := s.GetModel(authed(context.Background()), &aigriddraftv1.GetModelRequest{Namespace: testNS, Name: name})
	if err != nil {
		t.Fatalf("GetModel %s: %v", name, err)
	}
	return resp.GetObject()
}

func createModel(t *testing.T, s *Server, m *aigriddraftv1.Model) *aigriddraftv1.Model {
	t.Helper()
	resp, err := s.CreateModel(authed(context.Background()), &aigriddraftv1.CreateModelRequest{Object: m})
	if err != nil {
		t.Fatalf("CreateModel %s: %v", m.GetName(), err)
	}
	return resp.GetObject()
}

// wantCond asserts one condition's status, reason and message exactly.
func wantCond(t *testing.T, conds []*aigriddraftv1.Condition, typ string, st aigriddraftv1.ConditionStatus, reason, message string) *aigriddraftv1.Condition {
	t.Helper()
	c := condition(conds, typ)
	if c == nil {
		t.Fatalf("condition %s missing in %v", typ, conds)
	}
	if c.GetStatus() != st || c.GetReason() != reason || c.GetMessage() != message {
		t.Errorf("%s = %s/%q/%q, want %s/%q/%q", typ, c.GetStatus(), c.GetReason(), c.GetMessage(), st, reason, message)
	}
	return c
}

func wantNoCond(t *testing.T, conds []*aigriddraftv1.Condition, typ string) {
	t.Helper()
	if condition(conds, typ) != nil {
		t.Errorf("condition %s should be absent", typ)
	}
}

func wantPhase(t *testing.T, m *aigriddraftv1.Model, phase aigriddraftv1.ModelPhase, reason string) {
	t.Helper()
	if m.GetStatus().GetPhase() != phase || m.GetStatus().GetReason() != reason {
		t.Errorf("%s: phase %s/%q, want %s/%q", m.GetName(), m.GetStatus().GetPhase(), m.GetStatus().GetReason(), phase, reason)
	}
}

// knownModelStrings are the strings a fixture model may use: the installed
// presets (the one deliberately unknown ref is listed by name), the hub's
// Gateways, the selector and toleration operators, the cluster sets and the
// rollout strategies the research verified.
func TestRepoModelFixturesUseOnlyKnownStrings(t *testing.T) {
	fx, err := Load(repoFixtures)
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.Models) != 5 || fx.Presets == nil || fx.Presets.Namespace != PresetsNamespace {
		t.Fatalf("expected five models and the presets file, got %d models, presets %+v", len(fx.Models), fx.Presets)
	}
	checkModelsUseOnlyKnownStrings(t, fx, map[string]string{"llama-3-70b": "v3-6-0-kserve-config-llm-template-tpu"})
	if fx.Models[2].GetName() != "mistral-7b-instruct" || fx.Models[2].GetMetadata().GetCreationTimestamp() != nil {
		t.Error("the mid-rollout fixture must omit creationTimestamp so it rolls out live")
	}
}

// checkModelsUseOnlyKnownStrings checks the models in fx against the strings
// the research verified; unknownRefs names the one baseRef per model that is
// deliberately not installed. It runs on every shipped fixture directory.
func checkModelsUseOnlyKnownStrings(t *testing.T, fx *Fixtures, unknownRefs map[string]string) {
	t.Helper()
	for _, want := range []string{PresetTemplate, PresetScheduler, PresetRouterRoute} {
		if !slices.Contains(fx.Presets.Names, want) {
			t.Errorf("presets.json lacks the implicit preset %s", want)
		}
	}
	for _, m := range fx.Models {
		name := m.GetName()
		if m.GetStatus() != nil {
			t.Errorf("%s: status is derived by the server and must not be in the fixture", name)
		}
		if m.GetMetadata().GetAnnotations()[AnnotationDisplayName] == "" {
			t.Errorf("%s: fixtures carry a display name annotation", name)
		}
		for _, ref := range m.GetSpec().GetBaseRefs() {
			if !slices.Contains(fx.Presets.Names, ref) && unknownRefs[name] != ref {
				t.Errorf("%s: baseRef %q is not an installed preset", name, ref)
			}
		}
		for _, gw := range m.GetSpec().GetRouter().GetGatewayRefs() {
			if !knownGateways[gw.GetNamespace()+"/"+gw.GetName()] {
				t.Errorf("%s: gateway %s/%s is not one of the product-default Gateways", name, gw.GetNamespace(), gw.GetName())
			}
		}
		if !slices.ContainsFunc(uriSchemes, func(s string) bool { return strings.HasPrefix(m.GetSpec().GetModel().GetUri(), s) }) {
			t.Errorf("%s: uri %q has no known scheme", name, m.GetSpec().GetModel().GetUri())
		}
		if p := m.GetSpec().GetPlacement(); p != nil {
			if err := validatePlacement(p); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if _, err := validateRollout(m.GetSpec().GetRollout()); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		} else if m.GetSpec().GetRollout() != nil {
			t.Errorf("%s: rollout without placement", name)
		}
	}
	for ns, sets := range fx.ModelBindings {
		for _, set := range sets {
			if !knownClusterSets[set] {
				t.Errorf("bindings[%s]: unknown set %s", ns, set)
			}
		}
	}
}

// TestRepoModelFixturesDeriveTheirStates pins what each shipped model reads
// as against the shipped clusters, at the fake's start and as the scenario
// and the rollout play.
func TestRepoModelFixturesDeriveTheirStates(t *testing.T) {
	fx, err := Load(repoFixtures)
	if err != nil {
		t.Fatal(err)
	}
	s, advance := modelServer(t, fx)
	tiny := getModel(t, s, "tiny-llm")
	wantPhase(t, tiny, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	if tiny.GetStatus().GetUrl() != "https://openshift-ai-inference.apps.hub.example.com/ai-grid-models/tiny-llm" || len(tiny.GetStatus().GetAddresses()) != 6 || len(tiny.GetStatus().GetConditions()) != 9 {
		t.Errorf("tiny-llm = url %q, %d addresses, %d conditions", tiny.GetStatus().GetUrl(), len(tiny.GetStatus().GetAddresses()), len(tiny.GetStatus().GetConditions()))
	}
	if ts := tiny.GetMetadata().GetCreationTimestamp().AsTime(); !ts.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("creationTimestamp %s is the fixture's", ts)
	}
	if ready := condition(tiny.GetStatus().GetConditions(), CondReady); !ready.GetLastTransitionTime().AsTime().Equal(time.Date(2026, 10, 1, 10, 0, 45, 0, time.UTC)) {
		t.Errorf("Ready transitioned at %s, want creation + 45s", ready.GetLastTransitionTime().AsTime())
	}

	granite := getModel(t, s, "granite-8b-instruct")
	wantPhase(t, granite, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	works := granite.GetStatus().GetRollout().GetWorks()
	// generateName is the MWRS name with no separator: model-<name><5 chars>.
	if len(works) != 1 || works[0].GetClusterName() != "site-paris-1" || works[0].GetWorkName() != "model-granite-8b-instructw0001" {
		t.Fatalf("granite works = %v", works)
	}
	wantCond(t, works[0].GetConditions(), CondWorkProgressing, cFalse, ReasonConditionRulesAggregated, MessageRuleProgressing)
	feedback := works[0].GetManifests()[1].GetFeedback()
	if len(feedback) != 2 || feedback[0].GetValue() != "True" || feedback[1].GetValue() != "https://openshift-ai-inference.apps.site-paris-1.example.com/ai-grid-models/granite-8b-instruct" {
		t.Errorf("granite feedback = %v", feedback)
	}
	if sum := granite.GetStatus().GetRollout().GetSummary(); sum.GetTotal() != 1 || sum.GetAvailable() != 1 || sum.GetProgressing() != 0 {
		t.Errorf("granite summary = %v", sum)
	}
	wantCond(t, granite.GetStatus().GetPlacement().GetConditions(), CondPlacementSatisfied, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	wantCond(t, granite.GetStatus().GetPlacement().GetConditions(), CondPlacementMisconfigured, cFalse, ReasonSucceedconfigured, MessageSucceedconfigured)

	llama := getModel(t, s, "llama-3-70b")
	wantPhase(t, llama, aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ReasonConfigNotFound)
	msg := `LLMInferenceServiceConfig "v3-6-0-kserve-config-llm-template-tpu" not found in namespaces [ai-grid-models redhat-ods-applications]`
	wantCond(t, llama.GetStatus().GetConditions(), CondPresetsCombined, cFalse, ReasonConfigNotFound, msg)
	wantCond(t, llama.GetStatus().GetConditions(), CondReady, cFalse, ReasonConfigNotFound, msg)
	wantCond(t, llama.GetStatus().GetConditions(), CondWorkloadsReady, cUnknown, "", "")
	wantCond(t, llama.GetStatus().GetConditions(), CondRouterReady, cUnknown, "", "")
	if len(llama.GetStatus().GetConditions()) != 4 || llama.GetStatus().GetMessage() != msg {
		t.Errorf("llama stops at the preset merge: %v", llama.GetStatus())
	}

	code := getModel(t, s, "codellama-13b")
	wantPhase(t, code, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonPlacementDecisionEmpty)
	wantCond(t, code.GetStatus().GetPlacement().GetConditions(), CondPlacementSatisfied, cFalse, ReasonNoManagedClusterMatched, MessageNoManagedClusterMatched)
	wantCond(t, code.GetStatus().GetRollout().GetConditions(), CondPlacementVerified, cFalse, ReasonPlacementDecisionEmpty, "")
	wantCond(t, code.GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cFalse, ReasonPlacementDecisionEmpty, "")
	// Succeeded (0) equals selected (0): the controller writes Complete for an
	// empty decision.
	wantCond(t, code.GetStatus().GetRollout().GetConditions(), CondPlacementRolledOut, cTrue, ReasonRolloutComplete, "")
	if code.GetStatus().GetPlacement().GetNumberOfSelectedClusters() != 0 || len(code.GetStatus().GetRollout().GetWorks()) != 0 {
		t.Errorf("codellama selects nothing: %v", code.GetStatus())
	}

	// The live Progressive rollout: frankfurt first, paris once frankfurt has
	// soaked 30s, berlin after it joins at 20s and the slot frees.
	mistral := getModel(t, s, "mistral-7b-instruct")
	wantPhase(t, mistral, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonNotAsExpected)
	if !mistral.GetMetadata().GetCreationTimestamp().AsTime().Equal(testStart) {
		t.Errorf("the live fixture is created at start, got %s", mistral.GetMetadata().GetCreationTimestamp().AsTime())
	}
	if works := mistral.GetStatus().GetRollout().GetWorks(); len(works) != 1 || works[0].GetClusterName() != "site-frankfurt-1" {
		t.Fatalf("t0 works = %v", works)
	}
	advance(50 * time.Second)
	mistral = getModel(t, s, "mistral-7b-instruct")
	wantPhase(t, mistral, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonRolloutProgressing)
	if d := mistral.GetStatus().GetPlacement().GetDecisions(); len(d) != 3 || d[0].GetClusterName() != "site-berlin-1" {
		t.Errorf("berlin joined the decision at 20s: %v", d)
	}
	if len(mistral.GetStatus().GetRollout().GetWorks()) != 1 {
		t.Errorf("paris waits for the 30s soak: %v", mistral.GetStatus().GetRollout().GetWorks())
	}
	advance(80 * time.Second)
	mistral = getModel(t, s, "mistral-7b-instruct")
	// Progressive fills the slot in cluster-name order among the clusters
	// still to apply: berlin, which joined at 20s, goes before paris.
	if works := mistral.GetStatus().GetRollout().GetWorks(); len(works) != 2 || works[0].GetClusterName() != "site-berlin-1" || !condition(works[0].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(75*time.Second)) {
		t.Errorf("berlin is applied at 45s + 30s soak: %v", works)
	}
	advance(200 * time.Second)
	mistral = getModel(t, s, "mistral-7b-instruct")
	wantPhase(t, mistral, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	if sum := mistral.GetStatus().GetRollout().GetSummary(); sum.GetTotal() != 3 || sum.GetAvailable() != 3 {
		t.Errorf("summary = %v", sum)
	}
	if works := mistral.GetStatus().GetRollout().GetWorks(); works[2].GetClusterName() != "site-paris-1" || !condition(works[2].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(150*time.Second)) {
		t.Errorf("paris is applied once berlin succeeded (120s) and soaked 30s: %v", works)
	}

	// madrid recovers its lease at 300s and joins both placements; its works
	// are applied at the event's time even though the fake was polled later,
	// and both models return to pending.
	advance(310 * time.Second)
	mistral = getModel(t, s, "mistral-7b-instruct")
	if d := mistral.GetStatus().GetPlacement().GetDecisions(); len(d) != 4 {
		t.Errorf("madrid joins mistral's decision too: %v", d)
	}
	if w := mistral.GetStatus().GetRollout().GetWorks(); len(w) != 4 || w[2].GetClusterName() != "site-madrid-1" || !condition(w[2].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(300*time.Second)) {
		t.Errorf("mistral's madrid work = %v", w)
	}
	wantPhase(t, mistral, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonProcessing)
	granite = getModel(t, s, "granite-8b-instruct")
	if d := granite.GetStatus().GetPlacement().GetDecisions(); len(d) != 2 || d[0].GetClusterName() != "site-madrid-1" {
		t.Errorf("madrid joins once its taint is gone: %v", d)
	}
	if w := granite.GetStatus().GetRollout().GetWorks()[0]; w.GetClusterName() != "site-madrid-1" || !condition(w.GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(300*time.Second)) {
		t.Errorf("madrid's work = %v", w)
	}
	wantPhase(t, granite, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonProcessing)
	// madrid's spokes are Ready at 345s: both models are Ready, granite on two
	// sites and mistral on four.
	advance(350 * time.Second)
	granite, mistral = getModel(t, s, "granite-8b-instruct"), getModel(t, s, "mistral-7b-instruct")
	wantPhase(t, granite, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	wantPhase(t, mistral, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	if len(granite.GetStatus().GetRollout().GetWorks()) != 2 || len(mistral.GetStatus().GetRollout().GetWorks()) != 4 {
		t.Errorf("works: granite %d, mistral %d", len(granite.GetStatus().GetRollout().GetWorks()), len(mistral.GetStatus().GetRollout().GetWorks()))
	}
}

func TestLLMISvcLifecycle(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{})
	created := createModel(t, s, hubModel("tiny"))
	if len(created.GetStatus().GetConditions()) != 0 || created.GetStatus().GetPhase() != aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING {
		t.Fatalf("a created model has no conditions yet: %v", created.GetStatus())
	}
	if created.GetMetadata().GetGeneration() != 1 || created.GetMetadata().GetUid() == "" || created.GetMetadata().GetResourceVersion() == "" || !created.GetMetadata().GetCreationTimestamp().AsTime().Equal(testStart) {
		t.Errorf("metadata = %v", created.GetMetadata())
	}

	// T1, the first reconcile: workload and router in one pass (the workload
	// first, so Ready ends with the router's reason), `url` and `addresses`
	// as soon as the route exists, `workloads` with nothing ready.
	advance(llmisvcT1)
	m := getModel(t, s, "tiny")
	conds := m.GetStatus().GetConditions()
	routesMsg := "The following HTTPRoutes exist but no Gateway controller has accepted them yet: [ai-grid-models/tiny-kserve-route]"
	wantCond(t, conds, CondPresetsCombined, cTrue, "", "")
	wantCond(t, conds, CondGatewaysReady, cTrue, "", "")
	wantCond(t, conds, CondHTTPRoutesReady, cFalse, ReasonWaitingForGateway, routesMsg)
	wantCond(t, conds, CondInferencePoolReady, cFalse, ReasonWaitingForGateway, "Inference Pool ai-grid-models/tiny-inference-pool exists but no Gateway controller has accepted it yet")
	wantCond(t, conds, CondSchedulerWorkloadReady, cFalse, ReasonProgressing, MessageDeploymentRolloutInProgress)
	wantCond(t, conds, CondRouterReady, cFalse, ReasonWaitingForGateway, routesMsg)
	wantCond(t, conds, CondMainWorkloadReady, cFalse, ReasonProgressing, "")
	wantCond(t, conds, CondWorkloadsReady, cFalse, ReasonProgressing, "")
	wantCond(t, conds, CondReady, cFalse, ReasonWaitingForGateway, routesMsg)
	if len(conds) != 9 {
		t.Errorf("%d conditions at T1, want 9", len(conds))
	}
	// Ready False with a starting reason is PENDING, not FAILED: the model is
	// coming up (deriveHubPhase).
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonWaitingForGateway)
	if m.GetStatus().GetObservedGeneration() != 1 {
		t.Errorf("observedGeneration = %d", m.GetStatus().GetObservedGeneration())
	}
	if m.GetStatus().GetUrl() != "https://openshift-ai-inference.apps.hub.example.com/ai-grid-models/tiny" || len(m.GetStatus().GetAddresses()) != 6 {
		t.Errorf("url and addresses come with the route, before Ready: %q %v", m.GetStatus().GetUrl(), m.GetStatus().GetAddresses())
	}
	if w := m.GetStatus().GetWorkloads(); w.GetPrimary().GetName() != "tiny-kserve" || w.GetPrimary().GetReadyReplicas() != 0 || w.GetScheduler().GetName() != "tiny-kserve-router-scheduler" || w.GetScheduler().GetReadyReplicas() != 0 || w.GetService().GetName() != "tiny-kserve-workload-svc" || w.GetService().GetKind() != "Service" {
		t.Errorf("workloads = %v", w)
	}
	wantApplied := []string{PresetScheduler + "/Preset", PresetRouterRoute + "/Preset", PresetTemplate + "/Preset", "v3-6-0-kserve-config-llm-template-cpu/UserRef"}
	var gotApplied []string
	for _, c := range m.GetStatus().GetAppliedConfigs() {
		gotApplied = append(gotApplied, c.GetName()+"/"+c.GetSource())
		if c.GetNamespace() != PresetsNamespace {
			t.Errorf("appliedConfig namespace %q", c.GetNamespace())
		}
	}
	if !slices.Equal(gotApplied, wantApplied) {
		t.Errorf("appliedConfigs = %v, want %v", gotApplied, wantApplied)
	}
	for i := 1; i < len(conds); i++ {
		if conds[i-1].GetType() >= conds[i].GetType() {
			t.Errorf("conditions are sorted by type like Knative writes them: %q before %q", conds[i-1].GetType(), conds[i].GetType())
		}
	}

	advance(llmisvcT2)
	m = getModel(t, s, "tiny")
	conds = m.GetStatus().GetConditions()
	wantCond(t, conds, CondHTTPRoutesReady, cTrue, "", "")
	wantCond(t, conds, CondInferencePoolReady, cTrue, "", "")
	wantCond(t, conds, CondSchedulerWorkloadReady, cTrue, "", "")
	wantCond(t, conds, CondRouterReady, cTrue, "", "")
	wantCond(t, conds, CondMainWorkloadReady, cFalse, ReasonProgressing, "")
	wantCond(t, conds, CondWorkloadsReady, cFalse, ReasonProgressing, "")
	ready := wantCond(t, conds, CondReady, cFalse, ReasonProgressing, "")
	// Knative moves lastTransitionTime on any change of reason or message,
	// not only of status.
	if !ready.GetLastTransitionTime().AsTime().Equal(testStart.Add(llmisvcT2)) {
		t.Errorf("Ready changed reason at T2, its transition time must move: %s", ready.GetLastTransitionTime().AsTime())
	}
	if lt := condition(conds, CondMainWorkloadReady).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT1)) {
		t.Errorf("MainWorkloadReady did not change at T2, got %s", lt)
	}
	if w := m.GetStatus().GetWorkloads(); w.GetPrimary().GetReadyReplicas() != 0 || w.GetScheduler().GetReadyReplicas() != 1 {
		t.Errorf("the scheduler is available at T2, the pods are not: %v", w)
	}

	advance(llmisvcT2b)
	conds = getModel(t, s, "tiny").GetStatus().GetConditions()
	wantCond(t, conds, CondMainWorkloadReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
	wantCond(t, conds, CondWorkloadsReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
	wantCond(t, conds, CondReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
	for _, typ := range []string{CondReady, CondMainWorkloadReady, CondWorkloadsReady} {
		if lt := condition(conds, typ).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT2b)) {
			t.Errorf("%s stayed False but changed reason at T2b: %s", typ, lt)
		}
	}

	advance(llmisvcT3)
	m = getModel(t, s, "tiny")
	conds = m.GetStatus().GetConditions()
	for _, typ := range []string{CondPresetsCombined, CondGatewaysReady, CondHTTPRoutesReady, CondInferencePoolReady, CondSchedulerWorkloadReady, CondRouterReady, CondMainWorkloadReady, CondWorkloadsReady, CondReady} {
		wantCond(t, conds, typ, cTrue, "", "")
	}
	if len(conds) != 9 {
		t.Errorf("%d conditions, want the hub instance's nine", len(conds))
	}
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	if lt := condition(conds, CondPresetsCombined).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT1)) {
		t.Errorf("PresetsCombined transitioned once, at T1, got %s", lt)
	}
	if lt := condition(conds, CondReady).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT3)) {
		t.Errorf("Ready transitioned at T3, got %s", lt)
	}
	st := m.GetStatus()
	if st.GetUrl() != "https://openshift-ai-inference.apps.hub.example.com/ai-grid-models/tiny" {
		t.Errorf("url = %q", st.GetUrl())
	}
	// Sorted by URL string (the internal Service host sorts first), the
	// internal host being the Gateway's `<gateway>-<gatewayClassName>` Service.
	internal := "https://openshift-ai-inference-openshift-default.openshift-ingress.svc.cluster.local"
	external := "https://openshift-ai-inference.apps.hub.example.com"
	wantAddresses := []string{
		AddressGatewayInternalModelRouting + " " + internal + "/ [publishers/ai-grid-models/models/example-org/tiny]",
		AddressGatewayInternal + " " + internal + "/ai-grid-models/tiny [publishers/ai-grid-models/models/example-org/tiny example-org/tiny]",
		AddressGatewayInternal + " " + internal + "/publishers/ai-grid-models/models/example-org/tiny [publishers/ai-grid-models/models/example-org/tiny example-org/tiny]",
		AddressGatewayExternalModelRouting + " " + external + "/ [publishers/ai-grid-models/models/example-org/tiny]",
		AddressGatewayExternal + " " + external + "/ai-grid-models/tiny [publishers/ai-grid-models/models/example-org/tiny example-org/tiny]",
		AddressGatewayExternal + " " + external + "/publishers/ai-grid-models/models/example-org/tiny [publishers/ai-grid-models/models/example-org/tiny example-org/tiny]",
	}
	var gotAddresses []string
	for _, a := range st.GetAddresses() {
		gotAddresses = append(gotAddresses, fmt.Sprintf("%s %s %v", a.GetName(), a.GetUrl(), a.GetModels()))
		if a.GetOrigin().GetKind() != GatewayKind || a.GetOrigin().GetName() != GatewayAIInference || a.GetOrigin().GetGroup() != GatewayGroup {
			t.Errorf("origin = %v", a.GetOrigin())
		}
	}
	if !slices.Equal(gotAddresses, wantAddresses) {
		t.Errorf("addresses = %q, want %q", gotAddresses, wantAddresses)
	}
	if st.GetWorkloads().GetPrimary().GetReadyReplicas() != 1 || st.GetWorkloads().GetScheduler().GetReadyReplicas() != 1 {
		t.Errorf("workloads = %v", st.GetWorkloads())
	}
}

// TestZeroReplicasIsReadyAtOnce: a Deployment with `replicas: 0` is Available
// at its first status sync (0 >= 0 - maxUnavailable), so KServe never reports
// MinimumReplicasUnavailable, even for an image that would never start.
func TestZeroReplicasIsReadyAtOnce(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{})
	for _, name := range []string{"zero", "zero-oom"} {
		m := hubModel(name)
		m.Spec.Replicas = proto.Int32(0)
		if name == "zero-oom" {
			m.Spec.Main.Image = "quay.io/example/oom:1"
		}
		createModel(t, s, m)
	}
	advance(llmisvcT2 + time.Second)
	for _, name := range []string{"zero", "zero-oom"} {
		m := getModel(t, s, name)
		wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
		wantCond(t, m.GetStatus().GetConditions(), CondMainWorkloadReady, cTrue, "", "")
		if r := m.GetStatus().GetWorkloads().GetPrimary().GetReadyReplicas(); r != 0 {
			t.Errorf("%s readyReplicas = %d", name, r)
		}
	}
	advance(llmisvcT3 + time.Second)
	for _, name := range []string{"zero", "zero-oom"} {
		m := getModel(t, s, name)
		wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
		if lt := condition(m.GetStatus().GetConditions(), CondMainWorkloadReady).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT2)) {
			t.Errorf("%s MainWorkloadReady True since T2, got %s", name, lt)
		}
	}
}

func TestLLMISvcFailureModes(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{})
	bad := hubModel("bad-preset")
	bad.Spec.BaseRefs = []string{"v3-6-0-kserve-config-llm-template-tpu"}
	createModel(t, s, bad)
	stuck := hubModel("stuck")
	stuck.Spec.Main.Image = "quay.io/example/vllm-OOM:latest"
	createModel(t, s, stuck)
	gw := hubModel("bad-gateway")
	gw.Spec.Router.GatewayRefs = []*aigriddraftv1.GatewayRef{{Name: "edge-gateway", Namespace: GatewayNamespace}}
	createModel(t, s, gw)
	gws := hubModel("bad-gateways")
	gws.Spec.Router.GatewayRefs = []*aigriddraftv1.GatewayRef{{Name: GatewayAIInference, Namespace: GatewayNamespace}, {Name: "a"}, {Name: "b", Namespace: "edge"}}
	createModel(t, s, gws)
	defaulted := hubModel("defaulted")
	defaulted.Spec.Router = nil
	defaulted.Spec.Model.Name = ""
	createModel(t, s, defaulted)
	noRefs := hubModel("no-refs")
	noRefs.Spec.Router = &aigriddraftv1.Router{ManagedRoute: true}
	createModel(t, s, noRefs)

	advance(llmisvcT3 + time.Second)
	m := getModel(t, s, "bad-preset")
	msg := `LLMInferenceServiceConfig "v3-6-0-kserve-config-llm-template-tpu" not found in namespaces [ai-grid-models redhat-ods-applications]`
	wantCond(t, m.GetStatus().GetConditions(), CondPresetsCombined, cFalse, ReasonConfigNotFound, msg)
	wantCond(t, m.GetStatus().GetConditions(), CondReady, cFalse, ReasonConfigNotFound, msg)
	// Knative initialises the other dependents of Ready to Unknown before the reconcile.
	wantCond(t, m.GetStatus().GetConditions(), CondWorkloadsReady, cUnknown, "", "")
	wantCond(t, m.GetStatus().GetConditions(), CondRouterReady, cUnknown, "", "")
	if len(m.GetStatus().GetConditions()) != 4 || len(m.GetStatus().GetAppliedConfigs()) != 0 || m.GetStatus().GetWorkloads() != nil || m.GetStatus().GetUrl() != "" {
		t.Errorf("a bad preset stops the controller before anything else: %v", m.GetStatus())
	}
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ReasonConfigNotFound)

	// A referenced Gateway that does not exist: RefsInvalid, the router stops
	// before the scheduler and the route, the workload still deploys.
	m = getModel(t, s, "bad-gateway")
	gwMsg := "Gateway openshift-ingress/edge-gateway does not exist"
	conds := m.GetStatus().GetConditions()
	wantCond(t, conds, CondGatewaysReady, cFalse, ReasonRefsInvalid, gwMsg)
	wantCond(t, conds, CondRouterReady, cFalse, ReasonRefsInvalid, gwMsg)
	wantCond(t, conds, CondReady, cFalse, ReasonRefsInvalid, gwMsg)
	wantCond(t, conds, CondMainWorkloadReady, cTrue, "", "")
	wantCond(t, conds, CondWorkloadsReady, cTrue, "", "")
	for _, typ := range []string{CondHTTPRoutesReady, CondInferencePoolReady, CondSchedulerWorkloadReady} {
		wantNoCond(t, conds, typ)
	}
	if st := m.GetStatus(); st.GetUrl() != "" || len(st.GetAddresses()) != 0 || st.GetWorkloads() != nil || len(st.GetAppliedConfigs()) != 4 {
		t.Errorf("no route, no url, no workloads (observeWorkloadStatus is not reached): %v", st)
	}
	if lt := condition(conds, CondReady).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT1)) {
		t.Errorf("Ready stayed RefsInvalid since T1, got %s", lt)
	}
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ReasonRefsInvalid)
	wantCond(t, getModel(t, s, "bad-gateways").GetStatus().GetConditions(), CondGatewaysReady, cFalse, ReasonRefsInvalid,
		"Gateway ai-grid-models/a does not exist; Gateway edge/b does not exist")

	// No router at all: no route, the workload Service as the only address,
	// no url, and only the roll-ups on the router side.
	m = getModel(t, s, "defaulted")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	if m.GetSpec().GetModel().GetName() != "defaulted" {
		t.Errorf("spec.model.name defaults to the name, got %q", m.GetSpec().GetModel().GetName())
	}
	if len(m.GetStatus().GetConditions()) != 5 || m.GetStatus().GetWorkloads().GetScheduler() != nil || m.GetStatus().GetUrl() != "" {
		t.Errorf("without a router there is no route, pool or scheduler: %v", m.GetStatus())
	}
	if a := m.GetStatus().GetAddresses(); len(a) != 1 || a[0].GetUrl() != "https://defaulted-kserve-workload-svc.ai-grid-models.svc.cluster.local" || a[0].GetName() != "" {
		t.Errorf("addresses = %v", a)
	}
	// A managed route without Gateway refs attaches to the default Gateway and
	// reports no GatewaysReady.
	m = getModel(t, s, "no-refs")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	wantNoCond(t, m.GetStatus().GetConditions(), CondGatewaysReady)
	if len(m.GetStatus().GetConditions()) != 6 || m.GetStatus().GetAddresses()[0].GetOrigin().GetName() != GatewayAIInference {
		t.Errorf("no refs: %v", m.GetStatus())
	}

	// A workload that never becomes available keeps MinimumReplicasUnavailable:
	// KServe ends on the Deployment's Available condition, so the progress
	// deadline never shows.
	for _, d := range []time.Duration{llmisvcT3 + time.Second, llmisvcT2 + 600*time.Second + time.Second, 24 * time.Hour} {
		advance(d)
		m = getModel(t, s, "stuck")
		wantCond(t, m.GetStatus().GetConditions(), CondMainWorkloadReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
		ready := wantCond(t, m.GetStatus().GetConditions(), CondReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
		wantCond(t, m.GetStatus().GetConditions(), CondRouterReady, cTrue, "", "")
		if !ready.GetLastTransitionTime().AsTime().Equal(testStart.Add(llmisvcT2b)) {
			t.Errorf("at %s: Ready has not changed since T2b, got %s", d, ready.GetLastTransitionTime().AsTime())
		}
		// No condition tells this from a slow start, so it stays PENDING with
		// the reason, as a stuck spoke does.
		wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonMinimumReplicasUnavailable)
		if m.GetStatus().GetUrl() == "" || m.GetStatus().GetWorkloads().GetPrimary().GetReadyReplicas() != 0 {
			t.Errorf("at %s: the route exists, the pods are not ready: %v", d, m.GetStatus())
		}
	}
}

func TestSpeedScalesTheModelLifecycle(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{Speed: 10})
	createModel(t, s, hubModel("fast"))
	advance(4 * time.Second) // 40 scenario seconds
	wantPhase(t, getModel(t, s, "fast"), aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonMinimumReplicasUnavailable)
	advance(5 * time.Second) // 50 scenario seconds
	m := getModel(t, s, "fast")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	if lt := condition(m.GetStatus().GetConditions(), CondReady).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(4500 * time.Millisecond)) {
		t.Errorf("Ready at wall %s, want start + 4.5s", lt)
	}
}

func TestCreateModelValidation(t *testing.T) {
	s, _ := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a")}})
	createModel(t, s, hubModel("taken"))
	ctx := authed(context.Background())
	trigger := []*aigriddraftv1.KedaTrigger{{Type: "prometheus", Metadata: map[string]string{"query": "vllm:num_requests_waiting"}}}
	create := func(mutate func(m *aigriddraftv1.Model)) error {
		m := hubModel("candidate")
		mutate(m)
		_, err := s.CreateModel(ctx, &aigriddraftv1.CreateModelRequest{Object: m})
		return err
	}
	cases := []struct {
		name   string
		mutate func(m *aigriddraftv1.Model)
		code   codes.Code
		msg    string
	}{
		{"replicas and scaling", func(m *aigriddraftv1.Model) {
			m.Spec.Scaling = &aigriddraftv1.Scaling{MaxReplicas: 3, Keda: true, Triggers: trigger}
		}, codes.InvalidArgument, MsgReplicasScalingExclusive},
		{"scaling without keda", func(m *aigriddraftv1.Model) {
			m.Spec.Replicas = nil
			m.Spec.Scaling = &aigriddraftv1.Scaling{MaxReplicas: 3}
		}, codes.InvalidArgument, MsgScalingNeedsKeda},
		{"keda without triggers", func(m *aigriddraftv1.Model) {
			m.Spec.Replicas = nil
			m.Spec.Scaling = &aigriddraftv1.Scaling{MaxReplicas: 3, Keda: true}
		}, codes.InvalidArgument, MsgKedaTriggerRequired},
		{"scaling without max", func(m *aigriddraftv1.Model) {
			m.Spec.Replicas = nil
			m.Spec.Scaling = &aigriddraftv1.Scaling{Keda: true, Triggers: trigger}
		}, codes.InvalidArgument, MsgMaxReplicasRequired},
		{"negative max", func(m *aigriddraftv1.Model) {
			m.Spec.Replicas = nil
			m.Spec.Scaling = &aigriddraftv1.Scaling{MaxReplicas: -2, Keda: true, Triggers: trigger}
		}, codes.InvalidArgument, "spec.scaling.maxReplicas: Invalid value: -2: spec.scaling.maxReplicas in body should be greater than or equal to 1"},
		{"negative min", func(m *aigriddraftv1.Model) {
			m.Spec.Replicas = nil
			m.Spec.Scaling = &aigriddraftv1.Scaling{MinReplicas: -1, MaxReplicas: 3, Keda: true, Triggers: trigger}
		}, codes.InvalidArgument, "spec.scaling.minReplicas: Invalid value: -1: spec.scaling.minReplicas in body should be greater than or equal to 1"},
		{"negative replicas", func(m *aigriddraftv1.Model) { m.Spec.Replicas = proto.Int32(-1) }, codes.InvalidArgument, "spec.replicas: Invalid value: -1: spec.replicas in body should be greater than or equal to 0"},
		{"min above max", func(m *aigriddraftv1.Model) {
			m.Spec.Replicas = nil
			m.Spec.Scaling = &aigriddraftv1.Scaling{MinReplicas: 5, MaxReplicas: 3, Keda: true, Triggers: trigger}
		}, codes.InvalidArgument, MsgMinMaxReplicas},
		{"uri missing", func(m *aigriddraftv1.Model) { m.Spec.Model.Uri = "" }, codes.InvalidArgument, MsgModelURIRequired},
		{"uri scheme", func(m *aigriddraftv1.Model) { m.Spec.Model.Uri = "https://example.com/model" }, codes.InvalidArgument, `spec.model.uri "https://example.com/model": unsupported scheme; expected one of hf://, s3://, pvc://, oci://`},
		{"name case", func(m *aigriddraftv1.Model) { m.Name = "Tiny" }, codes.InvalidArgument, fmt.Sprintf(MsgDNS1123SubdomainFm, "metadata.name", "Tiny")},
		{"name empty", func(m *aigriddraftv1.Model) { m.Name = "" }, codes.InvalidArgument, "metadata.name is required"},
		{"namespace empty", func(m *aigriddraftv1.Model) { m.Metadata.Namespace = "" }, codes.InvalidArgument, "metadata.namespace is required"},
		{"namespace dotted", func(m *aigriddraftv1.Model) { m.Metadata.Namespace = "a.b" }, codes.InvalidArgument, fmt.Sprintf(MsgDNS1123LabelFm, "metadata.namespace", "a.b")},
		{"duplicate", func(m *aigriddraftv1.Model) { m.Name = "taken" }, codes.AlreadyExists, `llminferenceservices.serving.kserve.io "taken" already exists`},
		{"rollout without placement", func(m *aigriddraftv1.Model) { m.Spec.Rollout = &aigriddraftv1.Rollout{} }, codes.InvalidArgument, MsgRolloutNoPlacement},
		{"bad operator", func(m *aigriddraftv1.Model) {
			m.Spec.Placement = &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchExpressions: []*aigriddraftv1.LabelSelectorRequirement{{Key: "k", Operator: "Equals"}}}}
		}, codes.InvalidArgument, `spec.placement.label_selector.match_expressions: operator "Equals" is not In, NotIn, Exists or DoesNotExist`},
		{"bad cluster set", func(m *aigriddraftv1.Model) {
			m.Spec.Placement = &aigriddraftv1.Placement{ClusterSets: []string{"edge"}}
		}, codes.InvalidArgument, `spec.placement.cluster_sets: ManagedClusterSet "edge" does not exist on the hub`},
		{"bad deadline", func(m *aigriddraftv1.Model) {
			m.Spec.Placement = &aigriddraftv1.Placement{}
			m.Spec.Rollout = &aigriddraftv1.Rollout{ProgressDeadline: "soon"}
		}, codes.InvalidArgument, `spec.rollout.progress_deadline: "soon" is not a duration such as 5m or 1h, or None`},
		{"fractional deadline", func(m *aigriddraftv1.Model) {
			// The CRD pattern ^(([0-9])+[h|m|s])|None$ rejects it.
			m.Spec.Placement = &aigriddraftv1.Placement{}
			m.Spec.Rollout = &aigriddraftv1.Rollout{ProgressDeadline: "1.5h"}
		}, codes.InvalidArgument, `spec.rollout.progress_deadline: "1.5h" is not a duration such as 5m or 1h, or None`},
		{"fractional percent", func(m *aigriddraftv1.Model) {
			m.Spec.Placement = &aigriddraftv1.Placement{}
			m.Spec.Rollout = &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxFailures: "12.5%"}
		}, codes.InvalidArgument, `spec.rollout.max_failures: "12.5%" is not a percentage between 0% and 100%`},
		{"concurrency for All", func(m *aigriddraftv1.Model) {
			m.Spec.Placement = &aigriddraftv1.Placement{}
			m.Spec.Rollout = &aigriddraftv1.Rollout{MaxConcurrency: "1"}
		}, codes.InvalidArgument, "spec.rollout.max_concurrency applies only to the Progressive strategy; All and ProgressivePerGroup have no maxConcurrency"},
		{"concurrency per group", func(m *aigriddraftv1.Model) {
			m.Spec.Placement = &aigriddraftv1.Placement{}
			m.Spec.Rollout = &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE_PER_GROUP, MaxConcurrency: "1"}
		}, codes.InvalidArgument, "spec.rollout.max_concurrency applies only to the Progressive strategy; All and ProgressivePerGroup have no maxConcurrency"},
		{"bad percent", func(m *aigriddraftv1.Model) {
			m.Spec.Placement = &aigriddraftv1.Placement{}
			m.Spec.Rollout = &aigriddraftv1.Rollout{MaxConcurrency: "150%"}
		}, codes.InvalidArgument, `spec.rollout.max_concurrency: "150%" is not a percentage between 0% and 100%`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustCode(t, create(tc.mutate), tc.code, tc.msg) })
	}
	if _, err := s.CreateModel(ctx, &aigriddraftv1.CreateModelRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no object = %v", err)
	}
	// A custom resource name is a DNS-1123 subdomain: dots and up to 253 characters.
	if err := create(func(m *aigriddraftv1.Model) { m.Name = "granite.3b-" + strings.Repeat("x", 60) }); err != nil {
		t.Errorf("a dotted, 71-character name is a valid subdomain, got %v", err)
	}
	if err := create(func(m *aigriddraftv1.Model) {
		m.Name = "scaled"
		m.Spec.Replicas = nil
		m.Spec.Scaling = &aigriddraftv1.Scaling{MinReplicas: 1, MaxReplicas: 4, Keda: true, Triggers: trigger}
	}); err != nil {
		t.Errorf("a complete scaling block is accepted, got %v", err)
	}
	// The scheduler, not admission, rejects an empty toleration key without
	// Exists (PlacementMisconfigured); `0s` is a valid, immediate deadline.
	if err := create(func(m *aigriddraftv1.Model) {
		m.Name = "misconfigured"
		m.Spec.Placement = &aigriddraftv1.Placement{Tolerations: []*aigriddraftv1.Toleration{{Operator: TolerationEqual}}}
		m.Spec.Rollout = &aigriddraftv1.Rollout{ProgressDeadline: "0s"}
	}); err != nil {
		t.Errorf("an empty-key toleration and a 0s deadline pass admission, got %v", err)
	}
	// Unknown presets pass admission: the controller reports them.
	if err := create(func(m *aigriddraftv1.Model) { m.Name = "unknown-preset"; m.Spec.BaseRefs = []string{"nope"} }); err != nil {
		t.Errorf("an unknown baseRef is accepted by the API server, got %v", err)
	}
}

func TestPlacedModelNeedsTheFeatureGate(t *testing.T) {
	off := false
	fx := &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a")}, ManifestWorkReplicaSetEnabled: &off, Models: []*aigriddraftv1.Model{placedModel("seeded", nil)}}
	s, _ := modelServer(t, fx)
	ctx := authed(context.Background())
	caps, err := s.GetHubCapabilities(ctx, &aigriddraftv1.GetHubCapabilitiesRequest{})
	if err != nil || caps.GetCapabilities().GetManifestWorkReplicaSetEnabled() || caps.GetCapabilities().GetAcmVersion() != defaultACMVersion {
		t.Fatalf("capabilities = %v, %v", caps, err)
	}
	_, err = s.CreateModel(ctx, &aigriddraftv1.CreateModelRequest{Object: placedModel("placed", nil)})
	mustCode(t, err, codes.FailedPrecondition, MessageFeatureGateDisabled)
	createModel(t, s, hubModel("hub-only")) // a hub deployment does not need the gate
	// A stored MWRS nobody reconciles: decisions, but no works and no status.
	seeded := getModel(t, s, "seeded")
	if seeded.GetStatus().GetPlacement().GetNumberOfSelectedClusters() != 1 || len(seeded.GetStatus().GetRollout().GetWorks()) != 0 || len(seeded.GetStatus().GetRollout().GetConditions()) != 0 {
		t.Errorf("gate off: %v", seeded.GetStatus())
	}
	wantPhase(t, seeded, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, "")
	if seeded.GetStatus().GetMessage() != MessageFeatureGateDisabled {
		t.Errorf("message = %q", seeded.GetStatus().GetMessage())
	}
	s.SetManifestWorkReplicaSetEnabled(true)
	createModel(t, s, placedModel("placed", nil))
	if _, err := s.GetHubCapabilities(context.Background(), &aigriddraftv1.GetHubCapabilitiesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("GetHubCapabilities needs authorization, got %v", err)
	}
}

func TestModelMutationsNeedAProviderAdmin(t *testing.T) {
	fx := &Fixtures{Access: Access{DefaultAllowed: true, DeniedTokens: []string{"viewer-token"}, RejectedTokens: []string{"expired-token"}}, Models: []*aigriddraftv1.Model{hubModel("seeded")}}
	fx.Models[0].Metadata.CreationTimestamp = timestamppb.New(testStart.Add(-time.Hour))
	s, _ := modelServer(t, fx)
	viewer := metadataCtx("viewer-token", "viewer")
	if _, err := s.ListModels(viewer, &aigriddraftv1.ListModelsRequest{}); err != nil {
		t.Errorf("a viewer reads: %v", err)
	}
	wantDenied := `User "viewer" is not a provider admin, and only provider admins can change models (a provider admin may patch ManagedClusters on the hub)`
	_, err := s.CreateModel(viewer, &aigriddraftv1.CreateModelRequest{Object: hubModel("new")})
	mustCode(t, err, codes.PermissionDenied, wantDenied)
	_, err = s.UpdateModel(viewer, &aigriddraftv1.UpdateModelRequest{Object: hubModel("seeded"), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathSpecReplicas}}})
	mustCode(t, err, codes.PermissionDenied, wantDenied)
	_, err = s.DeleteModel(viewer, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "seeded"})
	mustCode(t, err, codes.PermissionDenied, wantDenied)
	anon := context.Background()
	for name, call := range map[string]func() error{
		"List": func() error { _, err := s.ListModels(anon, &aigriddraftv1.ListModelsRequest{}); return err },
		"Get": func() error {
			_, err := s.GetModel(anon, &aigriddraftv1.GetModelRequest{Namespace: testNS, Name: "seeded"})
			return err
		},
		"Create": func() error {
			_, err := s.CreateModel(anon, &aigriddraftv1.CreateModelRequest{Object: hubModel("x")})
			return err
		},
		"Delete": func() error {
			_, err := s.DeleteModel(anon, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "seeded"})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s without authorization = %v", name, err)
		}
	}
	if _, err := s.ListModels(metadataCtx("expired-token", "x"), &aigriddraftv1.ListModelsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("a rejected token reads nothing: %v", err)
	}
}

func metadataCtx(token, user string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token, "x-forwarded-user", user))
}

func TestComputePlacement(t *testing.T) {
	now := testStart
	ident := func(t time.Time) time.Time { return t }
	a := enrolled("site-a")
	a.Labels["ai-grid/region"] = "europe-west"
	a.Status.ClusterClaims = []*aigriddraftv1.ClusterClaim{{Name: ClaimPlatform, Value: "BareMetal"}}
	b := enrolled("site-b")
	delete(b.Labels, LabelClusterSet)
	b.Labels["ai-grid/region"] = "us-east"
	c := readyCluster("site-c") // not enrolled
	d := enrolled("site-d")     // unreachable for 100s
	d.Taints = []*aigriddraftv1.Taint{{Key: TaintUnreachable, Effect: TaintNoSelect, TimeAdded: timestamppb.New(now.Add(-100 * time.Second))}}
	e := enrolled("site-e") // terminating
	e.DeletionTimestamp = timestamppb.New(now)
	f := enrolled("site-f") // carries an empty-valued label
	f.Labels["dedicated"] = ""
	clusters := map[string]*aigriddraftv1.ManagedCluster{"site-a": a, "site-b": b, "site-c": c, "site-d": d, "site-e": e, "site-f": f}
	global := []string{ClusterSetGlobal}
	enabledSel := &aigriddraftv1.LabelSelector{MatchLabels: map[string]string{"ai-grid/enabled": "true"}}
	names := func(st *aigriddraftv1.PlacementStatus) []string {
		var out []string
		for _, d := range st.GetDecisions() {
			out = append(out, d.GetClusterName())
		}
		return out
	}
	check := func(name string, p *aigriddraftv1.Placement, bound []string, wantNames []string, status aigriddraftv1.ConditionStatus, reason, message string) {
		t.Helper()
		st := computePlacement(p, bound, clusters, nil, now, ident)
		if !slices.Equal(names(st), wantNames) || int(st.GetNumberOfSelectedClusters()) != len(wantNames) {
			t.Errorf("%s: decisions %v, want %v", name, names(st), wantNames)
		}
		wantCond(t, st.GetConditions(), CondPlacementSatisfied, status, reason, message)
		wantCond(t, st.GetConditions(), CondPlacementMisconfigured, cFalse, ReasonSucceedconfigured, MessageSucceedconfigured)
	}
	check("enabled", &aigriddraftv1.Placement{LabelSelector: enabledSel}, global, []string{"site-a", "site-b", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	// matchLabels needs the key present with that exact value: an empty value
	// matches an empty label, never an absent key; a wrong value matches nothing.
	check("empty value", &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: map[string]string{"dedicated": ""}}}, global, []string{"site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("value mismatch", &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: map[string]string{"dedicated": "gpu"}}}, global, nil, cFalse, ReasonNoManagedClusterMatched, MessageNoManagedClusterMatched)
	check("absent key", &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: map[string]string{"ai-grid/region": ""}}}, global, nil, cFalse, ReasonNoManagedClusterMatched, MessageNoManagedClusterMatched)
	check("unbound", &aigriddraftv1.Placement{LabelSelector: enabledSel}, nil, nil, cFalse, ReasonNoManagedClusterSetBindings, MessageNoManagedClusterSetBindings)
	check("no intersection", &aigriddraftv1.Placement{ClusterSets: []string{ClusterSetDefault}, LabelSelector: enabledSel}, global, nil, cFalse, ReasonNoIntersection, "None of ManagedClusterSets [default] is bound to placement namespace")
	check("default set", &aigriddraftv1.Placement{ClusterSets: []string{ClusterSetDefault}, LabelSelector: enabledSel}, []string{ClusterSetGlobal, ClusterSetDefault}, []string{"site-a", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("tolerated", &aigriddraftv1.Placement{LabelSelector: enabledSel, Tolerations: []*aigriddraftv1.Toleration{{Key: TaintUnreachable, Operator: TolerationExists, TolerationSeconds: proto.Int64(600)}}}, global, []string{"site-a", "site-b", "site-d", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("toleration expired", &aigriddraftv1.Placement{LabelSelector: enabledSel, Tolerations: []*aigriddraftv1.Toleration{{Key: TaintUnreachable, Operator: TolerationExists, TolerationSeconds: proto.Int64(50)}}}, global, []string{"site-a", "site-b", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("toleration value mismatch", &aigriddraftv1.Placement{LabelSelector: enabledSel, Tolerations: []*aigriddraftv1.Toleration{{Key: TaintUnreachable, Value: "x"}}}, global, []string{"site-a", "site-b", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("toleration forever", &aigriddraftv1.Placement{LabelSelector: enabledSel, Tolerations: []*aigriddraftv1.Toleration{{Key: TaintUnreachable, Effect: TaintNoSelect}}}, global, []string{"site-a", "site-b", "site-d", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("capped", &aigriddraftv1.Placement{LabelSelector: enabledSel, NumberOfClusters: proto.Int32(1)}, global, []string{"site-a"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("unscheduled", &aigriddraftv1.Placement{LabelSelector: enabledSel, NumberOfClusters: proto.Int32(4)}, global, []string{"site-a", "site-b", "site-f"}, cFalse, ReasonNotAllDecisionsScheduled, "1 cluster decisions unscheduled")
	check("expression NotIn", &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: enabledSel.MatchLabels, MatchExpressions: []*aigriddraftv1.LabelSelectorRequirement{{Key: "ai-grid/region", Operator: OperatorNotIn, Values: []string{"us-east"}}}}}, global, []string{"site-a", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("expression Exists", &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchExpressions: []*aigriddraftv1.LabelSelectorRequirement{{Key: "ai-grid/region", Operator: OperatorExists}}}}, global, []string{"site-a", "site-b"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("expression DoesNotExist", &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: enabledSel.MatchLabels, MatchExpressions: []*aigriddraftv1.LabelSelectorRequirement{{Key: "ai-grid/region", Operator: OperatorDoesNotExist}}}}, global, []string{"site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("claim", &aigriddraftv1.Placement{ClaimExpressions: []*aigriddraftv1.ClaimExpression{{Key: ClaimPlatform, Operator: OperatorIn, Values: []string{"BareMetal"}}}}, global, []string{"site-a"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	check("nothing", &aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: map[string]string{"ai-grid/region": "asia-east"}}}, global, nil, cFalse, ReasonNoManagedClusterMatched, MessageNoManagedClusterMatched)
	empty := map[string]*aigriddraftv1.ManagedCluster{"site-b": b}
	st := computePlacement(&aigriddraftv1.Placement{ClusterSets: []string{ClusterSetDefault}}, []string{ClusterSetDefault}, empty, nil, now, ident)
	wantCond(t, st.GetConditions(), CondPlacementSatisfied, cFalse, ReasonAllManagedClusterSetsEmpty, "All ManagedClusterSets [default] have no member ManagedCluster")
	// A set whose only member is terminating is empty: terminating clusters
	// are dropped while the available set is built.
	st = computePlacement(&aigriddraftv1.Placement{ClusterSets: []string{ClusterSetDefault}}, []string{ClusterSetDefault}, map[string]*aigriddraftv1.ManagedCluster{"site-e": e}, nil, now, ident)
	wantCond(t, st.GetConditions(), CondPlacementSatisfied, cFalse, ReasonAllManagedClusterSetsEmpty, "All ManagedClusterSets [default] have no member ManagedCluster")

	// An empty toleration key with Exists matches every taint.
	check("wildcard toleration", &aigriddraftv1.Placement{LabelSelector: enabledSel, Tolerations: []*aigriddraftv1.Toleration{{Operator: TolerationExists}}}, global, []string{"site-a", "site-b", "site-d", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	// tolerationSeconds against a taint without timeAdded: already expired.
	d.Taints[0].TimeAdded = nil
	check("no timeAdded", &aigriddraftv1.Placement{LabelSelector: enabledSel, Tolerations: []*aigriddraftv1.Toleration{{Key: TaintUnreachable, Operator: TolerationExists, TolerationSeconds: proto.Int64(600)}}}, global, []string{"site-a", "site-b", "site-f"}, cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
	// Misconfigured tolerations: no decision, PlacementMisconfigured True.
	for _, tc := range []struct {
		tol *aigriddraftv1.Toleration
		msg string
	}{
		{&aigriddraftv1.Toleration{Operator: TolerationEqual}, "TaintToleration:If the key is empty, operator must be Exists."},
		{&aigriddraftv1.Toleration{}, "TaintToleration:If the key is empty, operator must be Exists."},
		{&aigriddraftv1.Toleration{Key: TaintUnreachable, Operator: TolerationExists, Value: "x"}, "TaintToleration:If the operator is Exists, the value should be empty."},
	} {
		st := computePlacement(&aigriddraftv1.Placement{LabelSelector: enabledSel, Tolerations: []*aigriddraftv1.Toleration{tc.tol}}, global, clusters, nil, now, ident)
		if len(st.GetDecisions()) != 0 {
			t.Errorf("%v: decisions %v", tc.tol, st.GetDecisions())
		}
		wantCond(t, st.GetConditions(), CondPlacementMisconfigured, cTrue, ReasonMisconfigured, tc.msg)
		wantCond(t, st.GetConditions(), CondPlacementSatisfied, cFalse, ReasonNoManagedClusterMatched, MessageNoManagedClusterMatched)
	}
	// The check only runs when the predicate left a cluster to filter.
	st = computePlacement(&aigriddraftv1.Placement{LabelSelector: &aigriddraftv1.LabelSelector{MatchLabels: map[string]string{"ai-grid/region": "asia-east"}}, Tolerations: []*aigriddraftv1.Toleration{{}}}, global, clusters, nil, now, ident)
	wantCond(t, st.GetConditions(), CondPlacementMisconfigured, cFalse, ReasonSucceedconfigured, MessageSucceedconfigured)

	// Taint effects: PreferNoSelect never filters; NoSelectIfNew filters only a
	// cluster that is not already selected.
	g := enrolled("site-g")
	g.Taints = []*aigriddraftv1.Taint{{Key: "dedicated", Effect: TaintPreferNoSelect}}
	h := enrolled("site-h")
	h.Taints = []*aigriddraftv1.Taint{{Key: "draining", Effect: TaintNoSelectIfNew}}
	effects := map[string]*aigriddraftv1.ManagedCluster{"site-g": g, "site-h": h}
	sel := &aigriddraftv1.Placement{LabelSelector: enabledSel}
	if got := names(computePlacement(sel, global, effects, nil, now, ident)); !slices.Equal(got, []string{"site-g"}) {
		t.Errorf("a new cluster with NoSelectIfNew is not selected: %v", got)
	}
	if got := names(computePlacement(sel, global, effects, map[string]bool{"site-h": true}, now, ident)); !slices.Equal(got, []string{"site-g", "site-h"}) {
		t.Errorf("an already selected cluster keeps its NoSelectIfNew place: %v", got)
	}
	// numberOfClusters keeps the clusters already selected (Steady), then
	// takes the rest by name.
	capped := &aigriddraftv1.Placement{LabelSelector: enabledSel, NumberOfClusters: proto.Int32(2)}
	if got := names(computePlacement(capped, global, clusters, map[string]bool{"site-f": true}, now, ident)); !slices.Equal(got, []string{"site-a", "site-f"}) {
		t.Errorf("Steady keeps site-f: %v", got)
	}
	if got := names(computePlacement(&aigriddraftv1.Placement{LabelSelector: enabledSel, NumberOfClusters: proto.Int32(1)}, global, clusters, map[string]bool{"site-f": true}, now, ident)); !slices.Equal(got, []string{"site-f"}) {
		t.Errorf("a lower-named cluster does not displace a selected one: %v", got)
	}
}

func TestProgressiveRollout(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), readyCluster("site-c")}})
	rollout := &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1", MinSuccessTime: "10s", ProgressDeadline: "5m"}
	m := createModel(t, s, placedModel("pm", rollout))
	rs := m.GetStatus().GetRollout()
	works := rs.GetWorks()
	if len(works) != 1 || works[0].GetClusterName() != "site-a" || works[0].GetWorkName() != "model-pmw0001" || works[0].GetGeneration() != 1 || works[0].GetObservedGeneration() != 1 {
		t.Fatalf("t0 works = %v", works)
	}
	wantCond(t, works[0].GetConditions(), CondWorkApplied, cTrue, ReasonAppliedManifestWorkComplete, MessageAppliedManifestWorkComplete)
	wantNoCond(t, works[0].GetConditions(), CondWorkAvailable) // absent until the status controller's first sync
	wantNoCond(t, works[0].GetConditions(), CondWorkProgressing)
	if mf := works[0].GetManifests(); len(mf) != 2 || mf[0].GetResource().GetKind() != kindNamespace || mf[0].GetResource().GetName() != testNS || mf[1].GetResource().GetOrdinal() != 1 || mf[1].GetResource().GetResource() != llmisvcResource || mf[1].GetResource().GetVersion() != llmisvcVersion || len(mf[1].GetConditions()) != 1 {
		t.Errorf("manifests = %v", mf)
	}
	if sum := rs.GetSummary(); sum.GetTotal() != 1 || sum.GetApplied() != 1 || sum.GetAvailable() != 0 || sum.GetUpdated() != 0 {
		t.Errorf("summary = %v", sum)
	}
	wantCond(t, rs.GetConditions(), CondPlacementVerified, cTrue, ReasonAsExpected, "")
	wantCond(t, rs.GetConditions(), CondPlacementRolledOut, cFalse, ReasonRolloutProgressing, "")
	wantCond(t, rs.GetConditions(), CondManifestworkApplied, cFalse, ReasonNotAsExpected, "")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonNotAsExpected)
	if m.GetStatus().GetPlacement().GetNumberOfSelectedClusters() != 2 || len(m.GetStatus().GetConditions()) != 0 || m.GetStatus().GetUrl() != "" {
		t.Errorf("a placed model has no hub deployment: %v", m.GetStatus())
	}

	advance(3 * time.Second)
	w := getModel(t, s, "pm").GetStatus().GetRollout().GetWorks()[0]
	wantCond(t, w.GetConditions(), CondWorkAvailable, cTrue, ReasonResourcesAvailable, MessageResourcesAvailable)
	wantCond(t, w.GetConditions(), CondWorkProgressing, cTrue, ReasonConditionRulesAggregated, MessageRuleProgressing)
	wantCond(t, w.GetConditions(), CondWorkDegraded, cFalse, ReasonConditionRulesAggregated, MessageRuleDegraded)
	ns, mdl := w.GetManifests()[0], w.GetManifests()[1]
	wantCond(t, ns.GetConditions(), CondWorkApplied, cTrue, ReasonAppliedManifestComplete, MessageAppliedManifestComplete)
	wantCond(t, ns.GetConditions(), CondWorkAvailable, cTrue, ReasonResourceAvailable, MessageResourceAvailable)
	wantCond(t, ns.GetConditions(), CondManifestStatusFeedbackSynced, cTrue, ReasonNoStatusFeedbackSynced, "")
	wantCond(t, mdl.GetConditions(), CondManifestStatusFeedbackSynced, cTrue, ReasonStatusFeedbackSynced, "")
	wantCond(t, mdl.GetConditions(), CondWorkProgressing, cTrue, ReasonConditionRuleEvaluated, MessageRuleProgressing)
	wantCond(t, mdl.GetConditions(), CondWorkDegraded, cFalse, ReasonConditionRuleEvaluated, MessageRuleDegraded)
	// Only Applied and the rule conditions carry observedGeneration; the
	// manifest Available and StatusFeedbackSynced do not.
	for typ, want := range map[string]int64{CondWorkApplied: 1, CondWorkProgressing: 1, CondWorkDegraded: 1, CondWorkAvailable: 0, CondManifestStatusFeedbackSynced: 0} {
		if got := condition(mdl.GetConditions(), typ).GetObservedGeneration(); got != want {
			t.Errorf("manifest %s observedGeneration = %d, want %d", typ, got, want)
		}
	}
	if got := condition(w.GetConditions(), CondWorkAvailable).GetObservedGeneration(); got != 1 {
		t.Errorf("work-level Available observedGeneration = %d", got)
	}
	if len(mdl.GetFeedback()) != 0 {
		t.Errorf("no Ready condition on the spoke yet, so no feedback: %v", mdl.GetFeedback())
	}
	wantCond(t, getModel(t, s, "pm").GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cFalse, ReasonProcessing, "")
	wantPhase(t, getModel(t, s, "pm"), aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonProcessing)

	advance(llmisvcT1 + 2*time.Second)
	mdl = getModel(t, s, "pm").GetStatus().GetRollout().GetWorks()[0].GetManifests()[1]
	// The spoke wrote its url with the route at its T1, before Ready.
	if fb := mdl.GetFeedback(); len(fb) != 2 || fb[0].GetName() != FeedbackReady || fb[0].GetValue() != "False" || fb[0].GetType() != FeedbackTypeString || fb[1].GetValue() != "https://openshift-ai-inference.apps.site-a.example.com/ai-grid-models/pm" {
		t.Errorf("feedback while starting = %v", fb)
	}
	// StatusFeedbackSynced is re-added whenever the values change.
	if lt := condition(mdl.GetConditions(), CondManifestStatusFeedbackSynced).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT1)) {
		t.Errorf("StatusFeedbackSynced at %s, want the feedback change at T1", lt)
	}

	advance(llmisvcT3 + time.Second)
	m = getModel(t, s, "pm")
	works = m.GetStatus().GetRollout().GetWorks()
	if len(works) != 1 {
		t.Fatalf("site-b waits for the soak: %v", works)
	}
	prog := wantCond(t, works[0].GetConditions(), CondWorkProgressing, cFalse, ReasonConditionRulesAggregated, MessageRuleProgressing)
	if !prog.GetLastTransitionTime().AsTime().Equal(testStart.Add(llmisvcT3)) {
		t.Errorf("Progressing fell at T3, got %s", prog.GetLastTransitionTime().AsTime())
	}
	fb := works[0].GetManifests()[1].GetFeedback()
	if len(fb) != 2 || fb[0].GetValue() != "True" || fb[1].GetName() != FeedbackURL || fb[1].GetValue() != "https://openshift-ai-inference.apps.site-a.example.com/ai-grid-models/pm" {
		t.Errorf("feedback = %v", fb)
	}
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cTrue, ReasonAsExpected, "")
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondPlacementRolledOut, cFalse, ReasonRolloutProgressing, "")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonRolloutProgressing)

	advance(llmisvcT3 + 10*time.Second + time.Second)
	works = getModel(t, s, "pm").GetStatus().GetRollout().GetWorks()
	if len(works) != 2 || works[1].GetClusterName() != "site-b" || works[1].GetWorkName() != "model-pmw0002" {
		t.Fatalf("site-b applied after the soak: %v", works)
	}
	if lt := condition(works[1].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT3 + 10*time.Second)) {
		t.Errorf("site-b applied exactly when the slot freed, got %s", lt)
	}

	advance(2*llmisvcT3 + 11*time.Second)
	m = getModel(t, s, "pm")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondPlacementRolledOut, cTrue, ReasonRolloutComplete, "")
	if sum := m.GetStatus().GetRollout().GetSummary(); sum.GetTotal() != 2 || sum.GetApplied() != 2 || sum.GetAvailable() != 2 || sum.GetProgressing() != 0 || sum.GetDegraded() != 0 || sum.GetUpdated() != 0 {
		t.Errorf("summary = %v", sum)
	}
}

func TestAllRolloutAndClusterChanges(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c")}})
	m := createModel(t, s, placedModel("all", nil))
	if works := m.GetStatus().GetRollout().GetWorks(); len(works) != 3 || works[2].GetClusterName() != "site-c" {
		t.Fatalf("All applies every cluster at once: %v", works)
	}
	ctx := authed(context.Background())
	// site-b leaves the decision: its work is deleted.
	b, _ := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: "site-b"})
	labels := copyMap(b.GetObject().GetLabels())
	delete(labels, "ai-grid/enabled")
	if _, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "site-b", Labels: labels}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathLabels}}}); err != nil {
		t.Fatal(err)
	}
	advance(time.Minute)
	m = getModel(t, s, "all")
	if works := m.GetStatus().GetRollout().GetWorks(); len(works) != 2 || works[1].GetClusterName() != "site-c" || m.GetStatus().GetPlacement().GetNumberOfSelectedClusters() != 2 {
		t.Errorf("site-b's work is removed: %v", works)
	}
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	// It comes back: a new work, applied now.
	labels["ai-grid/enabled"] = "true"
	if _, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "site-b", Labels: labels}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathLabels}}}); err != nil {
		t.Fatal(err)
	}
	advance(time.Minute + 3*time.Second)
	m = getModel(t, s, "all")
	works := m.GetStatus().GetRollout().GetWorks()
	if len(works) != 3 || works[1].GetWorkName() != "model-allw0004" || !condition(works[1].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(time.Minute)) {
		t.Errorf("site-b gets a fresh work, applied when its label came back: %v", works)
	}
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonProcessing)
}

func TestFailApplyAndTimeouts(t *testing.T) {
	broken := enrolled("site-b")
	broken.Labels[LabelFailApply] = "true"
	s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), broken}})
	createModel(t, s, placedModel("all", nil))
	advance(3 * time.Second)
	m := getModel(t, s, "all")
	w := m.GetStatus().GetRollout().GetWorks()[1]
	wantCond(t, w.GetConditions(), CondWorkApplied, cFalse, ReasonAppliedManifestWorkFailed, MessageAppliedManifestWorkFailed)
	wantCond(t, w.GetConditions(), CondWorkAvailable, cUnknown, ReasonResourcesStatusUnknown, "1 of 2 resources have unknown status")
	// The status controller skips the feedback and the condition rules of a
	// manifest it cannot find: no Progressing, no Degraded.
	wantNoCond(t, w.GetConditions(), CondWorkDegraded)
	wantNoCond(t, w.GetConditions(), CondWorkProgressing)
	mdl := w.GetManifests()[1]
	wantCond(t, mdl.GetConditions(), CondWorkApplied, cFalse, ReasonAppliedManifestFailed, `Failed to apply manifest: the server doesn't have a resource type "LLMInferenceService"`)
	wantCond(t, mdl.GetConditions(), CondWorkAvailable, cUnknown, ReasonIncompleteResourceMeta, MessageIncompleteResourceMeta)
	if r := mdl.GetResource(); r.GetResource() != "" || r.GetKind() != llmisvcKind || r.GetName() != "all" || r.GetVersion() != llmisvcVersion {
		t.Errorf("an unmapped kind keeps its meta but no resource: %v", r)
	}
	if len(mdl.GetFeedback()) != 0 || len(mdl.GetConditions()) != 2 || condition(mdl.GetConditions(), CondWorkApplied).GetObservedGeneration() != 1 {
		t.Errorf("nothing to scrape, no rules: %v", mdl)
	}
	if sum := m.GetStatus().GetRollout().GetSummary(); sum.GetDegraded() != 0 || sum.GetApplied() != 1 || sum.GetAvailable() != 1 || sum.GetProgressing() != 1 {
		t.Errorf("summary = %v", sum)
	}
	// While site-a still progresses the controller's rule reads Processing
	// (progressing > 0, degraded == 0); once it is done, available != total
	// gives NotAsExpected and the unapplied work makes the phase FAILED.
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cFalse, ReasonProcessing, "")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonProcessing)
	advance(2 * time.Minute)
	m = getModel(t, s, "all")
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cFalse, ReasonNotAsExpected, "")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ReasonNotAsExpected)

	// Progressive with the broken cluster first: ToApply (Applied not True)
	// has no timeout in sdk-go, so it holds its slot indefinitely, never counts
	// towards maxFailures, and the rollout stalls on it, whatever the deadline
	// or maxFailures say.
	broken2 := enrolled("site-a2")
	broken2.Labels[LabelFailApply] = "true"
	s2, advance2 := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{broken2, enrolled("site-b2")}})
	createModel(t, s2, placedModel("prog", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1", MaxFailures: "1", ProgressDeadline: "1m"}))
	for _, d := range []time.Duration{30 * time.Second, 5 * time.Minute, time.Hour} {
		advance2(d)
		m = getModel(t, s2, "prog")
		if works := m.GetStatus().GetRollout().GetWorks(); len(works) != 1 || works[0].GetClusterName() != "site-a2" {
			t.Errorf("at %s: the unapplied cluster keeps its slot and site-b2 is never applied: %v", d, works)
		}
	}
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondPlacementRolledOut, cFalse, ReasonRolloutProgressing, "")
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cFalse, ReasonNotAsExpected, "")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ReasonNotAsExpected)

	// A Progressing work, by contrast, does time out: a stuck spoke keeps
	// MinimumReplicasUnavailable, which the Degraded rule does not match, so
	// its work stays Progressing (not Failed) and the model reads
	// PENDING/Processing; progressDeadline counts from the work's Progressing
	// condition (first written at the agent's first sync, 2s after apply),
	// after which the cluster times out, frees its slot and counts as a failure.
	s3, advance3 := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a3"), enrolled("site-b3")}})
	stuck := placedModel("stuck", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1", MaxFailures: "1", ProgressDeadline: "1m"})
	stuck.Spec.Main.Image = "quay.io/example/vllm-oom"
	createModel(t, s3, stuck)
	advance3(61 * time.Second)
	m = getModel(t, s3, "stuck")
	if works := m.GetStatus().GetRollout().GetWorks(); len(works) != 1 {
		t.Errorf("before Progressing's lastTransitionTime + 1m site-a3 holds the slot: %v", works)
	} else {
		wantCond(t, works[0].GetConditions(), CondWorkProgressing, cTrue, ReasonConditionRulesAggregated, MessageRuleProgressing)
		wantCond(t, works[0].GetConditions(), CondWorkDegraded, cFalse, ReasonConditionRulesAggregated, MessageRuleDegraded)
	}
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonProcessing)
	advance3(63 * time.Second)
	if works := getModel(t, s3, "stuck").GetStatus().GetRollout().GetWorks(); len(works) != 2 || !condition(works[1].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(62*time.Second)) {
		t.Errorf("site-b3 is applied when site-a3 times out at 2s + 1m (maxFailures 1): %v", works)
	}
	// progressDeadline 0s times a progressing cluster out as soon as its
	// Progressing condition exists.
	s5, advance5 := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a5"), enrolled("site-b5")}})
	createModel(t, s5, placedModel("zero", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1", MaxFailures: "1", ProgressDeadline: "0s"}))
	advance5(3 * time.Second)
	if works := getModel(t, s5, "zero").GetStatus().GetRollout().GetWorks(); len(works) != 2 || !condition(works[1].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(2*time.Second)) {
		t.Errorf("0s deadline: site-b5 applied at 2s: %v", works)
	}
	s4, advance4 := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a4"), enrolled("site-b4")}})
	stuck.Name = "stuck-strict"
	stuck.Spec.Rollout.MaxFailures = "0"
	createModel(t, s4, stuck)
	advance4(5 * time.Minute)
	if works := getModel(t, s4, "stuck-strict").GetStatus().GetRollout().GetWorks(); len(works) != 1 {
		t.Errorf("with maxFailures 0 the timed-out cluster breaches and nothing more is applied: %v", works)
	}
}

func TestUnreachableClusterAgentDoesNotReport(t *testing.T) {
	lost := enrolled("site-lost")
	lost.Status.Conditions = []*aigriddraftv1.Condition{proto.Clone(accepted).(*aigriddraftv1.Condition), proto.Clone(joined).(*aigriddraftv1.Condition), proto.Clone(imported).(*aigriddraftv1.Condition),
		{Type: CondAvailable, Status: cUnknown, Reason: proto.String(ReasonLeaseStopped), Message: proto.String(MessageLeaseStopped), LastTransitionTime: timestamppb.New(testStart.Add(-time.Minute))}}
	lost.Taints = []*aigriddraftv1.Taint{{Key: TaintUnreachable, Effect: TaintNoSelect, TimeAdded: timestamppb.New(testStart.Add(-time.Minute))}}
	fx := &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{lost, enrolled("site-ok")}, Scenarios: []ScenarioEvent{{Cluster: "site-lost", At: Duration{2 * time.Minute}, Event: EventLeaseRecovered}}}
	s, advance := modelServer(t, fx)
	m := placedModel("tol", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1", ProgressDeadline: "10m"})
	m.Spec.Placement.Tolerations = []*aigriddraftv1.Toleration{{Key: TaintUnreachable, Operator: TolerationExists}}
	createModel(t, s, m)
	advance(time.Minute)
	got := getModel(t, s, "tol")
	works := got.GetStatus().GetRollout().GetWorks()
	// Progressive, one at a time: the silent cluster is first by name and
	// holds the only slot (ToApply never times out), so site-ok waits.
	if len(works) != 1 || works[0].GetClusterName() != "site-lost" {
		t.Fatalf("works = %v", works)
	}
	if len(works[0].GetConditions()) != 0 || works[0].GetObservedGeneration() != 0 || len(works[0].GetManifests()) != 0 || works[0].GetGeneration() != 1 {
		t.Errorf("only the spoke agent writes a work's status; an agent that does not report leaves it empty: %v", works[0])
	}
	wantCond(t, got.GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cFalse, ReasonNotAsExpected, "")
	wantPhase(t, got, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonNotAsExpected)
	if sum := got.GetStatus().GetRollout().GetSummary(); sum.GetTotal() != 1 || sum.GetApplied() != 0 || sum.GetAvailable() != 0 {
		t.Errorf("summary = %v", sum)
	}
	// The lease recovers at 2m: the agent applies from then, site-lost
	// succeeds at 2m45s and site-ok takes the slot.
	advance(2*time.Minute + llmisvcT3 + time.Second)
	got = getModel(t, s, "tol")
	works = got.GetStatus().GetRollout().GetWorks()
	if lt := condition(works[0].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(2 * time.Minute)) {
		t.Errorf("applied when the agent came back: %v", works[0].GetConditions())
	}
	if len(works) != 2 || !condition(works[1].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(2*time.Minute+llmisvcT3)) {
		t.Errorf("site-ok applied once site-lost succeeded: %v", works)
	}
	advance(3*time.Minute + llmisvcT3 + time.Second)
	wantPhase(t, getModel(t, s, "tol"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
}

func TestUpdateModel(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a")}})
	ctx := authed(context.Background())
	createModel(t, s, hubModel("hub"))
	createModel(t, s, placedModel("placed", nil))
	advance(time.Minute)
	hub := getModel(t, s, "hub")
	update := func(obj *aigriddraftv1.Model, lock bool, paths ...string) (*aigriddraftv1.Model, error) {
		resp, err := s.UpdateModel(ctx, &aigriddraftv1.UpdateModelRequest{Object: obj, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, Lock: lock})
		return resp.GetObject(), err
	}
	_, err := update(&aigriddraftv1.Model{Name: "hub", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}}, false)
	mustCode(t, err, codes.InvalidArgument, "update_mask must name at least one of "+strings.Join(modelPaths, ", "))
	_, err = update(&aigriddraftv1.Model{Name: "hub", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}}, false, "spec.model.uri")
	mustCode(t, err, codes.InvalidArgument, `update_mask path "spec.model.uri" is not supported; supported paths: `+strings.Join(modelPaths, ", "))
	_, err = update(&aigriddraftv1.Model{Name: "hub", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS, ResourceVersion: "stale"}, Spec: &aigriddraftv1.ModelSpec{Replicas: proto.Int32(2)}}, true, PathSpecReplicas)
	mustCode(t, err, codes.Aborted, fmt.Sprintf("object with identifier 'ai-grid-models/hub' has resource version %s, the request carries \"stale\"; fetch it again and retry", hub.GetMetadata().GetResourceVersion()))
	_, err = update(&aigriddraftv1.Model{Name: "nope", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{}}, false, PathSpecReplicas)
	mustCode(t, err, codes.NotFound, "object with identifier 'ai-grid-models/nope' not found")

	// Labels only: a new version, the same generation, the deployment untouched.
	labelled, err := update(&aigriddraftv1.Model{Name: "hub", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS, ResourceVersion: hub.GetMetadata().GetResourceVersion(), Labels: map[string]string{"team": "a"}}}, true, PathMetadataLabels)
	if err != nil || labelled.GetMetadata().GetLabels()["team"] != "a" || labelled.GetMetadata().GetGeneration() != 1 || labelled.GetMetadata().GetResourceVersion() == hub.GetMetadata().GetResourceVersion() {
		t.Fatalf("labels update = %v, %v", labelled.GetMetadata(), err)
	}
	wantPhase(t, labelled, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")

	// Replicas: generation 2, the Deployment rolls out again.
	updated, err := update(&aigriddraftv1.Model{Name: "hub", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS, ResourceVersion: labelled.GetMetadata().GetResourceVersion()}, Spec: &aigriddraftv1.ModelSpec{Replicas: proto.Int32(3)}}, true, PathSpecReplicas)
	if err != nil || updated.GetSpec().GetReplicas() != 3 || updated.GetMetadata().GetGeneration() != 2 {
		t.Fatalf("replicas update = %v, %v", updated, err)
	}
	if updated.GetStatus().GetObservedGeneration() != 1 {
		t.Errorf("the controller has not observed generation 2 yet: %d", updated.GetStatus().GetObservedGeneration())
	}
	advance(time.Minute + llmisvcT2 + time.Second)
	m := getModel(t, s, "hub")
	wantCond(t, m.GetStatus().GetConditions(), CondMainWorkloadReady, cFalse, ReasonProgressing, "")
	if m.GetStatus().GetObservedGeneration() != 2 || m.GetStatus().GetUrl() != "https://openshift-ai-inference.apps.hub.example.com/ai-grid-models/hub" {
		t.Errorf("rolling out generation 2, url kept from the unchanged route: %v", m.GetStatus())
	}
	if lt := condition(m.GetStatus().GetConditions(), CondPresetsCombined).GetLastTransitionTime().AsTime(); !lt.Equal(testStart.Add(llmisvcT1)) {
		t.Errorf("PresetsCombined never changed status: %s", lt)
	}
	advance(time.Minute + llmisvcT3 + time.Second)
	m = getModel(t, s, "hub")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	if m.GetStatus().GetWorkloads().GetPrimary().GetReadyReplicas() != 3 {
		t.Errorf("readyReplicas = %d", m.GetStatus().GetWorkloads().GetPrimary().GetReadyReplicas())
	}

	// Image on the placed model: works re-applied at generation 2.
	placed := getModel(t, s, "placed")
	updated, err = update(&aigriddraftv1.Model{Name: "placed", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS, ResourceVersion: placed.GetMetadata().GetResourceVersion()}, Spec: &aigriddraftv1.ModelSpec{Main: &aigriddraftv1.Container{Image: "quay.io/example/vllm:v2"}}}, true, PathSpecMainImage)
	if err != nil {
		t.Fatal(err)
	}
	w := updated.GetStatus().GetRollout().GetWorks()[0]
	if w.GetGeneration() != 2 || w.GetObservedGeneration() != 2 || !condition(w.GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(time.Minute+llmisvcT3+time.Second)) {
		t.Errorf("work re-applied at the update: %v", w)
	}
	wantNoCond(t, w.GetConditions(), CondWorkAvailable) // not synced yet at the new generation
	if sum := updated.GetStatus().GetRollout().GetSummary(); sum.GetTotal() != 1 || sum.GetApplied() != 1 {
		t.Errorf("summary = %v", sum)
	}
	wantPhase(t, updated, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonNotAsExpected)

	// Env, rollout, annotations.
	updated, err = update(&aigriddraftv1.Model{Name: "placed", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS, Annotations: map[string]string{AnnotationDisplayName: "Renamed"}},
		Spec: &aigriddraftv1.ModelSpec{Main: &aigriddraftv1.Container{Env: []*aigriddraftv1.EnvVar{{Name: "VLLM_ADDITIONAL_ARGS", Value: "--max-model-len 8192"}}}, Rollout: &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1"}}},
		false, PathSpecMainEnv, PathSpecRollout, PathMetadataAnnotation)
	if err != nil || len(updated.GetSpec().GetMain().GetEnv()) != 1 || updated.GetSpec().GetMain().GetImage() != "quay.io/example/vllm:v2" || updated.GetSpec().GetRollout().GetStrategy() != aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE || updated.GetMetadata().GetAnnotations()[AnnotationDisplayName] != "Renamed" || updated.GetMetadata().GetGeneration() != 3 {
		t.Errorf("env/rollout/annotations update = %v, %v", updated, err)
	}
	// Removing the placement makes it a hub deployment that starts now.
	updated, err = update(&aigriddraftv1.Model{Name: "placed", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{}}, false, PathSpecPlacement)
	if err != nil || updated.GetSpec().GetPlacement() != nil || updated.GetSpec().GetRollout() != nil || updated.GetStatus().GetRollout() != nil || len(updated.GetStatus().GetConditions()) != 0 {
		t.Fatalf("placement removed = %v, %v", updated, err)
	}
	advance(3*time.Minute + llmisvcT3 + time.Second)
	wantPhase(t, getModel(t, s, "placed"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	// Adding one back is gated and starts the works.
	s.SetManifestWorkReplicaSetEnabled(false)
	_, err = update(&aigriddraftv1.Model{Name: "placed", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Placement: &aigriddraftv1.Placement{}}}, false, PathSpecPlacement)
	mustCode(t, err, codes.FailedPrecondition, MessageFeatureGateDisabled)
	s.SetManifestWorkReplicaSetEnabled(true)
	updated, err = update(&aigriddraftv1.Model{Name: "placed", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Placement: &aigriddraftv1.Placement{}}}, false, PathSpecPlacement)
	if err != nil || len(updated.GetStatus().GetRollout().GetWorks()) != 1 || updated.GetStatus().GetPlacement().GetNumberOfSelectedClusters() != 1 {
		t.Errorf("placement added = %v, %v", updated.GetStatus(), err)
	}
	// The new ManifestWork is a new object: its own generation starts at 1,
	// whatever the model's generation is.
	if ws := updated.GetStatus().GetRollout().GetWorks(); len(ws) == 1 && (updated.GetMetadata().GetGeneration() <= 1 || ws[0].GetGeneration() != 1) {
		t.Errorf("work created at model generation %d has generation %d, want 1", updated.GetMetadata().GetGeneration(), ws[0].GetGeneration())
	}
	// Replicas and scaling stay exclusive through an update.
	_, err = update(&aigriddraftv1.Model{Name: "hub", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Replicas: proto.Int32(2), Scaling: &aigriddraftv1.Scaling{Keda: true, MaxReplicas: 4}}}, false, PathSpecReplicas)
	if err != nil {
		t.Errorf("the mask only carries replicas, scaling in the object is ignored: %v", err)
	}
}

func TestDeleteModel(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a")}})
	ctx := authed(context.Background())
	createModel(t, s, hubModel("hub"))
	createModel(t, s, placedModel("placed", nil))
	createModel(t, s, hubModel("referenced"))
	s.SetReferencedBy(func(ns, name string) []string {
		if name == "referenced" {
			return []string{"catalog entry " + ns + "/" + name, "subscription acme/gold"}
		}
		return nil
	})
	_, err := s.DeleteModel(ctx, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "referenced"})
	mustCode(t, err, codes.FailedPrecondition, "model ai-grid-models/referenced is referenced by catalog entry ai-grid-models/referenced, subscription acme/gold; remove those references first")
	_, err = s.DeleteModel(ctx, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "nope"})
	mustCode(t, err, codes.NotFound, "object with identifier 'ai-grid-models/nope' not found")
	_, err = s.DeleteModel(ctx, &aigriddraftv1.DeleteModelRequest{Name: "hub"})
	mustCode(t, err, codes.InvalidArgument, "name and namespace are required")

	// A delete precondition on the stored resource version.
	rv := getModel(t, s, "hub").GetMetadata().GetResourceVersion()
	_, err = s.DeleteModel(ctx, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "hub", ResourceVersion: "1"})
	mustCode(t, err, codes.Aborted, "object with identifier 'ai-grid-models/hub' has resource version "+rv+", the request carries \"1\"; fetch it again and retry")
	getModel(t, s, "hub")
	if _, err := s.DeleteModel(ctx, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "hub", ResourceVersion: rv}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetModel(ctx, &aigriddraftv1.GetModelRequest{Namespace: testNS, Name: "hub"}); status.Code(err) != codes.NotFound {
		t.Errorf("a hub deployment is gone at once (the KServe finalizer window is a declared deviation), got %v", err)
	}
	advance(time.Second)
	if _, err := s.DeleteModel(ctx, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "placed"}); err != nil {
		t.Fatal(err)
	}
	// Cascade Background: the MWRS, and with it the model, is gone at once;
	// its works drain in the cluster namespaces out of sight.
	if _, err := s.GetModel(ctx, &aigriddraftv1.GetModelRequest{Namespace: testNS, Name: "placed"}); status.Code(err) != codes.NotFound {
		t.Errorf("a placed model disappears at once, got %v", err)
	}
	if _, err := s.DeleteModel(ctx, &aigriddraftv1.DeleteModelRequest{Namespace: testNS, Name: "placed"}); status.Code(err) != codes.NotFound {
		t.Errorf("deleting twice = %v", err)
	}
	list, _ := s.ListModels(ctx, &aigriddraftv1.ListModelsRequest{})
	if list.GetTotal() != 1 {
		t.Errorf("total = %d", list.GetTotal())
	}
}

// TestListModelsFilterDecodesLiterals: the filter's values are CEL string
// literals, so escaped quotes and backslashes are decoded and a quoted ` && `
// does not split a term (CEL parses it; nothing splits the text).
func TestListModelsFilterDecodesLiterals(t *testing.T) {
	s, _ := modelServer(t, &Fixtures{})
	for name, v := range map[string]string{"quote": `say "hi"`, "slash": `a\b`, "and": `x && y`, "plain": "x"} {
		m := hubModel(name)
		m.Metadata.Labels = map[string]string{"k": v}
		createModel(t, s, m)
	}
	ctx := authed(context.Background())
	for literal, want := range map[string]string{`"say \"hi\""`: "quote", `"a\\b"`: "slash", `"x && y"`: "and", `"\u0078"`: "plain"} {
		filter := `this.metadata.namespace == "` + testNS + `" && "k" in this.metadata.labels && this.metadata.labels["k"] == ` + literal
		resp, err := s.ListModels(ctx, &aigriddraftv1.ListModelsRequest{Filter: proto.String(filter)})
		if err != nil || len(resp.GetItems()) != 1 || resp.GetItems()[0].GetName() != want {
			t.Errorf("%s = %v, %v; want %s", literal, resp.GetItems(), err, want)
		}
	}
	bad := `"k" in this.metadata.labels && this.metadata.labels["k"] == "\q"`
	_, err := s.ListModels(ctx, &aigriddraftv1.ListModelsRequest{Filter: proto.String(bad)})
	if status.Code(err) != codes.InvalidArgument || !strings.HasPrefix(status.Convert(err).Message(), "filter "+bad+" is not a valid CEL expression: ") {
		t.Errorf("malformed escape = %v", err)
	}
}

func TestListModelsFilterAndPaging(t *testing.T) {
	s, _ := modelServer(t, &Fixtures{})
	for _, spec := range []struct{ ns, name, team string }{{"zeta", "b", "x"}, {"alpha", "z", "x"}, {"alpha", "a", "y"}} {
		m := hubModel(spec.name)
		m.Metadata.Namespace = spec.ns
		m.Metadata.Labels = map[string]string{"team": spec.team}
		createModel(t, s, m)
	}
	ctx := authed(context.Background())
	list := func(req *aigriddraftv1.ListModelsRequest) []string {
		t.Helper()
		resp, err := s.ListModels(ctx, req)
		if err != nil {
			t.Fatalf("list %v: %v", req, err)
		}
		var out []string
		for _, m := range resp.GetItems() {
			out = append(out, m.GetMetadata().GetNamespace()+"/"+m.GetName())
		}
		return out
	}
	if got := list(&aigriddraftv1.ListModelsRequest{}); !slices.Equal(got, []string{"alpha/a", "alpha/z", "zeta/b"}) {
		t.Errorf("order = %v", got)
	}
	if got := list(&aigriddraftv1.ListModelsRequest{Filter: proto.String(`this.metadata.namespace == "alpha"`)}); !slices.Equal(got, []string{"alpha/a", "alpha/z"}) {
		t.Errorf("namespace filter = %v", got)
	}
	if got := list(&aigriddraftv1.ListModelsRequest{Filter: proto.String(`"team" in this.metadata.labels && this.metadata.labels["team"] == "x"`)}); !slices.Equal(got, []string{"alpha/z", "zeta/b"}) {
		t.Errorf("label filter = %v", got)
	}
	if got := list(&aigriddraftv1.ListModelsRequest{Filter: proto.String(`this.metadata.namespace == "alpha" && "team" in this.metadata.labels && this.metadata.labels["team"] == "x"`)}); !slices.Equal(got, []string{"alpha/z"}) {
		t.Errorf("combined filter = %v", got)
	}
	// A label no model carries: the guard makes it false, not an error.
	if got := list(&aigriddraftv1.ListModelsRequest{Filter: proto.String(`"absent" in this.metadata.labels && this.metadata.labels["absent"] == "x"`)}); len(got) != 0 {
		t.Errorf("missing label = %v", got)
	}
	// Any CEL over the Model message as the read derives it, status included.
	for expr, want := range map[string][]string{
		`this.name == "a"`:        {"alpha/a"},
		`this.name in ["a", "b"]`: {"alpha/a", "zeta/b"},
		`this.metadata.namespace.startsWith("al") && this.metadata.labels["team"] == "x"`: {"alpha/z"},
		`"other" in this.metadata.labels && this.metadata.labels["team"] == "x"`:          nil, // the guard names another key: false, no error
		`this.spec.replicas == 1 && !has(this.spec.placement)`:                            {"alpha/a", "alpha/z", "zeta/b"},
		`this.status.phase == ai_grid.draft.v1.ModelPhase.MODEL_PHASE_PENDING`:            {"alpha/a", "alpha/z", "zeta/b"},
		`this.metadata.creation_timestamp <= now`:                                         {"alpha/a", "alpha/z", "zeta/b"},
	} {
		if got := list(&aigriddraftv1.ListModelsRequest{Filter: proto.String(expr)}); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
	// Paging applies to the filtered list; total counts it.
	if resp, err := s.ListModels(ctx, &aigriddraftv1.ListModelsRequest{Filter: proto.String(`this.metadata.labels["team"] == "x"`), Offset: proto.Int32(1), Limit: proto.Int32(5)}); err != nil || resp.GetTotal() != 2 || resp.GetSize() != 1 || resp.GetItems()[0].GetName() != "b" {
		t.Errorf("page of the filtered list = %v, %v", resp, err)
	}
	if got := list(&aigriddraftv1.ListModelsRequest{Offset: proto.Int32(1), Limit: proto.Int32(1)}); !slices.Equal(got, []string{"alpha/z"}) {
		t.Errorf("page = %v", got)
	}
	resp, _ := s.ListModels(ctx, &aigriddraftv1.ListModelsRequest{Limit: proto.Int32(0)})
	if resp.GetSize() != 0 || resp.GetTotal() != 3 {
		t.Errorf("count only = %v", resp)
	}
	for _, req := range []*aigriddraftv1.ListModelsRequest{
		{Filter: proto.String(`this.metadata.labels["absent"] == "x"`)},                                 // unguarded index of a missing key
		{Filter: proto.String(`this.metadata.namespace == "alpha" &&`)},                                 // dangling &&
		{Filter: proto.String(`this.metadata.namespace == "alpha" this.metadata.namespace == "alpha"`)}, // no operator
		{Filter: proto.String(`this.metadata.namespace == "a\q"`)},                                      // malformed escape
		{Filter: proto.String(`this.metadata.namespace`)},                                               // not a bool
		{Filter: proto.String(`this.namespace == "alpha"`)},                                             // no such field on Model
		{Order: proto.String("name desc")},
		{Offset: proto.Int32(-1)},
		{Limit: proto.Int32(-1)},
	} {
		if _, err := s.ListModels(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v, want InvalidArgument", req, err)
		}
	}
	_, err := s.ListModels(ctx, &aigriddraftv1.ListModelsRequest{Filter: proto.String(`this.metadata.labels["absent"] == "x"`)})
	mustCode(t, err, codes.InvalidArgument, `filter this.metadata.labels["absent"] == "x" failed on 'alpha/a': no such key: absent`)
	if _, err := s.GetModel(ctx, &aigriddraftv1.GetModelRequest{Name: "a"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Get without namespace = %v", err)
	}
}

func TestUnboundNamespace(t *testing.T) {
	fx := &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a")}, ModelBindings: map[string][]string{"unbound": {}}}
	s, _ := modelServer(t, fx)
	m := placedModel("lonely", nil)
	m.Metadata.Namespace = "unbound"
	created := createModel(t, s, m)
	wantCond(t, created.GetStatus().GetPlacement().GetConditions(), CondPlacementSatisfied, cFalse, ReasonNoManagedClusterSetBindings, MessageNoManagedClusterSetBindings)
	wantPhase(t, created, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonPlacementDecisionEmpty)
}

func TestLoadModelFixtures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("models.json", `{"models": [{"name": "a", "nope": 1}]}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "model 0") {
		t.Errorf("unknown proto field should fail with position, got %v", err)
	}
	write("models.json", `{"models": [], "bindings": {"ns": ["edge"]}}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), `"edge"`) {
		t.Errorf("unknown cluster set in bindings should fail, got %v", err)
	}
	write("models.json", `{"models": [{"name": "a", "metadata": {"namespace": "ns"}, "spec": {"model": {"uri": "hf://x/a"}}}]}`)
	write("presets.json", `{"presets": ["p"]}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "presets.json") {
		t.Errorf("presets without namespace should fail, got %v", err)
	}
	write("presets.json", `{"namespace": "apps", "presets": ["p"]}`)
	write("settings.json", `{"manifestWorkReplicaSetEnabled": false, "observabilityInstalled": true, "acmVersion": "2.18.0"}`)
	fx, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.Models) != 1 || fx.Presets.Namespace != "apps" || fx.ManifestWorkReplicaSetEnabled == nil || *fx.ManifestWorkReplicaSetEnabled || !fx.ObservabilityInstalled || fx.ACMVersion != "2.18.0" {
		t.Errorf("fixtures = %+v", fx)
	}
	s := newServer(t, fx)
	caps, _ := s.GetHubCapabilities(authed(context.Background()), &aigriddraftv1.GetHubCapabilitiesRequest{})
	if caps.GetCapabilities().GetManifestWorkReplicaSetEnabled() || !caps.GetCapabilities().GetObservabilityInstalled() || caps.GetCapabilities().GetAcmVersion() != "2.18.0" {
		t.Errorf("capabilities = %v", caps)
	}
	// A fixture model fails at start like a rejected create would.
	bad := hubModel("Bad")
	if _, err := New(&Fixtures{Models: []*aigriddraftv1.Model{bad}}, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "RFC 1123") {
		t.Errorf("bad fixture name = %v", err)
	}
	dup := hubModel("dup")
	if _, err := New(&Fixtures{Models: []*aigriddraftv1.Model{dup, hubModel("dup")}}, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate fixture = %v", err)
	}
	future := hubModel("future")
	future.Metadata.CreationTimestamp = timestamppb.New(time.Now().Add(time.Hour))
	if _, err := New(&Fixtures{Models: []*aigriddraftv1.Model{future}}, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "not in the past") {
		t.Errorf("future creationTimestamp = %v", err)
	}
}

func TestRolloutHelpers(t *testing.T) {
	if aggregatedMessage("one", 1) != "one" || aggregatedMessage("one", 3) != "Aggregated from 3 manifest conditions" {
		t.Error("aggregated message")
	}
	// sdk-go calculateRolloutSize: zero, or more than total, falls back.
	for _, tc := range []struct {
		in         string
		total, def int
		want       int
	}{{"", 4, 4, 4}, {"2", 4, 4, 2}, {"50%", 3, 0, 2}, {"0", 4, 4, 4}, {"0%", 4, 4, 4}, {"5", 4, 0, 0}, {"5", 4, 4, 4}, {"100%", 4, 0, 4}, {"1%", 4, 0, 1}} {
		if got := rolloutSize(tc.in, tc.total, tc.def); got != tc.want {
			t.Errorf("rolloutSize(%q, %d, %d) = %d, want %d", tc.in, tc.total, tc.def, got, tc.want)
		}
	}
	for in, ok := range map[string]bool{"": true, "3": true, "100%": true, "7%": true, "x": false, "-1": false, "101%": false, "12.5%": false, "+1": false, "007%": false} {
		if err := validIntOrPercent(in); (err == nil) != ok {
			t.Errorf("validIntOrPercent(%q) = %v", in, err)
		}
	}
	for in, want := range map[string]time.Duration{"": noDeadline, "None": noDeadline, "0s": 0, "5m": 5 * time.Minute, "1h30m": 90 * time.Minute} {
		if d, err := parseProgressDeadline(in); err != nil || d != want {
			t.Errorf("parseProgressDeadline(%q) = %v, %v", in, d, err)
		}
	}
	for _, in := range []string{"-5m", "1.5h", "5mx", "soon"} {
		if _, err := parseProgressDeadline(in); err == nil {
			t.Errorf("parseProgressDeadline(%q) accepted", in)
		}
	}
	if _, err := parseMinSuccessTime("-5m"); err == nil {
		t.Error("negative minSuccessTime accepted")
	}
	first, second := newWorkSuffix(), newWorkSuffix()
	if first == second || len(first) != generateNameLength || strings.Trim(first, generateNameAlphabet) != "" {
		t.Errorf("work suffixes are random five-character strings from the generateName alphabet, got %q %q", first, second)
	}
	if got := workNameFor("model-granite", "x7k2p"); got != "model-granitex7k2p" {
		t.Errorf("workNameFor = %q: generateName has no separator", got)
	}
	long := "model-" + strings.Repeat("a", 70)
	if got := workNameFor(long, "x7k2p"); len(got) != 63 || got != long[:58]+"x7k2p" {
		t.Errorf("a long base is cut to 58 characters: %q", got)
	}
}

func TestFailedClustersCountTowardsMaxFailuresAtOnce(t *testing.T) {
	// A placed model with a preset the spokes lack: every spoke's Ready goes
	// False/ConfigNotFound at T1, the Degraded rule fires and the works are
	// Failed. sdk-go counts them towards maxFailures at once, so a cluster
	// that joins later is not applied even though a slot is free.
	s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), readyCluster("site-c")}})
	m := placedModel("broken", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "3", MaxFailures: "0"})
	m.Spec.BaseRefs = []string{"v3-6-0-kserve-config-llm-template-tpu"}
	createModel(t, s, m)
	advance(llmisvcT1 + 3*time.Second)
	got := getModel(t, s, "broken")
	works := got.GetStatus().GetRollout().GetWorks()
	if len(works) != 2 {
		t.Fatalf("works = %v", works)
	}
	wantCond(t, works[0].GetConditions(), CondWorkProgressing, cTrue, ReasonConditionRulesAggregated, MessageRuleProgressing)
	wantCond(t, works[0].GetConditions(), CondWorkDegraded, cTrue, ReasonConditionRulesAggregated, MessageRuleDegraded)
	if fb := works[0].GetManifests()[1].GetFeedback(); len(fb) != 1 || fb[0].GetValue() != "False" {
		t.Errorf("feedback = %v", fb)
	}
	if sum := got.GetStatus().GetRollout().GetSummary(); sum.GetDegraded() != 2 || sum.GetProgressing() != 2 {
		t.Errorf("summary = %v", sum)
	}
	wantCond(t, got.GetStatus().GetRollout().GetConditions(), CondManifestworkApplied, cFalse, ReasonNotAsExpected, "")
	wantPhase(t, got, aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ReasonNotAsExpected)
	ctx := authed(context.Background())
	c, _ := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: "site-c"})
	labels := copyMap(c.GetObject().GetLabels())
	labels["ai-grid/enabled"] = "true"
	if _, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "site-c", Labels: labels}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathLabels}}}); err != nil {
		t.Fatal(err)
	}
	advance(llmisvcT1 + 10*time.Second)
	got = getModel(t, s, "broken")
	if len(got.GetStatus().GetPlacement().GetDecisions()) != 3 || len(got.GetStatus().GetRollout().GetWorks()) != 2 {
		t.Errorf("maxFailures breached: site-c is selected but never applied: %v", got.GetStatus().GetRollout().GetWorks())
	}
}

func TestProgressivePerGroupAndPercentConcurrency(t *testing.T) {
	clusters := []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c"), enrolled("site-d")}
	s, advance := modelServer(t, &Fixtures{Clusters: clusters})
	createModel(t, s, placedModel("half", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "50%"}))
	// ProgressivePerGroup rolls a whole decision group out at once; the
	// console's Placements have one group, so every cluster starts together.
	createModel(t, s, placedModel("group", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE_PER_GROUP, MaxFailures: "0"}))
	// maxConcurrency above the number of clusters falls back to all of them.
	createModel(t, s, placedModel("wide", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "9"}))
	names := func(name string) []string {
		var out []string
		for _, w := range getModel(t, s, name).GetStatus().GetRollout().GetWorks() {
			out = append(out, w.GetClusterName())
		}
		return out
	}
	all := []string{"site-a", "site-b", "site-c", "site-d"}
	if got := names("half"); !slices.Equal(got, []string{"site-a", "site-b"}) {
		t.Errorf("50%% of 4 is 2 at once: %v", got)
	}
	if got := names("group"); !slices.Equal(got, all) {
		t.Errorf("one decision group, all at once: %v", got)
	}
	if got := names("wide"); !slices.Equal(got, all) {
		t.Errorf("9 of 4 is all: %v", got)
	}
	advance(llmisvcT3 + time.Second)
	if got := names("half"); !slices.Equal(got, all) {
		t.Errorf("the next two once the first two succeeded: %v", got)
	}
	wantPhase(t, getModel(t, s, "group"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	advance(2*llmisvcT3 + time.Second)
	wantPhase(t, getModel(t, s, "half"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	// MWRS and Placement condition times do not move between polls.
	before := getModel(t, s, "group").GetStatus()
	advance(4*llmisvcT3 + time.Minute)
	after := getModel(t, s, "group").GetStatus()
	for _, typ := range []string{CondPlacementVerified, CondPlacementRolledOut, CondManifestworkApplied} {
		if a, b := condition(before.GetRollout().GetConditions(), typ), condition(after.GetRollout().GetConditions(), typ); !a.GetLastTransitionTime().AsTime().Equal(b.GetLastTransitionTime().AsTime()) {
			t.Errorf("%s moved between polls: %s -> %s", typ, a.GetLastTransitionTime().AsTime(), b.GetLastTransitionTime().AsTime())
		}
	}
	for _, typ := range []string{CondPlacementSatisfied, CondPlacementMisconfigured} {
		if a, b := condition(before.GetPlacement().GetConditions(), typ), condition(after.GetPlacement().GetConditions(), typ); !a.GetLastTransitionTime().AsTime().Equal(b.GetLastTransitionTime().AsTime()) {
			t.Errorf("%s moved between polls", typ)
		}
	}
	if rolled := condition(after.GetRollout().GetConditions(), CondPlacementRolledOut); !rolled.GetLastTransitionTime().AsTime().Equal(testStart.Add(llmisvcT3 + time.Second)) {
		t.Errorf("PlacementRolledOut was noticed at the poll that saw the rollout complete, got %s", rolled.GetLastTransitionTime().AsTime())
	}
}

func TestUpdatesAcrossGenerations(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{})
	ctx := authed(context.Background())
	createModel(t, s, hubModel("gen"))
	setImage := func(image string) {
		t.Helper()
		if _, err := s.UpdateModel(ctx, &aigriddraftv1.UpdateModelRequest{Object: &aigriddraftv1.Model{Name: "gen", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Main: &aigriddraftv1.Container{Image: image}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathSpecMainImage}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Closely spaced updates: generation 2 at 60s, generation 3 at 70s.
	// Generation 2's rollout would complete at 105s; it is superseded, so
	// Ready waits for generation 3 at 115s.
	advance(60 * time.Second)
	wantPhase(t, getModel(t, s, "gen"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	setImage("quay.io/example/vllm:v2")
	advance(70 * time.Second)
	setImage("quay.io/example/vllm:v3")
	advance(106 * time.Second)
	m := getModel(t, s, "gen")
	wantCond(t, m.GetStatus().GetConditions(), CondMainWorkloadReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
	wantCond(t, m.GetStatus().GetConditions(), CondReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
	if m.GetStatus().GetObservedGeneration() != 3 || m.GetStatus().GetWorkloads().GetPrimary().GetReadyReplicas() != 0 {
		t.Errorf("generation 2's completion must not mark generation 3 Ready: %v", m.GetStatus())
	}
	advance(116 * time.Second)
	m = getModel(t, s, "gen")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	if m.GetStatus().GetObservedGeneration() != 3 || !condition(m.GetStatus().GetConditions(), CondReady).GetLastTransitionTime().AsTime().Equal(testStart.Add(115*time.Second)) {
		t.Errorf("generation 3 Ready at 115s: %v", m.GetStatus())
	}

	// Healthy to stuck: generation 4 at 200s with an image that never comes
	// up stays at MinimumReplicasUnavailable, past its own T3 and past the
	// Deployment's 600s progress deadline: KServe ends on Available.
	advance(200 * time.Second)
	setImage("quay.io/example/vllm-oom:v4")
	advance(260 * time.Second)
	m = getModel(t, s, "gen")
	wantCond(t, m.GetStatus().GetConditions(), CondMainWorkloadReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
	if m.GetStatus().GetObservedGeneration() != 4 {
		t.Errorf("observedGeneration = %d", m.GetStatus().GetObservedGeneration())
	}
	advance(200*time.Second + llmisvcT2 + 600*time.Second + time.Second)
	wantPhase(t, getModel(t, s, "gen"), aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonMinimumReplicasUnavailable)

	// Recovery: generation 3 with a healthy image rolls out and becomes Ready
	// and stays Ready.
	s2, advance2 := modelServer(t, &Fixtures{})
	createModel(t, s2, hubModel("rec"))
	advance2(60 * time.Second)
	if _, err := s2.UpdateModel(ctx, &aigriddraftv1.UpdateModelRequest{Object: &aigriddraftv1.Model{Name: "rec", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Main: &aigriddraftv1.Container{Image: "quay.io/example/vllm-oom"}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathSpecMainImage}}}); err != nil {
		t.Fatal(err)
	}
	advance2(160 * time.Second)
	wantPhase(t, getModel(t, s2, "rec"), aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonMinimumReplicasUnavailable)
	if _, err := s2.UpdateModel(ctx, &aigriddraftv1.UpdateModelRequest{Object: &aigriddraftv1.Model{Name: "rec", Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Main: &aigriddraftv1.Container{Image: "quay.io/example/vllm:fixed"}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathSpecMainImage}}}); err != nil {
		t.Fatal(err)
	}
	advance2(160*time.Second + llmisvcT3 + time.Second)
	m = getModel(t, s2, "rec")
	wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
	if m.GetStatus().GetObservedGeneration() != 3 {
		t.Errorf("observedGeneration = %d", m.GetStatus().GetObservedGeneration())
	}
	advance2(60*time.Second + llmisvcT2 + 600*time.Second + time.Minute)
	wantPhase(t, getModel(t, s2, "rec"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "")
}

func TestUpdateDuringProgressiveRolloutReappliesStaleWorksInNameOrder(t *testing.T) {
	s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c")}})
	ctx := authed(context.Background())
	setImage := func(name, image string) {
		t.Helper()
		if _, err := s.UpdateModel(ctx, &aigriddraftv1.UpdateModelRequest{Object: &aigriddraftv1.Model{Name: name, Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Main: &aigriddraftv1.Container{Image: image}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathSpecMainImage}}}); err != nil {
			t.Fatal(err)
		}
	}
	createModel(t, s, placedModel("roll", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1", MinSuccessTime: "30s"}))
	// At 20s the template changes to an image that never comes up. The
	// controller counts only works at the current template, so site-a's
	// stale work holds no slot: site-a, first by name, is re-applied at once
	// with generation 2 and, stuck without a deadline, holds the slot for good.
	advance(20 * time.Second)
	setImage("roll", "quay.io/example/vllm-oom")
	for _, d := range []time.Duration{44 * time.Second, time.Hour} {
		advance(d)
		m := getModel(t, s, "roll")
		works := m.GetStatus().GetRollout().GetWorks()
		if len(works) != 1 || works[0].GetGeneration() != 2 || !condition(works[0].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(20*time.Second)) {
			t.Fatalf("at %s: works = %v", d, works)
		}
		wantCond(t, works[0].GetConditions(), CondWorkProgressing, cTrue, ReasonConditionRulesAggregated, MessageRuleProgressing)
		wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonProcessing)
	}

	// A healthy update after a complete rollout: the stale works are not
	// counted in the summary and are re-applied one at a time in name order.
	s2, advance2 := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c")}})
	s = s2
	createModel(t, s, placedModel("ok", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1"}))
	advance2(3*llmisvcT3 + time.Second)
	wantPhase(t, getModel(t, s, "ok"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	at := 3*llmisvcT3 + time.Second
	setImage("ok", "quay.io/example/vllm:v2")
	m := getModel(t, s, "ok")
	works := m.GetStatus().GetRollout().GetWorks()
	if len(works) != 3 || works[0].GetGeneration() != 2 || works[1].GetGeneration() != 1 || works[2].GetGeneration() != 1 {
		t.Fatalf("site-a re-applied at once, the others still stale: %v", works)
	}
	if sum := m.GetStatus().GetRollout().GetSummary(); sum.GetTotal() != 1 || sum.GetApplied() != 1 || sum.GetAvailable() != 0 {
		t.Errorf("only the work at the current template counts: %v", sum)
	}
	wantCond(t, m.GetStatus().GetRollout().GetConditions(), CondPlacementRolledOut, cFalse, ReasonRolloutProgressing, "")
	advance2(at + llmisvcT3 + time.Second)
	works = getModel(t, s, "ok").GetStatus().GetRollout().GetWorks()
	if works[1].GetGeneration() != 2 || works[2].GetGeneration() != 1 || !condition(works[1].GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(at+llmisvcT3)) {
		t.Errorf("site-b re-applied once site-a succeeded: %v", works)
	}
}

// workApplied is each work's cluster, own generation and Applied time.
func workApplied(m *aigriddraftv1.Model) map[string]string {
	out := map[string]string{}
	for _, w := range m.GetStatus().GetRollout().GetWorks() {
		out[w.GetClusterName()] = fmt.Sprintf("%s gen %d applied %s", w.GetWorkName(), w.GetGeneration(), condition(w.GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Format(time.RFC3339))
	}
	return out
}

// Only a change of the work template re-applies the works
// (deploy_reconcile.go:80-89): the rollout strategy is the MWRS's
// placementRefs and the Placement a separate object, so changing either
// leaves every existing work as it is.
func TestOnlyATemplateChangeReappliesWorks(t *testing.T) {
	ctx := authed(context.Background())
	three := func() *Fixtures {
		return &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c")}}
	}
	update := func(s *Server, obj *aigriddraftv1.Model, paths ...string) *aigriddraftv1.Model {
		t.Helper()
		obj.Metadata = &aigriddraftv1.ObjectMeta{Namespace: testNS}
		resp, err := s.UpdateModel(ctx, &aigriddraftv1.UpdateModelRequest{Object: obj, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetObject()
	}

	t.Run("maxConcurrency on a rolled-out model", func(t *testing.T) {
		s, advance := modelServer(t, three())
		createModel(t, s, placedModel("roll", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1"}))
		at := 3*llmisvcT3 + time.Second
		advance(at)
		before := getModel(t, s, "roll")
		wantPhase(t, before, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
		got := update(s, &aigriddraftv1.Model{Name: "roll", Spec: &aigriddraftv1.ModelSpec{Rollout: &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "2"}}}, PathSpecRollout)
		if got.GetMetadata().GetGeneration() != 2 {
			t.Errorf("a spec change bumps the model generation: %d", got.GetMetadata().GetGeneration())
		}
		for _, m := range []*aigriddraftv1.Model{got, func() *aigriddraftv1.Model { advance(at + time.Hour); return getModel(t, s, "roll") }()} {
			wantPhase(t, m, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
			if got, want := workApplied(m), workApplied(before); !maps.Equal(got, want) {
				t.Errorf("no work re-applied: %v, before %v", got, want)
			}
			if sum := m.GetStatus().GetRollout().GetSummary(); sum.GetTotal() != 3 || sum.GetAvailable() != 3 {
				t.Errorf("summary = %v", sum)
			}
		}
	})

	t.Run("placement adds a cluster", func(t *testing.T) {
		s, advance := modelServer(t, three())
		m := placedModel("grow", nil)
		m.Spec.Placement.NumberOfClusters = proto.Int32(2)
		createModel(t, s, m)
		at := llmisvcT3 + time.Second
		advance(at)
		before := getModel(t, s, "grow")
		wantPhase(t, before, aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
		if len(before.GetStatus().GetRollout().GetWorks()) != 2 {
			t.Fatalf("works = %v", before.GetStatus().GetRollout().GetWorks())
		}
		p := proto.Clone(m.GetSpec().GetPlacement()).(*aigriddraftv1.Placement)
		p.NumberOfClusters = nil
		got := update(s, &aigriddraftv1.Model{Name: "grow", Spec: &aigriddraftv1.ModelSpec{Placement: p}}, PathSpecPlacement)
		applied := workApplied(got)
		if len(applied) != 3 || applied["site-a"] != workApplied(before)["site-a"] || applied["site-b"] != workApplied(before)["site-b"] {
			t.Errorf("site-a and site-b kept as they were: %v, before %v", applied, workApplied(before))
		}
		if want := fmt.Sprintf("model-grow%s gen 1 applied %s", "w0003", testStart.Add(at).Format(time.RFC3339)); applied["site-c"] != want {
			t.Errorf("site-c = %q, want %q", applied["site-c"], want)
		}
		advance(2*at + time.Second)
		wantPhase(t, getModel(t, s, "grow"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	})

	t.Run("image re-applies every work", func(t *testing.T) {
		s, advance := modelServer(t, three())
		createModel(t, s, placedModel("img", nil))
		at := llmisvcT3 + time.Second
		advance(at)
		wantPhase(t, getModel(t, s, "img"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
		got := update(s, &aigriddraftv1.Model{Name: "img", Spec: &aigriddraftv1.ModelSpec{Main: &aigriddraftv1.Container{Image: "quay.io/example/vllm:v2"}}}, PathSpecMainImage)
		works := got.GetStatus().GetRollout().GetWorks()
		if len(works) != 3 {
			t.Fatalf("works = %v", works)
		}
		for _, w := range works {
			if w.GetGeneration() != 2 || !condition(w.GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Equal(testStart.Add(at)) {
				t.Errorf("%s re-applied at the update with generation 2: %v", w.GetClusterName(), w)
			}
		}
		wantPhase(t, got, aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ReasonNotAsExpected)
		advance(2*at + time.Second)
		wantPhase(t, getModel(t, s, "img"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
	})
}

// A rollout change re-applies nothing, and the works still to apply are
// scheduled under the new settings from the change on: none is applied at
// an earlier instant, which would replay its lifecycle as already under way.
func TestARolloutChangeNeverSchedulesAWorkBeforeIt(t *testing.T) {
	ctx := authed(context.Background())
	stuck := func(name string, rollout *aigriddraftv1.Rollout) (*Server, func(time.Duration)) {
		t.Helper()
		s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c")}})
		m := placedModel(name, rollout)
		m.Spec.Main = &aigriddraftv1.Container{Image: "quay.io/example/vllm-oom"} // never comes up
		createModel(t, s, m)
		return s, advance
	}
	setRollout := func(s *Server, name string, rollout *aigriddraftv1.Rollout) *aigriddraftv1.Model {
		t.Helper()
		resp, err := s.UpdateModel(ctx, &aigriddraftv1.UpdateModelRequest{Object: &aigriddraftv1.Model{Name: name, Metadata: &aigriddraftv1.ObjectMeta{Namespace: testNS}, Spec: &aigriddraftv1.ModelSpec{Rollout: rollout}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathSpecRollout}}})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetObject()
	}
	appliedAt := func(m *aigriddraftv1.Model) map[string]time.Duration {
		out := map[string]time.Duration{}
		for _, w := range m.GetStatus().GetRollout().GetWorks() {
			out[w.GetClusterName()] = condition(w.GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Sub(testStart)
		}
		return out
	}
	const change = 600 * time.Second

	t.Run("maxConcurrency raised while a stuck work holds the only slot", func(t *testing.T) {
		s, advance := stuck("wide", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1"})
		advance(change)
		if got := appliedAt(getModel(t, s, "wide")); !maps.Equal(got, map[string]time.Duration{"site-a": 0}) {
			t.Fatalf("site-a holds the only slot: %v", got)
		}
		got := setRollout(s, "wide", &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "3"})
		if want := map[string]time.Duration{"site-a": 0, "site-b": change, "site-c": change}; !maps.Equal(appliedAt(got), want) {
			t.Errorf("site-b and site-c applied at the PATCH, site-a kept: %v, want %v", appliedAt(got), want)
		}
		for _, w := range got.GetStatus().GetRollout().GetWorks() {
			if w.GetGeneration() != 1 {
				t.Errorf("%s: generation %d, want 1", w.GetClusterName(), w.GetGeneration())
			}
			if w.GetClusterName() != "site-a" {
				wantNoCond(t, w.GetConditions(), CondWorkAvailable) // the agent has not synced yet
			}
		}
	})

	t.Run("maxFailures raised after a breach", func(t *testing.T) {
		rollout := &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1", ProgressDeadline: "60s"}
		s, advance := stuck("brk", rollout)
		// site-a times out about a minute in: a failure beyond maxFailures 0
		// stops the rollout.
		advance(change)
		if got := appliedAt(getModel(t, s, "brk")); !maps.Equal(got, map[string]time.Duration{"site-a": 0}) {
			t.Fatalf("the rollout stopped after site-a: %v", got)
		}
		raised := proto.Clone(rollout).(*aigriddraftv1.Rollout)
		raised.MaxFailures = "2"
		got := setRollout(s, "brk", raised)
		if want := map[string]time.Duration{"site-a": 0, "site-b": change}; !maps.Equal(appliedAt(got), want) {
			t.Errorf("site-b applied at the PATCH, not when site-a timed out: %v, want %v", appliedAt(got), want)
		}
	})
}

// A decision change without a PATCH (a cluster relabelled, tainted or
// terminating) schedules the works still to apply from when the fake
// notices it, never at an earlier instant judged with the new decision.
func TestADecisionChangeNeverSchedulesAWorkBeforeIt(t *testing.T) {
	ctx := authed(context.Background())
	const change = 600 * time.Second
	setEnabled := func(s *Server, cluster string, enabled bool) {
		t.Helper()
		resp, err := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: cluster})
		if err != nil {
			t.Fatal(err)
		}
		labels := copyMap(resp.GetObject().GetLabels())
		if enabled {
			labels["ai-grid/enabled"] = "true"
		} else {
			delete(labels, "ai-grid/enabled")
		}
		if _, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: cluster, Labels: labels}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathLabels}}}); err != nil {
			t.Fatal(err)
		}
	}
	appliedAt := func(m *aigriddraftv1.Model) map[string]time.Duration {
		out := map[string]time.Duration{}
		for _, w := range m.GetStatus().GetRollout().GetWorks() {
			out[w.GetClusterName()] = condition(w.GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Sub(testStart)
		}
		return out
	}
	stuck := func(s *Server, name, maxConcurrency string) {
		t.Helper()
		m := placedModel(name, &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: maxConcurrency})
		m.Spec.Main = &aigriddraftv1.Container{Image: "quay.io/example/vllm-oom"} // never comes up
		createModel(t, s, m)
	}

	t.Run("the cluster holding the only slot leaves", func(t *testing.T) {
		s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c")}})
		stuck(s, "leave", "1")
		advance(change)
		if got := appliedAt(getModel(t, s, "leave")); !maps.Equal(got, map[string]time.Duration{"site-a": 0}) {
			t.Fatalf("site-a holds the only slot: %v", got)
		}
		setEnabled(s, "site-a", false)
		if got, want := appliedAt(getModel(t, s, "leave")), map[string]time.Duration{"site-b": change}; !maps.Equal(got, want) {
			t.Errorf("site-a's work gone, site-b applied when the fake noticed: %v, want %v", got, want)
		}
	})

	t.Run("a percentage maxConcurrency over a growing decision", func(t *testing.T) {
		off := enrolled("site-c")
		delete(off.Labels, "ai-grid/enabled")
		s, advance := modelServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), off}})
		stuck(s, "grow", "50%") // 2 clusters: 1 slot
		advance(change)
		if got := appliedAt(getModel(t, s, "grow")); !maps.Equal(got, map[string]time.Duration{"site-a": 0}) {
			t.Fatalf("site-a holds the only slot: %v", got)
		}
		setEnabled(s, "site-c", true) // 3 clusters: 2 slots
		if got, want := appliedAt(getModel(t, s, "grow")), map[string]time.Duration{"site-a": 0, "site-b": change}; !maps.Equal(got, want) {
			t.Errorf("site-b takes the second slot when the fake noticed the third cluster: %v, want %v", got, want)
		}
	})
}

// A decision change catches the rollout up under the previous decision
// first, so the instant each work is applied does not depend on how often
// the fake is read: site-d terminating at 100 s leaves site-b and site-c
// applied when their slots freed, as the controller would, with or without
// a read in between. The same when site-d leaves because a toleration
// expires, which no event marks: the first pass after it catches up.
func TestRolloutTimingAcrossADecisionChangeIsIndependentOfPolling(t *testing.T) {
	const detach = 100 * time.Second
	rollout := &aigriddraftv1.Rollout{Strategy: aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE, MaxConcurrency: "1"}
	clusters := func() []*aigriddraftv1.ManagedCluster {
		return []*aigriddraftv1.ManagedCluster{enrolled("site-a"), enrolled("site-b"), enrolled("site-c"), enrolled("site-d")}
	}
	appliedAt := func(m *aigriddraftv1.Model) map[string]time.Duration {
		out := map[string]time.Duration{}
		for _, w := range m.GetStatus().GetRollout().GetWorks() {
			out[w.GetClusterName()] = condition(w.GetConditions(), CondWorkApplied).GetLastTransitionTime().AsTime().Sub(testStart)
		}
		return out
	}
	// The controller's times: the same rollout with no decision change,
	// read once site-c is applied and before site-d's turn.
	control, advance := modelServer(t, &Fixtures{Clusters: clusters()})
	createModel(t, control, placedModel("poll", rollout))
	advance(detach)
	want := appliedAt(getModel(t, control, "poll"))
	if len(want) != 3 || want["site-b"] <= 0 || want["site-c"] <= want["site-b"] || want["site-c"] >= detach {
		t.Fatalf("control: site-b and site-c applied one after the other before %s: %v", detach, want)
	}
	delete(want, "site-d")

	const maintenance = "example.com/maintenance"
	leaves := map[string]func() (*Fixtures, *aigriddraftv1.Model){
		"detaching": func() (*Fixtures, *aigriddraftv1.Model) {
			return &Fixtures{Clusters: clusters(), Scenarios: []ScenarioEvent{{Cluster: "site-d", At: Duration{detach}, Event: EventDetaching}}}, placedModel("poll", rollout)
		},
		"toleration expiry": func() (*Fixtures, *aigriddraftv1.Model) {
			cs := clusters()
			cs[3].Taints = []*aigriddraftv1.Taint{{Key: maintenance, Effect: TaintNoSelect, TimeAdded: timestamppb.New(testStart)}}
			m := placedModel("poll", rollout)
			m.Spec.Placement.Tolerations = []*aigriddraftv1.Toleration{{Key: maintenance, Operator: TolerationExists, TolerationSeconds: proto.Int64(int64(detach / time.Second))}}
			return &Fixtures{Clusters: cs}, m
		},
	}
	for name, setup := range leaves {
		for _, reads := range [][]time.Duration{nil, {50 * time.Second}, {50 * time.Second, want["site-c"] - time.Second}, {detach - time.Second}} {
			fx, m := setup()
			s, advance := modelServer(t, fx)
			createModel(t, s, m)
			for _, at := range reads {
				advance(at)
				getModel(t, s, "poll")
			}
			advance(detach + time.Second)
			got := getModel(t, s, "poll")
			if n := got.GetStatus().GetPlacement().GetNumberOfSelectedClusters(); n != 3 {
				t.Errorf("%s: site-d left the decision: %d selected", name, n)
			}
			if applied := appliedAt(got); !maps.Equal(applied, want) {
				t.Errorf("%s, reads at %v: works applied at %v, want %v", name, reads, applied, want)
			}
		}
	}
}

func TestDisconnectedAgentFreezesTheWork(t *testing.T) {
	c := enrolled("site-flaky")
	fx := &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{c}, Scenarios: []ScenarioEvent{
		{Cluster: "site-flaky", At: Duration{10 * time.Second}, Event: EventLeaseLost},
		{Cluster: "site-flaky", At: Duration{120 * time.Second}, Event: EventLeaseRecovered},
	}}
	s, advance := modelServer(t, fx)
	m := placedModel("flaky", nil)
	m.Spec.Placement.Tolerations = []*aigriddraftv1.Toleration{{Key: TaintUnreachable, Operator: TolerationExists}}
	createModel(t, s, m)
	snapshot := func(d time.Duration) *aigriddraftv1.ManifestWorkStatus {
		t.Helper()
		advance(d)
		works := getModel(t, s, "flaky").GetStatus().GetRollout().GetWorks()
		if len(works) != 1 {
			t.Fatalf("at %s: works = %v", d, works)
		}
		return works[0]
	}
	// The agent reported until 10s (T1 passed on the spoke, Ready False), then
	// lost its lease: the hub keeps that report unchanged.
	at60 := snapshot(60 * time.Second)
	wantCond(t, at60.GetConditions(), CondWorkProgressing, cTrue, ReasonConditionRulesAggregated, MessageRuleProgressing)
	if fb := at60.GetManifests()[1].GetFeedback(); len(fb) != 2 || fb[0].GetValue() != "False" || fb[1].GetName() != FeedbackURL {
		t.Errorf("frozen feedback = %v", fb)
	}
	at100 := snapshot(100 * time.Second)
	if !proto.Equal(at60, at100) {
		t.Errorf("the status moved while the agent was disconnected:\n%v\n%v", at60, at100)
	}
	// Back at 120s: the spoke resumes where it stopped (10s into its
	// lifecycle), so Ready comes at 120 + 35 = 155s.
	at150 := snapshot(150 * time.Second)
	if fb := at150.GetManifests()[1].GetFeedback(); len(fb) != 2 || fb[0].GetValue() != "False" {
		t.Errorf("still starting after reconnect: %v", fb)
	}
	at160 := snapshot(160 * time.Second)
	prog := wantCond(t, at160.GetConditions(), CondWorkProgressing, cFalse, ReasonConditionRulesAggregated, MessageRuleProgressing)
	if !prog.GetLastTransitionTime().AsTime().Equal(testStart.Add(155 * time.Second)) {
		t.Errorf("Progressing fell at 155s, got %s", prog.GetLastTransitionTime().AsTime())
	}
	if fb := at160.GetManifests()[1].GetFeedback(); len(fb) != 2 || fb[0].GetValue() != "True" {
		t.Errorf("feedback = %v", fb)
	}
	wantPhase(t, getModel(t, s, "flaky"), aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete)
}

func TestWorkSuffixGeneratorRenamesExistingWorks(t *testing.T) {
	// The shipped fixtures place models on several sites, so New already
	// generated works with random suffixes. A generator set afterwards
	// renames them in model and cluster order, so two servers built the
	// same way serve the same work names (what the golden responses need).
	names := func() []string {
		fx, err := Load(repoFixtures)
		if err != nil {
			t.Fatal(err)
		}
		s := newServer(t, fx)
		s.SetClock(func() time.Time { return testStart })
		n := 0
		s.SetWorkSuffixGenerator(func() string { n++; return fmt.Sprintf("g%04d", n) })
		resp, err := s.ListModels(authed(context.Background()), &aigriddraftv1.ListModelsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range resp.GetItems() {
			for _, w := range m.GetStatus().GetRollout().GetWorks() {
				if !strings.HasPrefix(w.GetWorkName(), "model-"+m.GetName()+"g") {
					t.Errorf("%s: work %q was not renamed by the generator", m.GetName(), w.GetWorkName())
				}
				out = append(out, w.GetClusterName()+"="+w.GetWorkName())
			}
		}
		return out
	}
	first, second := names(), names()
	if len(first) == 0 {
		t.Fatal("the fixture models have no works; the test proves nothing")
	}
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("work names differ between two identical servers:\n%v\n%v", first, second)
	}
}
