package servers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Writes to two different models at once both rewrite a shared index, so
// the second commit conflicts. Its rerun on the new head succeeds and keeps
// both models in the indexes.
func TestModelsGitWriteRetriesAnotherModelsIndexChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := &racingWriter{MemoryWriter: gitops.NewMemoryWriter(nil)}
	models := mustGitModelsServer(t, writer)
	other := mustGitModelsServer(t, writer.MemoryWriter)
	otherWrite := func(write func() error) func() {
		return func() {
			if err := write(); err != nil {
				t.Errorf("racing write error = %v", err)
			}
		}
	}
	assertIndex := func(index string, want ...string) {
		t.Helper()
		if got := kustomizationResources(t, writer, index); !slices.Equal(got, want) {
			t.Fatalf("%s resources = %v, want %v", index, got, want)
		}
	}

	// Create: both write the ingress index and site-a's.
	writer.race = otherWrite(func() error {
		_, err := other.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("model-b", "site-a")})
		return err
	})
	created, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("model-a", "site-a")})
	if err != nil {
		t.Fatalf("Create() racing another create error = %v", err)
	}
	if created.GetObject().GetMetadata().GetVersion() != 1 {
		t.Fatalf("Create version = %d, want 1", created.GetObject().GetMetadata().GetVersion())
	}
	if writer.commits != 2 {
		t.Fatalf("Create commits = %d, want 2: one that conflicted and its rerun", writer.commits)
	}
	assertIndex(ingressGitIndex, "../models/model-a/catalog", "../models/model-b/catalog")
	assertIndex(clusterGitIndex("site-a"), "../../models/model-a/deployment", "../../models/model-b/deployment")

	// Update with the version it read: moving model-a to site-b writes
	// site-b's index, which another create also writes.
	writer.commits = 0
	writer.race = otherWrite(func() error {
		_, err := other.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("model-c", "site-b")})
		return err
	})
	move := gitSampleModel("model-a", "site-b")
	move.Metadata.Version = 1
	updated, err := models.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     move,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
		Lock:       true,
	})
	if err != nil {
		t.Fatalf("Update() racing another create error = %v", err)
	}
	if updated.GetObject().GetMetadata().GetVersion() != 2 || writer.commits != 2 {
		t.Fatalf("Update version = %d, commits = %d; want version 2 after 2 commits", updated.GetObject().GetMetadata().GetVersion(), writer.commits)
	}
	assertIndex(clusterGitIndex("site-a"), "../../models/model-b/deployment")
	assertIndex(clusterGitIndex("site-b"), "../../models/model-a/deployment", "../../models/model-c/deployment")

	// Delete with the version it read: both remove an entry from the
	// ingress index.
	writer.commits = 0
	writer.race = otherWrite(func() error {
		_, err := other.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "model-b", ResourceVersion: 1})
		return err
	})
	if _, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "model-a", ResourceVersion: 2}); err != nil {
		t.Fatalf("Delete() racing another delete error = %v", err)
	}
	if writer.commits != 2 {
		t.Fatalf("Delete commits = %d, want 2", writer.commits)
	}
	assertIndex(ingressGitIndex, "../models/model-c/catalog")
	assertIndex(clusterGitIndex("site-a"))
	assertIndex(clusterGitIndex("site-b"), "../../models/model-c/deployment")

	want := []string{
		"models: create model-b", "models: create model-a",
		"models: create model-c", "models: update model-a",
		"models: delete model-b", "models: delete model-a",
	}
	if got := writer.Messages(); !slices.Equal(got, want) {
		t.Fatalf("commit messages = %v, want %v", got, want)
	}
}

// A rerun decides again on the new head, so a change to the model itself is
// refused as it would have been without the race: Aborted for a caller that
// sent the version it read, AlreadyExists for a create of an id created
// meanwhile. A caller that sent no version merges onto the latest model.
func TestModelsGitRetryKeepsTheChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := &racingWriter{MemoryWriter: gitops.NewMemoryWriter(nil)}
	models := mustGitModelsServer(t, writer)
	other := mustGitModelsServer(t, writer.MemoryWriter)
	otherUpdate := func(mask string, set func(*aigridv1.Model)) func() {
		return func() {
			object := gitSampleModel("model-a", "site-a")
			set(object)
			if _, err := other.Update(ctx, &aigridv1.ModelsUpdateRequest{
				Object:     object,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{mask}},
			}); err != nil {
				t.Errorf("racing Update() error = %v", err)
			}
		}
	}
	replicas := func(n int32) func(*aigridv1.Model) {
		return func(m *aigridv1.Model) { m.Spec.Replicas = &n }
	}

	writer.race = func() {
		theirs := gitSampleModel("model-a", "site-b")
		theirs.Metadata.Annotations["openshift.io/display-name"] = "Theirs"
		if _, err := other.Create(ctx, &aigridv1.ModelsCreateRequest{Object: theirs}); err != nil {
			t.Errorf("racing Create() error = %v", err)
		}
	}
	_, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("model-a", "site-a")})
	assertConnectCode(t, err, connect.CodeAlreadyExists)
	got, err := models.Get(ctx, &aigridv1.ModelsGetRequest{Id: "model-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if name := got.GetObject().GetMetadata().GetAnnotations()["openshift.io/display-name"]; name != "Theirs" {
		t.Fatalf("display name after the refused create = %q, want the other create's", name)
	}

	for _, op := range []struct {
		name  string
		write func(version int32) error
	}{
		{"Update", func(version int32) error {
			mine := gitSampleModel("model-a", "site-a")
			mine.Metadata.Version = version
			mine.Metadata.Annotations["openshift.io/display-name"] = "Mine"
			_, err := models.Update(ctx, &aigridv1.ModelsUpdateRequest{
				Object:     mine,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}},
				Lock:       true,
			})
			return err
		}},
		{"Delete", func(version int32) error {
			_, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "model-a", ResourceVersion: version})
			return err
		}},
	} {
		got, err := models.Get(ctx, &aigridv1.ModelsGetRequest{Id: "model-a"})
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		read := got.GetObject().GetMetadata().GetVersion()
		writer.commits = 0
		writer.race = otherUpdate("spec.replicas", replicas(3))
		err = op.write(read)
		assertConnectCode(t, err, connect.CodeAborted)
		if errors.Is(err, gitops.ErrConflict) {
			t.Fatalf("%s() error = %v, want the rerun's version check, not the conflict", op.name, err)
		}
		if writer.commits != 1 {
			t.Fatalf("%s() commits = %d, want 1: the rerun refuses before committing", op.name, writer.commits)
		}
		got, err = models.Get(ctx, &aigridv1.ModelsGetRequest{Id: "model-a"})
		if err != nil {
			t.Fatalf("Get() after the refused %s error = %v", op.name, err)
		}
		if v := got.GetObject().GetMetadata().GetVersion(); v != read+1 || got.GetObject().GetMetadata().GetAnnotations()["openshift.io/display-name"] != "Theirs" {
			t.Fatalf("after the refused %s: version %d, annotations %v; want only the other write at version %d", op.name, v, got.GetObject().GetMetadata().GetAnnotations(), read+1)
		}
	}

	writer.race = otherUpdate("spec.replicas", replicas(4))
	mine := gitSampleModel("model-a", "site-a")
	mine.Metadata.Annotations["openshift.io/display-name"] = "Mine"
	updated, err := models.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     mine,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}},
	})
	if err != nil {
		t.Fatalf("Update() without a version error = %v", err)
	}
	if o := updated.GetObject(); o.GetSpec().GetReplicas() != 4 || o.GetMetadata().GetAnnotations()["openshift.io/display-name"] != "Mine" {
		t.Fatalf("Update() without a version = replicas %d, annotations %v; want Mine merged onto the other write's 4 replicas",
			o.GetSpec().GetReplicas(), o.GetMetadata().GetAnnotations())
	}

	writer.race = otherUpdate("spec.replicas", replicas(5))
	if _, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "model-a"}); err != nil {
		t.Fatalf("Delete() without a version error = %v", err)
	}
	_, err = models.Get(ctx, &aigridv1.ModelsGetRequest{Id: "model-a"})
	assertConnectCode(t, err, connect.CodeNotFound)
}

// A subscription's commit reads its models, so a model written meanwhile
// makes it conflict; the rerun succeeds while the model is still there, and
// answers FailedPrecondition once it is gone. A change to the subscription
// itself is Aborted for a caller that sent the version it read.
func TestSubscriptionsGitRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := &racingWriter{MemoryWriter: gitops.NewMemoryWriter(nil)}
	models := mustGitModelsServer(t, writer.MemoryWriter)
	subscriptions := mustGitSubscriptionsServer(t, writer)
	other := mustGitSubscriptionsServer(t, writer.MemoryWriter)
	for _, id := range []string{"sim-model", "model-b"} {
		if _, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel(id, "site-a")}); err != nil {
			t.Fatalf("Create model %s error = %v", id, err)
		}
	}
	updateModel := func(n int32) func() {
		return func() {
			object := gitSampleModel("sim-model", "site-a")
			object.Spec.Replicas = &n
			if _, err := models.Update(ctx, &aigridv1.ModelsUpdateRequest{
				Object:     object,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.replicas"}},
			}); err != nil {
				t.Errorf("racing model Update() error = %v", err)
			}
		}
	}
	updateSubscription := func() {
		object := gitSampleSubscription("team-a")
		object.Spec.Priority = 50
		if _, err := other.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
			Object:     object,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.priority"}},
		}); err != nil {
			t.Errorf("racing subscription Update() error = %v", err)
		}
	}

	writer.race = updateModel(2)
	if _, err := subscriptions.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create() racing a model update error = %v", err)
	}
	if writer.commits != 2 {
		t.Fatalf("Create commits = %d, want 2", writer.commits)
	}

	writer.commits = 0
	writer.race = updateModel(3)
	update := gitSampleSubscription("team-a")
	update.Metadata.Version = 1
	update.Spec.Priority = 20
	updated, err := subscriptions.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     update,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec"}},
		Lock:       true,
	})
	if err != nil {
		t.Fatalf("Update() racing a model update error = %v", err)
	}
	if updated.GetObject().GetMetadata().GetVersion() != 2 || writer.commits != 2 {
		t.Fatalf("Update version = %d, commits = %d; want version 2 after 2 commits", updated.GetObject().GetMetadata().GetVersion(), writer.commits)
	}

	// The model a new ref names is deleted meanwhile.
	writer.race = func() {
		if _, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "model-b"}); err != nil {
			t.Errorf("racing model Delete() error = %v", err)
		}
	}
	refs := gitSampleSubscription("team-a")
	refs.Spec.ModelRefs[0].Name = "model-b"
	_, err = subscriptions.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     refs,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.model_refs"}},
	})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)

	// The subscription itself changes meanwhile.
	writer.commits = 0
	writer.race = updateSubscription
	_, err = subscriptions.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a", ResourceVersion: 2})
	assertConnectCode(t, err, connect.CodeAborted)
	if errors.Is(err, gitops.ErrConflict) || writer.commits != 1 {
		t.Fatalf("Delete() error = %v after %d commits, want the rerun's version check, after 1", err, writer.commits)
	}
	writer.race = updateSubscription
	if _, err := subscriptions.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a"}); err != nil {
		t.Fatalf("Delete() without a version error = %v", err)
	}
	_, err = subscriptions.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	assertConnectCode(t, err, connect.CodeNotFound)
}

// conflictingWriter fails every Commit with ErrConflict, as when the branch
// moves before each push, and counts them.
type conflictingWriter struct {
	*gitops.MemoryWriter
	commits int
}

func (w *conflictingWriter) Commit(context.Context, string, gitops.Change) (string, error) {
	w.commits++
	return "", fmt.Errorf("gitops: push: %w", gitops.ErrConflict)
}

// Every write gives up after gitops.ConflictAttempts commits, with the
// Aborted error a single conflicting commit answered before.
func TestGitWritesGiveUpAfterConflictAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seed := gitops.NewMemoryWriter(nil)
	if _, err := mustGitModelsServer(t, seed).Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim-model", "site-a")}); err != nil {
		t.Fatalf("seed model error = %v", err)
	}
	if _, err := mustGitSubscriptionsServer(t, seed).Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("seed subscription error = %v", err)
	}
	writer := &conflictingWriter{MemoryWriter: seed}
	models := mustGitModelsServer(t, writer)
	subscriptions := mustGitSubscriptionsServer(t, writer)
	subUpdate := gitSampleSubscription("team-a")
	subUpdate.Spec.Priority = 20

	for _, tc := range []struct {
		name  string
		want  *connect.Error
		write func() error
	}{
		{"Models/Create", staleModelError("create", "model-b"), func() error {
			_, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("model-b", "site-a")})
			return err
		}},
		{"Models/Update", staleModelError("update", "sim-model"), func() error {
			_, err := models.Update(ctx, &aigridv1.ModelsUpdateRequest{
				Object:     gitSampleModel("sim-model", "site-b"),
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
			})
			return err
		}},
		{"Models/Delete", staleModelError("delete", "model-c"), func() error {
			// sim-model is referenced; a model nothing names reaches the
			// commit. Created on the seed, it is there for every attempt.
			if _, err := mustGitModelsServer(t, seed).Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("model-c", "site-a")}); err != nil {
				t.Fatalf("seed model-c error = %v", err)
			}
			_, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "model-c"})
			return err
		}},
		{"Subscriptions/Create", staleSubscriptionError("create", "team-b"), func() error {
			_, err := subscriptions.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-b")})
			return err
		}},
		{"Subscriptions/Update", staleSubscriptionError("update", "team-a"), func() error {
			_, err := subscriptions.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
				Object:     subUpdate,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.priority"}},
			})
			return err
		}},
		{"Subscriptions/Delete", staleSubscriptionError("delete", "team-a"), func() error {
			_, err := subscriptions.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a"})
			return err
		}},
	} {
		writer.commits = 0
		err := tc.write()
		assertConnectCode(t, err, connect.CodeAborted)
		if !errors.Is(err, gitops.ErrConflict) {
			t.Fatalf("%s error = %v, want the last conflict as its cause", tc.name, err)
		}
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Message() != tc.want.Message() {
			t.Fatalf("%s message = %q, want %q", tc.name, cerr.Message(), tc.want.Message())
		}
		if writer.commits != gitops.ConflictAttempts {
			t.Fatalf("%s commits = %d, want %d", tc.name, writer.commits, gitops.ConflictAttempts)
		}
	}
}
