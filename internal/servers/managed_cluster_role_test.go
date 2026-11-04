package servers

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/runtime"
)

// gridCluster is a ManagedCluster in the grid with role; an empty role
// leaves the label out.
func gridCluster(name, role string) runtime.Object {
	labels := map[string]string{"ai-grid/enabled": "true", "location": "paris"}
	if role != "" {
		labels["ai-grid/role"] = role
	}
	return managedCluster(name, labels, true)
}

// placeModels commits one model per id to writer, each placed on the
// clusters given, through the real Models server.
func placeModels(t *testing.T, writer gitops.Writer, placements map[string][]string) {
	t.Helper()
	models := mustGitModelsServer(t, writer)
	for id, clusters := range placements {
		if _, err := models.Create(context.Background(), &aigridv1.ModelsCreateRequest{Object: gitSampleModel(id, clusters...)}); err != nil {
			t.Fatalf("Create model %s: %v", id, err)
		}
	}
}

// labelsUpdate is a labels-only Update of cluster id to labels, the whole
// console set as the console sends it.
func labelsUpdate(id string, labels map[string]string) *aigridv1.ClustersUpdateRequest {
	return &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: id, Metadata: &aigridv1.Metadata{Labels: labels}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
	}
}

func roleLabels(role string) map[string]string {
	return map[string]string{"ai-grid/enabled": "true", "ai-grid/role": role, "location": "paris"}
}

// Clusters/Update checks an ai-grid/role it writes, and refuses a role change
// of a grid cluster that would leave a placed model without inference or the
// grid without an ingress cluster. The catalog is the in-memory one; the
// models are placed through the real Models server.
func TestUpdateClusterRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		clusters []runtime.Object
		models   map[string][]string // model id: the clusters it is placed on
		request  *aigridv1.ClustersUpdateRequest
		want     connect.Code // 0: the update succeeds and writes request's labels
	}{
		// The role's value.
		{
			name:     "unknown role on a grid cluster",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", roleLabels("gateway")),
			want:     connect.CodeInvalidArgument,
		},
		{
			name:     "role in another case",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", roleLabels("Ingress")),
			want:     connect.CodeInvalidArgument,
		},
		{
			name:     "empty role",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", roleLabels("")),
			want:     connect.CodeInvalidArgument,
		},
		{
			name:     "unknown role on a cluster joining the grid",
			clusters: []runtime.Object{managedCluster("site-a", nil, true)},
			request:  labelsUpdate("site-a", roleLabels("both")),
			want:     connect.CodeInvalidArgument,
		},
		{
			name:     "unknown role on a cluster outside the grid",
			clusters: []runtime.Object{managedCluster("site-a", nil, true)},
			request:  labelsUpdate("site-a", map[string]string{"ai-grid/role": "gateway"}),
			want:     connect.CodeInvalidArgument,
		},
		{
			name:     "no role on a cluster joining the grid",
			clusters: []runtime.Object{managedCluster("site-a", nil, true)},
			request:  labelsUpdate("site-a", map[string]string{"ai-grid/enabled": "true"}),
		},
		{
			name:     "a stored unknown role the request leaves as it is",
			clusters: []runtime.Object{gridCluster("site-a", "gateway"), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", map[string]string{"ai-grid/enabled": "true", "ai-grid/role": "gateway", "location": "berlin"}),
		},

		// Changes that take nothing away.
		{
			name:     "ingress to combined, the last ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "inference")},
			models:   map[string][]string{"model-b": {"site-b"}},
			request:  labelsUpdate("site-a", roleLabels("combined")),
		},
		{
			name:     "inference to combined with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", roleLabels("combined")),
		},
		{
			name:     "no role to inference",
			clusters: []runtime.Object{gridCluster("site-a", ""), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", roleLabels("inference")),
		},

		// Changes that are safe as the grid is.
		{
			name:     "combined to ingress with models placed elsewhere only",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "inference")},
			models:   map[string][]string{"model-b": {"site-b"}},
			request:  labelsUpdate("site-a", roleLabels("ingress")),
		},
		{
			name:     "combined to inference with another ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", roleLabels("inference")),
		},
		{
			name:     "combined to inference with another combined cluster",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "combined")},
			request:  labelsUpdate("site-a", roleLabels("inference")),
		},
		{
			name:     "inference to ingress with no models placed",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "inference")},
			models:   map[string][]string{"model-b": {"site-b"}},
			request:  labelsUpdate("site-a", roleLabels("ingress")),
		},
		{
			name:     "ingress to inference with another ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", roleLabels("inference")),
		},

		// Refused: a placed model would lose its inference cluster.
		{
			name:     "inference to ingress with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a", "site-c"}},
			request:  labelsUpdate("site-a", roleLabels("ingress")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "combined to ingress with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", roleLabels("ingress")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "the role removed with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", map[string]string{"ai-grid/enabled": "true", "location": "paris"}),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "inference to an unknown role is InvalidArgument first",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", roleLabels("gateway")),
			want:     connect.CodeInvalidArgument,
		},

		// Refused: the grid would have no ingress cluster.
		{
			name:     "ingress to inference, the last ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "inference")},
			request:  labelsUpdate("site-a", roleLabels("inference")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "combined to inference, the last ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "inference")},
			request:  labelsUpdate("site-a", roleLabels("inference")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name: "an ingress cluster outside the grid doesn't count",
			clusters: []runtime.Object{
				gridCluster("site-a", "ingress"),
				managedCluster("site-b", map[string]string{"ai-grid/role": "ingress"}, true),
				managedCluster("site-c", map[string]string{"ai-grid/enabled": "false", "ai-grid/role": "combined"}, true),
			},
			request: labelsUpdate("site-a", roleLabels("inference")),
			want:    connect.CodeFailedPrecondition,
		},

		// No guard when the cluster isn't in the grid before or after.
		{
			name:     "joining the grid with a new role",
			clusters: []runtime.Object{managedCluster("site-a", map[string]string{"ai-grid/role": "inference"}, true), gridCluster("site-b", "inference")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", roleLabels("ingress")),
		},
		{
			name:     "leaving the grid with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", map[string]string{"location": "paris"}),
		},
		{
			name:     "leaving the grid as the last ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "inference")},
			request:  labelsUpdate("site-a", map[string]string{"ai-grid/role": "ingress", "location": "paris"}),
		},
		{
			name:     "a role change outside the grid",
			clusters: []runtime.Object{managedCluster("site-a", map[string]string{"ai-grid/role": "combined"}, true)},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", map[string]string{"ai-grid/role": "ingress"}),
		},

		// The role as it is, other fields changed.
		{
			name:     "other labels of the last ingress cluster with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "combined")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request: labelsUpdate("site-a", map[string]string{
				"ai-grid/enabled": "true", "ai-grid/role": "combined", "location": "berlin",
				"ai-grid/site": "berlin-dc1", "ai-grid/region": "europe-central",
			}),
		},
		{
			name:     "hubAcceptsClient of the last ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "ingress")},
			request: &aigridv1.ClustersUpdateRequest{
				Object:     &aigridv1.ManagedCluster{Id: "site-a", Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: false}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.hub_accepts_client"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			writer := gitops.NewMemoryWriter(nil)
			placeModels(t, writer, tc.models)
			client := newManagedClusterClient(tc.clusters...)
			server, err := NewClustersServer().SetDynamicClient(client).SetCatalog(writer).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			before, err := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-a"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			got, err := server.Update(ctx, tc.request)
			if tc.want != 0 {
				assertConnectCode(t, err, tc.want)
				after, err := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-a"})
				if err != nil {
					t.Fatalf("Get() error = %v", err)
				}
				if !maps.Equal(after.GetObject().GetMetadata().GetLabels(), before.GetObject().GetMetadata().GetLabels()) {
					t.Fatalf("labels after a refused Update = %v, want %v", after.GetObject().GetMetadata().GetLabels(), before.GetObject().GetMetadata().GetLabels())
				}
				return
			}
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if labels := tc.request.GetObject().GetMetadata().GetLabels(); labels != nil {
				if gotLabels := got.GetObject().GetMetadata().GetLabels(); !maps.Equal(gotLabels, labels) {
					t.Fatalf("labels = %v, want %v", gotLabels, labels)
				}
			}
		})
	}
}

// The role checks sit behind the version check: a stale version is Aborted
// before the role is looked at; at the current version a refused change is
// refused, and an allowed one is written.
func TestUpdateClusterRoleKeepsTheVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := managedCluster("site-a", roleLabels("ingress"), true)
	cluster.SetResourceVersion("7")
	server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(cluster, gridCluster("site-b", "inference"))).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	at := func(version int32, role string) *aigridv1.ClustersUpdateRequest {
		req := labelsUpdate("site-a", roleLabels(role))
		req.Object.Metadata.Version = version
		req.Lock = true
		return req
	}
	_, err = server.Update(ctx, at(6, "gateway"))
	assertConnectCode(t, err, connect.CodeAborted)
	_, err = server.Update(ctx, at(6, "inference"))
	assertConnectCode(t, err, connect.CodeAborted)
	_, err = server.Update(ctx, at(7, "inference"))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)

	got, err := server.Update(ctx, at(7, "combined"))
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if role := got.GetObject().GetMetadata().GetLabels()["ai-grid/role"]; role != "combined" {
		t.Fatalf("role = %q, want combined", role)
	}
}

// Location, site and coordinates change on a grid cluster as before, on one
// whose role can't change: the checks look at the role, not at whether the
// request sends labels.
func TestUpdateGridClusterLocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	placeModels(t, writer, map[string][]string{"model-a": {"site-a"}})
	server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-a", "combined"))).SetCatalog(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	update := func(role string) *aigridv1.ClustersUpdateRequest {
		return &aigridv1.ClustersUpdateRequest{
			Object: &aigridv1.ManagedCluster{Id: "site-a", Metadata: &aigridv1.Metadata{
				Labels:      map[string]string{"ai-grid/enabled": "true", "ai-grid/role": role, "location": "berlin", "ai-grid/site": "berlin-dc1"},
				Annotations: map[string]string{"ai-grid/latitude": "52.52", "ai-grid/longitude": "13.405", "ai-grid/site-name": "Berlin"},
			}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata"}},
		}
	}
	for _, role := range []string{"inference", "ingress"} {
		_, err := server.Update(ctx, update(role))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
	}
	labels, annotations := update("combined").Object.Metadata.Labels, update("combined").Object.Metadata.Annotations
	got, err := server.Update(ctx, update("combined"))
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !maps.Equal(got.GetObject().GetMetadata().GetLabels(), labels) {
		t.Fatalf("labels = %v, want %v", got.GetObject().GetMetadata().GetLabels(), labels)
	}
	if !maps.Equal(got.GetObject().GetMetadata().GetAnnotations(), annotations) {
		t.Fatalf("annotations = %v, want %v", got.GetObject().GetMetadata().GetAnnotations(), annotations)
	}
}

// The refusals say which cluster, which models and what to do: the console
// shows the message to the provider admin.
func TestUpdateClusterRoleMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	writer := gitops.NewMemoryWriter(nil)
	placeModels(t, writer, map[string][]string{"model-a": {"site-a"}, "model-b": {"site-a", "site-b"}})
	server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-a", "inference"), gridCluster("site-b", "ingress"))).SetCatalog(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("ingress")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `cluster "site-a" cannot stop serving inference while 2 models are placed on it (model-a, model-b); move them to other clusters first`)

	placeModels(t, writer, map[string][]string{"m-1": {"site-c"}, "m-2": {"site-c"}, "m-3": {"site-c"}, "m-4": {"site-c"}, "m-5": {"site-c"}, "m-6": {"site-c"}, "m-7": {"site-c"}})
	server, err = NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-c", "combined"), gridCluster("site-b", "ingress"))).SetCatalog(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-c", roleLabels("ingress")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `cluster "site-c" cannot stop serving inference while 7 models are placed on it (m-1, m-2, m-3, m-4, m-5 and 2 more); move them to other clusters first`)

	server, err = NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-b", "ingress"), gridCluster("site-a", "inference"))).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-b", roleLabels("inference")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `cluster "site-b" is the last cluster in the grid that serves ingress, and users reach models through ingress clusters; give another cluster role ingress or combined before this one stops serving ingress`)

	_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("gateway")))
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	assertConnectMessage(t, err, `label "ai-grid/role" must be ingress, inference or combined, got "gateway"`)
}

// The placed models come from the catalog as the remote has it now, not from
// a snapshot the read window (AI_GRID_GIT_READ_MAX_AGE) serves: a model
// another backend placed a moment ago, on a Git repository both use, refuses
// the change. Without the catalog, and with a backend started without a
// repository, there is nothing placed; a repository not cloned yet can't be
// read, so the change is Unavailable rather than allowed unchecked.
func TestUpdateClusterRoleReadsTheCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clusters := func() *ClustersServerBuilder {
		return NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")))
	}

	t.Run("a model another writer placed within the read window", func(t *testing.T) {
		t.Parallel()
		url := "file://" + newBareCatalog(t)
		open := func() *gitops.RepoWriter {
			t.Helper()
			w, err := gitops.NewRepoWriter(ctx, gitops.RepoConfig{
				URL: url, Branch: "main", AuthorName: "ai-grid-provider-backend", AuthorEmail: "backend@example.com",
				// Head serves the clone's snapshot for the whole test.
				ReadMaxAge: time.Hour,
			})
			if err != nil {
				t.Fatalf("NewRepoWriter() error = %v", err)
			}
			return w
		}
		ours, other := open(), open()
		server, err := clusters().SetCatalog(ours).Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		placeModels(t, other, map[string][]string{"model-a": {"site-a"}})
		if head, err := ours.Head(ctx); err != nil {
			t.Fatalf("Head() error = %v", err)
		} else if _, err := head.Read(clusterGitIndex("site-a")); !errors.Is(err, gitops.ErrNotFound) {
			t.Fatalf("Head() within the read window has the other writer's index (err = %v); the test needs a lagging head", err)
		}
		_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("ingress")))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
	})

	t.Run("no catalog", func(t *testing.T) {
		t.Parallel()
		server, err := clusters().Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if _, err := server.Update(ctx, labelsUpdate("site-a", roleLabels("ingress"))); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	})

	t.Run("no repository configured", func(t *testing.T) {
		t.Parallel()
		server, err := clusters().SetCatalog(gitops.Unconfigured{}).Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if _, err := server.Update(ctx, labelsUpdate("site-a", roleLabels("ingress"))); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	})

	t.Run("repository not cloned yet", func(t *testing.T) {
		t.Parallel()
		writer := gitops.StartBackgroundWriter(ctx, func(context.Context) (gitops.Writer, error) {
			return nil, errors.New("clone: host unreachable")
		}, time.Hour)
		server, err := clusters().SetCatalog(writer).Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("ingress")))
		assertConnectCode(t, err, connect.CodeUnavailable)
		// A change that doesn't stop the cluster serving inference doesn't
		// read the catalog.
		if _, err := server.Update(ctx, labelsUpdate("site-a", roleLabels("combined"))); err != nil {
			t.Fatalf("Update() to combined error = %v", err)
		}
	})
}

// newBareCatalog returns the path of a bare repository whose main branch has
// one commit, for a file:// catalog served in-process.
func newBareCatalog(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "catalog.git")
	if _, err := git.PlainInitWithOptions(bare, &git.PlainInitOptions{
		Bare:        true,
		InitOptions: git.InitOptions{DefaultBranch: "refs/heads/main"},
	}); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	seed := filepath.Join(root, "seed")
	repo, err := git.PlainInitWithOptions(seed, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: "refs/heads/main"}})
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("catalog\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add: %v", err)
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	if _, err := wt.Commit("seed", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatalf("remote: %v", err)
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	return bare
}
