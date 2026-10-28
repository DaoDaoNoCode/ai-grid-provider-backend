package servers

import (
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

// labelAIGridEnabled marks a cluster as an AI Grid member. The hub's AI Grid
// Placements select clusters with matchLabels ai-grid/enabled: "true"
// (ai-grid-env-setup apps/ai-grid-policies/manifests/templates/placements.yaml),
// so only that exact value makes a member.
const labelAIGridEnabled = "ai-grid/enabled"

// Condition types and reasons the state is derived from, as the hub writes
// them (the same strings internal/fakeinfra/hub/acm.go cites upstream for).
const (
	conditionHubAccepted     = "HubAcceptedManagedCluster"
	conditionJoined          = "ManagedClusterJoined"
	conditionAvailable       = "ManagedClusterConditionAvailable"
	conditionImportSucceeded = "ManagedClusterImportSucceeded"
	reasonImporting          = "ManagedClusterImporting"
	reasonImportFailed       = "ManagedClusterImportFailed"
)

// isAIGridMember reports whether a cluster's labels make it an AI Grid member.
func isAIGridMember(labels map[string]string) bool {
	return labels[labelAIGridEnabled] == "true"
}

// managedClusterState derives a cluster's lifecycle state and the message of
// the condition that decided it, in the order the ACM console uses
// (stolostron/console frontend/src/resources/utils/get-cluster.ts
// L1467-1551, where "not True" means False or absent,
// status-conditions.ts L5-9). It is the derivation the provider console's
// fake has answered with (internal/fakeinfra/draft/state.go, deriveState,
// ported from the console's bff/internal/fakebackend/state.go), so the
// console shows the same states against the real backend. Its choices are
// kept:
//   - the message is filled for every state from the deciding condition;
//     the ACM console shows one only for importing, importfailed, offline
//     and unknown;
//   - a cluster without any condition is UNSPECIFIED;
//   - the ACM console's needsapproval (a pending CSR) and hosted-cluster
//     branches aren't derived, as the backend reads neither CSRs nor
//     HostedClusters, so such a cluster is PENDING_IMPORT.
//
// Conditions are matched by type, first match wins, and by the exact
// status "True" or "False". spec.hubAcceptsClient is not read: the hub's
// registration controller turns it into the HubAcceptedManagedCluster
// condition, which is what the ACM console reads.
func managedClusterState(deleting bool, conditions []*aigridv1.ManagedClusterCondition) (aigridv1.ManagedClusterState, string) {
	find := func(condType string) *aigridv1.ManagedClusterCondition {
		for _, c := range conditions {
			if c.GetType() == condType {
				return c
			}
		}
		return nil
	}
	isTrue := func(condType string) bool { return find(condType).GetStatus() == "True" }

	if deleting {
		return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_DETACHING, find(conditionImportSucceeded).GetMessage()
	}
	if len(conditions) == 0 {
		return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNSPECIFIED, ""
	}
	if !isTrue(conditionHubAccepted) {
		return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_NOT_ACCEPTED, find(conditionHubAccepted).GetMessage()
	}
	if !isTrue(conditionJoined) {
		imported := find(conditionImportSucceeded)
		if imported.GetStatus() == "False" {
			switch imported.GetReason() {
			case reasonImporting:
				return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORTING, imported.GetMessage()
			case reasonImportFailed:
				return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORT_FAILED, imported.GetMessage()
			}
		}
		return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_PENDING_IMPORT, imported.GetMessage()
	}
	available := find(conditionAvailable)
	switch available.GetStatus() {
	case "True":
		return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_READY, available.GetMessage()
	case "False":
		return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_OFFLINE, available.GetMessage()
	default:
		return aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNKNOWN, available.GetMessage()
	}
}
