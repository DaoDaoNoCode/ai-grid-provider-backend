package servers

import (
	"context"
	"fmt"
	"slices"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseModelCapabilities(t *testing.T) {
	t.Parallel()
	valid := []struct {
		value string
		want  []string
	}{
		{`["Chat"]`, []string{"Chat"}},
		{`["Text completion"]`, []string{"Text completion"}},
		{`["Vision","Chat"]`, []string{"Vision", "Chat"}},
		{` [ "Chat" ] `, []string{"Chat"}},
		{`[]`, []string{}},
	}
	for _, tc := range valid {
		got, err := parseModelCapabilities(tc.value)
		if err != nil {
			t.Errorf("parseModelCapabilities(%s) error = %v, want nil", tc.value, err)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("parseModelCapabilities(%s) = %q, want %q", tc.value, got, tc.want)
		}
	}

	for _, value := range []string{
		``,
		`Chat`,
		`"Chat"`,
		`null`,
		`{"Chat":true}`,
		`["Chat"`,
		`["Chat"] ["Vision"]`,
		`[1]`,
		`["Chat",true]`,
		`["Chat",null]`,
		`[["Chat"]]`,
		`[""]`,
		`["Chat"," \t"]`,
	} {
		if got, err := parseModelCapabilities(value); err == nil {
			t.Errorf("parseModelCapabilities(%s) = %q, want an error", value, got)
		}
	}
}

func TestNormalizeModelCapability(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"Chat", "chat"},
		{"  CHAT\t", "chat"},
		{"Text completion", "text completion"},
		{" TEXT Completion\n", "text completion"},
		{"text_completion", "text_completion"},
		{"text-completion", "text-completion"},
		{"Vision", "vision"},
		{"", ""},
	} {
		if got := normalizeModelCapability(tc.in); got != tc.want {
			t.Errorf("normalizeModelCapability(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// catalogDoc returns the object of kind in model id's catalog file.
func catalogDoc(t *testing.T, writer gitops.Writer, id, kind string) *unstructured.Unstructured {
	t.Helper()
	for _, doc := range readGitYAMLDocs(t, writer, "catalog/models/"+id+"/catalog/model.yaml") {
		if doc.GetKind() == kind {
			return doc
		}
	}
	t.Fatalf("catalog/models/%s/catalog/model.yaml has no %s", id, kind)
	return nil
}

// externalModelPath is the path of the ExternalModel's one provider ref.
func externalModelPath(t *testing.T, writer gitops.Writer, id string) string {
	t.Helper()
	refs, _, _ := unstructured.NestedSlice(catalogDoc(t, writer, id, "ExternalModel").Object, "spec", "externalProviderRefs")
	if len(refs) != 1 {
		t.Fatalf("ExternalModel %s externalProviderRefs = %v, want one", id, refs)
	}
	ref := refs[0].(map[string]interface{})
	if ref["apiFormat"] != "openai-chat" {
		t.Fatalf("ExternalModel %s apiFormat = %v, want openai-chat", id, ref["apiFormat"])
	}
	return ref["path"].(string)
}

// A Model may carry the capabilities annotation; Create writes it as sent
// on the LLMInferenceService and on the MaaSModelRef MaaS reads it from, and
// a read returns it. A value that isn't a JSON array of strings is
// InvalidArgument, and nothing is committed.
func TestModelsGitCapabilitiesAnnotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	for i, value := range []string{`["Chat"]`, `["Text completion"]`, `["Vision","Chat"]`} {
		id := fmt.Sprintf("caps-%d", i)
		object := gitSampleModel(id, "site-a")
		object.Metadata.Annotations[annotationModelCapabilities] = value
		created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: object})
		if err != nil {
			t.Fatalf("Create(%s) error = %v", value, err)
		}
		if got := created.GetObject().GetMetadata().GetAnnotations()[annotationModelCapabilities]; got != value {
			t.Fatalf("Create(%s) annotation = %q", value, got)
		}
		llm := readGitYAMLDocs(t, writer, "catalog/models/"+id+"/deployment/deployment.yaml")[0]
		if got := llm.GetAnnotations()[annotationModelCapabilities]; llm.GetKind() != llmInferenceKind || got != value {
			t.Fatalf("%s annotation = %q, want %q", llm.GetKind(), got, value)
		}
		if got := catalogDoc(t, writer, id, maasModelRefKind).GetAnnotations()[annotationModelCapabilities]; got != value {
			t.Fatalf("MaaSModelRef annotation = %q, want %q", got, value)
		}
		read, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: id})
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if got := read.GetObject().GetMetadata().GetAnnotations()[annotationModelCapabilities]; got != value {
			t.Fatalf("Get(%s) annotation = %q, want %q", id, got, value)
		}
	}
	commits := len(writer.Messages())

	for _, value := range []string{`Chat`, `{"Chat":true}`, `["Chat"`, `[1]`, `["Chat",null]`, `[""]`, `null`} {
		object := gitSampleModel("refused", "site-a")
		object.Metadata.Annotations[annotationModelCapabilities] = value
		_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: object})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	}
	// Only as an annotation: a prefixed label is still refused.
	label := gitSampleModel("refused", "site-a")
	label.Metadata.Labels = map[string]string{annotationModelCapabilities: "chat"}
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: label})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if n := len(writer.Messages()); n != commits {
		t.Fatalf("commits = %d, want %d: a refused create commits nothing", n, commits)
	}
}

// An update of the annotations alone may add, change and remove the
// capabilities annotation; a malformed value is InvalidArgument and leaves
// the model as it was.
func TestModelsGitCapabilitiesUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}}
	model := created.GetObject()

	for _, value := range []string{`["Text completion"]`, `["Chat"]`, ""} {
		if value == "" {
			delete(model.Metadata.Annotations, annotationModelCapabilities)
		} else {
			model.Metadata.Annotations[annotationModelCapabilities] = value
		}
		updated, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: model, UpdateMask: mask, Lock: true})
		if err != nil {
			t.Fatalf("Update(%q) error = %v", value, err)
		}
		model = updated.GetObject()
		got, ok := model.GetMetadata().GetAnnotations()[annotationModelCapabilities]
		if got != value || ok != (value != "") {
			t.Fatalf("Update(%q) annotation = %q (present %v)", value, got, ok)
		}
		if got := catalogDoc(t, writer, "sim", maasModelRefKind).GetAnnotations()[annotationModelCapabilities]; got != value {
			t.Fatalf("Update(%q) MaaSModelRef annotation = %q", value, got)
		}
		if got := gitModelClusters(model); !slices.Equal(got, []string{"site-a"}) {
			t.Fatalf("Update(%q) placement clusters = %v, want unchanged", value, got)
		}
	}
	if v := model.GetMetadata().GetVersion(); v != 4 {
		t.Fatalf("version = %d, want 4 after three updates", v)
	}

	bad := proto.CloneOf(model)
	bad.Metadata.Annotations[annotationModelCapabilities] = `["Chat",1]`
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: bad, UpdateMask: mask})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if n := len(writer.Messages()); n != 4 {
		t.Fatalf("commits = %d, want 4: a refused update commits nothing", n)
	}
}

// The ExternalModel routes to the endpoint the model serves: a
// completion-only model ("Text completion" its only well-known capability)
// to /v1/completions, every other model, malformed annotation included, to
// /v1/chat/completions.
func TestModelsGitExternalModelPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value string // "" for no annotation
		want  string
	}{
		{"none", "", modelChatCompletionsPath},
		{"chat", `["Chat"]`, modelChatCompletionsPath},
		{"completion", `["Text completion"]`, modelCompletionsPath},
		{"completion-case-and-space", `[" TEXT Completion "]`, modelCompletionsPath},
		{"completion-and-custom", `["Text completion","Code"]`, modelCompletionsPath},
		{"both", `["Text completion","Chat"]`, modelChatCompletionsPath},
		{"chat-case-and-space", `["Text completion"," CHAT "]`, modelChatCompletionsPath},
		{"vision-and-completion", `["Vision"," TEXT Completion "]`, modelChatCompletionsPath},
		{"transcription-and-completion", `["Transcription","Text completion"]`, modelChatCompletionsPath},
		{"transcription-case-and-space", `["Text completion"," TRANSCRIPTION "]`, modelChatCompletionsPath},
		{"vision", `["Vision"]`, modelChatCompletionsPath},
		{"underscore", `["text_completion"]`, modelChatCompletionsPath},
		{"hyphen", `["text-completion"]`, modelChatCompletionsPath},
		{"empty", `[]`, modelChatCompletionsPath},
		{"malformed", `["Text completion"`, modelChatCompletionsPath},
		{"not-strings", `[["Text completion"]]`, modelChatCompletionsPath},
	} {
		object := gitSampleModel("sim", "site-a")
		if tc.value != "" {
			object.Metadata.Annotations[annotationModelCapabilities] = tc.value
		}
		if got := modelExternalPath(object); got != tc.want {
			t.Errorf("%s: modelExternalPath = %q, want %q", tc.name, got, tc.want)
		}
	}

	// Rendered: Create writes the path, and an update that flips the
	// annotation rewrites it.
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)
	object := gitSampleModel("sim", "site-a")
	object.Metadata.Annotations[annotationModelCapabilities] = `["Text completion"]`
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: object})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := externalModelPath(t, writer, "sim"); got != modelCompletionsPath {
		t.Fatalf("Create path = %q, want %q", got, modelCompletionsPath)
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}}
	model := created.GetObject()
	for _, step := range []struct {
		value string
		want  string
	}{
		{`["Chat"]`, modelChatCompletionsPath},
		{`["text completion"]`, modelCompletionsPath},
		{"", modelChatCompletionsPath},
	} {
		if step.value == "" {
			delete(model.Metadata.Annotations, annotationModelCapabilities)
		} else {
			model.Metadata.Annotations[annotationModelCapabilities] = step.value
		}
		updated, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: model, UpdateMask: mask})
		if err != nil {
			t.Fatalf("Update(%q) error = %v", step.value, err)
		}
		model = updated.GetObject()
		if got := externalModelPath(t, writer, "sim"); got != step.want {
			t.Fatalf("Update(%q) path = %q, want %q", step.value, got, step.want)
		}
	}
}
