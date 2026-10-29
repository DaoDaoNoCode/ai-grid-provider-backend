package servers

import (
	"context"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// MaaS lists a model under its id and forwards the request body unchanged, so
// a tenant's "model" is the id all the way to vLLM. The backend serves the
// model as its id (the LLMInferenceService's spec.model.name, vLLM's served
// name) and points the ExternalModel's targetModel at the id. As in
// odh-dashboard, the served name is the resource name: spec.model.name is
// accepted empty or equal to the id and always reads back as the id. Two
// models may serve one Hugging Face model on one cluster: their ids differ.
func TestModelsGitServesModelByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	for id, sent := range map[string]string{"first": "", "second": "second"} {
		model := gitSampleModel(id, "site-b")
		model.Spec.Model.Name = sent
		created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model})
		if err != nil {
			t.Fatalf("Create(%s, spec.model.name %q) error = %v", id, sent, err)
		}
		if name := created.GetObject().GetSpec().GetModel().GetName(); name != id {
			t.Fatalf("%s: Create spec.model.name = %q, want the id", id, name)
		}
		llm := readGitYAMLDocs(t, writer, "catalog/models/"+id+"/deployment/deployment.yaml")[0]
		if got, _, _ := unstructured.NestedString(llm.Object, "spec", "model", "name"); got != id {
			t.Fatalf("%s: LLMInferenceService spec.model.name = %q, want the id (vLLM's served name)", id, got)
		}
		if _, ok := llm.GetAnnotations()["ai-grid/model-name"]; ok {
			t.Fatalf("%s: LLMInferenceService has annotation ai-grid/model-name; the backend no longer writes it", id)
		}
		if got := gitTargetModel(t, writer, id); got != id {
			t.Fatalf("%s: ExternalModel targetModel = %q, want the id", id, got)
		}
		got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: id})
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if name := got.GetObject().GetSpec().GetModel().GetName(); name != id {
			t.Fatalf("%s: Get spec.model.name = %q, want the id", id, name)
		}
	}
}

// A spec.model.name other than the id would be a name nothing serves, so
// Create and Update refuse it, saying the model is served as its id.
func TestModelsGitRejectsServedNameOtherThanID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	model := gitSampleModel("opt", "site-a")
	model.Spec.Model.Name = "facebook/opt-125m"
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if msg := err.Error(); !strings.Contains(msg, `"facebook/opt-125m"`) || !strings.Contains(msg, `"opt"`) {
		t.Fatalf("Create error = %v, want it to name the sent name and the id", err)
	}

	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("opt", "site-a")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for _, path := range []string{"spec.model", "spec.model.name"} {
		edit := gitSampleModel("opt", "site-a")
		edit.Spec.Model.Name = "renamed"
		edit.Metadata.Version = created.GetObject().GetMetadata().GetVersion()
		_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
			Object:     edit,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}},
			Lock:       true,
		})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	}
}

// A model written before the backend served models as their id serves its
// own spec.model.name, such as a Hugging Face name. Until it is rewritten, a
// read reports the name it actually serves, so the API never claims a name
// vLLM doesn't answer to. Its next update switches it to serve the id (vLLM
// restarts once), whatever that update changes, and a caller may send back
// the name it read. The first builds that served the id also kept the caller's
// name in annotation ai-grid/model-name; such an entry already serves the id,
// reads as the id, and its next update drops the annotation.
func TestModelsGitLegacyEntryReportsTheNameItServes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		annotate bool   // ai-grid/model-name, as demo1's carry build wrote it
		served   string // the deployment's spec.model.name, what vLLM serves
		mask     string // the update that switches it
		sent     string // spec.model.name that update sends, with mask spec.model
	}{
		"before ids, replica update":                {served: "facebook/opt-125m", mask: "spec.replicas"},
		"before ids, sends back the name it read":   {served: "facebook/opt-125m", mask: "spec.model", sent: "facebook/opt-125m"},
		"before ids, sends the id":                  {served: "facebook/opt-125m", mask: "spec.model", sent: "old"},
		"carry build with ai-grid/model-name, edit": {annotate: true, served: "old", mask: "spec.replicas"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			writer := gitops.NewMemoryWriter(nil)
			srv := mustGitModelsServer(t, writer)

			if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("old", "site-a")}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			// Rewrite the deployment the way an earlier backend wrote it.
			p := "catalog/models/old/deployment/deployment.yaml"
			docs := readGitYAMLDocs(t, writer, p)
			if tc.annotate {
				annotations := docs[0].GetAnnotations()
				annotations["ai-grid/model-name"] = "facebook/opt-125m"
				docs[0].SetAnnotations(annotations)
			}
			if err := unstructured.SetNestedField(docs[0].Object, tc.served, "spec", "model", "name"); err != nil {
				t.Fatal(err)
			}
			data, err := marshalYAMLDocs(docs...)
			if err != nil {
				t.Fatal(err)
			}
			head, err := writer.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Commit(ctx, head.Revision(), gitops.Change{Put: map[string][]byte{p: data}, Message: "old format"}); err != nil {
				t.Fatalf("Commit() error = %v", err)
			}

			got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "old"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if name := got.GetObject().GetSpec().GetModel().GetName(); name != tc.served {
				t.Fatalf("Get spec.model.name = %q, want %q, the name the deployment serves", name, tc.served)
			}
			list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if name := list.GetItems()[0].GetSpec().GetModel().GetName(); name != tc.served {
				t.Fatalf("List spec.model.name = %q, want %q", name, tc.served)
			}
			if _, ok := got.GetObject().GetMetadata().GetAnnotations()["ai-grid/model-name"]; ok {
				t.Fatal("Get returned annotation ai-grid/model-name")
			}

			// A third name is still refused, and nothing changes.
			bad := gitSampleModel("old", "site-a")
			bad.Spec.Model.Name = "renamed"
			bad.Metadata.Version = got.GetObject().GetMetadata().GetVersion()
			_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: bad, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.model"}}, Lock: true})
			assertConnectCode(t, err, connect.CodeInvalidArgument)

			replicas := int32(2)
			edit := gitSampleModel("old", "site-a")
			edit.Spec.Replicas = &replicas
			edit.Spec.Model.Name = tc.sent
			edit.Metadata.Version = got.GetObject().GetMetadata().GetVersion()
			updated, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
				Object:     edit,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{tc.mask}},
				Lock:       true,
			})
			if err != nil {
				t.Fatalf("Update(%s) error = %v", tc.mask, err)
			}
			if name := updated.GetObject().GetSpec().GetModel().GetName(); name != "old" {
				t.Fatalf("Update spec.model.name = %q, want the id", name)
			}
			llm := readGitYAMLDocs(t, writer, p)[0]
			if served, _, _ := unstructured.NestedString(llm.Object, "spec", "model", "name"); served != "old" {
				t.Fatalf("after update spec.model.name = %q, want the id", served)
			}
			if _, ok := llm.GetAnnotations()["ai-grid/model-name"]; ok {
				t.Fatal("after update the deployment still has annotation ai-grid/model-name")
			}
			if got := gitTargetModel(t, writer, "old"); got != "old" {
				t.Fatalf("after update targetModel = %q, want the id", got)
			}
		})
	}
}

func gitTargetModel(t *testing.T, writer gitops.Writer, id string) string {
	t.Helper()
	external := readGitYAMLDocs(t, writer, "catalog/models/"+id+"/catalog/model.yaml")[0]
	refs, _, _ := unstructured.NestedSlice(external.Object, "spec", "externalProviderRefs")
	if len(refs) != 1 {
		t.Fatalf("%s: ExternalModel externalProviderRefs = %v, want one", id, refs)
	}
	target, _ := refs[0].(map[string]interface{})["targetModel"].(string)
	return target
}
