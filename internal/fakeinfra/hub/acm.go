package hub

// Ported from the console's fake backend (ai-grid-cloud/ai-grid-provider-console
// 537df38, bff/internal/fakebackend/acm.go). The console version works on its
// draft proto; this one works on the ManagedCluster CR itself, so a condition
// status is the Kubernetes string ("True", "False", "Unknown") and the hub's
// `generation` bump for a spec change happens in Hub.commit, the way the API
// server does it, instead of in each helper.

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	LabelLocalCluster      = "local-cluster"
	LabelAddonPrefix       = "feature.open-cluster-management.io/addon-"
	LabelAutoDetect        = "auto-detect"
	// LabelManagedBy accompanies the OpenShiftDedicated -> OpenShift vendor
	// rewrite (autodetect_controller.go L75-79).
	LabelManagedBy = "managed-by"

	// ClusterSetDefault is the webhook's defaultClusterSetName
	// (managedcluster_mutating.go L25).
	ClusterSetDefault = "default"

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

	// DefaultLeaseDurationSeconds is the CRD default of
	// spec.leaseDurationSeconds (api:cluster/v1/types.go
	// `+kubebuilder:default=60`, recalled, not re-read for this port; every
	// fixture cluster and the console's research example carry 60).
	DefaultLeaseDurationSeconds int32 = 60
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
func findCondition(c *ManagedCluster, typ string) *metav1.Condition {
	if c.Status == nil {
		return nil
	}
	for i := range c.Status.Conditions {
		if c.Status.Conditions[i].Type == typ {
			return &c.Status.Conditions[i]
		}
	}
	return nil
}

// terminating reports whether deletion was requested. Most hub controllers
// stop touching such a cluster (taints, add-on labels, autodetect, the
// clusterset webhook default).
func terminating(c *ManagedCluster) bool { return c.DeletionTimestamp != nil }

// setCondition writes a condition the way a controller does with
// apimachinery's meta.SetStatusCondition: the status, reason and message are
// replaced and lastTransitionTime moves only when the status actually
// changes. observedGeneration is left unset: the hub's ManagedCluster
// conditions never carry it (checked on the dev hub).
func setCondition(c *ManagedCluster, now time.Time, typ string, st metav1.ConditionStatus, reason, message string) {
	if c.Status == nil {
		c.Status = &ManagedClusterStatus{}
	}
	cond := findCondition(c, typ)
	if cond == nil {
		c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{Type: typ, LastTransitionTime: metav1.NewTime(now)})
		cond = &c.Status.Conditions[len(c.Status.Conditions)-1]
	} else if cond.Status != st {
		cond.LastTransitionTime = metav1.NewTime(now)
	}
	cond.Status = st
	cond.Reason = reason
	cond.Message = message
}

// hasTaint reports whether a taint with that key is present.
func hasTaint(c *ManagedCluster, key string) bool {
	return slices.ContainsFunc(c.Spec.Taints, func(t Taint) bool { return t.Key == key })
}

// addTaint adds a hub taint with NoSelect and timeAdded (the mutating
// webhook's stamp, managedcluster_mutating.go L69-105); a second add is a
// no-op like on the hub.
func addTaint(c *ManagedCluster, now time.Time, key string) {
	if hasTaint(c, key) {
		return
	}
	c.Spec.Taints = append(c.Spec.Taints, Taint{Key: key, Effect: TaintNoSelect, TimeAdded: metav1.NewTime(now)})
}

// removeTaint drops every taint with that key.
func removeTaint(c *ManagedCluster, key string) {
	c.Spec.Taints = slices.DeleteFunc(c.Spec.Taints, func(t Taint) bool { return t.Key == key })
	if len(c.Spec.Taints) == 0 {
		c.Spec.Taints = nil
	}
}

// syncTaints is the hub taint controller
// (ocm:pkg/registration/hub/taint/controller.go L67-93): from the Available
// condition it keeps exactly one of the two hub taints, or none, and leaves
// a terminating cluster alone. A taint change is a spec change, so Hub.commit
// bumps `generation` like the API server does.
func syncTaints(c *ManagedCluster, now time.Time) {
	if terminating(c) {
		return
	}
	switch avail := findCondition(c, CondAvailable); {
	case avail == nil || avail.Status == metav1.ConditionUnknown:
		removeTaint(c, TaintUnavailable)
		addTaint(c, now, TaintUnreachable)
	case avail.Status == metav1.ConditionFalse:
		removeTaint(c, TaintUnreachable)
		addTaint(c, now, TaintUnavailable)
	default:
		removeTaint(c, TaintUnavailable)
		removeTaint(c, TaintUnreachable)
	}
}

// addonLabels returns the add-on names and values carried as
// `feature.open-cluster-management.io/addon-*` labels.
func addonLabels(c *ManagedCluster) map[string]string {
	out := map[string]string{}
	for k, v := range c.Labels {
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
func setAddonLabels(c *ManagedCluster, value string) {
	if terminating(c) || findCondition(c, CondAvailable) == nil {
		return
	}
	for k := range c.Labels {
		if strings.HasPrefix(k, LabelAddonPrefix) {
			c.Labels[k] = value
		}
	}
}

// claim returns a claim value or "".
func claim(c *ManagedCluster, name string) string {
	if c.Status == nil {
		return ""
	}
	for _, cl := range c.Status.ClusterClaims {
		if cl.Name == name {
			return cl.Value
		}
	}
	return ""
}

// sortClaims orders the claims by name, as the hub reports them.
func sortClaims(c *ManagedCluster) {
	if c.Status == nil {
		return
	}
	claims := c.Status.ClusterClaims
	sort.SliceStable(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })
}

// autodetectLabels is the foundation autodetect controller
// (foundation:pkg/controllers/clusterinfo/autodetect_controller.go L42-131)
// reading what the agent synced (defaultInfo_sync.go L17-49). It replaces
// `cloud` and `vendor` only while their value is `auto-detect` (L68, L73), so
// a removed label is not added back; it always writes `clusterID` (the
// `id.openshift.io` claim, `id.k8s.io` on MicroShift) and the three
// `openshiftVersion*` labels; it skips a terminating cluster (L49-51).
func autodetectLabels(c *ManagedCluster) {
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
func setDeletingCondition(c *ManagedCluster, now time.Time) {
	addons := addonLabels(c)
	if len(addons) == 0 {
		setCondition(c, now, CondDeleting, metav1.ConditionTrue, ReasonNoResource, MessageNoResource)
		return
	}
	pending := 0
	for name := range addons {
		if addonsWithFinalizer[name] {
			pending++
		}
	}
	setCondition(c, now, CondDeleting, metav1.ConditionFalse, ReasonResourceRemaining,
		fmt.Sprintf(MessageResourceRemainingFmt, GCResourceManagedClusterAddOns, len(addons), pending))
}

// admit is what OCM's ManagedCluster mutating webhook does to every create
// and update before the API server stores it: it sets the default clusterset
// label when the label is empty on a live cluster (managedcluster_mutating.go
// L50-63, for a caller allowed to join the default set, which every token the
// fake hub accepts is) and stamps `timeAdded` on a taint that is new
// (L69-105); a taint that was already there keeps its stamp.
func admit(c, old *ManagedCluster, now time.Time) {
	if !terminating(c) {
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		if c.Labels[LabelClusterSet] == "" {
			c.Labels[LabelClusterSet] = ClusterSetDefault
		}
	}
	for i := range c.Spec.Taints {
		t := &c.Spec.Taints[i]
		if old != nil {
			if j := slices.IndexFunc(old.Spec.Taints, func(o Taint) bool {
				return o.Key == t.Key && o.Value == t.Value && o.Effect == t.Effect
			}); j >= 0 {
				t.TimeAdded = old.Spec.Taints[j].TimeAdded
				continue
			}
		}
		t.TimeAdded = metav1.NewTime(now)
	}
}

// reconcile is what the hub's controllers write shortly after a client
// created or changed a live cluster (on the hub they write within seconds of
// the request; the fake writes immediately after it, as a separate write
// with its own resourceVersion, so the client's response is the object as
// the API server stored it, before the controllers):
//   - the import controller restores `name` and
//     `open-cluster-management/created-via` (managedcluster_controller.go
//     L119-164, L196-217) and adds its cleanup finalizer (L144);
//   - the foundation controllers and the gc controller add their finalizers
//     (see the Finalizer* constants), the registration controller the
//     api-resource-cleanup one once the cluster is accepted;
//   - the registration controller writes HubAcceptedManagedCluster from
//     spec.hubAcceptsClient, True when accepted and False/Denied only when it
//     was True before (controller.go L164-167, L203-208, L264-269);
//   - the import-status controller writes ImportSucceeded
//     False/WaitForImporting when the condition is missing
//     (importstatus_controller.go L85-97);
//   - the discovery controller restores the add-on labels from the add-ons
//     that exist, which in the fake are the add-on labels before the write
//     (discovery_controller.go L99-128), and the autodetect controller
//     re-adds `clusterID` and `openshiftVersion*` but not `cloud` or `vendor`;
//   - the taint controller syncs its taints from Available.
//
// before is the object as it was before the client's write (nil for a
// create). A terminating cluster is left to the detach path.
func reconcile(c, before *ManagedCluster, now time.Time) {
	if terminating(c) {
		return
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	c.Labels[LabelName] = c.Name
	if before != nil && findCondition(c, CondAvailable) != nil {
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

	finalizers := []string{FinalizerImportCleanup, FinalizerClusterInfo, FinalizerResourceCleanup, FinalizerClusterRole}
	if c.Spec.HubAcceptsClient {
		finalizers = append(finalizers, FinalizerClusterAPIResource)
	}
	for _, f := range finalizers {
		if !slices.Contains(c.Finalizers, f) {
			c.Finalizers = append(c.Finalizers, f)
		}
	}

	accepted := findCondition(c, CondHubAccepted)
	switch {
	case c.Spec.HubAcceptsClient:
		setCondition(c, now, CondHubAccepted, metav1.ConditionTrue, ReasonHubAccepted, MessageHubAccepted)
	case accepted != nil && accepted.Status == metav1.ConditionTrue:
		setCondition(c, now, CondHubAccepted, metav1.ConditionFalse, ReasonHubDenied, MessageHubDenied)
	}
	if findCondition(c, CondImportSucceeded) == nil {
		setCondition(c, now, CondImportSucceeded, metav1.ConditionFalse, ReasonWaitForImporting, MessageWaitForImporting)
	}
	syncTaints(c, now)
}

// markDetaching is what the hub's controllers write once deletion was
// requested: the import controller's ImportSucceeded False/Detaching and the
// gc controller's Deleting report. Taints, add-on labels and autodetect
// labels freeze.
func markDetaching(c *ManagedCluster, now time.Time) {
	setCondition(c, now, CondImportSucceeded, metav1.ConditionFalse, ReasonDetaching, MessageDetaching)
	setDeletingCondition(c, now)
}
