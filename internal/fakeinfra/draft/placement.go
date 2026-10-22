package draft

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Strings the hub's placement controller writes on a Placement and a
// PlacementDecision (cluster.open-cluster-management.io/v1beta1). Every value
// comes from docs/draft/research/acm.md section 3 and was re-checked against
// open-cluster-management-io/ocm at 8b0d31f (`ocm:` paths below are
// pkg/placement/ unless they say otherwise). Keep them verbatim.
const (
	CondPlacementSatisfied     = "PlacementSatisfied"
	CondPlacementMisconfigured = "PlacementMisconfigured"

	// PlacementSatisfied (ocm:controllers/scheduling/scheduling_controller.go:317-360).
	ReasonAllDecisionsScheduled         = "AllDecisionsScheduled"
	MessageAllDecisionsScheduled        = "All cluster decisions scheduled"
	ReasonNotAllDecisionsScheduled      = "NotAllDecisionsScheduled"
	MessageNotAllDecisionsScheduledFm   = "%d cluster decisions unscheduled"
	ReasonNoManagedClusterSetBindings   = "NoManagedClusterSetBindings"
	MessageNoManagedClusterSetBindings  = "No valid ManagedClusterSetBindings found in placement namespace"
	ReasonNoIntersection                = "NoIntersection"
	MessageNoIntersectionFm             = "None of ManagedClusterSets %s is bound to placement namespace"
	ReasonAllManagedClusterSetsEmpty    = "AllManagedClusterSetsEmpty"
	MessageAllManagedClusterSetsEmptyFm = "All ManagedClusterSets %s have no member ManagedCluster"
	ReasonNoManagedClusterMatched       = "NoManagedClusterMatched"
	MessageNoManagedClusterMatched      = "No ManagedCluster matches any of the cluster predicate"

	// PlacementMisconfigured (ocm:controllers/scheduling/scheduling_controller.go:362-378):
	// False/Succeedconfigured, or True/Misconfigured with `<plugin>:<message>`
	// when a scheduling plugin rejects the spec. The only such plugin the
	// console's Placements reach is TaintToleration, with the two messages
	// below (ocm:plugins/tainttoleration/taint_toleration.go:58-74).
	ReasonSucceedconfigured      = "Succeedconfigured"
	MessageSucceedconfigured     = "Placement configurations check pass"
	ReasonMisconfigured          = "Misconfigured"
	MessageTolerationEmptyKey    = "TaintToleration:If the key is empty, operator must be Exists."
	MessageTolerationExistsValue = "TaintToleration:If the operator is Exists, the value should be empty."
	TaintPreferNoSelect          = "PreferNoSelect"
	TaintNoSelectIfNew           = "NoSelectIfNew"

	// Built-in ManagedClusterSets (acm.md section 2): `global` selects every
	// cluster (LabelSelector {}), `default` the clusters whose
	// `cluster.open-cluster-management.io/clusterset` label is `default`
	// (ExclusiveClusterSetLabel).
	ClusterSetGlobal  = "global"
	ClusterSetDefault = "default"

	// Selector operators (metav1.LabelSelectorRequirement; the claim selector
	// uses the same set).
	OperatorIn           = "In"
	OperatorNotIn        = "NotIn"
	OperatorExists       = "Exists"
	OperatorDoesNotExist = "DoesNotExist"

	// Toleration operators; the CRD defaults an omitted operator to Equal
	// and has no enum on it (api:cluster/v1beta1/0000_02_clusters.open-cluster-management.io_placements.crd.yaml:537-538).
	TolerationEqual  = "Equal"
	TolerationExists = "Exists"

	// LabelModel marks the Placement and the ManifestWorkReplicaSet of a model
	// (acm.md "Proposal": `ai-grid/model=<name>`); the objects are named
	// `model-<name>` in the model's namespace.
	// Its PlacementDecision is `model-<name>-decision-1`. On create the API
	// server applies the CRD defaults (`sortBy: ClusterName`,
	// `scoreRateLimit: 1m`, `prioritizerPolicy.mode: Additive`,
	// `decisionStrategy.groupStrategy.clustersPerDecisionGroup: 100%`,
	// placements.crd.yaml:94, :424, :436, :444); the console's Placement
	// message carries none of them.
	LabelModel          = "ai-grid/model"
	placementNamePrefix = "model-"
)

// maxClusterScore is the most a prioritizer gives a cluster (ocm:plugins/interface.go:23).
const maxClusterScore int64 = 100

// knownOperators are the selector operators the fake evaluates.
var knownOperators = map[string]bool{OperatorIn: true, OperatorNotIn: true, OperatorExists: true, OperatorDoesNotExist: true}

// knownClusterSets are the ManagedClusterSets that exist on the hub.
var knownClusterSets = map[string]bool{ClusterSetGlobal: true, ClusterSetDefault: true}

// placementName is the name of a model's Placement and ManifestWorkReplicaSet.
func placementName(model string) string { return placementNamePrefix + model }

// matchRequirement evaluates one In/NotIn/Exists/DoesNotExist requirement
// against a key/value map, with Kubernetes semantics: NotIn and DoesNotExist
// match when the key is absent.
func matchRequirement(values map[string]string, key, operator string, wanted []string) bool {
	v, present := values[key]
	switch operator {
	case OperatorIn:
		return present && slices.Contains(wanted, v)
	case OperatorNotIn:
		return !present || !slices.Contains(wanted, v)
	case OperatorExists:
		return present
	case OperatorDoesNotExist:
		return !present
	}
	return false
}

// matchesPredicate is the one predicate's requiredClusterSelector: every
// matchLabels entry, every label expression and every claim expression must
// hold (AND inside a predicate, acm.md section 3).
func matchesPredicate(p *aigriddraftv1.Placement, c *aigriddraftv1.ManagedCluster) bool {
	labels := c.GetLabels()
	for k, v := range p.GetLabelSelector().GetMatchLabels() {
		// A matchLabels entry needs the key present with that exact value; an
		// empty value matches an empty label, never an absent one.
		if lv, ok := labels[k]; !ok || lv != v {
			return false
		}
	}
	for _, req := range p.GetLabelSelector().GetMatchExpressions() {
		if !matchRequirement(labels, req.GetKey(), req.GetOperator(), req.GetValues()) {
			return false
		}
	}
	if len(p.GetClaimExpressions()) > 0 {
		claims := map[string]string{}
		for _, cl := range c.GetStatus().GetClusterClaims() {
			claims[cl.GetName()] = cl.GetValue()
		}
		for _, req := range p.GetClaimExpressions() {
			if !matchRequirement(claims, req.GetKey(), req.GetOperator(), req.GetValues()) {
				return false
			}
		}
	}
	return true
}

// misconfiguredToleration is the TaintToleration filter's spec check
// (ocm:plugins/tainttoleration/taint_toleration.go:58-74): an empty key needs
// operator Exists, and Exists takes no value. It returns the
// PlacementMisconfigured message, or "" when the tolerations are valid.
func misconfiguredToleration(p *aigriddraftv1.Placement) string {
	for _, tol := range p.GetTolerations() {
		if tol.GetKey() == "" && tol.GetOperator() != TolerationExists {
			return MessageTolerationEmptyKey
		}
		if tol.GetOperator() == TolerationExists && tol.GetValue() != "" {
			return MessageTolerationExistsValue
		}
	}
	return ""
}

// tolerated reports whether every taint of the cluster is tolerated at `now`
// (ocm:plugins/tainttoleration/taint_toleration.go:116-200): a PreferNoSelect
// taint never filters, a NoSelectIfNew taint filters only a cluster that is
// not already in the decision (`inDecision`), and a NoSelect taint (or a
// NoSelectIfNew one on a new cluster) needs a toleration whose effect is empty
// or equal, whose key is empty (any key) or equal, whose value matches for
// Equal (the default), and whose `tolerationSeconds`, counted from the taint's
// `timeAdded` (the zero time when unset, so already expired), has not run out.
// Both times are scenario time.
func tolerated(p *aigriddraftv1.Placement, c *aigriddraftv1.ManagedCluster, inDecision bool, now time.Time, toScenario func(time.Time) time.Time) bool {
	for _, taint := range c.GetTaints() {
		if taint.GetEffect() == TaintPreferNoSelect || (taint.GetEffect() == TaintNoSelectIfNew && inDecision) {
			continue
		}
		ok := false
		for _, tol := range p.GetTolerations() {
			if tol.GetEffect() != "" && tol.GetEffect() != taint.GetEffect() {
				continue
			}
			if tol.GetKey() != "" && tol.GetKey() != taint.GetKey() {
				continue
			}
			if op := tol.GetOperator(); op != TolerationExists && tol.GetValue() != taint.GetValue() {
				continue // "" or Equal
			}
			if tol.TolerationSeconds != nil {
				added := time.Time{}
				if taint.GetTimeAdded() != nil {
					added = toScenario(taint.GetTimeAdded().AsTime())
				}
				if !now.Before(added.Add(time.Duration(tol.GetTolerationSeconds()) * time.Second)) {
					continue
				}
			}
			ok = true
			break
		}
		if !ok {
			return false
		}
	}
	return true
}

// inClusterSet reports membership in a built-in set.
func inClusterSet(set string, c *aigriddraftv1.ManagedCluster) bool {
	switch set {
	case ClusterSetGlobal:
		return true
	case ClusterSetDefault:
		return c.GetLabels()[LabelClusterSet] == ClusterSetDefault
	}
	return false
}

// bracketList formats names the way the scheduling controller does in its
// messages: `[a,b]`.
func bracketList(names []string) string { return "[" + strings.Join(names, ",") + "]" }

// computePlacement is the scheduling controller's decision for one Placement
// (acm.md section 3 and "What the fake backend must simulate" item 6;
// ocm:controllers/scheduling/scheduling_controller.go:214-282): the clusters
// of the bound sets that are not terminating
// (ocm:controllers/scheduling/helpers.go:73-105), filtered by the predicate
// and by tolerated taints, scored and capped at numberOfClusters
// (ocm:controllers/scheduling/schedule.go:179-338), listed in name order.
// `bound` are the ManagedClusterSets bound into the namespace; `previous` the
// clusters of the current decision, which the Steady prioritizer and the
// NoSelectIfNew taint look at; `now` is scenario time.
func computePlacement(p *aigriddraftv1.Placement, bound []string, clusters map[string]*aigriddraftv1.ManagedCluster, previous map[string]bool, now time.Time, toScenario func(time.Time) time.Time) *aigriddraftv1.PlacementStatus {
	st := &aigriddraftv1.PlacementStatus{}
	misconfigured := ""
	satisfied := func(status aigriddraftv1.ConditionStatus, reason, message string) *aigriddraftv1.PlacementStatus {
		if misconfigured != "" {
			st.Conditions = writeCondition(st.Conditions, now, CondPlacementMisconfigured, cTrue, ReasonMisconfigured, misconfigured)
		} else {
			st.Conditions = writeCondition(st.Conditions, now, CondPlacementMisconfigured, cFalse, ReasonSucceedconfigured, MessageSucceedconfigured)
		}
		st.Conditions = writeCondition(st.Conditions, now, CondPlacementSatisfied, status, reason, message)
		return st
	}
	if len(bound) == 0 {
		return satisfied(cFalse, ReasonNoManagedClusterSetBindings, MessageNoManagedClusterSetBindings)
	}
	sets := p.GetClusterSets()
	if len(sets) == 0 {
		sets = bound // empty = all bound sets
	}
	var usable []string
	for _, set := range sets {
		if slices.Contains(bound, set) && knownClusterSets[set] {
			usable = append(usable, set)
		}
	}
	if len(usable) == 0 {
		return satisfied(cFalse, ReasonNoIntersection, fmt.Sprintf(MessageNoIntersectionFm, bracketList(sets)))
	}
	var members []*aigriddraftv1.ManagedCluster
	for _, c := range clusters {
		if c.GetDeletionTimestamp() != nil {
			continue // a terminating cluster is not available (helpers.go:97-99)
		}
		for _, set := range usable {
			if inClusterSet(set, c) {
				members = append(members, c)
				break
			}
		}
	}
	if len(members) == 0 {
		return satisfied(cFalse, ReasonAllManagedClusterSetsEmpty, fmt.Sprintf(MessageAllManagedClusterSetsEmptyFm, bracketList(usable)))
	}
	var matched []*aigriddraftv1.ManagedCluster
	for _, c := range members {
		if matchesPredicate(p, c) {
			matched = append(matched, c)
		}
	}
	// The TaintToleration filter checks the tolerations only when the
	// predicate left a cluster to filter (taint_toleration.go:52-56); a
	// misconfigured toleration ends the scheduling with no decision, so
	// PlacementSatisfied reads NoManagedClusterMatched.
	if len(matched) > 0 {
		misconfigured = misconfiguredToleration(p)
	}
	var names []string
	if misconfigured == "" {
		for _, c := range matched {
			if tolerated(p, c, previous[c.GetName()], now, toScenario) {
				names = append(names, c.GetName())
			}
		}
	}
	// Default prioritizers Balance and Steady, weight 1 each (schedule.go:151-160),
	// then name on equal scores (:286-293). Steady gives a cluster already in
	// the decision 100 (ocm:plugins/steady/steady.go); Balance would lower the
	// clusters other Placements already select, which the fake does not
	// model, so every cluster gets Balance's full 100 (models.md "Deviations").
	score := func(name string) int64 {
		if previous[name] {
			return 2 * maxClusterScore
		}
		return maxClusterScore
	}
	sort.SliceStable(names, func(i, j int) bool {
		if a, b := score(names[i]), score(names[j]); a != b {
			return a > b
		}
		return names[i] < names[j]
	})
	unscheduled := 0
	if p.NumberOfClusters != nil {
		want := int(p.GetNumberOfClusters())
		if len(names) > want {
			names = names[:max(want, 0)]
		}
		unscheduled = want - len(names)
	}
	sort.Strings(names) // decisions are listed by cluster name (scheduling_controller.go:749-752)
	for _, n := range names {
		st.Decisions = append(st.Decisions, &aigriddraftv1.ClusterDecision{ClusterName: n})
	}
	st.NumberOfSelectedClusters = int32(len(names))
	switch {
	case len(names) == 0:
		return satisfied(cFalse, ReasonNoManagedClusterMatched, MessageNoManagedClusterMatched)
	case unscheduled > 0:
		return satisfied(cFalse, ReasonNotAllDecisionsScheduled, fmt.Sprintf(MessageNotAllDecisionsScheduledFm, unscheduled))
	}
	return satisfied(cTrue, ReasonAllDecisionsScheduled, MessageAllDecisionsScheduled)
}

// decisionSet is the set of clusters in a decision.
func decisionSet(st *aigriddraftv1.PlacementStatus) map[string]bool {
	out := map[string]bool{}
	for _, d := range st.GetDecisions() {
		out[d.GetClusterName()] = true
	}
	return out
}

// validatePlacement is the console's (and the backend's) admission check on
// a Placement, with console wording. Only the toleration effect is a CRD enum
// (placements.crd.yaml:521-529); the API server accepts any selector or
// toleration operator, a negative numberOfClusters (:224, no minimum) and a
// ManagedClusterSet that does not exist (the scheduler then reports
// NoIntersection or NoManagedClusterSetBindings). Tolerations the scheduler
// rejects as misconfigured (an empty key without Exists, Exists with a value)
// pass here and surface as PlacementMisconfigured, as on the hub.
func validatePlacement(p *aigriddraftv1.Placement) error {
	for _, req := range p.GetLabelSelector().GetMatchExpressions() {
		if err := validateRequirement("spec.placement.label_selector.match_expressions", req.GetKey(), req.GetOperator(), req.GetValues()); err != nil {
			return err
		}
	}
	for _, req := range p.GetClaimExpressions() {
		if err := validateRequirement("spec.placement.claim_expressions", req.GetKey(), req.GetOperator(), req.GetValues()); err != nil {
			return err
		}
	}
	for _, tol := range p.GetTolerations() {
		if op := tol.GetOperator(); op != "" && op != TolerationEqual && op != TolerationExists {
			return fmt.Errorf("spec.placement.tolerations: operator %q is not %s or %s", op, TolerationEqual, TolerationExists)
		}
		if tol.GetEffect() != "" && tol.GetEffect() != TaintNoSelect && tol.GetEffect() != "PreferNoSelect" && tol.GetEffect() != "NoSelectIfNew" {
			return fmt.Errorf("spec.placement.tolerations: effect %q is not NoSelect, PreferNoSelect or NoSelectIfNew", tol.GetEffect())
		}
	}
	if p.NumberOfClusters != nil && p.GetNumberOfClusters() < 0 {
		return fmt.Errorf("spec.placement.number_of_clusters must not be negative, got %d", p.GetNumberOfClusters())
	}
	for _, set := range p.GetClusterSets() {
		if !knownClusterSets[set] {
			return fmt.Errorf("spec.placement.cluster_sets: ManagedClusterSet %q does not exist on the hub", set)
		}
	}
	return nil
}

func validateRequirement(path, key, operator string, values []string) error {
	if key == "" {
		return fmt.Errorf("%s: key is required", path)
	}
	if !knownOperators[operator] {
		return fmt.Errorf("%s: operator %q is not In, NotIn, Exists or DoesNotExist", path, operator)
	}
	switch operator {
	case OperatorIn, OperatorNotIn:
		if len(values) == 0 {
			return fmt.Errorf("%s: operator %s needs at least one value", path, operator)
		}
	default:
		if len(values) != 0 {
			return fmt.Errorf("%s: operator %s takes no values", path, operator)
		}
	}
	return nil
}
