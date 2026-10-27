package servers

import (
	"testing"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestEnrichModelFromMWRSNotAsExpectedIsPending(t *testing.T) {
	t.Parallel()
	model := &aigridv1.Model{Spec: &aigridv1.ModelSpec{}}
	mwrs := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "ManifestworkApplied",
					"status": "False",
					"reason": "NotAsExpected",
				},
			},
		},
	}}
	enrichModelFromMWRS(model, mwrs)
	if model.Status.GetPhase() != aigridv1.ModelPhase_MODEL_PHASE_PENDING {
		t.Fatalf("phase = %v, want PENDING", model.Status.GetPhase())
	}
	if model.Status.GetReason() != "NotAsExpected" {
		t.Fatalf("reason = %q", model.Status.GetReason())
	}
}

func TestEnrichModelFromMWRSDegradedWorkIsFailed(t *testing.T) {
	t.Parallel()
	model := &aigridv1.Model{Spec: &aigridv1.ModelSpec{}}
	mwrs := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "ManifestworkApplied",
					"status": "False",
					"reason": "NotAsExpected",
				},
			},
		},
	}}
	enrichModelFromMWRS(model, mwrs)
	model.Status.Rollout.Works = []*aigridv1.ModelWorkStatus{{
		Cluster:  "site-a",
		Degraded: "True",
	}}
	if !mwrsHasFailedOrDegradedWork(model.Status.Rollout) {
		t.Fatal("expected degraded work to count as failure")
	}
	// Re-run phase selection with works present: simulate post-enrichment check.
	rs := model.Status.Rollout
	if rs.RolledOut && rs.Applied {
		t.Fatal("unexpected ready")
	}
	if mwrsHasFailedOrDegradedWork(rs) {
		model.Status.Phase = aigridv1.ModelPhase_MODEL_PHASE_FAILED
	}
	if model.Status.GetPhase() != aigridv1.ModelPhase_MODEL_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED", model.Status.GetPhase())
	}
}

func TestMwrsHasFailedOrDegradedWorkAppliedFalse(t *testing.T) {
	t.Parallel()
	rs := &aigridv1.ModelRolloutStatus{
		Works: []*aigridv1.ModelWorkStatus{{
			Cluster: "site-a",
			Applied: "False",
		}},
	}
	if !mwrsHasFailedOrDegradedWork(rs) {
		t.Fatal("Applied=False should count as failure")
	}
	model := &aigridv1.Model{
		Status: &aigridv1.ModelStatus{Rollout: rs},
	}
	deriveModelPhaseFromRollout(model)
	if model.Status.GetPhase() != aigridv1.ModelPhase_MODEL_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED via derive path", model.Status.GetPhase())
	}
}
