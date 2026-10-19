package servers

import (
	"testing"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
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
			name: "OCM long condition names True",
			object: managedClusterUnstructured("cluster-c", nil, nil, true, []map[string]interface{}{
				{"type": "ManagedClusterConditionAvailable", "status": "True"},
				{"type": "ManagedClusterConditionJoined", "status": "True"},
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
