package draft

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// fixedNow is the clock of newRepoServer / newPoCServer.
var fixedNow = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

// pocFixtures is the preset `make dev-fake-poc` loads.
const pocFixtures = "../../../fixtures/draft/poc"

// newRepoServer is a server on the shipped fixtures whose clock stands still
// at fixedNow.
func newRepoServer(t *testing.T) *Server {
	t.Helper()
	fx, err := Load(repoFixtures)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(fx, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return fixedNow })
	return s
}

// fieldViolations are the BadRequest field violations of err, by field.
func fieldViolations(err error) map[string]string {
	out := map[string]string{}
	for _, d := range status.Convert(err).Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				out[v.GetField()] = v.GetDescription()
			}
		}
	}
	return out
}

// pocCluster is what the PoC shows for one of its sites.
type pocCluster struct {
	name, site, siteName, region, latitude, longitude string
	enrolled, provisioned                             bool
}

var pocClusters = []pocCluster{
	{"site1", "san-francisco", "San Francisco", "us-west", "37.7749", "-122.4194", true, true},
	{"site2", "new-york", "New York", "us-east", "40.7128", "-74.0060", true, true},
	{"site3", "brussels", "Brussels", "europe-west", "50.8503", "4.3517", true, true},
	{"site4", "sydney", "Sydney", "asia-pacific", "-33.8688", "151.2093", false, false},
}

func newPoCServer(t *testing.T) *Server {
	t.Helper()
	fx, err := Load(pocFixtures)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(fx, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return fixedNow })
	return s
}

// TestPoCPresetMatchesThePoC: the preset seeds the PoC's cluster and tenant
// inventory. Models and subscriptions live in ai_grid.v1 (fake hub fixtures),
// not the draft fake.
func TestPoCPresetMatchesThePoC(t *testing.T) {
	fx, err := Load(pocFixtures)
	if err != nil {
		t.Fatal(err)
	}
	checkClustersLookLikeTheHub(t, fx)
	checkClustersUseOnlyHubStrings(t, fx)
	if fx.Latency.Duration() != 0 || fx.Speed != 1 || fx.ManifestWorkReplicaSetEnabled == nil || !*fx.ManifestWorkReplicaSetEnabled || len(fx.Scenarios) != 0 {
		t.Errorf("settings: latency %s, speed %v, MWRS %v, %d scenario events; want 0s, 1, true, none", fx.Latency.Duration(), fx.Speed, fx.ManifestWorkReplicaSetEnabled, len(fx.Scenarios))
	}

	s := newPoCServer(t)
	ctx := authed(context.Background())

	clusters, err := s.ListManagedClusters(ctx, &aigriddraftv1.ListManagedClustersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters.GetClusters()) != len(pocClusters) {
		t.Fatalf("%d clusters, want %d", len(clusters.GetClusters()), len(pocClusters))
	}
	for i, want := range pocClusters {
		c := clusters.GetClusters()[i]
		l, a := c.GetLabels(), c.GetAnnotations()
		if c.GetName() != want.name || l[LabelLocation] != want.name || l[LabelAIGridSite] != want.site || l[LabelAIGridRegion] != want.region ||
			a["ai-grid/site-name"] != want.siteName || a["ai-grid/latitude"] != want.latitude || a["ai-grid/longitude"] != want.longitude {
			t.Errorf("cluster %d = %s location %q site %q region %q name %q at %q,%q; want %+v", i, c.GetName(), l[LabelLocation], l[LabelAIGridSite], l[LabelAIGridRegion],
				a["ai-grid/site-name"], a["ai-grid/latitude"], a["ai-grid/longitude"], want)
		}
		if got := c.GetStatus().GetState(); got != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE {
			t.Errorf("%s: state %s, want ready", c.GetName(), got)
		}
		if (l[LabelAIGridEnabled] == "true") != want.enrolled || (l[LabelAIGridProvisioned] == "true") != want.provisioned {
			t.Errorf("%s: enabled %q provisioned %q, want enrolled %v provisioned %v", c.GetName(), l[LabelAIGridEnabled], l[LabelAIGridProvisioned], want.enrolled, want.provisioned)
		}
	}

	tenants, err := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var tenantNames []string
	for _, tn := range tenants.GetTenants() {
		tenantNames = append(tenantNames, tn.GetName())
		if tn.GetIdentityProvider() == nil || tn.GetResourceVersion() == "" || tn.GetCreateTime() == nil {
			t.Errorf("%s: identity provider %v, version %q, created %v; want all three", tn.GetName(), tn.GetIdentityProvider(), tn.GetResourceVersion(), tn.GetCreateTime())
		}
	}
	slices.Sort(tenantNames)
	if !slices.Equal(tenantNames, []string{"acme-robotics", "globex-research", "initech-labs"}) {
		t.Errorf("organizations %v", tenantNames)
	}
}

// TestPoCPresetEnrollsSite4AsProvisioning: as in the PoC, enrolling site4
// makes it a member that is not provisioned.
func TestPoCPresetEnrollsSite4AsProvisioning(t *testing.T) {
	s := newPoCServer(t)
	ctx := authed(context.Background())
	site4, err := s.GetManagedCluster(ctx, &aigriddraftv1.GetManagedClusterRequest{Name: "site4"})
	if err != nil {
		t.Fatal(err)
	}
	labels := site4.GetObject().GetLabels()
	labels[LabelAIGridEnabled] = "true"
	resp, err := s.UpdateManagedCluster(ctx, &aigriddraftv1.UpdateManagedClusterRequest{
		Object:     &aigriddraftv1.ManagedCluster{Name: "site4", Labels: labels},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if l := resp.GetObject().GetLabels(); l[LabelAIGridEnabled] != "true" || l[LabelAIGridProvisioned] == "true" {
		t.Fatalf("site4 labels %v", l)
	}
}
