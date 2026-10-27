package servers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
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
const (
	modelsGitDir    = "catalog/models"
	clustersGitDir  = "catalog/clusters"
	ingressGitIndex = "catalog/ingress/kustomization.yaml"

	// gitClustersAnnotation records the model's clusters on its
	// LLMInferenceService, so a read returns the placement without scanning
	// every cluster index.
	gitClustersAnnotation = "ai-grid/clusters"
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
	for _, resource := range resources {
		// ../models/<id>/catalog
		id := path.Base(path.Dir(resource))
		current, err := s.readGitModel(head, id)
		if err != nil {
			return nil, err
		}
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
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("create", id, err)
	}
	if _, err := head.Read(path.Join(modelDeploymentGitDir(id), "deployment.yaml")); err == nil {
		return nil, connect.Errorf(connect.CodeAlreadyExists, "Model %q already exists", id)
	} else if !errors.Is(err, gitops.ErrNotFound) {
		return nil, modelGitError("create", id, err)
	}
	return s.gitCommitModel(ctx, head, "create", object, 1, nil, clusters)
}

func (s *ModelsServer) gitUpdate(ctx context.Context, req *aigridv1.ModelsUpdateRequest) (*aigridv1.Model, error) {
	object := req.GetObject()
	id := modelID(object)
	version := object.GetMetadata().GetVersion()
	if req.GetLock() && version == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "lock requires metadata.version")
	}
	paths, err := parseModelUpdateMask(req.GetUpdateMask())
	if err != nil {
		return nil, err
	}
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("update", id, err)
	}
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
	if err := validateModelSpec(merged.GetSpec()); err != nil {
		return nil, err
	}
	clusters, err := gitPlacementClusters(merged.GetSpec())
	if err != nil {
		return nil, err
	}
	return s.gitCommitModel(ctx, head, "update", merged, current.model.GetMetadata().GetVersion()+1, current.clusters, clusters)
}

// gitDelete removes the model's files and its index entries. A nonzero
// version must equal the model's, or the delete is Aborted. It is refused while
// a subscription in the repository names the model.
func (s *ModelsServer) gitDelete(ctx context.Context, id string, version int32) error {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return modelGitError("delete", id, err)
	}
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

	change := gitops.Change{Put: map[string][]byte{}, Message: "models: delete " + id}
	for _, dir := range []string{modelDeploymentGitDir(id), modelCatalogGitDir(id)} {
		names, err := head.List(dir)
		if err != nil {
			return modelGitError("delete", id, err)
		}
		for _, name := range names {
			change.Delete = append(change.Delete, path.Join(dir, name))
		}
	}
	if err := putIndexChanges(head, change.Put, id, current.clusters, nil, false); err != nil {
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
	if err := putIndexChanges(head, change.Put, id, from, to, true); err != nil {
		return nil, err
	}

	if _, err := s.writer.Commit(ctx, head.Revision(), change); err != nil {
		return nil, modelGitError(op, id, err)
	}
	written, err := parseGitModel(deployment)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read back model %q: %v", id, err).WithCause(err)
	}
	return written.model, nil
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
	llm.SetAnnotations(annotations)

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
	target := object.GetSpec().GetModel().GetName()
	if target == "" {
		target = id
	}
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
// become metadata.version and spec.placement. Git mode never reads the
// clusters, so status says only that the model is saved to Git (no phase,
// reason SavedToGit) and lists the clusters the file places it on.
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
	delete(annotations, gitVersionAnnotation)
	delete(annotations, gitClustersAnnotation)
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
		Message: "Saved to Git; the model's status isn't read from the clusters",
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
// map to.
func gitPlacementClusters(spec *aigridv1.ModelSpec) ([]string, error) {
	if !defaultRollout(spec.GetRollout()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "spec.rollout supports only strategy All in git mode")
	}
	placement := spec.GetPlacement()
	if placement == nil {
		return nil, nil
	}
	if len(placement.GetClusterSets()) > 0 || placement.NumberOfClusters != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "spec.placement supports only the cluster name label in git mode")
	}
	var clusters []string
	selector := placement.GetLabelSelector()
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
		return nil, connect.NewError(connect.CodeInvalidArgument, "spec.placement must name at least one cluster in git mode")
	}
	for _, cluster := range clusters {
		if !modelNameRE.MatchString(cluster) {
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

// putIndexChanges adds to put the index files that change when model id
// moves from the clusters in from to those in to. With inIngress the model
// is in the ingress index, without it the model leaves it.
func putIndexChanges(head gitops.Snapshot, put map[string][]byte, id string, from, to []string, inIngress bool) error {
	deployment := "../../" + path.Join("models", id, "deployment")
	for _, cluster := range from {
		if !slices.Contains(to, cluster) {
			if err := putIndexEntry(head, put, clusterGitIndex(cluster), deployment, false); err != nil {
				return err
			}
		}
	}
	for _, cluster := range to {
		if err := putIndexEntry(head, put, clusterGitIndex(cluster), deployment, true); err != nil {
			return err
		}
	}
	return putIndexEntry(head, put, ingressGitIndex, "../"+path.Join("models", id, "catalog"), inIngress)
}

// putIndexEntry adds or removes one resource in a kustomization index, and
// puts the file only when it changes.
func putIndexEntry(head gitops.Snapshot, put map[string][]byte, index, resource string, present bool) error {
	resources, err := readKustomizationResources(head, index)
	if err != nil {
		return err
	}
	if slices.Contains(resources, resource) == present {
		return nil
	}
	if present {
		resources = append(resources, resource)
		slices.Sort(resources)
	} else {
		resources = slices.DeleteFunc(resources, func(r string) bool { return r == resource })
	}
	put[index] = renderKustomization(resources)
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
	switch {
	case errors.Is(err, gitops.ErrNotFound):
		return connect.Errorf(connect.CodeNotFound, "Model %q not found", id).WithCause(err)
	case errors.Is(err, gitops.ErrConflict):
		return staleModelError(op, id).WithCause(err)
	default:
		return connect.Errorf(connect.CodeUnavailable, "%s Model %q: git: %v", op, id, err).WithCause(err)
	}
}
