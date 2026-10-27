package servers

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const testOrgID = "0b6c2f1e-org"

func TestSubscriptionsGitCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
	srv := mustGitSubscriptionsServer(t, writer)

	created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := created.GetObject(); got.GetId() != "team-a" || got.GetMetadata().GetVersion() != 1 {
		t.Fatalf("Create object id = %q, version = %d", got.GetId(), got.GetMetadata().GetVersion())
	}
	if got := created.GetObject().GetSpec().GetOwner().GetGroups(); !slices.Equal(got, []string{"team-a"}) {
		t.Fatalf("Create owner.groups = %v, want the bare name", got)
	}
	if _, ok := created.GetObject().GetMetadata().GetAnnotations()[gitVersionAnnotation]; ok {
		t.Fatal("Create returned the version annotation")
	}
	if created.GetAuthPolicy().GetId() != "team-a-policy" {
		t.Fatalf("Create auth_policy = %v", created.GetAuthPolicy())
	}

	sub, policy := readGitSubscriptionFile(t, writer, "team-a")
	if sub.GetNamespace() != "models-as-a-service" || sub.GetAnnotations()[gitVersionAnnotation] != "1" {
		t.Fatalf("file MaaSSubscription namespace = %q, annotations = %v", sub.GetNamespace(), sub.GetAnnotations())
	}
	if policy.GetName() != "team-a-policy" || policy.GetNamespace() != "models-as-a-service" || policy.GetLabels()[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("file policy name = %q, namespace = %q, labels = %v", policy.GetName(), policy.GetNamespace(), policy.GetLabels())
	}
	if refs := policy.GetOwnerReferences(); len(refs) != 0 {
		t.Fatalf("file policy ownerReferences = %v, want none: Argo CD prunes it with the file", refs)
	}

	got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetObject().GetSpec().GetPriority() != 10 || len(got.GetAuthPolicies()) != 1 {
		t.Fatalf("Get priority = %d, auth_policies = %v", got.GetObject().GetSpec().GetPriority(), got.GetAuthPolicies())
	}
	regs := got.GetModelRegistrations()
	refs := got.GetObject().GetSpec().GetModelRefs()
	if len(regs) != len(refs) || len(regs) == 0 || regs[0].GetName() != refs[0].GetName() || regs[0].GetNamespace() != refs[0].GetNamespace() ||
		regs[0].GetPhase() != "" || regs[0].GetConditions()[0].GetReason() != "SavedToGit" {
		t.Fatalf("Get model_registrations = %v, want one per model ref, saved to Git", regs)
	}

	update := gitSampleSubscription("team-a")
	update.Metadata.Version = 1
	update.Spec.Priority = 20
	updated, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     update,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec"}},
		Lock:       true,
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.GetObject().GetMetadata().GetVersion() != 2 || updated.GetObject().GetSpec().GetPriority() != 20 {
		t.Fatalf("Update version = %d, priority = %d", updated.GetObject().GetMetadata().GetVersion(), updated.GetObject().GetSpec().GetPriority())
	}

	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: update, Lock: true})
	assertConnectCode(t, err, connect.CodeAborted)

	list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.GetTotal() != 1 || list.GetItems()[0].GetMetadata().GetVersion() != 2 {
		t.Fatalf("List = %v", list)
	}

	// A stale resource_version is Aborted and leaves the file.
	_, err = srv.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a", ResourceVersion: 1})
	assertConnectCode(t, err, connect.CodeAborted)
	if _, err := srv.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a", ResourceVersion: 2}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err = srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	assertConnectCode(t, err, connect.CodeNotFound)

	want := []string{"subscriptions: create team-a", "subscriptions: update team-a", "subscriptions: delete team-a"}
	if got := writer.Messages(); !slices.Equal(got, want) {
		t.Fatalf("commit messages = %v, want %v", got, want)
	}
}

// A zero resource_version skips the version check, as on the cluster.
func TestSubscriptionsGitDeleteWithoutVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitSubscriptionsServer(t, gitops.NewMemoryWriter(gitModelSeed("sim-model")))
	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := srv.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a"}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err := srv.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a"})
	assertConnectCode(t, err, connect.CodeNotFound)
}

func TestSubscriptionsGitUpdateMasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
	srv := mustGitSubscriptionsServer(t, writer)
	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	labelsOnly := &aigridv1.Subscription{Id: "team-a", Metadata: &aigridv1.Metadata{Labels: map[string]string{"tier": "gold"}}}
	updated, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     labelsOnly,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
	})
	if err != nil {
		t.Fatalf("Update(labels) error = %v", err)
	}
	if updated.GetObject().GetMetadata().GetLabels()["tier"] != "gold" || updated.GetObject().GetSpec().GetPriority() != 10 {
		t.Fatalf("Update(labels) = %v", updated.GetObject())
	}
	if updated.GetObject().GetMetadata().GetAnnotations()["openshift.io/display-name"] != "team-a" {
		t.Fatalf("Update(labels) dropped annotations: %v", updated.GetObject().GetMetadata().GetAnnotations())
	}

	// A single spec field leaves the rest of the spec.
	priority := &aigridv1.Subscription{Id: "team-a", Spec: &aigridv1.SubscriptionSpec{Priority: 30}}
	updated, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     priority,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.priority"}},
	})
	if err != nil {
		t.Fatalf("Update(spec.priority) error = %v", err)
	}
	if spec := updated.GetObject().GetSpec(); spec.GetPriority() != 30 || len(spec.GetModelRefs()) != 1 || spec.GetOrganization().GetId() != testOrgID {
		t.Fatalf("Update(spec.priority) spec = %v", spec)
	}

	// A mask path the API doesn't support is InvalidArgument.
	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     labelsOnly,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status"}},
	})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	// An id-only mask changes nothing and commits nothing.
	if _, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     labelsOnly,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"id"}},
	}); err != nil {
		t.Fatalf("Update(id) error = %v", err)
	}
	if n := len(writer.Messages()); n != 3 {
		t.Fatalf("commits = %d, want 3", n)
	}

	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: labelsOnly, Lock: true})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

func TestSubscriptionsGitValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitSubscriptionsServer(t, gitops.NewMemoryWriter(gitModelSeed("sim-model")))

	noOrg := gitSampleSubscription("team-a")
	noOrg.Spec.Organization = nil
	_, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: noOrg})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	for _, org := range []string{"org/admins", "Org A", "org\n"} {
		badOrg := gitSampleSubscription("team-a")
		badOrg.Spec.Organization.Id = org
		_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: badOrg})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	}

	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")})
	assertConnectCode(t, err, connect.CodeAlreadyExists)

	otherOrg := gitSampleSubscription("team-a")
	otherOrg.Spec.Organization.Id = "other-org"
	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: otherOrg})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: gitSampleSubscription("missing")})
	assertConnectCode(t, err, connect.CodeNotFound)
	_, err = srv.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "missing", ResourceVersion: 1})
	assertConnectCode(t, err, connect.CodeNotFound)
}

// TestSubscriptionsGitOwnerGroups checks the organization qualification:
// groups are written as <organization>/<group> in both the subscription and
// its policy, and read back bare. A group already qualified with the same
// organization is kept, one qualified with another organization or empty is
// rejected, and repeats are dropped.
func TestSubscriptionsGitOwnerGroups(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		groups   []string
		wantFile []string
		wantAPI  []string
		wantCode connect.Code
	}{
		"bare":                  {groups: []string{"team-a", "admins"}, wantFile: []string{testOrgID + "/team-a", testOrgID + "/admins"}, wantAPI: []string{"team-a", "admins"}},
		"same org prefix":       {groups: []string{testOrgID + "/team-a"}, wantFile: []string{testOrgID + "/team-a"}, wantAPI: []string{"team-a"}},
		"duplicates":            {groups: []string{"team-a", testOrgID + "/team-a", "team-a", "b"}, wantFile: []string{testOrgID + "/team-a", testOrgID + "/b"}, wantAPI: []string{"team-a", "b"}},
		"foreign prefix":        {groups: []string{"other-org/team-a"}, wantCode: connect.CodeInvalidArgument},
		"empty":                 {groups: []string{""}, wantCode: connect.CodeInvalidArgument},
		"blank":                 {groups: []string{"  "}, wantCode: connect.CodeInvalidArgument},
		"empty after prefix":    {groups: []string{testOrgID + "/"}, wantCode: connect.CodeInvalidArgument},
		"nested after prefix":   {groups: []string{testOrgID + "/a/b"}, wantCode: connect.CodeInvalidArgument},
		"leading separator":     {groups: []string{"/team-a"}, wantCode: connect.CodeInvalidArgument},
		"one bad among good":    {groups: []string{"team-a", "x/team-b"}, wantCode: connect.CodeInvalidArgument},
		"same org, twice cased": {groups: []string{"Team-A", "team-a"}, wantFile: []string{testOrgID + "/Team-A", testOrgID + "/team-a"}, wantAPI: []string{"Team-A", "team-a"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
			srv := mustGitSubscriptionsServer(t, writer)
			object := gitSampleSubscription("team-a")
			object.Spec.Owner.Groups = tc.groups
			created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: object})
			if tc.wantCode != 0 {
				assertConnectCode(t, err, tc.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if got := created.GetObject().GetSpec().GetOwner().GetGroups(); !slices.Equal(got, tc.wantAPI) {
				t.Fatalf("Create owner.groups = %v, want %v", got, tc.wantAPI)
			}
			sub, policy := readGitSubscriptionFile(t, writer, "team-a")
			if got := groupNames(t, sub, "spec", "owner", "groups"); !slices.Equal(got, tc.wantFile) {
				t.Fatalf("file owner.groups = %v, want %v", got, tc.wantFile)
			}
			if got := groupNames(t, policy, "spec", "subjects", "groups"); !slices.Equal(got, tc.wantFile) {
				t.Fatalf("file policy subjects.groups = %v, want %v", got, tc.wantFile)
			}
		})
	}
}

// An Update that brings a foreign organization's group is rejected too.
func TestSubscriptionsGitUpdateRejectsForeignGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitSubscriptionsServer(t, gitops.NewMemoryWriter(gitModelSeed("sim-model")))
	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	update := &aigridv1.Subscription{Id: "team-a", Spec: &aigridv1.SubscriptionSpec{
		Owner: &aigridv1.SubscriptionOwner{Groups: []string{"other-org/team-a"}},
	}}
	_, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: update, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.owner"}}})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

// TestSubscriptionsGitConcurrentWrite checks that a write whose base went
// stale between its read and its commit answers Aborted instead of
// overwriting the other write.
func TestSubscriptionsGitConcurrentWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := &racingWriter{MemoryWriter: gitops.NewMemoryWriter(gitModelSeed("sim-model"))}
	srv := mustGitSubscriptionsServer(t, writer)
	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	writer.race = func() {
		head, _ := writer.Head(ctx)
		if _, err := writer.MemoryWriter.Commit(ctx, head.Revision(), gitops.Change{
			Put:     map[string][]byte{subscriptionGitPath("team-a"): []byte("other write")},
			Message: "other",
		}); err != nil {
			t.Errorf("racing Commit() error = %v", err)
		}
	}
	update := gitSampleSubscription("team-a")
	update.Spec.Priority = 30
	_, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: update})
	assertConnectCode(t, err, connect.CodeAborted)
}

func TestSubscriptionsGitListFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitSubscriptionsServer(t, gitops.NewMemoryWriter(map[string][]byte{
		subscriptionsGitDir + "/README.md":                               []byte("not a subscription"),
		path.Join(modelDeploymentGitDir("sim-model"), "deployment.yaml"): []byte("seed"),
	}))
	for _, id := range []string{"team-a", "team-b"} {
		if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription(id)}); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	filter := `this.id == "team-b"`
	list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{Filter: &filter})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.GetTotal() != 1 || list.GetItems()[0].GetId() != "team-b" {
		t.Fatalf("List = %v", list)
	}
}

func TestSubscriptionsGitRejectsBrokenFile(t *testing.T) {
	t.Parallel()
	srv := mustGitSubscriptionsServer(t, gitops.NewMemoryWriter(map[string][]byte{
		subscriptionGitPath("team-a"): []byte("kind: MaaSSubscription\nmetadata:\n  name: team-a\n"),
	}))
	_, err := srv.Get(context.Background(), &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	assertConnectCode(t, err, connect.CodeInternal)
	_, err = srv.List(context.Background(), &aigridv1.SubscriptionsListRequest{})
	assertConnectCode(t, err, connect.CodeInternal)
}

// A model ref without a namespace points at the catalog entry in the
// subscription's own namespace, where the Models API writes MaaSModelRefs.
func TestSubscriptionsGitModelRefNamespaceDefault(t *testing.T) {
	t.Parallel()
	writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
	srv := mustGitSubscriptionsServer(t, writer)

	object := gitSampleSubscription("team-a")
	object.Spec.ModelRefs[0].Namespace = ""
	if _, err := srv.Create(context.Background(), &aigridv1.SubscriptionsCreateRequest{Object: object}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	sub, policy := readGitSubscriptionFile(t, writer, "team-a")
	for _, obj := range []*unstructured.Unstructured{sub, policy} {
		refs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "modelRefs")
		if len(refs) != 1 || refs[0].(map[string]interface{})["namespace"] != "models-as-a-service" {
			t.Fatalf("file %s modelRefs = %v, want namespace models-as-a-service", obj.GetKind(), refs)
		}
	}
}

// A model ref in the subscription namespace must name a model in the
// repository, on Create and on Update; a ref to another namespace isn't
// checked, because the Models API doesn't write there.
func TestSubscriptionsGitModelRefsMustExist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
	srv := mustGitSubscriptionsServer(t, writer)

	missing := gitSampleSubscription("team-a")
	missing.Spec.ModelRefs[0].Name = "no-such-model"
	_, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: missing})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	missing.Spec.ModelRefs[0].Namespace = "models-as-a-service"
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: missing})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)

	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	update := gitSampleSubscription("team-a")
	update.Spec.ModelRefs[0].Name = "no-such-model"
	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: update, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.model_refs"}}})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)

	other := gitSampleSubscription("team-b")
	other.Spec.ModelRefs[0].Name = "elsewhere"
	other.Spec.ModelRefs[0].Namespace = "other-namespace"
	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: other}); err != nil {
		t.Fatalf("Create(ref in another namespace) error = %v", err)
	}
	if n := len(writer.Messages()); n != 2 {
		t.Fatalf("commits = %d, want the two accepted creates", n)
	}
}

// gitModelSeed returns repository files in which each model id exists, for
// subscriptions to refer to.
func gitModelSeed(ids ...string) map[string][]byte {
	files := make(map[string][]byte, len(ids))
	for _, id := range ids {
		files[path.Join(modelDeploymentGitDir(id), "deployment.yaml")] = []byte("seed")
	}
	return files
}

// racingWriter runs race, once, between a Head and the Commit that follows it.
type racingWriter struct {
	*gitops.MemoryWriter
	race func()
}

func (w *racingWriter) Commit(ctx context.Context, base string, c gitops.Change) (string, error) {
	if race := w.race; race != nil {
		w.race = nil
		race()
	}
	return w.MemoryWriter.Commit(ctx, base, c)
}

func mustGitSubscriptionsServer(t *testing.T, writer gitops.Writer) *SubscriptionsServer {
	t.Helper()
	// No dynamic client: git mode must never call the cluster.
	srv, err := NewSubscriptionsServer().SetWriter(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return srv
}

// gitSampleSubscription is a subscription of organization testOrgID that
// grants group <id> 100 tokens a minute on sim-model.
func gitSampleSubscription(id string) *aigridv1.Subscription {
	return &aigridv1.Subscription{
		Id: id,
		Metadata: &aigridv1.Metadata{
			Name:        id,
			Annotations: map[string]string{"openshift.io/display-name": id},
		},
		Spec: &aigridv1.SubscriptionSpec{
			Organization: &aigridv1.SubscriptionOrganization{Id: testOrgID},
			Owner:        &aigridv1.SubscriptionOwner{Groups: []string{id}},
			ModelRefs: []*aigridv1.SubscriptionModelRef{{
				Name:            "sim-model",
				TokenRateLimits: []*aigridv1.TokenRateLimit{{Limit: 100, Window: "1m"}},
			}},
			Priority: 10,
		},
	}
}

func readGitSubscriptionFile(t *testing.T, writer gitops.Writer, id string) (sub, policy *unstructured.Unstructured) {
	t.Helper()
	for _, item := range readGitYAMLDocs(t, writer, subscriptionGitPath(id)) {
		switch item.GetKind() {
		case "MaaSSubscription":
			sub = item
		case "MaaSAuthPolicy":
			policy = item
		}
	}
	if sub == nil || policy == nil {
		t.Fatalf("file %s lacks a MaaSSubscription or MaaSAuthPolicy", subscriptionGitPath(id))
	}
	return sub, policy
}

func readGitYAMLDocs(t *testing.T, writer gitops.Writer, p string) []*unstructured.Unstructured {
	t.Helper()
	head, err := writer.Head(context.Background())
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	data, err := head.Read(p)
	if err != nil {
		t.Fatalf("Read(%s) error = %v", p, err)
	}
	docs, err := unmarshalYAMLDocs(data)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", p, err)
	}
	return docs
}

func groupNames(t *testing.T, obj *unstructured.Unstructured, fields ...string) []string {
	t.Helper()
	groups, _, err := unstructured.NestedSlice(obj.Object, fields...)
	if err != nil {
		t.Fatalf("%v: %v", fields, err)
	}
	var out []string
	for _, g := range groups {
		out = append(out, g.(map[string]interface{})["name"].(string))
	}
	return out
}

// Labels and annotations reach the files Argo CD applies, so only keys
// without a prefix and the console's two openshift.io annotations are
// allowed; every other prefixed key is rejected on Create and Update.
func TestSubscriptionsGitMetadataKeyAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
	srv := mustGitSubscriptionsServer(t, writer)
	allowed := gitSampleSubscription("team-a")
	allowed.Metadata.Labels = map[string]string{"team": "a", "env": "dev"}
	allowed.Metadata.Annotations = map[string]string{"openshift.io/display-name": "Team A", "openshift.io/description": "d", "note": "n"}
	created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: allowed})
	if err != nil {
		t.Fatalf("Create(allowed keys) error = %v", err)
	}
	// What a client reads back, it can send back.
	if _, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: created.GetObject(), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata"}}}); err != nil {
		t.Fatalf("Update(read metadata back) error = %v", err)
	}

	for _, key := range []string{
		"helm.sh/resource-policy", "argocd.argoproj.io/sync-options", "argocd.argoproj.io/compare-options",
		"kubectl.kubernetes.io/last-applied-configuration", "app.kubernetes.io/instance", "app.kubernetes.io/managed-by",
		"ai-grid/version", "ai-grid/clusters",
	} {
		labels := gitSampleSubscription("team-b")
		labels.Metadata.Labels = map[string]string{key: "x"}
		_, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: labels})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		if err != nil && !strings.Contains(err.Error(), key) {
			t.Errorf("Create(label %s) error = %v, want it to name the key", key, err)
		}
		annotations := gitSampleSubscription("team-b")
		annotations.Metadata.Annotations[key] = "x"
		_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: annotations})
		assertConnectCode(t, err, connect.CodeInvalidArgument)

		update := &aigridv1.Subscription{Id: "team-a", Metadata: &aigridv1.Metadata{Annotations: map[string]string{key: "x"}}}
		_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: update, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}}})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	}
	// The display-name annotation is allowed; as a label it is not.
	label := gitSampleSubscription("team-b")
	label.Metadata.Labels = map[string]string{"openshift.io/display-name": "x"}
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: label})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if n := len(writer.Messages()); n != 2 {
		t.Fatalf("commits = %d, want the create and the update", n)
	}
}
