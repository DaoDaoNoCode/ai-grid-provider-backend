package servers

import (
	"maps"
	"testing"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestManagedClusterFromUnstructured(t *testing.T) {
	t.Parallel()

	created := metav1.NewTime(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))

	tests := []struct {
		name          string
		object        *unstructured.Unstructured
		wantAvailable bool
		wantJoined    bool
		wantHub       bool
		wantLabel     string
		wantAnno      string
	}{
		{
			name:      "no conditions",
			object:    managedClusterUnstructured("cluster-a", map[string]string{"environment": "dev"}, map[string]string{"note": "x"}, true, nil),
			wantHub:   true,
			wantLabel: "dev",
			wantAnno:  "x",
		},
		{
			name: "short Available and Joined True",
			object: managedClusterUnstructured("cluster-b", nil, nil, false, []map[string]interface{}{
				{"type": "Available", "status": "True"},
				{"type": "Joined", "status": "True"},
			}),
			wantAvailable: true,
			wantJoined:    true,
		},
		{
			name: "condition types as the hub writes them, True",
			object: managedClusterUnstructured("cluster-c", nil, nil, true, []map[string]interface{}{
				{"type": "ManagedClusterConditionAvailable", "status": "True"},
				{"type": "ManagedClusterJoined", "status": "True"},
			}),
			wantAvailable: true,
			wantJoined:    true,
			wantHub:       true,
		},
		{
			name: "False recorded; later True still sets available",
			object: managedClusterUnstructured("cluster-d", nil, nil, false, []map[string]interface{}{
				{"type": "Available", "status": "False"},
				{"type": "Joined", "status": "Unknown"},
				{"type": "Available", "status": "true"}, // EqualFold True
			}),
			wantAvailable: true,
			wantJoined:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.object.SetCreationTimestamp(created)

			got := managedClusterFromUnstructured(*tc.object)
			if got.GetId() != tc.object.GetName() {
				t.Fatalf("id = %q, want %q", got.GetId(), tc.object.GetName())
			}
			if got.GetMetadata().GetName() != tc.object.GetName() {
				t.Fatalf("metadata.name = %q", got.GetMetadata().GetName())
			}
			if got.GetSpec().GetHubAcceptsClient() != tc.wantHub {
				t.Fatalf("hubAcceptsClient = %v, want %v", got.GetSpec().GetHubAcceptsClient(), tc.wantHub)
			}
			if got.GetStatus().GetAvailable() != tc.wantAvailable {
				t.Fatalf("available = %v, want %v", got.GetStatus().GetAvailable(), tc.wantAvailable)
			}
			if got.GetStatus().GetJoined() != tc.wantJoined {
				t.Fatalf("joined = %v, want %v", got.GetStatus().GetJoined(), tc.wantJoined)
			}
			if tc.name == "False recorded; later True still sets available" {
				if got.GetStatus().GetAvailableCondition() != "False" {
					t.Fatalf("available_condition = %q, want False (first Available)", got.GetStatus().GetAvailableCondition())
				}
			}
			if tc.wantAvailable && tc.name == "OCM long condition names True" {
				if got.GetStatus().GetAvailableCondition() != "True" {
					t.Fatalf("available_condition = %q, want True", got.GetStatus().GetAvailableCondition())
				}
			}
			if tc.wantLabel != "" && got.GetMetadata().GetLabels()["environment"] != tc.wantLabel {
				t.Fatalf("labels = %v", got.GetMetadata().GetLabels())
			}
			if tc.wantAnno != "" && got.GetMetadata().GetAnnotations()["note"] != tc.wantAnno {
				t.Fatalf("annotations = %v", got.GetMetadata().GetAnnotations())
			}
			if got.GetMetadata().GetCreationTimestamp() == nil {
				t.Fatal("creation_timestamp is nil")
			}
		})
	}
}

func TestManagedClusterID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		object *aigridv1.ManagedCluster
		want   string
	}{
		{name: "nil", object: nil, want: ""},
		{name: "empty", object: &aigridv1.ManagedCluster{}, want: ""},
		{name: "id wins", object: &aigridv1.ManagedCluster{Id: "from-id", Metadata: &aigridv1.Metadata{Name: "from-name"}}, want: "from-id"},
		{name: "fallback metadata.name", object: &aigridv1.ManagedCluster{Metadata: &aigridv1.Metadata{Name: "from-name"}}, want: "from-name"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := managedClusterID(tc.object); got != tc.want {
				t.Fatalf("managedClusterID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplaceStringMapPatch(t *testing.T) {
	t.Parallel()
	current := map[string]string{"remove-me": "x", "keep": "y", "owned": "old"}
	next := map[string]string{"keep": "y2", "new": "z"}
	preserve := map[string]string{"owned": "forced"}
	got := replaceStringMapPatch(current, next, preserve)
	if got["remove-me"] != nil {
		t.Fatalf("remove-me = %v, want null", got["remove-me"])
	}
	if got["keep"] != "y2" || got["new"] != "z" || got["owned"] != "forced" {
		t.Fatalf("got = %#v", got)
	}
	empty := replaceStringMapPatch(map[string]string{"remove-me": "x"}, map[string]string{}, nil)
	if empty["remove-me"] != nil || len(empty) != 1 {
		t.Fatalf("empty replace = %#v", empty)
	}
}

func TestApplyListWindow(t *testing.T) {
	t.Parallel()

	items := []string{"a", "b", "c", "d"}
	offset1 := int32(1)
	limit2 := int32(2)
	limit0 := int32(0)
	offsetPast := int32(10)

	page, total := applyListWindow(items, nil, nil)
	if total != 4 || len(page) != 4 {
		t.Fatalf("nil window page=%v total=%d", page, total)
	}

	page, total = applyListWindow(items, &offset1, &limit2)
	if total != 4 || len(page) != 2 || page[0] != "b" || page[1] != "c" {
		t.Fatalf("offset/limit page=%v total=%d", page, total)
	}

	page, total = applyListWindow(items, &offset1, &limit0)
	if total != 4 || len(page) != 0 {
		t.Fatalf("limit 0 page=%v total=%d", page, total)
	}

	page, total = applyListWindow(items, &offsetPast, &limit2)
	if total != 4 || len(page) != 0 {
		t.Fatalf("past end page=%v total=%d", page, total)
	}
}

func managedClusterUnstructured(
	name string,
	labels, annotations map[string]string,
	accepts bool,
	conditions []map[string]interface{},
) *unstructured.Unstructured {
	jsonLabels := make(map[string]interface{}, len(labels))
	for k, v := range labels {
		jsonLabels[k] = v
	}
	jsonAnnotations := make(map[string]interface{}, len(annotations))
	for k, v := range annotations {
		jsonAnnotations[k] = v
	}
	obj := map[string]interface{}{
		"apiVersion": "cluster.open-cluster-management.io/v1",
		"kind":       "ManagedCluster",
		"metadata": map[string]interface{}{
			"name":        name,
			"labels":      jsonLabels,
			"annotations": jsonAnnotations,
		},
		"spec": map[string]interface{}{
			"hubAcceptsClient": accepts,
		},
	}
	if conditions != nil {
		raw := make([]interface{}, 0, len(conditions))
		for _, cond := range conditions {
			raw = append(raw, cond)
		}
		obj["status"] = map[string]interface{}{
			"conditions": raw,
		}
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestProvisioningFromPolicy(t *testing.T) {
	policy := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"status": []interface{}{
			map[string]interface{}{"clustername": "a", "compliant": "Compliant"},
			map[string]interface{}{"clustername": "b", "compliant": "NonCompliant"},
			map[string]interface{}{"clustername": "c"},
			map[string]interface{}{"clustername": "d", "compliant": "Pending"},
			map[string]interface{}{"compliant": "Compliant"},
		}},
	}}
	got := provisioningFromPolicy(policy)
	want := map[string]string{"a": "Compliant", "b": "NonCompliant", "c": "Pending", "d": "Pending"}
	if !maps.Equal(got, want) {
		t.Fatalf("provisioningFromPolicy() = %v, want %v", got, want)
	}
	if got := provisioningFromPolicy(unstructured.Unstructured{Object: map[string]interface{}{}}); len(got) != 0 {
		t.Fatalf("no status: %v", got)
	}
}

func TestManagedClusterStatusDetails(t *testing.T) {
	t.Parallel()

	// A ManagedCluster as the hub reports it: conditions, version, capacity,
	// allocatable and claims, with values from a real hub (names fictional).
	object := managedClusterUnstructured("site-a", nil, nil, true, nil)
	object.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type": "ManagedClusterConditionAvailable", "status": "True", "reason": "ManagedClusterAvailable",
				"message": "Managed cluster is available", "lastTransitionTime": "2026-10-21T04:01:02Z",
			},
			map[string]interface{}{"type": "ManagedClusterJoined", "status": "True"},
			map[string]interface{}{"type": "HubAcceptedManagedCluster", "status": "True", "lastTransitionTime": "not a time"},
		},
		"version":     map[string]interface{}{"kubernetes": "v1.34.9"},
		"capacity":    map[string]interface{}{"cpu": "68", "memory": "273509824Ki", "nvidia.com/gpu": "1"},
		"allocatable": map[string]interface{}{"cpu": "65", "memory": "264506816Ki", "nvidia.com/gpu": "1"},
		"clusterClaims": []interface{}{
			map[string]interface{}{"name": "consoleurl.cluster.open-cluster-management.io", "value": "https://console.apps.site-a.example.com"},
			map[string]interface{}{"name": "platform.open-cluster-management.io", "value": "AWS"},
			map[string]interface{}{"value": "a claim without a name is dropped"},
		},
	}

	want := &aigridv1.ManagedClusterStatus{
		Available:          true,
		Joined:             true,
		AvailableCondition: "True",
		Conditions: []*aigridv1.ManagedClusterCondition{
			{
				Type: "ManagedClusterConditionAvailable", Status: "True", Reason: "ManagedClusterAvailable",
				Message:            "Managed cluster is available",
				LastTransitionTime: timestamppb.New(time.Date(2026, 10, 21, 4, 1, 2, 0, time.UTC)),
			},
			{Type: "ManagedClusterJoined", Status: "True"},
			// An unparseable time is left out rather than failing the cluster.
			{Type: "HubAcceptedManagedCluster", Status: "True"},
		},
		KubernetesVersion: "v1.34.9",
		Capacity:          map[string]string{"cpu": "68", "memory": "273509824Ki", "nvidia.com/gpu": "1"},
		Allocatable:       map[string]string{"cpu": "65", "memory": "264506816Ki", "nvidia.com/gpu": "1"},
		ClusterClaims: []*aigridv1.ManagedClusterClaim{
			{Name: "consoleurl.cluster.open-cluster-management.io", Value: "https://console.apps.site-a.example.com"},
			{Name: "platform.open-cluster-management.io", Value: "AWS"},
		},
	}
	if got := managedClusterFromUnstructured(*object).GetStatus(); !proto.Equal(got, want) {
		t.Fatalf("status = %v\nwant %v", got, want)
	}

	// Malformed entries are skipped; the valid ones beside them stay.
	object.Object["status"] = map[string]interface{}{
		"conditions":    []interface{}{"not a map", map[string]interface{}{"type": "ManagedClusterJoined", "status": "True"}},
		"capacity":      map[string]interface{}{"cpu": int64(68), "memory": "1Gi"},
		"clusterClaims": []interface{}{int64(42), map[string]interface{}{"name": "product.open-cluster-management.io", "value": "ROSA"}},
	}
	malformed := managedClusterFromUnstructured(*object).GetStatus()
	if len(malformed.GetConditions()) != 1 || !malformed.GetJoined() {
		t.Fatalf("conditions = %v, joined = %v; want only the Joined one", malformed.GetConditions(), malformed.GetJoined())
	}
	if !maps.Equal(malformed.GetCapacity(), map[string]string{"memory": "1Gi"}) {
		t.Fatalf("capacity = %v, want only the string quantity", malformed.GetCapacity())
	}
	if len(malformed.GetClusterClaims()) != 1 || malformed.GetClusterClaims()[0].GetValue() != "ROSA" {
		t.Fatalf("claims = %v, want only the product claim", malformed.GetClusterClaims())
	}

	// A cluster whose agent hasn't reported yet has none of them.
	empty := managedClusterFromUnstructured(*managedClusterUnstructured("site-new", nil, nil, true, nil)).GetStatus()
	if empty.GetKubernetesVersion() != "" || empty.GetCapacity() != nil || empty.GetAllocatable() != nil ||
		empty.GetClusterClaims() != nil || empty.GetConditions() != nil {
		t.Fatalf("status of a new cluster = %v, want none of the details", empty)
	}
}
