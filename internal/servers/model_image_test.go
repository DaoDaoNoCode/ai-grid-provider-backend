package servers

import (
	"context"
	"errors"
	"path"
	"testing"

	"buf.build/go/protovalidate"
	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// TestModelsGitImageFromPreset checks spec.main.image on Create and Update.
// KServe applies its default preset (the llm-template, vLLM for NVIDIA GPUs)
// first, then the LLMInferenceServiceConfigs in spec.baseRefs, then the
// service's own spec (combineBaseRefsConfig in kserve
// pkg/controller/v1alpha2/llmisvc/config_merge.go), and an empty image in the
// service doesn't override theirs. So a model may leave the image empty, with
// base_refs or without them: the default preset's image applies. ModelSpec has
// no buf.validate rule on the image, so protovalidate accepts both, and the
// server alone refuses a blank image or one with a space or tab.
func TestModelsGitImageFromPreset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	for name, model := range map[string]*aigridv1.Model{
		"base_refs, no image":    presetModel("p", "ai-grid-gpu"),
		"no base_refs, no image": presetModel("p"),
	} {
		if err := protovalidate.Validate(model.GetSpec()); err != nil {
			t.Fatalf("protovalidate(%s) error = %v, want none", name, err)
		}
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
	// Removing the presets leaves the default preset's image.
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: presetModel("granite"), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.base_refs"}}}); err != nil {
		t.Fatalf("Update(spec.base_refs to none, no image) error = %v", err)
	}

	// No presets and no image: the committed main container names no image, so
	// the default preset's applies on the cluster.
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: presetModel("bare")}); err != nil {
		t.Fatalf("Create(no base_refs, no image) error = %v", err)
	}
	docs := readGitYAMLDocs(t, writer, path.Join(modelDeploymentGitDir("bare"), "deployment.yaml"))
	if refs, found, _ := unstructured.NestedSlice(docs[0].Object, "spec", "baseRefs"); found && len(refs) > 0 {
		t.Fatalf("committed baseRefs = %v, want none", refs)
	}
	containers, _, err := unstructured.NestedSlice(docs[0].Object, "spec", "template", "containers")
	if err != nil || len(containers) == 0 {
		t.Fatalf("committed containers = %v, %v", containers, err)
	}
	if image, found := containers[0].(map[string]interface{})["image"]; found {
		t.Fatalf("committed main image = %v, want none", image)
	}

	// A blank image, or one with a space or tab, is still refused.
	commits := len(writer.Messages())
	for name, image := range map[string]string{"blank": "   ", "inner space": "quay.io/a b", "tab": "quay.io/a\tb"} {
		model := presetModel("bad-image")
		model.Spec.Main.Image = image
		_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: Create(image %q) error = %v, want InvalidArgument", name, image, err)
		}
	}
	if n := len(writer.Messages()); n != commits {
		t.Fatalf("refused calls committed %d times", n-commits)
	}
}

// TestModelsGitEmptyBaseRef checks that a blank preset name is refused: it
// names no LLMInferenceServiceConfig, and model_mapper.go drops an empty
// baseRefs name, so it would read back differently than it was written. The
// base_refs rule (buf.validate) must refuse the same names.
func TestModelsGitEmptyBaseRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)
	for name, refs := range map[string][]string{
		"empty":          {""},
		"blank":          {" "},
		"tab":            {"\t"},
		"inner space":    {"ai-grid gpu"},
		"one of two":     {"ai-grid-gpu", ""},
		"padded":         {" ai-grid-gpu"},
		"newline inside": {"ai-grid-gpu\n"},
	} {
		model := presetModel("blank-ref", refs...)
		var verr *protovalidate.ValidationError
		if err := protovalidate.Validate(model.GetSpec()); !errors.As(err, &verr) {
			t.Errorf("%s: protovalidate(base_refs %q) error = %v, want a ValidationError", name, refs, err)
		}
		_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: Create(base_refs %q) error = %v, want InvalidArgument", name, refs, err)
		}
	}
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: presetModel("granite", "ai-grid-gpu")}); err != nil {
		t.Fatalf("Create(base_refs [ai-grid-gpu]) error = %v", err)
	}
	_, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: presetModel("granite", ""), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.base_refs"}}})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if n := len(writer.Messages()); n != 1 {
		t.Fatalf("commits = %d, want 1 (only the valid create)", n)
	}
}
