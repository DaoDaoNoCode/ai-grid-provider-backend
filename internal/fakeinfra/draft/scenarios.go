package draft

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
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
	// Spoke, for `joined`: what the agent reports once it joins, as a
	// protojson ManagedClusterStatus (`kubernetesVersion`, `capacity`,
	// `allocatable`, `clusterClaims`). Labels are derived from the claims
	// like the autodetect controller does.
	Spoke json.RawMessage `json:"spoke,omitempty"`
	// Addons, for `addons-available`: add-on names without the label prefix.
	// Defaults to the seven ACM add-ons in defaultAddons.
	Addons []string `json:"addons,omitempty"`
	// StatusCode and Detail, for `api-down`: fill the agent's message
	// "The kube-apiserver is not ok, status code: %d, %v". Default 503.
	StatusCode int    `json:"statusCode,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// scenario is a parsed event waiting to fire.
type scenario struct {
	ScenarioEvent
	spoke   *aigriddraftv1.ManagedClusterStatus
	applied bool
}

var knownEvents = map[string]bool{
	EventJoined: true, EventImported: true, EventAddonsAvailable: true, EventLeaseLost: true,
	EventLeaseRecovered: true, EventAPIDown: true, EventAPIUp: true, EventDetaching: true, EventDetached: true,
}

// parseScenarios validates the events and orders them by time.
func parseScenarios(events []ScenarioEvent, clusters map[string]*aigriddraftv1.ManagedCluster) ([]*scenario, error) {
	out := make([]*scenario, 0, len(events))
	for i, ev := range events {
		if !knownEvents[ev.Event] {
			return nil, fmt.Errorf("scenario %d: unknown event %q", i, ev.Event)
		}
		if _, ok := clusters[ev.Cluster]; !ok {
			return nil, fmt.Errorf("scenario %d: cluster %q is not in the fixtures", i, ev.Cluster)
		}
		sc := &scenario{ScenarioEvent: ev}
		if len(ev.Spoke) > 0 {
			sc.spoke = &aigriddraftv1.ManagedClusterStatus{}
			if err := protojson.Unmarshal(ev.Spoke, sc.spoke); err != nil {
				return nil, fmt.Errorf("scenario %d: spoke: %w", i, err)
			}
		}
		out = append(out, sc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Duration() < out[j].At.Duration() })
	return out, nil
}

// advance fires every due event. It is called under s.mu by every read and
// write, so transitions are evaluated lazily from the clock and no goroutine
// is needed. It returns the scenario time (see Server.scenarioTime).
func (s *Server) advance() time.Time {
	now := s.clock()
	elapsed := time.Duration(float64(now.Sub(s.start)) * s.speed)
	for _, sc := range s.scenarios {
		if sc.applied || sc.At.Duration() > elapsed {
			continue
		}
		sc.applied = true
		c, ok := s.clusters[sc.Cluster]
		if !ok {
			continue // detached earlier
		}
		// The event happened at its scheduled wall-clock time, not when it
		// was noticed, so lastTransitionTime and timeAdded are consistent.
		at := s.start.Add(time.Duration(float64(sc.At.Duration()) / s.speed))
		s.applyEvent(c, sc, at)
		s.logger.Info("scenario", "cluster", sc.Cluster, "event", sc.Event, "at", sc.At.Duration())
	}
	return s.start.Add(elapsed)
}

func (s *Server) applyEvent(c *aigriddraftv1.ManagedCluster, sc *scenario, now time.Time) {
	switch sc.Event {
	case EventJoined:
		if sc.spoke != nil {
			if c.Status == nil {
				c.Status = &aigriddraftv1.ManagedClusterStatus{}
			}
			// A copy, so the stored object never aliases the scenario.
			spoke := proto.Clone(sc.spoke).(*aigriddraftv1.ManagedClusterStatus)
			c.Status.KubernetesVersion = spoke.GetKubernetesVersion()
			c.Status.Capacity = spoke.GetCapacity()
			c.Status.Allocatable = spoke.GetAllocatable()
			c.Status.ClusterClaims = spoke.GetClusterClaims()
			sortClaims(c)
		}
		setCondition(c, now, CondJoined, aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonJoined, MessageJoined)
		setCondition(c, now, CondAvailable, aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonAvailable, MessageAvailable)
		setCondition(c, now, CondClockSynced, aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonClockSynced, MessageClockSynced)
		autodetectLabels(c)
		syncTaints(c, now)
	case EventImported:
		setCondition(c, now, CondImportSucceeded, aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonImported, MessageImported)
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
		setCondition(c, now, CondAvailable, aigriddraftv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED, ReasonLeaseStopped, MessageLeaseStopped)
		syncTaints(c, now)
		setAddonLabels(c, AddonUnreachable)
	case EventLeaseRecovered:
		setCondition(c, now, CondAvailable, aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonAvailable, MessageAvailable)
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
		setCondition(c, now, CondAvailable, aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAPIServerUnavail, fmt.Sprintf(MessageAPIServerUnavailFm, code, detail))
		syncTaints(c, now)
	case EventAPIUp:
		setCondition(c, now, CondAvailable, aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonAvailable, MessageAvailable)
		syncTaints(c, now)
	case EventDetaching:
		if c.DeletionTimestamp == nil {
			c.Generation++ // the API server bumps generation when it marks an object deleting (apiserver registry/generic/registry/store.go markAsDeleting L1016-1021)
		}
		c.DeletionTimestamp = timestamppb.New(now)
		setCondition(c, now, CondImportSucceeded, aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonDetaching, MessageDetaching)
		setDeletingCondition(c, now)
	case EventDetached:
		delete(s.clusters, c.GetName())
		return
	}
	s.bumpResourceVersion(c)
}
