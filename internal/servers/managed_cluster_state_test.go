package servers

import (
	"testing"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Conditions as the hub writes them (the strings and their upstream sources
// are in internal/fakeinfra/hub/acm.go).
var (
	condImported   = map[string]interface{}{"type": "ManagedClusterImportSucceeded", "status": "True", "reason": "ManagedClusterImported", "message": "Import succeeded"}
	condAccepted   = map[string]interface{}{"type": "HubAcceptedManagedCluster", "status": "True", "reason": "HubClusterAdminAccepted", "message": "Accepted by hub cluster admin"}
	condJoined     = map[string]interface{}{"type": "ManagedClusterJoined", "status": "True", "reason": "ManagedClusterJoined", "message": "Managed cluster joined"}
	condAvailable  = map[string]interface{}{"type": "ManagedClusterConditionAvailable", "status": "True", "reason": "ManagedClusterAvailable", "message": "Managed cluster is available"}
	condClock      = map[string]interface{}{"type": "ManagedClusterConditionClockSynced", "status": "True", "reason": "ManagedClusterClockSynced", "message": "The clock of the managed cluster is synced with the hub."}
	condDenied     = map[string]interface{}{"type": "HubAcceptedManagedCluster", "status": "False", "reason": "HubClusterAdminDenied", "message": "Denied by hub cluster admin"}
	condDetaching  = map[string]interface{}{"type": "ManagedClusterImportSucceeded", "status": "False", "reason": "ManagedClusterDetaching", "message": "The managed cluster is being detached now"}
	condWaitImport = map[string]interface{}{"type": "ManagedClusterImportSucceeded", "status": "False", "reason": "ManagedClusterWaitForImporting", "message": "Wait for importing"}
	condImporting  = map[string]interface{}{"type": "ManagedClusterImportSucceeded", "status": "False", "reason": "ManagedClusterImporting", "message": "Importing resources are applied, wait for resources be available"}
	condImportFail = map[string]interface{}{"type": "ManagedClusterImportSucceeded", "status": "False", "reason": "ManagedClusterImportFailed", "message": "Try to import managed cluster, error: timeout"}
	condAPIDown    = map[string]interface{}{"type": "ManagedClusterConditionAvailable", "status": "False", "reason": "ManagedClusterKubeAPIServerUnavailable", "message": "The kube-apiserver is not ok, status code: 503, the server is currently unable to handle the request"}
	condLeaseLost  = map[string]interface{}{"type": "ManagedClusterConditionAvailable", "status": "Unknown", "reason": "ManagedClusterLeaseUpdateStopped", "message": "Registration agent stopped updating its lease."}
)

func conds(c ...map[string]interface{}) []map[string]interface{} {
	return append([]map[string]interface{}{}, c...)
}

// TestManagedClusterState runs the cases of the console fake's deriveState
// test (internal/fakeinfra/draft/clusters_test.go, TestDeriveState) through
// the real mapper, so both answer the same state for the same conditions.
func TestManagedClusterState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		deleting   bool
		accepts    bool
		conditions []map[string]interface{}
		want       aigridv1.ManagedClusterState
		message    string
	}{
		{"detaching wins over everything", true, true, conds(condDetaching, condAccepted, condJoined, condAvailable, condClock), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_DETACHING, "The managed cluster is being detached now"},
		{"detaching without conditions", true, true, nil, aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_DETACHING, ""},
		{"no conditions", false, true, nil, aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNSPECIFIED, ""},
		{"denied by hub admin", false, false, conds(condDenied, condJoined, condAvailable), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_NOT_ACCEPTED, "Denied by hub cluster admin"},
		{"accepted condition absent", false, true, conds(condJoined, condAvailable), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_NOT_ACCEPTED, ""},
		{"accepted, import not started", false, true, conds(condAccepted, condWaitImport), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_PENDING_IMPORT, "Wait for importing"},
		{"accepted, nothing else", false, true, conds(condAccepted), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_PENDING_IMPORT, ""},
		{"importing", false, true, conds(condAccepted, condImporting), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORTING, "Importing resources are applied, wait for resources be available"},
		{"import failed", false, true, conds(condAccepted, condImportFail), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORT_FAILED, "Try to import managed cluster, error: timeout"},
		{"ready", false, true, conds(condImported, condAccepted, condJoined, condAvailable, condClock), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_READY, "Managed cluster is available"},
		{"joined but import not finished is still ready", false, true, conds(condImporting, condAccepted, condJoined, condAvailable), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_READY, "Managed cluster is available"},
		{"offline", false, true, conds(condImported, condAccepted, condJoined, condAPIDown), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_OFFLINE, "The kube-apiserver is not ok, status code: 503, the server is currently unable to handle the request"},
		{"unknown, lease expired", false, true, conds(condImported, condAccepted, condJoined, condLeaseLost), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNKNOWN, "Registration agent stopped updating its lease."},
		{"unknown, available condition missing", false, true, conds(condImported, condAccepted, condJoined), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNKNOWN, ""},
		// The spec is not read: the condition decides, as in the ACM console.
		{"hubAcceptsClient false, still accepted by the hub", false, false, conds(condAccepted, condJoined, condAvailable), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_READY, "Managed cluster is available"},
		// The first condition of a type wins, like the ACM console's find.
		{"first Available wins", false, true, conds(condAccepted, condJoined, condAPIDown, condAvailable), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_OFFLINE, "The kube-apiserver is not ok, status code: 503, the server is currently unable to handle the request"},
		// Condition status is read case-insensitively, as for status.available.
		{"lowercase true is True", false, true, conds(map[string]interface{}{"type": "HubAcceptedManagedCluster", "status": "true"}), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_PENDING_IMPORT, ""},
		{"lowercase available", false, true, conds(condAccepted, condJoined, map[string]interface{}{"type": "ManagedClusterConditionAvailable", "status": "true"}), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_READY, ""},
		{"lowercase unavailable", false, true, conds(condAccepted, condJoined, map[string]interface{}{"type": "ManagedClusterConditionAvailable", "status": "false"}), aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_OFFLINE, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			object := managedClusterUnstructured("c", nil, nil, tc.accepts, tc.conditions)
			if tc.deleting {
				now := metav1.NewTime(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
				object.SetDeletionTimestamp(&now)
			}
			status := managedClusterFromUnstructured(*object).GetStatus()
			if status.GetState() != tc.want || status.GetStateMessage() != tc.message {
				t.Errorf("state = %s %q, want %s %q", status.GetState(), status.GetStateMessage(), tc.want, tc.message)
			}
			// available and state read the Available condition the same way.
			if ready := tc.want == aigridv1.ManagedClusterState_MANAGED_CLUSTER_STATE_READY; ready && !status.GetAvailable() {
				t.Errorf("state READY with available = false")
			}
		})
	}
}

func TestAIGridMember(t *testing.T) {
	t.Parallel()

	cases := []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"ai-grid/enabled": "true"}, true},
		{map[string]string{"ai-grid/enabled": "false"}, false},
		// The Placements match "true" exactly, so nothing else is a member.
		{map[string]string{"ai-grid/enabled": "True"}, false},
		{map[string]string{"ai-grid/enabled": ""}, false},
		{map[string]string{"ai-grid/role": "inference"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		object := managedClusterUnstructured("c", tc.labels, nil, true, nil)
		if got := managedClusterFromUnstructured(*object).GetStatus().GetAiGridMember(); got != tc.want {
			t.Errorf("labels %v: ai_grid_member = %v, want %v", tc.labels, got, tc.want)
		}
	}
}
