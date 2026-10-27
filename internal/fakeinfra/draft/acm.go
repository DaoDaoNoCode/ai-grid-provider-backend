package draft

import (
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Condition status short names used by setCondition and the cluster tests.
const (
	cTrue    = aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE
	cFalse   = aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE
	cUnknown = aigriddraftv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
)

// Strings the hub writes on a ManagedCluster. Each group cites the upstream
// source that writes it (repo:path Lline, clones of open-cluster-management-io
// ocm and api, stolostron managedcluster-import-controller,
// multicloud-operators-foundation and console, checked 2026-10-16); nothing
// here is invented. Keep them verbatim, including upstream typos: the UI and
// the real backend will match on them.
const (
	// Condition types (`status.conditions[].type`).
	CondHubAccepted     = "HubAcceptedManagedCluster"
	CondJoined          = "ManagedClusterJoined"
	CondAvailable       = "ManagedClusterConditionAvailable"
	CondClockSynced     = "ManagedClusterConditionClockSynced"
	CondImportSucceeded = "ManagedClusterImportSucceeded" // import-controller:pkg/constants/constants.go L154
	// Deleting is written by the hub gc controller while it drains the
	// cluster namespace (api:cluster/v1/types.go L299,
	// ocm:pkg/registration/hub/gc/gc_resources.go L67-99).
	CondDeleting = "Deleting"

	// HubAcceptedManagedCluster (ocm:pkg/registration/hub/managedcluster/controller.go
	// L264-269 accepted, L203-208 denied; denial is written only when the
	// condition was True before, L164-167).
	ReasonHubAccepted  = "HubClusterAdminAccepted"
	MessageHubAccepted = "Accepted by hub cluster admin"
	ReasonHubDenied    = "HubClusterAdminDenied"
	MessageHubDenied   = "Denied by hub cluster admin"

	// ManagedClusterJoined (ocm:pkg/registration/spoke/managedcluster/joining_reconcile.go
	// L28-32); never set to False.
	ReasonJoined  = "ManagedClusterJoined"
	MessageJoined = "Managed cluster joined"

	// ManagedClusterConditionAvailable: True and False from the spoke agent
	// (ocm:pkg/registration/spoke/managedcluster/available_reconcile.go
	// L62-88), Unknown from the hub lease controller
	// (ocm:pkg/registration/hub/lease/controller.go L148-153).
	ReasonAvailable           = "ManagedClusterAvailable"
	MessageAvailable          = "Managed cluster is available"
	ReasonAPIServerUnavail    = "ManagedClusterKubeAPIServerUnavailable"
	MessageAPIServerUnavailFm = "The kube-apiserver is not ok, status code: %d, %v"
	ReasonLeaseStopped        = "ManagedClusterLeaseUpdateStopped"
	MessageLeaseStopped       = "Registration agent stopped updating its lease."

	// ManagedClusterConditionClockSynced
	// (ocm:pkg/registration/hub/lease/clocksynccontroller.go L137-149).
	ReasonClockSynced     = "ManagedClusterClockSynced"
	MessageClockSynced    = "The clock of the managed cluster is synced with the hub."
	ReasonClockOutOfSync  = "ManagedClusterClockOutOfSync"
	MessageClockOutOfSync = "The clock of hub and agent is out of sync. This may cause the Unknown status and affect agent functionalities."

	// ManagedClusterImportSucceeded (ACM import controller). WaitForImporting
	// and Imported: import-controller:pkg/controller/importstatus/importstatus_controller.go
	// L90-96, L123-128. Importing and ImportFailed:
	// import-controller:pkg/helpers/autoimport.go L141-237 (the "apply
	// resources error ... Will Retry" text is filed under Importing, L222-227).
	// Detaching: import-controller:pkg/controller/resourcecleanup/resourcecleanup_controller.go L131-138.
	ReasonWaitForImporting     = "ManagedClusterWaitForImporting"
	MessageWaitForImporting    = "Wait for importing"
	ReasonImporting            = "ManagedClusterImporting"
	MessageImportStart         = "Start to import managed cluster"
	MessageImportWaitSecret    = "Wait for import secret"
	MessageImportApplied       = "Importing resources are applied, wait for resources be available"
	MessageImportNoWorks       = "Expect klusterlet manifestworks created, but got none. Will retry"
	MessageImportApplyRetryFmt = "Try to import managed cluster, apply resources error: %v. Will Retry"
	ReasonImportFailed         = "ManagedClusterImportFailed"
	MessageImportFailedFmt     = "Try to import managed cluster, error: %v"
	ReasonImported             = "ManagedClusterImported"
	MessageImported            = "Import succeeded"
	ReasonDetaching            = "ManagedClusterDetaching"
	MessageDetaching           = "The managed cluster is being detached now"
	ReasonForceDetaching       = "ManagedClusterForceDetaching"
	MessageForceDetaching      = "The managed cluster is being detached by force"
	// MessageImportWaitSecretH is the hosted-mode variant
	// (import-controller:pkg/controller/hosted/controller.go L206-210).
	MessageImportWaitSecretH = "Wait for import secret to be created"

	// Deleting reasons and messages (ocm:pkg/registration/hub/gc/gc_resources.go
	// L67-73: False/ResourceRemaining while a resource kind is left, upstream
	// spelling "remaning"; L93-99: True/NoResource once the namespace is
	// clean; ocm:pkg/registration/hub/gc/controller.go L100-105:
	// False/DeletingError with the cleanup error as message). The kinds are
	// drained in the order of the hub's gc-resource-list,
	// managedclusteraddons then manifestworks
	// (ocm:pkg/registration/hub/manager.go L76-77).
	ReasonResourceRemaining        = "ResourceRemaining"
	MessageResourceRemainingFmt    = "The resource %v is remaning, the remaining count is %v, the finalizer pending count is %v"
	ReasonNoResource               = "NoResource"
	MessageNoResource              = "No cleaned resource in cluster ns."
	ReasonDeletingError            = "DeletingError"
	GCResourceManagedClusterAddOns = "managedclusteraddons"

	// Finalizers the hub's controllers add to every live ManagedCluster,
	// joined or not; the manifestwork one only while klusterlet
	// ManifestWorks exist in the cluster namespace.
	FinalizerImportCleanup      = "managedcluster-import-controller.open-cluster-management.io/cleanup"              // import-controller:pkg/constants/constants.go L58, managedcluster_controller.go L144
	FinalizerManifestWork       = "managedcluster-import-controller.open-cluster-management.io/manifestwork-cleanup" // import-controller:pkg/constants/constants.go L141, pkg/helpers/manifestwork.go L34-53
	FinalizerClusterInfo        = "managedclusterinfo.finalizers.open-cluster-management.io"                         // foundation:pkg/controllers/clusterinfo/clusterinfo_controller.go L29, L105-108
	FinalizerClusterRole        = "open-cluster-management.io/managedclusterrole"                                    // foundation:pkg/controllers/clusterrole/clusterrole_controller.go L27, L161-166
	FinalizerResourceCleanup    = "cluster.open-cluster-management.io/resource-cleanup"                              // ocm:pkg/common/helpers/clusters.go L14, pkg/registration/hub/gc/controller.go L80-82
	FinalizerClusterAPIResource = "cluster.open-cluster-management.io/api-resource-cleanup"                          // api:cluster/v1/types.go L294; added once accepted, ocm:pkg/registration/hub/managedcluster/controller.go L244

	// The import controller's created-via annotation, reset to `other` when
	// missing or not one of its known values
	// (import-controller:pkg/controller/managedcluster/managedcluster_controller.go L196-217).
	AnnotationCreatedVia      = "open-cluster-management/created-via"
	AnnotationCreatedViaOther = "other"

	// Taints the hub's taint controller adds, both with effect NoSelect
	// (ocm:pkg/registration/hub/taint/controller.go L23-31). They are
	// mutually exclusive: Available Unknown or missing sets unreachable and
	// removes unavailable, False does the opposite, True removes both
	// (L76-85); a terminating cluster is left alone (L67-69).
	TaintUnavailable = "cluster.open-cluster-management.io/unavailable"
	TaintUnreachable = "cluster.open-cluster-management.io/unreachable"
	TaintNoSelect    = "NoSelect"

	// Labels the hub writes. `name`: the import controller, re-added when
	// missing (import-controller:pkg/controller/managedcluster/managedcluster_controller.go
	// L124-128). `cloud`, `vendor`, `clusterID`, `openshiftVersion*`: the
	// foundation autodetect controller
	// (foundation:pkg/controllers/clusterinfo/autodetect_controller.go L62-123).
	// The clusterset label: OCM's ManagedCluster mutating webhook, whenever it
	// is empty on a live cluster (ocm:pkg/registration/webhook/v1/managedcluster_mutating.go L50-63).
	// `feature.open-cluster-management.io/addon-*`: OCM's add-on discovery
	// controller (ocm:pkg/registration/hub/addon/discovery_controller.go L27-31).
	LabelName              = "name"
	LabelCloud             = "cloud"
	LabelVendor            = "vendor"
	LabelClusterID         = "clusterID"
	LabelOpenShiftVersion  = "openshiftVersion"
	LabelOpenShiftMajor    = "openshiftVersion-major"
	LabelOpenShiftMajorMin = "openshiftVersion-major-minor"
	LabelClusterSet        = "cluster.open-cluster-management.io/clusterset"
	// ClusterSetDefault is the hub's built-in set (acm.md): clusters whose
	// clusterset label is "default". Kept after draft Models/Placement retirement
	// because the ManagedCluster mutating webhook still fills an empty label.
	ClusterSetDefault = "default"
	LabelLocalCluster = "local-cluster"
	LabelAddonPrefix  = "feature.open-cluster-management.io/addon-"
	LabelAutoDetect   = "auto-detect"
	// LabelManagedBy accompanies the OpenShiftDedicated -> OpenShift vendor
	// rewrite (autodetect_controller.go L75-79).
	LabelManagedBy = "managed-by"

	// Values of the addon labels, mirroring each ManagedClusterAddOn's
	// Available condition: True available, False unhealthy, Unknown or
	// missing unreachable (discovery_controller.go L140-154). A new add-on
	// reads unreachable until its agent first reports.
	AddonAvailable   = "available"
	AddonUnhealthy   = "unhealthy"
	AddonUnreachable = "unreachable"

	// Claims the console and the autodetect controller read
	// (foundation:pkg/klusterlet/clusterclaim/clusterclaimer.go L31-49).
	ClaimID          = "id.k8s.io"
	ClaimOpenShiftID = "id.openshift.io"
	ClaimPlatform    = "platform.open-cluster-management.io"
	ClaimProduct     = "product.open-cluster-management.io"
	ClaimOCPVersion  = "version.openshift.io"
	ClaimKubeVersion = "kubeversion.open-cluster-management.io"
	ClaimConsoleURL  = "consoleurl.cluster.open-cluster-management.io"
)

// defaultAddons are the seven ACM add-ons every enrolled fixture cluster
// carries; their labels appear after import as each agent reports. The dev
// hub's local-cluster lists ten (`oc get managedclusteraddon -n
// local-cluster`, 2026-10-16): these seven plus hypershift-addon,
// multicluster-observability-addon and submariner, which need extra
// operators or configuration.
var defaultAddons = []string{
	"application-manager",
	"cert-policy-controller",
	"cluster-proxy",
	"config-policy-controller",
	"governance-policy-framework",
	"managed-serviceaccount",
	"work-manager",
}

// addonsWithFinalizer are the add-ons whose ManagedClusterAddOn carries a
// finalizer on the dev hub (2026-10-16: config-policy-controller and
// hypershift-addon `addon.open-cluster-management.io/addon-pre-delete`,
// submariner `submarineraddon.open-cluster-management.io/submariner-addon-cleanup`).
// The gc controller reports them as "the finalizer pending count".
var addonsWithFinalizer = map[string]bool{"config-policy-controller": true, "hypershift-addon": true, "submariner": true}

// platformToCloud maps the platform claim to the `cloud` label
// (foundation:pkg/klusterlet/clusterinfo/defaultInfo_sync.go L51-81, values
// from cluster-lifecycle-api:clusterinfo/v1beta1/types.go L267-291); any other
// platform, KubeVirt included, is `Other`.
var platformToCloud = map[string]string{
	"AWS": "Amazon", "GCP": "Google", "Azure": "Azure", "IBM": "IBM", "IBMPowerPlatform": "IBMPowerPlatform",
	"IBMZPlatform": "IBMZPlatform", "OpenStack": "OpenStack", "VSphere": "VSphere", "RHV": "RHV",
	"AlibabaCloud": "AlibabaCloud", "BareMetal": "BareMetal", "Nutanix": "Nutanix",
}

// productToVendor maps the product claim to the `vendor` label
// (defaultInfo_sync.go L82-114). Every OpenShift flavour reports `OpenShift`;
// OpenShiftDedicated is rewritten by the autodetect controller with
// `managed-by=platform` (autodetect_controller.go L75-79). Anything else is
// `Other`.
var productToVendor = map[string]string{
	"OpenShift": "OpenShift", "OpenShiftDedicated": "OpenShift", "ROSA": "OpenShift", "ARO": "OpenShift", "ROKS": "OpenShift",
	"MicroShift": "MicroShift", "AKS": "AKS", "EKS": "EKS", "GKE": "GKE", "IKS": "IKS", "ICP": "ICP",
}

// findCondition returns the condition of the given type, or nil.
func findCondition(c *aigriddraftv1.ManagedCluster, typ string) *aigriddraftv1.Condition {
	for _, cond := range c.GetStatus().GetConditions() {
		if cond.GetType() == typ {
			return cond
		}
	}
	return nil
}

// isTrue mirrors the ACM console's checkForCondition
// (console:frontend/src/resources/utils/status-conditions.ts L5-9): True
// only when the condition exists and its status is True.
func isTrue(c *aigriddraftv1.ManagedCluster, typ string) bool {
	return findCondition(c, typ).GetStatus() == aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE
}

// terminating reports whether deletion was requested. Most hub controllers
// stop touching such a cluster (taints, add-on labels, autodetect, the
// clusterset webhook default).
func terminating(c *aigriddraftv1.ManagedCluster) bool { return c.GetDeletionTimestamp() != nil }

// setCondition writes a condition the way a controller does with
// apimachinery's meta.SetStatusCondition: the status, reason and message are
// replaced and lastTransitionTime moves only when the status actually
// changes. observedGeneration is left unset: the hub's ManagedCluster
// conditions never carry it (checked on the dev hub).
func setCondition(c *aigriddraftv1.ManagedCluster, now time.Time, typ string, st aigriddraftv1.ConditionStatus, reason, message string) {
	if c.Status == nil {
		c.Status = &aigriddraftv1.ManagedClusterStatus{}
	}
	cond := findCondition(c, typ)
	if cond == nil {
		cond = &aigriddraftv1.Condition{Type: typ, LastTransitionTime: timestamppb.New(now)}
		c.Status.Conditions = append(c.Status.Conditions, cond)
	} else if cond.GetStatus() != st {
		cond.LastTransitionTime = timestamppb.New(now)
	}
	cond.Status = st
	cond.Reason = proto.String(reason)
	cond.Message = proto.String(message)
}

// hasTaint reports whether a taint with that key is present.
func hasTaint(c *aigriddraftv1.ManagedCluster, key string) bool {
	for _, t := range c.GetTaints() {
		if t.GetKey() == key {
			return true
		}
	}
	return false
}

// addTaint adds a hub taint with NoSelect and timeAdded (the mutating
// webhook's stamp, managedcluster_mutating.go L69-105); a second add is a
// no-op like on the hub. It reports whether the spec changed.
func addTaint(c *aigriddraftv1.ManagedCluster, now time.Time, key string) bool {
	if hasTaint(c, key) {
		return false
	}
	c.Taints = append(c.Taints, &aigriddraftv1.Taint{Key: key, Effect: TaintNoSelect, TimeAdded: timestamppb.New(now)})
	return true
}

// removeTaint drops every taint with that key and reports whether one was
// there.
func removeTaint(c *aigriddraftv1.ManagedCluster, key string) bool {
	kept := c.GetTaints()[:0]
	for _, t := range c.GetTaints() {
		if t.GetKey() != key {
			kept = append(kept, t)
		}
	}
	removed := len(kept) != len(c.GetTaints())
	c.Taints = kept
	return removed
}

// syncTaints is the hub taint controller
// (ocm:pkg/registration/hub/taint/controller.go L67-93): from the Available
// condition it keeps exactly one of the two hub taints, or none, and leaves
// a terminating cluster alone. A taint change is a spec patch, so it bumps
// `generation` like the API server does for a spec change.
func syncTaints(c *aigriddraftv1.ManagedCluster, now time.Time) {
	if terminating(c) {
		return
	}
	var changed bool
	switch avail := findCondition(c, CondAvailable); {
	case avail == nil || avail.GetStatus() == aigriddraftv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED:
		changed = removeTaint(c, TaintUnavailable)
		changed = addTaint(c, now, TaintUnreachable) || changed
	case avail.GetStatus() == aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE:
		changed = removeTaint(c, TaintUnreachable)
		changed = addTaint(c, now, TaintUnavailable) || changed
	default:
		changed = removeTaint(c, TaintUnavailable)
		changed = removeTaint(c, TaintUnreachable) || changed
	}
	if changed {
		c.Generation++
	}
}

// addonLabels returns the add-on names and values carried as
// `feature.open-cluster-management.io/addon-*` labels.
func addonLabels(c *aigriddraftv1.ManagedCluster) map[string]string {
	out := map[string]string{}
	for k, v := range c.GetLabels() {
		if name, ok := strings.CutPrefix(k, LabelAddonPrefix); ok {
			out[name] = v
		}
	}
	return out
}

// setAddonLabels sets every add-on label to value, as the discovery
// controller does when every add-on's Available condition moves together:
// Unknown when the hub's add-on health check sees the cluster Unknown
// (ocm:pkg/registration/hub/addon/healthcheck_controller.go L65-105, which
// leaves add-ons alone when Available is False), True again when the agents
// report. The discovery controller skips a terminating cluster and one with
// no Available condition yet (discovery_controller.go L89-97).
func setAddonLabels(c *aigriddraftv1.ManagedCluster, value string) {
	if terminating(c) || findCondition(c, CondAvailable) == nil {
		return
	}
	for k := range c.GetLabels() {
		if strings.HasPrefix(k, LabelAddonPrefix) {
			c.Labels[k] = value
		}
	}
}

// claim returns a claim value or "".
func claim(c *aigriddraftv1.ManagedCluster, name string) string {
	for _, cl := range c.GetStatus().GetClusterClaims() {
		if cl.GetName() == name {
			return cl.GetValue()
		}
	}
	return ""
}

// sortClaims orders the claims by name, as the hub reports them.
func sortClaims(c *aigriddraftv1.ManagedCluster) {
	claims := c.GetStatus().GetClusterClaims()
	sort.SliceStable(claims, func(i, j int) bool { return claims[i].GetName() < claims[j].GetName() })
}

// autodetectLabels is the foundation autodetect controller
// (foundation:pkg/controllers/clusterinfo/autodetect_controller.go L42-131)
// reading what the agent synced (defaultInfo_sync.go L17-49). It replaces
// `cloud` and `vendor` only while their value is `auto-detect` (L68, L73), so
// a removed label is not added back; it always writes `clusterID` (the
// `id.openshift.io` claim, `id.k8s.io` on MicroShift) and the three
// `openshiftVersion*` labels; it skips a terminating cluster (L49-51).
func autodetectLabels(c *aigriddraftv1.ManagedCluster) {
	if terminating(c) {
		return
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	if p := claim(c, ClaimPlatform); p != "" && c.Labels[LabelCloud] == LabelAutoDetect {
		c.Labels[LabelCloud] = valueOr(platformToCloud, p, "Other")
	}
	product := claim(c, ClaimProduct)
	if product != "" && c.Labels[LabelVendor] == LabelAutoDetect {
		c.Labels[LabelVendor] = valueOr(productToVendor, product, "Other")
		if product == "OpenShiftDedicated" {
			c.Labels[LabelManagedBy] = "platform"
		}
	}
	id := claim(c, ClaimOpenShiftID)
	if product == "MicroShift" {
		id = claim(c, ClaimID)
	}
	if id != "" {
		c.Labels[LabelClusterID] = id
	}
	if v := claim(c, ClaimOCPVersion); v != "" {
		c.Labels[LabelOpenShiftVersion] = v
		parts := strings.Split(v, ".")
		c.Labels[LabelOpenShiftMajor] = parts[0]
		if len(parts) >= 2 {
			c.Labels[LabelOpenShiftMajorMin] = parts[0] + "." + parts[1]
		}
	}
}

func valueOr(m map[string]string, k, def string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

// setDeletingCondition is the hub gc controller's report on a terminating
// cluster (ocm:pkg/registration/hub/gc/gc_resources.go L48-102). The fake
// counts the add-on labels as the ManagedClusterAddOns still in the cluster
// namespace (the first kind drained); with none left it reports the namespace
// clean. It does not model the ManifestWorks drained after them.
func setDeletingCondition(c *aigriddraftv1.ManagedCluster, now time.Time) {
	addons := addonLabels(c)
	if len(addons) == 0 {
		setCondition(c, now, CondDeleting, aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonNoResource, MessageNoResource)
		return
	}
	pending := 0
	for name := range addons {
		if addonsWithFinalizer[name] {
			pending++
		}
	}
	setCondition(c, now, CondDeleting, aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonResourceRemaining,
		fmt.Sprintf(MessageResourceRemainingFmt, GCResourceManagedClusterAddOns, len(addons), pending))
}

// reconcileMetadata applies what the hub's webhook and controllers do after
// a client rewrites the labels or annotations of a live cluster: the
// mutating webhook sets the default clusterset label when it is empty
// (managedcluster_mutating.go L50-63, for a caller allowed to join the
// default set, which every token the fake hub accepts is); the import
// controller restores `name` and `open-cluster-management/created-via`
// (managedcluster_controller.go L119-164, L196-217); the discovery controller
// restores the add-on labels from the add-ons that exist, which in the fake
// are the add-on labels before the write (discovery_controller.go L99-128);
// the autodetect controller re-adds `clusterID` and `openshiftVersion*` but
// not `cloud` or `vendor`. On the hub the controllers write shortly after the
// request; the fake applies them before it answers.
func reconcileMetadata(c *aigriddraftv1.ManagedCluster, before *aigriddraftv1.ManagedCluster) {
	if terminating(c) {
		return
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	if c.Labels[LabelClusterSet] == "" {
		c.Labels[LabelClusterSet] = ClusterSetDefault // defaultClusterSetName, managedcluster_mutating.go L25
	}
	c.Labels[LabelName] = c.GetName()
	if findCondition(c, CondAvailable) != nil {
		maps.DeleteFunc(c.Labels, func(k, _ string) bool { return strings.HasPrefix(k, LabelAddonPrefix) })
		for name, v := range addonLabels(before) {
			c.Labels[LabelAddonPrefix+name] = v
		}
	}
	autodetectLabels(c)
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	switch c.Annotations[AnnotationCreatedVia] {
	case "assisted-installer", "hive", "discovery", "hypershift":
	default:
		c.Annotations[AnnotationCreatedVia] = AnnotationCreatedViaOther
	}
}
