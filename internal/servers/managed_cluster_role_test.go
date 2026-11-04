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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
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

// newRoleCatalog is an in-memory catalog in which sim-model exists, so that
// gitSampleSubscription can be created on it.
func newRoleCatalog() *gitops.MemoryWriter {
	return gitops.NewMemoryWriter(gitModelSeed("sim-model"))
}

// createSubscriptions commits one subscription per id to writer, a catalog
// from newRoleCatalog, through the real Subscriptions server.
func createSubscriptions(t *testing.T, writer gitops.Writer, ids ...string) {
	t.Helper()
	subscriptions := mustGitSubscriptionsServer(t, writer)
	for _, id := range ids {
		if _, err := subscriptions.Create(context.Background(), &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription(id)}); err != nil {
			t.Fatalf("Create subscription %s: %v", id, err)
		}
	}
}

// removeLabels is the label set a removal from the grid sends: the cluster's
// labels without ai-grid/enabled, its role kept.
func removeLabels(role string) map[string]string {
	labels := map[string]string{"location": "paris"}
	if role != "" {
		labels["ai-grid/role"] = role
	}
	return labels
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
// of a grid cluster, or its removal from the grid, that would revoke every
// subscription's keys, leave the grid without an ingress cluster or leave a
// placed model without its cluster. The catalog is the in-memory one; the
// models and subscriptions are written through the real Models and
// Subscriptions servers.
func TestUpdateClusterRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		clusters      []runtime.Object
		models        map[string][]string // model id: the clusters it is placed on
		subscriptions []string
		request       *aigridv1.ClustersUpdateRequest
		want          connect.Code // 0: the update succeeds and writes request's labels
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

		// Refused: every subscription's keys would be revoked on all
		// clusters. The same changes are allowed above without subscriptions.
		{
			name:          "combined to inference with another ingress cluster and subscriptions",
			clusters:      []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "ingress")},
			subscriptions: []string{"sub-a", "sub-b"},
			request:       labelsUpdate("site-a", roleLabels("inference")),
			want:          connect.CodeFailedPrecondition,
		},
		{
			name:          "ingress to inference with another ingress cluster and a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", roleLabels("inference")),
			want:          connect.CodeFailedPrecondition,
		},
		{
			name:          "the role of an ingress cluster removed with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "combined")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", map[string]string{"ai-grid/enabled": "true", "location": "paris"}),
			want:          connect.CodeFailedPrecondition,
		},
		{
			name:          "removing an ingress cluster with another ingress cluster and a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", removeLabels("ingress")),
			want:          connect.CodeFailedPrecondition,
		},
		{
			name:          "removing a combined cluster with another ingress cluster and a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", removeLabels("combined")),
			want:          connect.CodeFailedPrecondition,
		},
		{
			name:          "ai-grid/enabled false on an ingress cluster with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", map[string]string{"ai-grid/enabled": "false", "ai-grid/role": "ingress", "location": "paris"}),
			want:          connect.CodeFailedPrecondition,
		},
		{
			name:          "removing the grid's only cluster, an ingress one, with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", removeLabels("ingress")),
			want:          connect.CodeFailedPrecondition,
		},

		// Subscriptions don't hold a cluster that keeps serving ingress, or
		// never served it.
		{
			name:          "ingress to combined with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", roleLabels("combined")),
		},
		{
			name:          "combined to ingress with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "combined")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", roleLabels("ingress")),
		},
		{
			name:          "inference to ingress with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", roleLabels("ingress")),
		},
		{
			name:          "removing an inference cluster with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:        map[string][]string{"model-b": {"site-b"}},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", removeLabels("inference")),
		},
		{
			name:          "other labels of an ingress cluster with a subscription",
			clusters:      []runtime.Object{gridCluster("site-a", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", map[string]string{"ai-grid/enabled": "true", "ai-grid/role": "ingress", "location": "berlin"}),
		},

		// Removing a cluster from the grid without subscriptions.
		{
			name:     "removing an ingress cluster with another ingress cluster",
			clusters: []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", removeLabels("ingress")),
		},
		{
			name:     "removing a combined cluster with another ingress cluster and models elsewhere",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "combined")},
			models:   map[string][]string{"model-b": {"site-b"}},
			request:  labelsUpdate("site-a", removeLabels("combined")),
		},
		{
			name:     "removing the last ingress cluster while an inference cluster stays",
			clusters: []runtime.Object{gridCluster("site-a", "ingress"), gridCluster("site-b", "inference")},
			request:  labelsUpdate("site-a", removeLabels("ingress")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "removing the last ingress cluster while a cluster without a role stays",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "")},
			request:  labelsUpdate("site-a", removeLabels("combined")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name: "removing the grid's last cluster, an ingress one",
			clusters: []runtime.Object{
				gridCluster("site-a", "ingress"),
				managedCluster("site-b", map[string]string{"ai-grid/role": "inference"}, true),
			},
			request: labelsUpdate("site-a", removeLabels("ingress")),
		},

		// Removing a cluster from the grid with models placed on it, whatever
		// its role.
		{
			name:     "removing an inference cluster with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", removeLabels("inference")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "removing a combined cluster with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "combined"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a", "site-c"}},
			request:  labelsUpdate("site-a", removeLabels("combined")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "removing a cluster with a stored unknown role and a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "gateway"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", removeLabels("gateway")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "removing a cluster without a role but with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", ""), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", removeLabels("")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "removing an inference cluster and setting role ingress with a placed model",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "ingress")},
			models:   map[string][]string{"model-a": {"site-a"}},
			request:  labelsUpdate("site-a", removeLabels("ingress")),
			want:     connect.CodeFailedPrecondition,
		},
		{
			name:     "removing an inference cluster with models placed elsewhere only",
			clusters: []runtime.Object{gridCluster("site-a", "inference"), gridCluster("site-b", "combined")},
			models:   map[string][]string{"model-b": {"site-b"}},
			request:  labelsUpdate("site-a", removeLabels("inference")),
		},
		{
			name:     "removing a cluster without a role",
			clusters: []runtime.Object{gridCluster("site-a", ""), gridCluster("site-b", "ingress")},
			request:  labelsUpdate("site-a", removeLabels("")),
		},

		// No guard when the cluster isn't in the grid before the update.
		{
			name:          "joining the grid with a new role",
			clusters:      []runtime.Object{managedCluster("site-a", map[string]string{"ai-grid/role": "inference"}, true), gridCluster("site-b", "inference")},
			models:        map[string][]string{"model-a": {"site-a"}},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", roleLabels("ingress")),
		},
		{
			name:          "joining the grid as an ingress cluster with subscriptions",
			clusters:      []runtime.Object{managedCluster("site-a", nil, true), gridCluster("site-b", "ingress")},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", roleLabels("ingress")),
		},
		{
			name:          "a role change outside the grid",
			clusters:      []runtime.Object{managedCluster("site-a", map[string]string{"ai-grid/role": "combined"}, true)},
			models:        map[string][]string{"model-a": {"site-a"}},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", map[string]string{"ai-grid/role": "inference"}),
		},
		{
			name:          "ai-grid/enabled false to absent on an ingress cluster",
			clusters:      []runtime.Object{managedCluster("site-a", map[string]string{"ai-grid/enabled": "false", "ai-grid/role": "ingress"}, true)},
			models:        map[string][]string{"model-a": {"site-a"}},
			subscriptions: []string{"sub-a"},
			request:       labelsUpdate("site-a", map[string]string{"ai-grid/role": "ingress"}),
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
			writer := newRoleCatalog()
			placeModels(t, writer, tc.models)
			createSubscriptions(t, writer, tc.subscriptions...)
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
	assertConnectMessage(t, err, `Cluster "site-a" has 2 models placed on it (model-a, model-b). Move them to other clusters before changing it to a role without inference.`)

	placeModels(t, writer, map[string][]string{"m-1": {"site-c"}, "m-2": {"site-c"}, "m-3": {"site-c"}, "m-4": {"site-c"}, "m-5": {"site-c"}, "m-6": {"site-c"}, "m-7": {"site-c"}})
	server, err = NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-c", "combined"), gridCluster("site-b", "ingress"))).SetCatalog(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-c", roleLabels("ingress")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-c" has 7 models placed on it (m-1, m-2, m-3, m-4, m-5 and 2 more). Move them to other clusters before changing it to a role without inference.`)

	placeModels(t, writer, map[string][]string{"solo": {"site-d"}})
	server, err = NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-d", "inference"), gridCluster("site-b", "ingress"))).SetCatalog(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-d", roleLabels("ingress")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-d" has 1 model placed on it (solo). Move it to another cluster before changing it to a role without inference.`)

	server, err = NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-b", "ingress"), gridCluster("site-a", "inference"))).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-b", roleLabels("inference")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-b" is the only AI Grid cluster that serves ingress, and users reach models through ingress clusters. Give another cluster the ingress or combined role first.`)

	_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("gateway")))
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	assertConnectMessage(t, err, `label "ai-grid/role" must be ingress, inference or combined, got "gateway"`)

	_, err = server.Update(ctx, labelsUpdate("site-b", removeLabels("ingress")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-b" is the only AI Grid cluster that serves ingress, and users reach models through ingress clusters. Give another cluster the ingress or combined role first.`)
}

// Removing a cluster from the grid, and the subscriptions, have their own
// wording: what the admin is doing, how many there are, and what to do.
func TestUpdateClusterRemoveAndSubscriptionMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := newRoleCatalog()
	placeModels(t, writer, map[string][]string{"model-a": {"site-b"}, "model-b": {"site-b"}, "solo": {"site-c"}})
	server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(
		gridCluster("site-a", "ingress"), gridCluster("site-b", "inference"), gridCluster("site-c", "combined"), gridCluster("site-d", "ingress"),
	)).SetCatalog(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	_, err = server.Update(ctx, labelsUpdate("site-b", removeLabels("inference")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-b" has 2 models placed on it (model-a, model-b). Move them to other clusters before removing it from AI Grid.`)
	_, err = server.Update(ctx, labelsUpdate("site-c", removeLabels("combined")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-c" has 1 model placed on it (solo). Move it to another cluster before removing it from AI Grid.`)

	createSubscriptions(t, writer, "sub-a")
	_, err = server.Update(ctx, labelsUpdate("site-a", removeLabels("ingress")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-a" serves ingress, and 1 subscription exists (sub-a). When a cluster stops serving ingress, the API keys of every subscription are revoked on all clusters. Delete the subscription first, or keep the cluster in AI Grid.`)

	createSubscriptions(t, writer, "sub-b", "sub-c")
	_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("inference")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-a" serves ingress, and 3 subscriptions exist (sub-a, sub-b, sub-c). When a cluster stops serving ingress, the API keys of every subscription are revoked on all clusters. Delete the subscriptions first, or keep the ingress or combined role.`)

	createSubscriptions(t, writer, "sub-d", "sub-e", "sub-f", "sub-g")
	_, err = server.Update(ctx, labelsUpdate("site-d", roleLabels("inference")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	assertConnectMessage(t, err, `Cluster "site-d" serves ingress, and 7 subscriptions exist (sub-a, sub-b, sub-c, sub-d, sub-e and 2 more). When a cluster stops serving ingress, the API keys of every subscription are revoked on all clusters. Delete the subscriptions first, or keep the ingress or combined role.`)
}

// When several rules refuse one update, the first in checkRoleChange's
// order answers: the subscriptions, then the last ingress cluster, then the
// models placed on the cluster.
func TestUpdateClusterRoleRuleOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const (
		subscriptions = `Cluster "site-a" serves ingress, and 1 subscription exists (sub-a). When a cluster stops serving ingress, the API keys of every subscription are revoked on all clusters.`
		lastIngress   = `Cluster "site-a" is the only AI Grid cluster that serves ingress, and users reach models through ingress clusters. Give another cluster the ingress or combined role first.`
		models        = `Cluster "site-a" has 1 model placed on it (model-a). Move it to another cluster before`
	)
	noRole := map[string]string{"ai-grid/enabled": "true", "location": "paris"}
	for _, tc := range []struct {
		name          string
		subscriptions bool
		otherIngress  bool // site-b serves ingress; otherwise inference only
		models        bool
		request       *aigridv1.ClustersUpdateRequest
		want          string // part of the message
	}{
		{"subscriptions before the last ingress cluster", true, false, false, labelsUpdate("site-a", roleLabels("inference")), subscriptions},
		{"subscriptions before the placed models", true, true, true, labelsUpdate("site-a", noRole), subscriptions},
		{"all three on a role change", true, false, true, labelsUpdate("site-a", noRole), subscriptions},
		{"all three on removal", true, false, true, labelsUpdate("site-a", removeLabels("combined")), subscriptions},
		{"the last ingress cluster before the placed models", false, false, true, labelsUpdate("site-a", noRole), lastIngress},
		{"the last ingress cluster before the placed models on removal", false, false, true, labelsUpdate("site-a", removeLabels("combined")), lastIngress},
		{"the placed models alone", false, false, true, labelsUpdate("site-a", roleLabels("ingress")), models},
		{"the placed models alone on removal", false, true, true, labelsUpdate("site-a", removeLabels("combined")), models},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writer := newRoleCatalog()
			if tc.models {
				placeModels(t, writer, map[string][]string{"model-a": {"site-a"}})
			}
			if tc.subscriptions {
				createSubscriptions(t, writer, "sub-a")
			}
			// site-b keeps the grid from being empty without site-a.
			other := "inference"
			if tc.otherIngress {
				other = "ingress"
			}
			server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-a", "combined"), gridCluster("site-b", other))).SetCatalog(writer).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			_, err = server.Update(ctx, tc.request)
			assertConnectCode(t, err, connect.CodeFailedPrecondition)
			assertConnectMessage(t, err, tc.want)
		})
	}
}

// Removing a cluster and adding it back with another role no longer gets a
// placed model off it: the removal is refused like the role change, and the
// cluster stays in the grid with its role.
func TestUpdateClusterRemoveCannotSidestepPlacedModels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := newRoleCatalog()
	placeModels(t, writer, map[string][]string{"model-a": {"site-a"}})
	server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-a", "inference"), gridCluster("site-b", "ingress"))).SetCatalog(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("ingress")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	_, err = server.Update(ctx, labelsUpdate("site-a", removeLabels("inference")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	got, err := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.GetObject().GetStatus().GetAiGridMember() {
		t.Fatal("site-a left the grid; the removal must be refused")
	}
	if role := got.GetObject().GetMetadata().GetLabels()["ai-grid/role"]; role != "inference" {
		t.Fatalf("role = %q, want inference", role)
	}
}

// countingCatalog counts FreshHead calls.
type countingCatalog struct {
	gitops.Writer
	fresh int
}

func (c *countingCatalog) FreshHead(ctx context.Context) (gitops.Snapshot, error) {
	c.fresh++
	return c.Writer.FreshHead(ctx)
}

// An update that needs both the subscriptions and the placed models reads
// the catalog once: one fetch, one snapshot for both rules.
func TestUpdateClusterRoleReadsTheCatalogOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	catalog := &countingCatalog{Writer: newRoleCatalog()}
	placeModels(t, catalog.Writer, map[string][]string{"model-b": {"site-b"}})
	server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-a", "combined"), gridCluster("site-b", "combined"))).SetCatalog(catalog).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if _, err := server.Update(ctx, labelsUpdate("site-a", removeLabels("combined"))); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if catalog.fresh != 1 {
		t.Fatalf("FreshHead calls = %d, want 1", catalog.fresh)
	}
}

// The placed models and the subscriptions come from the catalog as the remote
// has it now, not from a snapshot the read window (AI_GRID_GIT_READ_MAX_AGE)
// serves: a model placed, or a subscription created, by another backend a
// moment ago, on a Git repository both use, refuses the change. Without the
// catalog, and with a backend started without a repository, there is
// nothing placed and no subscription; a repository not cloned yet can't be
// read, so the change is Unavailable rather than allowed unchecked.
//
// site-a serves inference, site-b and site-c ingress: removing site-b reads
// only the subscriptions, and changing site-a's role or removing it only the
// placed models.
func TestUpdateClusterRoleReadsTheCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clusters := func() *ClustersServerBuilder {
		return NewClustersServer().SetDynamicClient(newManagedClusterClient(
			gridCluster("site-a", "inference"), gridCluster("site-b", "ingress"), gridCluster("site-c", "ingress"),
		))
	}
	// open clones url with a read window that serves the clone's head for
	// the whole test.
	open := func(t *testing.T, url string) *gitops.RepoWriter {
		t.Helper()
		w, err := gitops.NewRepoWriter(ctx, gitops.RepoConfig{
			URL: url, Branch: "main", AuthorName: "ai-grid-provider-backend", AuthorEmail: "backend@example.com",
			ReadMaxAge: time.Hour,
		})
		if err != nil {
			t.Fatalf("NewRepoWriter() error = %v", err)
		}
		return w
	}

	t.Run("a model another writer placed within the read window", func(t *testing.T) {
		t.Parallel()
		url := "file://" + newBareCatalog(t)
		ours, other := open(t, url), open(t, url)
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
		_, err = server.Update(ctx, labelsUpdate("site-a", removeLabels("inference")))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
	})

	t.Run("a subscription another writer created within the read window", func(t *testing.T) {
		t.Parallel()
		url := "file://" + newBareCatalog(t)
		ours, other := open(t, url), open(t, url)
		server, err := clusters().SetCatalog(ours).Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		placeModels(t, other, map[string][]string{"sim-model": {"site-a"}})
		createSubscriptions(t, other, "sub-a")
		if head, err := ours.Head(ctx); err != nil {
			t.Fatalf("Head() error = %v", err)
		} else if files, err := head.List(subscriptionsGitDir); err != nil || len(files) != 0 {
			t.Fatalf("Head() within the read window lists subscriptions %v (err = %v); the test needs a lagging head", files, err)
		}
		_, err = server.Update(ctx, labelsUpdate("site-b", removeLabels("ingress")))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		_, err = server.Update(ctx, labelsUpdate("site-b", roleLabels("inference")))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
	})

	// Nothing refuses: site-a changes role and leaves, and so does site-b.
	unchecked := func(t *testing.T, server *ClustersServer) {
		t.Helper()
		for _, update := range []*aigridv1.ClustersUpdateRequest{
			labelsUpdate("site-a", roleLabels("ingress")),
			labelsUpdate("site-a", removeLabels("ingress")),
			labelsUpdate("site-b", removeLabels("ingress")),
		} {
			if _, err := server.Update(ctx, update); err != nil {
				t.Fatalf("Update(%s, %v) error = %v", update.GetObject().GetId(), update.GetObject().GetMetadata().GetLabels(), err)
			}
		}
	}

	t.Run("no catalog", func(t *testing.T) {
		t.Parallel()
		server, err := clusters().Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		unchecked(t, server)
	})

	t.Run("no repository configured", func(t *testing.T) {
		t.Parallel()
		server, err := clusters().SetCatalog(gitops.Unconfigured{}).Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		unchecked(t, server)
	})

	// Every update that reads the catalog is Unavailable, and the cluster
	// keeps its labels.
	unavailable := func(t *testing.T, server *ClustersServer) {
		t.Helper()
		before := map[string]map[string]string{}
		for _, id := range []string{"site-a", "site-b"} {
			got, err := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: id})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			before[id] = got.GetObject().GetMetadata().GetLabels()
		}
		for _, update := range []*aigridv1.ClustersUpdateRequest{
			labelsUpdate("site-a", roleLabels("ingress")),
			labelsUpdate("site-a", removeLabels("inference")),
			labelsUpdate("site-b", removeLabels("ingress")),
			labelsUpdate("site-b", roleLabels("inference")),
		} {
			id := update.GetObject().GetId()
			_, err := server.Update(ctx, update)
			assertConnectCode(t, err, connect.CodeUnavailable)
			got, err := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: id})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if labels := got.GetObject().GetMetadata().GetLabels(); !maps.Equal(labels, before[id]) {
				t.Fatalf("%s labels after a refused Update = %v, want %v", id, labels, before[id])
			}
		}
	}

	t.Run("the remote can't be fetched after the clone", func(t *testing.T) {
		t.Parallel()
		bare := newBareCatalog(t)
		writer := open(t, "file://"+bare)
		if err := os.RemoveAll(bare); err != nil {
			t.Fatal(err)
		}
		server, err := clusters().SetCatalog(writer).Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		unavailable(t, server)
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
		unavailable(t, server)
		// A change that stops the cluster serving neither inference nor
		// ingress doesn't read the catalog.
		if _, err := server.Update(ctx, labelsUpdate("site-a", roleLabels("combined"))); err != nil {
			t.Fatalf("Update() to combined error = %v", err)
		}
		if _, err := server.Update(ctx, labelsUpdate("site-b", roleLabels("combined"))); err != nil {
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

// Another ingress cluster that is being deleted is about to leave the grid,
// so it doesn't let the last other one stop serving ingress.
func TestUpdateClusterRoleIgnoresADeletingIngressCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deleting := managedCluster("site-b", roleLabels("ingress"), true)
	deleting.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	deleting.SetFinalizers([]string{"cluster.open-cluster-management.io/api-resource-cleanup"})
	server, err := NewClustersServer().SetDynamicClient(newManagedClusterClient(gridCluster("site-a", "ingress"), deleting)).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("inference")))
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}

// A caller the hub won't let list ManagedClusters gets PermissionDenied, as
// for the other ManagedCluster calls, and the cluster keeps its role.
func TestUpdateClusterRoleListDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newManagedClusterClient(gridCluster("site-a", "ingress"), gridCluster("site-b", "ingress"))
	client.PrependReactor("list", "managedclusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "cluster.open-cluster-management.io", Resource: "managedclusters"}, "", errors.New("no list"))
	})
	server, err := NewClustersServer().SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = server.Update(ctx, labelsUpdate("site-a", roleLabels("inference")))
	assertConnectCode(t, err, connect.CodePermissionDenied)
	got, err := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if role := got.GetObject().GetMetadata().GetLabels()["ai-grid/role"]; role != "ingress" {
		t.Fatalf("role after a refused Update = %q, want ingress", role)
	}
}
