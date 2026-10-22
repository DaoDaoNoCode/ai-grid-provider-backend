package servers

import (
	"strings"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func conditionsFromSlice(raw []interface{}) []*aigridv1.Condition {
	out := make([]*aigridv1.Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		c := &aigridv1.Condition{
			Type:               stringField(m, "type"),
			Status:             conditionStatusFromString(stringField(m, "status")),
			ObservedGeneration: nestedInt64(m, "observedGeneration"),
		}
		if reason := stringField(m, "reason"); reason != "" {
			c.Reason = &reason
		}
		if message := stringField(m, "message"); message != "" {
			c.Message = &message
		}
		if ts := stringField(m, "lastTransitionTime"); ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				c.LastTransitionTime = timestamppb.New(t)
			} else if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				c.LastTransitionTime = timestamppb.New(t)
			}
		}
		out = append(out, c)
	}
	return out
}

func conditionStatusFromString(s string) aigridv1.ConditionStatus {
	switch strings.ToLower(s) {
	case "true":
		return aigridv1.ConditionStatus_CONDITION_STATUS_TRUE
	case "false":
		return aigridv1.ConditionStatus_CONDITION_STATUS_FALSE
	default:
		return aigridv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}

func nestedInt64(obj map[string]interface{}, fields ...string) int64 {
	v, found, err := unstructured.NestedInt64(obj, fields...)
	if err != nil || !found {
		f, found, err := unstructured.NestedFloat64(obj, fields...)
		if err != nil || !found {
			return 0
		}
		return int64(f)
	}
	return v
}

func nestedBool(obj map[string]interface{}, fields ...string) bool {
	v, found, err := unstructured.NestedBool(obj, fields...)
	if err != nil || !found {
		return false
	}
	return v
}

func stringField(obj map[string]interface{}, key string) string {
	v, _, _ := unstructured.NestedString(obj, key)
	return v
}

func modelRefKey(name, namespace string) string {
	if namespace == "" {
		namespace = "llm"
	}
	return namespace + "/" + name
}
