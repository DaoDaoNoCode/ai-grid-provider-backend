package servers

import (
	"testing"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func cond(typ, status, reason, message string) map[string]interface{} {
	c := map[string]interface{}{"type": typ, "status": status}
	if reason != "" {
		c["reason"] = reason
	}
	if message != "" {
		c["message"] = message
	}
	return c
}

func mwrsWithConditions(conds ...map[string]interface{}) *unstructured.Unstructured {
	raw := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		raw = append(raw, c)
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"conditions": raw},
	}}
}

// llmManifest is the LLMInferenceService entry of
// status.resourceStatus.manifests[] with a `ready` feedback value of type
// String; ready "" leaves the feedback out.
func llmManifest(ready string) map[string]interface{} {
	m := map[string]interface{}{
		"resourceMeta": map[string]interface{}{
			"ordinal": int64(0), "group": "serving.kserve.io", "version": "v1alpha2",
			"resource": "llminferenceservices", "kind": "LLMInferenceService",
			"namespace": "ai-grid-models", "name": "m",
		},
		"conditions": []interface{}{},
	}
	if ready != "" {
		m["statusFeedback"] = map[string]interface{}{
			"values": []interface{}{
				map[string]interface{}{
					"name":       "ready",
					"fieldValue": map[string]interface{}{"type": "String", "string": ready},
				},
			},
		}
	}
	return m
}

func manifestWork(cluster, ready string, conds ...map[string]interface{}) unstructured.Unstructured {
	rawConds := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		rawConds = append(rawConds, c)
	}
	status := map[string]interface{}{"conditions": rawConds}
	if ready != "" {
		status["resourceStatus"] = map[string]interface{}{
			"manifests": []interface{}{llmManifest(ready)},
		}
	}
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "work.open-cluster-management.io/v1",
		"kind":       "ManifestWork",
		"metadata":   map[string]interface{}{"name": "model-m", "namespace": cluster},
		"spec": map[string]interface{}{
			"manifestConfigs": []interface{}{llmManifestConfig("ai-grid-models", "m")},
		},
		"status": status,
	}}
}

// legacyWork is a work generated from a template without manifestConfigs:
// an MWRS created before the backend asked for feedback, or by GitOps.
func legacyWork(cluster string, conds ...map[string]interface{}) unstructured.Unstructured {
	w := manifestWork(cluster, "", conds...)
	delete(w.Object, "spec")
	return w
}

// withGenerations adds the generation and observedGeneration feedback values.
func withGenerations(w unstructured.Unstructured, generation, observed int64) unstructured.Unstructured {
	manifests, _, _ := unstructured.NestedSlice(w.Object, "status", "resourceStatus", "manifests")
	m := manifests[0].(map[string]interface{})
	values, _, _ := unstructured.NestedSlice(m, "statusFeedback", "values")
	for name, v := range map[string]int64{"generation": generation, "observedGeneration": observed} {
		values = append(values, map[string]interface{}{
			"name": name, "fieldValue": map[string]interface{}{"type": "Integer", "integer": v},
		})
	}
	_ = unstructured.SetNestedSlice(m, values, "statusFeedback", "values")
	_ = unstructured.SetNestedSlice(w.Object, manifests, "status", "resourceStatus", "manifests")
	return w
}

var (
	condAppliedTrue     = cond("Applied", "True", "AppliedManifestWorkComplete", "Apply manifest work complete")
	condAppliedFalse    = cond("Applied", "False", "AppliedManifestWorkFailed", "Failed to apply manifest work")
	condAvailableTrue   = cond("Available", "True", "ResourcesAvailable", "All resources are available")
	condAvailableUnk    = cond("Available", "Unknown", "ResourcesStatusUnknown", "1 of 2 resources have unknown status")
	condProgressingTrue = cond("Progressing", "True", "ConditionRulesAggregated", "LLMInferenceService rolling out")
	condProgressingOff  = cond("Progressing", "False", "ConditionRulesAggregated", "LLMInferenceService rolling out")
	condDegradedFalse   = cond("Degraded", "False", "ConditionRulesAggregated", "LLMInferenceService not ready")
	condDegradedTrue    = cond("Degraded", "True", "ConditionRulesAggregated", "LLMInferenceService not ready")
)

func TestModelWorkStatusFromManifestWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		work        unstructured.Unstructured
		wantPhase   aigridv1.ModelPhase
		wantReason  string
		wantMessage string
		wantReady   *bool
	}{
		{
			name:      "ready feedback True is READY",
			work:      manifestWork("site-a", "True", condAppliedTrue, condAvailableTrue, condProgressingOff, condDegradedFalse),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_READY,
			wantReady: boolPtr(true),
		},
		{
			name:        "Applied False is FAILED with its reason, before Degraded",
			work:        manifestWork("site-a", "", condAppliedFalse, condAvailableUnk, condDegradedTrue),
			wantPhase:   aigridv1.ModelPhase_MODEL_PHASE_FAILED,
			wantReason:  "AppliedManifestWorkFailed",
			wantMessage: "Failed to apply manifest work",
		},
		{
			name:        "Degraded True is FAILED",
			work:        manifestWork("site-a", "False", condAppliedTrue, condAvailableTrue, condProgressingTrue, condDegradedTrue),
			wantPhase:   aigridv1.ModelPhase_MODEL_PHASE_FAILED,
			wantReason:  "ConditionRulesAggregated",
			wantMessage: "LLMInferenceService not ready",
			wantReady:   boolPtr(false),
		},
		{
			name:        "Available not True is PENDING with its reason",
			work:        manifestWork("site-a", "", condAppliedTrue, condAvailableUnk),
			wantPhase:   aigridv1.ModelPhase_MODEL_PHASE_PENDING,
			wantReason:  "ResourcesStatusUnknown",
			wantMessage: "1 of 2 resources have unknown status",
		},
		{
			name:        "still Progressing is PENDING with its reason",
			work:        manifestWork("site-a", "False", condAppliedTrue, condAvailableTrue, condProgressingTrue, condDegradedFalse),
			wantPhase:   aigridv1.ModelPhase_MODEL_PHASE_PENDING,
			wantReason:  "ConditionRulesAggregated",
			wantMessage: "LLMInferenceService rolling out",
			wantReady:   boolPtr(false),
		},
		{
			name:      "agent has not reported: PENDING without reason",
			work:      manifestWork("site-a", ""),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING,
		},
		{
			name:      "ready feedback for the current generation is READY",
			work:      withGenerations(manifestWork("site-a", "True", condAppliedTrue, condAvailableTrue, condProgressingOff), 3, 3),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_READY,
			wantReady: boolPtr(true),
		},
		{
			name:        "Ready from an older generation does not count",
			work:        withGenerations(manifestWork("site-a", "True", condAppliedTrue, condAvailableTrue, condProgressingTrue, condDegradedFalse), 2, 1),
			wantPhase:   aigridv1.ModelPhase_MODEL_PHASE_PENDING,
			wantReason:  "ConditionRulesAggregated",
			wantMessage: "LLMInferenceService rolling out",
		},
		{
			name:      "template without ready feedback: Applied and Available is READY",
			work:      legacyWork("site-a", condAppliedTrue, condAvailableTrue),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_READY,
		},
		{
			name:        "template without ready feedback: still Progressing is PENDING",
			work:        legacyWork("site-a", condAppliedTrue, condAvailableTrue, condProgressingTrue),
			wantPhase:   aigridv1.ModelPhase_MODEL_PHASE_PENDING,
			wantReason:  "ConditionRulesAggregated",
			wantMessage: "LLMInferenceService rolling out",
		},
		{
			name:        "template without ready feedback: Applied False is FAILED",
			work:        legacyWork("site-a", condAppliedFalse, condAvailableUnk),
			wantPhase:   aigridv1.ModelPhase_MODEL_PHASE_FAILED,
			wantReason:  "AppliedManifestWorkFailed",
			wantMessage: "Failed to apply manifest work",
		},
		{
			name:      "template with ready feedback not reported yet stays PENDING",
			work:      manifestWork("site-a", "", condAppliedTrue, condAvailableTrue),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING,
		},
		{
			name:      "ready Unknown leaves ready unset",
			work:      manifestWork("site-a", "Unknown", condAppliedTrue, condAvailableTrue, condProgressingTrue),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "ConditionRulesAggregated",
			wantMessage: "LLMInferenceService rolling out",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := modelWorkStatusFromManifestWork(tt.work)
			if got.GetCluster() != "site-a" {
				t.Fatalf("cluster = %q", got.GetCluster())
			}
			if got.GetPhase() != tt.wantPhase || got.GetReason() != tt.wantReason || got.GetMessage() != tt.wantMessage {
				t.Fatalf("phase/reason/message = %v/%q/%q, want %v/%q/%q",
					got.GetPhase(), got.GetReason(), got.GetMessage(), tt.wantPhase, tt.wantReason, tt.wantMessage)
			}
			if (got.Ready == nil) != (tt.wantReady == nil) || (got.Ready != nil && *got.Ready != *tt.wantReady) {
				t.Fatalf("ready = %v, want %v", got.Ready, tt.wantReady)
			}
		})
	}
}

func TestModelWorkStatusKeepsConditions(t *testing.T) {
	t.Parallel()
	applied := cond("Applied", "True", "AppliedManifestWorkComplete", "Apply manifest work complete")
	applied["lastTransitionTime"] = "2026-10-20T09:10:05Z"
	applied["observedGeneration"] = int64(2)
	got := modelWorkStatusFromManifestWork(manifestWork("site-a", "", applied, condAvailableTrue))
	if len(got.GetConditions()) != 2 {
		t.Fatalf("conditions = %v", got.GetConditions())
	}
	c := got.GetConditions()[0]
	if c.GetType() != "Applied" || c.GetStatus() != aigridv1.ConditionStatus_CONDITION_STATUS_TRUE ||
		c.GetReason() != "AppliedManifestWorkComplete" || c.GetMessage() != "Apply manifest work complete" ||
		c.GetObservedGeneration() != 2 {
		t.Fatalf("Applied condition = %v", c)
	}
	want := time.Date(2026, 10, 20, 9, 10, 5, 0, time.UTC)
	if !c.GetLastTransitionTime().AsTime().Equal(want) {
		t.Fatalf("lastTransitionTime = %v, want %v", c.GetLastTransitionTime().AsTime(), want)
	}
	if got.GetApplied() != "True" || got.GetAvailable() != "True" || got.GetProgressing() != "" {
		t.Fatalf("summary strings = %q %q %q", got.GetApplied(), got.GetAvailable(), got.GetProgressing())
	}
}

func TestReadyFeedbackBoolean(t *testing.T) {
	t.Parallel()
	work := manifestWork("site-a", "True")
	manifests, _, _ := unstructured.NestedSlice(work.Object, "status", "resourceStatus", "manifests")
	m := manifests[0].(map[string]interface{})
	m["statusFeedback"] = map[string]interface{}{"values": []interface{}{
		map[string]interface{}{"name": "ready", "fieldValue": map[string]interface{}{"type": "Boolean", "boolean": false}},
	}}
	_ = unstructured.SetNestedSlice(work.Object, manifests, "status", "resourceStatus", "manifests")
	if got := readyFromFeedback(llmFeedbackValues(work)); got == nil || *got {
		t.Fatalf("ready = %v, want false", got)
	}
}

func TestReadyFeedbackIgnoresOtherManifests(t *testing.T) {
	t.Parallel()
	work := manifestWork("site-a", "True")
	manifests, _, _ := unstructured.NestedSlice(work.Object, "status", "resourceStatus", "manifests")
	m := manifests[0].(map[string]interface{})
	m["resourceMeta"] = map[string]interface{}{"group": "", "resource": "namespaces", "kind": "Namespace", "name": "ai-grid-models"}
	_ = unstructured.SetNestedSlice(work.Object, manifests, "status", "resourceStatus", "manifests")
	if got := readyFromFeedback(llmFeedbackValues(work)); got != nil {
		t.Fatalf("ready = %v, want unset for a Namespace manifest", *got)
	}
}

// placedModel builds a placed model as enrichPlacement does: placement status
// with `selected` clusters, the MWRS conditions, then the works.
func placedModel(selected int, mwrs *unstructured.Unstructured, works ...unstructured.Unstructured) *aigridv1.Model {
	return placedModelOn(nil, selected, mwrs, works...)
}

// placedModelOn is placedModel with known decisions (clusters).
func placedModelOn(clusters []string, selected int, mwrs *unstructured.Unstructured, works ...unstructured.Unstructured) *aigridv1.Model {
	if clusters != nil {
		selected = len(clusters)
	}
	model := &aigridv1.Model{
		Spec: &aigridv1.ModelSpec{},
		Status: &aigridv1.ModelStatus{Placement: &aigridv1.ModelPlacementStatus{
			NumberOfSelectedClusters: int32(selected), SelectedClusters: clusters,
		}},
	}
	enrichModelFromMWRS(model, mwrs)
	for _, w := range works {
		model.Status.Rollout.Works = append(model.Status.Rollout.Works, modelWorkStatusFromManifestWork(w))
	}
	deriveModelPhaseFromRollout(model)
	return model
}

func TestDeriveModelPhaseFromRollout(t *testing.T) {
	t.Parallel()
	rolledOut := cond("PlacementRolledOut", "True", "Complete", "")
	rolling := cond("PlacementRolledOut", "False", "Progressing", "")
	appliedOK := cond("ManifestworkApplied", "True", "AsExpected", "")
	verified := cond("PlacementVerified", "True", "AsExpected", "")
	ready := func(c string) unstructured.Unstructured {
		return manifestWork(c, "True", condAppliedTrue, condAvailableTrue, condProgressingOff, condDegradedFalse)
	}
	starting := func(c string) unstructured.Unstructured {
		return manifestWork(c, "False", condAppliedTrue, condAvailableTrue, condProgressingTrue, condDegradedFalse)
	}
	failedApply := func(c string) unstructured.Unstructured {
		return manifestWork(c, "", condAppliedFalse, condAvailableUnk)
	}
	tests := []struct {
		name        string
		selected    int
		mwrs        *unstructured.Unstructured
		works       []unstructured.Unstructured
		wantPhase   aigridv1.ModelPhase
		wantReason  string
		wantMessage string
	}{
		{
			name:     "every location serving is READY",
			selected: 2, mwrs: mwrsWithConditions(verified, rolledOut, appliedOK),
			works:     []unstructured.Unstructured{ready("site-a"), ready("site-b")},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_READY, wantReason: "Complete",
		},
		{
			name:     "stale summary: MWRS rolled out but the only work failed is FAILED",
			selected: 1, mwrs: mwrsWithConditions(verified, rolledOut, appliedOK),
			works:      []unstructured.Unstructured{failedApply("site-a")},
			wantPhase:  aigridv1.ModelPhase_MODEL_PHASE_FAILED,
			wantReason: "AppliedManifestWorkFailed", wantMessage: "Failed to apply manifest work",
		},
		{
			name:     "stale summary: MWRS rolled out, one work failed, one serving is PARTIALLY_AVAILABLE",
			selected: 2, mwrs: mwrsWithConditions(verified, rolledOut, appliedOK),
			works:      []unstructured.Unstructured{ready("site-a"), failedApply("site-b")},
			wantPhase:  aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE,
			wantReason: "PartiallyAvailable", wantMessage: "1 of 2 clusters are serving the model.",
		},
		{
			name:     "one serving, one starting is PARTIALLY_AVAILABLE",
			selected: 2, mwrs: mwrsWithConditions(verified, rolling, cond("ManifestworkApplied", "False", "Processing", "")),
			works:      []unstructured.Unstructured{ready("site-a"), starting("site-b")},
			wantPhase:  aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE,
			wantReason: "PartiallyAvailable", wantMessage: "1 of 2 clusters are serving the model.",
		},
		{
			name:     "a selected cluster without a work counts as not serving",
			selected: 3, mwrs: mwrsWithConditions(verified, rolling, appliedOK),
			works:      []unstructured.Unstructured{ready("site-a"), ready("site-b")},
			wantPhase:  aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE,
			wantReason: "PartiallyAvailable", wantMessage: "2 of 3 clusters are serving the model.",
		},
		{
			name:     "no location serving and a degraded work is FAILED",
			selected: 2, mwrs: mwrsWithConditions(verified, rolling, cond("ManifestworkApplied", "False", "NotAsExpected", "")),
			works: []unstructured.Unstructured{
				starting("site-a"),
				manifestWork("site-b", "False", condAppliedTrue, condAvailableTrue, condProgressingTrue, condDegradedTrue),
			},
			wantPhase:  aigridv1.ModelPhase_MODEL_PHASE_FAILED,
			wantReason: "ConditionRulesAggregated", wantMessage: "LLMInferenceService not ready",
		},
		{
			name:     "NotAsExpected without a failed work stays PENDING",
			selected: 1, mwrs: mwrsWithConditions(verified, rolling, cond("ManifestworkApplied", "False", "NotAsExpected", "")),
			works:     []unstructured.Unstructured{starting("site-a")},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "NotAsExpected",
		},
		{
			name:     "PlacementVerified False decides the PENDING reason",
			selected: 0, mwrs: mwrsWithConditions(
				cond("PlacementVerified", "False", "PlacementDecisionEmpty", ""),
				rolledOut, cond("ManifestworkApplied", "False", "PlacementDecisionEmpty", "")),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "PlacementDecisionEmpty",
		},
		{
			name:     "applied so far, more clusters to come: PENDING with PlacementRolledOut's reason",
			selected: 2, mwrs: mwrsWithConditions(verified, rolling, appliedOK),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "Progressing",
		},
		{
			name:     "no works readable: MWRS rolled out and applied is READY",
			selected: 2, mwrs: mwrsWithConditions(verified, rolledOut, appliedOK),
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_READY, wantReason: "Complete",
		},
		{
			name:      "MWRS without status is PENDING without reason",
			selected:  1,
			mwrs:      &unstructured.Unstructured{Object: map[string]interface{}{}},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := placedModel(tt.selected, tt.mwrs, tt.works...).GetStatus()
			if st.GetPhase() != tt.wantPhase || st.GetReason() != tt.wantReason || st.GetMessage() != tt.wantMessage {
				t.Fatalf("phase/reason/message = %v/%q/%q, want %v/%q/%q",
					st.GetPhase(), st.GetReason(), st.GetMessage(), tt.wantPhase, tt.wantReason, tt.wantMessage)
			}
		})
	}
}

// TestLegacyTemplateRolledOutIsReady: an MWRS created without
// manifestConfigs (before the backend asked for feedback, or by GitOps)
// generates works that never report ready; rolled out with every work
// applied and available, the model is READY, not stuck in PENDING.
func TestLegacyTemplateRolledOutIsReady(t *testing.T) {
	t.Parallel()
	mwrs := mwrsWithConditions(
		cond("PlacementVerified", "True", "AsExpected", ""),
		cond("PlacementRolledOut", "True", "Complete", ""),
		cond("ManifestworkApplied", "True", "AsExpected", ""))
	st := placedModelOn([]string{"site-a", "site-b"}, 0, mwrs,
		legacyWork("site-a", condAppliedTrue, condAvailableTrue),
		legacyWork("site-b", condAppliedTrue, condAvailableTrue)).GetStatus()
	if st.GetPhase() != aigridv1.ModelPhase_MODEL_PHASE_READY || st.GetReason() != "Complete" {
		t.Fatalf("status = %v/%q, want READY/Complete", st.GetPhase(), st.GetReason())
	}
	for _, w := range st.GetRollout().GetWorks() {
		if w.Ready != nil {
			t.Fatalf("work %s ready = %v, want unset without feedback", w.GetCluster(), *w.Ready)
		}
	}
}

// TestDeriveModelPhaseCountsSelectedClustersOnly: with decisions known,
// works on clusters that left the decision do not count, a selected cluster
// without a work is not serving, and a cluster counts once.
func TestDeriveModelPhaseCountsSelectedClustersOnly(t *testing.T) {
	t.Parallel()
	rolling := mwrsWithConditions(
		cond("PlacementVerified", "True", "AsExpected", ""),
		cond("PlacementRolledOut", "False", "Progressing", ""),
		cond("ManifestworkApplied", "False", "Processing", ""))
	ready := func(c string) unstructured.Unstructured {
		return manifestWork(c, "True", condAppliedTrue, condAvailableTrue, condProgressingOff, condDegradedFalse)
	}
	failedApply := manifestWork("site-b", "", condAppliedFalse, condAvailableUnk)
	tests := []struct {
		name       string
		selected   []string
		works      []unstructured.Unstructured
		wantPhase  aigridv1.ModelPhase
		wantReason string
		wantMsg    string
	}{
		{
			name:     "same-size swap A to B: A's lingering READY work does not count",
			selected: []string{"site-b"}, works: []unstructured.Unstructured{ready("site-a")},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "Processing",
		},
		{
			name:     "a deselected FAILED work is ignored",
			selected: []string{"site-a"}, works: []unstructured.Unstructured{ready("site-a"), failedApply},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_READY,
		},
		{
			name:      "a cluster selected twice and with two works counts once",
			selected:  []string{"site-a", "site-a", "site-c"},
			works:     []unstructured.Unstructured{ready("site-a"), ready("site-a")},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE, wantReason: "PartiallyAvailable",
			wantMsg: "1 of 2 clusters are serving the model.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := placedModelOn(tt.selected, 0, rolling, tt.works...).GetStatus()
			if st.GetPhase() != tt.wantPhase || st.GetReason() != tt.wantReason || st.GetMessage() != tt.wantMsg {
				t.Fatalf("status = %v/%q/%q, want %v/%q/%q", st.GetPhase(), st.GetReason(), st.GetMessage(), tt.wantPhase, tt.wantReason, tt.wantMsg)
			}
		})
	}
}

// TestDeriveModelPhaseFromRolloutReplacesHubPhase: a placed model's phase
// comes from the rollout, not from the hub LLMInferenceService's Ready.
func TestDeriveModelPhaseFromRolloutReplacesHubPhase(t *testing.T) {
	t.Parallel()
	model := &aigridv1.Model{
		Spec: &aigridv1.ModelSpec{},
		Status: &aigridv1.ModelStatus{
			Phase: aigridv1.ModelPhase_MODEL_PHASE_READY, Reason: "HubReady", Message: "hub",
			Placement: &aigridv1.ModelPlacementStatus{NumberOfSelectedClusters: 1},
		},
	}
	enrichModelFromMWRS(model, mwrsWithConditions(cond("ManifestworkApplied", "False", "Processing", "")))
	if model.Status.GetPhase() != aigridv1.ModelPhase_MODEL_PHASE_PENDING || model.Status.GetReason() != "Processing" || model.Status.GetMessage() != "" {
		t.Fatalf("status = %v/%q/%q", model.Status.GetPhase(), model.Status.GetReason(), model.Status.GetMessage())
	}
}

func boolPtr(b bool) *bool { return &b }

// A hub-only model's phase from its Ready condition: KServe's terminal
// reasons are FAILED, and GatewaysNotReady (a Gateway that exists but isn't
// Ready yet) is PENDING, since a starting Gateway recovers on its own.
func TestDeriveModelPhaseHubReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason string
		want   aigridv1.ModelPhase
	}{
		{"ConfigNotFound", aigridv1.ModelPhase_MODEL_PHASE_FAILED},
		{"RefsInvalid", aigridv1.ModelPhase_MODEL_PHASE_FAILED},
		{"GatewaysNotReady", aigridv1.ModelPhase_MODEL_PHASE_PENDING},
		{"MinimumReplicasUnavailable", aigridv1.ModelPhase_MODEL_PHASE_PENDING},
	} {
		reason := tc.reason
		status := &aigridv1.ModelStatus{Conditions: []*aigridv1.Condition{{
			Type: "Ready", Status: aigridv1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: &reason,
		}}}
		deriveModelPhase(status)
		if status.GetPhase() != tc.want || status.GetReason() != tc.reason {
			t.Errorf("%s: phase %v reason %q, want %v", tc.reason, status.GetPhase(), status.GetReason(), tc.want)
		}
	}
}
