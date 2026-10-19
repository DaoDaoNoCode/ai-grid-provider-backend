package hub

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// importingCluster is site-berlin-1 of the shipped fixtures in short:
// accepted, the import running, no agent yet.
func importingCluster(name, rv string) ManagedCluster {
	c := readyCluster(name, rv)
	c.Generation = 2
	c.Labels = map[string]string{LabelName: name, LabelClusterSet: ClusterSetDefault, LabelCloud: LabelAutoDetect, LabelVendor: LabelAutoDetect}
	c.Spec.Taints = []Taint{{Key: TaintUnreachable, Effect: TaintNoSelect, TimeAdded: metav1.NewTime(time.Date(2026, 10, 15, 10, 0, 3, 0, time.UTC))}}
	c.Status = &ManagedClusterStatus{Conditions: []metav1.Condition{
		{Type: CondHubAccepted, Status: metav1.ConditionTrue, Reason: ReasonHubAccepted, Message: MessageHubAccepted, LastTransitionTime: c.CreationTimestamp},
		{Type: CondImportSucceeded, Status: metav1.ConditionFalse, Reason: ReasonImporting, Message: MessageImportApplied, LastTransitionTime: c.CreationTimestamp},
	}}
	return c
}

func leaseLostCluster(name, rv string) ManagedCluster {
	c := readyCluster(name, rv)
	c.Generation = 4
	c.Labels[LabelAddonPrefix+"work-manager"] = AddonUnreachable
	c.Spec.Taints = []Taint{{Key: TaintUnreachable, Effect: TaintNoSelect, TimeAdded: metav1.NewTime(time.Date(2026, 10, 15, 11, 5, 0, 0, time.UTC))}}
	for i := range c.Status.Conditions {
		if c.Status.Conditions[i].Type == CondAvailable {
			c.Status.Conditions[i].Status = metav1.ConditionUnknown
			c.Status.Conditions[i].Reason = ReasonLeaseStopped
			c.Status.Conditions[i].Message = MessageLeaseStopped
		}
	}
	return c
}

func detachingCluster(name, rv string) ManagedCluster {
	c := readyCluster(name, rv)
	ts := metav1.NewTime(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	var zero int64
	c.DeletionTimestamp, c.DeletionGracePeriodSeconds = &ts, &zero
	return c
}

func scenarioFixtures(speed float64) *Fixtures {
	fx := testFixtures()
	fx.Clusters = []ManagedCluster{importingCluster("site-berlin-1", "200"), leaseLostCluster("site-madrid-1", "210"), detachingCluster("edge-lyon-2", "220")}
	fx.Settings.Speed = &speed
	fx.Scenarios = []ScenarioEvent{
		{Cluster: "site-berlin-1", At: Duration(20 * time.Second), Event: EventJoined, Spoke: &SpokeStatus{
			Version:  ManagedClusterVersion{Kubernetes: "v1.35.6"},
			Capacity: map[string]string{"cpu": "64", "memory": "524288Mi"},
			ClusterClaims: []ManagedClusterClaim{
				{Name: ClaimProduct, Value: "OpenShift"}, {Name: ClaimPlatform, Value: "BareMetal"},
				{Name: ClaimOpenShiftID, Value: "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"}, {Name: ClaimOCPVersion, Value: "4.22.15"},
			},
		}},
		{Cluster: "site-berlin-1", At: Duration(70 * time.Second), Event: EventImported},
		{Cluster: "site-berlin-1", At: Duration(90 * time.Second), Event: EventAddonsAvailable, Addons: []string{"work-manager", "cluster-proxy", "managed-serviceaccount"}},
		{Cluster: "site-berlin-1", At: Duration(130 * time.Second), Event: EventAddonsAvailable},
		{Cluster: "site-madrid-1", At: Duration(300 * time.Second), Event: EventLeaseRecovered},
		{Cluster: "edge-lyon-2", At: Duration(30 * time.Second), Event: EventDetached},
	}
	return fx
}

func getCluster(t *testing.T, th *testHub, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := th.client(t, testToken).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(%s) error = %v", name, err)
	}
	return obj
}

func TestScenarioEventsFireAtTheirOffsetsDividedBySpeed(t *testing.T) {
	th := startHub(t, scenarioFixtures(2))
	start := th.clock.Now()

	// Just before 20s/2: nothing happened yet.
	th.clock.Advance(10*time.Second - time.Millisecond)
	berlin := getCluster(t, th, "site-berlin-1")
	if _, joined := conditionsOf(t, berlin)[CondJoined]; joined || berlin.GetResourceVersion() != "200" {
		t.Fatalf("joined early: rv %s", berlin.GetResourceVersion())
	}

	th.clock.Advance(time.Millisecond)
	berlin = getCluster(t, th, "site-berlin-1")
	conds := conditionsOf(t, berlin)
	for typ, reason := range map[string]string{CondJoined: ReasonJoined, CondAvailable: ReasonAvailable, CondClockSynced: ReasonClockSynced} {
		c := conds[typ]
		if c["status"] != "True" || c["reason"] != reason {
			t.Fatalf("%s = %v", typ, c)
		}
		if c["lastTransitionTime"] != start.Add(10*time.Second).Format(time.RFC3339) {
			t.Fatalf("%s lastTransitionTime = %v, want the scheduled time", typ, c["lastTransitionTime"])
		}
	}
	if berlin.GetResourceVersion() != "221" || berlin.GetGeneration() != 3 {
		t.Fatalf("rv %s gen %d, want one write (221) and a generation bump for the removed taint", berlin.GetResourceVersion(), berlin.GetGeneration())
	}
	if _, found, _ := unstructured.NestedSlice(berlin.Object, "spec", "taints"); found {
		t.Fatal("unreachable taint not removed")
	}
	labels := berlin.GetLabels()
	if labels[LabelCloud] != "BareMetal" || labels[LabelVendor] != "OpenShift" || labels[LabelClusterID] != "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9" || labels[LabelOpenShiftMajorMin] != "4.22" {
		t.Fatalf("autodetect labels = %v", labels)
	}
	if v, _, _ := unstructured.NestedString(berlin.Object, "status", "version", "kubernetes"); v != "v1.35.6" {
		t.Fatalf("version = %q", v)
	}
	claims, _, _ := unstructured.NestedSlice(berlin.Object, "status", "clusterClaims")
	if len(claims) != 4 || claims[0].(map[string]any)["name"] != ClaimOpenShiftID {
		t.Fatalf("claims not name-sorted: %v", claims)
	}
	if c := conditionsOf(t, berlin)[CondImportSucceeded]; c["reason"] != ReasonImporting {
		t.Fatalf("imported early: %v", c)
	}

	// edge-lyon-2 finishes detaching at 30s/2.
	th.clock.Advance(5 * time.Second)
	if _, err := th.client(t, testToken).Get(context.Background(), "edge-lyon-2", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("edge-lyon-2 after detach = %v, want NotFound", err)
	}

	// Jump past every event at once: they fire in order, at their own times.
	th.clock.Advance(10 * time.Minute)
	berlin = getCluster(t, th, "site-berlin-1")
	if c := conditionsOf(t, berlin)[CondImportSucceeded]; c["status"] != "True" || c["reason"] != ReasonImported || c["lastTransitionTime"] != start.Add(35*time.Second).Format(time.RFC3339) {
		t.Fatalf("ImportSucceeded = %v", c)
	}
	for _, addon := range defaultAddons {
		if berlin.GetLabels()[LabelAddonPrefix+addon] != AddonAvailable {
			t.Fatalf("addon %s label = %q", addon, berlin.GetLabels()[LabelAddonPrefix+addon])
		}
	}
	madrid := getCluster(t, th, "site-madrid-1")
	if c := conditionsOf(t, madrid)[CondAvailable]; c["status"] != "True" || c["lastTransitionTime"] != start.Add(150*time.Second).Format(time.RFC3339) {
		t.Fatalf("madrid Available = %v", c)
	}
	if madrid.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonAvailable {
		t.Fatalf("madrid add-on labels = %v", madrid.GetLabels())
	}
	if _, found, _ := unstructured.NestedSlice(madrid.Object, "spec", "taints"); found {
		t.Fatal("madrid unreachable taint not removed")
	}
	// joined, detached, imported, addons, addons, lease-recovered: six writes.
	list, _ := th.client(t, testToken).List(context.Background(), metav1.ListOptions{})
	if list.GetResourceVersion() != "226" {
		t.Fatalf("resourceVersion = %s, want 226", list.GetResourceVersion())
	}
}

func TestLeaseLostAndAPIDownEvents(t *testing.T) {
	fx := testFixtures()
	fx.Scenarios = []ScenarioEvent{
		{Cluster: "site-a", At: Duration(time.Second), Event: EventLeaseLost},
		{Cluster: "site-b", At: Duration(time.Second), Event: EventAPIDown},
		{Cluster: "site-b", At: Duration(2 * time.Second), Event: EventAPIUp},
		{Cluster: "site-a", At: Duration(3 * time.Second), Event: EventDetaching},
	}
	th := startHub(t, fx)
	th.clock.Advance(time.Second)

	a := getCluster(t, th, "site-a")
	if c := conditionsOf(t, a)[CondAvailable]; c["status"] != "Unknown" || c["reason"] != ReasonLeaseStopped || c["message"] != MessageLeaseStopped {
		t.Fatalf("lease lost Available = %v", c)
	}
	if a.GetLabels()[LabelAddonPrefix+"work-manager"] != AddonUnreachable {
		t.Fatalf("add-on label = %q", a.GetLabels()[LabelAddonPrefix+"work-manager"])
	}
	taints, _, _ := unstructured.NestedSlice(a.Object, "spec", "taints")
	if len(taints) != 1 || taints[0].(map[string]any)["key"] != TaintUnreachable {
		t.Fatalf("taints = %v", taints)
	}
	b := getCluster(t, th, "site-b")
	if c := conditionsOf(t, b)[CondAvailable]; c["status"] != "False" || c["message"] != "The kube-apiserver is not ok, status code: 503, the server is currently unable to handle the request" {
		t.Fatalf("api down Available = %v", c)
	}
	taints, _, _ = unstructured.NestedSlice(b.Object, "spec", "taints")
	if len(taints) != 1 || taints[0].(map[string]any)["key"] != TaintUnavailable {
		t.Fatalf("taints = %v", taints)
	}

	th.clock.Advance(2 * time.Second)
	b = getCluster(t, th, "site-b")
	if _, found, _ := unstructured.NestedSlice(b.Object, "spec", "taints"); found || conditionsOf(t, b)[CondAvailable]["status"] != "True" {
		t.Fatalf("api up = %v", b.Object["status"])
	}
	a = getCluster(t, th, "site-a")
	if a.GetDeletionTimestamp() == nil || a.GetGeneration() != 5 {
		t.Fatalf("detaching: deletionTimestamp %v generation %d", a.GetDeletionTimestamp(), a.GetGeneration())
	}
	if c := conditionsOf(t, a)[CondDeleting]; c["status"] != "False" || c["reason"] != ReasonResourceRemaining {
		t.Fatalf("Deleting = %v", c)
	}
}

func TestScenarioValidation(t *testing.T) {
	for name, ev := range map[string]ScenarioEvent{
		"unknown event":   {Cluster: "site-a", Event: "exploded"},
		"unknown cluster": {Cluster: "nope", Event: EventJoined},
		"spoke elsewhere": {Cluster: "site-a", Event: EventImported, Spoke: &SpokeStatus{}},
	} {
		fx := testFixtures()
		fx.Scenarios = []ScenarioEvent{ev}
		if _, err := New(fx); err == nil {
			t.Fatalf("%s: New() accepted %+v", name, ev)
		}
	}
}

// TestReconcileLeavesSettledClustersAlone checks that the controllers the
// fake runs after a client write change nothing on a cluster they have
// already settled, so a label update writes only what the client asked for.
func TestReconcileLeavesSettledClustersAlone(t *testing.T) {
	for _, c := range []ManagedCluster{readyCluster("a", "1"), importingCluster("b", "2"), leaseLostCluster("c", "3")} {
		next := c.DeepCopy()
		reconcile(next, c.DeepCopy(), time.Now())
		if !jsonEqual(&c, next) {
			t.Fatalf("reconcile changed settled cluster %s", c.Name)
		}
	}
}
