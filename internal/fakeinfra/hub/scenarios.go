package hub

// Ported from the console's fake backend (ai-grid-cloud/ai-grid-provider-console
// 537df38, bff/internal/fakebackend/scenarios.go). The events, their offsets
// and what each one writes are the same; here they write the ManagedCluster
// CR that the real backend reads, and every event is one API server write
// (one resourceVersion) of the controller that drives it.

import (
	"fmt"
	"slices"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Scenario events. Each one is a transition a hub controller drives, with the
// condition, taint and label changes the cited controllers make (see the
// constants in acm.go for each source line).
const (
	// EventJoined: the klusterlet registered and its certificate was signed.
	// Adds ManagedClusterJoined, Available and ClockSynced True, publishes
	// what the agent reports (claims, capacity, allocatable, version), lets
	// autodetect replace the `auto-detect` labels and the taint controller
	// remove `unreachable`. On a hub `cloud`, `vendor` and `clusterID` follow
	// a little later, once the work-manager add-on syncs ManagedClusterInfo;
	// the fake writes them at join.
	EventJoined = "joined"
	// EventImported: the `<cluster>-klusterlet` ManifestWork is Available;
	// ImportSucceeded becomes True/ManagedClusterImported. The summary state
	// does not change (a joined cluster already reads from Available), only
	// that condition and resourceVersion do.
	EventImported = "imported"
	// EventAddonsAvailable: the add-on agents report; the
	// `feature.open-cluster-management.io/addon-*` labels become
	// `available`. On a hub each label first appears as `unreachable` when
	// the ManagedClusterAddOn is created and flips once its agent reports,
	// one add-on after another; the fake writes `available` directly for the
	// listed add-ons (schedule several events with different `addons` lists
	// to spread them).
	EventAddonsAvailable = "addons-available"
	// EventLeaseLost: 5 x leaseDurationSeconds without a lease renewal.
	// Available Unknown/ManagedClusterLeaseUpdateStopped; the taint
	// controller swaps any `unavailable` taint for `unreachable` (with
	// timeAdded); the add-on health check marks every add-on Unknown, so the
	// add-on labels read `unreachable`.
	EventLeaseLost = "lease-lost"
	// EventLeaseRecovered: the agent renews again: Available True (a new
	// lastTransitionTime), no hub taint, and the add-on labels `available`
	// (on a hub each add-on reports its own state again; the fake assumes
	// they are all healthy).
	EventLeaseRecovered = "lease-recovered"
	// EventAPIDown: the agent is alive but the managed API server answers
	// errors. Available False/ManagedClusterKubeAPIServerUnavailable; the
	// taint controller swaps any `unreachable` taint for `unavailable`. The
	// add-on labels are left as they are: the hub's add-on health check acts
	// only on Unknown.
	EventAPIDown = "api-down"
	// EventAPIUp: Available True again and no hub taint; add-on labels
	// untouched.
	EventAPIUp = "api-up"
	// EventDetaching: deletion requested; deletionTimestamp is set,
	// ImportSucceeded becomes False/ManagedClusterDetaching and the gc
	// controller writes Deleting: False/ResourceRemaining naming the
	// managedclusteraddons still in the namespace, or True/NoResource when
	// there are none. Taints, add-on labels and autodetect labels freeze.
	EventDetaching = "detaching"
	// EventDetached: the finalizers are gone; the object disappears.
	EventDetached = "detached"
)

// ScenarioEvent is one timed transition from scenarios.json. `at` is relative
// to the fake's start and is divided by the settings' `speed`.
type ScenarioEvent struct {
	// Cluster the event applies to.
	Cluster string `json:"cluster"`
	// At is when the event fires, relative to start, for example "20s".
	At Duration `json:"at"`
	// Event is one of the Event* constants.
	Event string `json:"event"`
	// Spoke, for `joined`: what the agent reports once it joins, in the
	// shape of the CR's status (`version`, `capacity`, `allocatable`,
	// `clusterClaims`). Labels are derived from the claims like the
	// autodetect controller does.
	Spoke *SpokeStatus `json:"spoke,omitempty"`
	// Addons, for `addons-available`: add-on names without the label prefix.
	// Defaults to the seven ACM add-ons in defaultAddons.
	Addons []string `json:"addons,omitempty"`
	// StatusCode and Detail, for `api-down`: fill the agent's message
	// "The kube-apiserver is not ok, status code: %d, %v". Default 503.
	StatusCode int    `json:"statusCode,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// SpokeStatus is the part of ManagedClusterStatus the spoke agent publishes
// when it joins.
type SpokeStatus struct {
	Version       ManagedClusterVersion `json:"version,omitempty"`
	Capacity      map[string]string     `json:"capacity,omitempty"`
	Allocatable   map[string]string     `json:"allocatable,omitempty"`
	ClusterClaims []ManagedClusterClaim `json:"clusterClaims,omitempty"`
}

// scenario is a parsed event waiting to fire. uid is the fixture object's
// metadata.uid: the event belongs to that object, and not to another one
// that is created later under the same name.
type scenario struct {
	ScenarioEvent
	uid     types.UID
	applied bool
}

var knownEvents = map[string]bool{
	EventJoined: true, EventImported: true, EventAddonsAvailable: true, EventLeaseLost: true,
	EventLeaseRecovered: true, EventAPIDown: true, EventAPIUp: true, EventDetaching: true, EventDetached: true,
}

// parseScenarios validates the events and orders them by time.
func parseScenarios(events []ScenarioEvent, clusters map[string]*ManagedCluster) ([]*scenario, error) {
	out := make([]*scenario, 0, len(events))
	for i, ev := range events {
		if !knownEvents[ev.Event] {
			return nil, fmt.Errorf("scenario %d: unknown event %q", i, ev.Event)
		}
		c, ok := clusters[ev.Cluster]
		if !ok {
			return nil, fmt.Errorf("scenario %d: cluster %q is not in the fixtures", i, ev.Cluster)
		}
		if ev.Spoke != nil && ev.Event != EventJoined {
			return nil, fmt.Errorf("scenario %d: spoke is only read by %q events", i, EventJoined)
		}
		out = append(out, &scenario{ScenarioEvent: ev, uid: c.UID})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Duration() < out[j].At.Duration() })
	return out, nil
}

// dueAction is a scenario event or a detach completion that is due.
type dueAction struct {
	at       time.Time
	scenario *scenario
	detach   string
}

// advance fires every due event and completes every due detach, in time
// order. It is called under h.mu by every request, so transitions are
// evaluated lazily from the clock and no goroutine is needed; the result is
// the same as controllers that ran in between.
func (h *Hub) advance() {
	now := h.clock()
	elapsed := time.Duration(float64(now.Sub(h.start)) * h.speed)
	var due []dueAction
	for _, sc := range h.scenarios {
		if sc.applied || sc.At.Duration() > elapsed {
			continue
		}
		sc.applied = true
		// The event happened at its scheduled wall-clock time, not when it
		// was noticed, so lastTransitionTime and timeAdded are consistent.
		at := h.start.Add(time.Duration(float64(sc.At.Duration()) / h.speed))
		due = append(due, dueAction{at: at, scenario: sc})
	}
	detachAfter := time.Duration(float64(h.detachAfter) / h.speed)
	for name, requested := range h.detaches {
		if at := requested.Add(detachAfter); !at.After(now) {
			due = append(due, dueAction{at: at, detach: name})
			delete(h.detaches, name)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, d := range due {
		if d.scenario != nil {
			h.applyScenario(d.scenario, d.at)
			continue
		}
		if c, ok := h.clusters[d.detach]; ok && terminating(c) {
			h.completeDetach(c)
		}
	}
}

func (h *Hub) applyScenario(sc *scenario, at time.Time) {
	c, ok := h.clusters[sc.Cluster]
	if !ok {
		return // detached earlier
	}
	if c.UID != sc.uid {
		h.logger.Info("scenario skipped: the fixture cluster was deleted and its name reused", "cluster", sc.Cluster, "event", sc.Event, "fixtureUID", sc.uid, "uid", c.UID)
		return
	}
	h.logger.Info("scenario", "cluster", sc.Cluster, "event", sc.Event, "at", sc.At.Duration())
	if sc.Event == EventDetached {
		if !terminating(c) {
			h.logger.Info("scenario skipped: detached needs a cluster whose deletion was requested", "cluster", sc.Cluster)
			return
		}
		h.completeDetach(c)
		return
	}
	next := c.DeepCopy()
	applyEvent(next, sc, at)
	h.commit(c, next)
}

// hubFinalizers are the finalizers the hub's own controllers hold on a
// ManagedCluster (the Finalizer* constants in acm.go, with their sources).
var hubFinalizers = []string{
	FinalizerImportCleanup, FinalizerManifestWork, FinalizerClusterInfo,
	FinalizerClusterRole, FinalizerResourceCleanup, FinalizerClusterAPIResource,
}

// completeDetach is the hub controllers finishing their cleanup of a
// terminating cluster: each removes its own finalizer, in one write. With no
// finalizer left the API server deletes the object; one that still carries
// a finalizer of somebody else stays terminating until that one goes too.
// Must be called under h.mu.
func (h *Hub) completeDetach(c *ManagedCluster) {
	next := c.DeepCopy()
	next.Finalizers = slices.DeleteFunc(next.Finalizers, func(f string) bool { return slices.Contains(hubFinalizers, f) })
	if len(next.Finalizers) == 0 {
		next.Finalizers = nil
	}
	delete(h.detaches, c.Name)
	if stored, _ := h.commit(c, next); stored != nil {
		h.logger.Info("detach finished; other finalizers keep the cluster", "cluster", c.Name, "finalizers", stored.Finalizers)
		return
	}
	h.logger.Info("detach finished", "cluster", c.Name)
}

// removeObject is the API server deleting an object (a write with its own
// resourceVersion that nobody can read back). Must be called under h.mu.
func (h *Hub) removeObject(c *ManagedCluster) {
	delete(h.clusters, c.Name)
	delete(h.detaches, c.Name)
	delete(h.enabledAt, c.Name)
	h.nextResourceVersion()
}

func applyEvent(c *ManagedCluster, sc *scenario, now time.Time) {
	switch sc.Event {
	case EventJoined:
		if sc.Spoke != nil {
			if c.Status == nil {
				c.Status = &ManagedClusterStatus{}
			}
			// A copy, so the stored object never aliases the scenario.
			spoke := (&ManagedCluster{Status: &ManagedClusterStatus{
				Version: sc.Spoke.Version, Capacity: sc.Spoke.Capacity,
				Allocatable: sc.Spoke.Allocatable, ClusterClaims: sc.Spoke.ClusterClaims,
			}}).DeepCopy().Status
			c.Status.Version = spoke.Version
			c.Status.Capacity = spoke.Capacity
			c.Status.Allocatable = spoke.Allocatable
			c.Status.ClusterClaims = spoke.ClusterClaims
			sortClaims(c)
		}
		setCondition(c, now, CondJoined, metav1.ConditionTrue, ReasonJoined, MessageJoined)
		setCondition(c, now, CondAvailable, metav1.ConditionTrue, ReasonAvailable, MessageAvailable)
		setCondition(c, now, CondClockSynced, metav1.ConditionTrue, ReasonClockSynced, MessageClockSynced)
		autodetectLabels(c)
		syncTaints(c, now)
	case EventImported:
		setCondition(c, now, CondImportSucceeded, metav1.ConditionTrue, ReasonImported, MessageImported)
	case EventAddonsAvailable:
		if terminating(c) {
			break // the discovery controller skips a terminating cluster
		}
		addons := sc.Addons
		if len(addons) == 0 {
			addons = defaultAddons
		}
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		for _, a := range addons {
			c.Labels[LabelAddonPrefix+a] = AddonAvailable
		}
	case EventLeaseLost:
		setCondition(c, now, CondAvailable, metav1.ConditionUnknown, ReasonLeaseStopped, MessageLeaseStopped)
		syncTaints(c, now)
		setAddonLabels(c, AddonUnreachable)
	case EventLeaseRecovered:
		setCondition(c, now, CondAvailable, metav1.ConditionTrue, ReasonAvailable, MessageAvailable)
		syncTaints(c, now)
		setAddonLabels(c, AddonAvailable)
	case EventAPIDown:
		code := sc.StatusCode
		if code == 0 {
			code = 503
		}
		detail := sc.Detail
		if detail == "" {
			detail = "the server is currently unable to handle the request"
		}
		setCondition(c, now, CondAvailable, metav1.ConditionFalse, ReasonAPIServerUnavail, fmt.Sprintf(MessageAPIServerUnavailFm, code, detail))
		syncTaints(c, now)
	case EventAPIUp:
		setCondition(c, now, CondAvailable, metav1.ConditionTrue, ReasonAvailable, MessageAvailable)
		syncTaints(c, now)
	case EventDetaching:
		// Hub.commit bumps generation when deletion is first requested, as
		// the API server does when it marks an object deleting
		// (apiserver registry/generic/registry/store.go markAsDeleting
		// L1016-1021), which also sets deletionGracePeriodSeconds to 0.
		if c.DeletionTimestamp == nil {
			ts := metav1.NewTime(now)
			c.DeletionTimestamp = &ts
			var zero int64
			c.DeletionGracePeriodSeconds = &zero
		}
		markDetaching(c, now)
	}
}
