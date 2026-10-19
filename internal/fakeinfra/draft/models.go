package draft

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/draft/celfilter"
)

// Models are KServe LLMInferenceServices on the hub, optionally fanned out to
// managed clusters by an ACM Placement and a ManifestWorkReplicaSet
// (docs/draft/research/models-maas.md 8, docs/draft/research/acm.md "Proposal"). The store
// keeps the desired state and the few facts the controllers would have
// written (when a model was created, when each generated work was applied);
// everything else, the LLMInferenceService conditions, the Placement decisions,
// the work conditions and feedback, the rollout summary, is derived on every
// read from the clock and the current clusters, the way the hub's controllers
// would have left it (kserve.go, placement.go, work.go).

// Field-mask paths UpdateModel accepts.
const (
	PathSpecReplicas       = "spec.replicas"
	PathSpecMainImage      = "spec.main.image"
	PathSpecMainEnv        = "spec.main.env"
	PathSpecPlacement      = "spec.placement"
	PathSpecRollout        = "spec.rollout"
	PathMetadataLabels     = "metadata.labels"
	PathMetadataAnnotation = "metadata.annotations"
)

// modelPaths is the list UpdateModel names in its InvalidArgument messages.
var modelPaths = []string{PathSpecReplicas, PathSpecMainImage, PathSpecMainEnv, PathSpecPlacement, PathSpecRollout, PathMetadataLabels, PathMetadataAnnotation}

// Validation messages. The CRD's CEL texts were read from the hub's
// llminferenceservices.serving.kserve.io CRD (v1alpha2 `spec` and
// `spec.scaling` x-kubernetes-validations) and are verbatim; the rest is the
// console's wording for rules the API server enforces with a message the
// research did not capture (marked "console wording").
const (
	// MsgReplicasScalingExclusive is the `spec` CEL rule
	// `!(has(self.replicas) && has(self.scaling))`.
	MsgReplicasScalingExclusive = "replicas and scaling are mutually exclusive; use scaling for autoscaled deployments or replicas for static deployments"
	// MsgScalingNeedsKeda, MsgKedaTriggerRequired and MsgMinMaxReplicas are
	// the `spec.scaling` CEL rules, in the CRD's order.
	MsgScalingNeedsKeda    = "either wva or keda must be specified when scaling is configured"
	MsgKedaTriggerRequired = "at least one trigger is required when using direct KEDA scaling"
	MsgMinMaxReplicas      = "minReplicas cannot exceed maxReplicas"
	// The CRD's openAPI schema rules, in the API server's field-error format
	// (hub CRD v1alpha2: spec.replicas minimum 0; spec.scaling required
	// [maxReplicas]; maxReplicas minimum 1; minReplicas minimum 1, default 1).
	MsgReplicasMinimumFm    = "spec.replicas: Invalid value: %d: spec.replicas in body should be greater than or equal to 0"
	MsgMaxReplicasRequired  = "spec.scaling.maxReplicas: Required value"
	MsgMaxReplicasMinimumFm = "spec.scaling.maxReplicas: Invalid value: %d: spec.scaling.maxReplicas in body should be greater than or equal to 1"
	MsgMinReplicasMinimumFm = "spec.scaling.minReplicas: Invalid value: %d: spec.scaling.minReplicas in body should be greater than or equal to 1"
	// MsgDNS1123SubdomainFm is apimachinery's IsDNS1123Subdomain message, the
	// check the API server applies to a custom resource's metadata.name
	// (NameIsDNSSubdomain); MsgDNS1123LabelFm is IsDNS1123Label, applied to a
	// namespace name.
	MsgDNS1123SubdomainFm = "%s: Invalid value: %q: a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"
	MsgDNS1123LabelFm     = "%s: Invalid value: %q: a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')"
	// MsgAlreadyExistsFm is the API server's AlreadyExists message.
	MsgAlreadyExistsFm = llmisvcResource + "." + llmisvcGroup + " %q already exists"
	// Console wording.
	MsgModelURIRequired   = "spec.model.uri is required"
	MsgModelURISchemeFm   = "spec.model.uri %q: unsupported scheme; expected one of hf://, s3://, pvc://, oci://"
	MsgRolloutNoPlacement = "spec.rollout requires spec.placement"
)

var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

const (
	maxDNSLabelLength     = 63
	maxDNSSubdomainLength = 253
)

// model is one stored model with the controller facts the derivation needs.
// Times are scenario time (see Server.scenarioTime).
type model struct {
	obj *aigriddraftv1.Model
	// created is when what serves the model today was created: the model's
	// creation, or the moment a placement was added or removed.
	created time.Time
	// live marks a fixture model without creationTimestamp: created when the
	// fake starts, so its lifecycle plays live.
	live bool
	// baseSpec is the spec at `created` (generation 1 of the
	// LLMInferenceService that serves the model today); epochs are the later
	// spec changes that change that LLMInferenceService (templateOf), its
	// generation 2, 3, ..., with the spec each one wrote: the Deployment rolls
	// out again from each one, and the latest one is when the
	// ManifestWorkReplicaSet's template last changed. A change of only
	// `placement` or `rollout` writes the Placement or the MWRS's
	// placementRefs, not the LLMInferenceService, and adds no epoch.
	baseSpec *aigriddraftv1.ModelSpec
	epochs   []specEpoch
	// works are the generated ManifestWorks, one per selected cluster.
	works []*work
	// candidateSince is when each cluster entered the Placement decision.
	candidateSince map[string]time.Time
	// scheduleChangedAt is when `rollout` or `placement` last changed without
	// a template change, or when the decision last lost a cluster or changed
	// its count (zero when never): no work is re-applied, but the works still
	// to apply are scheduled under the new settings from then on, never at an
	// earlier instant judged with them.
	scheduleChangedAt time.Time
	// decisions is the Placement's status as the scheduling controller last
	// wrote it; the next pass reads its clusters (Steady, NoSelectIfNew).
	decisions *aigriddraftv1.PlacementStatus
	// Conditions the hub controllers write, kept so lastTransitionTime moves
	// only on a status change.
	placementConds []*aigriddraftv1.Condition
	rolloutConds   []*aigriddraftv1.Condition
}

// specEpoch is one generation after the first: when it was written and what.
type specEpoch struct {
	at   time.Time
	spec *aigriddraftv1.ModelSpec
}

// templateOf is the part of a spec that ends up in the LLMInferenceService,
// the ManifestWorkReplicaSet's `manifestWorkTemplate`: everything but
// `placement`, which is the separate Placement object, and `rollout`, which
// is the MWRS's `spec.placementRefs[].rolloutStrategy`. The model's labels and
// annotations are metadata, outside the spec, and the fake copies none of
// them into the manifest. Two specs with the same
// template give equal ManifestWorks, so the controller leaves a work alone
// across a change of only those fields (ocm:pkg/work/hub/controllers/
// manifestworkreplicasetcontroller/manifestworkreplicaset_deploy_reconcile.go:
// 80-89 compares the work with the template, ManifestWorkEqual).
func templateOf(spec *aigriddraftv1.ModelSpec) *aigriddraftv1.ModelSpec {
	t := cloneSpec(spec)
	t.Placement, t.Rollout = nil, nil
	return t
}

func modelKey(namespace, name string) string { return namespace + "/" + name }

// cloneSpec returns an owned copy of a spec.
func cloneSpec(spec *aigriddraftv1.ModelSpec) *aigriddraftv1.ModelSpec {
	return proto.Clone(spec).(*aigriddraftv1.ModelSpec)
}

// inputFor is the lifecycle input of one spec of the model on a cluster domain.
func (s *Server) inputFor(m *model, spec *aigriddraftv1.ModelSpec, appsDomain string) llmisvcInput {
	return newLLMISvcInput(&aigriddraftv1.Model{Name: m.obj.GetName(), Metadata: m.obj.GetMetadata(), Spec: spec}, s.presets, appsDomain)
}

// scenarioTime maps a wall-clock instant to the scenario clock, which runs
// `speed` times faster from the fake's start; wallTime is the inverse. Every
// stored model time is scenario time so the lifecycle offsets (kserve.go,
// work.go) apply unscaled, and output timestamps are converted back.
func (s *Server) scenarioTime(wall time.Time) time.Time {
	return s.start.Add(time.Duration(float64(wall.Sub(s.start)) * s.speed))
}

func (s *Server) wallTime(scenario time.Time) time.Time {
	return s.start.Add(time.Duration(float64(scenario.Sub(s.start)) / s.speed))
}

// bindingsFor is the ManagedClusterSets bound into a namespace: `global` by
// default, or what models.json says for that namespace (an empty list means
// no binding, the dev hub's state for a namespace nobody prepared).
func (s *Server) bindingsFor(namespace string) []string {
	if bound, ok := s.bindings[namespace]; ok {
		return bound
	}
	return []string{ClusterSetGlobal}
}

// validateModelName is the API server's DNS-1123 subdomain check on a
// custom resource's name (253 characters, dots allowed).
func validateModelName(field, name string) error {
	if name == "" {
		return status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	if len(name) > maxDNSSubdomainLength || !dns1123Subdomain.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, MsgDNS1123SubdomainFm, field, name)
	}
	return nil
}

// validateNamespaceName is the API server's DNS-1123 label check on a
// namespace name.
func validateNamespaceName(field, name string) error {
	if name == "" {
		return status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	if len(name) > maxDNSLabelLength || !dns1123Label.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, MsgDNS1123LabelFm, field, name)
	}
	return nil
}

// validateModelSpec is what the API server (CRD schema and CEL rules) rejects
// on an LLMInferenceService, plus the console's (and the backend's) own
// checks, as InvalidArgument. The replicas minimum, the scaling maxReplicas
// presence and minimum and the minReplicas minimum are the CRD's openAPI
// schema, with the API server's field-error texts; the CEL texts are the
// CRD's. Schema errors come before CEL errors, as the API server lists them
// (the fake returns the first). The URI scheme, the Placement operators,
// numberOfClusters and cluster-set existence, and the rollout field rules are
// console checks with console wording: the CRD types `spec.model.uri` as a
// plain string (hub CRD, v1alpha2 spec.model.uri) and the KServe webhook does
// not check the scheme. A `min_replicas` of 0 is proto's unset, the CRD's
// default 1.
func validateModelSpec(spec *aigriddraftv1.ModelSpec) error {
	if spec == nil {
		return status.Error(codes.InvalidArgument, "spec is required")
	}
	uri := spec.GetModel().GetUri()
	if uri == "" {
		return status.Error(codes.InvalidArgument, MsgModelURIRequired)
	}
	if !slices.ContainsFunc(uriSchemes, func(scheme string) bool { return strings.HasPrefix(uri, scheme) && len(uri) > len(scheme) }) {
		return status.Errorf(codes.InvalidArgument, MsgModelURISchemeFm, uri)
	}
	if spec.Replicas != nil && spec.GetReplicas() < 0 {
		return status.Errorf(codes.InvalidArgument, MsgReplicasMinimumFm, spec.GetReplicas())
	}
	if sc := spec.GetScaling(); sc != nil {
		switch {
		case sc.GetMaxReplicas() == 0:
			return status.Error(codes.InvalidArgument, MsgMaxReplicasRequired)
		case sc.GetMaxReplicas() < 1:
			return status.Errorf(codes.InvalidArgument, MsgMaxReplicasMinimumFm, sc.GetMaxReplicas())
		case sc.GetMinReplicas() < 0:
			return status.Errorf(codes.InvalidArgument, MsgMinReplicasMinimumFm, sc.GetMinReplicas())
		}
		if spec.Replicas != nil {
			return status.Error(codes.InvalidArgument, MsgReplicasScalingExclusive)
		}
		if !sc.GetKeda() {
			return status.Error(codes.InvalidArgument, MsgScalingNeedsKeda)
		}
		if len(sc.GetTriggers()) == 0 {
			return status.Error(codes.InvalidArgument, MsgKedaTriggerRequired)
		}
		if max(sc.GetMinReplicas(), 1) > sc.GetMaxReplicas() {
			return status.Error(codes.InvalidArgument, MsgMinMaxReplicas)
		}
	}
	if p := spec.GetPlacement(); p != nil {
		if err := validatePlacement(p); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	} else if spec.GetRollout() != nil {
		return status.Error(codes.InvalidArgument, MsgRolloutNoPlacement)
	}
	if r := spec.GetRollout(); r != nil {
		if _, err := validateRollout(r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return nil
}

// checkGate is the backend's check for the ManifestWorkReplicaSet controller
// before it writes an MWRS (acm.md 4, "feature gate OFF").
func (s *Server) checkGate(spec *aigriddraftv1.ModelSpec) error {
	if spec.GetPlacement() != nil && !s.capabilities.GetManifestWorkReplicaSetEnabled() {
		return status.Error(codes.FailedPrecondition, MessageFeatureGateDisabled)
	}
	return nil
}

// bumpModelVersion stamps a written model with the next store revision.
// Must be called under s.mu.
func (s *Server) bumpModelVersion(m *aigriddraftv1.Model) {
	s.resourceVersion++
	m.Metadata.ResourceVersion = strconv.FormatInt(s.resourceVersion, 10)
}

// normalizeModel fills what the KServe defaulting webhook and the API server
// fill on admission: `spec.model.name` from the name, empty maps for labels
// and annotations, and strips every output-only field a client may have sent.
func normalizeModel(obj *aigriddraftv1.Model) *aigriddraftv1.Model {
	m := proto.Clone(obj).(*aigriddraftv1.Model)
	if m.Metadata == nil {
		m.Metadata = &aigriddraftv1.ObjectMeta{}
	}
	m.Metadata.Labels = copyMap(m.Metadata.GetLabels())
	m.Metadata.Annotations = copyMap(m.Metadata.GetAnnotations())
	m.Metadata.Uid, m.Metadata.ResourceVersion, m.Metadata.Generation = "", "", 0
	m.Metadata.CreationTimestamp, m.Metadata.DeletionTimestamp, m.Metadata.Finalizers = nil, nil, nil
	m.Status = nil
	if m.Spec.GetModel() != nil && m.Spec.Model.GetName() == "" {
		m.Spec.Model.Name = m.GetName() // llm_inference_service_defaults.go
	}
	return m
}

// seedModel validates one fixture model as CreateModel would and stores it
// as created at its creationTimestamp, or at the fake's start when the fixture
// carries none (so it starts its lifecycle live). Must be called before Serve.
func (s *Server) seedModel(obj *aigriddraftv1.Model) error {
	if err := validateModelName("metadata.name", obj.GetName()); err != nil {
		return fmt.Errorf("fixture model %q: %s", obj.GetName(), status.Convert(err).Message())
	}
	if err := validateNamespaceName("metadata.namespace", obj.GetMetadata().GetNamespace()); err != nil {
		return fmt.Errorf("fixture model %q: %s", obj.GetName(), status.Convert(err).Message())
	}
	if err := validateModelSpec(obj.GetSpec()); err != nil {
		return fmt.Errorf("fixture model %q: %s", obj.GetName(), status.Convert(err).Message())
	}
	key := modelKey(obj.GetMetadata().GetNamespace(), obj.GetName())
	if _, dup := s.models[key]; dup {
		return fmt.Errorf("fixture model %s listed twice", key)
	}
	m := normalizeModel(obj)
	m.Metadata.Uid = obj.GetMetadata().GetUid()
	if m.Metadata.Uid == "" {
		m.Metadata.Uid = s.newID()
	}
	m.Metadata.Generation = max(obj.GetMetadata().GetGeneration(), 1)
	created := s.start
	live := true
	if ts := obj.GetMetadata().GetCreationTimestamp(); ts != nil {
		created, live = s.scenarioTime(ts.AsTime()), false
		if !ts.AsTime().Before(s.start) {
			return fmt.Errorf("fixture model %s: creationTimestamp %s is not in the past; omit it to create the model when the fake starts", key, ts.AsTime().Format(time.RFC3339))
		}
	}
	m.Metadata.CreationTimestamp = timestamppb.New(s.wallTime(created))
	if rv := obj.GetMetadata().GetResourceVersion(); rv != "" {
		m.Metadata.ResourceVersion = rv
		if n, err := strconv.ParseInt(rv, 10, 64); err == nil && n > s.resourceVersion {
			s.resourceVersion = n
		}
	} else {
		s.bumpModelVersion(m)
	}
	stored := &model{obj: m, created: created, live: live}
	s.models[key] = stored
	s.resetSeeded(stored)
	return nil
}

// resetSeeded (re)starts a fixture model's controller facts from its creation
// time: no works yet, and every cluster its Placement selects today counted
// as selected since the model was created, so a model created days ago reads
// as rolled out, not as rolling out from the moment the fake started. Must
// be called under s.mu, before the next reconcile.
func (s *Server) resetSeeded(m *model) {
	if m.live {
		m.created = s.start
	} else {
		m.created = s.scenarioTime(m.obj.GetMetadata().GetCreationTimestamp().AsTime())
	}
	m.obj.Metadata.CreationTimestamp = timestamppb.New(s.wallTime(m.created))
	m.works, m.epochs, m.placementConds, m.rolloutConds, m.decisions = nil, nil, nil, nil, nil
	m.scheduleChangedAt = time.Time{}
	m.baseSpec = cloneSpec(m.obj.GetSpec())
	m.candidateSince = map[string]time.Time{}
	if p := m.obj.GetSpec().GetPlacement(); p != nil {
		decisions := computePlacement(p, s.bindingsFor(m.obj.GetMetadata().GetNamespace()), s.clusters, nil, m.created, s.scenarioTime)
		for _, d := range decisions.GetDecisions() {
			m.candidateSince[d.GetClusterName()] = m.created
		}
	}
}

// reconcileModels is the controllers' pass, run under s.mu on every read and
// write after the cluster scenarios: it drops deleted models whose works are
// gone, recomputes each placed model's decisions from the current clusters,
// removes the works of clusters that left, and applies works to clusters that
// joined as the rollout strategy allows. `now` is scenario time.
func (s *Server) reconcileModels(now time.Time) {
	for _, m := range s.models {
		p := m.obj.GetSpec().GetPlacement()
		if p == nil {
			m.works, m.placementConds, m.rolloutConds, m.decisions = nil, nil, nil, nil
			continue
		}
		decisions := computePlacement(p, s.bindingsFor(m.obj.GetMetadata().GetNamespace()), s.clusters, decisionSet(m.decisions), now, s.scenarioTime)
		prevDecisions := m.decisions // nil before the model's first pass
		m.decisions = decisions
		for _, c := range decisions.GetConditions() {
			m.placementConds = writeCondition(m.placementConds, s.wallTime(now), c.GetType(), c.GetStatus(), c.GetReason(), c.GetMessage())
		}
		if !s.capabilities.GetManifestWorkReplicaSetEnabled() {
			continue // nothing reconciles the MWRS
		}
		selected := map[string]bool{}
		joined, left := false, false
		for _, d := range decisions.GetDecisions() {
			selected[d.GetClusterName()] = true
			if _, ok := m.candidateSince[d.GetClusterName()]; !ok {
				joined = true
			}
		}
		for c := range m.candidateSince {
			if !selected[c] {
				left = true
			}
		}
		// A decision that lost a cluster (and its work, which may have held a
		// slot) or changed its count (behind a percentage maxConcurrency or
		// maxFailures) is the controller's input from the moment the fake
		// notices it, like every MWRS time; the works still to apply are
		// scheduled under it from then on, never earlier. Up to that moment
		// the previous decision held, so the rollout first catches up under
		// it (a slot that freed in between is used when it freed, however
		// often the fake is read). A joining cluster is itself floored by
		// candidateSince.
		if prevDecisions != nil && (left || joined && len(m.candidateSince) > 0) {
			for _, w := range m.works {
				s.observeAgent(w, now)
			}
			s.rollout(m, prevDecisions, now)
			m.scheduleChangedAt = now
		}
		for c := range selected {
			if _, ok := m.candidateSince[c]; !ok {
				m.candidateSince[c] = now
			}
		}
		for c := range m.candidateSince {
			if !selected[c] {
				delete(m.candidateSince, c) // ClustersRemoved: the work is deleted
			}
		}
		m.works = slices.DeleteFunc(m.works, func(w *work) bool { return !selected[w.cluster] })
		for _, w := range m.works {
			s.observeAgent(w, now)
		}
		s.rollout(m, decisions, now)
		sortWorks(m.works)
		s.writeRolloutConditions(m, decisions, now)
	}
}

// observeAgent follows the spoke agent's connection. It records when the
// agent first reported (once the cluster is Available, from the later of the
// work's apply time and the cluster's Available transition; a cluster that is
// not Available has an agent that reports nothing, acm.md item 9). After
// that, a cluster that loses its lease freezes the work's status at the
// instant Available stopped being True (the hub keeps the last report), and
// when the agent is back the spoke's lifecycle resumes where it stopped.
func (s *Server) observeAgent(w *work, now time.Time) {
	c := s.clusters[w.cluster]
	avail := findCondition(c, CondAvailable)
	available := avail.GetStatus() == cTrue
	since := s.scenarioTime(avail.GetLastTransitionTime().AsTime())
	if w.agentStart == nil {
		if !available {
			return
		}
		start := w.startedAt
		if since.After(start) {
			start = since
		}
		if !start.After(now) {
			w.agentStart = &start
		}
		return
	}
	switch {
	case !available && w.frozenAt == nil:
		frozen := since
		if frozen.Before(*w.agentStart) {
			frozen = *w.agentStart
		}
		if frozen.After(now) {
			frozen = now
		}
		w.frozenAt = &frozen
	case available && w.frozenAt != nil:
		back := since
		if back.Before(*w.frozenAt) {
			back = *w.frozenAt
		}
		if back.After(now) {
			back = now
		}
		shifted := w.agentStart.Add(back.Sub(*w.frozenAt))
		w.agentStart, w.frozenAt = &shifted, nil
	}
}

// spokeInput is the LLMInferenceService lifecycle on one managed cluster,
// from the spec the work applied.
func (s *Server) spokeInput(m *model, w *work) llmisvcInput {
	return s.inputFor(m, w.spec, "apps."+w.cluster+".example.com")
}

// deriveWorkAt is deriveWork for one of a model's works at a scenario time.
func (s *Server) deriveWorkAt(m *model, w *work, params rolloutParams, at time.Time) workView {
	return deriveWork(w, s.spokeInput(m, w), at, params.progressDeadline, s.wallTime)
}

// applyWork creates or updates the work of one cluster at scenario time t:
// the controller's `generateName` work carrying the current template `tmpl`
// (templateOf the model's spec).
func (s *Server) applyWork(m *model, tmpl *aigriddraftv1.ModelSpec, cluster string, t time.Time) {
	var w *work
	for _, existing := range m.works {
		if existing.cluster == cluster {
			w = existing
		}
	}
	if w == nil {
		w = &work{cluster: cluster, name: workNameFor(placementName(m.obj.GetName()), s.newSuffix()), workGeneration: 1}
		m.works = append(m.works, w)
	} else if !proto.Equal(w.spec, tmpl) {
		w.workGeneration++ // the template update changes the work's spec
	}
	w.startedAt, w.agentStart, w.frozenAt = t, nil, nil
	w.spec = cloneSpec(tmpl)
	w.failApply = s.clusters[cluster].GetLabels()[LabelFailApply] == "true"
	s.observeAgent(w, t)
}

// rollout is the ManifestWorkReplicaSet deploy reconcile (acm.md "Rollout
// counting"; ocm:pkg/work/hub/controllers/manifestworkreplicasetcontroller/
// manifestworkreplicaset_deploy_reconcile.go:32-226 and sdk-go
// pkg/apis/cluster/v1alpha1/rollout.go). Only works whose spec equals the
// current template count (deploy_reconcile.go:79-104); a stale one holds no
// slot and its cluster is to apply again, like a new one. The template is
// templateOf the spec, so a change of only the rollout strategy or the
// placement leaves every existing work current: the placement adds and
// removes the works of the clusters entering and leaving the decision, the
// strategy only schedules the works still to apply.
//
// `All` applies every selected cluster at once (rollout.go:115-138).
// `ProgressivePerGroup` rolls out a whole decision group at once and has no
// maxConcurrency (rollout.go:195-244, :370-448); the console's Placements have
// one decision group (clustersPerDecisionGroup defaults to 100%), and a
// maxFailures breach inside the last group stops nothing, so it behaves like
// `All`. `Progressive` fills `maxConcurrency` slots in cluster-name order
// (progressivePerCluster, rollout.go:270-368), a slot staying taken while its
// work is to apply, progressing or failed, and for `minSuccessTime` after it
// succeeded; a failed cluster counts towards `maxFailures` at once and a
// timed-out one (a Progressing or Failed work `progressDeadline` after its
// condition's lastTransitionTime; ToApply never times out) frees its slot and
// counts too; beyond `maxFailures` nothing more is applied. The fake computes
// the exact instant a slot freed, so timing does not depend on how often it is
// polled; across a decision change reconcileModels first catches up under the
// previous decision, and advance() reconciles before each scenario event. The
// one exception is a change only time makes (a tolerationSeconds running out),
// noticed at the next read; see the declared deviation in docs/draft/areas/models.md.
func (s *Server) rollout(m *model, decisions *aigriddraftv1.PlacementStatus, now time.Time) {
	params, err := validateRollout(m.obj.GetSpec().GetRollout())
	if err != nil {
		return // rejected on write; cannot happen
	}
	tmpl := templateOf(m.obj.GetSpec())
	templateAt := m.created
	if len(m.epochs) > 0 {
		templateAt = m.epochs[len(m.epochs)-1].at
	}
	var pending []string
	for _, d := range decisions.GetDecisions() {
		name := d.GetClusterName()
		i := slices.IndexFunc(m.works, func(w *work) bool { return w.cluster == name })
		if i < 0 || !proto.Equal(m.works[i].spec, tmpl) {
			pending = append(pending, name)
		}
	}
	// floorFor is the earliest instant a cluster's work can be applied: the
	// last template change, the last rollout, placement or decision change
	// (the controller only reads the new strategy and decision from then on)
	// and the cluster's entry into the decision, whichever is latest.
	floorFor := func(cluster string) time.Time {
		t := templateAt
		if m.scheduleChangedAt.After(t) {
			t = m.scheduleChangedAt
		}
		if since := m.candidateSince[cluster]; since.After(t) {
			t = since
		}
		return t
	}
	if params.strategy != aigriddraftv1.RolloutStrategy_ROLLOUT_STRATEGY_PROGRESSIVE {
		for _, cluster := range pending {
			if t := floorFor(cluster); !t.After(now) {
				s.applyWork(m, tmpl, cluster, t)
			}
		}
		return
	}
	total := len(decisions.GetDecisions())
	maxConcurrency := rolloutSize(params.maxConcurrency, total, total) // rollout.go:183
	maxFailures := rolloutSize(params.maxFailures, total, 0)           // rollout.go:160
	current := func() []*work {
		return slices.DeleteFunc(slices.Clone(m.works), func(w *work) bool { return !proto.Equal(w.spec, tmpl) })
	}
	// slotsAt counts the taken slots and the failures at instant t.
	slotsAt := func(t time.Time) (taken, failed int) {
		for _, w := range current() {
			if w.startedAt.After(t) {
				continue
			}
			v := s.deriveWorkAt(m, w, params, t)
			switch v.state {
			case rolloutSucceeded:
				if t.Before(v.since.Add(params.minSuccessTime)) {
					taken++
				}
			case rolloutTimeOut:
				failed++
			case rolloutFailed:
				// progressivePerCluster: a Failed cluster keeps its slot
				// until its timeout and counts towards maxFailures at once.
				taken++
				failed++
			default:
				taken++
			}
		}
		return taken, failed
	}
	// frontier is the latest (re)apply among the works: the fake keeps no
	// history before it, so no slot is computed for an earlier instant.
	var frontier time.Time
	for _, w := range m.works {
		if w.startedAt.After(frontier) {
			frontier = w.startedAt
		}
	}
	for _, cluster := range pending {
		floor := floorFor(cluster)
		if frontier.After(floor) {
			floor = frontier
		}
		if floor.After(now) {
			continue
		}
		// Candidate instants: the cluster's floor and every moment a slot frees.
		candidates := []time.Time{floor}
		for _, w := range current() {
			v := s.deriveWorkAt(m, w, params, now)
			switch v.state {
			case rolloutSucceeded:
				candidates = append(candidates, v.since.Add(params.minSuccessTime))
			case rolloutTimeOut:
				candidates = append(candidates, v.since.Add(params.progressDeadline))
			}
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })
		applied := false
		for _, t := range candidates {
			if t.Before(floor) || t.After(now) {
				continue
			}
			taken, failed := slotsAt(t)
			if failed > maxFailures {
				return // MaxFailureBreach: the rollout stops
			}
			if taken < maxConcurrency {
				s.applyWork(m, tmpl, cluster, t)
				applied, frontier = true, t
				break
			}
		}
		if !applied {
			return // every slot is taken; later clusters wait too
		}
	}
}

// writeRolloutConditions is the status the deploy and status reconciles write
// (acm.md 4): PlacementVerified AsExpected once a work at the current template
// exists, else PlacementDecisionEmpty (deploy_reconcile.go:198-209);
// PlacementRolledOut Complete when the succeeded works at the current
// template number the selected clusters, which includes an empty decision
// (0 == 0, :211-215); ManifestworkApplied from the summary
// (manifestworkreplicaset_status_reconcile.go:22-96).
func (s *Server) writeRolloutConditions(m *model, decisions *aigriddraftv1.PlacementStatus, now time.Time) {
	params, _ := validateRollout(m.obj.GetSpec().GetRollout())
	summary, succeeded := s.summarize(m, params, now)
	wall := s.wallTime(now)
	set := func(typ string, st aigriddraftv1.ConditionStatus, reason string) {
		m.rolloutConds = writeCondition(m.rolloutConds, wall, typ, st, reason, "")
	}
	if summary.GetTotal() > 0 {
		set(CondPlacementVerified, cTrue, ReasonAsExpected)
	} else {
		set(CondPlacementVerified, cFalse, ReasonPlacementDecisionEmpty)
	}
	if succeeded == int(decisions.GetNumberOfSelectedClusters()) {
		set(CondPlacementRolledOut, cTrue, ReasonRolloutComplete)
	} else {
		set(CondPlacementRolledOut, cFalse, ReasonRolloutProgressing)
	}
	switch {
	case summary.GetTotal() == 0:
		set(CondManifestworkApplied, cFalse, ReasonPlacementDecisionEmpty) // PlacementVerified is PlacementDecisionEmpty
	case summary.GetAvailable() == summary.GetTotal() && summary.GetProgressing() == 0 && summary.GetDegraded() == 0:
		set(CondManifestworkApplied, cTrue, ReasonAsExpected)
	case summary.GetProgressing() > 0 && summary.GetDegraded() == 0:
		set(CondManifestworkApplied, cFalse, ReasonProcessing)
	default:
		set(CondManifestworkApplied, cFalse, ReasonNotAsExpected)
	}
}

// summarize is `status.summary`: the works at the current template, counted
// by condition (deploy_reconcile.go:174-197 for total,
// manifestworkreplicaset_status_reconcile.go:40-87 for the rest; a work whose
// spec differs from the template is skipped). It also returns how many of
// those works are Succeeded (deploy_reconcile.go:100-103). `desiredTotal` and
// `updated` are declared in the API but never written by the controller, so
// the summary carries neither.
func (s *Server) summarize(m *model, params rolloutParams, now time.Time) (*aigriddraftv1.RolloutSummary, int) {
	sum := &aigriddraftv1.RolloutSummary{}
	succeeded := 0
	tmpl := templateOf(m.obj.GetSpec())
	for _, w := range m.works {
		if !proto.Equal(w.spec, tmpl) {
			continue
		}
		sum.Total++
		v := s.deriveWorkAt(m, w, params, now)
		isTrue := func(typ string) bool { return condition(v.status.GetConditions(), typ).GetStatus() == cTrue }
		if isTrue(CondWorkApplied) {
			sum.Applied++
		}
		if isTrue(CondWorkAvailable) {
			sum.Available++
		}
		if isTrue(CondWorkProgressing) {
			sum.Progressing++
		}
		if isTrue(CondWorkDegraded) {
			sum.Degraded++
		}
		if v.state == rolloutSucceeded {
			succeeded++
		}
	}
	return sum, succeeded
}

// decidedLocations (P8) is the `location` labels of the decided clusters,
// unique and sorted; a cluster without one adds nothing.
func (s *Server) decidedLocations(decisions []*aigriddraftv1.ClusterDecision) []string {
	var out []string
	for _, d := range decisions {
		l := s.clusters[d.GetClusterName()].GetLabels()[LabelLocation]
		if l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

// viewModel is the model as a read returns it: the stored object plus the
// status derived at scenario time `now`.
func (s *Server) viewModel(m *model, now time.Time) *aigriddraftv1.Model {
	out := proto.Clone(m.obj).(*aigriddraftv1.Model)
	st := &aigriddraftv1.ModelStatus{}
	if m.obj.GetSpec().GetPlacement() == nil {
		gens := []llmisvcGeneration{{in: s.inputFor(m, m.baseSpec, hubAppsDomain)}}
		for _, e := range m.epochs {
			gens = append(gens, llmisvcGeneration{in: s.inputFor(m, e.spec, hubAppsDomain), at: e.at.Sub(m.created)})
		}
		st, _ = llmisvcStatus(gens, now.Sub(m.created), func(d time.Duration) time.Time { return s.wallTime(m.created.Add(d)) })
		st.Phase, st.Reason, st.Message = deriveHubPhase(st)
		out.Status = st
		return out
	}
	decisions := m.decisions // what the scheduling controller last wrote (reconcileModels runs first)
	if decisions == nil {
		decisions = computePlacement(m.obj.GetSpec().GetPlacement(), s.bindingsFor(m.obj.GetMetadata().GetNamespace()), s.clusters, nil, now, s.scenarioTime)
	}
	st.Placement = &aigriddraftv1.PlacementStatus{NumberOfSelectedClusters: decisions.GetNumberOfSelectedClusters(), Decisions: decisions.GetDecisions(), Locations: s.decidedLocations(decisions.GetDecisions())}
	for _, c := range m.placementConds {
		st.Placement.Conditions = append(st.Placement.Conditions, proto.Clone(c).(*aigriddraftv1.Condition))
	}
	st.Rollout = &aigriddraftv1.RolloutStatus{Summary: &aigriddraftv1.RolloutSummary{}}
	if !s.capabilities.GetManifestWorkReplicaSetEnabled() {
		// The CRD stores the object, nothing reconciles it: empty status.
		st.Phase, st.Message = aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, MessageFeatureGateDisabled
		out.Status = st
		return out
	}
	params, _ := validateRollout(m.obj.GetSpec().GetRollout())
	st.Rollout.Summary, _ = s.summarize(m, params, now)
	for _, c := range m.rolloutConds {
		st.Rollout.Conditions = append(st.Rollout.Conditions, proto.Clone(c).(*aigriddraftv1.Condition))
	}
	for _, w := range m.works {
		st.Rollout.Works = append(st.Rollout.Works, s.deriveWorkAt(m, w, params, now).status)
	}
	st.Phase, st.Reason, st.Message = derivePlacedPhase(st.Rollout)
	out.Status = st
	return out
}

// derivePlacedPhase is the phase derivation for a placed model (see
// ModelPhase in model_type.proto), a console rule over the MWRS conditions:
// rolled out and applied -> READY; applied NotAsExpected with a degraded or
// unapplied work -> FAILED; otherwise PENDING with the reason of
// PlacementVerified when it is False, of PlacementRolledOut when
// ManifestworkApplied is True (every work so far applied, more clusters to
// come), else of ManifestworkApplied. A stuck spoke never reads FAILED: its
// work stays Progressing (MinimumReplicasUnavailable is not a terminal
// reason), so the model reads PENDING/Processing, and a Progressive rollout
// times it out after progressDeadline, which no MWRS condition shows.
func derivePlacedPhase(rollout *aigriddraftv1.RolloutStatus) (aigriddraftv1.ModelPhase, string, string) {
	conds := rollout.GetConditions()
	verified := condition(conds, CondPlacementVerified)
	rolledOut := condition(conds, CondPlacementRolledOut)
	applied := condition(conds, CondManifestworkApplied)
	broken := rollout.GetSummary().GetDegraded() > 0
	for _, w := range rollout.GetWorks() {
		if condition(w.GetConditions(), CondWorkApplied).GetStatus() == cFalse {
			broken = true
		}
	}
	switch {
	case rolledOut.GetStatus() == cTrue && applied.GetStatus() == cTrue:
		return aigriddraftv1.ModelPhase_MODEL_PHASE_READY, ReasonRolloutComplete, ""
	case applied.GetStatus() == cFalse && applied.GetReason() == ReasonNotAsExpected && broken:
		return aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ReasonNotAsExpected, ""
	case verified != nil && verified.GetStatus() == cFalse:
		return aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, verified.GetReason(), ""
	case applied.GetStatus() == cTrue:
		return aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, rolledOut.GetReason(), "" // applied everywhere so far, more clusters to come
	default:
		return aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, applied.GetReason(), ""
	}
}

// ListModels implements Models: namespace then name order, offset/limit
// paging (omitted limit means all, zero counts only, negative is
// InvalidArgument). `filter` is CEL over `this` (a Model, its status as this
// read derives it) and `now` (celfilter), evaluated in memory over every
// model before paging (backend-conventions.md 5.3, "Filter"), so `total`
// counts the filtered set; a label term needs the `in` guard, because CEL
// indexes a missing key with a `no such key` error, which is InvalidArgument
// here as it is in cel-go.
func (s *Server) ListModels(ctx context.Context, req *aigriddraftv1.ListModelsRequest) (*aigriddraftv1.ListModelsResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	filter, err := celfilter.Compile(req.GetFilter(), &aigriddraftv1.Model{})
	if err != nil {
		return nil, err
	}
	if req.GetOrder() != "" {
		return nil, status.Error(codes.InvalidArgument, "order is not supported by this server yet; models are returned by namespace and name")
	}
	if req.GetOffset() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "offset must not be negative, got %d", req.GetOffset())
	}
	if req.Limit != nil && req.GetLimit() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "limit must not be negative, got %d", req.GetLimit())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.advance()
	views := make([]*aigriddraftv1.Model, 0, len(s.models))
	for _, m := range s.models {
		views = append(views, s.viewModel(m, now))
	}
	sort.Slice(views, func(i, j int) bool {
		if a, b := views[i].GetMetadata().GetNamespace(), views[j].GetMetadata().GetNamespace(); a != b {
			return a < b
		}
		return views[i].GetName() < views[j].GetName()
	})
	// Filtered in that order, so an evaluation error names the same model
	// every time.
	all := make([]*aigriddraftv1.Model, 0, len(views))
	for _, v := range views {
		ok, err := filter.Match(ctx, v, modelKey(v.GetMetadata().GetNamespace(), v.GetName()), s.wallTime(now))
		if err != nil {
			return nil, err
		}
		if ok {
			all = append(all, v)
		}
	}
	total := len(all)
	offset := min(int(req.GetOffset()), total)
	page := all[offset:]
	if req.Limit != nil && int(req.GetLimit()) < len(page) {
		page = page[:req.GetLimit()]
	}
	return &aigriddraftv1.ListModelsResponse{Items: page, Size: int32(len(page)), Total: proto.Int32(int32(total))}, nil
}

// GetModel implements Models.
func (s *Server) GetModel(ctx context.Context, req *aigriddraftv1.GetModelRequest) (*aigriddraftv1.GetModelResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	if req.GetName() == "" || req.GetNamespace() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and namespace are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.advance()
	m, ok := s.models[modelKey(req.GetNamespace(), req.GetName())]
	if !ok {
		return nil, notFound(modelKey(req.GetNamespace(), req.GetName()))
	}
	return &aigriddraftv1.GetModelResponse{Object: s.viewModel(m, now)}, nil
}

// CreateModel implements Models: admission in the API server's order (names,
// schema and CEL rules, the webhook), then the backend's feature-gate check,
// then the AlreadyExists check, then the object is stored with no
// LLMInferenceService status; the controllers fill it over the following
// minutes (kserve.go, work.go). A placed model comes back with its Placement
// decision and MWRS conditions already reconciled (and, for All, its works).
func (s *Server) CreateModel(ctx context.Context, req *aigriddraftv1.CreateModelRequest) (*aigriddraftv1.CreateModelResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "models"); err != nil {
		return nil, err
	}
	obj := req.GetObject()
	if obj == nil {
		return nil, status.Error(codes.InvalidArgument, "object is required")
	}
	if err := validateModelName("metadata.name", obj.GetName()); err != nil {
		return nil, err
	}
	if err := validateNamespaceName("metadata.namespace", obj.GetMetadata().GetNamespace()); err != nil {
		return nil, err
	}
	if err := validateModelSpec(obj.GetSpec()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.advance()
	if err := s.checkGate(obj.GetSpec()); err != nil {
		return nil, err
	}
	key := modelKey(obj.GetMetadata().GetNamespace(), obj.GetName())
	if _, dup := s.models[key]; dup {
		return nil, status.Errorf(codes.AlreadyExists, MsgAlreadyExistsFm, obj.GetName())
	}
	m := normalizeModel(obj)
	m.Metadata.Uid = s.newID()
	m.Metadata.Generation = 1
	m.Metadata.CreationTimestamp = timestamppb.New(s.wallTime(now))
	s.bumpModelVersion(m)
	stored := &model{obj: m, created: now, baseSpec: cloneSpec(m.GetSpec()), candidateSince: map[string]time.Time{}}
	s.models[key] = stored
	s.reconcileModels(now)
	s.logger.Debug("model created", "model", key, "placed", m.GetSpec().GetPlacement() != nil)
	return &aigriddraftv1.CreateModelResponse{Object: s.viewModel(stored, now)}, nil
}

// UpdateModel implements Models with the conventions' semantics
// (backend-conventions.md 5.3): the mask names whole fields, `lock` compares
// `metadata.resource_version` and fails with Aborted, every write bumps the
// version and a spec change bumps the generation. A change of the template
// (templateOf: everything but `placement` and `rollout`) also changes the
// LLMInferenceService, which makes its Deployment roll out again, and the
// ManifestWorkReplicaSet's template, which re-applies the works; a change of
// only `placement` or `rollout` leaves both as they are. An update after which
// the spec, labels and annotations equal the stored ones is not written: the
// stored model comes back with its `resource_version` and `generation`
// unchanged, as the Kubernetes API server answers an unchanged update
// (k8s.io/apiserver v0.35.8 pkg/storage/etcd3/store.go:553-577); the `lock`
// precondition and the admission checks run before, as there.
func (s *Server) UpdateModel(ctx context.Context, req *aigriddraftv1.UpdateModelRequest) (*aigriddraftv1.UpdateModelResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "models"); err != nil {
		return nil, err
	}
	obj := req.GetObject()
	if obj.GetName() == "" || obj.GetMetadata().GetNamespace() == "" {
		return nil, status.Error(codes.InvalidArgument, "object.name and object.metadata.namespace are required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "update_mask must name at least one of %s", strings.Join(modelPaths, ", "))
	}
	for _, p := range paths {
		if !slices.Contains(modelPaths, p) {
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not supported; supported paths: %s", p, strings.Join(modelPaths, ", "))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.advance()
	key := modelKey(obj.GetMetadata().GetNamespace(), obj.GetName())
	m, ok := s.models[key]
	if !ok {
		return nil, notFound(key)
	}
	cur := m.obj
	if req.GetLock() && obj.GetMetadata().GetResourceVersion() != cur.GetMetadata().GetResourceVersion() {
		return nil, status.Errorf(codes.Aborted, "object with identifier '%s' has resource version %s, the request carries %q; fetch it again and retry", key, cur.GetMetadata().GetResourceVersion(), obj.GetMetadata().GetResourceVersion())
	}
	next := proto.Clone(cur).(*aigriddraftv1.Model)
	if next.Spec == nil {
		next.Spec = &aigriddraftv1.ModelSpec{}
	}
	for _, p := range paths {
		switch p {
		case PathSpecReplicas:
			next.Spec.Replicas = nil
			if obj.GetSpec().Replicas != nil {
				next.Spec.Replicas = proto.Int32(obj.GetSpec().GetReplicas())
			}
		case PathSpecMainImage:
			if next.Spec.Main == nil {
				next.Spec.Main = &aigriddraftv1.Container{}
			}
			next.Spec.Main.Image = obj.GetSpec().GetMain().GetImage()
		case PathSpecMainEnv:
			if next.Spec.Main == nil {
				next.Spec.Main = &aigriddraftv1.Container{}
			}
			next.Spec.Main.Env = nil
			for _, e := range obj.GetSpec().GetMain().GetEnv() {
				next.Spec.Main.Env = append(next.Spec.Main.Env, proto.Clone(e).(*aigriddraftv1.EnvVar))
			}
		case PathSpecPlacement:
			next.Spec.Placement = nil
			if p := obj.GetSpec().GetPlacement(); p != nil {
				next.Spec.Placement = proto.Clone(p).(*aigriddraftv1.Placement)
			}
		case PathSpecRollout:
			next.Spec.Rollout = nil
			if r := obj.GetSpec().GetRollout(); r != nil {
				next.Spec.Rollout = proto.Clone(r).(*aigriddraftv1.Rollout)
			}
		case PathMetadataLabels:
			next.Metadata.Labels = copyMap(obj.GetMetadata().GetLabels())
		case PathMetadataAnnotation:
			next.Metadata.Annotations = copyMap(obj.GetMetadata().GetAnnotations())
		}
	}
	if next.Spec.Placement == nil && next.Spec.Rollout != nil && cur.GetSpec().GetPlacement() != nil && !slices.Contains(paths, PathSpecRollout) {
		next.Spec.Rollout = nil // the rollout goes with the placement it belonged to
	}
	if err := validateModelSpec(next.GetSpec()); err != nil {
		return nil, err
	}
	if cur.GetSpec().GetPlacement() == nil {
		if err := s.checkGate(next.GetSpec()); err != nil {
			return nil, err
		}
	}
	if proto.Equal(cur.GetSpec(), next.GetSpec()) && maps.Equal(cur.GetMetadata().GetLabels(), next.GetMetadata().GetLabels()) && maps.Equal(cur.GetMetadata().GetAnnotations(), next.GetMetadata().GetAnnotations()) {
		s.logger.Debug("model update changes nothing", "model", key, "paths", paths, "resourceVersion", cur.GetMetadata().GetResourceVersion())
		return &aigriddraftv1.UpdateModelResponse{Object: s.viewModel(m, now)}, nil
	}
	if !proto.Equal(cur.GetSpec(), next.GetSpec()) {
		next.Metadata.Generation++
		if !proto.Equal(templateOf(cur.GetSpec()), templateOf(next.GetSpec())) {
			m.epochs = append(m.epochs, specEpoch{at: now, spec: cloneSpec(next.GetSpec())})
		} else {
			m.scheduleChangedAt = now // rollout or placement only
		}
		if (cur.GetSpec().GetPlacement() == nil) != (next.GetSpec().GetPlacement() == nil) {
			// The hub deployment is replaced by the fan-out, or the other way
			// round: whatever now serves the model is created at this moment.
			m.created, m.epochs, m.baseSpec = now, nil, cloneSpec(next.GetSpec())
			m.works, m.candidateSince, m.rolloutConds, m.placementConds = nil, map[string]time.Time{}, nil, nil
		}
	}
	s.bumpModelVersion(next)
	m.obj = next
	s.reconcileModels(now)
	s.logger.Debug("model updated", "model", key, "paths", paths, "generation", next.GetMetadata().GetGeneration())
	return &aigriddraftv1.UpdateModelResponse{Object: s.viewModel(m, now)}, nil
}

// DeleteModel implements Models. The model disappears at once. A hub
// deployment really carries KServe's `serving.kserve.io/llmisvc-finalizer`
// (kserve:controller.go:204-211) and stays, with a deletionTimestamp, until
// its finalize pass completes (:215-231, :330-354), normally one reconcile;
// the fake does not model that window (a declared deviation in
// docs/draft/areas/models.md) and shows no finalizers. The MWRS uses the CRD
// default cascade `Background`, under which the finalize reconciler
// deletes the works and removes the finalizer in the same pass
// (ocm:manifestworkreplicaset_finalize_reconcile.go); the works drain in the
// cluster namespaces afterwards, invisibly to this API (only `Foreground`
// would keep the MWRS until they are gone). Aborted when `resource_version`
// is set and stale. FailedPrecondition while
// something references the model: the check is `referencedBy`, which the
// catalog and subscriptions area will wire; today nothing references a model.
func (s *Server) DeleteModel(ctx context.Context, req *aigriddraftv1.DeleteModelRequest) (*aigriddraftv1.DeleteModelResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "models"); err != nil {
		return nil, err
	}
	if req.GetName() == "" || req.GetNamespace() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and namespace are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	key := modelKey(req.GetNamespace(), req.GetName())
	m, ok := s.models[key]
	if !ok {
		return nil, notFound(key)
	}
	// A Kubernetes delete precondition (DeleteOptions.preconditions): the API
	// server answers 409 Conflict, which the backend maps to Aborted.
	if rv := req.GetResourceVersion(); rv != "" && rv != m.obj.GetMetadata().GetResourceVersion() {
		return nil, status.Errorf(codes.Aborted, "object with identifier '%s' has resource version %s, the request carries %q; fetch it again and retry", key, m.obj.GetMetadata().GetResourceVersion(), rv)
	}
	if refs := s.referencedBy(req.GetNamespace(), req.GetName()); len(refs) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, MessageReferencedByCatalogFm, key, strings.Join(refs, ", "))
	}
	delete(s.models, key)
	s.logger.Debug("model deleted", "model", key)
	return &aigriddraftv1.DeleteModelResponse{}, nil
}

// GetHubCapabilities implements AiGrid from the settings fixture.
func (s *Server) GetHubCapabilities(ctx context.Context, _ *aigriddraftv1.GetHubCapabilitiesRequest) (*aigriddraftv1.GetHubCapabilitiesResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &aigriddraftv1.GetHubCapabilitiesResponse{Capabilities: proto.Clone(s.capabilities).(*aigriddraftv1.HubCapabilities)}, nil
}

// SetReferencedBy installs the check DeleteModel runs: the names of the
// catalog entries and subscriptions that reference a model. The catalog area
// wires it; until then nothing references a model.
func (s *Server) SetReferencedBy(fn func(namespace, name string) []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.referencedBy = fn
}

// SetWorkSuffixGenerator replaces the source of generated work name suffixes
// (newWorkSuffix, a random generateName suffix, by default), so tests and the
// golden responses can predict work names. The works that already exist, such
// as the ones New generated for the fixture models, are renamed with the new
// generator in model and cluster order, so every work name is predictable
// from the moment it is set (call SetClock first: it regenerates the fixture
// models' works).
func (s *Server) SetWorkSuffixGenerator(next func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newSuffix = next
	keys := make([]string, 0, len(s.models))
	for key := range s.models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m := s.models[key]
		sortWorks(m.works)
		for _, w := range m.works {
			w.name = workNameFor(placementName(m.obj.GetName()), next())
		}
	}
}

// SetManifestWorkReplicaSetEnabled flips the hub capability at runtime, for
// tests of both paths.
func (s *Server) SetManifestWorkReplicaSetEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capabilities.ManifestWorkReplicaSetEnabled = enabled
}
