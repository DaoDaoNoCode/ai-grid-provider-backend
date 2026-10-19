package draft

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Strings KServe writes on an LLMInferenceService (serving.kserve.io/v1alpha2).
// Every value comes from docs/draft/research/models-maas.md section 2.1 and was
// re-checked against the controller source (opendatahub-io/kserve at 01d6280;
// `kserve:` paths below are pkg/controller/v1alpha2/llmisvc/ unless they name
// pkg/apis) and knative.dev/pkg at 0795cb8 (`knative:`); nothing here is
// invented unless its comment says so. Keep them verbatim: the UI and the real
// backend will match on them.
const (
	// Condition types. `Ready` rolls up PresetsCombined, WorkloadsReady and
	// RouterReady (Knative LivingConditionSet,
	// kserve:pkg/apis/serving/v1alpha2/llm_inference_service_lifecycle.go:164-168);
	// WorkloadsReady rolls up MainWorkloadReady (:258-283); RouterReady rolls up
	// GatewaysReady, HTTPRoutesReady, InferencePoolReady and
	// SchedulerWorkloadReady, the first False one in that order (:405-425).
	CondReady                  = "Ready"
	CondPresetsCombined        = "PresetsCombined"
	CondWorkloadsReady         = "WorkloadsReady"
	CondMainWorkloadReady      = "MainWorkloadReady"
	CondRouterReady            = "RouterReady"
	CondGatewaysReady          = "GatewaysReady"
	CondHTTPRoutesReady        = "HTTPRoutesReady"
	CondInferencePoolReady     = "InferencePoolReady"
	CondSchedulerWorkloadReady = "SchedulerWorkloadReady"

	// PresetsCombined False (kserve:config_merge.go:378). The message format
	// is kserve:config_merge.go:1283, `%v` of the namespaces searched.
	ReasonConfigNotFound    = "ConfigNotFound"
	MessageConfigNotFoundFm = "LLMInferenceServiceConfig %q not found in namespaces %v"

	// MainWorkloadReady False: KServe copies the Deployment's own conditions
	// (kserve:workload_single_node.go:418-443, propagateWorkloadDeploymentStatus).
	// A Progressing=False condition is marked (ProgressDeadlineExceeded) but the
	// loop goes on and returns at the Available condition, so Available=False
	// ends at MinimumReplicasUnavailable with the Deployment's message whatever
	// Progressing says. Without an Available condition the loop falls through
	// to :442, which overwrites it with Progressing and an empty message
	// (knative:apis/condition_set.go:187-209 replaces a condition of the same
	// type). The reconciler works on a copy and writes the status once, after
	// the whole pass (kserve:controller.go:246-262; reconcile_common.go:93-111
	// only grooms lastTransitionTime), so ProgressDeadlineExceeded never
	// persists on MainWorkloadReady in either case. The deployment controller writes Available on every
	// status sync (k8s.io/kubernetes@v1.35.8 pkg/controller/deployment/
	// sync.go:525-531), so a workload that never becomes available keeps
	// MinimumReplicasUnavailable for good. No Deployment condition yet gives
	// Progressing with an empty message (:442).
	ReasonMinimumReplicasUnavailable  = "MinimumReplicasUnavailable"
	MessageMinimumReplicasUnavailable = "Deployment does not have minimum availability."
	ReasonProgressing                 = "Progressing"
	// SchedulerWorkloadReady before the scheduler Deployment reports
	// (kserve:scheduler.go:875).
	MessageDeploymentRolloutInProgress = "Deployment rollout in progress"

	// GatewaysReady False. A referenced Gateway that does not exist fails
	// validateGatewayReferences (kserve:router_validation.go:118-156):
	// RefsInvalid with one "Gateway <ns>/<name> does not exist" per missing
	// Gateway joined with "; " (:142, :150-151), marked at :68. The router then
	// returns before the scheduler and the HTTPRoute are reconciled.
	// GatewaysNotReady is only for a Gateway that exists but is not programmed
	// (kserve:router.go:622-656, :649); every Gateway the fake knows is programmed,
	// so it never produces it.
	ReasonRefsInvalid         = "RefsInvalid"
	MessageGatewayMissingFm   = "Gateway %s/%s does not exist"
	ReasonGatewaysNotReady    = "GatewaysNotReady"
	MessageGatewaysNotReadyFm = "The following Gateways are not ready: %v"

	// HTTPRoutesReady False (kserve:router.go:713-779). Once the managed
	// `<name>-kserve-route` is written and until a Gateway controller accepts
	// it, the controller reports WaitingForGateway listing the routes
	// (:771-772). HTTPRouteNotObserved (:732) is only reported when the route
	// list is empty for an inline spec, which cannot happen after
	// reconcileHTTPRoutes wrote the managed route, so the fake never produces
	// it.
	ReasonHTTPRouteNotObserved  = "HTTPRouteNotObserved"
	MessageHTTPRouteNotObserved = "The managed HTTPRoute has not been observed yet"
	ReasonWaitingForGateway     = "WaitingForGateway"
	MessageWaitingForGatewayFm  = "The following HTTPRoutes exist but no Gateway controller has accepted them yet: %v"
	managedRouteSuffix          = "-kserve-route"

	// InferencePoolReady False while no Gateway has accepted the managed pool
	// `<name>-inference-pool` (kserve:router.go:938-939, pool name
	// kserve:scheduler.go:346).
	MessagePoolWaitingForGatewayFm = "Inference Pool %s/%s exists but no Gateway controller has accepted it yet"
	inferencePoolSuffix            = "-inference-pool"

	// `status.addresses[].name` values (kserve:router_discovery_filter.go:91-105).
	AddressGatewayExternal             = "gateway-external"
	AddressGatewayExternalModelRouting = "gateway-external-model-routing"
	AddressGatewayInternal             = "gateway-internal"
	AddressGatewayInternalModelRouting = "gateway-internal-model-routing"

	// `status.appliedConfigs[].source` values.
	AppliedConfigPreset  = "Preset"
	AppliedConfigUserRef = "UserRef"

	// Gateway API group and kind of an address origin.
	GatewayGroup = "gateway.networking.k8s.io"
	GatewayKind  = "Gateway"

	// The product-default Gateway names, as recorded in
	// docs/draft/research/models-maas.md section 1: `openshift-ai-inference`, the
	// Gateway RHOAI installs for KServe, and `maas-default-gateway`, the MaaS
	// one, both in `openshift-ingress`. They are what the console writes by
	// default, not a claim about any particular hub (a hub's Gateway set
	// changes with its installation).
	GatewayNamespace   = "openshift-ingress"
	GatewayAIInference = "openshift-ai-inference"
	GatewayMaaSDefault = "maas-default-gateway"
	defaultGatewayName = GatewayAIInference
	defaultGatewayNS   = GatewayNamespace
	// gatewayClassName is an invented GatewayClass for the fake's Gateways.
	// The internal address is the Gateway's backing Service
	// (kserve:router_discovery.go:92-134), which the gateway controller names
	// `<gateway>-<gatewayClassName>`.
	gatewayClassName = "openshift-default"

	workloadsAPIGroup  = "apps"
	workloadsKind      = "Deployment"
	workloadMainSuffix = "-kserve" // kserve:workload_single_node.go:606
	// The scheduler Deployment is `<name>-kserve-router-scheduler`
	// (kserve:scheduler.go:2364). The EPP Service is `<name>-epp-service`
	// (kserve:pkg/apis/serving/v1alpha2/llm_inference_service_types_func.go:44);
	// `<name>-kserve-scheduler` is an Istio DestinationRule
	// (kserve:router_platform_networking_odh.go:193). The workload Service is
	// `<name>-kserve-workload-svc` (kserve:workload.go:327).
	workloadSchedSuffix = "-kserve-router-scheduler"
	workloadSvcSuffix   = "-kserve-workload-svc"
	serviceKind         = "Service"

	// Presets the controller merges under every spec, in merge order
	// (kserve:config_merge.go:579-674): scheduler, router-route, template as
	// Preset, then the baseRefs as UserRef. Upstream also merges
	// `<prefix>config-llm-scheduler-eppconfig-default` right after the
	// scheduler preset when the scheduler preset's router image is at least
	// 0.11.0 (:584-616); whether the installed 3.6 router image qualifies was
	// not observed, so the fake leaves it out (models.md "Deviations"). Names
	// are versioned by RHOAI release; presets.json carries the installed list
	// (models-maas.md open question 10).
	PresetTemplate    = "v3-6-0-kserve-config-llm-template"
	PresetScheduler   = "v3-6-0-kserve-config-llm-scheduler"
	PresetRouterRoute = "v3-6-0-kserve-config-llm-router-route"
	// PresetsNamespace is where RHOAI installs the LLMInferenceServiceConfigs.
	PresetsNamespace = "redhat-ods-applications"

	// Annotations the display name and description live in (the OpenShift
	// convention the POC and the ODH dashboard use, models-maas.md 7.3).
	AnnotationDisplayName = "openshift.io/display-name"
	AnnotationDescription = "openshift.io/description"

	// URI schemes the console accepts in `spec.model.uri`. The CRD types the
	// field as a plain string (hub CRD, v1alpha2 spec.model.uri) and the
	// webhook does not check the scheme, so this is a console check.
	schemeHF  = "hf://"
	schemeS3  = "s3://"
	schemePVC = "pvc://"
	schemeOCI = "oci://"

	// hubAppsDomain is the invented wildcard domain of the hub's Gateway load
	// balancer; the spoke domains are `apps.<cluster>.example.com`, matching the
	// console URLs in managedclusters.json.
	hubAppsDomain = "apps.hub.example.com"
)

// Stage offsets of the LLMInferenceService lifecycle, in scenario seconds
// (divided by the settings' `speed`), following models-maas.md 8.2: the
// controller's first reconcile (presets merged, the Deployment, the scheduler,
// the InferencePool and the managed HTTPRoute created in one pass, workload
// before router: kserve:controller.go:308-325), the route and the pool
// accepted and the scheduler available, the Deployment reporting
// Available=False, the pods ready. A workload that never becomes ready stays at
// the third stage for good.
const (
	llmisvcT1  = 5 * time.Second
	llmisvcT2  = 15 * time.Second
	llmisvcT2b = 20 * time.Second
	llmisvcT3  = 45 * time.Second
)

// Condition status values, short names for the derivations below.
const (
	cTrue    = aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE
	cFalse   = aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE
	cUnknown = aigriddraftv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
)

// uriSchemes lists the accepted `spec.model.uri` prefixes.
var uriSchemes = []string{schemeHF, schemeS3, schemePVC, schemeOCI}

// knownGateways are the Gateways that exist (and are programmed) wherever the
// fake deploys a model; a reference to any other one does not exist, which
// yields GatewaysReady=False/RefsInvalid.
var knownGateways = map[string]bool{
	GatewayNamespace + "/" + GatewayAIInference: true,
	GatewayNamespace + "/" + GatewayMaaSDefault: true,
}

// stuckMarkers select the "pods never become ready" failure mode when found in
// the image or the model name (models-maas.md 8.2).
var stuckMarkers = []string{"oom", "fail"}

// llmisvcInput is everything the lifecycle depends on, extracted once from a
// Model so the replay below is a pure function of (input, time).
type llmisvcInput struct {
	name, namespace string
	modelName       string
	baseRefs        []string
	// gateways are the route's parents: the referenced Gateways, or the
	// default one when none is referenced.
	gateways []*aigriddraftv1.GatewayRef
	// gatewayRefs is whether `spec.router.gateway.refs` is set; without refs
	// GatewaysReady is left unset (kserve:router.go:630-635).
	gatewayRefs  bool
	scheduler    bool
	managedRoute bool
	replicas     int32
	// appsDomain is the wildcard domain the external addresses use.
	appsDomain string
	// unknownPreset is the first baseRef not installed, or "" when all are.
	unknownPreset string
	// presetsNamespace is where the presets live, for the ConfigNotFound message.
	presetsNamespace string
	// stuck: the workload never reaches minimum availability.
	stuck bool
	// missingGateways are the referenced Gateways that do not exist.
	missingGateways []*aigriddraftv1.GatewayRef
}

// newLLMISvcInput reads the lifecycle inputs from a model for a given cluster
// domain. presets is the installed LLMInferenceServiceConfig list.
func newLLMISvcInput(m *aigriddraftv1.Model, presets *Presets, appsDomain string) llmisvcInput {
	spec := m.GetSpec()
	in := llmisvcInput{
		name:             m.GetName(),
		namespace:        m.GetMetadata().GetNamespace(),
		modelName:        spec.GetModel().GetName(),
		baseRefs:         spec.GetBaseRefs(),
		gateways:         spec.GetRouter().GetGatewayRefs(),
		scheduler:        spec.GetRouter().GetScheduler(),
		managedRoute:     spec.GetRouter().GetManagedRoute(),
		replicas:         spec.GetReplicas(),
		appsDomain:       appsDomain,
		presetsNamespace: presets.Namespace,
	}
	if in.modelName == "" {
		in.modelName = in.name // llm_inference_service_defaults.go
	}
	if spec.Replicas == nil {
		in.replicas = 1
		if sc := spec.GetScaling(); sc != nil {
			in.replicas = max(sc.GetMinReplicas(), 1)
		}
	}
	in.gatewayRefs = len(in.gateways) > 0
	if !in.gatewayRefs {
		in.gateways = []*aigriddraftv1.GatewayRef{{Name: defaultGatewayName, Namespace: defaultGatewayNS}}
	}
	for _, ref := range in.baseRefs {
		if !slices.Contains(presets.Names, ref) {
			in.unknownPreset = ref
			break
		}
	}
	if in.gatewayRefs {
		for _, gw := range in.gateways {
			if !knownGateways[gatewayNS(gw, in.namespace)+"/"+gw.GetName()] {
				in.missingGateways = append(in.missingGateways, gw)
			}
		}
	}
	image := strings.ToLower(spec.GetMain().GetImage())
	modelName := strings.ToLower(in.modelName)
	for _, marker := range stuckMarkers {
		if strings.Contains(image, marker) || strings.Contains(modelName, marker) {
			in.stuck = true
		}
	}
	return in
}

// gatewayNS is a Gateway reference's namespace, the service's own when empty
// (kserve:router_validation.go:135-137).
func gatewayNS(gw *aigriddraftv1.GatewayRef, own string) string {
	if gw.GetNamespace() == "" {
		return own
	}
	return gw.GetNamespace()
}

// Presets is the installed LLMInferenceServiceConfig list (presets.json).
type Presets struct {
	// Namespace the presets live in.
	Namespace string `json:"namespace"`
	// Names of the installed presets; `baseRefs` are validated against them.
	Names []string `json:"presets"`
}

// defaultPresets is what the fake uses when presets.json is absent: the
// presets the hub instance's appliedConfigs show.
func defaultPresets() *Presets {
	return &Presets{Namespace: PresetsNamespace, Names: []string{PresetTemplate, PresetScheduler, PresetRouterRoute, "v3-6-0-kserve-config-llm-template-cpu"}}
}

// llmisvcEvent is one controller reconcile in the lifecycle replay.
type llmisvcEvent struct {
	at    time.Duration
	apply func(st *aigriddraftv1.ModelStatus, now time.Time)
}

// llmisvcGeneration is one generation of the spec: the lifecycle inputs it
// produced and the elapsed offset (from creation) at which it was written.
// Generation 1 is at offset zero.
type llmisvcGeneration struct {
	in llmisvcInput
	at time.Duration
}

// llmisvcMarks are the offsets (from creation) at which the parts of the
// status the work agent reads last changed, for the condition rules and the
// feedback of a ManifestWork carrying this object (work.go). Zero when the
// part never changed.
type llmisvcMarks struct {
	// readyStatus: Ready's status last changed (the Progressing rule).
	readyStatus time.Duration
	// terminal: Ready last entered or left a terminal failure (the Degraded rule).
	terminal time.Duration
	// feedback: the `ready` (Ready's status) or `url` feedback value last changed.
	feedback time.Duration
}

// llmisvcStatus derives the LLMInferenceService status at `elapsed` after
// creation by replaying the controller's reconciles in order, each stamped
// with the time it happened, with Knative's lastTransitionTime rule (see
// setModelCondition). `gens` are the generations of the spec in order (the
// first at offset zero); each one's events are derived from its own inputs,
// and the events of a generation that a newer one supersedes before they
// happen are dropped: a controller that sees a new generation reconciles the
// new spec, so a Deployment rollout of the old one can never mark the new one
// Ready. `at(offset)` converts an offset into wall time.
func llmisvcStatus(gens []llmisvcGeneration, elapsed time.Duration, at func(time.Duration) time.Time) (*aigriddraftv1.ModelStatus, llmisvcMarks) {
	st := &aigriddraftv1.ModelStatus{}
	var events []llmisvcEvent
	for i, g := range gens {
		var own []llmisvcEvent
		switch {
		case i == 0 || gens[i-1].in.unknownPreset != "":
			// First generation, or the previous one stopped at the presets
			// and never created the workload: the whole lifecycle runs.
			own = llmisvcEvents(g.in, g.at, int64(i)+1)
		default:
			own = llmisvcRestartEvents(g.in, g.at, int64(i)+1)
		}
		if i+1 < len(gens) {
			cutoff := gens[i+1].at
			own = slices.DeleteFunc(own, func(ev llmisvcEvent) bool { return ev.at >= cutoff })
		}
		events = append(events, own...)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })
	var marks llmisvcMarks
	for _, ev := range events {
		if ev.at > elapsed {
			break
		}
		before := proto.Clone(st).(*aigriddraftv1.ModelStatus)
		ev.apply(st, at(ev.at))
		was, now := condition(before.GetConditions(), CondReady), condition(st.GetConditions(), CondReady)
		if now != nil && (was == nil || was.GetStatus() != now.GetStatus()) {
			marks.readyStatus = ev.at
		}
		if now != nil && (was == nil || isTerminalFailure(was) != isTerminalFailure(now)) {
			marks.terminal = ev.at
		}
		if readyFeedback(was) != readyFeedback(now) || before.GetUrl() != st.GetUrl() {
			marks.feedback = ev.at
		}
	}
	sort.SliceStable(st.Conditions, func(i, j int) bool { return st.Conditions[i].GetType() < st.Conditions[j].GetType() }) // knative:apis/condition_set.go:207
	return st, marks
}

// readyFeedback is the `ready` feedback value of a Ready condition ("" when
// absent).
func readyFeedback(ready *aigriddraftv1.Condition) string {
	if ready == nil {
		return ""
	}
	return conditionStatusString(ready.GetStatus())
}

// terminalReadyReasons are the `Ready=False` reasons that mean the model will
// not recover on its own: a missing preset, a referenced Gateway that does not
// exist, or one that is not programmed. The other False reasons
// (`WaitingForGateway`, `Progressing`, `MinimumReplicasUnavailable`) are what
// KServe reports while a healthy model starts, and MinimumReplicasUnavailable
// is also what a workload that never becomes available keeps reporting
// (kserve:workload_single_node.go:418-443), so a rule cannot tell those two
// apart. The console's ManifestWork template encodes this set in its Degraded
// condition rule:
//
//	object.status.conditions.exists(c, c.type == "Ready" && c.status == "False"
//	  && c.reason in ["ConfigNotFound", "RefsInvalid", "GatewaysNotReady"])
//
// so a spoke that is starting, or stuck, is Progressing, not Degraded: a stuck
// spoke stays Progressing until the rollout's progressDeadline times it out.
var terminalReadyReasons = map[string]bool{ReasonConfigNotFound: true, ReasonRefsInvalid: true, ReasonGatewaysNotReady: true}

// isTerminalFailure applies terminalReadyReasons to a `Ready` condition.
func isTerminalFailure(ready *aigriddraftv1.Condition) bool {
	return ready.GetStatus() == cFalse && terminalReadyReasons[ready.GetReason()]
}

// llmisvcEvents is the lifecycle from creation at `base`, generation `gen`.
func llmisvcEvents(in llmisvcInput, base time.Duration, gen int64) []llmisvcEvent {
	// T1, the first reconcile (kserve:controller.go:279-327): presets merged;
	// reconcileWorkload creates the Deployment, which has no condition yet;
	// reconcileRouter validates the Gateways, creates the scheduler, the
	// InferencePool and the managed HTTPRoute, and writes `url` and
	// `addresses` from the route's Gateways right away (updateRoutingStatus,
	// kserve:router.go:452-553, called before the route is accepted);
	// observeWorkloadStatus writes `workloads` (kserve:workload_status.go:62).
	t1 := func(st *aigriddraftv1.ModelStatus, now time.Time) {
		st.ObservedGeneration = gen // knative:reconciler/reconcile_common.go:82
		if in.unknownPreset != "" {
			// reconcileBaseRefs fails before any child resource is touched.
			// PreProcessReconcile has already initialised every dependent of
			// the living condition set that is absent to Unknown, without a
			// reason (kserve:controller.go:250 ->
			// knative:reconciler/reconcile_common.go:64-66,
			// knative:apis/condition_set.go:416-450; dependents
			// kserve:llm_inference_service_lifecycle.go:164-168), and the
			// workload and router never run to overwrite them.
			msg := fmt.Sprintf(MessageConfigNotFoundFm, in.unknownPreset, []string{in.namespace, in.presetsNamespace})
			setModelCondition(st, now, CondPresetsCombined, cFalse, ReasonConfigNotFound, msg)
			for _, typ := range []string{CondWorkloadsReady, CondRouterReady} {
				if condition(st.Conditions, typ) == nil {
					setModelCondition(st, now, typ, cUnknown, "", "")
				}
			}
			setModelCondition(st, now, CondReady, cFalse, ReasonConfigNotFound, msg)
			return
		}
		setModelCondition(st, now, CondPresetsCombined, cTrue, "", "")
		st.AppliedConfigs = appliedConfigs(in)
		setModelCondition(st, now, CondMainWorkloadReady, cFalse, ReasonProgressing, "")
		if len(in.missingGateways) > 0 {
			// The router returns the validation error before the scheduler and
			// the route; reconcile stops there, so observeWorkloadStatus never
			// runs and `workloads`, `url` and `addresses` stay unset. The
			// Deployment was already created and keeps rolling out.
			var missing []string
			for _, gw := range in.missingGateways {
				missing = append(missing, fmt.Sprintf(MessageGatewayMissingFm, gatewayNS(gw, in.namespace), gw.GetName()))
			}
			setModelCondition(st, now, CondGatewaysReady, cFalse, ReasonRefsInvalid, strings.Join(missing, "; "))
			rollUp(st, now)
			return
		}
		if in.scheduler {
			setModelCondition(st, now, CondSchedulerWorkloadReady, cFalse, ReasonProgressing, MessageDeploymentRolloutInProgress)
		}
		if in.managedRoute {
			setModelCondition(st, now, CondHTTPRoutesReady, cFalse, ReasonWaitingForGateway, fmt.Sprintf(MessageWaitingForGatewayFm, []string{in.namespace + "/" + in.name + managedRouteSuffix}))
			st.Url, st.Addresses = routedAddresses(in)
		} else {
			st.Addresses = workloadServiceAddress(in)
		}
		if in.scheduler {
			setModelCondition(st, now, CondInferencePoolReady, cFalse, ReasonWaitingForGateway, fmt.Sprintf(MessagePoolWaitingForGatewayFm, in.namespace, in.name+inferencePoolSuffix))
		}
		if in.gatewayRefs {
			setModelCondition(st, now, CondGatewaysReady, cTrue, "", "")
		}
		st.Workloads = workloads(in, 0, 0)
		rollUp(st, now)
	}
	if in.unknownPreset != "" {
		return []llmisvcEvent{{base + llmisvcT1, t1}} // never progresses
	}
	events := []llmisvcEvent{{base + llmisvcT1, t1}}
	if len(in.missingGateways) == 0 {
		// T2: the Gateway controller accepted the route and the pool, the
		// scheduler Deployment has an available replica
		// (kserve:scheduler.go:841-876).
		t2 := func(st *aigriddraftv1.ModelStatus, now time.Time) {
			if in.managedRoute {
				setModelCondition(st, now, CondHTTPRoutesReady, cTrue, "", "")
			}
			if in.scheduler {
				setModelCondition(st, now, CondInferencePoolReady, cTrue, "", "")
				setModelCondition(st, now, CondSchedulerWorkloadReady, cTrue, "", "")
			}
			st.Workloads = workloads(in, 0, 1)
			rollUp(st, now)
		}
		events = append(events, llmisvcEvent{base + llmisvcT2, t2})
	}
	return append(events, llmisvcWorkloadEvents(in, base)...)
}

// llmisvcRestartEvents is what a spec change does: the controller observes the
// new generation at once and the Deployment rolls out again. `url` and
// `addresses` stay: the route is unchanged, and only a stopped service clears
// them (kserve:router.go:455-460).
func llmisvcRestartEvents(in llmisvcInput, base time.Duration, gen int64) []llmisvcEvent {
	if in.unknownPreset != "" || len(in.missingGateways) > 0 {
		// The new spec fails in the controller: the presets or the Gateway
		// check now fail, as on a first reconcile.
		return llmisvcEvents(in, base, gen)
	}
	observe := func(st *aigriddraftv1.ModelStatus, now time.Time) {
		st.ObservedGeneration = gen
		st.AppliedConfigs = appliedConfigs(in)
	}
	restart := func(st *aigriddraftv1.ModelStatus, now time.Time) {
		st.Workloads = workloads(in, 0, 1)
		setModelCondition(st, now, CondMainWorkloadReady, cFalse, ReasonProgressing, "")
		rollUp(st, now)
	}
	return append([]llmisvcEvent{{base + llmisvcT1, observe}, {base + llmisvcT2, restart}}, llmisvcWorkloadEvents(in, base)...)
}

// llmisvcWorkloadEvents are the Deployment's reports after it was created:
// Available=False, then the pods ready, or, for a stuck workload, nothing more:
// MinimumReplicasUnavailable for good.
func llmisvcWorkloadEvents(in llmisvcInput, base time.Duration) []llmisvcEvent {
	t2b := func(st *aigriddraftv1.ModelStatus, now time.Time) {
		setModelCondition(st, now, CondMainWorkloadReady, cFalse, ReasonMinimumReplicasUnavailable, MessageMinimumReplicasUnavailable)
		rollUp(st, now)
	}
	if in.replicas == 0 {
		// `replicas: 0` (the CRD's minimum): the Deployment controller marks
		// it Available at its first status sync, availableReplicas 0 >= 0
		// replicas - maxUnavailable (kubernetes:pkg/controller/deployment/
		// sync.go:525-531, v1.35.8), and KServe copies that verdict
		// (kserve:workload_single_node.go:433-436). No pod runs, so neither
		// MinimumReplicasUnavailable nor a stuck image shows; the fake puts
		// that sync at T2.
		ready := func(st *aigriddraftv1.ModelStatus, now time.Time) {
			setModelCondition(st, now, CondMainWorkloadReady, cTrue, "", "")
			if len(in.missingGateways) == 0 {
				st.Workloads = workloads(in, 0, 1)
			}
			rollUp(st, now)
		}
		return []llmisvcEvent{{base + llmisvcT2, ready}}
	}
	if in.stuck {
		return []llmisvcEvent{{base + llmisvcT2b, t2b}}
	}
	t3 := func(st *aigriddraftv1.ModelStatus, now time.Time) {
		setModelCondition(st, now, CondMainWorkloadReady, cTrue, "", "")
		if len(in.missingGateways) == 0 {
			st.Workloads = workloads(in, in.replicas, 1)
		}
		rollUp(st, now)
	}
	return []llmisvcEvent{{base + llmisvcT2b, t2b}, {base + llmisvcT3, t3}}
}

// rollUp recomputes the roll-up conditions at the end of a reconcile.
// WorkloadsReady copies the first False workload sub-condition
// (DetermineWorkloadReadiness, kserve:pkg/apis/serving/v1alpha2/
// llm_inference_service_lifecycle.go:258-283); RouterReady the first False of
// GatewaysReady, HTTPRoutesReady, InferencePoolReady, SchedulerWorkloadReady,
// True when none is False (DetermineRouterReadiness, :405-425). Both are
// Knative dependents of Ready: MarkFalse on a dependent sets Ready to the same
// reason and message, MarkTrue recomputes it from the remaining False
// dependents (knative:apis/condition_set.go:283-301, :395-411). The workload
// is reconciled before the router (kserve:controller.go:308, :312), so Ready
// ends with RouterReady's reason when the router is not ready, else with
// WorkloadsReady's, else True.
func rollUp(st *aigriddraftv1.ModelStatus, now time.Time) {
	firstFalse := func(types ...string) *aigriddraftv1.Condition {
		for _, typ := range types {
			if c := condition(st.Conditions, typ); c != nil && c.GetStatus() == cFalse {
				return c
			}
		}
		return nil
	}
	if main := condition(st.Conditions, CondMainWorkloadReady); main != nil {
		if f := firstFalse(CondMainWorkloadReady); f != nil {
			setModelCondition(st, now, CondWorkloadsReady, cFalse, f.GetReason(), f.GetMessage())
		} else {
			setModelCondition(st, now, CondWorkloadsReady, cTrue, "", "")
		}
	}
	router := firstFalse(CondGatewaysReady, CondHTTPRoutesReady, CondInferencePoolReady, CondSchedulerWorkloadReady)
	if router != nil {
		setModelCondition(st, now, CondRouterReady, cFalse, router.GetReason(), router.GetMessage())
	} else {
		setModelCondition(st, now, CondRouterReady, cTrue, "", "")
	}
	switch f := firstFalse(CondRouterReady, CondWorkloadsReady, CondPresetsCombined); {
	case f != nil:
		setModelCondition(st, now, CondReady, cFalse, f.GetReason(), f.GetMessage())
	default:
		setModelCondition(st, now, CondReady, cTrue, "", "")
	}
}

// appliedConfigs lists the presets the controller merges, in merge order:
// scheduler and router-route when those features are on, template always,
// then the user's baseRefs (kserve:config_merge.go:579-674; the hub instance's
// status.appliedConfigs in models-maas.md 2.1 showed the same order).
func appliedConfigs(in llmisvcInput) []*aigriddraftv1.AppliedConfig {
	var out []*aigriddraftv1.AppliedConfig
	if in.scheduler {
		out = append(out, &aigriddraftv1.AppliedConfig{Name: PresetScheduler, Namespace: in.presetsNamespace, Source: AppliedConfigPreset})
	}
	if in.managedRoute {
		out = append(out, &aigriddraftv1.AppliedConfig{Name: PresetRouterRoute, Namespace: in.presetsNamespace, Source: AppliedConfigPreset})
	}
	out = append(out, &aigriddraftv1.AppliedConfig{Name: PresetTemplate, Namespace: in.presetsNamespace, Source: AppliedConfigPreset})
	for _, ref := range in.baseRefs {
		out = append(out, &aigriddraftv1.AppliedConfig{Name: ref, Namespace: in.presetsNamespace, Source: AppliedConfigUserRef})
	}
	return out
}

// workloads is `status.workloads` (kserve:workload_status.go:62-120): the main
// Deployment, the scheduler Deployment when enabled, and the workload Service.
// `readyReplicas` is each Deployment's availableReplicas (:136).
func workloads(in llmisvcInput, ready, schedReady int32) *aigriddraftv1.Workloads {
	w := &aigriddraftv1.Workloads{
		Primary: &aigriddraftv1.WorkloadRef{ApiGroup: workloadsAPIGroup, Kind: workloadsKind, Name: in.name + workloadMainSuffix, ReadyReplicas: ready},
		Service: &aigriddraftv1.WorkloadRef{Kind: serviceKind, Name: in.name + workloadSvcSuffix},
	}
	if in.scheduler {
		w.Scheduler = &aigriddraftv1.WorkloadRef{ApiGroup: workloadsAPIGroup, Kind: workloadsKind, Name: in.name + workloadSchedSuffix, ReadyReplicas: schedReady}
	}
	return w
}

// routedAddresses is `status.url` and `status.addresses` for a managed route:
// per Gateway an external and an internal root address (model routing by
// header, accepting only the `publishers/...` identifier) and the
// `/<ns>/<name>` and `/publishers/<ns>/models/<model.name>` path addresses
// (accepting that and the bare model name), models-maas.md 2.1 and
// kserve:router_discovery_filter.go:91-127. The external host is the Gateway's
// load balancer, the internal one its backing Service. The controller sorts
// the discovered URLs by their string (kserve:router.go:517-519) and takes
// `url` from the external ones, preferring the `/<ns>/<name>` path
// (:530, :555-572).
func routedAddresses(in llmisvcInput) (string, []*aigriddraftv1.Address) {
	publishers := "publishers/" + in.namespace + "/models/" + in.modelName
	var out []*aigriddraftv1.Address
	url := ""
	for _, gw := range in.gateways {
		ns := gatewayNS(gw, in.namespace)
		origin := &aigriddraftv1.ObjectRef{Group: GatewayGroup, Kind: GatewayKind, Name: gw.GetName(), Namespace: ns}
		for _, side := range []struct{ host, name, routing string }{
			{"https://" + gw.GetName() + "." + in.appsDomain, AddressGatewayExternal, AddressGatewayExternalModelRouting},
			{"https://" + gw.GetName() + "-" + gatewayClassName + "." + ns + ".svc.cluster.local", AddressGatewayInternal, AddressGatewayInternalModelRouting},
		} {
			out = append(out,
				&aigriddraftv1.Address{Name: side.routing, Url: side.host + "/", Origin: origin, Models: []string{publishers}},
				&aigriddraftv1.Address{Name: side.name, Url: side.host + "/" + in.namespace + "/" + in.name, Origin: origin, Models: []string{publishers, in.modelName}},
				&aigriddraftv1.Address{Name: side.name, Url: side.host + "/" + publishers, Origin: origin, Models: []string{publishers, in.modelName}},
			)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetUrl() < out[j].GetUrl() })
	for _, a := range out {
		if a.GetName() == AddressGatewayExternal && strings.HasSuffix(a.GetUrl(), "/"+in.namespace+"/"+in.name) {
			url = a.GetUrl()
			break
		}
	}
	return url, out
}

// workloadServiceAddress is `status.addresses` without a route: the workload
// Service only, no `url` (kserve:router.go:466-482). The scheme is https when
// the ingress configuration enables LLMInferenceService TLS, which the fake
// assumes.
func workloadServiceAddress(in llmisvcInput) []*aigriddraftv1.Address {
	return []*aigriddraftv1.Address{{Url: "https://" + in.name + workloadSvcSuffix + "." + in.namespace + ".svc.cluster.local"}}
}

// setModelCondition writes a Knative condition: status, reason and message
// replaced, lastTransitionTime moved whenever any of them changes
// (knative:apis/condition_set.go:187-209 keeps the old time only for an
// identical condition, and the reconciler re-applies that rule between the
// stored and the reconciled status, knative:reconciler/reconcile_common.go:93-111).
// An empty reason or message is left unset, as a True Knative condition
// carries neither.
func setModelCondition(st *aigriddraftv1.ModelStatus, now time.Time, typ string, status aigriddraftv1.ConditionStatus, reason, message string) {
	if c := condition(st.Conditions, typ); c != nil && (c.GetStatus() != status || c.GetReason() != reason || c.GetMessage() != message) {
		c.LastTransitionTime = timestamppb.New(now)
	}
	st.Conditions = writeCondition(st.Conditions, now, typ, status, reason, message)
}

// writeCondition is the apimachinery meta.SetStatusCondition write the OCM
// controllers use: a new condition is appended with lastTransitionTime=now,
// an existing one keeps its time unless the status changed; reason and
// message are replaced.
func writeCondition(conds []*aigriddraftv1.Condition, now time.Time, typ string, status aigriddraftv1.ConditionStatus, reason, message string) []*aigriddraftv1.Condition {
	var cond *aigriddraftv1.Condition
	for _, c := range conds {
		if c.GetType() == typ {
			cond = c
			break
		}
	}
	if cond == nil {
		cond = &aigriddraftv1.Condition{Type: typ, LastTransitionTime: timestamppb.New(now)}
		conds = append(conds, cond)
	} else if cond.GetStatus() != status {
		cond.LastTransitionTime = timestamppb.New(now)
	}
	cond.Status = status
	cond.Reason, cond.Message = nil, nil
	if reason != "" {
		cond.Reason = proto.String(reason)
	}
	if message != "" {
		cond.Message = proto.String(message)
	}
	return conds
}

// condition returns the condition of that type from a list, or nil.
func condition(conds []*aigriddraftv1.Condition, typ string) *aigriddraftv1.Condition {
	for _, c := range conds {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

// conditionStatusString is the Kubernetes spelling of a condition status.
func conditionStatusString(st aigriddraftv1.ConditionStatus) string {
	switch st {
	case cTrue:
		return "True"
	case cFalse:
		return "False"
	default:
		return "Unknown"
	}
}

// deriveHubPhase is the phase derivation for a hub deployment
// (models-maas.md 2.1, refined): Ready True -> READY; False with a reason
// that does not recover on its own (terminalReadyReasons: a missing preset,
// a missing or unprogrammed Gateway) -> FAILED with its reason and message;
// False with any other reason, and absent or Unknown -> PENDING, reason and
// message kept. KServe reports Ready False while a healthy model starts
// (`WaitingForGateway`, `Progressing`, `MinimumReplicasUnavailable`), so
// "False means failed" would show every new model as failed for its first
// seconds. The cost is the one the placed path already has (see
// terminalReadyReasons): a workload that never becomes available keeps
// `MinimumReplicasUnavailable` for good, which no condition tells apart from
// a slow start, so it stays PENDING with that reason.
func deriveHubPhase(st *aigriddraftv1.ModelStatus) (aigriddraftv1.ModelPhase, string, string) {
	ready := condition(st.GetConditions(), CondReady)
	switch {
	case ready.GetStatus() == cTrue:
		return aigriddraftv1.ModelPhase_MODEL_PHASE_READY, "", ""
	case isTerminalFailure(ready):
		return aigriddraftv1.ModelPhase_MODEL_PHASE_FAILED, ready.GetReason(), ready.GetMessage()
	default:
		return aigriddraftv1.ModelPhase_MODEL_PHASE_PENDING, ready.GetReason(), ready.GetMessage()
	}
}
