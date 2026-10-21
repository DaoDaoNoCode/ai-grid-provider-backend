package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var managedClustersResource = schema.GroupVersionResource{
	Group: "cluster.open-cluster-management.io", Version: "v1", Resource: "managedclusters",
}

// RFC 1123 DNS label: 1–63 chars, lowercase alphanumeric, hyphens allowed inside.
var managedClusterNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var policiesResource = schema.GroupVersionResource{
	Group: "policy.open-cluster-management.io", Version: "v1", Resource: "policies",
}

// DefaultReadyPolicy is the hub Policy, as namespace/name, whose compliance
// for a cluster is its AI Grid provisioning state.
const DefaultReadyPolicy = "ai-grid-policies/ai-grid-ready"

// ClustersServerBuilder configures a ClustersServer.
type ClustersServerBuilder struct {
	logger      *slog.Logger
	client      dynamic.Interface
	readyPolicy string
}

// ClustersServer implements ai_grid.v1.Clusters against hub ManagedCluster CRs.
type ClustersServer struct {
	aigridv1.UnimplementedClustersServer
	logger               *slog.Logger
	client               dynamic.Interface
	readyPolicyNamespace string
	readyPolicyName      string
}

var _ aigridv1.ClustersServer = (*ClustersServer)(nil)

// NewClustersServer creates a builder for ClustersServer.
func NewClustersServer() *ClustersServerBuilder {
	return &ClustersServerBuilder{}
}

// SetLogger sets the logger. Optional.
func (b *ClustersServerBuilder) SetLogger(logger *slog.Logger) *ClustersServerBuilder {
	b.logger = logger
	return b
}

// SetDynamicClient sets the Kubernetes dynamic client. Mandatory.
func (b *ClustersServerBuilder) SetDynamicClient(client dynamic.Interface) *ClustersServerBuilder {
	b.client = client
	return b
}

// SetReadyPolicy sets the hub Policy, as namespace/name, that reports each
// cluster's provisioning. Optional; DefaultReadyPolicy when empty.
func (b *ClustersServerBuilder) SetReadyPolicy(namespacedName string) *ClustersServerBuilder {
	b.readyPolicy = namespacedName
	return b
}

// Build creates the ClustersServer.
func (b *ClustersServerBuilder) Build() (*ClustersServer, error) {
	if b.client == nil {
		return nil, errors.New("kubernetes dynamic client is mandatory")
	}
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	readyPolicy := b.readyPolicy
	if readyPolicy == "" {
		readyPolicy = DefaultReadyPolicy
	}
	namespace, name, ok := strings.Cut(readyPolicy, "/")
	if !ok || namespace == "" || name == "" {
		return nil, fmt.Errorf("ready policy %q must be namespace/name", readyPolicy)
	}
	return &ClustersServer{logger: logger, client: b.client, readyPolicyNamespace: namespace, readyPolicyName: name}, nil
}

// List returns ManagedCluster objects from the Kubernetes API.
func (s *ClustersServer) List(ctx context.Context, req *aigridv1.ClustersListRequest) (*aigridv1.ClustersListResponse, error) {
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &aigridv1.ClustersListResponse{}, nil
		}
		return nil, fmt.Errorf("list ManagedCluster resources: %w", err)
	}

	provisioning := s.provisioning(ctx)
	items := make([]*aigridv1.ManagedCluster, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, s.withProvisioning(managedClusterFromUnstructured(item), provisioning))
	}
	page, total := applyListWindow(items, req.Offset, req.Limit)
	return &aigridv1.ClustersListResponse{
		Size:  int32(len(page)),
		Total: total,
		Items: page,
	}, nil
}

// Get returns a single ManagedCluster by id.
func (s *ClustersServer) Get(ctx context.Context, req *aigridv1.ClustersGetRequest) (*aigridv1.ClustersGetResponse, error) {
	id := req.GetId()
	if err := validateManagedClusterID(id); err != nil {
		return nil, err
	}
	obj, err := s.client.Resource(managedClustersResource).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, mapManagedClusterAPIError("get", id, err)
	}
	return &aigridv1.ClustersGetResponse{Object: s.withProvisioning(managedClusterFromUnstructured(*obj), s.provisioning(ctx))}, nil
}

// Create creates a hub-side ManagedCluster CR.
func (s *ClustersServer) Create(ctx context.Context, req *aigridv1.ClustersCreateRequest) (*aigridv1.ClustersCreateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, status.Error(codes.InvalidArgument, "object is required")
	}
	id := managedClusterID(object)
	if err := validateManagedClusterID(id); err != nil {
		return nil, err
	}

	hubAccepts := false
	if object.GetSpec() != nil {
		hubAccepts = object.GetSpec().GetHubAcceptsClient()
	}
	labels := map[string]string{}
	if object.GetMetadata() != nil {
		for k, v := range object.GetMetadata().GetLabels() {
			labels[k] = v
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cluster.open-cluster-management.io/v1",
		"kind":       "ManagedCluster",
		"metadata": map[string]interface{}{
			"name": id,
		},
		"spec": map[string]interface{}{
			"hubAcceptsClient": hubAccepts,
		},
	}}
	if len(labels) > 0 {
		_ = unstructured.SetNestedMap(obj.Object, stringMapToInterfaceMap(labels), "metadata", "labels")
	}

	created, err := s.client.Resource(managedClustersResource).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, mapManagedClusterAPIError("create", id, err)
	}
	return &aigridv1.ClustersCreateResponse{Object: managedClusterFromUnstructured(*created)}, nil
}

// Update merge-patches labels and/or hubAcceptsClient according to update_mask.
// An empty mask updates the mutable fields the request sets: labels merge when
// metadata is set, hubAcceptsClient when spec is set.
func (s *ClustersServer) Update(ctx context.Context, req *aigridv1.ClustersUpdateRequest) (*aigridv1.ClustersUpdateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, status.Error(codes.InvalidArgument, "object is required")
	}
	id := managedClusterID(object)
	if err := validateManagedClusterID(id); err != nil {
		return nil, err
	}

	current, err := s.client.Resource(managedClustersResource).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, mapManagedClusterAPIError("get", id, err)
	}

	var version int32
	if object.GetMetadata() != nil {
		version = object.GetMetadata().GetVersion()
	}
	if req.GetLock() && version == 0 {
		return nil, status.Error(codes.InvalidArgument, "lock requires metadata.version")
	}
	// A stale version is Aborted before anything else is checked: a client
	// sends back the labels it read, and ACM may have changed some since.
	if version != 0 && strconv.FormatInt(int64(version), 10) != current.GetResourceVersion() {
		return nil, staleClusterError("update", id)
	}

	paths := updatePaths(req.GetUpdateMask(), object)

	patchMeta := map[string]interface{}{}
	if paths.labels {
		if err := checkConsoleKeys("label", current.GetLabels(), object.GetMetadata().GetLabels()); err != nil {
			return nil, err
		}
		patchMeta["labels"] = managedKeysPatch("label", current.GetLabels(), object.GetMetadata().GetLabels())
	}
	if paths.annotations {
		if err := checkConsoleKeys("annotation", current.GetAnnotations(), object.GetMetadata().GetAnnotations()); err != nil {
			return nil, err
		}
		if err := checkCoordinates(object.GetMetadata().GetAnnotations()); err != nil {
			return nil, err
		}
		patchMeta["annotations"] = managedKeysPatch("annotation", current.GetAnnotations(), object.GetMetadata().GetAnnotations())
	}
	patchBody := map[string]interface{}{}
	if len(patchMeta) > 0 {
		patchBody["metadata"] = patchMeta
	}
	if paths.hub {
		hubAccepts := false
		if object.GetSpec() != nil {
			hubAccepts = object.GetSpec().GetHubAcceptsClient()
		}
		patchBody["spec"] = map[string]interface{}{
			"hubAcceptsClient": hubAccepts,
		}
	}
	if len(patchBody) == 0 {
		return &aigridv1.ClustersUpdateResponse{Object: managedClusterFromUnstructured(*current)}, nil
	}
	// metadata.version is the hub resourceVersion: sent back in the merge
	// patch, the API server rejects the write with a Conflict when it is stale.
	if version != 0 {
		meta, _ := patchBody["metadata"].(map[string]interface{})
		if meta == nil {
			meta = map[string]interface{}{}
			patchBody["metadata"] = meta
		}
		meta["resourceVersion"] = strconv.FormatInt(int64(version), 10)
	}

	patchBytes, err := json.Marshal(patchBody)
	if err != nil {
		return nil, fmt.Errorf("marshal ManagedCluster patch: %w", err)
	}

	updated, err := s.client.Resource(managedClustersResource).Patch(
		ctx, id, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		return nil, mapManagedClusterAPIError("update", id, err)
	}
	return &aigridv1.ClustersUpdateResponse{Object: s.withProvisioning(managedClusterFromUnstructured(*updated), s.provisioning(ctx))}, nil
}

// Delete deletes a ManagedCluster by id.
func (s *ClustersServer) Delete(ctx context.Context, req *aigridv1.ClustersDeleteRequest) (*aigridv1.ClustersDeleteResponse, error) {
	id := req.GetId()
	if err := validateManagedClusterID(id); err != nil {
		return nil, err
	}
	err := s.client.Resource(managedClustersResource).Delete(ctx, id, metav1.DeleteOptions{})
	if err != nil {
		return nil, mapManagedClusterAPIError("delete", id, err)
	}
	return &aigridv1.ClustersDeleteResponse{}, nil
}

type clusterUpdatePaths struct {
	labels, annotations, hub bool
}

// provisioning reads the ready Policy's per-cluster compliance from the root
// Policy's status.status. A missing or unreadable Policy gives an empty map,
// so clusters show no provisioning state rather than failing the request.
func (s *ClustersServer) provisioning(ctx context.Context) map[string]string {
	policy, err := s.client.Resource(policiesResource).Namespace(s.readyPolicyNamespace).Get(ctx, s.readyPolicyName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			s.logger.Warn("read ready policy", "policy", s.readyPolicyNamespace+"/"+s.readyPolicyName, "error", err)
		}
		return map[string]string{}
	}
	return provisioningFromPolicy(*policy)
}

func (s *ClustersServer) withProvisioning(cluster *aigridv1.ManagedCluster, provisioning map[string]string) *aigridv1.ManagedCluster {
	if cluster.Status == nil {
		cluster.Status = &aigridv1.ManagedClusterStatus{}
	}
	cluster.Status.Provisioning = provisioning[cluster.GetId()]
	return cluster
}

// provisioningFromPolicy maps each cluster in a root Policy's status.status
// to "Compliant", "NonCompliant" or, while it has no result yet, "Pending".
// ACM lists a cluster there once the Policy's placement selects it.
func provisioningFromPolicy(policy unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	entries, _, _ := unstructured.NestedSlice(policy.Object, "status", "status")
	for _, raw := range entries {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		cluster, _ := entry["clustername"].(string)
		if cluster == "" {
			continue
		}
		switch compliant, _ := entry["compliant"].(string); compliant {
		case "Compliant", "NonCompliant":
			out[cluster] = compliant
		default:
			out[cluster] = "Pending"
		}
	}
	return out
}

// updatePaths reports which mutable fields an Update changes. Without a mask
// these are the parts the request sets: a request with no spec leaves
// hubAcceptsClient as it is instead of setting it to false, and empty label
// or annotation maps leave those as they are.
func updatePaths(mask *fieldmaskpb.FieldMask, object *aigridv1.ManagedCluster) clusterUpdatePaths {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return clusterUpdatePaths{
			labels:      len(object.GetMetadata().GetLabels()) > 0,
			annotations: len(object.GetMetadata().GetAnnotations()) > 0,
			hub:         object.GetSpec() != nil,
		}
	}
	var paths clusterUpdatePaths
	for _, path := range mask.GetPaths() {
		p := strings.TrimPrefix(path, "object.")
		switch {
		case p == "metadata":
			paths.labels, paths.annotations = true, true
		case p == "metadata.labels" || strings.HasPrefix(p, "metadata.labels."):
			paths.labels = true
		case p == "metadata.annotations" || strings.HasPrefix(p, "metadata.annotations."):
			paths.annotations = true
		case p == "spec.hub_accepts_client" || p == "spec" || strings.HasPrefix(p, "spec."):
			paths.hub = true
		}
	}
	return paths
}

// isConsoleManagedKey reports whether the console owns a label or annotation
// key: the ai-grid/* keys, and for labels ACM's placement label "location".
func isConsoleManagedKey(kind, key string) bool {
	return strings.HasPrefix(key, "ai-grid/") || (kind == "label" && key == "location")
}

// checkConsoleKeys refuses a request that adds or changes a label or
// annotation outside the console's keys. Clients send back the map they
// read, so other keys are allowed when they match the hub; managedKeysPatch
// doesn't write them.
func checkConsoleKeys(kind string, current, requested map[string]string) error {
	keys := make([]string, 0, len(requested))
	for k := range requested {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if cur, ok := current[k]; !isConsoleManagedKey(kind, k) && (!ok || cur != requested[k]) {
			return status.Errorf(codes.InvalidArgument, "%s %q cannot be changed through the console; only ai-grid/* labels and annotations and the location label can", kind, k)
		}
	}
	return nil
}

// checkCoordinates refuses an ai-grid/latitude or ai-grid/longitude
// annotation that isn't a finite number within -90..90 or -180..180. Blank
// values and surrounding whitespace are allowed, as readers ignore them.
func checkCoordinates(annotations map[string]string) error {
	for _, c := range []struct {
		key   string
		limit float64
	}{{"ai-grid/latitude", 90}, {"ai-grid/longitude", 180}} {
		v := strings.TrimSpace(annotations[c.key])
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < -c.limit || f > c.limit {
			return status.Errorf(codes.InvalidArgument, "annotation %q must be a number between %v and %v, got %q", c.key, -c.limit, c.limit, v)
		}
	}
	return nil
}

// managedKeysPatch builds a JSON merge patch for a label or annotation map.
// It writes console-managed keys only, and for those the request is the
// whole set: one on the hub but missing from the request is removed, so
// "Remove from grid" can drop ai-grid/enabled. Other keys, such as the ones
// ACM writes, are left as they are on the hub, even if they changed after
// checkConsoleKeys read them.
func managedKeysPatch(kind string, current, requested map[string]string) map[string]interface{} {
	patch := map[string]interface{}{}
	for k := range current {
		if _, ok := requested[k]; !ok && isConsoleManagedKey(kind, k) {
			patch[k] = nil
		}
	}
	for k, v := range requested {
		if isConsoleManagedKey(kind, k) {
			patch[k] = v
		}
	}
	return patch
}

func validateManagedClusterID(id string) error {
	if id == "" {
		return status.Error(codes.InvalidArgument, "id is required")
	}
	if !managedClusterNameRE.MatchString(id) {
		return status.Errorf(codes.InvalidArgument, "id %q is not a valid RFC 1123 DNS label", id)
	}
	return nil
}

// staleClusterError is the Aborted error for a write against an old version.
func staleClusterError(op, id string) error {
	return status.Errorf(codes.Aborted, "%s ManagedCluster %q: the cluster changed since it was read; reload and retry", op, id)
}

func mapManagedClusterAPIError(op, id string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return status.Errorf(codes.NotFound, "ManagedCluster %q not found", id)
	case apierrors.IsAlreadyExists(err):
		return status.Errorf(codes.AlreadyExists, "ManagedCluster %q already exists", id)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return status.Errorf(codes.InvalidArgument, "%s ManagedCluster %q: %v", op, id, err)
	case apierrors.IsConflict(err):
		return staleClusterError(op, id)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return status.Errorf(codes.PermissionDenied, "%s ManagedCluster %q: %v", op, id, err)
	default:
		return fmt.Errorf("%s ManagedCluster %q: %w", op, id, err)
	}
}
