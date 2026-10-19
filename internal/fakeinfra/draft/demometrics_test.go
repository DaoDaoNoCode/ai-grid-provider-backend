package draft

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// golden is internal/fakeinfra/draft/testdata/demo-metrics.json, the PoC's demoMetrics output
// written by hack/demo-metrics-golden.mjs.
type golden struct {
	Input struct {
		Clusters []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"clusters"`
		Start int64 `json:"start"`
		End   int64 `json:"end"`
		Step  int64 `json:"step"`
	} `json:"input"`
	Metrics []struct {
		Key     string  `json:"key"`
		Label   string  `json:"label"`
		Unit    string  `json:"unit"`
		Status  string  `json:"status"`
		Message *string `json:"message"`
		Points  []struct {
			Timestamp int64   `json:"timestamp"`
			Value     float64 `json:"value"`
		} `json:"points"`
	} `json:"metrics"`
}

// pocStatus is the PoC's metric status string for a MetricStatus.
var pocStatus = map[aigriddraftv1.MetricStatus]string{
	aigriddraftv1.MetricStatus_METRIC_STATUS_OK:             "ok",
	aigriddraftv1.MetricStatus_METRIC_STATUS_UNAVAILABLE:    "unavailable",
	aigriddraftv1.MetricStatus_METRIC_STATUS_NOT_CONFIGURED: "not-configured",
	aigriddraftv1.MetricStatus_METRIC_STATUS_NO_DATA:        "no-data",
}

// fixtureMembers are the clusters in dir that GetOverview passes to
// demoMetrics: the AI Grid members.
func fixtureMembers(t *testing.T, dir string) []*aigriddraftv1.ManagedCluster {
	t.Helper()
	fx, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []*aigriddraftv1.ManagedCluster
	for _, c := range fx.Clusters {
		if c.GetLabels()["ai-grid/enabled"] == "true" {
			out = append(out, c)
		}
	}
	return out
}

func TestDemoMetricsMatchThePoC(t *testing.T) {
	raw, err := os.ReadFile("testdata/demo-metrics.json")
	if err != nil {
		t.Fatal(err)
	}
	var want golden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	// The golden's input is the PoC's three sites, which are the members of
	// the PoC preset (fixtures/draft/poc): the preset feeds demoMetrics, so a
	// preset that drifts from the golden fails here.
	clusters := fixtureMembers(t, pocFixtures)
	if len(clusters) != len(want.Input.Clusters) {
		t.Fatalf("preset members %d, golden input %d", len(clusters), len(want.Input.Clusters))
	}
	for i, c := range want.Input.Clusters {
		if clusters[i].GetName() != c.Name || c.Status != "Ready" || !isTrue(clusters[i], CondAvailable) {
			t.Fatalf("preset member %d is %s (available %v), golden input %s %s", i, clusters[i].GetName(), isTrue(clusters[i], CondAvailable), c.Name, c.Status)
		}
	}

	got := demoMetrics(clusters, want.Input.Start, want.Input.End, want.Input.Step)
	if len(got) != len(want.Metrics) {
		t.Fatalf("%d metrics, want %d", len(got), len(want.Metrics))
	}
	for i, w := range want.Metrics {
		g := got[i]
		if g.GetKey() != w.Key || g.GetLabel() != w.Label || g.GetUnit() != w.Unit || pocStatus[g.GetStatus()] != w.Status {
			t.Fatalf("metric %d: got %s %q %q %s, want %s %q %q %s", i, g.GetKey(), g.GetLabel(), g.GetUnit(), g.GetStatus(), w.Key, w.Label, w.Unit, w.Status)
		}
		if (g.Message != nil) != (w.Message != nil) || (w.Message != nil && g.GetMessage() != *w.Message) {
			t.Fatalf("%s message %v, want %v", w.Key, g.Message, w.Message)
		}
		if len(g.GetPoints()) != len(w.Points) {
			t.Fatalf("%s: %d points, want %d", w.Key, len(g.GetPoints()), len(w.Points))
		}
		for j, wp := range w.Points {
			gp := g.GetPoints()[j]
			if gp.GetTimestamp() != wp.Timestamp || math.Abs(gp.GetValue()-wp.Value) > 1e-9 {
				t.Fatalf("%s point %d: got (%d, %v), want (%d, %v)", w.Key, j, gp.GetTimestamp(), gp.GetValue(), wp.Timestamp, wp.Value)
			}
		}
	}
}

func TestDemoMetricsWithoutReadyClusters(t *testing.T) {
	offline := fixtureMembers(t, repoFixtures)[0]
	offline.Status = &aigriddraftv1.ManagedClusterStatus{Conditions: []*aigriddraftv1.Condition{
		{Type: "ManagedClusterConditionAvailable", Status: aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE},
	}}
	unknown := &aigriddraftv1.ManagedCluster{Name: "no-status"}
	for _, tc := range []struct {
		name     string
		clusters []*aigriddraftv1.ManagedCluster
		message  string
	}{
		{"none ready", []*aigriddraftv1.ManagedCluster{offline, unknown}, "No available clusters report simulated telemetry for this selection."},
		{"none", nil, "No enrolled clusters match this selection."},
	} {
		got := demoMetrics(tc.clusters, 1790000000, 1790003600, 60)
		if len(got) != 4 {
			t.Fatalf("%s: %d metrics", tc.name, len(got))
		}
		for _, m := range got {
			if m.GetStatus() != aigriddraftv1.MetricStatus_METRIC_STATUS_NO_DATA || m.GetMessage() != tc.message || len(m.GetPoints()) != 0 {
				t.Fatalf("%s: %v", tc.name, m)
			}
		}
	}
}

// TestDemoHash checks the hash against the PoC's, values from node.
func TestDemoHash(t *testing.T) {
	for name, want := range map[string]uint32{"site1": 309851683, "a.example": 931874998, "a😀b": 2017919, "zzzzzzzzzzzzzzzzzzzz": 3197943559} {
		if got := demoHash(name); got != want {
			t.Errorf("demoHash(%q) = %d, want %d", name, got, want)
		}
	}
}

// TestToFixed3 checks the rounding against JavaScript's Number(x.toFixed(3)),
// which rounds an exact tie up, where %.3f would round it to even.
func TestToFixed3(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0.0625, 0.063}, // exact tie: (0.0625).toFixed(3) is "0.063"
		{0.1875, 0.188}, // exact tie
		{1.0005, 1},     // 1.0005 is 1.000499999... as a double: "1.000"
		{355.6694, 355.669},
		{2.5, 2.5},
	} {
		if got := toFixed3(tc.in); got != tc.want {
			t.Errorf("toFixed3(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestGetOverview(t *testing.T) {
	s := newRepoServer(t)
	ctx := authed(context.Background())
	names := func(resp *aigriddraftv1.GetOverviewResponse) string {
		var out []string
		for _, c := range resp.GetClusters() {
			out = append(out, c.GetName())
		}
		return strings.Join(out, ",")
	}

	for _, r := range []string{"2h", "1d"} {
		_, err := s.GetOverview(ctx, &aigriddraftv1.GetOverviewRequest{Range: r, IncludeMetrics: true})
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "Choose a time range of 1h, 6h, or 24h." {
			t.Fatalf("range %q: %v", r, err)
		}
	}

	// Members only (PR 12's fixtures: Paris, Madrid and Ashburn; the others
	// are not), the PoC's sources, metrics ending now.
	resp, err := s.GetOverview(ctx, &aigriddraftv1.GetOverviewRequest{Range: "6h", IncludeMetrics: true})
	if err != nil {
		t.Fatal(err)
	}
	if names(resp) != "site-ashburn-1,site-madrid-1,site-paris-1" || resp.GetInventorySource() != aigriddraftv1.InventorySource_INVENTORY_SOURCE_ACM ||
		resp.GetMetricsSource() != aigriddraftv1.MetricsSource_METRICS_SOURCE_DEMO || !resp.GetUpdatedAt().AsTime().Equal(fixedNow.Truncate(time.Second)) {
		t.Fatalf("overview %v", resp)
	}
	end := fixedNow.Unix()
	if len(resp.GetMetrics()) != 4 {
		t.Fatalf("metrics %v", resp.GetMetrics())
	}
	for _, m := range resp.GetMetrics() {
		p := m.GetPoints()
		if m.GetStatus() != aigriddraftv1.MetricStatus_METRIC_STATUS_OK || len(p) != 61 || p[0].GetTimestamp() != end-21600 || p[1].GetTimestamp()-p[0].GetTimestamp() != 360 || p[60].GetTimestamp() != end {
			t.Fatalf("%s: %d points from %d", m.GetKey(), len(p), p[0].GetTimestamp())
		}
	}

	resp, err = s.GetOverview(ctx, &aigriddraftv1.GetOverviewRequest{Range: "1h", Site: strptr("paris-dc1"), IncludeMetrics: true})
	if err != nil {
		t.Fatal(err)
	}
	if names(resp) != "site-paris-1" || len(resp.GetMetrics()[0].GetPoints()) != 61 {
		t.Fatalf("paris %v", resp)
	}

	resp, err = s.GetOverview(ctx, &aigriddraftv1.GetOverviewRequest{Range: "24h", Site: strptr("nowhere"), IncludeMetrics: true})
	if err != nil {
		t.Fatal(err)
	}
	if names(resp) != "" || resp.GetMetrics()[0].GetMessage() != "No enrolled clusters match this selection." {
		t.Fatalf("nowhere %v", resp)
	}

	resp, err = s.GetOverview(ctx, &aigriddraftv1.GetOverviewRequest{Range: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if names(resp) != "site-ashburn-1,site-madrid-1,site-paris-1" || len(resp.GetMetrics()) != 0 {
		t.Fatalf("without metrics %v", resp)
	}
}

func strptr(s string) *string { return &s }

// TestOverviewRejectsAnUnknownRange: the backend owns the range: anything
// but 1h, 6h or 24h is the PoC's InvalidArgument, an empty range is 1h, and
// both sources are always set. It authenticates like PR 12's RPCs.
func TestOverviewRejectsAnUnknownRange(t *testing.T) {
	s := newRepoServer(t)
	ctx := authed(context.Background())
	_, err := s.GetOverview(ctx, &aigriddraftv1.GetOverviewRequest{Range: "2h"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "Choose a time range of 1h, 6h, or 24h." {
		t.Fatalf("2h: %v", err)
	}
	resp, err := s.GetOverview(ctx, &aigriddraftv1.GetOverviewRequest{IncludeMetrics: true})
	if err != nil {
		t.Fatal(err)
	}
	p := resp.GetMetrics()[0].GetPoints()
	if len(p) != 61 || p[60].GetTimestamp()-p[0].GetTimestamp() != 3600 {
		t.Fatalf("an empty range is not 1h: %d points", len(p))
	}
	if resp.GetInventorySource() == aigriddraftv1.InventorySource_INVENTORY_SOURCE_UNSPECIFIED || resp.GetMetricsSource() == aigriddraftv1.MetricsSource_METRICS_SOURCE_UNSPECIFIED {
		t.Fatalf("sources %v %v", resp.GetInventorySource(), resp.GetMetricsSource())
	}
	if _, err := s.GetOverview(context.Background(), &aigriddraftv1.GetOverviewRequest{Range: "1h"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}
