package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path"
	"slices"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/proto"
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

// TestSubscriptionsGitTokenWindow checks the rate-limit window on Create and
// Update: the MaaS pattern, and at most 366 days (8784h), the MaaS
// controller's maxWindowSeconds. With at most four digits, only hours can
// exceed it; 527041m and 31622401s are refused by the pattern. A refused
// window commits nothing, and its message names the window and the maximum.
// The same rules are declared on TokenRateLimit.window (buf.validate), and
// protovalidate must accept and refuse exactly the windows the server does.
func TestSubscriptionsGitTokenWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		window string
		ok     bool
	}{
		{"1s", true},
		{"9999s", true},
		{"1m", true},
		{"9999m", true},
		{"1h", true},
		{"24h", true},
		{"8784h", true},
		{"8785h", false},
		{"9999h", false},
		{"10000s", false},
		{"31622400s", false},
		{"31622401s", false},
		{"10000m", false},
		{"527040m", false},
		{"527041m", false},
		{"10000h", false},
		{"0h", false},
		{"1d", false},
		{"", false},
	} {
		t.Run(tc.window, func(t *testing.T) {
			t.Parallel()
			err := protovalidate.Validate(&aigridv1.TokenRateLimit{Limit: 1, Window: tc.window})
			var verr *protovalidate.ValidationError
			switch {
			case tc.ok && err != nil:
				t.Fatalf("protovalidate(window %q) error = %v, want none", tc.window, err)
			case !tc.ok && !errors.As(err, &verr):
				// A compile or runtime error is not a refusal.
				t.Fatalf("protovalidate(window %q) error = %v, want a ValidationError", tc.window, err)
			}

			ctx := context.Background()
			writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
			srv := mustGitSubscriptionsServer(t, writer)
			check := func(op string, err error, commits int) {
				t.Helper()
				if tc.ok {
					if err != nil {
						t.Fatalf("%s(window %q) error = %v", op, tc.window, err)
					}
					return
				}
				assertConnectCode(t, err, connect.CodeInvalidArgument)
				msg := err.Error()
				if !strings.Contains(msg, `"`+tc.window+`"`) || !strings.Contains(msg, "8784h") {
					t.Fatalf("%s(window %q) message = %q, want the window and 8784h", op, tc.window, msg)
				}
				if n := len(writer.Messages()); n != commits {
					t.Fatalf("%s(window %q) commits = %d, want %d", op, tc.window, n, commits)
				}
			}

			create := gitSampleSubscription("team-a")
			create.Spec.ModelRefs[0].TokenRateLimits[0].Window = tc.window
			_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: create})
			check("Create", err, 0)

			if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-b")}); err != nil {
				t.Fatalf("Create(team-b) error = %v", err)
			}
			commits := len(writer.Messages())
			update := gitSampleSubscription("team-b")
			update.Spec.ModelRefs[0].TokenRateLimits[0].Window = tc.window
			_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
				Object:     update,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.model_refs"}},
			})
			check("Update", err, commits)
		})
	}
}

// TestSubscriptionsGitOwnerGroups checks the organization qualification:
// groups are written as <organization>/<group> in both the subscription and
// its policy, and read back bare. A group already qualified with the same
// organization is kept, one qualified with another organization or empty is
// rejected, surrounding spaces are trimmed, and repeats are dropped.
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
		"padded":                {groups: []string{" team-a ", "team-a", "\tb"}, wantFile: []string{testOrgID + "/team-a", testOrgID + "/b"}, wantAPI: []string{"team-a", "b"}},
		"padded after prefix":   {groups: []string{" " + testOrgID + "/ team-a"}, wantFile: []string{testOrgID + "/team-a"}, wantAPI: []string{"team-a"}},
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
// stale between its read and its commit never overwrites the other write:
// the rerun on the new head answers Aborted to a caller that sent the version
// it read, and merges onto the other write for a caller that sent none.
func TestSubscriptionsGitConcurrentWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := &racingWriter{MemoryWriter: gitops.NewMemoryWriter(gitModelSeed("sim-model"))}
	srv := mustGitSubscriptionsServer(t, writer)
	other := mustGitSubscriptionsServer(t, writer.MemoryWriter)
	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	otherWrite := func(mask string, set func(*aigridv1.Subscription)) func() {
		return func() {
			object := gitSampleSubscription("team-a")
			set(object)
			if _, err := other.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
				Object:     object,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{mask}},
			}); err != nil {
				t.Errorf("racing Update() error = %v", err)
			}
		}
	}

	writer.race = otherWrite("spec.priority", func(s *aigridv1.Subscription) { s.Spec.Priority = 20 })
	update := gitSampleSubscription("team-a")
	update.Metadata.Version = 1
	update.Spec.Priority = 30
	_, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     update,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.priority"}},
		Lock:       true,
	})
	assertConnectCode(t, err, connect.CodeAborted)
	if errors.Is(err, gitops.ErrConflict) {
		t.Fatalf("Update() error = %v, want the rerun's version check, not the conflict", err)
	}
	got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetObject().GetSpec().GetPriority() != 20 || got.GetObject().GetMetadata().GetVersion() != 2 {
		t.Fatalf("after the refused update priority = %d, version = %d, want the other write's 20 at version 2",
			got.GetObject().GetSpec().GetPriority(), got.GetObject().GetMetadata().GetVersion())
	}

	writer.race = otherWrite("metadata.annotations", func(s *aigridv1.Subscription) {
		s.Metadata.Annotations["openshift.io/display-name"] = "Team A"
	})
	update.Metadata.Version = 0
	updated, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     update,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.priority"}},
	})
	if err != nil {
		t.Fatalf("Update() without a version error = %v", err)
	}
	if o := updated.GetObject(); o.GetSpec().GetPriority() != 30 || o.GetMetadata().GetAnnotations()["openshift.io/display-name"] != "Team A" || o.GetMetadata().GetVersion() != 4 {
		t.Fatalf("Update() without a version = priority %d, annotations %v, version %d; want 30 merged onto the other write, at version 4",
			o.GetSpec().GetPriority(), o.GetMetadata().GetAnnotations(), o.GetMetadata().GetVersion())
	}
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

// racingWriter runs race, once, between a Head and the Commit that follows it,
// and counts the Commits made through it.
type racingWriter struct {
	*gitops.MemoryWriter
	race    func()
	commits int
}

func (w *racingWriter) Commit(ctx context.Context, base string, c gitops.Change) (string, error) {
	w.commits++
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

// Label and annotation keys and values must be ones Kubernetes accepts, or
// Argo CD can't apply the file: the API answers InvalidArgument and commits
// nothing, as cluster mode did.
func TestSubscriptionsGitMetadataValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(gitModelSeed("sim-model"))
	srv := mustGitSubscriptionsServer(t, writer)
	models := mustGitModelsServer(t, writer)
	for name, meta := range map[string]*aigridv1.Metadata{
		"label value with a space":  {Labels: map[string]string{"team": "two words"}},
		"label value too long":      {Labels: map[string]string{"team": strings.Repeat("a", 64)}},
		"label key not a name":      {Labels: map[string]string{"bad key!": "a"}},
		"label key too long":        {Labels: map[string]string{strings.Repeat("k", 64): "a"}},
		"annotation key not a name": {Annotations: map[string]string{"bad key": "a"}},
		"annotations too large":     {Annotations: map[string]string{"note": strings.Repeat("a", 256*1024)}},
	} {
		sub := gitSampleSubscription("team-a")
		for k, v := range meta.GetLabels() {
			sub.Metadata.Labels = map[string]string{k: v}
		}
		for k, v := range meta.GetAnnotations() {
			sub.Metadata.Annotations[k] = v
		}
		_, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: sub})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("subscription Create(%s) error = %v, want InvalidArgument", name, err)
		}
		model := gitSampleModel("other-model", "site-a")
		model.Metadata = meta
		_, err = models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("model Create(%s) error = %v, want InvalidArgument", name, err)
		}
	}
	if n := len(writer.Messages()); n != 0 {
		t.Fatalf("commits = %d, want none", n)
	}
}

// A backend started without AI_GRID_GIT_URL answers FailedPrecondition for
// every Subscriptions call, which all read or write the catalog, naming the
// setting, rather than not starting.
func TestSubscriptionsGitUnconfigured(t *testing.T) {
	ctx := context.Background()
	srv := mustGitSubscriptionsServer(t, gitops.Unconfigured{})
	_, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), "AI_GRID_GIT_URL") {
		t.Errorf("List() error = %v, want it to name AI_GRID_GIT_URL", err)
	}
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}

// Each model ref names the Model it refers to by model_id, with the display
// name and description of the MaaSModelRef the Models API publishes for it,
// on every read and every write's answer, so a client joins a ref to its
// model by id and needs no namespace of its own. They're read from the
// model's catalog entry each time, never stored in the subscription file, and
// what a client sends in them is ignored.
func TestSubscriptionsGitModelRefCatalogJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	models := mustGitModelsServer(t, writer)
	srv := mustGitSubscriptionsServer(t, writer)

	granite := gitSampleModel("granite", "site-a")
	granite.Metadata.Annotations = map[string]string{
		annotationDisplayName: "Granite 3B",
		annotationDescription: "A small model",
	}
	plain := gitSampleModel("plain", "site-a")
	plain.Metadata.Annotations = nil
	for _, model := range []*aigridv1.Model{granite, plain} {
		if _, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model}); err != nil {
			t.Fatalf("Create model %s error = %v", model.GetId(), err)
		}
	}

	object := gitSampleSubscription("team-a")
	limits := object.Spec.ModelRefs[0].TokenRateLimits
	object.Spec.ModelRefs = []*aigridv1.SubscriptionModelRef{
		// A client's values in the output fields are ignored.
		{Name: "granite", TokenRateLimits: limits, ModelId: "bogus", DisplayName: "stale", Description: "stale"},
		{Name: "plain", Namespace: "models-as-a-service", TokenRateLimits: limits},
		// Not a MaaSModelRef the Models API wrote: no model.
		{Name: "granite", Namespace: "other-namespace", TokenRateLimits: limits, ModelId: "granite"},
	}
	want := []*aigridv1.SubscriptionModelRef{
		{Name: "granite", Namespace: "models-as-a-service", ModelId: "granite", DisplayName: "Granite 3B", Description: "A small model"},
		{Name: "plain", Namespace: "models-as-a-service", ModelId: "plain"},
		{Name: "granite", Namespace: "other-namespace"},
	}
	check := func(op string, sub *aigridv1.Subscription) {
		t.Helper()
		refs := sub.GetSpec().GetModelRefs()
		if len(refs) != len(want) {
			t.Fatalf("%s model_refs = %v, want %d", op, refs, len(want))
		}
		for i, w := range want {
			got := refs[i]
			if got.GetName() != w.GetName() || got.GetNamespace() != w.GetNamespace() || got.GetModelId() != w.GetModelId() ||
				got.GetDisplayName() != w.GetDisplayName() || got.GetDescription() != w.GetDescription() {
				t.Errorf("%s model_refs[%d] = {name %q, namespace %q, model_id %q, display_name %q, description %q}, want {%q, %q, %q, %q, %q}",
					op, i, got.GetName(), got.GetNamespace(), got.GetModelId(), got.GetDisplayName(), got.GetDescription(),
					w.GetName(), w.GetNamespace(), w.GetModelId(), w.GetDisplayName(), w.GetDescription())
			}
		}
		if err := protovalidate.Validate(sub); err != nil {
			t.Errorf("%s answer fails its own validation rules: %v", op, err)
		}
	}

	created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: object})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	check("Create", created.GetObject())

	got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	check("Get", got.GetObject())

	list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.GetItems()) != 1 {
		t.Fatalf("List items = %v, want team-a", list.GetItems())
	}
	check("List", list.GetItems()[0])

	// The subscriptions that use a model, as the model's page asks for them.
	for filter, wantIDs := range map[string][]string{
		`this.spec.model_refs.exists(r, r.model_id == "granite")`: {"team-a"},
		`this.spec.model_refs.exists(r, r.model_id == "plain")`:   {"team-a"},
		`this.spec.model_refs.exists(r, r.model_id == "other")`:   nil,
	} {
		list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{Filter: &filter})
		if err != nil {
			t.Fatalf("List(%s) error = %v", filter, err)
		}
		var ids []string
		for _, item := range list.GetItems() {
			ids = append(ids, item.GetId())
		}
		if !slices.Equal(ids, wantIDs) {
			t.Errorf("List(%s) = %v, want %v", filter, ids, wantIDs)
		}
	}

	update := proto.CloneOf(object)
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
	check("Update", updated.GetObject())
	// A mask that names nothing stored answers without a commit.
	update.Metadata.Version = 2
	unchanged, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     update,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"id"}},
	})
	if err != nil {
		t.Fatalf("Update(no-op) error = %v", err)
	}
	check("Update(no-op)", unchanged.GetObject())

	sub, _ := readGitSubscriptionFile(t, writer, "team-a")
	refs, _, _ := unstructured.NestedSlice(sub.Object, "spec", "modelRefs")
	for i, raw := range refs {
		for _, key := range []string{"modelId", "model_id", "displayName", "display_name", "description"} {
			if _, ok := raw.(map[string]interface{})[key]; ok {
				t.Errorf("file modelRefs[%d] has %q; it is output only", i, key)
			}
		}
	}

	// A new display name on the model shows on the next read.
	rename := proto.CloneOf(granite)
	rename.Metadata.Annotations = map[string]string{annotationDisplayName: "Granite 3B v2"}
	if _, err := models.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     rename,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}},
	}); err != nil {
		t.Fatalf("Update model error = %v", err)
	}
	want[0].DisplayName, want[0].Description = "Granite 3B v2", ""
	got, err = srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	if err != nil {
		t.Fatalf("Get() after the model's update error = %v", err)
	}
	check("Get after the model's update", got.GetObject())
}

// model_id follows the MaaSModelRef a ref names on the ingress clusters, not
// the backend's settings: a ref is joined only when the model's catalog entry
// has a MaaSModelRef of that name in that namespace. A model without a
// catalog entry has none.
func TestSubscriptionsGitModelIDFollowsTheCatalogEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("another subscription namespace", func(t *testing.T) {
		t.Parallel()
		writer := gitops.NewMemoryWriter(nil)
		models := mustGitModelsServer(t, writer) // publishes into models-as-a-service
		if _, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim-model", "site-a")}); err != nil {
			t.Fatalf("Create model error = %v", err)
		}
		srv, err := NewSubscriptionsServer().SetWriter(writer).SetNamespace("tenant-maas").Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		ref := created.GetObject().GetSpec().GetModelRefs()[0]
		if ref.GetNamespace() != "tenant-maas" || ref.GetModelId() != "" || ref.GetDisplayName() != "" {
			t.Fatalf("ref = %v, want namespace tenant-maas and no model: the MaaSModelRef is in models-as-a-service", ref)
		}
	})

	t.Run("no catalog entry", func(t *testing.T) {
		t.Parallel()
		srv := mustGitSubscriptionsServer(t, gitops.NewMemoryWriter(gitModelSeed("sim-model")))
		if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if id := got.GetObject().GetSpec().GetModelRefs()[0].GetModelId(); id != "" {
			t.Fatalf("model_id = %q, want empty: the repository has no catalog entry", id)
		}
	})
}

// The join only decorates: a model's catalog entry that can't be read or
// parsed leaves the refs to that model without model_id, display_name and
// description, and List, Get and Update of every subscription still answer,
// with the other refs joined, so a subscription can still be edited around
// the broken model.
func TestSubscriptionsGitBrokenCatalogEntryOnlyUnjoinsItsRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	brokenPath := path.Join(modelCatalogGitDir("broken"), "model.yaml")
	for name, breakEntry := range map[string]func(t *testing.T, w *unreadableWriter){
		"bad YAML": func(t *testing.T, w *unreadableWriter) {
			t.Helper()
			head, err := w.Head(ctx)
			if err != nil {
				t.Fatalf("Head() error = %v", err)
			}
			if _, err := w.Commit(ctx, head.Revision(), gitops.Change{
				Put:     map[string][]byte{brokenPath: []byte("kind: [")},
				Message: "hand edit",
			}); err != nil {
				t.Fatalf("Commit(hand edit) error = %v", err)
			}
		},
		"read error": func(_ *testing.T, w *unreadableWriter) { w.unreadable = brokenPath },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &unreadableWriter{MemoryWriter: gitops.NewMemoryWriter(nil)}
			models := mustGitModelsServer(t, writer)
			var logs bytes.Buffer
			srv, err := NewSubscriptionsServer().SetWriter(writer).SetLogger(slog.New(slog.NewJSONHandler(&logs, nil))).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			for _, id := range []string{"broken", "plain"} {
				model := gitSampleModel(id, "site-a")
				model.Metadata.Annotations = map[string]string{annotationDisplayName: id + " model"}
				if _, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model}); err != nil {
					t.Fatalf("Create model %s error = %v", id, err)
				}
			}
			both := gitSampleSubscription("team-a")
			limits := both.Spec.ModelRefs[0].TokenRateLimits
			both.Spec.ModelRefs = []*aigridv1.SubscriptionModelRef{
				{Name: "broken", TokenRateLimits: limits},
				{Name: "plain", TokenRateLimits: limits},
			}
			plainOnly := gitSampleSubscription("team-b")
			plainOnly.Spec.ModelRefs[0].Name = "plain"
			for _, sub := range []*aigridv1.Subscription{both, plainOnly} {
				if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: sub}); err != nil {
					t.Fatalf("Create(%s) error = %v", sub.GetId(), err)
				}
			}
			breakEntry(t, writer)

			check := func(op string, sub *aigridv1.Subscription) {
				t.Helper()
				for _, ref := range sub.GetSpec().GetModelRefs() {
					wantID, wantName := "", ""
					if ref.GetName() == "plain" {
						wantID, wantName = "plain", "plain model"
					}
					if ref.GetModelId() != wantID || ref.GetDisplayName() != wantName || ref.GetDescription() != "" {
						t.Errorf("%s %s ref %s = {model_id %q, display_name %q, description %q}, want {%q, %q, \"\"}",
							op, sub.GetId(), ref.GetName(), ref.GetModelId(), ref.GetDisplayName(), ref.GetDescription(), wantID, wantName)
					}
				}
			}

			list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v, want an answer", err)
			}
			if len(list.GetItems()) != 2 {
				t.Fatalf("List items = %d, want 2", len(list.GetItems()))
			}
			for _, item := range list.GetItems() {
				check("List", item)
			}
			got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
			if err != nil {
				t.Fatalf("Get() error = %v, want an answer", err)
			}
			check("Get", got.GetObject())
			relabel := gitSampleSubscription("team-a")
			relabel.Metadata.Labels = map[string]string{"team": "a"}
			updated, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
				Object:     relabel,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
			})
			if err != nil {
				t.Fatalf("Update(labels) error = %v, want an answer", err)
			}
			if updated.GetObject().GetMetadata().GetLabels()["team"] != "a" {
				t.Fatalf("Update(labels) labels = %v", updated.GetObject().GetMetadata().GetLabels())
			}
			check("Update", updated.GetObject())
			// Each call logs the broken entry once, naming the model and file.
			var warned int
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				if entry["level"] != "WARN" || entry["model"] != "broken" || entry["file"] != brokenPath || entry["error"] == "" {
					t.Errorf("log = %v, want a warning naming model broken and %s", entry, brokenPath)
				}
				warned++
			}
			if warned != 3 {
				t.Errorf("warnings = %d, want one each for List, Get and Update", warned)
			}
		})
	}
}

// unreadableWriter is a MemoryWriter whose snapshots fail to read one path
// with an error other than ErrNotFound, as a Git read can.
type unreadableWriter struct {
	*gitops.MemoryWriter
	unreadable string
}

func (w *unreadableWriter) Head(ctx context.Context) (gitops.Snapshot, error) {
	head, err := w.MemoryWriter.Head(ctx)
	if err != nil || w.unreadable == "" {
		return head, err
	}
	return unreadableSnapshot{Snapshot: head, unreadable: w.unreadable}, nil
}

type unreadableSnapshot struct {
	gitops.Snapshot
	unreadable string
}

func (s unreadableSnapshot) Read(p string) ([]byte, error) {
	if p == s.unreadable {
		return nil, errors.New("object not readable")
	}
	return s.Snapshot.Read(p)
}
