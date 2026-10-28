package servers

import (
	"context"
	"errors"
	"testing"

	"buf.build/go/protovalidate"
	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// presetModel is shaped like granite-4-tiny on the dev hub (hf299, written
// by git mode to ai-grid-cloud/demo-cluster): spec.main sets resources but no
// image, and the preset named in base_refs provides the image.
func presetModel(id string, baseRefs ...string) *aigridv1.Model {
	model := gitSampleModel(id, "site-a")
	model.Spec.BaseRefs = baseRefs
	model.Spec.Main = &aigridv1.ModelContainer{
		Requests: map[string]string{"cpu": "2", "memory": "8Gi", "nvidia.com/gpu": "1"},
		Limits:   map[string]string{"cpu": "3", "memory": "12Gi", "nvidia.com/gpu": "1"},
	}
	return model
}

// TestModelsGitImageFromPreset checks spec.main.image on Create and Update:
// KServe merges the LLMInferenceServiceConfigs in spec.baseRefs under the
// service's own spec, and an empty image in the service doesn't override
// the config's (mergeSpecs in kserve pkg/controller/v1alpha2/llmisvc/
// config_merge.go), so a model with base_refs may leave the image empty.
// Without base_refs nothing provides one, and it is still InvalidArgument.
// The rule on ModelSpec (buf.validate) must give the same answers.
func TestModelsGitImageFromPreset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	if err := protovalidate.Validate(presetModel("p", "ai-grid-gpu").GetSpec()); err != nil {
		t.Fatalf("protovalidate(base_refs, no image) error = %v, want none", err)
	}
	var verr *protovalidate.ValidationError
	if err := protovalidate.Validate(presetModel("p").GetSpec()); !errors.As(err, &verr) {
		t.Fatalf("protovalidate(no base_refs, no image) error = %v, want a ValidationError", err)
	}

	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: presetModel("granite", "ai-grid-gpu")})
	if err != nil {
		t.Fatalf("Create(base_refs, no image) error = %v", err)
	}
	if main := created.GetObject().GetSpec().GetMain(); main.GetImage() != "" || main.GetRequests()["nvidia.com/gpu"] != "1" {
		t.Fatalf("Create(base_refs, no image) spec.main = %v", main)
	}

	// An update that doesn't touch spec.main, as the console's label and
	// display-name edits do, and one that changes the resources.
	labelled := presetModel("granite", "ai-grid-gpu")
	labelled.Metadata.Labels = map[string]string{"team": "a"}
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: labelled, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}}}); err != nil {
		t.Fatalf("Update(metadata.labels) error = %v", err)
	}
	resized := presetModel("granite", "ai-grid-gpu")
	resized.Spec.Main.Requests["cpu"] = "3"
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: resized, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.main"}}}); err != nil {
		t.Fatalf("Update(spec.main, no image) error = %v", err)
	}

	commits := len(writer.Messages())
	_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: presetModel("bare")})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	// Removing the presets leaves nothing to provide the image.
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: presetModel("granite"), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.base_refs"}}})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if n := len(writer.Messages()); n != commits {
		t.Fatalf("refused calls committed %d times", n-commits)
	}
}
