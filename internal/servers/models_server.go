package servers

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/celfilter"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// ModelsServerBuilder configures a ModelsServer.
type ModelsServerBuilder struct {
	logger                *slog.Logger
	client                dynamic.Interface
	writer                gitops.Writer
	status                *CatalogStatusReader
	gridNamespace         string
	namespace             string
	subscriptionNamespace string
	readyPolicy           string
}

// ModelsServer implements ai_grid.v1.Models over a Git repository: each model
// is an LLMInferenceService that Argo CD delivers to the clusters its
// placement names, and an ExternalModel and MaaSModelRef for the ingress
// clusters (models_git.go). It never writes to the cluster; it reads the
// ManagedClusters for ListPlacementClusters, status.placement.locations and
// the check that a placement names existing clusters, and status from the hub
// through its CatalogStatusReader.
type ModelsServer struct {
	aigridv1connect.UnimplementedModelsHandler
	logger                *slog.Logger
	client                dynamic.Interface
	writer                gitops.Writer
	status                *CatalogStatusReader
	gridNamespace         string
	namespace             string
	subscriptionNamespace string
	readyPolicyNamespace  string
	readyPolicyName       string
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

// SetWriter sets the Git repository models are read from and committed to.
// Mandatory.
func (b *ModelsServerBuilder) SetWriter(writer gitops.Writer) *ModelsServerBuilder {
	b.writer = writer
	return b
}

// SetDynamicClient sets the hub client that ListPlacementClusters (and the
// deprecated ListLocations), status.placement.locations and the placement's
// cluster check read ManagedClusters with. Optional; without it the lists are
// empty and placements are not checked.
func (b *ModelsServerBuilder) SetDynamicClient(client dynamic.Interface) *ModelsServerBuilder {
	b.client = client
	return b
}

// SetCatalogStatus sets where status is read from the spokes. Optional;
// without it every model is saved to Git.
func (b *ModelsServerBuilder) SetCatalogStatus(status *CatalogStatusReader) *ModelsServerBuilder {
	b.status = status
	return b
}

// SetGridNamespace sets the namespace of the grid on the inference spokes,
// which each model's NetworkPolicy lets reach the model. Default
// redhat-ai-grid.
func (b *ModelsServerBuilder) SetGridNamespace(namespace string) *ModelsServerBuilder {
	b.gridNamespace = namespace
	return b
}

// SetNamespace sets the namespace of the LLMInferenceService on the inference
// spokes. Default ai-grid-internal-models.
func (b *ModelsServerBuilder) SetNamespace(namespace string) *ModelsServerBuilder {
	b.namespace = namespace
	return b
}

// SetSubscriptionNamespace sets the namespace of the ExternalModel and
// MaaSModelRef on the ingress spokes, where subscriptions' model refs point.
// Default models-as-a-service.
func (b *ModelsServerBuilder) SetSubscriptionNamespace(namespace string) *ModelsServerBuilder {
	b.subscriptionNamespace = namespace
	return b
}

// Build creates the ModelsServer.
// SetReadyPolicy sets the hub Policy, as namespace/name, whose compliance says
// a cluster is provisioned. Optional; DefaultReadyPolicy when empty.
func (b *ModelsServerBuilder) SetReadyPolicy(namespacedName string) *ModelsServerBuilder {
	b.readyPolicy = namespacedName
	return b
}

func (b *ModelsServerBuilder) Build() (*ModelsServer, error) {
	if b.writer == nil {
		return nil, errors.New("git writer is mandatory")
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
	gridNS := b.gridNamespace
	if gridNS == "" {
		gridNS = defaultGridNamespace
	}
	readyNS, readyName, err := splitReadyPolicy(b.readyPolicy)
	if err != nil {
		return nil, err
	}
	return &ModelsServer{
		readyPolicyNamespace:  readyNS,
		readyPolicyName:       readyName,
		logger:                logger,
		client:                b.client,
		writer:                b.writer,
		status:                b.status,
		gridNamespace:         gridNS,
		namespace:             namespace,
		subscriptionNamespace: subNS,
	}, nil
}

// List returns the repository's models.
func (s *ModelsServer) List(ctx context.Context, req *aigridv1.ModelsListRequest) (*aigridv1.ModelsListResponse, error) {
	if err := validateListLimit(req.Limit); err != nil {
		return nil, err
	}
	items, err := s.gitList(ctx)
	if err != nil {
		return nil, err
	}
	s.enrichGitLocations(ctx, items...)
	return listModelItems(ctx, req, items)
}

// Get returns one model by id.
func (s *ModelsServer) Get(ctx context.Context, req *aigridv1.ModelsGetRequest) (*aigridv1.ModelsGetResponse, error) {
	id := req.GetId()
	if err := validateModelID(id); err != nil {
		return nil, err
	}
	m, err := s.gitGet(ctx, id)
	if err != nil {
		return nil, err
	}
	s.enrichGitLocations(ctx, m)
	return &aigridv1.ModelsGetResponse{Object: m}, nil
}

// Create commits a model's deployment and catalog entry, and adds it to the
// indexes of the clusters its placement names.
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

	normalizeModelResources(object.GetSpec().GetMain())
	if err := validateModelSpec(object.GetSpec()); err != nil {
		return nil, err
	}
	if err := normalizeServedName(id, object.GetSpec()); err != nil {
		return nil, err
	}
	m, err := s.gitCreate(ctx, object)
	if err != nil {
		return nil, err
	}
	s.enrichGitLocations(ctx, m)
	return &aigridv1.ModelsCreateResponse{Object: m}, nil
}

// Update commits the fields the mask selects, and moves the model between
// cluster indexes when its placement changes.
func (s *ModelsServer) Update(ctx context.Context, req *aigridv1.ModelsUpdateRequest) (*aigridv1.ModelsUpdateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "object is required")
	}
	if err := validateModelID(modelID(object)); err != nil {
		return nil, err
	}
	m, err := s.gitUpdate(ctx, req)
	if err != nil {
		return nil, err
	}
	s.enrichGitLocations(ctx, m)
	return &aigridv1.ModelsUpdateResponse{Object: m}, nil
}

// Delete removes the model's files and index entries. A nonzero
// resource_version must equal the stored metadata.version, or the delete is
// Aborted; zero skips the check. It is FailedPrecondition while a
// subscription names the model.
func (s *ModelsServer) Delete(ctx context.Context, req *aigridv1.ModelsDeleteRequest) (*aigridv1.ModelsDeleteResponse, error) {
	id := req.GetId()
	if err := validateModelID(id); err != nil {
		return nil, err
	}
	if err := s.gitDelete(ctx, id, req.GetResourceVersion()); err != nil {
		return nil, err
	}
	return &aigridv1.ModelsDeleteResponse{}, nil
}

// listModelItems applies the request's filter, order and window to items.
func listModelItems(ctx context.Context, req *aigridv1.ModelsListRequest, items []*aigridv1.Model) (*aigridv1.ModelsListResponse, error) {
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

// ListPlacementClusters returns the unique sorted names of the clusters a
// model can be placed on: the values a placement's name In [...] takes, since
// git mode places a model by cluster name. A cluster qualifies when it is
// labelled ai-grid/enabled=true and ai-grid/role inference or combined, is
// Available, and is Compliant with the ready Policy (provisioned).
func (s *ModelsServer) ListPlacementClusters(ctx context.Context, _ *aigridv1.ModelsListPlacementClustersRequest) (*aigridv1.ModelsListPlacementClustersResponse, error) {
	names, err := s.placementClusters(ctx)
	if err != nil {
		return nil, err
	}
	return &aigridv1.ModelsListPlacementClustersResponse{Clusters: names}, nil
}

// ListLocations is the deprecated name of ListPlacementClusters: the same
// cluster names, as locations (docs/API_CONTRACT.md, expand step).
func (s *ModelsServer) ListLocations(ctx context.Context, _ *aigridv1.ModelsListLocationsRequest) (*aigridv1.ModelsListLocationsResponse, error) {
	names, err := s.placementClusters(ctx)
	if err != nil {
		return nil, err
	}
	return &aigridv1.ModelsListLocationsResponse{Locations: names}, nil
}

// placementClusters answers ListPlacementClusters and ListLocations.
func (s *ModelsServer) placementClusters(ctx context.Context) ([]string, error) {
	if s.client == nil {
		return nil, nil
	}
	items, err := s.listManagedClusters(ctx)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	provisioned := readProvisioning(ctx, s.client, s.logger, s.readyPolicyNamespace, s.readyPolicyName)
	var names []string
	for i := range items {
		if isPlacementCluster(items[i], provisioned) {
			names = append(names, items[i].GetName())
		}
	}
	sort.Strings(names)
	return slices.Compact(names), nil
}

// listManagedClusters lists the ManagedClusters on the hub. A hub without the
// ManagedCluster API (NotFound) has no cluster, so the list is empty. Any
// other error is Internal, as in Clusters/List: the backend lists with its own
// credentials, so a failure, Forbidden included, is not the caller's to fix.
func (s *ModelsServer) listManagedClusters(ctx context.Context) ([]unstructured.Unstructured, error) {
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, connect.Errorf(connect.CodeInternal, "list ManagedCluster resources: %v", err).WithCause(err)
	}
	return list.Items, nil
}

// checkPlacementClusters answers InvalidArgument naming each cluster in added
// that is not a ManagedCluster on the hub. A placement names clusters, and a
// name no cluster has (a location label, a typo) would commit a model that
// Argo CD delivers nowhere, with no error. Only the clusters a create or
// update adds are checked, so a model on a cluster that has since left the
// hub can still be changed or moved off it. Without a dynamic client there is
// nothing to check against.
//
// The error carries a google.rpc.ResourceInfo detail for each unknown cluster,
// in the order of added (resource_type managedClusterResourceType,
// resource_name the cluster), so a client can find the names without parsing
// the message.
func (s *ModelsServer) checkPlacementClusters(ctx context.Context, added []string) error {
	if s.client == nil || len(added) == 0 {
		return nil
	}
	items, err := s.listManagedClusters(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for i := range items {
		known[items[i].GetName()] = true
	}
	var unknown, quoted []string
	for _, cluster := range added {
		if !known[cluster] {
			unknown = append(unknown, cluster)
			quoted = append(quoted, strconv.Quote(cluster))
		}
	}
	var cerr *connect.Error
	switch len(unknown) {
	case 0:
		return nil
	case 1:
		cerr = connect.Errorf(connect.CodeInvalidArgument, "spec.placement names cluster %s, which is not a ManagedCluster on the hub; a placement takes cluster names (Models/ListPlacementClusters), not location labels", quoted[0])
	default:
		cerr = connect.Errorf(connect.CodeInvalidArgument, "spec.placement names clusters %s, which are not ManagedClusters on the hub; a placement takes cluster names (Models/ListPlacementClusters), not location labels", strings.Join(quoted, ", "))
	}
	for _, cluster := range unknown {
		// Marshalling a ResourceInfo does not fail; if it did, the message
		// still names the cluster.
		if detail, err := connectproto.NewErrorDetail(&errdetails.ResourceInfo{
			ResourceType: managedClusterResourceType,
			ResourceName: cluster,
			Description:  "not a ManagedCluster on the hub",
		}); err == nil {
			cerr = cerr.WithDetail(detail)
		}
	}
	return cerr
}

// managedClusterResourceType is the google.rpc.ResourceInfo resource_type of
// a cluster: the type URL of ai_grid.v1.ManagedCluster, whose id is the
// cluster name.
var managedClusterResourceType = "type.googleapis.com/" + string((&aigridv1.ManagedCluster{}).ProtoReflect().Descriptor().FullName())

// isPlacementCluster reports whether a model can be placed on the cluster
// item, by the rules ListPlacementClusters documents.
func isPlacementCluster(item unstructured.Unstructured, provisioned map[string]string) bool {
	labels := item.GetLabels()
	if labels["ai-grid/enabled"] != "true" || provisioned[item.GetName()] != "Compliant" {
		return false
	}
	if role := labels["ai-grid/role"]; role != "inference" && role != "combined" {
		return false
	}
	status := managedClusterStatusFromUnstructured(item)
	return status != nil && status.GetAvailable()
}

// enrichGitLocations fills status.placement.locations of models from the
// location labels of the clusters their files place them on. Without a dynamic client it leaves
// them empty.
func (s *ModelsServer) enrichGitLocations(ctx context.Context, models ...*aigridv1.Model) {
	if s.client == nil || len(models) == 0 {
		return
	}
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		s.logger.Warn("list ManagedClusters for model locations", "error", err)
		return
	}
	byName := map[string]string{}
	for i := range list.Items {
		if loc := list.Items[i].GetLabels()["location"]; loc != "" {
			byName[list.Items[i].GetName()] = loc
		}
	}
	for _, m := range models {
		placement := m.GetStatus().GetPlacement()
		if placement == nil {
			continue
		}
		var locs []string
		for _, cluster := range placement.GetSelectedClusters() {
			if loc := byName[cluster]; loc != "" && !slices.Contains(locs, loc) {
				locs = append(locs, loc)
			}
		}
		sort.Strings(locs)
		placement.Locations = locs
	}
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
			if !paths.mainLimits {
				dropDerivedLimits(out.Spec.GetMain(), main)
			}
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

// baseRefRE is the buf.validate pattern on ModelSpec.base_refs items.
var baseRefRE = regexp.MustCompile(`^\S+$`)

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
	for i, ref := range spec.GetBaseRefs() {
		// The buf.validate pattern on ModelSpec.base_refs. A blank name would
		// count as a preset below and read back as none (model_mapper.go).
		if !baseRefRE.MatchString(ref) {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.base_refs[%d] %q must be a preset name with no whitespace", i, ref)
		}
	}
	if main := spec.GetMain(); main != nil {
		// With base_refs the presets may provide the image (ModelContainer.image
		// in model_type.proto); without them nothing does.
		img := strings.TrimSpace(main.GetImage())
		if img == "" && (main.GetImage() != "" || len(spec.GetBaseRefs()) == 0) {
			return connect.NewError(connect.CodeInvalidArgument, "spec.main.image is required when spec.base_refs names no preset")
		}
		if strings.ContainsAny(img, " \t") {
			return connect.NewError(connect.CodeInvalidArgument, "spec.main.image must not contain whitespace")
		}
		if err := validateModelResources(main); err != nil {
			return err
		}
	}
	if placement := spec.GetPlacement(); placement != nil {
		sel := placement.GetLabelSelector()
		if sel != nil {
			for i, e := range sel.GetMatchExpressions() {
				if e == nil {
					continue
				}
				op := e.GetOperator()
				switch op {
				case "In", "NotIn", "Exists", "DoesNotExist":
				default:
					return connect.Errorf(connect.CodeInvalidArgument, "spec.placement.label_selector.match_expressions[%d].operator %q is invalid", i, op)
				}
			}
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
