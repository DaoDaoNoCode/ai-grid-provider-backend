package draft

import (
	"context"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

func cond(typ string, st aigriddraftv1.ConditionStatus, reason, message string) *aigriddraftv1.Condition {
	return &aigriddraftv1.Condition{Type: typ, Status: st, Reason: proto.String(reason), Message: proto.String(message), LastTransitionTime: timestamppb.New(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))}
}

// withConditions clones the conditions so the package-level templates below
// are never aliased into a server's store.
func withConditions(conds ...*aigriddraftv1.Condition) *aigriddraftv1.ManagedCluster {
	owned := make([]*aigriddraftv1.Condition, 0, len(conds))
	for _, cd := range conds {
		owned = append(owned, proto.Clone(cd).(*aigriddraftv1.Condition))
	}
	return &aigriddraftv1.ManagedCluster{Name: "c", HubAcceptsClient: true, Status: &aigriddraftv1.ManagedClusterStatus{Conditions: owned}}
}

var (
	accepted    = cond(CondHubAccepted, cTrue, ReasonHubAccepted, MessageHubAccepted)
	joined      = cond(CondJoined, cTrue, ReasonJoined, MessageJoined)
	clockSynced = cond(CondClockSynced, cTrue, ReasonClockSynced, MessageClockSynced)
	imported    = cond(CondImportSucceeded, cTrue, ReasonImported, MessageImported)
	available   = cond(CondAvailable, cTrue, ReasonAvailable, MessageAvailable)
)

// TestDeriveState walks the ACM console's derivation with the exact condition
// inputs for each of the eight states (plus "no conditions").
func TestDeriveState(t *testing.T) {
	detaching := withConditions(cond(CondImportSucceeded, cFalse, ReasonDetaching, MessageDetaching), accepted, joined, available, clockSynced)
	detaching.DeletionTimestamp = timestamppb.Now()
	denied := withConditions(cond(CondHubAccepted, cFalse, ReasonHubDenied, MessageHubDenied), joined, available)
	denied.HubAcceptsClient = false
	apiDown := "The kube-apiserver is not ok, status code: 503, the server is currently unable to handle the request"

	cases := []struct {
		name    string
		c       *aigriddraftv1.ManagedCluster
		want    aigriddraftv1.ManagedClusterState
		message string
	}{
		{"detaching wins over everything", detaching, aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_DETACHING, MessageDetaching},
		{"no conditions", withConditions(), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNSPECIFIED, ""},
		{"denied by hub admin", denied, aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_NOT_ACCEPTED, MessageHubDenied},
		{"accepted condition absent", withConditions(joined, available), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_NOT_ACCEPTED, ""},
		{"accepted, import not started", withConditions(accepted, cond(CondImportSucceeded, cFalse, ReasonWaitForImporting, MessageWaitForImporting)), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_PENDING_IMPORT, MessageWaitForImporting},
		{"accepted, nothing else", withConditions(accepted), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_PENDING_IMPORT, ""},
		{"importing", withConditions(accepted, cond(CondImportSucceeded, cFalse, ReasonImporting, MessageImportApplied)), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORTING, MessageImportApplied},
		{"import failed", withConditions(accepted, cond(CondImportSucceeded, cFalse, ReasonImportFailed, "Try to import managed cluster, error: timeout")), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORT_FAILED, "Try to import managed cluster, error: timeout"},
		{"ready", withConditions(imported, accepted, joined, available, clockSynced), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE, MessageAvailable},
		{"joined but import not finished is still ready", withConditions(cond(CondImportSucceeded, cFalse, ReasonImporting, MessageImportApplied), accepted, joined, available), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE, MessageAvailable},
		{"offline", withConditions(imported, accepted, joined, cond(CondAvailable, cFalse, ReasonAPIServerUnavail, apiDown)), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNAVAILABLE, apiDown},
		{"unknown, lease expired", withConditions(imported, accepted, joined, cond(CondAvailable, cUnknown, ReasonLeaseStopped, MessageLeaseStopped)), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNKNOWN, MessageLeaseStopped},
		{"unknown, available condition missing", withConditions(imported, accepted, joined), aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNKNOWN, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := deriveState(tc.c)
			if got != tc.want || msg != tc.message {
				t.Errorf("deriveState = %s %q, want %s %q", got, msg, tc.want, tc.message)
			}
		})
	}
}

func TestReadsRecomputeStateFromConditions(t *testing.T) {
	// A fixture that claims to be AVAILABLE while its conditions say offline.
	c := withConditions(imported, accepted, joined, cond(CondAvailable, cFalse, ReasonAPIServerUnavail, "The kube-apiserver is not ok, status code: 500, x"))
	c.Status.State = aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE
	c.Status.StateMessage = "lies"
	s, err := New(&Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{c}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetManagedCluster(authed(context.Background()), &aigriddraftv1.GetManagedClusterRequest{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetObject().GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNAVAILABLE || !strings.HasPrefix(got.GetObject().GetStatus().GetStateMessage(), "The kube-apiserver is not ok") {
		t.Errorf("state must be recomputed on read, got %v", got.GetObject().GetStatus())
	}
}

// newServer builds a server whose hub RBAC allows writes, as Load defaults
// access.json; tests about denial pass their own Access to New.
func newServer(t *testing.T, fx *Fixtures) *Server {
	t.Helper()
	if len(fx.Access.DeniedTokens) == 0 && len(fx.Access.RejectedTokens) == 0 {
		fx.Access.DefaultAllowed = true
	}
	s, err := New(fx, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readyCluster(name string) *aigriddraftv1.ManagedCluster {
	c := withConditions(imported, accepted, joined, available, clockSynced)
	c.Name = name
	c.Labels = map[string]string{LabelName: name, LabelCloud: "BareMetal", LabelVendor: "OpenShift", LabelAddonPrefix + "work-manager": AddonAvailable}
	c.Annotations = map[string]string{"open-cluster-management/created-via": "other"}
	c.ResourceVersion = "100"
	return c
}

func TestRequiresAuthorizationExceptPing(t *testing.T) {
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
	anon := context.Background()
	if _, err := s.Ping(anon, &aigriddraftv1.PingRequest{Name: "probe"}); err != nil {
		t.Errorf("Ping is anonymous, got %v", err)
	}
	if _, err := s.ListManagedClusters(anon, &aigriddraftv1.ListManagedClustersRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("List without authorization = %v, want Unauthenticated", err)
	}
	if _, err := s.GetManagedCluster(anon, &aigriddraftv1.GetManagedClusterRequest{Name: "a"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Get without authorization = %v, want Unauthenticated", err)
	}
	if _, err := s.ListTenants(anon, &aigriddraftv1.ListTenantsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("ListTenants without authorization = %v, want Unauthenticated", err)
	}
	// x-forwarded-user alone grants nothing (advisory header).
	onlyUser := metadata.NewIncomingContext(anon, metadata.Pairs("x-forwarded-user", "alice"))
	if _, err := s.ListManagedClusters(onlyUser, &aigriddraftv1.ListManagedClustersRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("x-forwarded-user alone = %v, want Unauthenticated", err)
	}
	notBearer := metadata.NewIncomingContext(anon, metadata.Pairs("authorization", "Basic abc"))
	if _, err := s.ListManagedClusters(notBearer, &aigriddraftv1.ListManagedClustersRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("non-bearer authorization = %v, want Unauthenticated", err)
	}
	upd := &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}
	if _, err := s.UpdateManagedCluster(anon, upd); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Update without authorization = %v, want Unauthenticated", err)
	}
}

// TestUpdateHonoursHubRBAC: the fake answers a mutating RPC the way the hub
// answers the BFF's access review for the same token (access.json), so a
// viewer cannot enrol a cluster and an expired token is Unauthenticated.
func TestUpdateHonoursHubRBAC(t *testing.T) {
	access := Access{DefaultAllowed: true, DeniedTokens: []string{"viewer-token"}, RejectedTokens: []string{"expired-token"}}
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}, Access: access})
	as := func(token, user string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token, "x-forwarded-user", user))
	}
	upd := func() *aigriddraftv1.UpdateManagedClusterRequest {
		return &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Labels: labelsWith(readyCluster("a"), "ai-grid/enabled", "true")}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}
	}
	_, err := s.UpdateManagedCluster(as("viewer-token", "viewer"), upd())
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer update = %v, want PermissionDenied", err)
	}
	want := `managedclusters.cluster.open-cluster-management.io "a" is forbidden: User "viewer" cannot patch resource "managedclusters" in API group "cluster.open-cluster-management.io" at the cluster scope`
	if got := status.Convert(err).Message(); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if _, err := s.UpdateManagedCluster(as("expired-token", "ghost"), upd()); status.Code(err) != codes.Unauthenticated {
		t.Errorf("rejected token = %v, want Unauthenticated", err)
	}
	// Reads are not gated by patch RBAC and the store is untouched.
	got, err := s.GetManagedCluster(as("viewer-token", "viewer"), &aigriddraftv1.GetManagedClusterRequest{Name: "a"})
	if err != nil || got.GetObject().GetLabels()["ai-grid/enabled"] != "" {
		t.Errorf("viewer read = %v, %v", got, err)
	}
	if _, err := s.ListManagedClusters(as("viewer-token", "viewer"), &aigriddraftv1.ListManagedClustersRequest{}); err != nil {
		t.Errorf("viewer list = %v", err)
	}
	// A rejected token is an expired session: the hub answers 401 to every call, reads included.
	for name, call := range map[string]func(context.Context) error{
		"List": func(ctx context.Context) error {
			_, err := s.ListManagedClusters(ctx, &aigriddraftv1.ListManagedClustersRequest{})
			return err
		},
		"Get": func(ctx context.Context) error {
			_, err := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: "a"})
			return err
		},
		"ListTenants": func(ctx context.Context) error {
			_, err := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{})
			return err
		},
	} {
		if err := call(as("expired-token", "ghost")); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s with a rejected token = %v, want Unauthenticated", name, err)
		}
	}
	if resp, err := s.UpdateManagedCluster(as("dev-token", "dev-user"), upd()); err != nil || resp.GetObject().GetLabels()["ai-grid/enabled"] != "true" {
		t.Errorf("admin update = %v, %v", resp, err)
	}
	denyAll, err := New(&Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}, Access: Access{DefaultAllowed: false}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := denyAll.UpdateManagedCluster(as("dev-token", "dev-user"), upd()); status.Code(err) != codes.PermissionDenied {
		t.Errorf("defaultAllowed=false = %v, want PermissionDenied", err)
	}
}

func TestSetConditionKeepsTransitionTimeWhenStatusUnchanged(t *testing.T) {
	c := readyCluster("a")
	first := findCondition(c, CondAvailable).GetLastTransitionTime().AsTime()
	later := first.Add(time.Hour)
	setCondition(c, later, CondAvailable, cTrue, ReasonAvailable, MessageAvailable)
	if got := findCondition(c, CondAvailable).GetLastTransitionTime().AsTime(); !got.Equal(first) {
		t.Errorf("same status: lastTransitionTime moved to %v", got)
	}
	setCondition(c, later, CondAvailable, cFalse, ReasonAPIServerUnavail, "The kube-apiserver is not ok, status code: 503, x")
	if got := findCondition(c, CondAvailable).GetLastTransitionTime().AsTime(); !got.Equal(later) {
		t.Errorf("status change: lastTransitionTime = %v, want %v", got, later)
	}
	if findCondition(c, CondAvailable).GetObservedGeneration() != 0 {
		t.Error("the hub does not write observedGeneration on ManagedCluster conditions")
	}
}

func TestConcurrentLockedUpdatesAdmitOne(t *testing.T) {
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
	ctx := authed(context.Background())
	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{
				Object:     &aigriddraftv1.ManagedCluster{Name: "a", ResourceVersion: "100", Labels: labelsWith(readyCluster("a"), "ai-grid/writer", strconv.Itoa(i))},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
				Lock:       true,
			})
		}()
	}
	wg.Wait()
	ok, aborted := 0, 0
	for _, err := range results {
		switch status.Code(err) {
		case codes.OK:
			ok++
		case codes.Aborted:
			aborted++
		default:
			t.Errorf("unexpected %v", err)
		}
	}
	if ok != 1 || aborted != n-1 {
		t.Errorf("ok=%d aborted=%d, want 1 and %d", ok, aborted, n-1)
	}
}

func TestUpdateDetachingClusterKeepsState(t *testing.T) {
	c := readyCluster("a")
	c.DeletionTimestamp = timestamppb.New(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	setCondition(c, time.Now(), CondImportSucceeded, cFalse, ReasonDetaching, MessageDetaching)
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{c}})
	resp, err := s.UpdateManagedCluster(authed(context.Background()), &aigriddraftv1.UpdateManagedClusterRequest{
		Object: &aigriddraftv1.ManagedCluster{Name: "a", Labels: labelsWith(c, "ai-grid/enabled", "true")}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetObject(); got.GetLabels()["ai-grid/enabled"] != "true" || got.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_DETACHING || got.GetDeletionTimestamp() == nil {
		t.Errorf("labels still change on a terminating object and it stays DETACHING: %v", got)
	}
}

func TestGetManagedClusterNotFound(t *testing.T) {
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
	_, err := s.GetManagedCluster(authed(context.Background()), &aigriddraftv1.GetManagedClusterRequest{Name: "nope"})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "object with identifier 'nope' not found" {
		t.Errorf("Get missing = %v", err)
	}
}

func TestListPaging(t *testing.T) {
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("c"), readyCluster("a"), readyCluster("b")}})
	ctx := authed(context.Background())
	resp, err := s.ListManagedClusters(ctx, &aigriddraftv1.ListManagedClustersRequest{Offset: proto.Int32(1), Limit: proto.Int32(1)})
	if err != nil || len(resp.GetClusters()) != 1 || resp.GetClusters()[0].GetName() != "b" || resp.GetSize() != 1 || resp.GetTotal() != 3 {
		t.Fatalf("page = %v, %v", resp, err)
	}
	if resp, err := s.ListManagedClusters(ctx, &aigriddraftv1.ListManagedClustersRequest{Limit: proto.Int32(0)}); err != nil || len(resp.GetClusters()) != 0 || resp.GetTotal() != 3 {
		t.Errorf("limit 0 counts only: %v, %v", resp, err)
	}
	if _, err := s.ListManagedClusters(ctx, &aigriddraftv1.ListManagedClustersRequest{Limit: proto.Int32(-1)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("negative limit = %v, want InvalidArgument", err)
	}
	if _, err := s.ListManagedClusters(ctx, &aigriddraftv1.ListManagedClustersRequest{Filter: proto.String(`this.name == "a"`)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("filter = %v, want InvalidArgument until implemented", err)
	}
}

func TestUpdateManagedCluster(t *testing.T) {
	ctx := authed(context.Background())
	update := func(s *Server, req *aigriddraftv1.UpdateManagedClusterRequest) (*aigriddraftv1.ManagedCluster, error) {
		resp, err := s.UpdateManagedCluster(ctx, req)
		return resp.GetObject(), err
	}
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

	t.Run("mask validation", func(t *testing.T) {
		s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
		_, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a"}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("empty mask = %v", err)
		}
		_, err = update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a"}, UpdateMask: mask("labels", "status.conditions")})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), `"status.conditions"`) {
			t.Errorf("bad path must be InvalidArgument naming the path, got %v", err)
		}
		_, err = update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a"}, UpdateMask: mask("labels.ai-grid/enabled")})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("per-key map path = %v, want InvalidArgument", err)
		}
		_, err = update(s, &aigriddraftv1.UpdateManagedClusterRequest{UpdateMask: mask("labels")})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("missing name = %v", err)
		}
		_, err = update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "zz"}, UpdateMask: mask("labels")})
		if status.Code(err) != codes.NotFound {
			t.Errorf("unknown cluster = %v", err)
		}
	})

	t.Run("whole-map replacement and version bump", func(t *testing.T) {
		s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
		// A map that drops cloud and vendor would remove labels the hub does
		// not put back (autodetect fills them only while they read
		// auto-detect): outside the console's keys, so refused.
		_, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{
			Object:     &aigriddraftv1.ManagedCluster{Name: "a", Labels: map[string]string{"ai-grid/enabled": "true"}},
			UpdateMask: mask("labels"),
		})
		mustCode(t, err, codes.InvalidArgument, `label "cloud" cannot be changed through the console; only ai-grid/* labels and annotations and the location label can`)
		// The stored map with one key added, `name` left out: the map is
		// replaced as a whole, then the hub puts back what its webhook and
		// controllers own (name, the default clusterset, the add-on labels).
		sent := labelsWith(readyCluster("a"), "ai-grid/enabled", "true")
		delete(sent, LabelName)
		got, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{
			Object:     &aigriddraftv1.ManagedCluster{Name: "a", Labels: sent},
			UpdateMask: mask("labels"),
		})
		if err != nil {
			t.Fatal(err)
		}
		wantLabels := map[string]string{"ai-grid/enabled": "true", LabelName: "a", LabelCloud: "BareMetal", LabelVendor: "OpenShift", LabelClusterSet: ClusterSetDefault, LabelAddonPrefix + "work-manager": AddonAvailable}
		if !maps.Equal(got.GetLabels(), wantLabels) {
			t.Errorf("labels = %v, want %v", got.GetLabels(), wantLabels)
		}
		if got.GetAnnotations()["open-cluster-management/created-via"] != "other" {
			t.Errorf("annotations were not in the mask and must be untouched, got %v", got.GetAnnotations())
		}
		if got.GetResourceVersion() == "100" || got.GetResourceVersion() == "" {
			t.Errorf("resource version must change on write, got %q", got.GetResourceVersion())
		}
		if got.GetGeneration() != 0 {
			t.Errorf("metadata changes do not bump generation, got %d", got.GetGeneration())
		}
		if got.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE {
			t.Errorf("response carries the derived state, got %s", got.GetStatus().GetState())
		}
		_, err = update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Annotations: map[string]string{"x": "y"}}, UpdateMask: mask("annotations")})
		mustCode(t, err, codes.InvalidArgument, `annotation "x" cannot be changed through the console; only ai-grid/* labels and annotations and the location label can`)
		got2, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Annotations: map[string]string{"ai-grid/site-name": "Site A"}}, UpdateMask: mask("annotations")})
		wantAnn := map[string]string{"ai-grid/site-name": "Site A", AnnotationCreatedVia: AnnotationCreatedViaOther}
		if err != nil || !maps.Equal(got2.GetAnnotations(), wantAnn) || !maps.Equal(got2.GetLabels(), wantLabels) {
			t.Errorf("annotations replaced (created-via restored by the import controller), labels untouched: %v, %v", got2, err)
		}
		addons := labelsWith(got2, LabelAddonPrefix+"work-manager", AddonUnhealthy, LabelAddonPrefix+"made-up", AddonAvailable)
		got3, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Labels: addons}, UpdateMask: mask("labels")})
		if err != nil || got3.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonAvailable || got3.GetLabels()[LabelAddonPrefix+"made-up"] != "" {
			t.Errorf("add-on labels follow the add-ons, not the client: %v, %v", got3.GetLabels(), err)
		}
	})

	t.Run("lock compares resource_version", func(t *testing.T) {
		s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
		_, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{
			Object: &aigriddraftv1.ManagedCluster{Name: "a", ResourceVersion: "99", Labels: map[string]string{}}, UpdateMask: mask("labels"), Lock: true,
		})
		if status.Code(err) != codes.Aborted {
			t.Fatalf("stale version = %v, want Aborted", err)
		}
		got, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{
			Object: &aigriddraftv1.ManagedCluster{Name: "a", ResourceVersion: "100", Labels: labelsWith(readyCluster("a"), "ai-grid/x", "y")}, UpdateMask: mask("labels"), Lock: true,
		})
		if err != nil || got.GetLabels()["ai-grid/x"] != "y" {
			t.Fatalf("matching version = %v, %v", got, err)
		}
		_, err = update(s, &aigriddraftv1.UpdateManagedClusterRequest{
			Object: &aigriddraftv1.ManagedCluster{Name: "a", ResourceVersion: "100", Labels: map[string]string{}}, UpdateMask: mask("labels"), Lock: true,
		})
		if status.Code(err) != codes.Aborted {
			t.Errorf("the version used once is stale afterwards, got %v", err)
		}
		if _, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", ResourceVersion: "1", Labels: labelsWith(readyCluster("a"), "ai-grid/x", "z")}, UpdateMask: mask("labels")}); err != nil {
			t.Errorf("without lock the version is ignored, got %v", err)
		}
	})

	t.Run("only the console's keys may change", func(t *testing.T) {
		s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
		// Moving the site to another cluster set changes Placements and RBAC
		// scope; the hub keeps a non-empty clusterset label as it is.
		_, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Labels: labelsWith(readyCluster("a"), LabelClusterSet, "other")}, UpdateMask: mask("labels")})
		mustCode(t, err, codes.InvalidArgument, `label "cluster.open-cluster-management.io/clusterset" cannot be changed through the console; only ai-grid/* labels and annotations and the location label can`)
		// location and the fake's own ai-grid-mock/* switches are writable.
		got, err := update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Labels: labelsWith(readyCluster("a"), "location", "paris", "ai-grid-mock/fail-apply", "true")}, UpdateMask: mask("labels")})
		if err != nil || got.GetLabels()["location"] != "paris" || got.GetLabels()["ai-grid-mock/fail-apply"] != "true" {
			t.Errorf("console keys = %v, %v", got.GetLabels(), err)
		}
		// Hub acceptance is not writable: turning it off drops a live site.
		_, err = update(s, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", HubAcceptsClient: false}, UpdateMask: mask("hub_accepts_client")})
		mustCode(t, err, codes.InvalidArgument, `update_mask path "hub_accepts_client" is not supported; supported paths: labels, annotations`)
	})
}

// importingCluster is site-berlin-1 from the research: created by ACM, agent
// not yet joined.
func importingCluster(name string, created time.Time) *aigriddraftv1.ManagedCluster {
	c := withConditions(
		cond(CondHubAccepted, cTrue, ReasonHubAccepted, MessageHubAccepted),
		cond(CondImportSucceeded, cFalse, ReasonImporting, MessageImportApplied),
	)
	c.Name = name
	c.Labels = map[string]string{LabelName: name, LabelCloud: LabelAutoDetect, LabelVendor: LabelAutoDetect, LabelClusterSet: "default"}
	c.Taints = []*aigriddraftv1.Taint{{Key: TaintUnreachable, Effect: TaintNoSelect, TimeAdded: timestamppb.New(created.Add(3 * time.Second))}}
	c.CreationTimestamp = timestamppb.New(created)
	return c
}

func TestScenarioImport(t *testing.T) {
	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	// A cluster whose `cloud` label someone removed: autodetect fills it only
	// while it reads auto-detect, so it stays absent.
	noCloudLabel := importingCluster("oslo", start)
	delete(noCloudLabel.Labels, LabelCloud)
	spoke := `{"kubernetesVersion":"v1.35.6","capacity":{"cpu":"64","nvidia.com/gpu":"8"},"allocatable":{"cpu":"61","nvidia.com/gpu":"8"},
	  "clusterClaims":[{"name":"version.openshift.io","value":"4.22.15"},{"name":"id.k8s.io","value":"0f1e2d3c-4b5a-4968-8776-655443322110"},
	  {"name":"id.openshift.io","value":"5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"},
	  {"name":"platform.open-cluster-management.io","value":"AWS"},{"name":"product.open-cluster-management.io","value":"OpenShift"}]}`
	fx := &Fixtures{
		Clusters: []*aigriddraftv1.ManagedCluster{importingCluster("berlin", start), noCloudLabel},
		Speed:    2, // "20s" fires after 10s of wall clock
		Scenarios: []ScenarioEvent{
			{Cluster: "berlin", At: Duration{20 * time.Second}, Event: EventJoined, Spoke: []byte(spoke)},
			{Cluster: "oslo", At: Duration{20 * time.Second}, Event: EventJoined, Spoke: []byte(spoke)},
			{Cluster: "berlin", At: Duration{70 * time.Second}, Event: EventImported},
			{Cluster: "berlin", At: Duration{90 * time.Second}, Event: EventAddonsAvailable, Addons: []string{"work-manager"}},
		},
	}
	s := newServer(t, fx)
	now := start
	s.SetClock(func() time.Time { return now })
	ctx := authed(context.Background())
	get := func() *aigriddraftv1.ManagedCluster {
		resp, err := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: "berlin"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetObject()
	}

	c := get()
	if c.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORTING || c.GetStatus().GetStateMessage() != MessageImportApplied {
		t.Fatalf("t0: %s %q", c.GetStatus().GetState(), c.GetStatus().GetStateMessage())
	}
	before := c.GetResourceVersion()

	now = start.Add(9 * time.Second) // 18 scenario seconds: nothing yet
	if c = get(); c.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_IMPORTING || c.GetResourceVersion() != before {
		t.Fatalf("t9: %s rv %s", c.GetStatus().GetState(), c.GetResourceVersion())
	}

	now = start.Add(10 * time.Second) // joined fires at scenario 20s
	c = get()
	if c.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE {
		t.Fatalf("after joined: %s", c.GetStatus().GetState())
	}
	for _, typ := range []string{CondJoined, CondAvailable, CondClockSynced} {
		cd := findCondition(c, typ)
		if cd.GetStatus() != cTrue || !cd.GetLastTransitionTime().AsTime().Equal(start.Add(10*time.Second)) {
			t.Errorf("%s = %v; transition time must be the scheduled wall-clock instant", typ, cd)
		}
	}
	if findCondition(c, CondJoined).GetMessage() != MessageJoined || findCondition(c, CondClockSynced).GetMessage() != MessageClockSynced {
		t.Error("joined/clock-synced messages are the controllers'")
	}
	if findCondition(c, CondImportSucceeded).GetStatus() != cFalse {
		t.Error("import is not finished at join time")
	}
	l := c.GetLabels()
	if l[LabelCloud] != "Amazon" || l[LabelVendor] != "OpenShift" || l[LabelClusterID] != "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9" /* id.openshift.io, not id.k8s.io */ || l[LabelOpenShiftVersion] != "4.22.15" || l[LabelOpenShiftMajor] != "4" || l[LabelOpenShiftMajorMin] != "4.22" {
		t.Errorf("autodetect labels = %v", l)
	}
	if hasTaint(c, TaintUnreachable) || c.GetGeneration() != 1 {
		t.Errorf("the unreachable taint goes once Available is True, a spec change: %v generation %d", c.GetTaints(), c.GetGeneration())
	}
	oslo, err := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: "oslo"})
	if err != nil {
		t.Fatal(err)
	}
	if l := oslo.GetObject().GetLabels(); l[LabelVendor] != "OpenShift" || l[LabelClusterID] == "" {
		t.Errorf("autodetect still fills vendor and clusterID: %v", l)
	} else if _, ok := l[LabelCloud]; ok {
		t.Errorf("a removed cloud label is not added back: %v", l)
	}
	if c.GetStatus().GetCapacity()["nvidia.com/gpu"] != "8" || c.GetStatus().GetKubernetesVersion() != "v1.35.6" {
		t.Errorf("spoke status not applied: %v", c.GetStatus())
	}
	if claims := c.GetStatus().GetClusterClaims(); len(claims) != 5 || claims[0].GetName() != ClaimID || claims[1].GetName() != ClaimOpenShiftID || claims[4].GetName() != ClaimOCPVersion {
		t.Errorf("claims must be sorted by name: %v", claims)
	}
	if c.GetResourceVersion() == before {
		t.Error("a controller write bumps resourceVersion")
	}

	now = start.Add(35 * time.Second) // 70s: imported
	c = get()
	if imp := findCondition(c, CondImportSucceeded); imp.GetStatus() != cTrue || imp.GetReason() != ReasonImported || imp.GetMessage() != MessageImported {
		t.Errorf("imported = %v", imp)
	}
	if _, ok := c.GetLabels()[LabelAddonPrefix+"work-manager"]; ok {
		t.Error("add-on labels appear only after the add-ons report")
	}

	now = start.Add(45 * time.Second) // 90s: addons
	if c = get(); c.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonAvailable {
		t.Errorf("addon label = %v", c.GetLabels())
	}
}

func TestScenarioLeaseAndDetach(t *testing.T) {
	start := time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC)
	fx := &Fixtures{
		Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("madrid"), readyCluster("lyon"), readyCluster("ashburn"), readyCluster("frankfurt")},
		Scenarios: []ScenarioEvent{
			{Cluster: "frankfurt", At: Duration{10 * time.Second}, Event: EventAPIDown},
			{Cluster: "frankfurt", At: Duration{15 * time.Second}, Event: EventLeaseLost},
			{Cluster: "lyon", At: Duration{15 * time.Second}, Event: EventLeaseLost},
			{Cluster: "madrid", At: Duration{10 * time.Second}, Event: EventLeaseLost},
			{Cluster: "madrid", At: Duration{20 * time.Second}, Event: EventLeaseRecovered},
			{Cluster: "ashburn", At: Duration{10 * time.Second}, Event: EventAPIDown},
			{Cluster: "ashburn", At: Duration{20 * time.Second}, Event: EventAPIUp},
			{Cluster: "lyon", At: Duration{10 * time.Second}, Event: EventDetaching},
			{Cluster: "lyon", At: Duration{20 * time.Second}, Event: EventDetached},
		},
	}
	s := newServer(t, fx)
	now := start
	s.SetClock(func() time.Time { return now })
	ctx := authed(context.Background())
	get := func(name string) (*aigriddraftv1.ManagedCluster, error) {
		resp, err := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: name})
		return resp.GetObject(), err
	}
	lyonBefore, err := get("lyon")
	if err != nil {
		t.Fatal(err)
	}

	now = start.Add(10 * time.Second)
	m, err := get("madrid")
	if err != nil {
		t.Fatal(err)
	}
	av := findCondition(m, CondAvailable)
	if m.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNKNOWN || av.GetStatus() != cUnknown || av.GetReason() != ReasonLeaseStopped || av.GetMessage() != MessageLeaseStopped {
		t.Errorf("lease lost: %s %v", m.GetStatus().GetState(), av)
	}
	if !hasTaint(m, TaintUnreachable) || m.GetTaints()[0].GetTimeAdded().AsTime() != now || m.GetTaints()[0].GetEffect() != TaintNoSelect {
		t.Errorf("unreachable taint with timeAdded expected, got %v", m.GetTaints())
	}
	if m.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonUnreachable {
		t.Errorf("addon labels flip to unreachable, got %v", m.GetLabels())
	}
	a, _ := get("ashburn")
	av = findCondition(a, CondAvailable)
	if a.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_UNAVAILABLE || av.GetReason() != ReasonAPIServerUnavail || !strings.HasPrefix(av.GetMessage(), "The kube-apiserver is not ok, status code: 503, ") || !hasTaint(a, TaintUnavailable) || hasTaint(a, TaintUnreachable) || a.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonAvailable {
		t.Errorf("api down: %s %v taints %v labels %v", a.GetStatus().GetState(), av, a.GetTaints(), a.GetLabels())
	}
	l, _ := get("lyon")
	if l.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_DETACHING || l.GetDeletionTimestamp().AsTime() != now || l.GetStatus().GetStateMessage() != MessageDetaching {
		t.Errorf("detaching: %v", l)
	}
	if l.GetGeneration() != lyonBefore.GetGeneration()+1 {
		t.Errorf("the API server bumps generation once when it marks the cluster deleting: %d -> %d", lyonBefore.GetGeneration(), l.GetGeneration())
	}
	if imp := findCondition(l, CondImportSucceeded); imp.GetStatus() != cFalse || imp.GetReason() != ReasonDetaching {
		t.Errorf("detaching import condition = %v", imp)
	}

	// The taint controller keeps the two hub taints mutually exclusive: a
	// lease lost after the API server went down swaps unavailable for
	// unreachable. On a terminating cluster taints and add-on labels freeze.
	now = start.Add(15 * time.Second)
	f, _ := get("frankfurt")
	if !hasTaint(f, TaintUnreachable) || hasTaint(f, TaintUnavailable) || len(f.GetTaints()) != 1 || f.GetGeneration() != 2 {
		t.Errorf("api-down then lease-lost: taints %v generation %d", f.GetTaints(), f.GetGeneration())
	}
	if f.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonUnreachable {
		t.Errorf("lease lost marks add-ons unreachable: %v", f.GetLabels())
	}
	l, _ = get("lyon")
	if findCondition(l, CondAvailable).GetStatus() != cUnknown || len(l.GetTaints()) != 0 || l.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonAvailable {
		t.Errorf("terminating cluster: Available moves, taints and add-on labels do not: %v", l)
	}

	// An event for a cluster that is already gone is skipped, and
	// addons-available without a list uses the default add-ons.
	s.scenarios = append(s.scenarios,
		&scenario{ScenarioEvent: ScenarioEvent{Cluster: "lyon", At: Duration{25 * time.Second}, Event: EventImported}},
		&scenario{ScenarioEvent: ScenarioEvent{Cluster: "madrid", At: Duration{25 * time.Second}, Event: EventAddonsAvailable}},
	)

	now = start.Add(20 * time.Second)
	m, _ = get("madrid")
	av = findCondition(m, CondAvailable)
	if m.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE || !av.GetLastTransitionTime().AsTime().Equal(now) || hasTaint(m, TaintUnreachable) || m.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonAvailable {
		t.Errorf("lease recovered: %s %v %v %v", m.GetStatus().GetState(), av, m.GetTaints(), m.GetLabels())
	}
	if a, _ = get("ashburn"); a.GetStatus().GetState() != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE || len(a.GetTaints()) != 0 || a.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonAvailable {
		t.Errorf("api up: %v", a)
	}
	if _, err := get("lyon"); status.Code(err) != codes.NotFound {
		t.Errorf("detached cluster must be gone, got %v", err)
	}
	list, _ := s.ListManagedClusters(ctx, &aigriddraftv1.ListManagedClustersRequest{})
	if list.GetTotal() != 3 {
		t.Errorf("list after detach = %d", list.GetTotal())
	}

	now = start.Add(30 * time.Second)
	m, _ = get("madrid")
	for _, a := range defaultAddons {
		if m.GetLabels()[LabelAddonPrefix+a] != AddonAvailable {
			t.Errorf("default add-on %s missing: %v", a, m.GetLabels())
		}
	}
	if _, err := get("lyon"); status.Code(err) != codes.NotFound {
		t.Errorf("an event on a detached cluster must not resurrect it, got %v", err)
	}
	for _, sc := range s.scenarios {
		if !sc.applied {
			t.Errorf("event %s/%s was not marked applied", sc.Cluster, sc.Event)
		}
	}
}

// TestDetachingEventAddsDeleting: the gc controller's Deleting condition
// (ocm:pkg/registration/hub/gc/gc_resources.go L67-73, L93-99) names the
// managedclusteraddons still in the namespace and how many carry a
// finalizer, or reports the namespace clean when none are left.
func TestDetachingEventAddsDeleting(t *testing.T) {
	withAddons := readyCluster("a")
	withAddons.Labels[LabelAddonPrefix+"config-policy-controller"] = AddonAvailable
	bare := readyCluster("b")
	delete(bare.Labels, LabelAddonPrefix+"work-manager")
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{withAddons, bare}, Scenarios: []ScenarioEvent{
		{Cluster: "a", At: Duration{time.Second}, Event: EventDetaching},
		{Cluster: "b", At: Duration{time.Second}, Event: EventDetaching},
	}})
	now := time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	now = now.Add(time.Second)
	get := func(name string) *aigriddraftv1.Condition {
		resp, err := s.GetManagedCluster(authed(context.Background()), &aigriddraftv1.GetManagedClusterRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return findCondition(resp.GetObject(), CondDeleting)
	}
	want := "The resource managedclusteraddons is remaning, the remaining count is 2, the finalizer pending count is 1"
	if d := get("a"); d.GetStatus() != cFalse || d.GetReason() != ReasonResourceRemaining || d.GetMessage() != want {
		t.Errorf("Deleting with add-ons left = %v, want False/ResourceRemaining %q", d, want)
	}
	if d := get("b"); d.GetStatus() != cTrue || d.GetReason() != ReasonNoResource || d.GetMessage() != MessageNoResource {
		t.Errorf("Deleting with nothing left = %v, want True/NoResource", d)
	}
}

func TestScenariosAreValidated(t *testing.T) {
	if _, err := New(&Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}, Scenarios: []ScenarioEvent{{Cluster: "a", Event: "explode"}}}, nil); err == nil {
		t.Error("unknown event must fail at load")
	}
	if _, err := New(&Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}, Scenarios: []ScenarioEvent{{Cluster: "b", Event: EventImported}}}, nil); err == nil {
		t.Error("unknown cluster must fail at load")
	}
	if _, err := New(&Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}, Scenarios: []ScenarioEvent{{Cluster: "a", Event: EventJoined, Spoke: []byte(`{"nope":1}`)}}}, nil); err == nil {
		t.Error("bad spoke payload must fail at load")
	}
}

// labelsWith is c's labels with the given key/value pairs set: a whole map
// as a client that loaded c and changed some keys would send it.
func labelsWith(c *aigriddraftv1.ManagedCluster, kv ...string) map[string]string {
	m := copyMap(c.GetLabels())
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}
