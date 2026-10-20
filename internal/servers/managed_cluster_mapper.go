package servers

import (
	"strconv"
	"strings"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func managedClusterFromUnstructured(item unstructured.Unstructured) *aigridv1.ManagedCluster {
	name := item.GetName()
	accepts, _, _ := unstructured.NestedBool(item.Object, "spec", "hubAcceptsClient")

	meta := &aigridv1.Metadata{
		Name:        name,
		Labels:      item.GetLabels(),
		Annotations: item.GetAnnotations(),
		Version:     resourceVersionToVersion(item.GetResourceVersion()),
	}
	if t := item.GetCreationTimestamp(); !t.IsZero() {
		meta.CreationTimestamp = timestamppb.New(t.Time)
	}
	if ts := item.GetDeletionTimestamp(); ts != nil && !ts.IsZero() {
		meta.DeletionTimestamp = timestamppb.New(ts.Time)
	}

	return &aigridv1.ManagedCluster{
		Id:       name,
		Metadata: meta,
		Spec: &aigridv1.ManagedClusterSpec{
			HubAcceptsClient: accepts,
		},
		Status: managedClusterStatusFromUnstructured(item),
	}
}

func managedClusterStatusFromUnstructured(item unstructured.Unstructured) *aigridv1.ManagedClusterStatus {
	status := &aigridv1.ManagedClusterStatus{}
	conditions, found, err := unstructured.NestedSlice(item.Object, "status", "conditions")
	if err != nil || !found {
		return status
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		condStatus, _ := cond["status"].(string)
		switch condType {
		case "ManagedClusterConditionAvailable", "Available":
			// Keep the first Available condition; later duplicates do not override.
			if status.AvailableCondition == "" {
				status.AvailableCondition = condStatus
			}
			if strings.EqualFold(condStatus, "True") {
				status.Available = true
			}
		// The hub writes "ManagedClusterJoined": OCM's constant is named
		// ManagedClusterConditionJoined but its value is "ManagedClusterJoined"
		// (open-cluster-management-io/api cluster/v1/types.go).
		case "ManagedClusterJoined", "Joined":
			if strings.EqualFold(condStatus, "True") {
				status.Joined = true
			}
		}
	}
	return status
}

func managedClusterID(object *aigridv1.ManagedCluster) string {
	if object == nil {
		return ""
	}
	if id := object.GetId(); id != "" {
		return id
	}
	if object.GetMetadata() != nil {
		return object.GetMetadata().GetName()
	}
	return ""
}

func stringMapToInterfaceMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func applyListWindow[T any](items []T, offset, limit *int32) (page []T, total int32) {
	total = int32(len(items))
	start := 0
	if offset != nil && *offset > 0 {
		start = int(*offset)
		if start > len(items) {
			start = len(items)
		}
	}
	end := len(items)
	if limit != nil && *limit >= 0 {
		end = start + int(*limit)
		if end > len(items) {
			end = len(items)
		}
		if *limit == 0 {
			end = start
		}
	}
	return items[start:end], total
}

// resourceVersionToVersion maps the hub's resourceVersion to metadata.version.
// The API server's resourceVersions are decimal etcd revisions; one that does
// not fit an int32 maps to 0, which turns the optimistic-lock check off.
func resourceVersionToVersion(rv string) int32 {
	v, err := strconv.ParseInt(rv, 10, 32)
	if err != nil || v < 0 {
		return 0
	}
	return int32(v)
}
