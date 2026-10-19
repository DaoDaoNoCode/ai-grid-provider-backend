package draft

import (
	"crypto/rand"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Strings the work agent writes on a ManifestWork and the hub's
// ManifestWorkReplicaSet controller writes on a ManifestWorkReplicaSet. Every
// value comes from docs/draft/research/acm.md section 4 and was re-checked against
// open-cluster-management-io/ocm at 8b0d31f (`ocm:` paths below are pkg/work/
// unless they say otherwise), open-cluster-management-io/api at 2c5da18
// (`api:`) and sdk-go at 592fbe6 (`sdk-go:`); nothing here is invented unless
// its comment says so.
const (
	// ManifestWork conditions (agent).
	CondWorkApplied     = "Applied"
	CondWorkAvailable   = "Available"
	CondWorkProgressing = "Progressing"
	CondWorkDegraded    = "Degraded"

	// Work-level Applied (ocm:spoke/controllers/manifestcontroller/manifestwork_reconciler.go:204-224).
	ReasonAppliedManifestWorkComplete  = "AppliedManifestWorkComplete"
	MessageAppliedManifestWorkComplete = "Apply manifest work complete"
	ReasonAppliedManifestWorkFailed    = "AppliedManifestWorkFailed"
	MessageAppliedManifestWorkFailed   = "Failed to apply manifest work"
	// Work-level Available, aggregated over the manifests
	// (ocm:spoke/conditions/helpers.go:91-126): any False wins, then any
	// Unknown.
	ReasonResourcesAvailable        = "ResourcesAvailable"
	MessageResourcesAvailable       = "All resources are available"
	ReasonResourcesNotAvailable     = "ResourcesNotAvailable"
	MessageResourcesNotAvailableFm  = "%d of %d resources are not available"
	ReasonResourcesStatusUnknown    = "ResourcesStatusUnknown"
	MessageResourcesStatusUnknownFm = "%d of %d resources have unknown status"
	// Rule-derived work conditions: reason, and the message when more than one
	// manifest carries the condition (a single one has its message copied),
	// ocm:spoke/conditions/helpers.go:128-169.
	ReasonConditionRulesAggregated = "ConditionRulesAggregated"
	MessageAggregatedFm            = "Aggregated from %d manifest conditions"

	// Manifest-level conditions (agent): Applied
	// (manifestwork_reconciler.go:412-445), Available
	// (ocm:spoke/objectreader/reader.go:76-110), StatusFeedbackSynced
	// (ocm:spoke/controllers/statuscontroller/availablestatus_controller.go:173-213).
	CondManifestStatusFeedbackSynced = "StatusFeedbackSynced"
	ReasonAppliedManifestComplete    = "AppliedManifestComplete"
	MessageAppliedManifestComplete   = "Apply manifest complete"
	ReasonAppliedManifestFailed      = "AppliedManifestFailed"
	MessageAppliedManifestFailedFm   = "Failed to apply manifest: %s"
	ReasonResourceAvailable          = "ResourceAvailable"
	MessageResourceAvailable         = "Resource is available"
	ReasonResourceNotAvailable       = "ResourceNotAvailable"
	MessageResourceNotAvailable      = "Resource is not available"
	ReasonIncompleteResourceMeta     = "IncompleteResourceMeta"
	MessageIncompleteResourceMeta    = "Resource meta is incomplete"
	ReasonNoStatusFeedbackSynced     = "NoStatusFeedbackSynced"
	ReasonStatusFeedbackSynced       = "StatusFeedbackSynced"
	ReasonConditionRuleEvaluated     = "ConditionRuleEvaluated"

	// ManifestWorkReplicaSet conditions (hub controller); messages are "".
	CondPlacementVerified   = "PlacementVerified"
	CondPlacementRolledOut  = "PlacementRolledOut"
	CondManifestworkApplied = "ManifestworkApplied"

	ReasonAsExpected                = "AsExpected"
	ReasonPlacementDecisionNotFound = "PlacementDecisionNotFound"
	ReasonPlacementDecisionEmpty    = "PlacementDecisionEmpty"
	ReasonNotAsExpected             = "NotAsExpected"
	ReasonRolloutComplete           = "Complete"
	ReasonRolloutProgressing        = "Progressing"
	ReasonProcessing                = "Processing"
	MessageFeatureGateDisabled      = "ManifestWorkReplicaSet feature gate disabled on hub" // acm.md "Proposal"; the console's text for the backend's FailedPrecondition
	MessageReferencedByCatalogFm    = "model %s is referenced by %s; remove those references first"
	// LabelManifestWorkReplicaSetLegacy is the deprecated owner label on a
	// generated work, value `<namespace>.<name>`, still set when that fits 63
	// characters (ocm:hub/controllers/manifestworkreplicasetcontroller/
	// manifestworkreplicaset_deploy_reconcile.go:404-414); the current owner
	// label is LabelManifestWorkReplicaSetOwnerKeyHash (ocm:helper/helpers.go:48).
	LabelManifestWorkReplicaSetLegacy       = "work.open-cluster-management.io/manifestworkreplicaset"
	LabelManifestWorkReplicaSetOwnerKeyHash = "work.open-cluster-management.io/ownerkey-hash"
	LabelPlacementName                      = "work.open-cluster-management.io/placementname"
	// LabelFailApply on a ManagedCluster makes its work fail to apply
	// (acm.md item 11). A console-owned mock label, not a hub one.
	LabelFailApply = "ai-grid-mock/fail-apply"

	// Feedback rule names and the resource identifiers of the two manifests
	// the console's template carries: the model namespace and the
	// LLMInferenceService (acm.md 4, "model deployment ManifestWork").
	FeedbackReady       = "ready"
	FeedbackURL         = "url"
	FeedbackTypeString  = "String"
	manifestNamespace   = "namespaces"
	kindNamespace       = "Namespace"
	llmisvcGroup        = "serving.kserve.io"
	llmisvcVersion      = "v1alpha2"
	llmisvcResource     = "llminferenceservices"
	llmisvcKind         = "LLMInferenceService"
	manifestCount       = 2
	ruleBearingManifest = 1

	// The condition rules' messages are the template author's, that is the
	// console's (the research example writes "InferenceService rolling out").
	MessageRuleProgressing = "LLMInferenceService rolling out"
	MessageRuleDegraded    = "LLMInferenceService not ready"

	// workAvailableAfter is how long after Applied the fake's status
	// controller first syncs (acm.md item 9: t+2 s). The real first pass is
	// triggered by the Applied patch itself (availablestatus_controller.go:74-76),
	// and later passes poll every 10 to 19 s (:102-103,
	// ocm:spoke/options.go:38), which the fake does not model (models.md
	// "Deviations").
	workAvailableAfter = 2 * time.Second
	// generateNameAlphabet is what Kubernetes draws a generateName suffix
	// from; the API server appends the five characters to the base cut to
	// 58 (k8s.io/apiserver@v0.32.3 pkg/storage/names/generate.go:42-54).
	generateNameAlphabet = "bcdfghjklmnpqrstvwxz2456789"
	generateNameLength   = 5
	maxGenerateNameBase  = 63 - generateNameLength
)

// MessageAppliedManifestFailedNoMatch is the apply error when the spoke lacks
// the KServe CRD: BuildResourceMeta cannot map the kind
// (ocm:helper/helpers.go:558-561) and the error becomes the manifest's Applied
// message (manifestwork_reconciler.go:307-308, :416). The resource meta keeps
// group, version, kind, namespace and name but no `resource`.
var MessageAppliedManifestFailedNoMatch = fmt.Sprintf(MessageAppliedManifestFailedFm, `the server doesn't have a resource type "`+llmisvcKind+`"`)

// rolloutPatterns are the CRD's validation patterns of the rollout strategy
// fields (api:cluster/v1alpha1/types_rolloutstrategy.go:74, :88, :145).
var (
	progressDeadlinePattern = regexp.MustCompile(`^(([0-9])+[h|m|s])|None$`)
	intOrPercentPattern     = regexp.MustCompile(`^((100|[0-9]{1,2})%|[0-9]+)$`)
)

// rolloutState is sdk-go's ClusterRolloutStatus for one selected cluster
// (acm.md "Rollout counting").
type rolloutState int

const (
	rolloutToApply rolloutState = iota
	rolloutProgressing
	rolloutSucceeded
	rolloutFailed
	rolloutTimeOut
)

// work is one generated ManifestWork. Times are scenario time.
type work struct {
	cluster string
	name    string
	// startedAt is when the controller created or last updated the work.
	startedAt time.Time
	// agentStart is when the spoke agent first reported, nil while the
	// cluster is not Available (an unreachable agent reports nothing). It is
	// shifted forward by the length of every later disconnection, so the
	// spoke's lifecycle resumes where it stopped.
	agentStart *time.Time
	// frozenAt is set while the agent is disconnected after it reported: the
	// hub keeps the status as it was at that instant until the agent is back.
	frozenAt *time.Time
	// spec is the template the work carries (templateOf the model's spec
	// when it was applied), which the rollout compares with the current one
	// to find stale works, as the controller compares the ManifestWork with
	// the MWRS template: a work waiting its turn in a Progressive update keeps
	// serving the spec it was applied with.
	spec *aigriddraftv1.ModelSpec
	// workGeneration is the ManifestWork's own `metadata.generation`: 1 when
	// the controller creates it, bumped by the API server each time an update
	// changes its spec. A work created while the model is at generation N
	// (a cluster joining later, a placement added by a PATCH) starts at 1.
	workGeneration int64
	// failApply mirrors the cluster's `ai-grid-mock/fail-apply` label when
	// the work was applied.
	failApply bool
}

// workView is a work's derived status at one instant.
type workView struct {
	status *aigriddraftv1.ManifestWorkStatus
	state  rolloutState
	// since is the lastTransitionTime the hub controller hands sdk-go for
	// this work (manifestworkreplicaset_deploy_reconcile.go:255-272): the
	// Degraded condition's for Failed, the Progressing condition's otherwise.
	// progressDeadline and minSuccessTime count from it (sdk-go
	// rollout.go:474-496). Scenario time; zero for ToApply.
	since time.Time
}

// newWorkSuffix draws a Kubernetes generateName suffix.
func newWorkSuffix() string {
	var b [generateNameLength]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	for i := range b {
		b[i] = generateNameAlphabet[int(b[i])%len(generateNameAlphabet)]
	}
	return string(b[:])
}

// noDeadline is sdk-go's "None": no timeout (rollout.go:574-590).
const noDeadline = time.Duration(math.MaxInt64)

// deriveWork is the agent's report at scenario time `now` for a work whose
// template carries the LLMInferenceService `in` (the spoke's lifecycle is the
// hub's, started when the agent applied the work): Applied at once, Available
// after workAvailableAfter, then the rule conditions and the `ready`/`url`
// feedback from the spoke object. Only the spoke agent writes a work's status
// (ocm:spoke), so a cluster whose agent never reported has an empty status:
// no conditions, no manifests. The rollout counts such a work, and one whose
// Applied is not True or whose Progressing is missing, as ToApply
// (manifestworkreplicaset_deploy_reconcile.go:228-308); sdk-go's
// determineRolloutStatus never times out ToApply (only Progressing and Failed
// reach TimeOut, `deadline` after `since`, rollout.go:462-499), so a dead or
// unapplied cluster holds its Progressive slot indefinitely. `at` converts
// scenario time to wall time.
func deriveWork(w *work, in llmisvcInput, now time.Time, deadline time.Duration, at func(time.Time) time.Time) workView {
	st := &aigriddraftv1.ManifestWorkStatus{ClusterName: w.cluster, WorkName: w.name, Generation: w.workGeneration}
	view := workView{status: st, state: rolloutToApply}
	hubNow := now // the rollout's clock: timeouts run on the hub
	if w.agentStart == nil || now.Before(*w.agentStart) {
		return view
	}
	if w.frozenAt != nil && w.frozenAt.Before(now) {
		now = *w.frozenAt // the agent is gone: the hub keeps its last report
	}
	start := *w.agentStart
	st.ObservedGeneration = w.workGeneration
	nsManifest := &aigriddraftv1.ManifestStatus{Resource: &aigriddraftv1.ResourceIdentifier{Ordinal: 0, Version: "v1", Resource: manifestNamespace, Kind: kindNamespace, Name: in.namespace}}
	modelManifest := &aigriddraftv1.ManifestStatus{Resource: &aigriddraftv1.ResourceIdentifier{Ordinal: 1, Group: llmisvcGroup, Version: llmisvcVersion, Resource: llmisvcResource, Kind: llmisvcKind, Namespace: in.namespace, Name: in.name}}
	if w.failApply {
		modelManifest.Resource.Resource = "" // no REST mapping for the kind (helpers.go:558-561)
	}
	st.Manifests = []*aigriddraftv1.ManifestStatus{nsManifest, modelManifest}
	// gen stamps a condition with the work's generation, as the agent does
	// for the Applied conditions (manifestwork_reconciler.go:206, :430,
	// :439), the rule conditions (availablestatus_controller.go:226-229) and
	// the work-level aggregates (ocm:spoke/conditions/helpers.go:98-166).
	// The manifest Available and StatusFeedbackSynced conditions carry none
	// (reader.go:77-109, availablestatus_controller.go:179-212): plain.
	gen := func(conds []*aigriddraftv1.Condition, t time.Time, typ string, status aigriddraftv1.ConditionStatus, reason, message string) []*aigriddraftv1.Condition {
		conds = writeCondition(conds, at(t), typ, status, reason, message)
		condition(conds, typ).ObservedGeneration = w.workGeneration
		return conds
	}
	plain := func(conds []*aigriddraftv1.Condition, t time.Time, typ string, status aigriddraftv1.ConditionStatus, reason, message string) []*aigriddraftv1.Condition {
		return writeCondition(conds, at(t), typ, status, reason, message)
	}
	nsManifest.Conditions = gen(nil, start, CondWorkApplied, cTrue, ReasonAppliedManifestComplete, MessageAppliedManifestComplete)
	if w.failApply {
		st.Conditions = gen(st.Conditions, start, CondWorkApplied, cFalse, ReasonAppliedManifestWorkFailed, MessageAppliedManifestWorkFailed)
		modelManifest.Conditions = gen(nil, start, CondWorkApplied, cFalse, ReasonAppliedManifestFailed, MessageAppliedManifestFailedNoMatch)
	} else {
		st.Conditions = gen(st.Conditions, start, CondWorkApplied, cTrue, ReasonAppliedManifestWorkComplete, MessageAppliedManifestWorkComplete)
		modelManifest.Conditions = gen(nil, start, CondWorkApplied, cTrue, ReasonAppliedManifestComplete, MessageAppliedManifestComplete)
	}
	synced := start.Add(workAvailableAfter)
	if now.Before(synced) {
		// Between the manifest controller's Applied and the status controller's
		// first sync, Available and the rule conditions are simply absent
		// (acm.md item 9): the work is ToApply (no Progressing yet).
		return view
	}
	nsManifest.Conditions = plain(nsManifest.Conditions, synced, CondWorkAvailable, cTrue, ReasonResourceAvailable, MessageResourceAvailable)
	nsManifest.Conditions = plain(nsManifest.Conditions, synced, CondManifestStatusFeedbackSynced, cTrue, ReasonNoStatusFeedbackSynced, "")
	if w.failApply {
		// A resource meta without `resource` cannot be read: Available
		// Unknown/IncompleteResourceMeta (reader.go:77-84), and the status
		// controller skips the feedback and the condition rules of that
		// manifest (availablestatus_controller.go:123-129), so no Progressing
		// or Degraded exists. The aggregate is Unknown, "1 of 2 resources have
		// unknown status" (ocm:spoke/conditions/helpers.go:101-107). The work
		// stays ToApply (Applied not True), which has no timeout.
		st.Conditions = gen(st.Conditions, synced, CondWorkAvailable, cUnknown, ReasonResourcesStatusUnknown, fmt.Sprintf(MessageResourcesStatusUnknownFm, 1, manifestCount))
		modelManifest.Conditions = plain(modelManifest.Conditions, synced, CondWorkAvailable, cUnknown, ReasonIncompleteResourceMeta, MessageIncompleteResourceMeta)
		return view
	}
	st.Conditions = gen(st.Conditions, synced, CondWorkAvailable, cTrue, ReasonResourcesAvailable, MessageResourcesAvailable)
	modelManifest.Conditions = plain(modelManifest.Conditions, synced, CondWorkAvailable, cTrue, ReasonResourceAvailable, MessageResourceAvailable)
	// The spoke object's lifecycle, as the feedback rules and the CEL
	// condition rules see it.
	spoke, marks := llmisvcStatus([]llmisvcGeneration{{in: in}}, now.Sub(start), func(d time.Duration) time.Time { return at(start.Add(d)) })
	ready := condition(spoke.GetConditions(), CondReady)
	if ready != nil {
		modelManifest.Feedback = append(modelManifest.Feedback, &aigriddraftv1.FeedbackValue{Name: FeedbackReady, Type: FeedbackTypeString, Value: conditionStatusString(ready.GetStatus())})
	}
	if spoke.GetUrl() != "" {
		modelManifest.Feedback = append(modelManifest.Feedback, &aigriddraftv1.FeedbackValue{Name: FeedbackURL, Type: FeedbackTypeString, Value: spoke.GetUrl()})
	}
	// StatusFeedbackSynced is removed and added again whenever the feedback
	// values change (availablestatus_controller.go:143-148), so its time is
	// the last change.
	modelManifest.Conditions = plain(modelManifest.Conditions, later(synced, start.Add(marks.feedback)), CondManifestStatusFeedbackSynced, cTrue, ReasonStatusFeedbackSynced, "")
	// The template's rules: Progressing while Ready is not True, Degraded
	// while Ready is False with a terminal reason (terminalReadyReasons). A
	// rule is first evaluated at the first sync.
	progressing, progressingAt := cTrue, synced
	if ready.GetStatus() == cTrue {
		progressing, progressingAt = cFalse, later(synced, start.Add(marks.readyStatus))
	}
	degraded, degradedAt := cFalse, synced
	if isTerminalFailure(ready) {
		degraded, degradedAt = cTrue, later(synced, start.Add(marks.terminal))
	}
	modelManifest.Conditions = gen(modelManifest.Conditions, synced, CondWorkProgressing, cTrue, ReasonConditionRuleEvaluated, MessageRuleProgressing)
	modelManifest.Conditions = gen(modelManifest.Conditions, progressingAt, CondWorkProgressing, progressing, ReasonConditionRuleEvaluated, MessageRuleProgressing)
	modelManifest.Conditions = gen(modelManifest.Conditions, synced, CondWorkDegraded, cFalse, ReasonConditionRuleEvaluated, MessageRuleDegraded)
	modelManifest.Conditions = gen(modelManifest.Conditions, degradedAt, CondWorkDegraded, degraded, ReasonConditionRuleEvaluated, MessageRuleDegraded)
	// Work-level aggregation: one rule-bearing manifest, so its message is
	// copied verbatim (acm.md 4, condition strings table).
	st.Conditions = gen(st.Conditions, synced, CondWorkProgressing, cTrue, ReasonConditionRulesAggregated, aggregatedMessage(MessageRuleProgressing, ruleBearingManifest))
	st.Conditions = gen(st.Conditions, progressingAt, CondWorkProgressing, progressing, ReasonConditionRulesAggregated, aggregatedMessage(MessageRuleProgressing, ruleBearingManifest))
	st.Conditions = gen(st.Conditions, synced, CondWorkDegraded, cFalse, ReasonConditionRulesAggregated, aggregatedMessage(MessageRuleDegraded, ruleBearingManifest))
	st.Conditions = gen(st.Conditions, degradedAt, CondWorkDegraded, degraded, ReasonConditionRulesAggregated, aggregatedMessage(MessageRuleDegraded, ruleBearingManifest))

	// clusterRolloutStatusFunc (manifestworkreplicaset_deploy_reconcile.go:255-272).
	switch {
	case progressing == cTrue && degraded == cTrue:
		view.state, view.since = rolloutFailed, degradedAt
	case progressing == cTrue:
		view.state, view.since = rolloutProgressing, progressingAt
	default:
		view.state, view.since = rolloutSucceeded, progressingAt
	}
	if view.state != rolloutSucceeded && deadline != noDeadline && !hubNow.Before(view.since.Add(deadline)) {
		view.state = rolloutTimeOut
	}
	return view
}

// later is the later of two instants.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// aggregatedMessage is the work-level message of a rule-derived condition:
// the manifest's own message when exactly one manifest carries the condition,
// otherwise "Aggregated from <n> manifest conditions".
func aggregatedMessage(single string, n int) string {
	if n == 1 {
		return single
	}
	return fmt.Sprintf(MessageAggregatedFm, n)
}

// validIntOrPercent checks a rollout count against the CRD pattern
// `^((100|[0-9]{1,2})%|[0-9]+)$`; the messages are the console's wording.
func validIntOrPercent(s string) error {
	if s == "" || intOrPercentPattern.MatchString(s) {
		return nil
	}
	if strings.HasSuffix(s, "%") {
		return fmt.Errorf("%q is not a percentage between 0%% and 100%%", s)
	}
	return fmt.Errorf("%q is not a non-negative integer or a percentage", s)
}

// rolloutSize is sdk-go's calculateRolloutSize (rollout.go:521-546): an
// integer, or a percentage of total rounded up; empty, a result of zero or
// less, or one above total yields def. s must be valid (validIntOrPercent).
func rolloutSize(s string, total, def int) int {
	n := def
	if pct, ok := strings.CutSuffix(s, "%"); ok {
		p, _ := strconv.Atoi(pct)
		n = int(math.Ceil(float64(p) / 100 * float64(total)))
	} else if s != "" {
		n, _ = strconv.Atoi(s)
	}
	if n <= 0 || n > total {
		n = def
	}
	return n
}

// parseProgressDeadline is sdk-go's parseTimeout (rollout.go:574-590) behind
// the CRD pattern (types_rolloutstrategy.go:74): "" and `None` mean no
// timeout, `0s` an immediate one. A value the pattern lets through but
// time.ParseDuration rejects (it anchors only the start) fails the hub's
// rollout at run time; the fake rejects it on write (console check).
func parseProgressDeadline(s string) (time.Duration, error) {
	if s == "" || s == "None" {
		return noDeadline, nil
	}
	d, err := time.ParseDuration(s)
	if !progressDeadlinePattern.MatchString(s) || err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 5m or 1h, or None", s)
	}
	return d, nil
}

// parseMinSuccessTime reads `minSuccessTime`, a metav1.Duration; empty is 0.
func parseMinSuccessTime(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration such as 30s or 5m", s)
	}
	return d, nil
}

// rolloutParams are the parsed rollout strategy fields.
type rolloutParams struct {
	strategy         aigriddraftv1.RolloutStrategy
	maxConcurrency   string
	maxFailures      string
	progressDeadline time.Duration // noDeadline for None
	minSuccessTime   time.Duration
}

// validateRollout parses the strategy fields; the messages are the console's
// wording for the CRD's IntOrString and duration validation. maxConcurrency
// exists only on RolloutProgressive (api:cluster/v1alpha1/
// types_rolloutstrategy.go:120-149), so it is rejected for the other two.
func validateRollout(r *aigriddraftv1.Rollout) (rolloutParams, error) {
	p := rolloutParams{strategy: r.GetStrategy(), maxConcurrency: strings.TrimSpace(r.GetMaxConcurrency()), maxFailures: strings.TrimSpace(r.GetMaxFailures())}
	if p.strategy == aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_UNSPECIFIED {
		p.strategy = aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_ALL
	}
	if _, ok := aigriddraftv1.RolloutStrategy_name[int32(p.strategy)]; !ok {
		return p, fmt.Errorf("spec.rollout.strategy %d is not All, Progressive or ProgressivePerGroup", p.strategy)
	}
	if err := validIntOrPercent(p.maxConcurrency); err != nil {
		return p, fmt.Errorf("spec.rollout.max_concurrency: %w", err)
	}
	if p.maxConcurrency != "" && p.strategy != aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE {
		return p, fmt.Errorf("spec.rollout.max_concurrency applies only to the Progressive strategy; All and ProgressivePerGroup have no maxConcurrency")
	}
	if err := validIntOrPercent(p.maxFailures); err != nil {
		return p, fmt.Errorf("spec.rollout.max_failures: %w", err)
	}
	var err error
	if p.progressDeadline, err = parseProgressDeadline(strings.TrimSpace(r.GetProgressDeadline())); err != nil {
		return p, fmt.Errorf("spec.rollout.progress_deadline: %w", err)
	}
	if p.minSuccessTime, err = parseMinSuccessTime(strings.TrimSpace(r.GetMinSuccessTime())); err != nil {
		return p, fmt.Errorf("spec.rollout.min_success_time: %w", err)
	}
	return p, nil
}

// workNameFor builds a generated work's name from the MWRS name: the
// controller sets generateName to the MWRS name with no separator
// (manifestworkreplicaset_deploy_reconcile.go:429-434) and the API server
// appends the suffix to the name cut to 58 characters.
func workNameFor(mwrs, suffix string) string {
	if len(mwrs) > maxGenerateNameBase {
		mwrs = mwrs[:maxGenerateNameBase]
	}
	return mwrs + suffix
}

// sortWorks orders works by cluster name, the order the decisions use.
func sortWorks(works []*work) {
	sort.Slice(works, func(i, j int) bool { return works[i].cluster < works[j].cluster })
}
