package draft

import (
	"context"
	"maps"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// The backend, not the BFF, decides that an update changes nothing: the BFF
// sends every field the client sent, and an unchanged object comes back with
// its stored resource_version, as the API server answers an unchanged update.

func TestUnchangedClusterUpdateIsNotWritten(t *testing.T) {
	// A cluster as the hub keeps it: with the default clusterset label the
	// webhook would otherwise add on the first write.
	c := readyCluster("a")
	c.Labels[LabelClusterSet] = ClusterSetDefault
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{c}})
	ctx := authed(context.Background())
	get := func() *aigriddraftv1.ManagedCluster {
		t.Helper()
		resp, err := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: "a"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetObject()
	}
	update := func(obj *aigriddraftv1.ManagedCluster, paths ...string) (*aigriddraftv1.ManagedCluster, error) {
		resp, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{Object: obj, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, Lock: true})
		return resp.GetObject(), err
	}
	stored := get()
	rv, gen := stored.GetResourceVersion(), stored.GetGeneration()

	withoutName := maps.Clone(stored.GetLabels())
	delete(withoutName, LabelName) // the webhook puts `name` back, so leaving it out changes nothing
	for _, tc := range []struct {
		name  string
		obj   *aigriddraftv1.ManagedCluster
		paths []string
	}{
		{"the stored maps", &aigriddraftv1.ManagedCluster{Labels: stored.GetLabels(), Annotations: stored.GetAnnotations()}, []string{PathLabels, PathAnnotations}},
		{"without a hub-owned label", &aigriddraftv1.ManagedCluster{Labels: withoutName}, []string{PathLabels}},
	} {
		tc.obj.Name, tc.obj.ResourceVersion = "a", rv
		got, err := update(tc.obj, tc.paths...)
		if err != nil || got.GetResourceVersion() != rv || got.GetGeneration() != gen {
			t.Errorf("%s: %v, rv %s gen %d; want the stored %s/%d", tc.name, err, got.GetResourceVersion(), got.GetGeneration(), rv, gen)
		}
	}
	if again := get(); again.GetResourceVersion() != rv || !maps.Equal(again.GetLabels(), stored.GetLabels()) {
		t.Errorf("a no-op must leave the store alone: %v", again)
	}
	// The precondition runs first: a stale no-op is still Aborted.
	_, err := update(&aigriddraftv1.ManagedCluster{Name: "a", ResourceVersion: "1", Labels: stored.GetLabels()}, PathLabels)
	mustCode(t, err, codes.Aborted, "")
	// A real change writes and bumps the version.
	labels := maps.Clone(stored.GetLabels())
	labels["ai-grid/enabled"] = "true"
	got, err := update(&aigriddraftv1.ManagedCluster{Name: "a", ResourceVersion: rv, Labels: labels}, PathLabels)
	if err != nil || got.GetResourceVersion() == rv || got.GetLabels()["ai-grid/enabled"] != "true" {
		t.Errorf("a change = %v, %v", got, err)
	}
}

// The coordinate annotations are the backend's convention (a console
// proposal, not API-server behaviour): present means a number in range.
func TestClusterCoordinatesAreChecked(t *testing.T) {
	s := newServer(t, &Fixtures{Clusters: []*aigriddraftv1.ManagedCluster{readyCluster("a")}})
	ctx := authed(context.Background())
	update := func(annotations map[string]string) (*aigriddraftv1.ManagedCluster, error) {
		resp, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Annotations: annotations}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathAnnotations}}})
		return resp.GetObject(), err
	}
	for annotations, want := range map[[2]string]string{
		{AnnotationLatitude, "91"}:      `annotation "ai-grid/latitude" must be a number between -90 and 90, got "91"`,
		{AnnotationLatitude, "north"}:   `annotation "ai-grid/latitude" must be a number between -90 and 90, got "north"`,
		{AnnotationLatitude, "NaN"}:     `annotation "ai-grid/latitude" must be a number between -90 and 90, got "NaN"`,
		{AnnotationLongitude, "-180.5"}: `annotation "ai-grid/longitude" must be a number between -180 and 180, got "-180.5"`,
		{AnnotationLongitude, "+Inf"}:   `annotation "ai-grid/longitude" must be a number between -180 and 180, got "+Inf"`,
	} {
		_, err := update(map[string]string{annotations[0]: annotations[1]})
		mustCode(t, err, codes.InvalidArgument, want)
	}
	got, err := update(map[string]string{AnnotationLatitude: "-90", AnnotationLongitude: "2.3522", "ai-grid/site-name": "Paris DC1"})
	if err != nil || got.GetAnnotations()[AnnotationLatitude] != "-90" {
		t.Errorf("valid coordinates = %v, %v", got, err)
	}
	// What the read side shows fine passes: it trims the value and shows an
	// empty one as no coordinate, so a client sending the map it loaded back
	// is not refused for a key it never touched.
	for _, v := range []string{" 2.3522", "2.3522\n", ""} {
		if _, err := update(map[string]string{AnnotationLongitude: v}); err != nil {
			t.Errorf("longitude %q = %v", v, err)
		}
	}
	// Labels alone are not checked against annotations the fixtures carry.
	if _, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{Object: &aigriddraftv1.ManagedCluster{Name: "a", Labels: labelsWith(readyCluster("a"), "ai-grid/x", "y")}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathLabels}}}); err != nil {
		t.Errorf("labels update = %v", err)
	}
}
