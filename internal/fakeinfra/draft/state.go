package draft

import (
	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// deriveState computes the summary state and the message of the condition
// that decided it, in the order the ACM console uses for a ManagedCluster
// (console:frontend/src/resources/utils/get-cluster.ts L1467-1551). "Not
// True" means False or absent (status-conditions.ts L5-9).
//
// Two parts are the console's own choice, not the ACM console's:
//   - state_message is filled for every state from the deciding condition.
//     The ACM console sets a message only for importing and importfailed
//     (L1494-1499) and for offline and unknown (L1538, L1550); for
//     detaching, notaccepted, pendingimport and ready it shows none.
//   - The ACM console's needsapproval (a pending CSR, L1503-1514) and the
//     hosted-cluster branch (L1515-1540) are not derived: the backend reads
//     no CSRs or HostedClusters, so such a cluster reads PENDING_IMPORT.
//
// The real backend is asked to do the same server-side so that the console
// and the ACM console never disagree about a cluster's state.
func deriveState(c *aigriddraftv1.ManagedCluster) (aigriddraftv1.ManagedClusterState, string) {
	msg := func(typ string) string { return findCondition(c, typ).GetMessage() }

	if c.GetDeletionTimestamp() != nil {
		return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_DETACHING, msg(CondImportSucceeded)
	}
	if len(c.GetStatus().GetConditions()) == 0 {
		return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNSPECIFIED, ""
	}
	if !isTrue(c, CondHubAccepted) {
		return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_NOT_ACCEPTED, msg(CondHubAccepted)
	}
	if !isTrue(c, CondJoined) {
		imp := findCondition(c, CondImportSucceeded)
		if imp.GetStatus() == aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE {
			switch imp.GetReason() {
			case ReasonImporting:
				return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORTING, imp.GetMessage()
			case ReasonImportFailed:
				return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORT_FAILED, imp.GetMessage()
			}
		}
		return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_PENDING_IMPORT, imp.GetMessage()
	}
	avail := findCondition(c, CondAvailable)
	switch avail.GetStatus() {
	case aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE:
		return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE, avail.GetMessage()
	case aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE:
		return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNAVAILABLE, avail.GetMessage()
	default:
		return aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNKNOWN, avail.GetMessage()
	}
}

// withDerivedState fills `status.state` and `status.state_message` on a copy
// of the stored object. Fixtures may carry values for them, but they are
// never trusted: the stored object is the source and the summary is computed
// on every read, exactly as the real backend would.
func withDerivedState(c *aigriddraftv1.ManagedCluster) *aigriddraftv1.ManagedCluster {
	out := cloneCluster(c)
	if out.Status == nil {
		out.Status = &aigriddraftv1.ManagedClusterStatus{}
	}
	out.Status.State, out.Status.StateMessage = deriveState(out)
	return out
}
