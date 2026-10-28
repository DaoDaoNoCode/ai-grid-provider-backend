package servers

import (
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestParseModelUpdateMask pins the paths ModelsUpdateRequest.update_mask
// documents (models_service.proto): the fields it names, with or without
// "object.", the subpaths that replace their whole field, and the rest,
// which are InvalidArgument.
func TestParseModelUpdateMask(t *testing.T) {
	t.Parallel()
	accepted := []string{
		"metadata.labels", "metadata.annotations", "spec.model", "spec.replicas",
		"spec.base_refs", "spec.runtime", "spec.router", "spec.main",
		"spec.main.image", "spec.main.env", "spec.main.requests", "spec.main.limits",
		"spec.scaling", "spec.placement", "spec.rollout",
		// Subpaths of the fields that take them.
		"metadata.labels.team", "metadata.annotations.openshift.io/display-name",
		"spec.model.uri", "spec.router.scheduler", "spec.main.env.0",
		"spec.main.requests.cpu", "spec.main.limits.memory", "spec.scaling.max_replicas",
		"spec.placement.label_selector", "spec.rollout.strategy",
	}
	for _, path := range accepted {
		for _, p := range []string{path, "object." + path} {
			if _, err := parseModelUpdateMask(&fieldmaskpb.FieldMask{Paths: []string{p}}); err != nil {
				t.Errorf("parseModelUpdateMask(%q) error = %v, want accepted", p, err)
			}
		}
	}
	for _, p := range []string{
		"id", "metadata.name", "metadata.version", "spec", "status", "status.phase",
		"spec.main.args", "spec.replicas.value", "spec.base_refs.0", "spec.runtime.name",
		"spec.main.image.tag",
	} {
		_, err := parseModelUpdateMask(&fieldmaskpb.FieldMask{Paths: []string{p}})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("parseModelUpdateMask(%q) error = %v, want InvalidArgument", p, err)
		}
	}
	for _, mask := range []*fieldmaskpb.FieldMask{nil, {}} {
		if _, err := parseModelUpdateMask(mask); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("parseModelUpdateMask(%v) error = %v, want InvalidArgument", mask, err)
		}
	}
}
