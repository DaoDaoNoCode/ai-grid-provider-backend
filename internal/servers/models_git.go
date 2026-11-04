package servers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"github.com/osac-project/ai-grid-provider-backend/internal/names"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// In git mode each model is two directories under catalog/models/<id>/, and
// Argo CD delivers them through two kinds of index:
//
//   - deployment/: the LLMInferenceService and its NetworkPolicy, in the
//     models namespace. catalog/clusters/<name>/kustomization.yaml lists
//     the deployments placed on the ManagedCluster <name>.
//   - catalog/: the ExternalModel and MaaSModelRef that publish the model
//     through the grid, in the subscription namespace.
//     catalog/ingress/kustomization.yaml lists every model's, for the ingress
//     spokes.
//
// Index files are never deleted, so a cluster or the ingress keeps its path
// when its last model goes, and Argo CD prunes instead of failing.
//
// The indexes are shared: writes to two different models at once both
// rewrite the ingress index, so the second commit conflicts. A write that
// reads an index and leaves it as it is has it in its read set, so a change
// to it since the read conflicts as well. Create, Update
// and Delete rerun the whole write on the new head (gitops.RetryOnConflict),
// which succeeds unless the model itself changed in a way the caller's
// version or checks refuse. Nothing before the commit writes anything:
// checkPlacementClusters only lists the hub's ManagedClusters, so a rerun may
// list them again.
const (
	modelsGitDir    = "catalog/models"
	clustersGitDir  = "catalog/clusters"
	ingressGitIndex = "catalog/ingress/kustomization.yaml"

	// gitClustersAnnotation records the model's clusters on its
	// LLMInferenceService, so a read returns the placement without scanning
	// every cluster index.
	gitClustersAnnotation = "ai-grid/clusters"
	// legacyModelNameAnnotation held the caller's spec.model.name in the
	// first builds that served a model as its id. The backend no longer
	// writes it: a read drops it, so the next update removes it from Git.
	legacyModelNameAnnotation = "ai-grid/model-name"
	// gridProviderName is the ExternalProvider that publishes the grid
	// gateway to MaaS on each ingress spoke.
	gridProviderName = "grid-provider"
	// clusterNameLabelKey is the label ACM sets on every ManagedCluster to
	// its name.
	clusterNameLabelKey = "name"
)

func modelDeploymentGitDir(id string) string { return path.Join(modelsGitDir, id, "deployment") }
func modelCatalogGitDir(id string) string    { return path.Join(modelsGitDir, id, "catalog") }
func clusterGitIndex(cluster string) string {
	return path.Join(clustersGitDir, cluster, "kustomization.yaml")
}

// gitModel is a model read from its files, with its clusters.
type gitModel struct {
	model    *aigridv1.Model
	clusters []string
}

func (s *ModelsServer) gitList(ctx context.Context) ([]*aigridv1.Model, error) {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("list", "", err)
	}
	resources, err := readKustomizationResources(head, ingressGitIndex)
	if err != nil {
		return nil, err
	}
	items := make([]*aigridv1.Model, 0, len(resources))
	reports := s.readModelReports(ctx)
	for _, resource := range resources {
		// ../models/<id>/catalog
		id := path.Base(path.Dir(resource))
		current, err := s.readGitModel(head, id)
		if err != nil {
			return nil, err
		}
		reports.apply(current)
		items = append(items, current.model)
	}
	return items, nil
}

func (s *ModelsServer) gitGet(ctx context.Context, id string) (*aigridv1.Model, error) {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("get", id, err)
	}
	current, err := s.readGitModel(head, id)
	if err != nil {
		return nil, err
	}
	s.readModelReports(ctx).apply(current)
	return current.model, nil
}

func (s *ModelsServer) gitCreate(ctx context.Context, object *aigridv1.Model) (*aigridv1.Model, error) {
	id := object.GetId()
	if err := validateGitMetadata(object.GetMetadata()); err != nil {
		return nil, err
	}
	clusters, err := gitPlacementClusters(object.GetSpec())
	if err != nil {
		return nil, err
	}
	var created *aigridv1.Model
	err = decideOnHead(ctx, s.writer, func(err error) error { return modelGitError("create", id, err) }, func(head gitops.Snapshot) error {
		if _, err := head.Read(path.Join(modelDeploymentGitDir(id), "deployment.yaml")); err == nil {
			return connect.Errorf(connect.CodeAlreadyExists, "Model %q already exists", id)
		} else if !errors.Is(err, gitops.ErrNotFound) {
			return modelGitError("create", id, err)
		}
		if err := s.checkPlacementClusters(ctx, clusters); err != nil {
			return err
		}
		var err error
		created, err = s.gitCommitModel(ctx, head, "create", proto.CloneOf(object), 1, nil, clusters)
		return err
	})
	return created, err
}

func (s *ModelsServer) gitUpdate(ctx context.Context, req *aigridv1.ModelsUpdateRequest) (*aigridv1.Model, error) {
	object := req.GetObject()
	id := modelID(object)
	version := object.GetMetadata().GetVersion()
	if req.GetLock() && version == 0 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "lock requires metadata.version")
	}
	paths, err := parseModelUpdateMask(req.GetUpdateMask())
	if err != nil {
		return nil, err
	}
	var updated *aigridv1.Model
	err = decideOnHead(ctx, s.writer, func(err error) error { return modelGitError("update", id, err) }, func(head gitops.Snapshot) (err error) {
		updated, err = s.gitUpdateOn(ctx, head, proto.CloneOf(object), version, paths)
		return err
	})
	return updated, err
}

// gitUpdateOn decides and commits an update of object, at version (0 for
// none), to the model as head has it.
func (s *ModelsServer) gitUpdateOn(ctx context.Context, head gitops.Snapshot, object *aigridv1.Model, version int32, paths modelUpdatePaths) (*aigridv1.Model, error) {
	id := modelID(object)
	current, err := s.readGitModel(head, id)
	if err != nil {
		return nil, err
	}
	if version != 0 && version != current.model.GetMetadata().GetVersion() {
		return nil, staleModelError("update", id)
	}

	merged, err := mergeModelUpdate(current.model, object, paths)
	if err != nil {
		return nil, err
	}
	merged.Status = nil
	if err := validateGitMetadata(merged.GetMetadata()); err != nil {
		return nil, err
	}
	normalizeModelResources(merged.GetSpec().GetMain())
	if err := validateModelSpec(merged.GetSpec()); err != nil {
		return nil, err
	}
	servedNameOnUpdate(current.model, merged)
	if err := normalizeServedName(id, merged.GetSpec()); err != nil {
		return nil, err
	}
	clusters, err := gitPlacementClusters(merged.GetSpec())
	if err != nil {
		return nil, err
	}
	var added []string
	for _, cluster := range clusters {
		if !slices.Contains(current.clusters, cluster) {
			added = append(added, cluster)
		}
	}
	if err := s.checkPlacementClusters(ctx, added); err != nil {
		return nil, err
	}
	return s.gitCommitModel(ctx, head, "update", merged, current.model.GetMetadata().GetVersion()+1, current.clusters, clusters)
}

// gitDelete removes the model's files and its index entries. A nonzero
// version must equal the model's, or the delete is Aborted. It is refused while
// a subscription in the repository names the model, and the subscriptions
// directory is in the commit's read set, so a subscription written since the
// check makes the commit conflict, and Delete's rerun checks again. So are the
// model's two directories, whose listing decides which files go, and the
// indexes it reads (putIndexChanges).
func (s *ModelsServer) gitDelete(ctx context.Context, id string, version int32) error {
	return decideOnHead(ctx, s.writer, func(err error) error { return modelGitError("delete", id, err) }, func(head gitops.Snapshot) error {
		return s.gitDeleteOn(ctx, head, id, version)
	})
}

// gitDeleteOn decides and commits the delete of model id, at version (0 for
// none), as head has it.
func (s *ModelsServer) gitDeleteOn(ctx context.Context, head gitops.Snapshot, id string, version int32) error {
	current, err := s.readGitModel(head, id)
	if err != nil {
		return err
	}
	if version != 0 && version != current.model.GetMetadata().GetVersion() {
		return staleModelError("delete", id)
	}
	if err := s.gitEnsureNotReferenced(head, id); err != nil {
		return err
	}

	change := gitops.Change{Put: map[string][]byte{}, Read: []string{subscriptionsGitDir}, Message: "models: delete " + id}
	for _, dir := range []string{modelDeploymentGitDir(id), modelCatalogGitDir(id)} {
		names, err := head.List(dir)
		if err != nil {
			return modelGitError("delete", id, err)
		}
		for _, name := range names {
			change.Delete = append(change.Delete, path.Join(dir, name))
		}
		change.Read = append(change.Read, dir)
	}
	if err := putIndexChanges(head, &change, id, current.clusters, nil, false); err != nil {
		return err
	}
	if _, err := s.writer.Commit(ctx, head.Revision(), change); err != nil {
		return modelGitError("delete", id, err)
	}
	return nil
}

// gitCommitModel writes the model's files at version, moves it from the
// cluster indexes in from to those in to, and returns the model as written.
func (s *ModelsServer) gitCommitModel(ctx context.Context, head gitops.Snapshot, op string, object *aigridv1.Model, version int32, from, to []string) (*aigridv1.Model, error) {
	id := object.GetId()
	change := gitops.Change{Put: map[string][]byte{}, Message: "models: " + op + " " + id}

	deployment, err := s.renderModelDeployment(object, version, to)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "render model %q: %v", id, err).WithCause(err)
	}
	catalog, err := s.renderModelCatalog(object)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "render model %q: %v", id, err).WithCause(err)
	}
	change.Put[path.Join(modelDeploymentGitDir(id), "deployment.yaml")] = deployment
	change.Put[path.Join(modelDeploymentGitDir(id), "kustomization.yaml")] = renderKustomization([]string{"deployment.yaml"})
	change.Put[path.Join(modelCatalogGitDir(id), "model.yaml")] = catalog
	change.Put[path.Join(modelCatalogGitDir(id), "kustomization.yaml")] = renderKustomization([]string{"model.yaml"})
	if err := putIndexChanges(head, &change, id, from, to, true); err != nil {
		return nil, err
	}

	if _, err := s.writer.Commit(ctx, head.Revision(), change); err != nil {
		return nil, modelGitError(op, id, err)
	}
	written, err := parseGitModel(deployment)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read back model %q: %v", id, err).WithCause(err)
	}
	s.readModelReports(ctx).apply(written)
	return written.model, nil
}

// modelReports is what the catalog's Applications report about models, all
// named by model id: inference holds each cluster's LLMInferenceServices
// (the models ApplicationSet), refs and externals each cluster's
// MaaSModelRefs and ExternalModels (the ingress ApplicationSet).
//
// ingressClusters are the clusters the hub labels as ingress: ai-grid/enabled
// "true" and ai-grid/role ingress or combined, the same labels
// ListPlacementClusters reads for inference and combined. Each should report
// the MaaSModelRef whether or not it has an ingress Application yet, so an
// ingress cluster Argo CD hasn't reached, or Applications that can't be read,
// don't count as ready.
type modelReports struct {
	inference, refs, externals ObjectReports
	ingressClusters            []string
	// clusters holds what the hub says about each ManagedCluster, for the
	// View link (viewLink).
	clusters map[string]clusterFacts
	// namespace is the models namespace, for the View link.
	namespace string
}

// clusterFacts is what the View link needs from a ManagedCluster.
type clusterFacts struct {
	role       string
	consoleURL string
	// openShiftAIURL is the ai-grid/openshift-ai-url annotation, for a
	// dashboard host that isn't rh-ai.<apps domain>.
	openShiftAIURL string
}

func (s *ModelsServer) readModelReports(ctx context.Context) modelReports {
	if s.status == nil {
		return modelReports{}
	}
	ingress, clusters := s.clusterFacts(ctx)
	return modelReports{
		inference:       readReports(ctx, s.logger, s.status, ModelsSet, kindLLMInferenceService),
		refs:            readReports(ctx, s.logger, s.status, IngressSet, kindMaaSModelRef),
		externals:       readReports(ctx, s.logger, s.status, IngressSet, kindExternalModel),
		ingressClusters: ingress,
		clusters:        clusters,
		namespace:       s.namespace,
	}
}

// clusterFacts lists the clusters labelled ai-grid/enabled "true" and
// ai-grid/role ingress or combined, and what the View link needs from every
// ManagedCluster. Without a dynamic client, or when the list fails (logged),
// it knows none: only the clusters with an ingress Application count as
// ingress, and no cluster has a View link.
func (s *ModelsServer) clusterFacts(ctx context.Context) ([]string, map[string]clusterFacts) {
	if s.client == nil {
		return nil, nil
	}
	items, err := s.listManagedClusters(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "list clusters for model status", "error", err)
		return nil, nil
	}
	var ingress []string
	facts := make(map[string]clusterFacts, len(items))
	for i := range items {
		labels := items[i].GetLabels()
		role := labels["ai-grid/role"]
		if labels["ai-grid/enabled"] == "true" && (role == "ingress" || role == "combined") {
			ingress = append(ingress, items[i].GetName())
		}
		facts[items[i].GetName()] = clusterFacts{
			role:           role,
			consoleURL:     managedClusterClaim(items[i], consoleURLClaim),
			openShiftAIURL: items[i].GetAnnotations()[openShiftAIURLAnnotation],
		}
	}
	return ingress, facts
}

const (
	// consoleURLClaim is the OpenShift console URL a managed cluster's agent
	// reports, such as https://console-openshift-console.apps.<domain>.
	consoleURLClaim = "consoleurl.cluster.open-cluster-management.io"
	// openShiftAIURLAnnotation overrides the OpenShift AI dashboard's base
	// URL on a ManagedCluster whose dashboard isn't at rh-ai.<apps domain>.
	openShiftAIURLAnnotation = "ai-grid/openshift-ai-url"
	// consoleHostPrefix is the OpenShift console's default host name, in
	// front of the cluster's apps domain.
	consoleHostPrefix = "console-openshift-console."
)

// managedClusterClaim returns a ManagedCluster's status.clusterClaims value for name.
func managedClusterClaim(item unstructured.Unstructured, name string) string {
	claims, _, _ := unstructured.NestedSlice(item.Object, "status", "clusterClaims")
	for _, raw := range claims {
		claim, _ := raw.(map[string]interface{})
		if n, _ := claim["name"].(string); n == name {
			value, _ := claim["value"].(string)
			return value
		}
	}
	return ""
}

// viewLink is where to see a model on a cluster. On an ingress or combined
// cluster, OpenShift AI's model deployments for the models namespace:
// https://rh-ai.<apps domain>/ai-hub/models/deployments/internal/<namespace>
// (opendatahub-operator's default dashboard subdomain, odh-dashboard
// deploymentsPaths.ts), or under the ai-grid/openshift-ai-url annotation's
// base URL. Inference clusters run no dashboard (cluster-gitops
// apps/rhoai/roles/inference.yaml), so there the link is the
// LLMInferenceService in the OpenShift console. The claim and the
// annotation come from the cluster, so only an https URL with a host and no
// user info is used (httpsBase); anything else gives no link.
func viewLink(facts clusterFacts, namespace, model string) (string, aigridv1.ModelViewTarget) {
	console, consoleOK := httpsBase(facts.consoleURL)
	if facts.role == "ingress" || facts.role == "combined" {
		base, ok := httpsBase(facts.openShiftAIURL)
		if domain, found := strings.CutPrefix(console.Host, consoleHostPrefix); !ok && consoleOK && found && domain != "" {
			base, ok = url.URL{Scheme: "https", Host: "rh-ai." + domain}, true
		}
		if ok {
			base.Path = path.Join(base.Path, "/ai-hub/models/deployments/internal", namespace)
			return base.String(), aigridv1.ModelViewTarget_MODEL_VIEW_TARGET_OPENSHIFT_AI
		}
	}
	if !consoleOK {
		return "", aigridv1.ModelViewTarget_MODEL_VIEW_TARGET_UNSPECIFIED
	}
	console.Path = path.Join(console.Path, "/k8s/ns", namespace, "serving.kserve.io~v1alpha2~LLMInferenceService", model)
	return console.String(), aigridv1.ModelViewTarget_MODEL_VIEW_TARGET_OPENSHIFT_CONSOLE
}

// httpsBase parses raw as the base of a link: an https URL with a host, no
// user info, query or fragment. Anything else, such as a javascript: or
// http URL, isn't one.
func httpsBase(raw string) (url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return url.URL{}, false
	}
	return url.URL{Scheme: "https", Host: u.Host, Path: strings.TrimSuffix(u.Path, "/")}, true
}

// kindReport is one cluster's report about one of the model's objects.
type kindReport struct {
	kind   string
	report ObjectReport
	found  bool
}

// state is what the report says for the model's phase, and whether there is
// one. A MaaSModelRef that waits only for a subscription counts as ready: the model
// is published and running, and status.maas says no one can call it yet.
func (r kindReport) state() (state ObjectState, found bool) {
	if !r.found {
		return StatePending, false
	}
	if r.report.NoSubscription {
		return StateReady, true
	}
	return r.report.State, true
}

// apply sets the model's status from the reports. Each cluster the file
// places the model on should report its LLMInferenceService, and each ingress
// cluster (one with an ingress Application, or r.ingressClusters) its
// MaaSModelRef and ExternalModel; a combined cluster reports all three. A
// cluster is failed when one of its reports is failed (Degraded, or its sync
// failed), not ready when one is pending, not reporting when one is missing,
// and otherwise ready; a MaaSModelRef waiting only for a subscription counts
// as ready here. status.message summarises every cluster and adds the message
// of each failed object and each one whose sync Argo CD is retrying;
// status.clusters holds each placed one (ModelClusterStatus), with its
// deployment state, and status.ingress each ingress one, with its MaaS state.
// The model is, in the console's order of precedence (ModelPhase in
// model_type.proto): READY when every cluster is ready; PARTIALLY_AVAILABLE
// when every ingress cluster is ready and some, not all, placed clusters are;
// FAILED when any cluster is failed; PENDING with reason Stopped when every
// placed cluster reports it stopped and synced; otherwise PENDING (reason
// NotReady). A placed cluster where it is stopped has reason Stopped. All
// but FAILED need at least one ingress cluster: with none, nothing shows the
// model published, so it is PENDING and the message says so.
// status.deployment and status.maas sum up the two sides apart. With no
// report at all it stays saved to Git, with no clusters.
func (r modelReports) apply(current *gitModel) {
	id := current.model.GetId()
	placed := current.clusters
	clusters := slices.Clone(placed)
	for cluster := range r.refs {
		clusters = append(clusters, cluster)
	}
	clusters = append(clusters, r.ingressClusters...)
	slices.Sort(clusters)
	clusters = slices.Compact(clusters)

	var ready, notReady, stopped, failed, noReport, failures []string
	// An ingress-only cluster publishes the model through MaaS and doesn't
	// serve it, so the message names it apart from the placed clusters.
	var published, notPublished, publishFailed []string
	var placedStatus []*aigridv1.ModelClusterStatus
	var ingressStatus []*aigridv1.ModelIngressStatus
	placedReady, ingressReady, anyIngress := 0, true, false
	reported := false
	for _, cluster := range clusters {
		isPlaced := slices.Contains(placed, cluster)
		_, isIngress := r.refs[cluster]
		isIngress = isIngress || slices.Contains(r.ingressClusters, cluster)
		anyIngress = anyIngress || isIngress
		var reports []kindReport
		expect := func(objects ObjectReports, kind string) kindReport {
			report, found := objects[cluster][id]
			reported = reported || found
			if found && (report.State == StateFailed || report.Retrying) && report.Message != "" {
				failures = append(failures, fmt.Sprintf("%s on %s: %s", kind, cluster, report.Message))
			}
			kr := kindReport{kind: kind, report: report, found: found}
			reports = append(reports, kr)
			return kr
		}
		var inference kindReport
		if isPlaced {
			inference = expect(r.inference, kindLLMInferenceService)
		}
		if isIngress {
			ref := expect(r.refs, kindMaaSModelRef)
			external := expect(r.externals, kindExternalModel)
			ingressStatus = append(ingressStatus, maasStatus(cluster, ref, external))
			if !ingressObjectsReady(ref, external) {
				ingressReady = false
			}
		}
		state := clusterState(reports)
		if !isPlaced {
			switch state {
			case "Failed":
				publishFailed = append(publishFailed, cluster)
			case "Ready":
				published = append(published, cluster)
			default:
				notPublished = append(notPublished, cluster)
			}
		}
		switch {
		case state == "Failed":
			failed = append(failed, cluster)
		case state == "NotReady" && inference.found && inference.report.Live == LiveStopped:
			stopped = append(stopped, cluster)
		case state == "NotReady":
			notReady = append(notReady, cluster)
		case state == "NoReport":
			noReport = append(noReport, cluster)
		default:
			ready = append(ready, cluster)
		}
		if !isPlaced {
			continue
		}
		entry := &aigridv1.ModelClusterStatus{
			Cluster: cluster,
			Phase:   aigridv1.ModelPhase_MODEL_PHASE_PENDING,
			Reason:  state,
			Message: clusterReports(reports),
		}
		entry.Deployment, entry.DeploymentMessage = deploymentState(inference)
		_, hasApp := r.inference[cluster]
		entry.Sync, entry.SyncMessage = syncState(inference, hasApp)
		if entry.Deployment == aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STOPPED && state == "NotReady" {
			entry.Reason = "Stopped"
		}
		entry.State = clusterRollout(entry)
		entry.ViewUrl, entry.ViewTarget = viewLink(r.clusters[cluster], r.namespace, id)
		switch state {
		case "Ready":
			entry.Phase = aigridv1.ModelPhase_MODEL_PHASE_READY
			placedReady++
		case "Failed":
			entry.Phase = aigridv1.ModelPhase_MODEL_PHASE_FAILED
		}
		placedStatus = append(placedStatus, entry)
	}
	if !reported {
		return
	}
	status := current.model.Status
	onPlaced := func(clusters []string) []string {
		return slices.DeleteFunc(slices.Clone(clusters), func(c string) bool { return !slices.Contains(placed, c) })
	}
	status.Message = withPublished(
		withStopped(clusterSummary(onPlaced(failed), onPlaced(notReady), onPlaced(ready), onPlaced(noReport)), stopped),
		publishFailed, notPublished, published)
	if !anyIngress {
		status.Message += "; no ingress cluster has reported the MaaSModelRef"
	}
	if len(failures) > 0 {
		status.Message += ". " + strings.Join(failures, "; ")
	}
	status.Clusters = placedStatus
	status.Ingress = ingressStatus
	status.Deployment = deploymentSummary(placedStatus)
	status.State = modelRollout(placedStatus)
	status.Maas = maasSummary(ingressStatus, slices.ContainsFunc(placedStatus, func(cluster *aigridv1.ModelClusterStatus) bool {
		return cluster.GetDeployment() == aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_RUNNING
	}))
	switch {
	case anyIngress && len(failed) == 0 && len(notReady) == 0 && len(stopped) == 0 && len(noReport) == 0:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_READY
		status.Reason = "Ready"
	case anyIngress && ingressReady && placedReady > 0 && placedReady < len(placedStatus):
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE
		status.Reason = "PartiallyAvailable"
	case len(failed) > 0:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_FAILED
		status.Reason = "Failed"
	case status.Deployment == aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STOPPED && allSynced(placedStatus):
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		status.Reason = "Stopped"
	default:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		status.Reason = "NotReady"
	}
}

// clusterState is what a cluster's reports add up to: Failed when one is
// failed, NotReady when one is pending, NoReport when one is missing, and
// Ready otherwise.
func clusterState(reports []kindReport) string {
	result := "Ready"
	for _, r := range reports {
		state, found := r.state()
		switch {
		case found && state == StateFailed:
			return "Failed"
		case found && state == StatePending:
			result = "NotReady"
		case !found && result == "Ready":
			result = "NoReport"
		}
	}
	return result
}

// ingressObjectsReady reports whether a cluster's MaaSModelRef and
// ExternalModel reports both count as ready.
func ingressObjectsReady(reports ...kindReport) bool {
	for _, r := range reports {
		if state, found := r.state(); !found || state != StateReady {
			return false
		}
	}
	return true
}

// deploymentState is a placed cluster's deployment state from what its
// LLMInferenceService on the cluster says (ObjectReport.Live), apart from
// Argo CD's sync, with KServe's message unless it is running.
func deploymentState(r kindReport) (aigridv1.ModelDeploymentState, string) {
	if !r.found {
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_UNSPECIFIED, ""
	}
	switch r.report.Live {
	case LiveReady:
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_RUNNING, ""
	case LiveFailed:
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_FAILED, r.report.LiveMessage
	case LiveStopped:
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STOPPED, r.report.LiveMessage
	case LiveStarting:
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STARTING, r.report.LiveMessage
	}
	return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_UNSPECIFIED, ""
}

// syncState is whether Argo CD has applied a placed cluster's
// LLMInferenceService as Git has it. A cluster whose Application doesn't
// list it yet is out of sync; one with no Application has no report.
func syncState(r kindReport, hasApp bool) (aigridv1.ModelSyncState, string) {
	switch {
	case !r.found && hasApp:
		return aigridv1.ModelSyncState_MODEL_SYNC_STATE_OUT_OF_SYNC, ""
	case !r.found:
		return aigridv1.ModelSyncState_MODEL_SYNC_STATE_UNSPECIFIED, ""
	}
	switch r.report.Sync {
	case SyncSynced:
		return aigridv1.ModelSyncState_MODEL_SYNC_STATE_SYNCED, ""
	case SyncFailed:
		return aigridv1.ModelSyncState_MODEL_SYNC_STATE_FAILED, r.report.SyncMessage
	}
	return aigridv1.ModelSyncState_MODEL_SYNC_STATE_OUT_OF_SYNC, r.report.SyncMessage
}

// maasStatus is an ingress cluster's MaaS state from its MaaSModelRef and
// ExternalModel reports: no report until it lists both; then failed when
// either is failed; awaiting a subscription and the route when the
// MaaSModelRef says so; configuring routing when either is pending for any
// other reason but a missing subscription; awaiting a subscription when the
// MaaSModelRef waits only for one; and available otherwise. The message is
// the deciding object's, MaaS's own words.
func maasStatus(cluster string, ref, external kindReport) *aigridv1.ModelIngressStatus {
	out := &aigridv1.ModelIngressStatus{Cluster: cluster}
	if !ref.found || !external.found {
		return out
	}
	for _, r := range []kindReport{ref, external} {
		if r.report.State == StateFailed {
			out.State, out.Message = aigridv1.ModelMaasState_MODEL_MAAS_STATE_FAILED, r.report.Message
			return out
		}
	}
	if ref.report.State == StatePending && ref.report.Message == maasAwaitingSubscriptionAndBackend {
		out.State, out.Message = aigridv1.ModelMaasState_MODEL_MAAS_STATE_AWAITING_SUBSCRIPTION_AND_BACKEND, ref.report.Message
		return out
	}
	for _, r := range []kindReport{ref, external} {
		if r.report.State == StatePending && !r.report.NoSubscription {
			out.State, out.Message = aigridv1.ModelMaasState_MODEL_MAAS_STATE_CONFIGURING_ROUTING, r.report.Message
			return out
		}
	}
	if ref.report.NoSubscription {
		out.State, out.Message = aigridv1.ModelMaasState_MODEL_MAAS_STATE_AWAITING_SUBSCRIPTION, ref.report.Message
		return out
	}
	out.State = aigridv1.ModelMaasState_MODEL_MAAS_STATE_AVAILABLE
	return out
}

// deploymentSummary sums the placed clusters' deployment states: FAILED when
// one reports it failed or its sync failed; STARTING when one is starting or
// hasn't reported; STOPPED when every one is stopped; RUNNING when every one
// runs it or is stopped, and at least one runs it; and UNSPECIFIED while
// none has reported a deployment.
func deploymentSummary(placed []*aigridv1.ModelClusterStatus) aigridv1.ModelDeploymentState {
	running, stopped, reported, starting := 0, 0, false, false
	for _, c := range placed {
		if c.GetSync() == aigridv1.ModelSyncState_MODEL_SYNC_STATE_FAILED {
			return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_FAILED
		}
		switch c.GetDeployment() {
		case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_FAILED:
			return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_FAILED
		case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_RUNNING:
			running++
			reported = true
		case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STOPPED:
			stopped++
			reported = true
		case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STARTING:
			starting, reported = true, true
		default:
			starting = true
		}
	}
	switch {
	case !reported:
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_UNSPECIFIED
	case starting:
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STARTING
	case running == 0:
		return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STOPPED
	}
	return aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_RUNNING
}

// clusterRollout is a placed cluster's Git sync and deployment in one word
// (ModelClusterStatus.state): the deployment once the LLMInferenceService is
// on the cluster, else what its sync says.
func clusterRollout(c *aigridv1.ModelClusterStatus) aigridv1.ModelState {
	if c.GetSync() == aigridv1.ModelSyncState_MODEL_SYNC_STATE_FAILED {
		return aigridv1.ModelState_MODEL_STATE_FAILED
	}
	// Stopped but out of sync: a start (or another edit) is on its way. A
	// running cluster out of sync stays READY: its previous revision serves,
	// and an edit in flight can't be told from lasting drift.
	if c.GetDeployment() == aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STOPPED &&
		c.GetSync() == aigridv1.ModelSyncState_MODEL_SYNC_STATE_OUT_OF_SYNC {
		return aigridv1.ModelState_MODEL_STATE_SYNCING
	}
	switch c.GetDeployment() {
	case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_RUNNING:
		return aigridv1.ModelState_MODEL_STATE_READY
	case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STARTING:
		return aigridv1.ModelState_MODEL_STATE_STARTING
	case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_FAILED:
		return aigridv1.ModelState_MODEL_STATE_FAILED
	case aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STOPPED:
		return aigridv1.ModelState_MODEL_STATE_STOPPED
	}
	switch c.GetSync() {
	case aigridv1.ModelSyncState_MODEL_SYNC_STATE_OUT_OF_SYNC:
		return aigridv1.ModelState_MODEL_STATE_SYNCING
	case aigridv1.ModelSyncState_MODEL_SYNC_STATE_SYNCED:
		return aigridv1.ModelState_MODEL_STATE_SYNCED
	}
	return aigridv1.ModelState_MODEL_STATE_UNSPECIFIED
}

// modelRollout sums the placed clusters' states (ModelStatus.state): READY
// when every one is ready; PARTIALLY_READY when some are; otherwise the
// first of FAILED, STOPPED (when all are), STARTING,
// SYNCING and SYNCED that any cluster reports. With no placed cluster
// reporting anything, the model hasn't left Git yet: SAVED_TO_GIT. A model
// with no report at all keeps the SAVED_TO_GIT it was read with (apply
// returns before this).
func modelRollout(placed []*aigridv1.ModelClusterStatus) aigridv1.ModelState {
	count := map[aigridv1.ModelState]int{}
	for _, c := range placed {
		count[c.GetState()]++
	}
	n := len(placed)
	switch ready := count[aigridv1.ModelState_MODEL_STATE_READY]; {
	case n > 0 && ready == n:
		return aigridv1.ModelState_MODEL_STATE_READY
	case ready > 0:
		return aigridv1.ModelState_MODEL_STATE_PARTIALLY_READY
	}
	if n > 0 && count[aigridv1.ModelState_MODEL_STATE_STOPPED] == n {
		return aigridv1.ModelState_MODEL_STATE_STOPPED
	}
	for _, state := range []aigridv1.ModelState{
		aigridv1.ModelState_MODEL_STATE_FAILED,
		aigridv1.ModelState_MODEL_STATE_STARTING,
		aigridv1.ModelState_MODEL_STATE_SYNCING,
		aigridv1.ModelState_MODEL_STATE_SYNCED,
	} {
		if count[state] > 0 {
			return state
		}
	}
	// Stopped where it reports, and no report yet elsewhere (a cluster
	// newly placed, with no Application yet).
	if count[aigridv1.ModelState_MODEL_STATE_STOPPED] > 0 {
		return aigridv1.ModelState_MODEL_STATE_STOPPED
	}
	return aigridv1.ModelState_MODEL_STATE_SAVED_TO_GIT
}

// withStopped puts the clusters where the model is stopped in front of a
// clusterSummary message, for example "Stopped on site-a; ready on site-b".
func withStopped(message string, stopped []string) string {
	if len(stopped) == 0 {
		return message
	}
	out := "Stopped on " + strings.Join(stopped, ", ")
	if message == "" {
		return out
	}
	return out + "; " + strings.ToLower(message[:1]) + message[1:]
}

// withPublished adds where the ingress-only clusters publish the model
// through MaaS to a clusterSummary message, for example "Not ready on site-b;
// not published on site-a". They don't serve the model, so they aren't listed
// with the placed clusters.
func withPublished(message string, failed, notPublished, published []string) string {
	parts := []string{}
	if message != "" {
		parts = append(parts, message)
	}
	for _, part := range []struct {
		prefix   string
		clusters []string
	}{{"failed to publish on ", failed}, {"not published on ", notPublished}, {"published on ", published}} {
		if len(part.clusters) > 0 {
			parts = append(parts, part.prefix+strings.Join(part.clusters, ", "))
		}
	}
	out := strings.Join(parts, "; ")
	if out == "" {
		return ""
	}
	return strings.ToUpper(out[:1]) + out[1:]
}

// allSynced reports whether every placed cluster has applied Git's change,
// so a stop or a start still on its way isn't read as settled.
func allSynced(placed []*aigridv1.ModelClusterStatus) bool {
	for _, c := range placed {
		if c.GetSync() != aigridv1.ModelSyncState_MODEL_SYNC_STATE_SYNCED {
			return false
		}
	}
	return true
}

// maasSummary sums the ingress clusters' MaaS states, worst first: FAILED;
// AWAITING_SUBSCRIPTION_AND_BACKEND; CONFIGURING_ROUTING, also while one
// hasn't reported; AWAITING_SUBSCRIPTION; AVAILABLE. While serving is
// false, AVAILABLE and CONFIGURING_ROUTING are UNAVAILABLE: MaaS checks only
// the route, so it can't see that no placed cluster serves the model, and a
// model stopped or failed everywhere would otherwise read "configuring
// routing" for good (MaaS says "Governed but backend is not ready" then).
// UNSPECIFIED while none has reported.
func maasSummary(ingress []*aigridv1.ModelIngressStatus, serving bool) aigridv1.ModelMaasState {
	rank := map[aigridv1.ModelMaasState]int{
		aigridv1.ModelMaasState_MODEL_MAAS_STATE_AVAILABLE:                         0,
		aigridv1.ModelMaasState_MODEL_MAAS_STATE_AWAITING_SUBSCRIPTION:             1,
		aigridv1.ModelMaasState_MODEL_MAAS_STATE_CONFIGURING_ROUTING:               2,
		aigridv1.ModelMaasState_MODEL_MAAS_STATE_AWAITING_SUBSCRIPTION_AND_BACKEND: 3,
		aigridv1.ModelMaasState_MODEL_MAAS_STATE_FAILED:                            4,
	}
	worst, reported := aigridv1.ModelMaasState_MODEL_MAAS_STATE_AVAILABLE, false
	for _, in := range ingress {
		state := in.GetState()
		if state == aigridv1.ModelMaasState_MODEL_MAAS_STATE_UNSPECIFIED {
			state = aigridv1.ModelMaasState_MODEL_MAAS_STATE_CONFIGURING_ROUTING
		} else {
			reported = true
		}
		if rank[state] > rank[worst] {
			worst = state
		}
	}
	switch {
	case !reported:
		return aigridv1.ModelMaasState_MODEL_MAAS_STATE_UNSPECIFIED
	case !serving && (worst == aigridv1.ModelMaasState_MODEL_MAAS_STATE_AVAILABLE ||
		worst == aigridv1.ModelMaasState_MODEL_MAAS_STATE_CONFIGURING_ROUTING):
		return aigridv1.ModelMaasState_MODEL_MAAS_STATE_UNAVAILABLE
	}
	return worst
}

// clusterReports says what one cluster reported about the model, for example
// "LLMInferenceService ready; MaaSModelRef not ready; ExternalModel ready" on
// a combined cluster. A MaaSModelRef waiting only for a subscription says so.
func clusterReports(reports []kindReport) string {
	parts := make([]string, 0, len(reports))
	for _, r := range reports {
		switch {
		case !r.found:
			parts = append(parts, "no "+r.kind+" report")
		case r.report.NoSubscription:
			parts = append(parts, r.kind+" ready, no subscription")
		case r.report.Live == LiveStopped:
			parts = append(parts, r.kind+" stopped")
		case r.report.Live == LiveReady && r.report.Sync != SyncSynced:
			parts = append(parts, r.kind+" ready on its previous revision, not synced yet")
		case r.report.State == StateReady:
			parts = append(parts, r.kind+" ready")
		case r.report.State == StateFailed:
			parts = append(parts, r.kind+" failed")
		default:
			parts = append(parts, r.kind+" not ready")
		}
	}
	message := strings.Join(parts, "; ")
	if message == "" {
		return ""
	}
	return strings.ToUpper(message[:1]) + message[1:]
}

// renderModelDeployment renders the inference side: the LLMInferenceService,
// with the version and clusters annotations, and a NetworkPolicy that lets
// the grid reach the model's workload and scheduler metrics.
func (s *ModelsServer) renderModelDeployment(object *aigridv1.Model, version int32, clusters []string) ([]byte, error) {
	id := object.GetId()
	llm := llmInferenceServiceFromModel(object, s.namespace)
	annotations := llm.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[gitVersionAnnotation] = strconv.FormatInt(int64(version), 10)
	annotations[gitClustersAnnotation] = strings.Join(clusters, ",")
	delete(annotations, legacyModelNameAnnotation)
	llm.SetAnnotations(annotations)
	if err := unstructured.SetNestedField(llm.Object, servedModelName(object), "spec", "model", "name"); err != nil {
		return nil, err
	}

	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]interface{}{
			"name":      id + "-grid",
			"namespace": s.namespace,
			"labels":    stringMapToInterfaceMap(map[string]string{modelLabelKey: id, managedByLabelKey: managedByLabelValue}),
		},
		"spec": map[string]interface{}{
			"podSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app.kubernetes.io/name": id},
			},
			"policyTypes": []interface{}{"Ingress"},
			"ingress": []interface{}{map[string]interface{}{
				// 8000: the grid gateway to the workload; 9090: the grid
				// operator scraping the scheduler's metrics.
				"ports": []interface{}{
					map[string]interface{}{"port": int64(8000), "protocol": "TCP"},
					map[string]interface{}{"port": int64(9090), "protocol": "TCP"},
				},
				"from": []interface{}{map[string]interface{}{
					"namespaceSelector": map[string]interface{}{
						"matchLabels": map[string]interface{}{"kubernetes.io/metadata.name": s.gridNamespace},
					},
				}},
			}},
		},
	}}
	return marshalYAMLDocs(llm, policy)
}

// renderModelCatalog renders the ingress side: an ExternalModel routed to the
// grid provider, and the MaaSModelRef that lists it in MaaS.
func (s *ModelsServer) renderModelCatalog(object *aigridv1.Model) ([]byte, error) {
	id := object.GetId()
	target := servedModelName(object)
	external := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "inference.opendatahub.io/v1alpha1",
		"kind":       "ExternalModel",
		"metadata": map[string]interface{}{
			"name":      id,
			"namespace": s.subscriptionNamespace,
			"labels":    stringMapToInterfaceMap(map[string]string{modelLabelKey: id, managedByLabelKey: managedByLabelValue}),
		},
		"spec": map[string]interface{}{
			"externalProviderRefs": []interface{}{map[string]interface{}{
				"ref":         map[string]interface{}{"name": gridProviderName},
				"apiFormat":   "openai-chat",
				"path":        "/v1/chat/completions",
				"targetModel": target,
				"weight":      int64(1),
			}},
		},
	}}
	modelRef := maasModelRefFromModel(object, s.subscriptionNamespace)
	if err := unstructured.SetNestedField(modelRef.Object, "ExternalModel", "spec", "modelRef", "kind"); err != nil {
		return nil, err
	}
	return marshalYAMLDocs(external, modelRef)
}

func (s *ModelsServer) readGitModel(head gitops.Snapshot, id string) (*gitModel, error) {
	p := path.Join(modelDeploymentGitDir(id), "deployment.yaml")
	data, err := head.Read(p)
	if err != nil {
		return nil, modelGitError("get", id, err)
	}
	current, err := parseGitModel(data)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read %s: %v", p, err).WithCause(err)
	}
	return current, nil
}

// parseGitModel reads the model from its deployment file: the annotations
// become metadata.version and spec.placement. The status says the model is
// saved to Git (no phase, reason SavedToGit) and lists the clusters the file
// places it on; modelReports.apply replaces the phase when the clusters report.
func parseGitModel(data []byte) (*gitModel, error) {
	docs := bytes.SplitN(data, []byte("\n---\n"), 2)
	llm := map[string]interface{}{}
	if err := yaml.Unmarshal(docs[0], &llm); err != nil {
		return nil, err
	}
	item := unstructured.Unstructured{Object: llm}
	if item.GetKind() != llmInferenceKind {
		return nil, fmt.Errorf("first document is %q, want %s", item.GetKind(), llmInferenceKind)
	}
	model := modelFromLLMInferenceService(item)
	annotations := model.GetMetadata().GetAnnotations()
	version, err := strconv.ParseInt(annotations[gitVersionAnnotation], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("annotation %s: %w", gitVersionAnnotation, err)
	}
	var clusters []string
	if s := annotations[gitClustersAnnotation]; s != "" {
		clusters = strings.Split(s, ",")
	}
	// spec.model.name is the name the deployment serves. A model written
	// before the backend served models as their id serves its own name until
	// its next update switches it (servedNameOnUpdate), so a read reports
	// that name rather than claim the id early. Empty is the id.
	if model.Spec.GetModel() != nil && model.Spec.Model.GetName() == "" {
		model.Spec.Model.Name = model.GetId()
	}
	delete(annotations, gitVersionAnnotation)
	delete(annotations, gitClustersAnnotation)
	delete(annotations, legacyModelNameAnnotation)
	if len(annotations) == 0 {
		model.Metadata.Annotations = nil
	}
	// The backend sets ai-grid/model and app.kubernetes.io/managed-by itself,
	// and a caller may not send them back, so they don't show in the API.
	delete(model.Metadata.Labels, modelLabelKey)
	delete(model.Metadata.Labels, managedByLabelKey)
	if len(model.Metadata.Labels) == 0 {
		model.Metadata.Labels = nil
	}
	model.Metadata.Version = int32(version)
	model.Status = &aigridv1.ModelStatus{
		Reason:  "SavedToGit",
		Message: "Saved to Git; no cluster has reported the model's status",
		State:   aigridv1.ModelState_MODEL_STATE_SAVED_TO_GIT,
	}
	if len(clusters) > 0 {
		model.Status.Placement = &aigridv1.ModelPlacementStatus{NumberOfSelectedClusters: int32(len(clusters)), SelectedClusters: clusters}
		model.Spec.Placement = &aigridv1.ModelPlacement{LabelSelector: &aigridv1.ModelLabelSelector{
			MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: clusterNameLabelKey, Operator: "In", Values: clusters}},
		}}
	}
	return &gitModel{model: model, clusters: clusters}, nil
}

// gitEnsureNotReferenced refuses to delete a model that a subscription in the
// repository still names.
func (s *ModelsServer) gitEnsureNotReferenced(head gitops.Snapshot, id string) error {
	names, err := head.List(subscriptionsGitDir)
	if err != nil {
		return modelGitError("delete", id, err)
	}
	for _, name := range names {
		subID, ok := strings.CutSuffix(name, ".yaml")
		if !ok {
			continue
		}
		doc, err := readSubscriptionDoc(head, subID)
		if err != nil {
			return err
		}
		for _, ref := range doc.sub.GetSpec().GetModelRefs() {
			if ref.GetName() == id && (ref.GetNamespace() == "" || ref.GetNamespace() == s.subscriptionNamespace) {
				return connect.Errorf(connect.CodeFailedPrecondition, "Model %q is referenced by Subscription %q", id, subID)
			}
		}
	}
	return nil
}

// gitPlacementClusters returns the clusters a placement names. Git mode
// places by cluster name only (the name label ACM sets on every
// ManagedCluster): Argo CD delivers a cluster's index to that cluster, so
// cluster sets, other labels, a cluster count and rollouts have nothing to
// map to. A placement is required: without one the model would be published
// to the ingress and deployed nowhere.
func gitPlacementClusters(spec *aigridv1.ModelSpec) ([]string, error) {
	if !defaultRollout(spec.GetRollout()) {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.rollout supports only strategy All in git mode")
	}
	placement := spec.GetPlacement()
	if placement == nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement must name at least one cluster in git mode")
	}
	if len(placement.GetClusterSets()) > 0 || placement.NumberOfClusters != nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement supports only the cluster name label in git mode")
	}
	var clusters []string
	selector := placement.GetLabelSelector()
	// A label selector ANDs its requirements, so two name requirements would
	// select only the clusters in both.
	if len(selector.GetMatchLabels())+len(selector.GetMatchExpressions()) > 1 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement must use one %q requirement in git mode: a label selector ANDs them", clusterNameLabelKey)
	}
	for key, value := range selector.GetMatchLabels() {
		if key != clusterNameLabelKey {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement label %q is not supported in git mode; use %q", key, clusterNameLabelKey)
		}
		clusters = append(clusters, value)
	}
	for _, req := range selector.GetMatchExpressions() {
		if req.GetKey() != clusterNameLabelKey || req.GetOperator() != "In" {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement supports only %q In [...] in git mode", clusterNameLabelKey)
		}
		clusters = append(clusters, req.GetValues()...)
	}
	if len(clusters) == 0 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement must name at least one cluster in git mode")
	}
	for _, cluster := range clusters {
		if !names.ValidDNSLabel(cluster) {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "cluster name %q is not a valid RFC 1123 DNS label", cluster)
		}
	}
	slices.Sort(clusters)
	return slices.Compact(clusters), nil
}

// defaultRollout reports whether r asks for what git mode does anyway: no
// rollout, or strategy All with no other field. Argo CD delivers every
// cluster's index at once, which is All, so that request is accepted and not
// stored.
func defaultRollout(r *aigridv1.ModelRollout) bool {
	if r == nil {
		return true
	}
	switch r.GetStrategy() {
	case aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_UNSPECIFIED, aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_ALL:
	default:
		return false
	}
	return r.GetMaxConcurrency() == "" && r.GetMaxFailures() == "" && r.GetProgressDeadline() == "" && r.GetMinSuccessTime() == ""
}

// putIndexChanges adds to change the index files that change when model id
// moves from the clusters in from to those in to. With inIngress the model
// is in the ingress index, without it the model leaves it. Every index it
// reads is decided on, so one it leaves as it is goes to change.Read: if
// another write changes that index before the commit, the commit conflicts,
// and the rerun decides again on the new head.
func putIndexChanges(head gitops.Snapshot, change *gitops.Change, id string, from, to []string, inIngress bool) error {
	deployment := "../../" + path.Join("models", id, "deployment")
	for _, cluster := range from {
		if !slices.Contains(to, cluster) {
			if err := putIndexEntry(head, change, clusterGitIndex(cluster), deployment, false); err != nil {
				return err
			}
		}
	}
	for _, cluster := range to {
		if err := putIndexEntry(head, change, clusterGitIndex(cluster), deployment, true); err != nil {
			return err
		}
	}
	return putIndexEntry(head, change, ingressGitIndex, "../"+path.Join("models", id, "catalog"), inIngress)
}

// putIndexEntry adds or removes one resource in a kustomization index. It
// puts the file when it changes, and otherwise adds it to change.Read.
func putIndexEntry(head gitops.Snapshot, change *gitops.Change, index, resource string, present bool) error {
	resources, err := readKustomizationResources(head, index)
	if err != nil {
		return err
	}
	if slices.Contains(resources, resource) == present {
		change.Read = append(change.Read, index)
		return nil
	}
	if present {
		resources = append(resources, resource)
		slices.Sort(resources)
	} else {
		resources = slices.DeleteFunc(resources, func(r string) bool { return r == resource })
	}
	change.Put[index] = renderKustomization(resources)
	return nil
}

// readKustomizationResources returns an index's resources; a missing index
// has none.
func readKustomizationResources(head gitops.Snapshot, index string) ([]string, error) {
	data, err := head.Read(index)
	if errors.Is(err, gitops.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, modelGitError("read", index, err)
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(data, &k); err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read %s: %v", index, err).WithCause(err)
	}
	return k.Resources, nil
}

func renderKustomization(resources []string) []byte {
	if resources == nil {
		resources = []string{}
	}
	data, _ := yaml.Marshal(map[string]interface{}{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  resources,
	})
	return data
}

func modelGitError(op, id string, err error) error {
	return mapGitopsError("Model", "Models", op, id, err, staleModelError)
}

// servedModelName is the name vLLM serves the model as and the ExternalModel
// targets: its id. MaaS lists the model under its id and forwards the request
// body unchanged (the AI gateway applies an ExternalModel's targetModel only
// with API translation on), so the "model" a tenant sends, and the grid routes
// on, is the id. renderModelDeployment sets the LLMInferenceService's
// spec.model.name to it. Ids are unique, so no two models share a served name.
func servedModelName(object *aigridv1.Model) string {
	return object.GetId()
}

// servedNameOnUpdate switches a model that still serves another name, written
// before the backend served models as their id, to its id. Any update does
// it, whatever it changes, and so does one that sends back the name a read
// reported: vLLM's served name changes, so its pods restart once. A name other
// than the stored one or the id is left for normalizeServedName to refuse.
func servedNameOnUpdate(current, merged *aigridv1.Model) {
	source := merged.GetSpec().GetModel()
	if source != nil && source.GetName() == current.GetSpec().GetModel().GetName() {
		source.Name = ""
	}
}

// normalizeServedName accepts a spec.model.name that is empty or the id, and sets
// it to the id. As in odh-dashboard, a model's served name is its resource
// name; the human-readable name is annotation openshift.io/display-name.
// Any other name would be one nothing serves, so it is InvalidArgument.
func normalizeServedName(id string, spec *aigridv1.ModelSpec) error {
	source := spec.GetModel()
	if source == nil {
		return nil
	}
	if name := source.GetName(); name != "" && name != id {
		return connect.Errorf(connect.CodeInvalidArgument,
			"spec.model.name %q must be empty or the model id %q: a model is served as its id, which is the name tenants send; set a human-readable name in annotation %s",
			name, id, annotationDisplayName)
	}
	source.Name = id
	return nil
}
