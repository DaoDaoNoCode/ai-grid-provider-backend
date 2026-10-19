# ACM / OCM hub behaviour for the AI Grid console mock

Research spike, 2026-10-15. Read-only (`oc get`, `oc explain`, `oc api-resources`) against the
dev hub plus upstream source and docs. All example values below are fictional (cluster names
`site-*`, `example.com` hosts, invented UUIDs). Field names, enum values, condition types and
reason strings are real and come from the cited CRD paths or source files.

Citation keys used throughout:

- `explain:<resource>.<path>` = `oc explain <resource>.<path>` on the hub (ACM 2.17.3, OCP 4.22, MCE
  `cluster-manager` with `workDriver: kube`).
- `hub:` = observed on the live hub (shape only; values anonymised).
- `api:<path>` = https://github.com/open-cluster-management-io/api/blob/main/<path>
- `ocm:<path>` = https://github.com/open-cluster-management-io/ocm/blob/main/<path>
- `sdk:<path>` = https://github.com/open-cluster-management-io/sdk-go/blob/main/<path>
- `mcic:<path>` = https://github.com/stolostron/managedcluster-import-controller/blob/main/<path>
- `found:<path>` = https://github.com/stolostron/multicloud-operators-foundation/blob/main/<path>
- `cla:<path>` = https://github.com/stolostron/cluster-lifecycle-api/blob/main/<path>
- `console:<path>` = https://github.com/stolostron/console/blob/main/<path>
- `docs:<page>` = https://open-cluster-management.io/docs/<page>

## Summary

1. A `ManagedCluster` is cluster-scoped; its status is written by three actors: the hub
   registration controller (`HubAcceptedManagedCluster`, lease-based `ManagedClusterConditionAvailable=Unknown`,
   `ManagedClusterConditionClockSynced`, taints), the spoke agent (`ManagedClusterJoined`, `Available=True/False`,
   `capacity`, `allocatable`, `version`, `clusterClaims`) and ACM's import controller
   (`ManagedClusterImportSucceeded`, labels `name`, annotation `open-cluster-management/created-via`).
2. ACM's foundation controller derives the labels `cloud`, `vendor`, `clusterID`, `openshiftVersion`,
   `openshiftVersion-major`, `openshiftVersion-major-minor` from `ManagedClusterInfo`/claims (`cloud` and
   `vendor` only while they read `auto-detect`, so they are not re-added once removed; `clusterID` and
   `openshiftVersion*` are rewritten on every reconcile; `found:pkg/controllers/clusterinfo/autodetect_controller.go`
   L68-121); the OCM
   registration hub's addon feature-discovery controller sets
   `feature.open-cluster-management.io/addon-<name>=available|unhealthy|unreachable` from each add-on's
   `Available` condition (True -> `available`, False -> `unhealthy`, Unknown or missing -> `unreachable`).
3. Cluster health for a UI is the triple (`HubAcceptedManagedCluster`, `ManagedClusterJoined`,
   `ManagedClusterConditionAvailable` True/False/Unknown) plus `ManagedClusterImportSucceeded` and
   `deletionTimestamp`; the ACM console maps that to `notaccepted | pendingimport | importing |
   importfailed | ready | offline | unknown | detaching`.
4. Clusters without a `cluster.open-cluster-management.io/clusterset` label are in no exclusive set;
   OCM's ManagedCluster mutating webhook labels every cluster that has no set label `clusterset=default`
   (with the `DefaultClusterSet` feature on and when the requester may add clusters to `default`;
   `ocm:pkg/registration/webhook/v1/managedcluster_mutating.go` L50-63). The `global` set (`LabelSelector {}`) always
   contains every cluster and is bound into `open-cluster-management-global-set`.
5. A `Placement` only sees clusters of sets bound (`ManagedClusterSetBinding`, condition `Bound`) in its
   own namespace; it writes `status.numberOfSelectedClusters`, `status.decisionGroups`, and the conditions
   `PlacementSatisfied` (`AllDecisionsScheduled` | `NotAllDecisionsScheduled` | `NoManagedClusterSetBindings` |
   `NoIntersection` | `AllManagedClusterSetsEmpty` | `NoManagedClusterMatched`) and `PlacementMisconfigured`
   (`Succeedconfigured` | `Misconfigured`).
6. `PlacementDecision`s are named `<placement>-decision-<n>` (n from 1, max 100 clusters each), labelled
   `cluster.open-cluster-management.io/placement=<placement>`, `decision-group-index`, `decision-group-name`;
   `status.decisions[].clusterName` is sorted by name; the controller writes only `clusterName` (no `reason`,
   no `score`; `ocm:pkg/placement/controllers/scheduling/scheduling_controller.go` L443, L736, L748-753). Label changes on a cluster recompute
   the decision within seconds (cluster informer) and the console must treat decisions as live.
7. `ManifestWork` status is `Applied` (`AppliedManifestWorkComplete`/`AppliedManifestWorkFailed`), `Available`
   (`ResourcesAvailable`/`ResourcesNotAvailable`/`ResourcesStatusUnknown`), optional rule-driven
   `Progressing`/`Degraded`/`Complete`, and per-manifest `resourceStatus.manifests[]` with `statusFeedback.values`
   (`Integer|String|Boolean|JsonRaw`). WellKnownStatus feedback exists only for Deployment, DaemonSet, Job, Pod.
8. `ManifestWorkReplicaSet` (v1alpha1) is CRD-present on the hub but the feature gate is OFF: no
   `cluster-manager-work-controller` Deployment exists. An MWRS created today is accepted by the API server,
   gets no finalizer, no status and no ManifestWorks. Enabling needs `ClusterManager.spec.workConfiguration.featureGates[{feature: ManifestWorkReplicaSet, mode: Enable}]`.
9. When enabled, MWRS creates one ManifestWork per selected cluster in the cluster namespace, labelled
   `work.open-cluster-management.io/placementname` and (on MCE 2.17.3, the certain one) the legacy
   `work.open-cluster-management.io/manifestworkreplicaset=<ns>.<name>`; upstream main additionally uses
   `generateName=<mwrs-name>` with no separator, so names read `<mwrs-name><5 random chars>` (since 2026-03;
   `ocm:.../manifestworkreplicaset_deploy_reconcile.go` L433) and the `ownerkey-hash` label plus owner annotation (since
   2026-10). It reports `summary{total,applied,available,degraded,progressing}` (`total` counts the works that
   exist and match the current template, not the selected clusters) plus `PlacementVerified`,
   `PlacementRolledOut`, `ManifestworkApplied` conditions. At current upstream main a work without a
   `Progressing` condition never counts as Succeeded, so the template needs a condition rule that yields
   `Progressing` for a rollout to complete.
10. Observability (`MultiClusterObservability` named `observability`) is installed; query goes through the
    `rbac-query-proxy` Service/Route or `observability-thanos-query` Service in
    `open-cluster-management-observability`. No Hive `ClusterDeployment` and no HyperShift `HostedCluster`
    exist, though both CRDs are installed.

## 1. ManagedCluster (`cluster.open-cluster-management.io/v1`, cluster-scoped, short `mcl`)

### Fields that matter

| Path | Type | Who sets it | Meaning |
|---|---|---|---|
| `metadata.name` | string | importer / agent | Cluster name. Also the hub namespace that holds the cluster's ManifestWorks, add-ons and import secret (`explain:managedcluster.spec.hubAcceptsClient`). |
| `metadata.labels.name` | string | ACM import controller | Always equals `metadata.name` (`mcic:pkg/controller/managedcluster/managedcluster_controller.go`, `clusterNameLabel = "name"`). |
| `metadata.labels.cloud` | string | ACM foundation autodetect | `Amazon`, `Azure`, `Google`, `IBM`, `VSphere`, `OpenStack`, `AlibabaCloud`, `BareMetal`, `Nutanix`, `RHV`, `IBMZPlatform`, `IBMPowerPlatform`, `Other`. Only overwritten while the label is `auto-detect`, so a removed `cloud` or `vendor` label is not re-added (`cla:clusterinfo/v1beta1/types.go`, `found:pkg/controllers/clusterinfo/autodetect_controller.go` L68-81). |
| `metadata.labels.vendor` | string | ACM foundation autodetect | `OpenShift`, `OpenShiftDedicated` (rewritten to `OpenShift` + `managed-by=platform`), `AKS`, `EKS`, `GKE`, `IKS`, `ICP`, `MicroShift`, `Other`. |
| `metadata.labels.clusterID` | string | ACM foundation autodetect | OpenShift `ClusterVersion` id (UUID), from `ManagedClusterInfo.status.clusterID`; same value as claim `id.k8s.io` / `id.openshift.io`. Rewritten whenever it differs, so re-added after removal (`autodetect_controller.go` L83-86). |
| `metadata.labels.openshiftVersion`, `openshiftVersion-major`, `openshiftVersion-major-minor` | string | ACM foundation autodetect | From claim `version.openshift.io`, e.g. `4.22.15`, `4`, `4.22`; rewritten from the claim on every reconcile (`autodetect_controller.go` L95-121), so only joined OpenShift clusters carry them. |
| `metadata.labels.local-cluster` | `"true"` | ACM | Only on the hub's own ManagedCluster (`mcic:pkg/constants/constants.go`, `SelfManagedLabel`). |
| `metadata.labels.cluster.open-cluster-management.io/clusterset` | string | ACM / user | Exclusive set membership; OCM's ManagedCluster mutating webhook sets `default` when the label is empty (`ocm:pkg/registration/webhook/v1/managedcluster_mutating.go` L50-63, `defaultClusterSetName` L25; `api:cluster/v1beta2/types_managedclusterset.go`, `ClusterSetLabel`). `hub:` present with value `default`. |
| `metadata.labels.feature.open-cluster-management.io/addon-<addon>` | string | registration hub (addon feature-discovery controller, `ocm:pkg/registration/hub/addon/discovery_controller.go`) | `available` (Available=True), `unhealthy` (Available=False) or `unreachable` (Available Unknown **or condition missing**); mirrors each `ManagedClusterAddOn`'s `Available` condition. `hub:` ten such labels on 2026-10-16, all `available` (`addon-application-manager`, `addon-cert-policy-controller`, `addon-cluster-proxy`, `addon-config-policy-controller`, `addon-governance-policy-framework`, `addon-hypershift-addon`, `addon-managed-serviceaccount`, `addon-multicluster-observability-addon`, `addon-submariner`, `addon-work-manager`; `addon-submariner` was added after 2026-10-15). |
| `metadata.labels.velero.io/exclude-from-backup` | `"true"` | ACM | On `local-cluster` only (`hub:`). |
| `metadata.annotations.open-cluster-management/created-via` | string | ACM import controller | `other` (default), `discovery`, `hive`, `assisted-installer`, `hypershift` (`mcic:pkg/constants/constants.go`, `mcic:.../managedcluster_controller.go`). |
| `metadata.annotations.installer.multicluster.openshift.io/release-version` | string | MCH | ACM version, e.g. `2.17.3` (`hub:`). |
| `metadata.annotations.open-cluster-management/tolerations` | JSON string | ACM | Klusterlet pod tolerations (`hub:`). |
| `metadata.annotations.import.open-cluster-management.io/klusterlet-deploy-mode` | string | ACM | `Default`, `Singleton` (what an unset annotation means), `Hosted` or `SingletonHosted`, compared case-insensitively (`mcic:pkg/helpers/helpers.go` `DetermineKlusterletMode` L882-900; annotation key in `mcic:pkg/constants/constants.go` L114). |
| `metadata.annotations.import.open-cluster-management.io/hosting-cluster-name` | string | ACM | Hosting cluster for hosted-mode klusterlet. |
| `metadata.finalizers` | []string | several | `hub:` six finalizers: `managedcluster-import-controller.open-cluster-management.io/cleanup`, `managedclusterinfo.finalizers.open-cluster-management.io`, `cluster.open-cluster-management.io/resource-cleanup`, `open-cluster-management.io/managedclusterrole`, `managedcluster-import-controller.open-cluster-management.io/manifestwork-cleanup`, `cluster.open-cluster-management.io/api-resource-cleanup`. Deletion takes seconds to minutes while they drain. |
| `spec.hubAcceptsClient` | bool | hub admin (RBAC `update` on `managedclusters/accept`) | Default `false`. `true` creates the cluster namespace + RBAC; `false` denies/removes (`explain:managedcluster.spec.hubAcceptsClient`). ACM's import flow sets it `true` immediately (not verified in source). |
| `spec.leaseDurationSeconds` | int | hub/ACM | Agent lease renewal period; `0` means 60 s (`explain:managedcluster.spec.leaseDurationSeconds`). `hub:` 60. Hub marks Available `Unknown` after 5x this (`ocm:pkg/registration/hub/lease/controller.go`, `leaseDurationTimes = 5`, i.e. 300 s). |
| `spec.managedClusterClientConfigs[]` | `{url, caBundle}` | agent (and importer) | Managed cluster API server URL and base64 CA bundle. `url` required. Shown by ACM as "MANAGED CLUSTER URLS". |
| `spec.taints[]` | `{key, value, effect, timeAdded}` | hub taint controller / user | `effect` enum `NoSelect`, `PreferNoSelect`, `NoSelectIfNew`. Hub adds `cluster.open-cluster-management.io/unavailable` (Available=False) or `cluster.open-cluster-management.io/unreachable` (Available missing/Unknown), both `effect: NoSelect`, and removes them when Available=True (`ocm:pkg/registration/hub/taint/controller.go`). `timeAdded` is stamped by the ManagedCluster mutating webhook (`ocm:pkg/registration/webhook/v1/managedcluster_mutating.go`, hub webhook `managedclustermutators.admission.cluster.open-cluster-management.io`) so `tolerationSeconds` can count from it. |
| `status.conditions[]` | metav1.Condition | see below | `type`, `status` (`True|False|Unknown`), `reason`, `message`, `lastTransitionTime`, `observedGeneration`. |
| `status.capacity` | map[string]Quantity | agent (+ ACM) | Sum of `node.status.capacity` over all nodes incl. unschedulable (`ocm:pkg/registration/spoke/managedcluster/resource_reconcile.go`). ACM adds `core_worker` and `socket_worker` (`hub:`; `found:pkg/controllers/clusterinfo/capacity_controller.go`, `resourceCoreWorker`/`resourceSocketWorker`, verified). Keys seen: `cpu`, `memory`, `pods`, `ephemeral-storage`, `hugepages-1Gi`, `hugepages-2Mi`, `core_worker`, `socket_worker`. |
| `status.allocatable` | map[string]Quantity | agent | Sum of `node.status.allocatable` over schedulable nodes only. GPU clusters additionally show `nvidia.com/gpu` when the device plugin advertises it (not on the dev hub). |
| `status.version.kubernetes` | string | agent | e.g. `v1.35.6`. |
| `status.clusterClaims[]` | `{name, value}` | agent, from `ClusterClaim` objects on the spoke | The spoke agent writes the reserved claims first, each group sorted by name, and caps custom claims at 20 (`--max-custom-cluster-claims`; `ocm:pkg/registration/spoke/managedcluster/claim_reconcile.go` L90-123). A claim is reserved when its name is in `ReservedClusterClaimNames` or ends with a reserved suffix, and ACM's import controller configures the suffixes `openshift.io` and `open-cluster-management.io` for every klusterlet (`mcic:pkg/bootstrap/render.go` L57-60, L454-457). So on an ACM-imported cluster every claim ACM and OpenShift create is reserved, and the hub (no custom claims) shows one name-sorted list (`hub:` `above.threshold...hypershift.openshift.io` comes before `id.k8s.io`); a custom claim would follow the reserved ones. A mock should simply sort by name. Only synced once `ManagedClusterJoined=True`. |
| `status.managedNamespaces[]` | list | agent | Namespaces managed via clusterset `managedNamespaces` (new; empty on the hub). |

#### Standard claim names and example values

Reserved by OCM (`api:cluster/v1alpha1/types.go`, `ReservedClusterClaimNames`): `id.k8s.io`,
`kubeversion.open-cluster-management.io`, `platform.open-cluster-management.io`,
`product.open-cluster-management.io` (verified), `hosting-cluster.open-cluster-management.io` (not verified).
Created by ACM's klusterlet (`found:pkg/klusterlet/clusterclaim/clusterclaimer.go`):

| Claim | Example value | Allowed values / note |
|---|---|---|
| `id.k8s.io` | `3f1c9c3e-8a4b-4e1d-9f0a-2b7d5e6c1a90` | OpenShift ClusterVersion id |
| `id.openshift.io` | same UUID | |
| `platform.open-cluster-management.io` | `AWS` | `AWS`, `GCP`, `Azure`, `IBM`, `IBMPowerPlatform`, `IBMZPlatform`, `OpenStack`, `VSphere`, `RHV`, `AlibabaCloud`, `BareMetal`, `KubeVirt`, `Nutanix`, `Other` |
| `product.open-cluster-management.io` | `OpenShift` | `OpenShift`, `OpenShiftDedicated`, `ROSA`, `ARO`, `ROKS`, `MicroShift`, `AKS`, `EKS`, `GKE`, `IKS`, `ICP`, `Other` |
| `region.open-cluster-management.io` | `eu-west-3` | from `Infrastructure` on AWS and GCP, else the nodes' topology region labels joined by commas; **omitted** when no region is known, never `Other` (`found:pkg/klusterlet/clusterclaim/clusterclaimer.go` L250-257, L435-487) |
| `version.openshift.io` | `4.22.15` | |
| `kubeversion.open-cluster-management.io` | `v1.35.6` | |
| `consoleurl.cluster.open-cluster-management.io` | `https://console-openshift-console.apps.site-paris-1.example.com` | |
| `apiserverurl.openshift.io` | `https://api.site-paris-1.example.com:6443` | |
| `oauthredirecturis.openshift.io` | `https://oauth.site-paris-1.example.com:6443/oauth/token/implicit` | |
| `infrastructure.openshift.io` | `{"infraName":"site-paris-1-k7x2m"}` | JSON string |
| `controlplanetopology.openshift.io` | `HighlyAvailable` | `HighlyAvailable`, `SingleReplica`, `External` (ROSA HCP) |
| `schedulable.open-cluster-management.io` | `"true"` | from `found:pkg/klusterlet/clusterclaim/schedulable.go` |
| `hostedcluster.hypershift.openshift.io`, `hostingcluster.hypershift.openshift.io`, `zero.hostedclustercount.hypershift.openshift.io`, `above.threshold.hostedclustercount.hypershift.openshift.io`, `full.hostedclustercount.hypershift.openshift.io` | `"true"` / `"false"` | added by the hypershift add-on (`hub:`) |
| `odfinfo.odf.openshift.io` | `openshift-storage/odf-info` | added by ODF when installed (`hub:`) |

### Conditions and who writes them

| Type | Status / Reason / Message | Writer |
|---|---|---|
| `HubAcceptedManagedCluster` | `True` / `HubClusterAdminAccepted` / "Accepted by hub cluster admin"; `False` / `HubClusterAdminDenied` / "Denied by hub cluster admin" when `hubAcceptsClient` flips back to false; `False` / `Error` / apply error | hub registration (`ocm:pkg/registration/hub/managedcluster/controller.go`) |
| `ManagedClusterJoined` | `True` / `ManagedClusterJoined` / "Managed cluster joined". Set once the agent holds a signed client cert and HubAccepted is true; never set to False | spoke agent (`ocm:pkg/registration/spoke/managedcluster/joining_reconcile.go`) |
| `ManagedClusterConditionAvailable` | `True` / `ManagedClusterAvailable` / "Managed cluster is available" (agent, `/livez` 200, falling back to `/healthz` when livez is 404/403); `False` / `ManagedClusterKubeAPIServerUnavailable` / "The kube-apiserver is not ok, status code: %d, %v" (agent); `Unknown` / `ManagedClusterLeaseUpdateStopped` / "Registration agent stopped updating its lease." (hub, after 5 x leaseDurationSeconds) | spoke (`ocm:pkg/registration/spoke/managedcluster/available_reconcile.go`), hub (`ocm:pkg/registration/hub/lease/controller.go`) |
| `ManagedClusterConditionClockSynced` | `True` / `ManagedClusterClockSynced` / "The clock of the managed cluster is synced with the hub."; `False` / `ManagedClusterClockOutOfSync` / "The clock of hub and agent is out of sync. This may cause the Unknown status and affect agent functionalities." Threshold is one lease window (5 x leaseDurationSeconds). Annotation `registration.open-cluster-management.io/skip-clock-sync-check: "true"` forces True | hub (`ocm:pkg/registration/hub/lease/clocksynccontroller.go`) |
| `ManagedClusterImportSucceeded` | `False` / `ManagedClusterWaitForImporting` / "Wait for importing"; `False` / `ManagedClusterImporting` / "Start to import managed cluster", "Wait for import secret", "Importing resources are applied, wait for resources be available", "Expect klusterlet manifestworks created, but got none. Will retry", "Try to import managed cluster, apply resources error: %v. Will Retry" (an internal server error while applying, which does not count as a failed attempt; `mcic:pkg/helpers/autoimport.go` L221-227); `False` / `ManagedClusterImportFailed` / "Try to import managed cluster, error: %v" (`autoimport.go` L209-213), "Get import secret failed: %v. Will retry" (L200-203), "AutoImportSecretInvalid <ns>/<secret>; ..."; the `WaitForImporting` row is written by the import-status controller when the condition is missing (`mcic:pkg/controller/importstatus/importstatus_controller.go` L85-97); `True` / `ManagedClusterImported` / "Import succeeded" (set once ManifestWork `<cluster>-klusterlet` is Available); `False` / `ManagedClusterDetaching` / "The managed cluster is being detached now" and `False` / `ManagedClusterForceDetaching` / "The managed cluster is being detached by force" during deletion (`mcic:pkg/controller/resourcecleanup/resourcecleanup_controller.go`). Hosted mode also emits "Wait for import secret to be created" | ACM import controller (`mcic:pkg/constants/constants.go`, `mcic:pkg/helpers/autoimport.go`, `mcic:pkg/controller/importstatus/importstatus_controller.go`) |
| `HubDeniedManagedCluster` | defined in API, rarely seen | `api:cluster/v1/types.go` |
| `Deleting` | `False` / `ResourceRemaining` / "The resource %v is remaning, the remaining count is %v, the finalizer pending count is %v" (upstream spelling) while the hub's resource-cleanup finalizer drains add-ons/ManifestWorks; `True` / `NoResource` / "No cleaned resource in cluster ns." once nothing is left (`ocm:pkg/registration/hub/gc/gc_resources.go` L65-73, L94-100). `DeletingError` is defined in `api:cluster/v1/types.go` but not written by the gc controller | hub gc controller |
| `Upgradeable` (on `ManagedClusterInfo`, not on ManagedCluster) | e.g. `False` / `IncompatibleOperatorsInstalled` | ACM foundation; `hub:` shows ACM copies all ManagedCluster conditions into `ManagedClusterInfo.status.conditions` and adds `ManagedClusterInfoSynced` and `Upgradeable` |

`oc get managedcluster` columns (`hub:`): `HUB ACCEPTED` = `spec.hubAcceptsClient`; `MANAGED CLUSTER URLS` =
`spec.managedClusterClientConfigs[*].url`; `JOINED` = `ManagedClusterJoined` status; `AVAILABLE` =
`ManagedClusterConditionAvailable` status (blank when the condition is absent).

### Lifecycle (ACM console mapping, `console:frontend/src/resources/utils/get-cluster.ts`)

Order of checks, ManagedCluster only (no Hive/HyperShift object). The console's `checkForCondition`
(`console:frontend/src/resources/utils/status-conditions.ts`) tests `status === 'True'`, so "not True"
below means absent **or** False:

1. `metadata.deletionTimestamp` set -> `detaching`
2. no conditions -> `failed`
3. `HubAcceptedManagedCluster` not `True` (absent, or `False` for a Denied cluster) -> `notaccepted`
4. `ManagedClusterJoined` not `True` -> `pendingimport`; refined to `importing` if `ManagedClusterImportSucceeded=False/ManagedClusterImporting`, to `importfailed` if reason `ManagedClusterImportFailed`; `needsapproval` if a CSR for the cluster is pending
5. joined: `Available=True` -> `ready`; `Available=False` -> `offline`; `Unknown` or missing -> `unknown`

| State | `hubAcceptsClient` | Conditions present | Taints | Labels of note |
|---|---|---|---|---|
| Created by ACM, agent not yet running (console `pendingimport`, then `importing`) | true | `HubAcceptedManagedCluster=True`, `ManagedClusterImportSucceeded=False/ManagedClusterWaitForImporting` "Wait for importing" (console `pendingimport`); once the import controller applies the klusterlet, `False/ManagedClusterImporting` (console `importing`; `console:.../get-cluster.ts` L1486-1499) | none yet (taint controller adds `unreachable` once it sees no Available condition) | `name`, `cloud=auto-detect`, `vendor=auto-detect`, `clusterset=default`; no `clusterID`/`openshiftVersion` yet; no `feature.../addon-*` yet |
| Agent registered, cert signed (console `ready`: the console reads `ImportSucceeded` only while not joined, `get-cluster.ts` L1540-1543) | true | + `ManagedClusterJoined=True`, `ManagedClusterConditionAvailable=True`, `ManagedClusterConditionClockSynced=True`; `ImportSucceeded` still False until `<cluster>-klusterlet` ManifestWork is Available | `unreachable` removed | status.capacity/allocatable/version/clusterClaims appear; autodetect fills `cloud`, `vendor`, `clusterID`, `openshiftVersion*` |
| Ready | true | all five True; `ImportSucceeded=True/ManagedClusterImported` | none | `feature.../addon-*=available` as add-ons come up (30-120 s each) |
| Offline (API server down, agent alive) | true | `Available=False/ManagedClusterKubeAPIServerUnavailable` | `cluster.open-cluster-management.io/unavailable` NoSelect | addon labels flip to `unhealthy`/`unreachable` |
| Unknown (lease expired: cluster off, network cut) | true | `Available=Unknown/ManagedClusterLeaseUpdateStopped` after ~300 s; other conditions keep last values; `ClockSynced` may go False | `cluster.open-cluster-management.io/unreachable` NoSelect | addon labels `unreachable` |
| Denied | false | `HubAcceptedManagedCluster=False/HubClusterAdminDenied`; namespace and RBAC removed | | |
| Detaching | any | `deletionTimestamp` set, finalizers draining; `ImportSucceeded=False/ManagedClusterDetaching` ("The managed cluster is being detached now"); `Deleting=False/ResourceRemaining` while add-ons or ManifestWorks remain, `Deleting=True/NoResource` once none are left | | object disappears after ManifestWorks and add-ons are gone (tens of seconds to minutes) |

Timing observed on the hub for the local cluster: `HubAccepted` at t0+2 s, `Joined`/`Available`/`ClockSynced`
at t0+8 s, `ImportSucceeded=True` at t0+12 s (`hub:` lastTransitionTime deltas). Real remote imports take
1-5 minutes between Accepted and Joined because the klusterlet must be deployed first.

### Example: Ready cluster (fictional)

```yaml
apiVersion: cluster.open-cluster-management.io/v1
kind: ManagedCluster
metadata:
  name: site-paris-1
  annotations:
    installer.multicluster.openshift.io/release-version: 2.17.3
    open-cluster-management/created-via: other
    ai-grid/site-name: Paris DC1          # console convention, user-set
    ai-grid/latitude: "48.8566"           # console convention, user-set
    ai-grid/longitude: "2.3522"           # console convention, user-set
  labels:
    name: site-paris-1
    cloud: BareMetal
    vendor: OpenShift
    clusterID: 3f1c9c3e-8a4b-4e1d-9f0a-2b7d5e6c1a90
    openshiftVersion: 4.22.15
    openshiftVersion-major: "4"
    openshiftVersion-major-minor: "4.22"
    cluster.open-cluster-management.io/clusterset: default
    feature.open-cluster-management.io/addon-application-manager: available
    feature.open-cluster-management.io/addon-cert-policy-controller: available
    feature.open-cluster-management.io/addon-cluster-proxy: available
    feature.open-cluster-management.io/addon-config-policy-controller: available
    feature.open-cluster-management.io/addon-governance-policy-framework: available
    feature.open-cluster-management.io/addon-managed-serviceaccount: available
    feature.open-cluster-management.io/addon-multicluster-observability-addon: available
    feature.open-cluster-management.io/addon-work-manager: available
    ai-grid/enabled: "true"               # console convention
    ai-grid/provisioned: "true"           # console convention
    location: paris                       # console convention
    ai-grid/site: paris-dc1               # console convention
    ai-grid/region: europe-west           # console convention
  finalizers:
  - managedcluster-import-controller.open-cluster-management.io/cleanup
  - managedclusterinfo.finalizers.open-cluster-management.io
  - cluster.open-cluster-management.io/resource-cleanup
  - open-cluster-management.io/managedclusterrole
  - managedcluster-import-controller.open-cluster-management.io/manifestwork-cleanup
  - cluster.open-cluster-management.io/api-resource-cleanup
  uid: 7d3e2b1a-5c6f-4a8b-9e0d-1f2a3b4c5d6e
spec:
  hubAcceptsClient: true
  leaseDurationSeconds: 60
  managedClusterClientConfigs:
  - url: https://api.site-paris-1.example.com:6443
    caBundle: LS0tLS1CRUdJTi...   # base64 PEM
status:
  version:
    kubernetes: v1.35.6
  capacity:
    cpu: "192"
    memory: 1572864Mi
    pods: "1000"
    ephemeral-storage: 3600Gi
    hugepages-1Gi: "0"
    hugepages-2Mi: "0"
    nvidia.com/gpu: "16"
    core_worker: "128"
    socket_worker: "8"
  allocatable:
    cpu: "186"
    memory: 1540000Mi
    pods: "1000"
    ephemeral-storage: 3300Gi
    hugepages-1Gi: "0"
    hugepages-2Mi: "0"
    nvidia.com/gpu: "16"
  clusterClaims:
  - {name: apiserverurl.openshift.io, value: "https://api.site-paris-1.example.com:6443"}
  - {name: consoleurl.cluster.open-cluster-management.io, value: "https://console-openshift-console.apps.site-paris-1.example.com"}
  - {name: controlplanetopology.openshift.io, value: HighlyAvailable}
  - {name: id.k8s.io, value: 3f1c9c3e-8a4b-4e1d-9f0a-2b7d5e6c1a90}
  - {name: id.openshift.io, value: 3f1c9c3e-8a4b-4e1d-9f0a-2b7d5e6c1a90}
  - {name: infrastructure.openshift.io, value: '{"infraName":"site-paris-1-k7x2m"}'}
  - {name: kubeversion.open-cluster-management.io, value: v1.35.6}
  - {name: oauthredirecturis.openshift.io, value: "https://oauth.site-paris-1.example.com:6443/oauth/token/implicit"}
  - {name: platform.open-cluster-management.io, value: BareMetal}
  - {name: product.open-cluster-management.io, value: OpenShift}
  - {name: schedulable.open-cluster-management.io, value: "true"}
  - {name: version.openshift.io, value: 4.22.15}
  conditions:
  - type: ManagedClusterImportSucceeded
    status: "True"
    reason: ManagedClusterImported
    message: Import succeeded
    lastTransitionTime: "2026-10-01T09:00:12Z"
  - type: HubAcceptedManagedCluster
    status: "True"
    reason: HubClusterAdminAccepted
    message: Accepted by hub cluster admin
    lastTransitionTime: "2026-10-01T09:00:02Z"
  - type: ManagedClusterJoined
    status: "True"
    reason: ManagedClusterJoined
    message: Managed cluster joined
    lastTransitionTime: "2026-10-01T09:00:08Z"
  - type: ManagedClusterConditionAvailable
    status: "True"
    reason: ManagedClusterAvailable
    message: Managed cluster is available
    lastTransitionTime: "2026-10-01T09:00:08Z"
  - type: ManagedClusterConditionClockSynced
    status: "True"
    reason: ManagedClusterClockSynced
    message: The clock of the managed cluster is synced with the hub.
    lastTransitionTime: "2026-10-01T09:00:08Z"
```

### Example: just created by ACM, agent not yet joined ("Pending import" in the ACM console)

```yaml
apiVersion: cluster.open-cluster-management.io/v1
kind: ManagedCluster
metadata:
  name: site-berlin-1
  annotations:
    open-cluster-management/created-via: other
  labels:
    name: site-berlin-1
    cloud: auto-detect
    vendor: auto-detect
    cluster.open-cluster-management.io/clusterset: default
  finalizers:
  - managedcluster-import-controller.open-cluster-management.io/cleanup
  - cluster.open-cluster-management.io/api-resource-cleanup
spec:
  hubAcceptsClient: true
  leaseDurationSeconds: 60
  taints:
  - key: cluster.open-cluster-management.io/unreachable
    effect: NoSelect
    timeAdded: "2026-10-15T10:00:03Z"
status:
  conditions:
  - type: HubAcceptedManagedCluster
    status: "True"
    reason: HubClusterAdminAccepted
    message: Accepted by hub cluster admin
    lastTransitionTime: "2026-10-15T10:00:01Z"
  - type: ManagedClusterImportSucceeded
    status: "False"
    reason: ManagedClusterWaitForImporting
    message: Wait for importing
    lastTransitionTime: "2026-10-15T10:00:05Z"
```

As soon as the import controller has applied the klusterlet the reason becomes `ManagedClusterImporting`
("Importing resources are applied, wait for resources be available") and the ACM console shows the cluster
as `importing`, not `pendingimport` (`console:frontend/src/resources/utils/get-cluster.ts` L1486-1499).

### Example: joined earlier, lease expired ("Unknown")

```yaml
apiVersion: cluster.open-cluster-management.io/v1
kind: ManagedCluster
metadata:
  name: site-madrid-1
  labels:
    name: site-madrid-1
    cloud: Amazon
    vendor: OpenShift
    clusterID: 9b2d4f60-1e3a-4c7b-8d9e-0a1b2c3d4e5f
    openshiftVersion: 4.21.9
    openshiftVersion-major: "4"
    openshiftVersion-major-minor: "4.21"
    cluster.open-cluster-management.io/clusterset: default
    feature.open-cluster-management.io/addon-work-manager: unreachable
    feature.open-cluster-management.io/addon-application-manager: unreachable
spec:
  hubAcceptsClient: true
  leaseDurationSeconds: 60
  managedClusterClientConfigs:
  - url: https://api.site-madrid-1.example.com:6443
  taints:
  - key: cluster.open-cluster-management.io/unreachable
    effect: NoSelect
    timeAdded: "2026-10-15T11:05:00Z"
status:
  version: {kubernetes: v1.34.3}
  capacity: {cpu: "48", memory: 196608Mi, pods: "750"}
  allocatable: {cpu: "45", memory: 190000Mi, pods: "750"}
  clusterClaims:
  - {name: id.k8s.io, value: 9b2d4f60-1e3a-4c7b-8d9e-0a1b2c3d4e5f}
  - {name: platform.open-cluster-management.io, value: AWS}
  - {name: product.open-cluster-management.io, value: OpenShift}
  - {name: region.open-cluster-management.io, value: eu-south-2}
  - {name: version.openshift.io, value: 4.21.9}
  conditions:
  - {type: ManagedClusterImportSucceeded, status: "True", reason: ManagedClusterImported, message: Import succeeded, lastTransitionTime: "2026-09-20T08:00:12Z"}
  - {type: HubAcceptedManagedCluster, status: "True", reason: HubClusterAdminAccepted, message: Accepted by hub cluster admin, lastTransitionTime: "2026-09-20T08:00:02Z"}
  - {type: ManagedClusterJoined, status: "True", reason: ManagedClusterJoined, message: Managed cluster joined, lastTransitionTime: "2026-09-20T08:00:08Z"}
  - type: ManagedClusterConditionAvailable
    status: Unknown
    reason: ManagedClusterLeaseUpdateStopped
    message: Registration agent stopped updating its lease.
    lastTransitionTime: "2026-10-15T11:05:00Z"
  - {type: ManagedClusterConditionClockSynced, status: "True", reason: ManagedClusterClockSynced, message: The clock of the managed cluster is synced with the hub., lastTransitionTime: "2026-09-20T08:00:08Z"}
```

Offline (API server down but agent alive) differs only in
`Available: {status: "False", reason: ManagedClusterKubeAPIServerUnavailable, message: "The kube-apiserver is not ok, status code: 503, ..."}`
and taint key `cluster.open-cluster-management.io/unavailable`.

Detaching: same object with `metadata.deletionTimestamp: "2026-10-15T12:00:00Z"`, finalizers still
present, `ManagedClusterImportSucceeded: {status: "False", reason: ManagedClusterDetaching, message: "The managed cluster is being detached now"}`,
and `Deleting: {status: "False", reason: ResourceRemaining, message: "The resource manifestworks is remaning, the remaining count is 2, the finalizer pending count is 0"}`
until the cluster namespace is empty (`ocm:pkg/registration/hub/gc/gc_resources.go` L65-73).

### Companion object: `ManagedClusterInfo` (`internal.open-cluster-management.io/v1beta1`, namespaced, name = cluster)

ACM-only. `status.consoleURL`, `status.distributionInfo.ocp.{version, desiredVersion, channel, availableUpdates[], upgradeFailed}`,
`status.nodeList[]{name, labels, capacity, conditions}`, `status.cloudVendor`, `status.kubeVendor`,
`status.clusterID`, plus every ManagedCluster condition copied and `ManagedClusterInfoSynced`, `Upgradeable`
(`cla:clusterinfo/v1beta1/types.go`, `hub:`). Useful if the console wants per-node GPU counts or available
upgrades; otherwise the claims on ManagedCluster are enough.

## 2. ManagedClusterSet and ManagedClusterSetBinding (`cluster.open-cluster-management.io/v1beta2`)

### ManagedClusterSet (cluster-scoped, short `mclset`)

| Path | Type | Who | Meaning |
|---|---|---|---|
| `spec.clusterSelector.selectorType` | enum `ExclusiveClusterSetLabel` \| `LabelSelector` | user/ACM | Exclusive: members are clusters with label `cluster.open-cluster-management.io/clusterset=<set name>`. LabelSelector: any labelSelector; `{}` matches every cluster (`explain:managedclusterset.spec.clusterSelector`, `sdk:pkg/apis/cluster/v1beta2/managedclusterset.go`). |
| `spec.clusterSelector.labelSelector` | metav1.LabelSelector | user | only for `LabelSelector` type |
| `spec.managedNamespaces[].name` | string | user | namespaces to manage across the set (new field) |
| `status.conditions[]` `ClusterSetEmpty` | `True`/`NoClusterMatched`/"No ManagedCluster selected" or `False`/`ClustersSelected`/"<n> ManagedClusters selected" | hub (`ocm:pkg/registration/hub/managedclusterset/controller.go`) | the only condition |

Built-in sets on the hub (`hub:`):

```yaml
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSet
metadata:
  name: default
  annotations:
    cluster.open-cluster-management.io/submariner-broker-ns: default-broker
  finalizers:
  - cluster.open-cluster-management.io/managedclusterset-clusterrole
  - cluster.open-cluster-management.io/submariner-cleanup
spec:
  clusterSelector:
    selectorType: ExclusiveClusterSetLabel
status:
  conditions:
  - type: ClusterSetEmpty
    status: "False"
    reason: ClustersSelected
    message: 3 ManagedClusters selected
---
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSet
metadata:
  name: global
  annotations:
    open-cluster-management.io/ns-create: "true"
  finalizers:
  - cluster.open-cluster-management.io/managedclusterset-clusterrole
spec:
  clusterSelector:
    labelSelector: {}
    selectorType: LabelSelector
status:
  conditions:
  - type: ClusterSetEmpty
    status: "False"
    reason: ClustersSelected
    message: 3 ManagedClusters selected
```

`default` is recreated every 10 s if deleted and its selector is forced back to
`ExclusiveClusterSetLabel` unless annotated `cluster.open-cluster-management.io/autoupdate: "false"`
(`ocm:pkg/registration/hub/managedclusterset/default_managedclusterset_controller.go`). `global` has a
namespace `open-cluster-management-global-set` created for it with a binding and a `Placement` named `global`
inside (`hub:`). A cluster is in at most one exclusive set (one label value) but always in `global`.

### ManagedClusterSetBinding (namespaced, short `mclsetbinding`)

| Path | Type | Who | Meaning |
|---|---|---|---|
| `metadata.name` | string | user | must equal `spec.clusterSet` (`api:cluster/v1beta2/types_managedclustersetbinding.go`) |
| `metadata.namespace` | string | user | the namespace that gains the right to place onto the set |
| `spec.clusterSet` | string | user | set name; immutable; needs RBAC `create` on `managedclustersets/bind` |
| `status.conditions[]` `Bound` | `True`/`ClusterSetBound` or `False`/`ClusterSetNotFound`, message `""` | hub (`ocm:pkg/registration/hub/managedclustersetbinding/controller.go`) | |

```yaml
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: global
  namespace: ai-grid-models
spec:
  clusterSet: global
status:
  conditions:
  - type: Bound
    status: "True"
    reason: ClusterSetBound
    message: ""
    lastTransitionTime: "2026-10-15T09:00:00Z"
```

The hub already has bindings of `global` into `open-cluster-management-global-set` and into a GitOps
namespace created by the POC (`hub:`). The console's model namespace needs its own binding (work-breakdown
item 5), to `global` or to `default`.

## 3. Placement and PlacementDecision (`cluster.open-cluster-management.io/v1beta1`, namespaced)

### Placement spec

| Path | Type | Meaning |
|---|---|---|
| `spec.clusterSets[]` | []string | sets to draw from; must be bound in the namespace. Empty = all bound sets (`explain:placement.spec`). |
| `spec.numberOfClusters` | int | cap on selected clusters; unset = all matching (`api:cluster/v1beta1/types_placement.go`). |
| `spec.predicates[].requiredClusterSelector.labelSelector` | metav1.LabelSelector | on ManagedCluster labels; predicates are OR-ed, selectors inside AND-ed (`explain:placement.spec.predicates.requiredClusterSelector`). |
| `spec.predicates[].requiredClusterSelector.claimSelector.matchExpressions[]` | `{key, operator, values}` | on `status.clusterClaims` (e.g. `platform.open-cluster-management.io In [AWS]`). |
| `spec.predicates[].requiredClusterSelector.celSelector.celExpressions[]` | []string | CEL over `managedCluster` and scores. Present in the API from `open-cluster-management-io/api` v1.0.0; absent at v0.15.0, v0.16.0 and v0.16.1 (checked per tag with `gh api .../contents/cluster/v1beta1/types_placement.go?ref=<tag>`). |
| `spec.prioritizerPolicy.mode` | `Additive` (default) \| `Exact` | Additive keeps `Steady` and `Balance` at weight 1 (`explain:placement.spec.prioritizerPolicy`). |
| `spec.prioritizerPolicy.configurations[].scoreCoordinate.{type: BuiltIn\|AddOn, builtIn, addOn{resourceName, scoreName}}`, `weight` (-10..10) | | BuiltIn names: `Balance`, `Steady`, `ResourceAllocatableCPU`, `ResourceAllocatableMemory` (`explain:`), `Spread` (not verified on the hub). |
| `spec.tolerations[]` | `{key, operator Equal\|Exists, value, effect, tolerationSeconds}` | tolerate `cluster.open-cluster-management.io/unreachable` or `/unavailable` so decisions survive outages; `tolerationSeconds` counts from taint `timeAdded`. ACM's own placements tolerate both (`hub:`). |
| `spec.spreadPolicy.spreadConstraints[]` | `{topologyKey, topologyKeyType Label\|Claim, maxSkew (default 1), whenUnsatisfiable DoNotSchedule\|ScheduleAnyway}` | spread across e.g. `ai-grid/region` (`explain:placement.spec.spreadPolicy.spreadConstraints`). |
| `spec.decisionStrategy.groupStrategy.clustersPerDecisionGroup` | int or percent string, CRD default `"100%"` (applied only when the caller sends a `groupStrategy` object) | splits selected clusters into groups for progressive rollouts. |
| `spec.decisionStrategy.groupStrategy.decisionGroups[]` | `{groupName, groupClusterSelector{labelSelector, claimSelector}}` | named canary groups. |
| `spec.scoreRateLimit` | duration string | CRD default `1m` (`hub:` on every Placement). Defined in the API only (`api:cluster/v1beta1/types_placement.go` L113-119); the placement controller does not read it. |
| `spec.sortBy` | `ClusterName` \| `Score` | CRD default `ClusterName` (`hub:` on every Placement). Defined in the API only (`types_placement.go` L104-111); the controller always sorts decisions by cluster name (`scheduling_controller.go` L748-753). |

Defaults actually applied on create: there is **no Placement mutating webhook** on the hub. The hub has two
mutating webhooks from this stack: OCM's `managedclustermutators.admission.cluster.open-cluster-management.io`
(`managedclusters` only; `ocm:manifests/cluster-manager/hub/registration/webhook-mutatingconfiguration.yaml`)
and ACM's `ocm-mutating-webhook` (`deployables`, `channels`, `subscriptions`, `placementrules`,
`applications`, `clusterdeployments`, `clusterpools`, `clusterclaims`; `hub:` 2026-10-16); neither covers
`placements`. The API server applies CRD defaults
only: `scoreRateLimit: 1m` and `sortBy: ClusterName` always; `prioritizerPolicy.mode: Additive`,
`clustersPerDecisionGroup: "100%"`, `spreadConstraints[].maxSkew: 1`, `whenUnsatisfiable: ScheduleAnyway`
and `tolerations[].operator: Equal` only when the caller sent the parent object. Observed (`hub:`): the
user-created Placement in the GitOps namespace carries only `scoreRateLimit` and `sortBy`; ACM's own three
Placements additionally show `clustersPerDecisionGroup: 0`, `prioritizerPolicy.mode: Additive`,
`spreadPolicy: {}`, `celSelector: {}` / `claimSelector: {}` because they were written by a Go typed client
(`PrioritizerPolicy` has no `omitempty`, struct fields serialise as `{}`, `IntOrString` as `0`, which
blocks the `"100%"` default; `api:cluster/v1beta1/types_placement.go`). A mock must **not** echo those
artefacts; after create it should add only `scoreRateLimit: 1m` and `sortBy: ClusterName` to what the caller
sent.

### Placement status

| Path | Meaning |
|---|---|
| `status.numberOfSelectedClusters` | `len(decisions)` after prioritising (`ocm:pkg/placement/controllers/scheduling/scheduling_controller.go`). |
| `status.decisionGroups[]{decisionGroupIndex, decisionGroupName, decisions[] (PlacementDecision names), clusterCount}` | always at least one group, index 0, name `""`. |
| `status.lastScoreUpdateTime` | defined in the API ("the last time that placement scores were updated", `api:cluster/v1beta1/types_placement.go` L474-475) but never written by the placement controller. |
| `status.conditions[]` `PlacementSatisfied` | `True`/`AllDecisionsScheduled`/"All cluster decisions scheduled"; `False`/`NotAllDecisionsScheduled`/"<n> cluster decisions unscheduled" (numberOfClusters > matches); `False`/`NoManagedClusterSetBindings`/"No valid ManagedClusterSetBindings found in placement namespace"; `False`/`NoIntersection`/"None of ManagedClusterSets [a,b] is bound to placement namespace"; `False`/`AllManagedClusterSetsEmpty`/"All ManagedClusterSets [a] have no member ManagedCluster"; `False`/`NoManagedClusterMatched`/"No ManagedCluster matches any of the cluster predicate". |
| `status.conditions[]` `PlacementMisconfigured` | `False`/`Succeedconfigured`/"Placement configurations check pass" (normal); `True`/`Misconfigured`/"<plugin>:<detail>". |

`oc get placement` columns (`hub:`): `SUCCEEDED` = PlacementSatisfied status, `REASON` = its reason,
`SELECTEDCLUSTERS` = numberOfSelectedClusters.

### PlacementDecision

- Name `<placement>-decision-<n>`, `n` starting at 1, at most 100 clusters per decision
  (`maxNumOfClusterDecisions = 100`, `ocm:.../scheduling_controller.go`).
- Labels: `cluster.open-cluster-management.io/placement=<placement>`,
  `cluster.open-cluster-management.io/decision-group-index="0"`, `cluster.open-cluster-management.io/decision-group-name=""`
  (`api:cluster/v1beta1/types_placementdecision.go`, `hub:`).
- `ownerReferences` -> the Placement (controller=true, blockOwnerDeletion=true); deleted with it.
- `status.decisions[]{clusterName, reason, score}` in the API; the controller builds every decision with
  `ClusterName` only, so `reason` is `""` and `score` (`omitzero`) is always omitted, even with prioritizers:
  scores only rank clusters before the cut to `numberOfClusters` (`ocm:.../scheduling_controller.go` L443,
  L736; `ocm:.../schedule.go` L280-293; `explain:placementdecision.status.decisions`, `hub:`).
- Sorted by `clusterName` ascending whatever `sortBy` says (`divideDecisionGroups`, `scheduling_controller.go` L748-753).
- When nothing matches, one PlacementDecision with `status.decisions: []` is still written.

Consumers (MWRS, GitOps, Policy) list decisions with the `placement` label and union the cluster names
(`sdk:pkg/apis/cluster/v1beta1/placement.go`, `PlacementDecisionClustersTracker`).

### Behaviour when labels change

The scheduling controller watches ManagedClusters, ManagedClusterSets, bindings and
`AddOnPlacementScore`s (`ocm:pkg/placement/controllers/scheduling/cluster_event_handler.go`). Adding
`ai-grid/enabled=true` to a cluster that a Placement's `labelSelector` requires adds the cluster to
`status.decisions` on the next reconcile (sub-second to a few seconds); removing the label, or the cluster
gaining an untolerated `NoSelect` taint (Available != True), removes it. With `numberOfClusters` set the
`Steady` prioritizer keeps existing picks stable. A deleted cluster (terminating) is never selected.
`PlacementSatisfied` flips to `NoManagedClusterMatched` when the last cluster leaves and
`numberOfSelectedClusters` goes to 0; the empty PlacementDecision remains.

### Example: Placement for a model, satisfied

```yaml
apiVersion: cluster.open-cluster-management.io/v1beta1
kind: Placement
metadata:
  name: model-granite-8b
  namespace: ai-grid-models
  labels:
    ai-grid/model: granite-8b
spec:
  clusterSets: [global]
  predicates:
  - requiredClusterSelector:
      labelSelector:
        matchLabels:
          ai-grid/enabled: "true"
        matchExpressions:
        - {key: ai-grid/region, operator: In, values: [europe-west]}
  tolerations:
  - key: cluster.open-cluster-management.io/unreachable
    operator: Exists
    tolerationSeconds: 600
  scoreRateLimit: 1m       # CRD default
  sortBy: ClusterName      # CRD default
status:
  numberOfSelectedClusters: 2
  decisionGroups:
  - decisionGroupIndex: 0
    decisionGroupName: ""
    decisions: [model-granite-8b-decision-1]
    clusterCount: 2
  conditions:
  - type: PlacementMisconfigured
    status: "False"
    reason: Succeedconfigured
    message: Placement configurations check pass
    lastTransitionTime: "2026-10-15T09:10:00Z"
  - type: PlacementSatisfied
    status: "True"
    reason: AllDecisionsScheduled
    message: All cluster decisions scheduled
    lastTransitionTime: "2026-10-15T09:10:00Z"
---
apiVersion: cluster.open-cluster-management.io/v1beta1
kind: PlacementDecision
metadata:
  name: model-granite-8b-decision-1
  namespace: ai-grid-models
  labels:
    cluster.open-cluster-management.io/placement: model-granite-8b
    cluster.open-cluster-management.io/decision-group-index: "0"
    cluster.open-cluster-management.io/decision-group-name: ""
  ownerReferences:
  - apiVersion: cluster.open-cluster-management.io/v1beta1
    kind: Placement
    name: model-granite-8b
    uid: 1a2b3c4d-0001-4000-8000-000000000001
    controller: true
    blockOwnerDeletion: true
status:
  decisions:
  - clusterName: site-berlin-1
    reason: ""
  - clusterName: site-paris-1
    reason: ""
```

### Example: Placement with nothing matching

```yaml
status:
  numberOfSelectedClusters: 0
  decisionGroups:
  - decisionGroupIndex: 0
    decisionGroupName: ""
    decisions: [model-granite-8b-decision-1]
    clusterCount: 0
  conditions:
  - {type: PlacementMisconfigured, status: "False", reason: Succeedconfigured, message: Placement configurations check pass}
  - type: PlacementSatisfied
    status: "False"
    reason: NoManagedClusterMatched
    message: No ManagedCluster matches any of the cluster predicate
# and the decision:
status:
  decisions: []
```

Namespace without a binding: `PlacementSatisfied=False/NoManagedClusterSetBindings`.
`clusterSets: [default]` but only `global` bound: `False/NoIntersection`, message
"None of ManagedClusterSets [default] is bound to placement namespace".

## 4. ManifestWork (`work.open-cluster-management.io/v1`) and ManifestWorkReplicaSet (`v1alpha1`)

### ManifestWork (namespaced in the cluster namespace)

| Path | Type | Meaning |
|---|---|---|
| `spec.workload.manifests[]` | []runtime.RawExtension | full Kubernetes objects to apply on the spoke (`explain:manifestwork.spec`). |
| `spec.manifestConfigs[].resourceIdentifier{group, resource, namespace, name}` | | which manifest the config targets; `resource` is plural lowercase (`deployments`, `inferenceservices`). |
| `spec.manifestConfigs[].feedbackRules[]{type: WellKnownStatus\|JSONPaths, jsonPaths[]{name, path, version}}` | | status fields to copy back to the hub (`explain:manifestwork.spec.manifestConfigs.feedbackRules`). |
| `spec.manifestConfigs[].feedbackScrapeType` | `Poll` \| `Watch` | how the agent scrapes feedback. |
| `spec.manifestConfigs[].conditionRules[]{type: WellKnownConditions\|CEL, condition, celExpressions[], message, messageExpression}` | | derive `Complete`, `Progressing`, `Degraded` or any custom condition per manifest; aggregated to the work. |
| `spec.manifestConfigs[].updateStrategy{type: Update\|CreateOnly\|ServerSideApply\|ReadOnly, serverSideApply{fieldManager, force, ignoreFields[]{condition: OnSpokePresent\|OnSpokeChange, jsonPaths[]}}}` | | default `Update` (`explain:manifestwork.spec.manifestConfigs.updateStrategy`). |
| `spec.deleteOption.propagationPolicy` | `Foreground` (default) \| `Orphan` \| `SelectivelyOrphan` | what happens to spoke resources when the work is deleted. |
| `spec.deleteOption.selectivelyOrphans.orphaningRules[]{group, resource, namespace, name}` | | resources to leave behind (e.g. the model namespace or a shared PVC). Note the plural `selectivelyOrphans`. |
| `spec.deleteOption.ttlSecondsAfterFinished` | int | auto-delete after `Complete`. |
| `spec.executor.subject{type: ServiceAccount, serviceAccount{name, namespace}}` | | impersonation on the spoke; the `ExecutorValidatingCaches` gate (default off, `api:feature/feature.go`) only adds caching of the permission checks. |
| `status.conditions[]` | | `Applied`, `Available`, plus `Progressing`/`Degraded`/`Complete` when rules define them; `Deleting` during deletion (`api:work/v1/types.go`). |
| `status.resourceStatus.manifests[]{resourceMeta{ordinal, group, version, resource, kind, namespace, name}, conditions[], statusFeedback{values[]{name, fieldValue{type: Integer\|String\|Boolean\|JsonRaw, integer, string, boolean, jsonRaw}}}}` | | one entry per manifest, in `ordinal` order (`explain:manifestwork.status`). |

Condition strings (verified on the hub and in source):

| Scope | Type | Status / Reason / Message | Source |
|---|---|---|---|
| work | `Applied` | `True`/`AppliedManifestWorkComplete`/"Apply manifest work complete"; `False`/`AppliedManifestWorkFailed`/"Failed to apply manifest work" | `ocm:pkg/work/spoke/controllers/manifestcontroller/manifestwork_reconciler.go`, `hub:` |
| work | `Available` | `True`/`ResourcesAvailable`/"All resources are available"; `False`/`ResourcesNotAvailable`/"<k> of <n> resources are not available"; `Unknown`/`ResourcesStatusUnknown`/"<k> of <n> resources have unknown status" (or "cannot get any available resource " when the work has no manifests) | `ocm:pkg/work/spoke/conditions/helpers.go`, `hub:` |
| work | `Progressing`, `Degraded`, `Complete` (and any custom) | only present when `conditionRules` exist; reason `ConditionRulesAggregated`. Progressing: any manifest True -> True, else any Unknown -> Unknown, else False. Others: any manifest False -> False; any Unknown or zero True -> Unknown; else True. Message: when exactly one manifest carries the condition, that manifest's message is copied verbatim; otherwise "Aggregated from <n> manifest conditions" | `ocm:pkg/work/spoke/conditions/helpers.go` |
| manifest | `Applied` | `True`/`AppliedManifestComplete`/"Apply manifest complete"; `False`/`AppliedManifestFailed`/"Failed to apply manifest: <err>"; `False`/`AppliedManifestSSAIgnoreFieldError` | same reconciler, `hub:` |
| manifest | `Available` | `True`/`ResourceAvailable`/"Resource is available"; `False`/`ResourceNotAvailable`/"Resource is not available" (object not found); `Unknown`/`FetchingResourceFailed`/"Failed to fetch resource: <err>" (other get errors); `Unknown`/`IncompleteResourceMeta`/"Resource meta is incomplete". There is no `ResourceStatusUnknown` reason at manifest level | `ocm:pkg/work/spoke/objectreader/reader.go`; `hub:` for the True case |
| manifest | `StatusFeedbackSynced` | `True`/`NoStatusFeedbackSynced`/"" (no rules); `True`/`StatusFeedbackSynced`; `False`/`StatusFeedbackSyncFailed`/"Sync status feedback failed with error ..." | `ocm:pkg/work/spoke/controllers/statuscontroller/availablestatus_controller.go`, `hub:` |
| manifest | `Complete` from WellKnownConditions | Job: True when a `Complete` or `Failed` condition is True ("Job is finished"); Pod: phase in Succeeded/Failed | `ocm:pkg/work/spoke/conditions/rules/rule.go` |

WellKnownStatus feedback names (`ocm:pkg/work/spoke/statusfeedback/rules/rule.go`): Deployment
`ReadyReplicas`, `Replicas`, `AvailableReplicas`; DaemonSet `NumberReady`, `DesiredNumberScheduled`,
`NumberAvailable`; Job `JobComplete`, `JobFailed`, `JobSucceeded`; Pod `PodReady`, `PodPhase`. Nothing for
StatefulSet or CRDs: an `InferenceService`/`LLMInferenceService` needs `JSONPaths`, e.g.
`.status.conditions[?(@.type=="Ready")].status` or `.status.url`. `JsonRaw` values are capped at 1024
bytes (`ocm:pkg/work/spoke/statusfeedback/reader.go`, `maxJSONRawLength`); longer values fail the rule.

Hub ACM ManifestWorks (`hub:`) all carry finalizer `cluster.open-cluster-management.io/manifest-work-cleanup`.
Add-on works are named `addon-<name>-deploy-0`, labelled `open-cluster-management.io/addon-name=<addon>` and
owned by the `ManagedClusterAddOn`; the import controller's `<cluster>-klusterlet` and
`<cluster>-klusterlet-crds` works are instead labelled `import.open-cluster-management.io/klusterlet-works`
and owned by the `ManagedCluster`.

#### Example: model deployment ManifestWork with feedback

```yaml
apiVersion: work.open-cluster-management.io/v1
kind: ManifestWork
metadata:
  name: model-granite-8bx7k2p           # generateName = the MWRS name with no separator at upstream main (since 2026-03; deploy reconcile L433); older controllers may use a fixed name
  namespace: site-paris-1
  labels:
    ai-grid/model: granite-8b
    work.open-cluster-management.io/manifestworkreplicaset: ai-grid-models.model-granite-8b   # legacy label, the certain one on MCE 2.17.3
    work.open-cluster-management.io/placementname: model-granite-8b
    work.open-cluster-management.io/ownerkey-hash: 9f86d081884c7d659a2feaa0c55ad015            # upstream main only (since 2026-10)
  annotations:
    work.open-cluster-management.io/manifestworkreplicaset-owner: ai-grid-models/model-granite-8b   # upstream main only (since 2026-10)
  finalizers:
  - cluster.open-cluster-management.io/manifest-work-cleanup
spec:
  deleteOption:
    propagationPolicy: SelectivelyOrphan
    selectivelyOrphans:
      orphaningRules:
      - group: ""
        resource: namespaces
        name: ai-grid-models
  manifestConfigs:
  - resourceIdentifier:
      group: serving.kserve.io
      resource: inferenceservices
      namespace: ai-grid-models
      name: granite-8b
    feedbackRules:
    - type: JSONPaths
      jsonPaths:
      - {name: ready, path: '.status.conditions[?(@.type=="Ready")].status'}
      - {name: url, path: .status.url}
    conditionRules:
    - type: CEL
      condition: Degraded
      celExpressions:
      - 'object.status.conditions.exists(c, c.type == "Ready" && c.status == "False")'
      message: InferenceService not ready
    - type: CEL
      condition: Progressing
      celExpressions:
      - '!object.status.conditions.exists(c, c.type == "Ready" && c.status == "True")'
      message: InferenceService rolling out
  workload:
    manifests:
    - apiVersion: v1
      kind: Namespace
      metadata: {name: ai-grid-models}
    - apiVersion: serving.kserve.io/v1beta1
      kind: InferenceService
      metadata: {name: granite-8b, namespace: ai-grid-models}
      spec: {predictor: {model: {modelFormat: {name: vLLM}, storageUri: "oci://registry.example.com/models/granite-8b:1.0"}}}
status:
  conditions:
  - {type: Applied, status: "True", reason: AppliedManifestWorkComplete, message: Apply manifest work complete, observedGeneration: 1, lastTransitionTime: "2026-10-15T09:10:05Z"}
  - {type: Available, status: "True", reason: ResourcesAvailable, message: All resources are available, observedGeneration: 1, lastTransitionTime: "2026-10-15T09:10:06Z"}
  - {type: Progressing, status: "False", reason: ConditionRulesAggregated, message: InferenceService rolling out, lastTransitionTime: "2026-10-15T09:14:30Z"}   # single rule-bearing manifest -> its message is copied
  - {type: Degraded, status: "False", reason: ConditionRulesAggregated, message: InferenceService not ready, lastTransitionTime: "2026-10-15T09:10:06Z"}
  resourceStatus:
    manifests:
    - resourceMeta: {ordinal: 0, group: "", version: v1, resource: namespaces, kind: Namespace, namespace: "", name: ai-grid-models}
      conditions:
      - {type: Applied, status: "True", reason: AppliedManifestComplete, message: Apply manifest complete, observedGeneration: 1}
      - {type: Available, status: "True", reason: ResourceAvailable, message: Resource is available}
      - {type: StatusFeedbackSynced, status: "True", reason: NoStatusFeedbackSynced, message: ""}
      statusFeedback: {}
    - resourceMeta: {ordinal: 1, group: serving.kserve.io, version: v1beta1, resource: inferenceservices, kind: InferenceService, namespace: ai-grid-models, name: granite-8b}
      conditions:
      - {type: Applied, status: "True", reason: AppliedManifestComplete, message: Apply manifest complete, observedGeneration: 1}
      - {type: Available, status: "True", reason: ResourceAvailable, message: Resource is available}
      - {type: StatusFeedbackSynced, status: "True", reason: StatusFeedbackSynced, message: ""}
      - {type: Progressing, status: "False", reason: ConditionRuleEvaluated, message: InferenceService rolling out}
      - {type: Degraded, status: "False", reason: ConditionRuleEvaluated, message: InferenceService not ready}
      statusFeedback:
        values:
        - {name: ready, fieldValue: {type: String, string: "True"}}
        - {name: url, fieldValue: {type: String, string: "https://granite-8b-ai-grid-models.apps.site-paris-1.example.com"}}
```

While the InferenceService is still coming up the same work shows `Progressing=True`, the `ready` feedback
`"False"` (or absent until the status field exists) and `Available=True` (the object exists). A CRD missing on
the spoke yields manifest `Applied=False/AppliedManifestFailed` with message
`Failed to apply manifest: the server doesn't have a resource type "InferenceService"` (the REST mapping fails in
`ocm:pkg/work/helper/helpers.go` L558-561; prefix from `manifestwork_reconciler.go` L416), work
`Applied=False/AppliedManifestWorkFailed`; because that mapping failure leaves the manifest's `resourceMeta.resource`
empty, the next status sync sets manifest `Available=Unknown/IncompleteResourceMeta` "Resource meta is incomplete"
(`ocm:pkg/work/spoke/objectreader/reader.go` L76-83) and work `Available=Unknown/ResourcesStatusUnknown`
"1 of 2 resources have unknown status" (`ocm:pkg/work/spoke/conditions/helpers.go` L100-108); the status controller skips the
feedback rules and the condition rules of a manifest it cannot fetch (`ocm:pkg/work/spoke/controllers/statuscontroller/availablestatus_controller.go`,
`continue` after `objectReader.Get` fails), so that work carries no `Progressing` or `Degraded` condition.
Only the spoke agent writes a ManifestWork's status: a work in the namespace of a cluster whose agent is not
running (Available Unknown/False) keeps an empty `status` (no conditions, no `resourceStatus`), and between
the manifest controller's `Applied` and the status controller's first sync `Available` is absent, not Unknown
(`ResourcesStatusUnknown` only arises from a manifest-level `FetchingResourceFailed`/`IncompleteResourceMeta`
or an empty workload).

### ManifestWorkReplicaSet (`work.open-cluster-management.io/v1alpha1`, namespaced, short `mwrs`)

| Path | Type | Meaning |
|---|---|---|
| `spec.manifestWorkTemplate` | ManifestWorkSpec | same fields as ManifestWork spec (`explain:manifestworkreplicaset.spec`). |
| `spec.placementRefs[]{name, rolloutStrategy}` | required | Placements in the same namespace. |
| `spec.placementRefs[].rolloutStrategy.type` | `All` \| `Progressive` \| `ProgressivePerGroup` | (`api:cluster/v1alpha1/types_rolloutstrategy.go` via `explain:manifestworkreplicaset.spec.placementRefs.rolloutStrategy`) |
| `...rolloutStrategy.all{minSuccessTime, progressDeadline, maxFailures}` | | All: everything at once; only `progressDeadline` matters. |
| `...rolloutStrategy.progressive{maxConcurrency (int or "20%"), maxFailures (int or percent), minSuccessTime, progressDeadline, mandatoryDecisionGroups[]{groupName, groupIndex}}` | | `maxConcurrency` default = all non-mandatory clusters (`sdk:pkg/apis/cluster/v1alpha1/rollout.go`); `maxFailures` default 0; `progressDeadline` default `None` (wait forever); durations like `5m`, `1h`. |
| `...rolloutStrategy.progressivePerGroup{maxFailures, minSuccessTime, progressDeadline, mandatoryDecisionGroups[]}` | | group by group in `decisionGroupIndex` order; `maxFailures` is per group. There is no `maxConcurrency`: every cluster of the current group is rolled out at once, so with the single default decision group (`clustersPerDecisionGroup: "100%"`) every selected cluster gets its work together (`sdk:pkg/apis/cluster/v1alpha1/rollout.go` `getProgressivePerGroupClusters` L195-240). |
| `spec.cascadeDeletionPolicy` | `Background` (default) \| `Foreground` | Foreground keeps the MWRS until all ManifestWorks are gone (`ocm:.../manifestworkreplicaset_finalize_reconcile.go`). |
| `status.summary{total, desiredTotal, applied, available, progressing, degraded, updated}` | | `total` = generated works that exist and whose spec matches the current template (works with a stale template are skipped until re-applied); `applied`/`available`/`progressing`/`degraded` = those works with that condition True (`ocm:.../manifestworkreplicaset_deploy_reconcile.go` L79-90, L175-191; `..._status_reconcile.go` L40-87). `desiredTotal` and `updated` are in the API (`api:work/v1alpha1/types_manifestworkreplicaset.go` L132-135) but no controller writes them, so they stay 0. |
| `status.placementSummary[]{name, availableDecisionGroups, summary{...}}` | | per placement; `availableDecisionGroups` is the string "<number of decision groups> (<works that exist> / <selected clusters> clusters applied)" (`getAvailableDecisionGroupProgressMessage`, deploy reconcile L171-172, L437-439). |
| `status.conditions[]` | | `PlacementVerified`, `PlacementRolledOut`, `ManifestworkApplied` (`api:work/v1alpha1/types_manifestworkreplicaset.go`). |

Condition semantics (`ocm:pkg/work/hub/controllers/manifestworkreplicasetcontroller/manifestworkreplicaset_deploy_reconcile.go`
and `..._status_reconcile.go`; messages are `""` except NotAsExpected error text):

| Type | Status / Reason | When |
|---|---|---|
| `PlacementVerified` | `True`/`AsExpected` | every placementRef resolves to a Placement with >= 1 decision cluster |
| | `False`/`PlacementDecisionNotFound` | only when the referenced **Placement object** does not exist; reconcile stops, no works created |
| | `False`/`PlacementDecisionEmpty` | 0 clusters selected (PlacementDecision missing or with empty decisions) |
| | `False`/`NotAsExpected` | apply/rollout errors (message = aggregated error; since upstream 2026-10-06 this wins over `PlacementDecisionEmpty` when every apply failed) |
| `PlacementRolledOut` | `True`/`Complete` | every selected cluster's work counts as Succeeded: `Applied=True` **and** `Progressing=False`, both with `observedGeneration` equal to the work's generation. A work with no `Progressing` condition is `ToApply`, never Succeeded |
| | `False`/`Progressing` | otherwise (works still to apply, progressing, failed, soaking) |
| `ManifestworkApplied` | `True`/`AsExpected` | `available == total && progressing == 0 && degraded == 0` |
| | `False`/`Processing` | `progressing > 0 && degraded == 0` |
| | `False`/`NotAsExpected` | `degraded > 0`, or `total == 0` for a reason other than empty decision |
| | `False`/`PlacementDecisionEmpty` | `total == 0` because the placement selected nothing |

Generated ManifestWork: namespace = cluster name; labels copied from the MWRS plus
`work.open-cluster-management.io/placementname=<placement>` and
`work.open-cluster-management.io/manifestworkreplicaset=<ns>.<name>` (written when it fits 63 chars; on
MCE 2.17.3 this legacy label is the one that can be relied on). Upstream main additionally uses
`generateName = <mwrs name>` with no separator (names like `model-granite-8bx7k2p`; deploy reconcile L433;
ocm #1421, 2026-03-19), the label
`work.open-cluster-management.io/ownerkey-hash=<hex of the first 16 bytes of sha256("<ns>/<name>")>` and the
annotation `work.open-cluster-management.io/manifestworkreplicaset-owner=<ns>/<name>` (ocm #1684,
2026-10-01; `ocm:pkg/work/helper/helpers.go`). Which of these the MCE 2.17.3 controller writes is not
verified (its bundled OCM commit is unknown and the controller is not deployed on the dev hub). Deleting the MWRS deletes its works (finalizer
`work.open-cluster-management.io/manifest-work-cleanup`). When a cluster drops out of the
PlacementDecision the controller deletes that cluster's work (`ClustersRemoved`), which with
`SelectivelyOrphan` keeps the listed resources on the spoke.

Rollout counting (`sdk:pkg/apis/cluster/v1alpha1/rollout.go` states; mapping in
`ocm:.../manifestworkreplicaset_deploy_reconcile.go`): a cluster is `ToApply` when there is no work, when
`Applied` is missing or not True at the current generation, **or when the `Progressing` condition is missing
or stale** (`shouldReturnToApply`); `Progressing` (work `Progressing=True`, or Unknown); `Failed`
(`Progressing=True` and `Degraded=True`); `Succeeded` (`Progressing=False` at the current generation);
`TimeOut` after `progressDeadline` counted from the work's `Progressing` condition's `lastTransitionTime` (the
`Degraded` condition's for a `Failed` work; deploy reconcile `clusterRolloutStatusFunc` L228-271), but only from
`Progressing` or `Failed` (`sdk:rollout.go` `determineRolloutStatus`:
`ToApply` is appended to the rollout set with no timeout, so a cluster whose work is never applied or never
reported holds its slot indefinitely); plus `Skip`. `Progressive` fills `maxConcurrency` slots alphabetically
(`progressivePerCluster`); a `Succeeded` cluster keeps its slot for `minSuccessTime`; a `Failed` cluster keeps
its slot until its timeout **and** counts towards `maxFailures` at once (`failureCount++` for `TimeOut || Failed`,
breach when `failureCount > maxFailures`, after which no new cluster is added). **Without `conditionRules` producing `Progressing`
the work agent never emits that condition (only `Available` and rule-generated conditions are aggregated),
so at current upstream main every cluster stays `ToApply`, `PlacementRolledOut` stays `False/Progressing`
and a Progressive rollout never advances** (ocm #1207 2025-10-22 and #1243 2025-12-01; unit test "no
progressing condition - should return ToApply"). A MWRS template therefore needs a condition rule that
yields `Progressing`. Whether MCE 2.17.3's controller already has this logic is not verified.

#### Example: MWRS, rollout complete on 2 clusters

```yaml
apiVersion: work.open-cluster-management.io/v1alpha1
kind: ManifestWorkReplicaSet
metadata:
  name: model-granite-8b
  namespace: ai-grid-models
  labels:
    ai-grid/model: granite-8b
  finalizers:
  - work.open-cluster-management.io/manifest-work-cleanup
spec:
  cascadeDeletionPolicy: Background
  placementRefs:
  - name: model-granite-8b
    rolloutStrategy:
      type: Progressive
      progressive:
        maxConcurrency: 1
        progressDeadline: 15m
        maxFailures: 0
  manifestWorkTemplate:
    deleteOption:
      propagationPolicy: SelectivelyOrphan
      selectivelyOrphans:
        orphaningRules:
        - {group: "", resource: namespaces, name: ai-grid-models}
    manifestConfigs: [...]   # as in the ManifestWork above
    workload:
      manifests: [...]
status:
  summary:
    total: 2
    applied: 2
    available: 2
    progressing: 0
    degraded: 0
  placementSummary:
  - name: model-granite-8b
    availableDecisionGroups: 1 (2 / 2 clusters applied)
    summary: {total: 2, applied: 2, available: 2, progressing: 0, degraded: 0}
  conditions:
  - {type: PlacementVerified, status: "True", reason: AsExpected, message: "", lastTransitionTime: "2026-10-15T09:10:01Z"}
  - {type: PlacementRolledOut, status: "True", reason: Complete, message: "", lastTransitionTime: "2026-10-15T09:14:40Z"}
  - {type: ManifestworkApplied, status: "True", reason: AsExpected, message: "", lastTransitionTime: "2026-10-15T09:14:40Z"}
```

Mid-rollout (first cluster still progressing, maxConcurrency 1) the same object reads
`summary: {total: 1, applied: 1, available: 1, progressing: 1, degraded: 0}` (the second cluster has no
ManifestWork yet, and `total` counts works that exist), `placementSummary[0].availableDecisionGroups:
"1 (1 / 2 clusters applied)"`, `PlacementRolledOut=False/Progressing`, `ManifestworkApplied=False/Processing`. With a broken manifest on one cluster: `degraded: 1`,
`ManifestworkApplied=False/NotAsExpected`, `PlacementRolledOut=False/Progressing` and rollout halts
(`maxFailures: 0`). Placement selecting nothing: `summary.total: 0`,
`PlacementVerified=False/PlacementDecisionEmpty`, `ManifestworkApplied=False/PlacementDecisionEmpty`.

#### Behaviour with the feature gate OFF (the dev hub today)

The CRD `manifestworkreplicasets.work.open-cluster-management.io` is installed (listed under
`ClusterManager.status.relatedResources`, `hub:`), so `oc create` of an MWRS succeeds and the object is
stored. Nothing watches it: no `cluster-manager-work-controller` Deployment exists in
`open-cluster-management-hub` (only `cluster-manager-registration-controller`,
`cluster-manager-registration-webhook`, `cluster-manager-work-webhook`, `cluster-manager-placement-controller`,
`cluster-manager-addon-webhook`, `cluster-manager-addon-manager-controller`; `hub:` 2026-10-16). The object keeps no
finalizer, `status` stays empty (`oc get mwrs` shows blank `Placement`/`Found`/`ManifestWorks`/`Applied`
columns), no ManifestWorks appear in cluster namespaces, and deletion is immediate (inferred from the
absence of a controller; not exercised, read-only). A backend writing MWRS
against this hub would silently do nothing; it must check for the controller (or
`ClusterManager.spec.workConfiguration.featureGates`) and surface "ManifestWorkReplicaSet not enabled on hub"
(gRPC `FailedPrecondition`).

## 5. Add-ons and observability

### ManagedClusterAddOn (`addon.open-cluster-management.io/v1alpha1` storage, `v1beta1` also served and preferred by `oc api-resources`; namespaced per cluster, short `mca`)

Condition types and reasons (`api:addon/v1beta1/types_managedclusteraddon.go`, `hub:`):

| Type | Typical healthy value | Other reasons |
|---|---|---|
| `Available` | `True`/`ManagedClusterAddOnLeaseUpdated`/"<addon> add-on is available." or `True`/`ProbeAvailable` | `ManagedClusterAddOnLeaseUpdateStopped`, `ManagedClusterAddOnLeaseNotFound`, `ProbeUnavailable`, `NoProbeResult`, `WorkNotFound`, `WorkApplyFailed`, `WorkNotApplied`, `WorkApplied` |
| `Progressing` | `False`/`Completed`/"completed with no errors." | `Progressing`, `Failed`, `WaitingForCanary`, `ConfigurationUnsupported` |
| `Degraded` | `False`/addon-specific (e.g. `HypershiftDeployed`) | addon-specific |
| `ManifestApplied` | `True`/`AddonManifestApplied`/"manifests of addon are applied successfully" | `AddonManifestAppliedFailed`, `ManifestWorkApplyFailed` |
| `RegistrationApplied` | `True`/`SetPermissionApplied`/"Registration of the addon agent is configured" | `SetPermissionFailed`, `NilRegistration` |
| `Configured` | `True`/`ConfigurationsConfigured`/"Configurations configured" | |
| `ClusterCertificateRotated` | `True`/`ClientCertificateUpdated`/"client certificate rotated starting from <t> to <t>" | |
| `HostingClusterValidity`, `HostingManifestApplied`, `HookManifestCompleted` | hosted mode only | |

`oc get mca -n <cluster>` columns: AVAILABLE, DEGRADED, PROGRESSING. The ManagedCluster label
`feature.open-cluster-management.io/addon-<name>` mirrors `Available` (True -> `available`, False ->
`unhealthy`, Unknown or condition missing -> `unreachable`; written by the registration hub's addon
feature-discovery controller, `ocm:pkg/registration/hub/addon/discovery_controller.go`), which is the cheap
way for a fleet page to show add-on health without reading add-ons.
Add-ons on an ACM-imported OpenShift cluster (`hub:`): `application-manager`, `cert-policy-controller`,
`cluster-proxy`, `config-policy-controller`, `governance-policy-framework`, `managed-serviceaccount`,
`work-manager`, plus `multicluster-observability-addon` once MCO exists, `hypershift-addon` on the hub itself.
`status.configReferences[]` lists `AddOnDeploymentConfig` / `PrometheusAgent` / `PrometheusRule` configs with
`desiredConfig.specHash` and `lastAppliedConfig.specHash`.

### MultiClusterObservability (`observability.open-cluster-management.io/v1beta2`, cluster-scoped, short `mco`)

Installed on the dev hub as `observability` (`hub:`), `spec.observabilityAddonSpec.{enableMetrics: true, interval: 300, scrapeSizeLimitBytes, workers}`,
`spec.enableDownsampling: true`, `spec.advanced.retentionConfig.{retentionResolutionRaw: 24h, retentionResolution5m: 7d, retentionResolution1h: 14d}`,
`spec.storageConfig`, `spec.instanceSize`. At the time of the check its status showed
`Installing=True` and `Failed=True/StatefulSetNotReady` ("statefulset observability-thanos-compact is not ready");
the three `observability-thanos-store-shard-*` StatefulSets were also 0/1 ready: the stack is present but not
fully healthy, so metric queries may be partial.

Names in namespace `open-cluster-management-observability` (`hub:`, names only):

- Services: `observability-thanos-query` (Thanos Query, HTTP 9090 with `cluster` label on every series),
  `observability-thanos-query-frontend`, `observability-observatorium-api`, `rbac-query-proxy`
  (RBAC-filtered PromQL; that the ACM console uses it is not verified), `observability-thanos-receive`, `observability-thanos-rule`,
  `observability-thanos-store-shard-*`, `observability-thanos-compact`, `alertmanager`, `grafana`,
  `metrics-collector`, `uwl-metrics-collector`.
- Routes: `rbac-query-proxy`, `observatorium-api`, `grafana`, `alertmanager`.
- Per-cluster `ObservabilityAddon` objects (`observability.open-cluster-management.io/v1beta1`) live in each
  cluster namespace once the add-on is deployed; none existed yet on the dev hub.

The backend's `Grid/GetOverview` should query `rbac-query-proxy` (user token) or
`observability-thanos-query` (service account) with PromQL filtered by `cluster="<name>"`; the mock only needs
to emit the POC's `requests/errors/latency/gpu` series per cluster.

## 6. Hosted or provisioning clusters

CRDs present on the hub: Hive (`clusterdeployments.hive.openshift.io`, `clusterpools`, `clusterclaims.hive.openshift.io`,
`machinepools`), HyperShift (`hostedclusters.hypershift.openshift.io`, `nodepools`, `hostedcontrolplanes`),
`clustercurators.cluster.open-cluster-management.io`, `clusterinstances.siteconfig.open-cluster-management.io`,
`discoveredclusters.discovery.open-cluster-management.io` (`oc api-resources`). No instances of
`ClusterDeployment` or `HostedCluster` exist (`hub:`). The ACM console only reaches the `creating`,
`provisionfailed`, `destroying`, `hibernating`, `stopping`, `resuming`, `prehookjob` states when one of those
objects exists for the cluster name (`console:.../get-cluster.ts`); for the AI Grid flow (bring-your-own
cluster via import) the ManagedCluster-only states in section 1 are sufficient. If provisioning is added
later, the Hive object sits in the cluster namespace with the same name and `created-via: hive`.

## Feature gate and installation facts found on the hub (anonymised)

| Fact | Value |
|---|---|
| ACM / MCE version | `MultiClusterHub` `multiclusterhub` 2.17.3, status `Running`, "All hub components ready." Components enabled: app-lifecycle, cluster-lifecycle, console, grc, insights, multicluster-engine, multicluster-observability, search, submariner-addon, volsync; disabled: cluster-backup, fine-grained-rbac, siteconfig, cnv-mtv-integrations. |
| Managed clusters | exactly one, `local-cluster` (the hub itself, ROSA HCP, `product=ROSA`, `platform=AWS`, `controlplanetopology=External`), Joined/Available True. |
| `ClusterManager` `cluster-manager` | `spec.deployOption.mode: Default`; `spec.workConfiguration: {workDriver: kube}` with **no `featureGates`**; `spec.placementConfiguration.featureGates: [{feature: PlacementDebugServer, mode: Enable}]`; no `registrationConfiguration.featureGates` (so `ManagedClusterAutoApproval`, `ClusterProperty`, `ResourceCleanup`, `ClusterProfile` are at their defaults). Owned by `MultiClusterEngine` `multiclusterengine`. |
| MWRS controller | absent: no `cluster-manager-work-controller` Deployment and no MWRS objects. Enabling is via `spec.workConfiguration.featureGates: [{feature: ManifestWorkReplicaSet, mode: Enable}]` (`docs:concepts/work-distribution/manifestworkreplicaset/`); because MCE owns the ClusterManager, the change may need to go through the `MultiClusterEngine` overrides or be re-applied if MCE reconciles it back (not tested, read-only). |
| Placement controller | running (`cluster-manager-placement-controller`), `HubPlacementDegraded=False/PlacementFunctional`. Four Placements exist: `global` and two ACM right-sizing ones in `open-cluster-management-global-set`, and one from the POC's GitOps app selecting `clusterSets: [global]`. |
| ClusterSets | `default` (ExclusiveClusterSetLabel) and `global` (LabelSelector `{}`), both with 1 member. Bindings of `global` into `open-cluster-management-global-set` and into the POC's GitOps namespace. |
| Observability | `MultiClusterObservability` `observability` present, status `Installing=True`, `Failed=True/StatefulSetNotReady`. |
| Hive / HyperShift | CRDs installed, no instances. |
| Add-ons on `local-cluster` | ten on 2026-10-16 (nine on 2026-10-15; `submariner` was added), all Available True. `ClusterManagementAddOn`s available hub-wide: application-manager, cert-policy-controller, cluster-proxy, config-policy-controller, gitops-addon, governance-policy-framework, governance-standalone-hub-templating, hypershift-addon, managed-serviceaccount, multicluster-observability-addon, search-collector, submariner, volsync, work-manager. |
| Cluster namespace labels | `cluster.open-cluster-management.io/managedCluster=<name>`, `open-cluster-management.io/cluster-name=<name>`, PSA `restricted` audit/warn. |

## Proposal: gRPC surface and what the fake backend must simulate

### `ManagedClusters` service

**Superseded for field numbers.** `proto/ai_grid/draft/v1/managed_cluster_type.proto` and
`managed_clusters_service.proto` are the authoritative draft; do not reuse the numbers below. The draft
differs: it keeps the CRD's shape (`annotations` 4, `uid` 5, `resource_version` 6, `creation_timestamp` 7,
`deletion_timestamp` 8, `lease_duration_seconds` 9, `managed_cluster_client_configs` 10 with `ca_bundle`,
`status` 11 as a nested `ManagedClusterStatus`, `taints` 12, `generation` 13, `finalizers` 14), puts
conditions, capacity, allocatable, `kubernetes_version`, `cluster_claims`, the derived `state` and
`state_message` (7) inside `ManagedClusterStatus`, uses the shared `Condition` with a `ConditionStatus` enum,
has no flattened claim fields (`console_url`, `openshift_version`, `platform`, `product`, `region`) and no
`addon_status` map (the BFF derives them), names the state enum `ManagedClusterState` with values
`UNSPECIFIED` 0, `PENDING` 1, `AVAILABLE` 2, `UNAVAILABLE` 3, `DETACHING` 4, `PENDING_IMPORT` 5, `IMPORTING` 6,
`IMPORT_FAILED` 7, `NOT_ACCEPTED` 8, `UNKNOWN` 9, and has one `UpdateManagedCluster` (field mask + `lock`)
instead of `UpdateManagedClusterLabels`, and no `Watch`. The sketch below is kept as the research record.

Keep `name`, `labels`, `hub_accepts_client` and add the fields below (field numbers continue from 4).
Everything is derived from one ManagedCluster read plus its annotations; no `ManagedClusterInfo` needed.

```proto
message ManagedCluster {
  string name = 1;
  map<string, string> labels = 2;
  bool hub_accepts_client = 3;
  map<string, string> annotations = 4;        // ai-grid/site-name, ai-grid/latitude, ai-grid/longitude, created-via
  repeated Condition conditions = 5;          // all status.conditions, verbatim
  repeated ClusterClaim cluster_claims = 6;   // {name, value}
  map<string, string> capacity = 7;           // quantity strings, e.g. "192", "1572864Mi", "16" for nvidia.com/gpu
  map<string, string> allocatable = 8;
  string kubernetes_version = 9;              // status.version.kubernetes
  repeated ClientConfig client_configs = 10;  // {url} only, never caBundle
  repeated Taint taints = 11;                 // {key, value, effect, time_added}
  google.protobuf.Timestamp creation_time = 12;
  google.protobuf.Timestamp deletion_time = 13; // set => detaching
  ClusterStatus status = 14;                  // derived server-side, see enum
  string status_message = 15;                 // message of the condition that decided status
  string console_url = 16;                    // claim consoleurl.cluster.open-cluster-management.io
  string openshift_version = 17;              // claim version.openshift.io
  string platform = 18;                       // claim platform.open-cluster-management.io
  string product = 19;                        // claim product.open-cluster-management.io
  string region = 20;                         // claim region.open-cluster-management.io
  map<string, string> addon_status = 21;      // addon name -> available|unhealthy|unreachable (from feature.* labels)
}
message Condition { string type = 1; string status = 2; string reason = 3; string message = 4; google.protobuf.Timestamp last_transition_time = 5; int64 observed_generation = 6; }
message ClusterClaim { string name = 1; string value = 2; }
message ClientConfig { string url = 1; }
message Taint { string key = 1; string value = 2; string effect = 3; google.protobuf.Timestamp time_added = 4; }
enum ClusterStatus { CLUSTER_STATUS_UNSPECIFIED = 0; PENDING_IMPORT = 1; IMPORTING = 2; IMPORT_FAILED = 3; READY = 4; OFFLINE = 5; UNKNOWN = 6; DETACHING = 7; NOT_ACCEPTED = 8; }

service ManagedClusters {
  rpc ListManagedClusters(ListManagedClustersRequest) returns (ListManagedClustersResponse);
  rpc GetManagedCluster(GetManagedClusterRequest) returns (ManagedCluster);          // NotFound
  rpc UpdateManagedClusterLabels(UpdateManagedClusterLabelsRequest) returns (ManagedCluster); // {name, set_labels map, remove_labels [], set_annotations map, remove_annotations []}; PermissionDenied without patch RBAC
  rpc WatchManagedClusters(WatchManagedClustersRequest) returns (stream ManagedClusterEvent); // ADDED|MODIFIED|DELETED, optional
}
```

`status` derivation (server-side, same as the ACM console): deletion_time -> DETACHING;
`HubAcceptedManagedCluster` not True (absent or False, so a Denied cluster lands here) -> NOT_ACCEPTED;
`ManagedClusterJoined` not True -> PENDING_IMPORT, or IMPORTING / IMPORT_FAILED by
`ManagedClusterImportSucceeded` reason; else by `ManagedClusterConditionAvailable` True -> READY, False ->
OFFLINE, Unknown/missing -> UNKNOWN.

The BFF's `GridCluster` then adds `status`, `statusMessage`, `consoleUrl`, `siteName`, `latitude`,
`longitude`, `platform`, `product`, `gpuCapacity` (`capacity["nvidia.com/gpu"]`), `kubernetesVersion`,
`addons`, as planned in `ai-grid-provider-console/docs/plan/bff-api.md`.

### `Placements` service

```proto
message Placement {
  string name = 1; string namespace = 2;
  map<string, string> labels = 3;
  repeated string cluster_sets = 4;
  optional int32 number_of_clusters = 5;
  repeated ClusterPredicate predicates = 6;          // {LabelSelector label_selector; repeated ClaimExpression claim_expressions}
  repeated Toleration tolerations = 7;
  repeated SpreadConstraint spread_constraints = 8;  // {topology_key, topology_key_type, max_skew, when_unsatisfiable}
  PrioritizerPolicy prioritizer_policy = 9;          // {mode; repeated {type, built_in, addon_resource, addon_score, weight}}
  DecisionStrategy decision_strategy = 10;           // {clusters_per_decision_group string; repeated DecisionGroup}
  int32 number_of_selected_clusters = 11;
  repeated Condition conditions = 12;                // PlacementSatisfied, PlacementMisconfigured
  repeated DecisionGroupStatus decision_groups = 13; // {index, name, decision_names[], cluster_count}
  repeated ClusterDecision decisions = 14;           // flattened from all PlacementDecisions: {cluster_name, reason, score, group_index}
  google.protobuf.Timestamp creation_time = 15;
}
message LabelSelector { map<string,string> match_labels = 1; repeated LabelSelectorRequirement match_expressions = 2; } // {key, operator In|NotIn|Exists|DoesNotExist, values}
message ClaimExpression { string key = 1; string operator = 2; repeated string values = 3; }

service Placements {
  rpc ListPlacements(ListPlacementsRequest) returns (ListPlacementsResponse);     // namespace filter
  rpc GetPlacement(GetPlacementRequest) returns (Placement);
  rpc CreatePlacement(CreatePlacementRequest) returns (Placement);                // AlreadyExists; FailedPrecondition when the namespace has no ManagedClusterSetBinding
  rpc UpdatePlacement(UpdatePlacementRequest) returns (Placement);
  rpc DeletePlacement(DeletePlacementRequest) returns (google.protobuf.Empty);
}
```

### `ManifestWorkReplicaSets` service

```proto
message ManifestWorkReplicaSet {
  string name = 1; string namespace = 2;
  map<string, string> labels = 3;
  repeated PlacementRef placement_refs = 4;          // {name, RolloutStrategy {type All|Progressive|ProgressivePerGroup, max_concurrency, max_failures, min_success_time, progress_deadline, mandatory_decision_groups[]}}
  ManifestWorkTemplate template = 5;                 // {repeated bytes manifests (JSON); repeated ManifestConfig; DeleteOption {propagation_policy, repeated OrphaningRule}; string cascade_deletion_policy}
  Summary summary = 6;                               // {total, desired_total, applied, available, progressing, degraded, updated}
  repeated PlacementSummary placement_summary = 7;   // {name, available_decision_groups, Summary}
  repeated Condition conditions = 8;                 // PlacementVerified, PlacementRolledOut, ManifestworkApplied
  repeated ManifestWorkStatus works = 9;             // per cluster, see below
  google.protobuf.Timestamp creation_time = 10;
  google.protobuf.Timestamp deletion_time = 11;
}
message ManifestConfig { ResourceIdentifier resource = 1; repeated FeedbackRule feedback_rules = 2; repeated ConditionRule condition_rules = 3; UpdateStrategy update_strategy = 4; }
message ManifestWorkStatus {                         // one per generated ManifestWork
  string cluster_name = 1; string work_name = 2;
  repeated Condition conditions = 3;                 // Applied, Available, Progressing, Degraded, Complete
  repeated ManifestStatus manifests = 4;             // {ResourceIdentifier (ordinal, group, version, resource, kind, namespace, name); repeated Condition; repeated FeedbackValue {name, type Integer|String|Boolean|JsonRaw, string value}}
  int64 generation = 5; int64 observed_generation = 6;
}

service ManifestWorkReplicaSets {
  rpc ListManifestWorkReplicaSets(...) returns (...);        // namespace filter
  rpc GetManifestWorkReplicaSet(...) returns (ManifestWorkReplicaSet); // includes works[]
  rpc CreateManifestWorkReplicaSet(...) returns (ManifestWorkReplicaSet); // FailedPrecondition "ManifestWorkReplicaSet feature gate disabled on hub"; InvalidArgument when placementRef not found in namespace
  rpc UpdateManifestWorkReplicaSet(...) returns (ManifestWorkReplicaSet);
  rpc DeleteManifestWorkReplicaSet(...) returns (google.protobuf.Empty);
  rpc GetHubCapabilities(GetHubCapabilitiesRequest) returns (HubCapabilities); // {manifest_work_replica_set_enabled, observability_installed, acm_version}
}
```

A `Models` service on top of this (the plan's `Models/*`) would wrap one Placement + one MWRS per model in
the models namespace, labelled `ai-grid/model=<name>`, and present `per-site status` from
`ManifestWorkStatus.conditions` and the `ready`/`url` feedback values.

### What the fake backend must simulate (realistic transitions)

ManagedCluster:

1. Seed 3-6 clusters in mixed states: READY with GPUs, READY without `ai-grid/enabled`, UNKNOWN (lease
   expired, taint `unreachable`, addon labels `unreachable`), OFFLINE (`unavailable` taint), one
   PENDING_IMPORT. Include `local-cluster` with `local-cluster: "true"` and no `ai-grid/*` labels.
2. "Enrol a cluster" (create ManagedCluster, `hubAcceptsClient: true`): t+0 `HubAcceptedManagedCluster=True`,
   `ImportSucceeded=False/ManagedClusterWaitForImporting` ("Wait for importing", console `pendingimport`), then
   `False/ManagedClusterImporting` ("Start to import managed cluster", console `importing`), labels `name`,
   `cloud=auto-detect`, `vendor=auto-detect`, `clusterset=default`, taint `unreachable`; t+20..60 s
   `ManagedClusterJoined=True`, `Available=True/ManagedClusterAvailable`, `ClockSynced=True`, taint removed,
   `capacity`/`allocatable`/`version`/`clusterClaims` populated, `cloud`/`vendor`/`clusterID`/`openshiftVersion*`
   labels replaced; t+70 s `ImportSucceeded=True/ManagedClusterImported` ("Import succeeded"); then
   `feature.../addon-*` labels appear one by one over the next 60 s.
3. Lease loss: flip `Available` to `Unknown/ManagedClusterLeaseUpdateStopped` with message
   "Registration agent stopped updating its lease.", add taint `unreachable` with `timeAdded`, set addon labels
   to `unreachable`; recovery reverses it with a new `lastTransitionTime`. Offline variant uses
   `False/ManagedClusterKubeAPIServerUnavailable` and taint `unavailable`.
4. Label/annotation patch: only mutates `labels`/`annotations`; `resourceVersion`-style conflict on
   concurrent writes is optional. Attempting to remove `name`, `cloud`, `vendor`, `clusterID`, `openshiftVersion*`
   should be allowed by the mock but is a UI bug to guard against: ACM re-adds `name` (import controller,
   `mcic:pkg/controller/managedcluster/managedcluster_controller.go` L125), `clusterID` and `openshiftVersion*`
   (foundation autodetect), but **not** `cloud` or `vendor`, which it only rewrites while they read `auto-detect`.
5. Delete: set `deletion_time`, status DETACHING, `ImportSucceeded=False/ManagedClusterDetaching` ("The managed
   cluster is being detached now"), `Deleting=False/ResourceRemaining` while works or add-ons remain and
   `Deleting=True/NoResource` "No cleaned resource in cluster ns." once they are gone; object
   disappears after 10-30 s; any PlacementDecision drops the cluster at once (terminating clusters are never
   selected) and the MWRS deletes that cluster's work.

Placement / PlacementDecision:

6. Recompute decisions on every ManagedCluster label/taint/deletion change and on every Placement change:
   filter by bound clusterSets (`global` = all; `default` = clusters with the `clusterset=default` label), then
   predicates (OR across predicates, AND inside), drop clusters with `NoSelect` taints not tolerated
   (honour `tolerationSeconds` against `time_added`), sort by name, cap at `number_of_clusters`. Write
   `number_of_selected_clusters`, one decision group (index 0, name ""), decisions
   `<name>-decision-1`, and `PlacementSatisfied` with the exact reasons in section 3 (`NoManagedClusterSetBindings`
   when the namespace has no binding; `NotAllDecisionsScheduled` with "<n> cluster decisions unscheduled" when
   `number_of_clusters` exceeds matches; `NoManagedClusterMatched` when zero). Always include
   `PlacementMisconfigured=False/Succeedconfigured`.
7. On create add only the CRD defaults `scoreRateLimit: 1m` and `sortBy: ClusterName`; do **not** inject
   `clustersPerDecisionGroup: 0`, `prioritizerPolicy.mode: Additive`, `spreadPolicy: {}` or empty
   `celSelector`/`claimSelector` (those are Go-client artefacts on ACM's own Placements, not hub defaults).
   `prioritizerPolicy.mode: Additive` / `clustersPerDecisionGroup: "100%"` only if the caller sent the parent
   object. Populate `status.clusterClaims` as one name-sorted list.

ManifestWorkReplicaSet:

8. Capability flag `manifest_work_replica_set_enabled` (default true in the mock, false to mirror the dev
   hub) gating Create with `FailedPrecondition`.
9. On create or when a Placement's decisions change: `PlacementVerified` per section 4
   (`PlacementDecisionNotFound` only when the Placement object is missing; 0 selected clusters ->
   `PlacementDecisionEmpty`); create one `ManifestWorkStatus` per selected cluster (`work_name =
   <mwrs><5 random chars>`, no separator, mirrors upstream `generateName`; label it with the legacy
   `work.open-cluster-management.io/manifestworkreplicaset=<ns>.<name>` and `placementname`; the
   `ownerkey-hash` label and owner annotation are upstream-main only), respecting `max_concurrency` for
   Progressive (next cluster starts only when the previous is Succeeded for `min_success_time`). Per work:
   t+0 `Applied=True/AppliedManifestWorkComplete` (no `Available` yet) -> t+2 s
   `Available=True/ResourcesAvailable`; if the template has condition rules, `Progressing=True` for 20-90 s
   then `False` (work-level message = the single rule-bearing manifest's message when only one manifest has
   rules, otherwise "Aggregated from <n> manifest conditions"); feedback `ready` goes `"False"` -> `"True"`,
   `url` appears. **Rollout rule (current upstream main): a work counts as Succeeded only when `Applied=True`
   and `Progressing=False` both at the current `observed_generation`; a work with no `Progressing`
   condition stays `ToApply`, so a template without a Progressing-yielding condition rule never completes
   its rollout and Progressive never advances past `max_concurrency`.** Manifest-level conditions as in the
   example; manifest `Available=Unknown` uses reason `FetchingResourceFailed`, `False` uses
   `ResourceNotAvailable`. For a cluster in UNKNOWN state the work keeps an empty status (no conditions, no
   manifests: only the spoke agent writes it) and counts as `ToApply` for as long as the agent is silent;
   `ToApply` has no timeout in sdk-go, so it holds its Progressive slot and never counts towards
   `max_failures`.
10. Summary recomputed from the works: `total` = works that exist and match the current template (not the
    selected clusters), `applied`/`available`/`progressing`/`degraded` = counts of those with that condition
    True; `desired_total` and `updated` stay 0; `PlacementRolledOut=True/Complete` only when every cluster is Succeeded
    by the rule in step 9; `ManifestworkApplied` by the three rules (AsExpected / Processing / NotAsExpected /
    PlacementDecisionEmpty). `placement_summary[].available_decision_groups = "<decision groups> (<works that exist> / <selected clusters> clusters applied)"`.
11. Failure injection: a cluster label such as `ai-grid-mock/fail-apply=true` makes its work
    `Applied=False/AppliedManifestWorkFailed`, manifest `AppliedManifestFailed` with
    `Failed to apply manifest: the server doesn't have a resource type "<Kind>"` and
    `Available=Unknown/IncompleteResourceMeta` "Resource meta is incomplete", work
    `Available=Unknown/ResourcesStatusUnknown` "<k> of <n> resources have unknown status" (section 4); no
    `Progressing` or `Degraded` condition exists (the status controller skips the rules of a manifest it
    cannot fetch), `summary.degraded` stays 0, `ManifestworkApplied=False/NotAsExpected` because
    `available != total`, `PlacementRolledOut=False/Progressing`, and the cluster stays `ToApply`, holding
    its Progressive slot without a timeout. A `Failed` cluster (rule `Degraded=True` from a terminal spoke
    condition) counts towards `max_failures` at once.
12. Update of the template bumps `generation`; works go back to `Applied` True with new
    `observed_generation` and re-run the Progressing cycle. Until a work is re-applied with the new template it
    drops out of every summary count (`summary.updated` is never written upstream, so it stays 0).
13. Delete: with `cascade_deletion_policy` Background (the default) the finalize reconciler deletes every
    work and removes the finalizer in the same pass (`ocm:manifestworkreplicaset_finalize_reconcile.go`), so
    the MWRS disappears within one reconcile and the works drain in the cluster namespaces afterwards;
    Foreground keeps the MWRS (with `deletion_time`) until the works are gone. Removing a cluster from the Placement
    deletes just that cluster's work; `SelectivelyOrphan` rules are informational to the mock.

Observability mock: per READY cluster emit the POC's `requests`, `errors`, `latency`, `gpu` series for
`1h|6h|24h`; UNKNOWN/OFFLINE clusters return no samples (Thanos has no fresh series), which the UI must
render as "no data", not zero.

## Open questions / not verified

1. Enabling the `ManifestWorkReplicaSet` gate on an MCE-owned `ClusterManager`: whether MCE reverts a
   direct edit, or whether it must be set through `MultiClusterEngine` overrides. Needs a write test on the
   dev hub (out of scope for this read-only spike) or the MCE operator docs for 2.17.
2. (Resolved by verification.) `ManagedClusterDetaching` / `ManagedClusterForceDetaching` messages are "The
   managed cluster is being detached now" / "The managed cluster is being detached by force"; manifest-level
   `Available` is `False/ResourceNotAvailable/"Resource is not available"` or
   `Unknown/FetchingResourceFailed/"Failed to fetch resource: <err>"`. Still open: which OCM commit MCE 2.17.3
   bundles, so every "upstream main" behaviour above (MWRS labels, `generateName`, Progressing-based rollout)
   is dated by commit rather than confirmed for the hub.
3. The hub's registration feature gates (`ManagedClusterAutoApproval` etc.) are at defaults; whether ACM
   2.17's import flow auto-approves CSRs for the console's enrol flow (it does when the import controller
   deploys the klusterlet with a bootstrap secret) vs. the `needsapproval` path for `clusteradm join`.
4. `status.capacity` extras `core_worker` / `socket_worker` come from ACM's foundation capacity controller
   (verified: `found:pkg/controllers/clusterinfo/capacity_controller.go`). GPU keys such as `nvidia.com/gpu`
   appear only when the device plugin runs; not observable on the dev hub.
5. (Resolved by the 2026-10-16 audit.) `PlacementDecision.status.decisions[].score` is never written: the
   controller sets `clusterName` only, and `sortBy`/`scoreRateLimit`/`lastScoreUpdateTime` exist only in the
   API (section 3). The mock should always omit `score`.
6. Observability on the dev hub was still `Installing`/`Failed` (thanos-compact StatefulSet not ready);
   whether `rbac-query-proxy` answers queries for `local-cluster` is unverified.
7. No second managed cluster exists on the dev hub, so multi-cluster PlacementDecision pagination, `Spread`
   prioritizer behaviour and MWRS Progressive rollouts could not be observed live; those sections rely on
   source and docs.
8. Whether the console should bind the models namespace to `global` (every cluster) or to `default` plus
   an `ai-grid` exclusive set (a cluster can be in only one exclusive set, which would conflict with
   `clusterset=default` that ACM writes). Recommendation: bind `global` and filter with `ai-grid/enabled`
   labels in the Placement, as the POC does.

## Verification

Independently re-checked on 2026-10-15 against the dev hub (read-only `oc get` / `oc explain` /
`oc api-resources`) and fresh shallow clones of upstream `main` (`api` 2c5da18, `ocm` 8b0d31f, `sdk-go`
592fbe6, `managedcluster-import-controller` f05b4e13, `multicloud-operators-foundation` 6654c28,
`cluster-lifecycle-api` 43755d6) plus `stolostron/console` `get-cluster.ts` / `status-conditions.ts` from
`main`.

Re-checked and confirmed: every ManagedCluster condition type/reason/message, taint keys and effect, the 5x
lease grace factor, auto-label and claim allowed values, import-controller reasons and messages,
ManagedClusterSet/Binding shapes and reasons, PlacementSatisfied/PlacementMisconfigured reasons and
messages, PlacementDecision naming/labels/100 cap/empty decision, ManifestWork Applied/Available/
StatusFeedbackSynced strings, WellKnownStatus names, the 1024-byte JsonRaw cap, MWRS condition reasons and
the ManifestworkApplied rules, rollout defaults, `cascadeDeletionPolicy` default, the feature-gate facts
(no `workConfiguration.featureGates`, no `cluster-manager-work-controller` Deployment, MWRS CRD present,
no MWRS objects), and the observability names and status.

Corrected in this revision: Placement "webhook defaults" (none exist; only CRD defaults `scoreRateLimit`
and `sortBy`), the MWRS rollout rule (a work without `Progressing` is `ToApply`; Succeeded needs
`Progressing=False` and `Applied=True` at the current generation), generated-work labels (legacy
`manifestworkreplicaset=<ns>.<name>` is the certain one on MCE 2.17.3), manifest `Available=Unknown` reason
(`FetchingResourceFailed`), the ACM console `notaccepted`/`pendingimport` tests ("not True", so Denied ->
notaccepted), claim ordering (one name-sorted list on the hub), the aggregated condition message for a
single rule-bearing manifest, `PlacementDecisionNotFound` scope, the Detaching messages, taint `timeAdded`
origin (ManagedCluster mutating webhook), the addon label writer and its missing/Unknown -> `unreachable`
rule, the `/healthz` fallback, the `oc get mwrs` columns, and the klusterlet-work labels. Added in the
Models-area review (2026-10-15, same `ocm`/`sdk-go` clones): a never-reporting agent leaves a ManifestWork
status empty and `Available` is absent (not Unknown) until the first status sync; a manifest that failed to
apply gets no rule conditions; `ToApply` has no rollout timeout while `Failed` counts towards `maxFailures`
at once; Background cascade removes the MWRS in one pass (section 4, items 9, 11, 13).

Still uncheckable from here: which OCM commit MCE 2.17.3 bundles (hub images are sha-pinned Red Hat
builds), MWRS runtime behaviour on this hub (would require a write; no controller is deployed),
multi-cluster PlacementDecision ordering/pagination and Progressive rollout timing (one managed cluster
only), and whether `rbac-query-proxy` answers queries for `local-cluster`.

Audit (2026-10-16). Re-checked against `ocm` 8b0d31f, `api` (shallow clone plus per-tag reads with
`gh api`), `sdk-go`, `managedcluster-import-controller` f05b4e13, `multicloud-operators-foundation`,
`stolostron/console` a2b2f57 and the hub (read-only `oc get`). Corrected: the missing-CRD failure strings
(`the server doesn't have a resource type "<Kind>"`, manifest `Available=Unknown/IncompleteResourceMeta`, work
`Available=Unknown/ResourcesStatusUnknown`; section 4 and fake item 11); generated work names have no separator
after the MWRS name; `summary.total` counts existing works matching the current template, `availableDecisionGroups`
is `<groups> (<existing works> / <selected> clusters applied)`, `desiredTotal`/`updated` are never written; the
mid-rollout example; `progressDeadline` counts from the work's `Progressing` (or `Degraded`) transition and
`ProgressivePerGroup` has no `maxConcurrency`; the "Pending import" example and lifecycle rows now match the ACM
console code (`ManagedClusterImporting` shows `importing`, joined plus Available shows `ready`, a pending import
carries `ManagedClusterWaitForImporting`); the region claim is omitted when unknown, never `Other`; `score`,
`sortBy`, `scoreRateLimit` and `lastScoreUpdateTime` exist only in the API and decisions are always name-sorted;
the claim ordering is explained by ACM's reserved suffixes; "apply resources error ... Will Retry" is a
`ManagedClusterImporting` message; the CEL selector arrived in `api` v1.0.0, not 0.16; ten add-ons on the hub;
klusterlet modes include `Singleton` and `SingletonHosted`; `cloud`/`vendor` are not re-added once removed;
`clusterset=default` comes from OCM's webhook; hub Deployment names carry the `cluster-manager-` prefix; ACM's
`ocm-mutating-webhook` exists beside OCM's (neither covers placements); the `Deleting` condition is
`False/ResourceRemaining` while resources remain and `True/NoResource` when done; the `ManagedClusters` proto
sketch is marked superseded by `proto/ai_grid/draft/v1`. Still not verified: which OCM commit MCE 2.17.3 bundles,
so the upstream-main MWRS behaviour above remains dated by commit.
