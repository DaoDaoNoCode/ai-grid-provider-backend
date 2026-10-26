package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/celfilter"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

// ModelsServerBuilder configures a ModelsServer.
type ModelsServerBuilder struct {
	logger                *slog.Logger
	client                dynamic.Interface
	namespace             string
	subscriptionNamespace string
	mwrsEnabled           *bool
}

// ModelsServer implements ai_grid.v1.Models against LLMInferenceService CRs
// (and optional Placement + ManifestWorkReplicaSet + MaaSModelRef).
type ModelsServer struct {
	aigridv1connect.UnimplementedModelsHandler
	logger                *slog.Logger
	client                dynamic.Interface
	namespace             string
	subscriptionNamespace string
	mwrsEnabled           *bool
}

var _ aigridv1connect.ModelsHandler = (*ModelsServer)(nil)

// NewModelsServer creates a builder for ModelsServer.
func NewModelsServer() *ModelsServerBuilder {
	return &ModelsServerBuilder{}
}

// SetLogger sets the logger.
func (b *ModelsServerBuilder) SetLogger(logger *slog.Logger) *ModelsServerBuilder {
	b.logger = logger
	return b
}

// SetDynamicClient sets the Kubernetes dynamic client. Mandatory.
func (b *ModelsServerBuilder) SetDynamicClient(client dynamic.Interface) *ModelsServerBuilder {
	b.client = client
	return b
}

// SetNamespace sets the namespace for LLMInferenceService / Placement / MWRS / MaaSModelRef.
func (b *ModelsServerBuilder) SetNamespace(namespace string) *ModelsServerBuilder {
	b.namespace = namespace
	return b
}

// SetSubscriptionNamespace sets the namespace used to check subscription/auth refs on Delete.
func (b *ModelsServerBuilder) SetSubscriptionNamespace(namespace string) *ModelsServerBuilder {
	b.subscriptionNamespace = namespace
	return b
}

// SetManifestWorkReplicaSetEnabled overrides MWRS capability detection (tests / fake).
func (b *ModelsServerBuilder) SetManifestWorkReplicaSetEnabled(enabled bool) *ModelsServerBuilder {
	b.mwrsEnabled = &enabled
	return b
}

// Build creates the ModelsServer.
func (b *ModelsServerBuilder) Build() (*ModelsServer, error) {
	if b.client == nil {
		return nil, errors.New("kubernetes dynamic client is mandatory")
	}
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	namespace := b.namespace
	if namespace == "" {
		namespace = defaultModelsNamespace
	}
	subNS := b.subscriptionNamespace
	if subNS == "" {
		subNS = defaultSubscriptionNamespace
	}
	return &ModelsServer{
		logger:                logger,
		client:                b.client,
		namespace:             namespace,
		subscriptionNamespace: subNS,
		mwrsEnabled:           b.mwrsEnabled,
	}, nil
}

// List returns models from the configured namespace.
func (s *ModelsServer) List(ctx context.Context, req *aigridv1.ModelsListRequest) (*aigridv1.ModelsListResponse, error) {
	if err := validateListLimit(req.Limit); err != nil {
		return nil, err
	}
	list, err := s.client.Resource(llmInferenceServicesResource).Namespace(s.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &aigridv1.ModelsListResponse{}, nil
		}
		return nil, mapModelAPIError("list", "", err)
	}
	items := make([]*aigridv1.Model, 0, len(list.Items))
	for i := range list.Items {
		m := modelFromLLMInferenceService(list.Items[i])
		s.enrichPlacement(ctx, m)
		items = append(items, m)
	}

	filter, err := celfilter.Compile(req.GetFilter(), &aigridv1.Model{})
	if err != nil {
		return nil, err
	}
	if filter != nil {
		now := time.Now().UTC()
		matched := make([]*aigridv1.Model, 0, len(items))
		for _, m := range items {
			ok, matchErr := filter.Match(ctx, m, modelSortID(m), now)
			if matchErr != nil {
				return nil, matchErr
			}
			if ok {
				matched = append(matched, m)
			}
		}
		items = matched
	}

	if err := applyModelOrder(items, req.Order); err != nil {
		return nil, err
	}
	page, total := applyListWindow(items, req.Offset, req.Limit)
	return &aigridv1.ModelsListResponse{
		Size:  int32(len(page)),
		Total: total,
		Items: page,
	}, nil
}

// Get returns one model by id.
func (s *ModelsServer) Get(ctx context.Context, req *aigridv1.ModelsGetRequest) (*aigridv1.ModelsGetResponse, error) {
	id := req.GetId()
	if err := validateModelID(id); err != nil {
		return nil, err
	}
	obj, err := s.client.Resource(llmInferenceServicesResource).Namespace(s.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, mapModelAPIError("get", id, err)
	}
	m := modelFromLLMInferenceService(*obj)
	s.enrichPlacement(ctx, m)
	return &aigridv1.ModelsGetResponse{Object: m}, nil
}

// Create deploys a model: LLMInferenceService + MaaSModelRef, and when
// placement is set, Placement + ManifestWorkReplicaSet.
func (s *ModelsServer) Create(ctx context.Context, req *aigridv1.ModelsCreateRequest) (*aigridv1.ModelsCreateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "object is required")
	}
	id := modelID(object)
	if err := validateModelID(id); err != nil {
		return nil, err
	}
	if object.Metadata == nil {
		object.Metadata = &aigridv1.Metadata{}
	}
	object.Metadata.Name = id
	object.Id = id

	if err := validateModelSpec(object.GetSpec()); err != nil {
		return nil, err
	}
	hasPlacement := object.GetSpec() != nil && object.GetSpec().GetPlacement() != nil
	if hasPlacement {
		if err := s.requireMWRS(ctx); err != nil {
			return nil, err
		}
	}

	var createdRefs []createdObjectRef
	deployment := llmInferenceServiceFromModel(object, s.namespace)
	created, err := s.client.Resource(llmInferenceServicesResource).Namespace(s.namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return nil, mapModelAPIError("create", id, err)
	}
	createdRefs = append(createdRefs, createdObjectRef{
		gvr: llmInferenceServicesResource, name: created.GetName(), uid: created.GetUID(),
	})

	modelRef := maasModelRefFromModel(object, s.namespace)
	createdRef, err := s.client.Resource(maasModelRefsResource).Namespace(s.namespace).Create(ctx, modelRef, metav1.CreateOptions{})
	if err != nil {
		s.compensateCreate(ctx, createdRefs)
		return nil, mapModelAPIError("create MaaSModelRef", id, err)
	}
	createdRefs = append(createdRefs, createdObjectRef{
		gvr: maasModelRefsResource, name: createdRef.GetName(), uid: createdRef.GetUID(),
	})

	if hasPlacement {
		placement := placementFromModel(object, s.namespace)
		createdPlacement, err := s.client.Resource(placementsResource).Namespace(s.namespace).Create(ctx, placement, metav1.CreateOptions{})
		if err != nil {
			s.compensateCreate(ctx, createdRefs)
			return nil, mapModelAPIError("create Placement", id, err)
		}
		createdRefs = append(createdRefs, createdObjectRef{
			gvr: placementsResource, name: createdPlacement.GetName(), uid: createdPlacement.GetUID(),
		})
		mwrs := mwrsFromModel(object, s.namespace, created)
		if _, err := s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).Create(ctx, mwrs, metav1.CreateOptions{}); err != nil {
			s.compensateCreate(ctx, createdRefs)
			if apierrors.IsNotFound(err) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, "ManifestWorkReplicaSet feature gate disabled on hub")
			}
			return nil, mapModelAPIError("create ManifestWorkReplicaSet", id, err)
		}
	}

	m := modelFromLLMInferenceService(*created)
	s.enrichPlacement(ctx, m)
	return &aigridv1.ModelsCreateResponse{Object: m}, nil
}

// Update applies a FieldMask patch to the model and syncs placement/MWRS.
func (s *ModelsServer) Update(ctx context.Context, req *aigridv1.ModelsUpdateRequest) (*aigridv1.ModelsUpdateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "object is required")
	}
	id := modelID(object)
	if err := validateModelID(id); err != nil {
		return nil, err
	}

	current, err := s.client.Resource(llmInferenceServicesResource).Namespace(s.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, mapModelAPIError("get", id, err)
	}

	var version int32
	if object.GetMetadata() != nil {
		version = object.GetMetadata().GetVersion()
	}
	if req.GetLock() && version == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "lock requires metadata.version")
	}
	if version != 0 && strconv.FormatInt(int64(version), 10) != current.GetResourceVersion() {
		return nil, staleModelError("update", id)
	}

	paths, err := parseModelUpdateMask(req.GetUpdateMask())
	if err != nil {
		return nil, err
	}

	currentModel := modelFromLLMInferenceService(*current)
	s.enrichPlacement(ctx, currentModel)
	merged, err := mergeModelUpdate(currentModel, object, paths)
	if err != nil {
		return nil, err
	}
	if err := validateModelSpec(merged.GetSpec()); err != nil {
		return nil, err
	}

	addingPlacement := paths.placement && merged.GetSpec().GetPlacement() != nil &&
		(currentModel.GetSpec() == nil || currentModel.GetSpec().GetPlacement() == nil)
	if addingPlacement || (paths.placement && merged.GetSpec().GetPlacement() != nil) {
		if err := s.requireMWRS(ctx); err != nil {
			return nil, err
		}
	}

	patchBody := map[string]interface{}{}
	patchMeta := map[string]interface{}{}
	ownedLabels := map[string]string{
		modelLabelKey: id, managedByLabelKey: managedByLabelValue,
	}
	if paths.labels {
		var next map[string]string
		if object.GetMetadata() != nil {
			next = object.GetMetadata().GetLabels()
		}
		patchMeta["labels"] = replaceStringMapPatch(current.GetLabels(), next, ownedLabels)
	}
	if paths.annotations {
		var next map[string]string
		if object.GetMetadata() != nil {
			next = object.GetMetadata().GetAnnotations()
		}
		patchMeta["annotations"] = replaceStringMapPatch(current.GetAnnotations(), next, nil)
	}
	if len(patchMeta) > 0 {
		patchBody["metadata"] = patchMeta
	}
	if paths.anyLLM() {
		patchBody["spec"] = modelSpecUpdatePatch(merged.GetSpec(), id, paths, current)
	}
	if version != 0 {
		meta, _ := patchBody["metadata"].(map[string]interface{})
		if meta == nil {
			meta = map[string]interface{}{}
			patchBody["metadata"] = meta
		}
		meta["resourceVersion"] = strconv.FormatInt(int64(version), 10)
	}

	var updated *unstructured.Unstructured
	if len(patchBody) > 0 {
		patchBytes, err := json.Marshal(patchBody)
		if err != nil {
			return nil, connect.Errorf(connect.CodeInternal, "marshal LLMInferenceService patch: %v", err).WithCause(err)
		}
		updated, err = s.client.Resource(llmInferenceServicesResource).Namespace(s.namespace).Patch(
			ctx, id, types.MergePatchType, patchBytes, metav1.PatchOptions{},
		)
		if err != nil {
			return nil, mapModelAPIError("update", id, err)
		}
	} else {
		updated = current
	}

	if paths.placement || paths.rollout || paths.anyLLM() {
		if err := s.syncPlacementSideEffects(ctx, merged, updated); err != nil {
			return nil, err
		}
	}

	// Sync MaaSModelRef annotations when annotations change.
	if paths.annotations {
		_ = s.syncModelRefAnnotations(ctx, id, merged)
	}

	m := modelFromLLMInferenceService(*updated)
	s.enrichPlacement(ctx, m)
	return &aigridv1.ModelsUpdateResponse{Object: m}, nil
}

// Delete removes Placement / MWRS first, then the LLMInferenceService and
// MaaSModelRef. Side effects are deleted before the service so a surviving
// MWRS cannot keep ManifestWorks on the spokes after a successful Delete.
// Failures deleting MWRS/Placement/ModelRef are returned (NotFound ignored)
// so a retry can finish cleanup. When the service is already gone, Delete
// still removes leftover Placement/MWRS/ModelRef, then returns NotFound.
//
// When resource_version is nonzero, the live service is compared first so a
// stale delete changes nothing; the same version is a K8s Delete precondition
// on the LLMInferenceService.
func (s *ModelsServer) Delete(ctx context.Context, req *aigridv1.ModelsDeleteRequest) (*aigridv1.ModelsDeleteResponse, error) {
	id := req.GetId()
	if err := validateModelID(id); err != nil {
		return nil, err
	}
	current, err := s.client.Resource(llmInferenceServicesResource).Namespace(s.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if cleanErr := s.deleteModelSideEffects(ctx, id); cleanErr != nil {
				return nil, cleanErr
			}
			return nil, mapModelAPIError("get", id, err)
		}
		return nil, mapModelAPIError("get", id, err)
	}

	if err := s.ensureNotReferenced(ctx, id); err != nil {
		return nil, err
	}

	if rv := req.GetResourceVersion(); rv != 0 {
		// Compare only when the hub reported a resourceVersion; empty means
		// fall through to the Delete precondition (fake clients often omit it).
		if currentRV := current.GetResourceVersion(); currentRV != "" &&
			strconv.FormatInt(int64(rv), 10) != currentRV {
			return nil, staleModelError("delete", id)
		}
	}

	if err := s.deleteModelSideEffects(ctx, id); err != nil {
		return nil, err
	}

	delOpts := metav1.DeleteOptions{}
	if rv := req.GetResourceVersion(); rv != 0 {
		rvStr := strconv.FormatInt(int64(rv), 10)
		delOpts.Preconditions = &metav1.Preconditions{ResourceVersion: &rvStr}
	}
	if err := s.client.Resource(llmInferenceServicesResource).Namespace(s.namespace).Delete(ctx, id, delOpts); err != nil {
		return nil, mapModelAPIError("delete", id, err)
	}
	return &aigridv1.ModelsDeleteResponse{}, nil
}

// deleteModelSideEffects removes Placement, MWRS, and MaaSModelRef for id.
// NotFound is ignored; other errors are returned so Delete stays retryable.
func (s *ModelsServer) deleteModelSideEffects(ctx context.Context, id string) error {
	pname := placementNameForModel(id)
	if err := s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).Delete(ctx, pname, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return mapModelAPIError("delete ManifestWorkReplicaSet", id, err)
	}
	if err := s.client.Resource(placementsResource).Namespace(s.namespace).Delete(ctx, pname, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return mapModelAPIError("delete Placement", id, err)
	}
	if err := s.client.Resource(maasModelRefsResource).Namespace(s.namespace).Delete(ctx, id, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return mapModelAPIError("delete MaaSModelRef", id, err)
	}
	return nil
}

func (s *ModelsServer) enrichPlacement(ctx context.Context, m *aigridv1.Model) {
	if m == nil {
		return
	}
	id := modelID(m)
	pname := placementNameForModel(id)
	placement, err := s.client.Resource(placementsResource).Namespace(s.namespace).Get(ctx, pname, metav1.GetOptions{})
	if err != nil {
		return
	}
	var decision *unstructured.Unstructured
	decisions, err := s.client.Resource(placementDecisionsResource).Namespace(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "cluster.open-cluster-management.io/placement=" + pname,
	})
	if err == nil && len(decisions.Items) > 0 {
		decision = &decisions.Items[0]
	}
	enrichModelFromPlacement(m, placement, decision)
	s.enrichPlacementLocations(ctx, m)

	mwrs, err := s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).Get(ctx, pname, metav1.GetOptions{})
	if err == nil {
		enrichModelFromMWRS(m, mwrs)
		s.enrichManifestWorks(ctx, m, pname)
		deriveModelPhaseFromRollout(m)
	}
}

// ListLocations returns unique sorted location labels of Available AI Grid member clusters.
func (s *ModelsServer) ListLocations(ctx context.Context, _ *aigridv1.ModelsListLocationsRequest) (*aigridv1.ModelsListLocationsResponse, error) {
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &aigridv1.ModelsListLocationsResponse{}, nil
		}
		return nil, mapModelAPIError("list locations", "", err)
	}
	seen := map[string]struct{}{}
	var locs []string
	for i := range list.Items {
		item := list.Items[i]
		if !isAIGridMemberCluster(item) {
			continue
		}
		loc := item.GetLabels()["location"]
		if loc == "" {
			continue
		}
		if _, ok := seen[loc]; ok {
			continue
		}
		seen[loc] = struct{}{}
		locs = append(locs, loc)
	}
	sort.Strings(locs)
	return &aigridv1.ModelsListLocationsResponse{Locations: locs}, nil
}

func isAIGridMemberCluster(item unstructured.Unstructured) bool {
	labels := item.GetLabels()
	if labels["ai-grid/enabled"] != "true" || labels["ai-grid/provisioned"] != "true" {
		return false
	}
	status := managedClusterStatusFromUnstructured(item)
	return status != nil && status.GetAvailable()
}

// enrichPlacementLocations fills status.placement.locations from decided clusters.
func (s *ModelsServer) enrichPlacementLocations(ctx context.Context, m *aigridv1.Model) {
	if m == nil || m.Status == nil || m.Status.Placement == nil {
		return
	}
	clusters := m.Status.Placement.GetSelectedClusters()
	if len(clusters) == 0 {
		return
	}
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	byName := map[string]string{}
	for i := range list.Items {
		name := list.Items[i].GetName()
		if loc := list.Items[i].GetLabels()["location"]; loc != "" {
			byName[name] = loc
		}
	}
	seen := map[string]struct{}{}
	var locs []string
	for _, name := range clusters {
		loc := byName[name]
		if loc == "" {
			continue
		}
		if _, ok := seen[loc]; ok {
			continue
		}
		seen[loc] = struct{}{}
		locs = append(locs, loc)
	}
	sort.Strings(locs)
	m.Status.Placement.Locations = locs
}

// enrichManifestWorks fills status.rollout.works from ManifestWorks owned by
// this model's MWRS (cluster = work namespace). Selectors use the MWRS
// namespace-qualified owner labels so another MWRS with the same placement
// name in a different namespace cannot contribute works.
func (s *ModelsServer) enrichManifestWorks(ctx context.Context, m *aigridv1.Model, pname string) {
	if m == nil || m.Status == nil || m.Status.Rollout == nil {
		return
	}
	legacyOwner := s.namespace + "." + pname
	list := s.listManifestWorksBySelector(ctx, labelMWRSLegacy+"="+legacyOwner)
	if list == nil || len(list.Items) == 0 {
		// Fall back to placementname, but drop works that carry a different
		// MWRS owner label (another MWRS with the same placement name).
		list = s.listManifestWorksBySelector(ctx, labelPlacementName+"="+pname)
		if list != nil {
			filtered := list.Items[:0]
			for i := range list.Items {
				labels := list.Items[i].GetLabels()
				if v, ok := labels[labelMWRSLegacy]; ok && v != legacyOwner {
					continue
				}
				filtered = append(filtered, list.Items[i])
			}
			list.Items = filtered
		}
	}
	if list == nil {
		return
	}
	works := make([]*aigridv1.ModelWorkStatus, 0, len(list.Items))
	for i := range list.Items {
		works = append(works, modelWorkStatusFromManifestWork(list.Items[i]))
	}
	sort.SliceStable(works, func(i, j int) bool { return works[i].GetCluster() < works[j].GetCluster() })
	m.Status.Rollout.Works = works
}

func (s *ModelsServer) listManifestWorksBySelector(ctx context.Context, selector string) *unstructured.UnstructuredList {
	list, err := s.client.Resource(manifestWorksResource).Namespace("").List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil
	}
	return list
}

// createdObjectRef records an object this Create call successfully made so
// compensateCreate can delete only those UIDs (not preexisting GitOps objects).
type createdObjectRef struct {
	gvr  schema.GroupVersionResource
	name string
	uid  types.UID
}

func (s *ModelsServer) compensateCreate(ctx context.Context, refs []createdObjectRef) {
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		opts := metav1.DeleteOptions{}
		if r.uid != "" {
			uid := r.uid
			opts.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		_ = s.client.Resource(r.gvr).Namespace(s.namespace).Delete(ctx, r.name, opts)
	}
}

func (s *ModelsServer) requireMWRS(ctx context.Context) error {
	if s.mwrsEnabled != nil {
		if !*s.mwrsEnabled {
			return connect.NewError(connect.CodeFailedPrecondition, "ManifestWorkReplicaSet feature gate disabled on hub")
		}
		return nil
	}
	_, err := s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil && apierrors.IsNotFound(err) {
		return connect.NewError(connect.CodeFailedPrecondition, "ManifestWorkReplicaSet feature gate disabled on hub")
	}
	return nil
}

// placementOptionalSpecKeys are the Placement spec keys placementFromModel
// writes only when the model sets them.
var placementOptionalSpecKeys = []string{"clusterSets", "numberOfClusters", "tolerations"}

func (s *ModelsServer) syncPlacementSideEffects(ctx context.Context, model *aigridv1.Model, deployment *unstructured.Unstructured) error {
	id := modelID(model)
	pname := placementNameForModel(id)
	hasPlacement := model.GetSpec() != nil && model.GetSpec().GetPlacement() != nil

	if !hasPlacement {
		_ = s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).Delete(ctx, pname, metav1.DeleteOptions{})
		_ = s.client.Resource(placementsResource).Namespace(s.namespace).Delete(ctx, pname, metav1.DeleteOptions{})
		return nil
	}

	placement := placementFromModel(model, s.namespace)
	existingPlacement, err := s.client.Resource(placementsResource).Namespace(s.namespace).Get(ctx, pname, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := s.client.Resource(placementsResource).Namespace(s.namespace).Create(ctx, placement, metav1.CreateOptions{}); err != nil {
			return mapModelAPIError("create Placement", id, err)
		}
	} else if err != nil {
		return mapModelAPIError("get Placement", id, err)
	} else {
		spec, _, _ := unstructured.NestedMap(placement.Object, "spec")
		// The placement is replaced as a whole: a merge patch keeps keys it
		// does not mention, so the optional ones the model leaves out are
		// cleared explicitly (predicates is a list and replaced anyway).
		for _, key := range placementOptionalSpecKeys {
			if _, ok := spec[key]; !ok {
				spec[key] = nil
			}
		}
		patch, _ := json.Marshal(map[string]interface{}{"spec": spec})
		if _, err := s.client.Resource(placementsResource).Namespace(s.namespace).Patch(
			ctx, pname, types.MergePatchType, patch, metav1.PatchOptions{},
		); err != nil {
			return mapModelAPIError("update Placement", id, err)
		}
		_ = existingPlacement
	}

	mwrs := mwrsFromModel(model, s.namespace, deployment)
	existingMWRS, err := s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).Get(ctx, pname, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).Create(ctx, mwrs, metav1.CreateOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				return connect.NewError(connect.CodeFailedPrecondition, "ManifestWorkReplicaSet feature gate disabled on hub")
			}
			return mapModelAPIError("create ManifestWorkReplicaSet", id, err)
		}
	} else if err != nil {
		return mapModelAPIError("get ManifestWorkReplicaSet", id, err)
	} else {
		spec, _, _ := unstructured.NestedMap(mwrs.Object, "spec")
		patch, _ := json.Marshal(map[string]interface{}{"spec": spec})
		if _, err := s.client.Resource(manifestWorkReplicaSetsResource).Namespace(s.namespace).Patch(
			ctx, pname, types.MergePatchType, patch, metav1.PatchOptions{},
		); err != nil {
			return mapModelAPIError("update ManifestWorkReplicaSet", id, err)
		}
		_ = existingMWRS
	}
	return nil
}

func (s *ModelsServer) syncModelRefAnnotations(ctx context.Context, id string, model *aigridv1.Model) error {
	anns := map[string]string{}
	if model.GetMetadata() != nil {
		anns = model.GetMetadata().GetAnnotations()
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": stringMapToInterfaceMap(anns),
		},
	})
	if err != nil {
		return err
	}
	_, err = s.client.Resource(maasModelRefsResource).Namespace(s.namespace).Patch(
		ctx, id, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return mapModelAPIError("update MaaSModelRef", id, err)
	}
	return nil
}

func (s *ModelsServer) ensureNotReferenced(ctx context.Context, id string) error {
	key := modelRefKey(id, s.namespace)
	subs, err := s.client.Resource(maasSubscriptionsResource).Namespace(s.subscriptionNamespace).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list MaaSSubscription for delete guard: %w", err)
	}
	if subs != nil {
		for i := range subs.Items {
			refs, _, _ := unstructured.NestedSlice(subs.Items[i].Object, "spec", "modelRefs")
			for _, raw := range refs {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if modelRefKey(stringField(m, "name"), stringField(m, "namespace")) == key ||
					(stringField(m, "name") == id && (stringField(m, "namespace") == "" || stringField(m, "namespace") == s.namespace)) {
					return connect.Errorf(connect.CodeFailedPrecondition,
						"model %q is referenced by subscription %q; remove it before deleting", id, subs.Items[i].GetName())
				}
			}
		}
	}
	pols, err := s.client.Resource(maasAuthPoliciesResource).Namespace(s.subscriptionNamespace).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list MaaSAuthPolicy for delete guard: %w", err)
	}
	if pols != nil {
		for i := range pols.Items {
			refs, _, _ := unstructured.NestedSlice(pols.Items[i].Object, "spec", "modelRefs")
			for _, raw := range refs {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if modelRefKey(stringField(m, "name"), stringField(m, "namespace")) == key ||
					(stringField(m, "name") == id && (stringField(m, "namespace") == "" || stringField(m, "namespace") == s.namespace)) {
					return connect.Errorf(connect.CodeFailedPrecondition,
						"model %q is referenced by auth policy %q; remove it before deleting", id, pols.Items[i].GetName())
				}
			}
		}
	}
	return nil
}

type modelUpdatePaths struct {
	labels, annotations bool
	replicas            bool
	main                bool // whole spec.main
	mainImage           bool
	mainEnv             bool
	mainRequests        bool
	mainLimits          bool
	model               bool
	baseRefs            bool
	runtime             bool
	router              bool
	scaling             bool
	placement, rollout  bool
}

func (p modelUpdatePaths) anyLLM() bool {
	return p.replicas || p.main || p.mainImage || p.mainEnv || p.mainRequests || p.mainLimits ||
		p.model || p.baseRefs || p.runtime || p.router || p.scaling
}

func parseModelUpdateMask(mask *fieldmaskpb.FieldMask) (modelUpdatePaths, error) {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return modelUpdatePaths{}, connect.NewError(connect.CodeInvalidArgument, "update_mask is required")
	}
	var paths modelUpdatePaths
	for _, path := range mask.GetPaths() {
		p := strings.TrimPrefix(path, "object.")
		switch {
		case p == "metadata.labels" || strings.HasPrefix(p, "metadata.labels."):
			paths.labels = true
		case p == "metadata.annotations" || strings.HasPrefix(p, "metadata.annotations."):
			paths.annotations = true
		case p == "spec.replicas":
			paths.replicas = true
		case p == "spec.main":
			paths.main = true
		case p == "spec.main.image":
			paths.mainImage = true
		case p == "spec.main.env" || strings.HasPrefix(p, "spec.main.env."):
			paths.mainEnv = true
		case p == "spec.main.requests" || strings.HasPrefix(p, "spec.main.requests."):
			paths.mainRequests = true
		case p == "spec.main.limits" || strings.HasPrefix(p, "spec.main.limits."):
			paths.mainLimits = true
		case strings.HasPrefix(p, "spec.main."):
			return modelUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "update_mask path %q is not supported", path)
		case p == "spec.model" || strings.HasPrefix(p, "spec.model."):
			paths.model = true
		case p == "spec.base_refs":
			paths.baseRefs = true
		case p == "spec.runtime":
			paths.runtime = true
		case p == "spec.router" || strings.HasPrefix(p, "spec.router."):
			paths.router = true
		case p == "spec.scaling" || strings.HasPrefix(p, "spec.scaling."):
			paths.scaling = true
		case p == "spec.placement" || strings.HasPrefix(p, "spec.placement."):
			paths.placement = true
		case p == "spec.rollout" || strings.HasPrefix(p, "spec.rollout."):
			paths.rollout = true
		default:
			return modelUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "update_mask path %q is not supported", path)
		}
	}
	return paths, nil
}

func mergeModelUpdate(current, incoming *aigridv1.Model, paths modelUpdatePaths) (*aigridv1.Model, error) {
	out := proto.Clone(current).(*aigridv1.Model)
	if out == nil {
		out = &aigridv1.Model{Id: modelID(current)}
	}
	if out.Spec == nil {
		out.Spec = &aigridv1.ModelSpec{}
	}
	if paths.labels || paths.annotations {
		meta := out.GetMetadata()
		if meta == nil {
			meta = &aigridv1.Metadata{}
		} else {
			meta = proto.Clone(meta).(*aigridv1.Metadata)
		}
		if paths.labels {
			if incoming.GetMetadata() != nil {
				meta.Labels = incoming.GetMetadata().GetLabels()
			} else {
				meta.Labels = nil
			}
		}
		if paths.annotations {
			if incoming.GetMetadata() != nil {
				meta.Annotations = incoming.GetMetadata().GetAnnotations()
			} else {
				meta.Annotations = nil
			}
		}
		out.Metadata = meta
	}
	if !paths.anyLLM() && !paths.placement && !paths.rollout {
		return out, nil
	}
	in := incoming.GetSpec()
	if in == nil {
		in = &aigridv1.ModelSpec{}
	}
	if paths.main {
		// Whole-main replace with a nil container would merge-patch
		// {"name":"main"} onto the CR and wipe image/args/env/resources.
		if in.GetMain() == nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "spec.main is required when update_mask includes spec.main")
		}
		out.Spec.Main = in.GetMain()
	} else if paths.mainImage || paths.mainEnv || paths.mainRequests || paths.mainLimits {
		main := out.Spec.GetMain()
		if main == nil {
			main = &aigridv1.ModelContainer{}
		} else {
			main = proto.Clone(main).(*aigridv1.ModelContainer)
		}
		inMain := in.GetMain()
		if inMain == nil {
			inMain = &aigridv1.ModelContainer{}
		}
		if paths.mainImage {
			main.Image = inMain.GetImage()
		}
		if paths.mainEnv {
			main.Env = inMain.GetEnv()
		}
		if paths.mainRequests {
			main.Requests = inMain.GetRequests()
		}
		if paths.mainLimits {
			main.Limits = inMain.GetLimits()
		}
		out.Spec.Main = main
	}
	if paths.model {
		out.Spec.Model = in.GetModel()
	}
	if paths.baseRefs {
		out.Spec.BaseRefs = in.GetBaseRefs()
	}
	if paths.runtime {
		out.Spec.Runtime = in.GetRuntime()
	}
	if paths.router {
		out.Spec.Router = in.GetRouter()
	}
	if paths.scaling {
		out.Spec.Scaling = in.GetScaling()
		out.Spec.Replicas = nil
	}
	if paths.replicas {
		out.Spec.Replicas = in.Replicas
		out.Spec.Scaling = nil
	}
	if paths.placement {
		out.Spec.Placement = in.GetPlacement()
		if out.Spec.Placement == nil && !paths.rollout {
			out.Spec.Rollout = nil
		}
	}
	if paths.rollout {
		out.Spec.Rollout = in.GetRollout()
	}
	return out, nil
}

func validateModelID(id string) error {
	if id == "" {
		return connect.NewError(connect.CodeInvalidArgument, "id is required")
	}
	if !modelNameRE.MatchString(id) {
		return connect.Errorf(connect.CodeInvalidArgument, "id %q is not a valid RFC 1123 DNS label", id)
	}
	return nil
}

func validateModelSpec(spec *aigridv1.ModelSpec) error {
	if spec == nil {
		return connect.NewError(connect.CodeInvalidArgument, "spec is required")
	}
	if spec.GetModel() == nil || strings.TrimSpace(spec.GetModel().GetUri()) == "" {
		return connect.NewError(connect.CodeInvalidArgument, "spec.model.uri is required")
	}
	uri := spec.GetModel().GetUri()
	if !modelURIHasAllowedScheme(uri) {
		return connect.Errorf(connect.CodeInvalidArgument, "spec.model.uri %q must use hf://, oci://, s3://, pvc://, http://, or https://", uri)
	}
	hasReplicas := spec.Replicas != nil
	hasScaling := spec.GetScaling() != nil
	if hasReplicas && hasScaling {
		return connect.NewError(connect.CodeInvalidArgument, "replicas and scaling are mutually exclusive; use scaling for autoscaled deployments or replicas for static deployments")
	}
	if !hasReplicas && !hasScaling {
		return connect.NewError(connect.CodeInvalidArgument, "spec.replicas or spec.scaling is required")
	}
	if hasReplicas {
		r := *spec.Replicas
		if r < 0 || r > 99 {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.replicas must be between 0 and 99, got %d", r)
		}
	}
	if hasScaling {
		sc := spec.GetScaling()
		if sc.GetMaxReplicas() < 1 {
			return connect.NewError(connect.CodeInvalidArgument, "spec.scaling.maxReplicas is required and must be at least 1")
		}
		min := sc.GetMinReplicas()
		if min <= 0 {
			min = 1
		}
		if min > sc.GetMaxReplicas() {
			return connect.NewError(connect.CodeInvalidArgument, "minReplicas cannot exceed maxReplicas")
		}
		if !sc.GetKeda() {
			return connect.NewError(connect.CodeInvalidArgument, "either wva or keda must be specified when scaling is configured")
		}
		if len(sc.GetTriggers()) == 0 {
			return connect.NewError(connect.CodeInvalidArgument, "at least one trigger is required when using direct KEDA scaling")
		}
	}
	if main := spec.GetMain(); main != nil {
		img := strings.TrimSpace(main.GetImage())
		if img == "" {
			return connect.NewError(connect.CodeInvalidArgument, "spec.main.image is required when spec.main is set")
		}
		if strings.ContainsAny(img, " \t") {
			return connect.NewError(connect.CodeInvalidArgument, "spec.main.image must not contain whitespace")
		}
	}
	if placement := spec.GetPlacement(); placement != nil {
		if err := validateRequirements("spec.placement.label_selector.match_expressions", placement.GetLabelSelector().GetMatchExpressions()); err != nil {
			return err
		}
		if err := validateRequirements("spec.placement.claim_expressions", placement.GetClaimExpressions()); err != nil {
			return err
		}
		if err := validateTolerations(placement.GetTolerations()); err != nil {
			return err
		}
		if placement.NumberOfClusters != nil && *placement.NumberOfClusters < 0 {
			return connect.NewError(connect.CodeInvalidArgument, "spec.placement.number_of_clusters must be non-negative")
		}
	}
	if rollout := spec.GetRollout(); rollout != nil && spec.GetPlacement() == nil {
		return connect.NewError(connect.CodeInvalidArgument, "spec.rollout requires spec.placement")
	}
	return nil
}

// validateRequirements applies metav1.LabelSelectorRequirement's rules
// (k8s.io/apimachinery pkg/apis/meta/v1/validation
// ValidateLabelSelectorRequirement; the claim selector uses the same type,
// open-cluster-management-io/api cluster/v1beta1/types_placement.go
// ClusterClaimSelector): the key is a qualified name, In and NotIn need at
// least one value, Exists and DoesNotExist take none.
func validateRequirements(path string, reqs []*aigridv1.ModelLabelSelectorRequirement) error {
	for i, e := range reqs {
		if e == nil {
			continue
		}
		if errs := k8svalidation.IsQualifiedName(e.GetKey()); len(errs) > 0 {
			return connect.Errorf(connect.CodeInvalidArgument, "%s[%d].key %q is invalid: %s", path, i, e.GetKey(), strings.Join(errs, "; "))
		}
		switch e.GetOperator() {
		case "In", "NotIn":
			if len(e.GetValues()) == 0 {
				return connect.Errorf(connect.CodeInvalidArgument, "%s[%d]: operator %s needs at least one value", path, i, e.GetOperator())
			}
		case "Exists", "DoesNotExist":
			if len(e.GetValues()) > 0 {
				return connect.Errorf(connect.CodeInvalidArgument, "%s[%d]: operator %s takes no values", path, i, e.GetOperator())
			}
		default:
			return connect.Errorf(connect.CodeInvalidArgument, "%s[%d].operator %q is invalid", path, i, e.GetOperator())
		}
	}
	return nil
}

// tolerationKeyRE and maxTolerationKeyLength are the Placement CRD's
// validation of spec.tolerations[].key (open-cluster-management-io/api
// cluster/v1beta1/types_placement.go Toleration.Key: kubebuilder Pattern and
// MaxLength=316): an optional DNS-subdomain prefix and a slash, then a name.
var tolerationKeyRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$`)

const maxTolerationKeyLength = 316

// validateTolerations applies the Placement Toleration rules
// (open-cluster-management-io/api cluster/v1beta1/types_placement.go): the
// operator is Equal (the CRD default when empty) or Exists, an empty key needs
// Exists, Exists takes no value, and the effect, when set, is NoSelect,
// PreferNoSelect or NoSelectIfNew (the CRD enum).
func validateTolerations(tolerations []*aigridv1.ModelToleration) error {
	for i, t := range tolerations {
		if t == nil {
			continue
		}
		if k := t.GetKey(); k != "" && (len(k) > maxTolerationKeyLength || !tolerationKeyRE.MatchString(k)) {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.placement.tolerations[%d].key %q is not a valid taint key", i, k)
		}
		switch t.GetOperator() {
		case "", "Equal", "Exists":
		default:
			return connect.Errorf(connect.CodeInvalidArgument, "spec.placement.tolerations[%d].operator %q is invalid; use Equal or Exists", i, t.GetOperator())
		}
		if t.GetKey() == "" && t.GetOperator() != "Exists" {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.placement.tolerations[%d]: an empty key requires operator Exists", i)
		}
		if t.GetOperator() == "Exists" && t.GetValue() != "" {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.placement.tolerations[%d]: operator Exists takes no value", i)
		}
		switch t.GetEffect() {
		case "", "NoSelect", "PreferNoSelect", "NoSelectIfNew":
		default:
			return connect.Errorf(connect.CodeInvalidArgument, "spec.placement.tolerations[%d].effect %q is invalid; use NoSelect, PreferNoSelect or NoSelectIfNew", i, t.GetEffect())
		}
	}
	return nil
}

func modelURIHasAllowedScheme(uri string) bool {
	lower := strings.ToLower(uri)
	for _, scheme := range modelURISchemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

func staleModelError(op, id string) *connect.Error {
	return connect.Errorf(connect.CodeAborted, "%s Model %q: the model changed since it was read; reload and retry", op, id)
}

func mapModelAPIError(op, id string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return connect.Errorf(connect.CodeNotFound, "Model %q not found", id).WithCause(err)
	case apierrors.IsAlreadyExists(err):
		return connect.Errorf(connect.CodeAlreadyExists, "Model %q already exists", id).WithCause(err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return connect.Errorf(connect.CodeInvalidArgument, "%s Model %q: %v", op, id, err).WithCause(err)
	case apierrors.IsConflict(err):
		return staleModelError(op, id).WithCause(err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return connect.Errorf(connect.CodePermissionDenied, "%s Model %q: %v", op, id, err).WithCause(err)
	default:
		return connect.Errorf(connect.CodeInternal, "%s Model %q: %v", op, id, err).WithCause(err)
	}
}
