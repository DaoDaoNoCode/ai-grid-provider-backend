# Models (LLMInferenceService, Placement, ManifestWorkReplicaSet)

What the fake backend and the BFF implement for this area, where they deviate from the
real controllers, which texts are invented, and what remains. The fixture data is described in
`docs/draft/fixtures/default.md`, the HTTP contract in `ai-grid-provider-console/docs/plan/bff-api.md`, the proto in
`docs/draft/proto/README.md`. The research reports cited here (`docs/draft/research/`) are background
reading; the source lines each behaviour follows are in comments next to the code
(opendatahub-io/kserve 01d6280, knative.dev/pkg 0795cb8, open-cluster-management-io/ocm 8b0d31f,
api 2c5da18, sdk-go 592fbe6).

## Implemented

Fake (`internal/fakeinfra/draft/{models.go,kserve.go,placement.go,work.go}`): a store of models
whose desired state is a KServe `LLMInferenceService` spec, optionally with an ACM `Placement`
and a `ManifestWorkReplicaSet` (both `model-<name>` in the model's namespace, labelled
`ai-grid/model=<name>`). The store keeps only what the controllers would have written (when a
model was created, when each generated work was applied, the last Placement decision) and
derives everything else on every read from the clock and the current clusters.

Admission. `CreateModel` checks, as InvalidArgument: a DNS-1123 subdomain name and a DNS-1123
label namespace with apimachinery's messages; the CRD's openAPI schema rules with the API
server's field-error texts (`replicas` minimum 0, "spec.replicas: Invalid value: -1:
spec.replicas in body should be greater than or equal to 0"; `scaling.maxReplicas` required,
"spec.scaling.maxReplicas: Required value", and minimum 1; `scaling.minReplicas` minimum 1,
default 1, so the proto's unset 0 reads as 1); and the CRD's four CEL rules verbatim ("replicas
and scaling are mutually exclusive; use scaling for autoscaled deployments or replicas for
static deployments", "either wva or keda must be specified when scaling is configured", "at
least one trigger is required when using direct KEDA scaling", "minReplicas cannot exceed
maxReplicas"), schema errors first as the API server lists them. The rest are console checks
with console wording, which the API server would accept: `spec.model.uri` with a known scheme
(the CRD types it as a plain string), Placement selector and toleration operators, a non-negative `numberOfClusters`, existing cluster sets (only the toleration
effect is a CRD enum), and the rollout fields (`maxConcurrency` and `maxFailures` against the
CRD pattern `^((100|[0-9]{1,2})%|[0-9]+)$`, `progressDeadline` against
`^(([0-9])+[h|m|s])|None$` and also parsable, `maxConcurrency` only with `Progressive`, since
`All` and `ProgressivePerGroup` have no such field). Then `AlreadyExists` with the API server's
message and, before writing an MWRS, `FailedPrecondition` "ManifestWorkReplicaSet feature gate
disabled on hub" (console text) when `settings.json` turns the gate off. The object comes back
with no LLMInferenceService conditions; a placed model already carries its Placement decision
and MWRS conditions (and, for `All`, its works).

LLMInferenceService lifecycle (the lifecycle of `docs/draft/research/models-maas.md` 8.2, replayed on
the scenario clock; every reconcile is stamped with the time it happened). `lastTransitionTime`
follows Knative: it moves whenever a condition's status, reason or message changes. Conditions
are sorted by type.
- T1 (5 s), the first reconcile, workload before router: `PresetsCombined=True` and
  `appliedConfigs` (scheduler, router-route and template presets, then the `baseRefs` as
  `UserRef`); the Deployment created with no condition yet, so
  `MainWorkloadReady=WorkloadsReady=False/Progressing` with an empty message;
  `SchedulerWorkloadReady=False/Progressing` "Deployment rollout in progress";
  `HTTPRoutesReady=False/WaitingForGateway` "The following HTTPRoutes exist but no Gateway
  controller has accepted them yet: [<ns>/<name>-kserve-route]";
  `InferencePoolReady=False/WaitingForGateway` "Inference Pool <ns>/<name>-inference-pool exists
  but no Gateway controller has accepted it yet"; `GatewaysReady=True` (only when gateway refs
  are written); `RouterReady` and `Ready` False/`WaitingForGateway` with the routes message;
  `url` (the external `/<ns>/<name>` address) and the six `addresses`, written with the route
  before the model is Ready, sorted by URL (the internal `<gateway>-<gatewayClass>` Service host
  sorts first); `workloads` (`<name>-kserve`, `<name>-kserve-router-scheduler`, the
  `<name>-kserve-workload-svc` Service) with no ready replicas.
- T2 (15 s): route and pool accepted, scheduler available (its `readyReplicas` 1), `RouterReady`
  True, `Ready=False/Progressing`.
- T2b (20 s): the Deployment reports `Available=False`: `MainWorkloadReady`, `WorkloadsReady` and
  `Ready` False/`MinimumReplicasUnavailable` "Deployment does not have minimum availability.".
- T3 (45 s): everything True, `workloads.primary.readyReplicas` = replicas.
- `replicas: 0` skips T2b and T3: the Deployment controller marks a zero-replica Deployment
  `Available` at its first status sync (0 available >= 0 - maxUnavailable, Kubernetes
  `pkg/controller/deployment/sync.go:525-531`), so at T2 `MainWorkloadReady` is True and the model
  Ready with `readyReplicas` 0, whatever the image.

Roll-ups: `WorkloadsReady` copies `MainWorkloadReady` when False; `RouterReady` copies the
first False of `GatewaysReady`, `HTTPRoutesReady`, `InferencePoolReady`,
`SchedulerWorkloadReady` (KServe's fixed order); `Ready` ends with `RouterReady`'s reason when
the router is not ready (it is reconciled last), else `WorkloadsReady`'s. So a healthy model
reads `WaitingForGateway`, then `Progressing`, then `MinimumReplicasUnavailable` while it starts.

Failure modes:
- An unknown `baseRef` stops at T1 with `PresetsCombined` and `Ready` False/`ConfigNotFound`
  and KServe's message (`LLMInferenceServiceConfig "<name>" not found in namespaces [<ns>
  <apps-ns>]`); `WorkloadsReady` and `RouterReady` Unknown (Knative initialises every dependent
  of `Ready` before the reconcile, `knative:reconciler/reconcile_common.go:64-66`); nothing else.
- An image or model name containing `oom` or `fail`: the pods never become available, and KServe
  ends on the Deployment's `Available` condition, so the model keeps
  `MinimumReplicasUnavailable` for good. It never shows `ProgressDeadlineExceeded`: KServe marks
  it on the way through the Deployment's conditions and then overwrites it, with the
  `Available` verdict or, without an `Available` condition, with `Progressing` and an empty
  message (`workload_single_node.go:418-443`, :442), before its one status write
  (`controller.go:246-262`); and the deployment controller writes `Available` on every status
  sync (Kubernetes `pkg/controller/deployment/sync.go:525-531`, v1.35.8), so only a Deployment
  without any status yet reads `Progressing`. `url` and `addresses` are present, `readyReplicas`
  stays 0. On a hub the phase is PENDING/`MinimumReplicasUnavailable` for good (no condition
  tells it from a slow start); on a spoke see the rollout below.
- A referenced Gateway that does not exist (anything but
  `openshift-ingress/{openshift-ai-inference,maas-default-gateway}`, the product-default names):
  `GatewaysReady`, `RouterReady` and `Ready` False/`RefsInvalid` "Gateway <ns>/<name> does not
  exist" (several joined with "; "). The router stops there, so there is no route, scheduler or
  pool condition, no `url`, `addresses` or `workloads`; the Deployment is still created and
  `MainWorkloadReady` reaches True at T3 while `Ready` stays `RefsInvalid`.
- A router without a managed route: no `HTTPRoutesReady`, the workload Service URL as the only
  address, no `url`. Gateway refs omitted: no `GatewaysReady`, the route uses the default Gateway.

A spec change bumps the model's generation. A change of the LLMInferenceService's own spec
(everything but `placement` and `rollout`) bumps its generation too: the controller observes it at
T1 after the change, and at T2 the Deployment rolls out again from `Progressing`; `url` and
`addresses` are kept. An update after which the spec, labels and annotations equal the stored
ones is not written: `UpdateModel` answers the stored model without bumping `resource_version`
or `generation`, as the API server skips an unchanged update (`k8s.io/apiserver` v0.35.8
`pkg/storage/etcd3/store.go:553-577`); the lock and the admission checks run first. Removing
the placement without naming `spec.rollout` drops the rollout with it.

Placement (`docs/draft/research/acm.md` 3, items 6 and 7), recomputed on every pass from the clusters
and the previous decision: the sets bound into the namespace (`global` = every cluster,
`default` = the `clusterset=default` label; `models.json` can unbind a namespace), terminating
clusters dropped before the empty-set check, the predicate (AND of `matchLabels`,
`matchExpressions` with Kubernetes `In|NotIn|Exists|DoesNotExist` semantics and claim
expressions), then taints: `NoSelect` filters unless tolerated, `NoSelectIfNew` filters only a
cluster not already selected, `PreferNoSelect` never; a toleration matches on effect (empty =
any), key (empty = any), value for `Equal`, and `tolerationSeconds` from the taint's
`timeAdded` (expired when unset). A toleration with an empty key and no `Exists`, or `Exists`
with a value, gives `PlacementMisconfigured=True/Misconfigured` ("TaintToleration:If the key is
empty, operator must be Exists." or "TaintToleration:If the operator is Exists, the value should
be empty.") and no decision, when the predicate left a cluster to filter. `numberOfClusters`
keeps the clusters already selected (Steady), then takes the rest by name; the decision is
listed by name. `PlacementSatisfied` has the exact reasons and messages
(`AllDecisionsScheduled`, `NotAllDecisionsScheduled` "<n> cluster decisions unscheduled",
`NoManagedClusterMatched`, `NoManagedClusterSetBindings`, `NoIntersection`,
`AllManagedClusterSetsEmpty`); otherwise `PlacementMisconfigured=False/Succeedconfigured`.

MWRS and works (`docs/draft/research/acm.md` 4, items 8-13). One work per selected cluster named
`model-<name><5 chars from the generateName alphabet>` (the controller's generateName is the
MWRS name with no separator, cut to 58 characters; random, tests inject a deterministic suffix
with `SetWorkSuffixGenerator`). The template is the LLMInferenceService, the spec without
`placement` (the Placement object) and `rollout` (the MWRS's `placementRefs[].rolloutStrategy`).
Only works whose spec is the current template count: a stale work holds no rollout slot, is left
out of the summary, and its cluster is to apply again like a new one.
- `All` applies every cluster at once. `ProgressivePerGroup` rolls out a whole decision group at
  once; the console's Placements have one group, so it behaves like `All`.
- `Progressive` fills `maxConcurrency` slots in cluster-name order (a percentage rounds up; empty,
  zero or more than the selected clusters means all of them). A slot is held while the work is
  to apply, progressing or failed, and for `minSuccessTime` after it succeeded. A failed cluster
  counts towards `maxFailures` at once (a value above the selected clusters falls back to 0, as
  in sdk-go); a progressing or failed one times out `progressDeadline` after its `Progressing`
  (or, failed, `Degraded`) condition's `lastTransitionTime` (`0s` at once, empty or `None`
  never), frees its slot and counts too; beyond `maxFailures` nothing more is applied. A
  `ToApply` cluster (no work yet, or a work whose `Applied` is not True or whose `Progressing` is
  missing) never times out, so it holds its slot and the rollout stalls on it, as on the hub. The
  fake computes the exact instant a slot frees, so timing does not depend on polling, with one
  exception declared below: a decision change that only time makes.
- Only the spoke agent writes a work's status: a cluster that is not Available has a work with an
  empty status (no conditions, no manifests), `ToApply` for as long as the agent is silent; a
  cluster that loses its lease after reporting keeps its last report, and its lifecycle resumes
  where it stopped. From the moment the agent reports: `Applied=True/AppliedManifestWorkComplete`
  at once, `Available` absent until the status controller's first sync 2 s later, then
  `True/ResourcesAvailable`; two manifests (the namespace and the LLMInferenceService) with
  `Applied`, `Available`, `StatusFeedbackSynced` (`NoStatusFeedbackSynced` for the namespace;
  for the model it is re-added, with a new time, whenever the feedback values change); feedback
  `ready` ("False" then "True") and `url` (from the spoke's T1). The template's CEL rules are
  evaluated against the spoke object: `Progressing=True` until its `Ready` is True, and
  `Degraded=True` while `Ready` is False with a terminal reason (`ConfigNotFound`, `RefsInvalid`,
  `GatewaysNotReady`), reason `ConditionRuleEvaluated` per manifest and
  `ConditionRulesAggregated` on the work (the single rule-bearing manifest's message copied;
  "Aggregated from <n> manifest conditions" otherwise). `observedGeneration` is on the
  `Applied` conditions, the rule conditions and the work-level conditions only.
- A spoke whose pods never come up keeps `Ready=False/MinimumReplicasUnavailable`, which the
  Degraded rule does not match: its work stays `Progressing=True`, `Degraded=False`, feedback
  `ready=False`, so the MWRS reads `ManifestworkApplied=False/Processing` and the model
  PENDING/`Processing` indefinitely (no MWRS condition shows the progress-deadline timeout, which
  only frees the slot in a `Progressive` rollout).
- A cluster labelled `ai-grid-mock/fail-apply=true` (a console mock label) behaves like a spoke
  without the KServe CRD: `Applied=False/AppliedManifestWorkFailed`, the manifest
  `AppliedManifestFailed` "Failed to apply manifest: the server doesn't have a resource type
  "LLMInferenceService"" with an empty `resource` in its meta, the manifest
  `Available=Unknown/IncompleteResourceMeta` "Resource meta is incomplete", and the work
  `Available=Unknown/ResourcesStatusUnknown` "1 of 2 resources have unknown status". The status
  controller skips the feedback and the rules of that manifest, so no `Progressing` or
  `Degraded` exists and the cluster stays `ToApply`, holding its slot without a timeout.
- Summary: `total`, `applied`, `available`, `progressing`, `degraded` over the works at the
  current template. `updated` and `desiredTotal` exist in the CRD but the controller never writes
  them; the fake leaves them out and the BFF does not expose them.
- MWRS conditions: `PlacementVerified` `AsExpected` once a work at the current template exists,
  else `PlacementDecisionEmpty`; `PlacementRolledOut` `Complete` when the succeeded works at the
  current template number the selected clusters (so also for an empty decision), else
  `Progressing`; `ManifestworkApplied` `AsExpected`, `Processing`, `NotAsExpected` or
  `PlacementDecisionEmpty` from the summary.
- A template change re-applies the works (all at once for `All`, in name order as slots free for
  `Progressive`). A change of only `rollout` or `placement` re-applies nothing, as the controller
  compares each work with the template alone (`deploy_reconcile.go:80-89`): a new strategy only
  schedules the works still to apply, a new placement only adds and removes the works of clusters
  entering and leaving the decision, and under either no work is applied at an instant before the
  change. The same holds when the decision loses a cluster or changes its count without a PATCH (a
  label, a taint, a terminating cluster): the works still to apply are scheduled from the moment
  the fake notices it, after the rollout has caught up under the previous decision to that
  moment, so a slot that freed before the change is used when it freed however often the fake is
  read. Scenario events and API writes are noticed at their own time; a change that only time
  makes (a `tolerationSeconds` running out) is noticed at the next read, and up to then the
  previous decision counts as held. That delays a rollout in two cases: a percentage
  `maxConcurrency` or `maxFailures` whose slot count changes with the decision, and a cluster
  whose toleration expires while it holds a Progressive slot, which keeps the slot until the next
  read (the hub's scheduler requeues the Placement at the expiry and frees the slot then). No
  fixture uses `tolerationSeconds`, so the shipped scenarios are not affected. A work's own `generation` is the ManifestWork's
  counter, 1 when it is created (also for a cluster that joins later or a placement added by a
  PATCH) and bumped by each update that changes its spec; a cluster leaving the decision loses
  its work;
  `DeleteModel` removes the model at once (a hub model's finalizer window is a declared
  deviation, below; the console's MWRS uses the CRD default cascade
  `Background`: the finalize reconciler deletes the works and drops the finalizer in one pass, so
  the MWRS is gone immediately and the works drain in the cluster namespaces out of sight), and
  answers `FailedPrecondition` through the `referencedBy` hook, which the subscriptions area wires
  up (`SetReferencedBy`): a subscription or an auth policy that names the model blocks the delete,
  the fixtures' unmanaged policies included, and the message names each referrer.

Phase (a console derivation, see `ModelPhase` in `model_type.proto`): a hub deployment is READY
when `Ready` is True, FAILED when it is False with a reason that does not recover on its own
(`terminalReadyReasons`: `ConfigNotFound`, `RefsInvalid`, `GatewaysNotReady`, the set the
ManifestWork template's Degraded rule uses), PENDING otherwise, with the condition's reason:
KServe reports `Ready` False with `WaitingForGateway`, `Progressing` and
`MinimumReplicasUnavailable` while a healthy model starts, so "False means failed" showed every
new model as failed for its first 45 s. A placed
model is READY when `PlacementRolledOut` and `ManifestworkApplied` are True; FAILED when
`ManifestworkApplied` is `NotAsExpected` and a work is degraded or failed to apply; otherwise
PENDING with the reason of `PlacementVerified` when it is False, of `PlacementRolledOut` when
`ManifestworkApplied` is True, else of `ManifestworkApplied`.

`GetHubCapabilities` answers from `settings.json`. Mutations are gated by the provider-admin rule
(a viewer gets PermissionDenied, 403 at the BFF); every RPC needs `authorization`. `ListModels`
evaluates `filter` as CEL over `this` (an `ai_grid.v1.Model` as the read derives it, status
included, proto field names) and `now`, with cel-go and its strings extension, over every model
and before paging, so `total` counts the matches (`internal/fakeinfra/draft/celfilter`, the
List rule of `docs/draft/research/backend-conventions.md` 5.3). Any expression works, for example
`this.metadata.namespace == "ai-grid-models" && "ai-grid/family" in this.metadata.labels &&
this.metadata.labels["ai-grid/family"] == "granite"` or `this.status.phase ==
ai_grid.v1.ModelPhase.MODEL_PHASE_FAILED`; in CEL a missing key is a `no such key` error, not
false, so an unguarded `this.metadata.labels["k"]` on a model without `k` makes the list
`InvalidArgument` (`filter <f> failed on '<namespace>/<name>': no such key: k`), as cel-go
answers. A filter that does not compile or is not a bool is `InvalidArgument` too.

## Deviations from the real controllers

- The console's Degraded rule fires only on the terminal `Ready=False` reasons above (the rule
  text is in `kserve.go`, the template's contract); KServe reports `MinimumReplicasUnavailable`
  both while a model starts and when it never comes up, so a rule cannot separate them and a
  stuck spoke reads Progressing (as on the hub with this template), not Degraded.
- The placed phase reads `NotAsExpected` without a degraded or unapplied work as PENDING, because
  OCM reports `NotAsExpected` for a work whose `Available` is simply not True yet.
- Stage offsets T1 to T3 are fixed (5, 15, 20, 45 s); a real model takes as long as its image
  pull and model load take.
- A spec change replays the Deployment rollout from `Progressing` at T2 and
  `MinimumReplicasUnavailable` at T2b. A real rolling update keeps the old pods serving: with the
  default strategy an image change keeps `Available=True` (and `Ready` True) throughout, and a
  replicas increase reports `MinimumReplicasUnavailable` until the new pods are ready.
- The scheduler EPP-config preset (`v3-6-0-kserve-config-llm-scheduler-eppconfig-default`) is not
  listed in `appliedConfigs`; upstream merges it after the scheduler preset when the scheduler's
  router image is at least 0.11.0, which was not observed for the installed 3.6 build. Only the
  single-node main workload is modelled (no prefill, worker, tokenizer, scaling or LoRA
  conditions), and the gateway controller is assumed to name a Gateway's Service
  `<gateway>-openshift-default` (an invented class); a router-less model's workload Service
  address assumes LLMInferenceService TLS is on (`https`).
- Knative's `severity` (`Info` on the sub-conditions) is dropped: the proto `Condition` has no
  such field.
- MWRS and Placement condition times, and the instant a decision change frees rollout slots, are
  the time the fake noticed a change (at most one poll late; the real controllers are
  event-driven with requeues). Placement decisions are recomputed
  at each pass, so the Steady prioritizer and `NoSelectIfNew` see the decision of the previous
  pass. The Balance prioritizer is not modelled: every cluster gets its full score, as if no other
  Placement selected it, which only matters with `numberOfClusters`. Decision groups, spread
  constraints, prioritizer configuration and add-on scores are not evaluated.
- The work agent: the status controller's first pass comes 2 s after `Applied` (acm.md item 9;
  the real one is triggered by the `Applied` patch itself), and afterwards the rule conditions and
  the feedback follow the spoke object at once, where the real agent re-polls every 10 to 19 s
  (`status-sync-interval` 10 s with 0.9 jitter, feedback scrape type `Poll`), so on the hub
  `Progressing`, `Degraded`, `ready` and `url` lag the spoke by up to 19 s.
- A re-applied work's conditions restart from the re-apply: the real agent keeps the earlier
  conditions (with their old `observedGeneration`) until it re-syncs, and keeps their
  `lastTransitionTime` where the status does not change (`meta.SetStatusCondition`), so on the
  hub `Applied` keeps the first apply's time and a work whose `Progressing` stays True across the
  update keeps the earlier time, which its `progressDeadline` then counts from. The spoke object
  of a re-applied work replays a fresh lifecycle from T1 rather than a rolling update.
- The rollout settings a run-time sdk-go error would reject (a `progressDeadline` such as `5mx`,
  which the CRD pattern lets through) are refused on write instead. The backend must write a bare
  integer `maxConcurrency`/`maxFailures` as an IntOrString int: sdk-go rejects a string without
  `%`.
- Generated work names are random unless a test injects a suffix; the `ai-grid-mock/fail-apply`
  label is a mock-only switch for the missing-CRD case.
- A hub model disappears at once on `DeleteModel`. KServe adds `serving.kserve.io/llmisvc-finalizer`
  on its first reconcile (`controller.go:204-211`) and on delete runs its finalize pass (group
  membership, platform resources, scheduler service account; requeued every 5 s while incomplete,
  `controller.go:215-231`, `:330-354`) before removing it, so on the hub the object is first seen
  with a `deletionTimestamp` and the finalizer, normally for one reconcile. `metadata.finalizers`
  is not shown.
- The `apierror` name marker (`ReconcileSingleNodeWorkloadError`) is deferred: its message is the
  free-form `err.Error()` of the failed Deployment write, so any plausible API-server error would
  do.

Invented texts (console wording, not hub messages): the admission messages other than the CEL, CRD
schema and DNS-1123 texts, "ManifestWorkReplicaSet feature gate disabled on hub", the `referencedBy`
message, the template's rule messages ("LLMInferenceService rolling out", "LLMInferenceService
not ready"), the ListModels filter and paging messages (`filter <f> is not a valid CEL
expression: ...`, `filter <f> must evaluate to a bool, not <type>`, `filter <f> failed on '<id>':
...`, with cel-go's own error after the colon), and the hosts and domains
(`*.example.com`, `apps.hub.example.com`).

## BFF

`GET /api/v1/models?offset=&limit=&filter=` (the client's CEL forwarded unchanged; the removed
`namespace` and `label` parameters are 422 `query parameter "<name>" is not supported; send a CEL
filter in "filter"`), `GET/PATCH/DELETE /api/v1/models/{namespace}/{name}`,
`POST /api/v1/models` (201 + Location) and `GET /api/v1/capabilities` (501 against today's
backend), with the JSON shape in `ai-grid-provider-console/docs/plan/bff-api.md`: the POC's `ManagedModel` without its
inventions, plus `placement` (selector, selected clusters, `PlacementSatisfied`) and `rollout`
(strategy, summary without `updated`, per-site works with their condition statuses and the
`ready` and `url` feedback); `url` is omitted when empty; PATCH is one `UpdateModel` with every
present field in the mask (no read first, no comparison; `labels` and `annotations` are whole
maps, and the display name and description are their `openshift.io/*` annotation keys, not body
fields on PATCH or POST), locked on the `If-Match` version the client must send (428 without it),
and a body that names no field is 422; DELETE needs `If-Match` too and passes
it as `DeleteModelRequest.resource_version`, which the fake checks like a Kubernetes
delete precondition (`Aborted` with the update's wording when stale); GET, POST and PATCH send
the `resourceVersion` as the `ETag`.

UI: the Models page and deploy wizard are the port's; a delete blocked
by a subscription or auth policy shows the 412 message.

## Remaining

Multi-node and prefill specs, decision groups (several groups for `ProgressivePerGroup`,
mandatory groups), the Balance prioritizer, the agent's poll lag, a rolling update that keeps
`Ready` True, the `apierror` failure mode, `status.router` in the proto, removing
`RolloutSummary.updated` (field 6) from the proposal, and the catalog reference check (subscriptions and auth policies already
block a delete; catalog entries do not exist yet).

Backend ask: the `Models` service and `Model` message as drafted in
`proto/ai_grid/draft/v1/model*.proto`, with the phase derived server-side;
`AiGrid/GetHubCapabilities`; the ManifestWorkReplicaSet feature gate on the hub
(`docs/draft/research/acm.md` open question 1).
