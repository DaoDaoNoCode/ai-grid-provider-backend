package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
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

// ClustersServerBuilder configures a ClustersServer.
type ClustersServerBuilder struct {
	logger *slog.Logger
	client dynamic.Interface
}

// ClustersServer implements ai_grid.v1.Clusters against hub ManagedCluster CRs.
type ClustersServer struct {
	aigridv1.UnimplementedClustersServer
	logger *slog.Logger
	client dynamic.Interface
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

// Build creates the ClustersServer.
func (b *ClustersServerBuilder) Build() (*ClustersServer, error) {
	if b.client == nil {
		return nil, errors.New("kubernetes dynamic client is mandatory")
	}
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ClustersServer{logger: logger, client: b.client}, nil
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

	items := make([]*aigridv1.ManagedCluster, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, managedClusterFromUnstructured(item))
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
	return &aigridv1.ClustersGetResponse{Object: managedClusterFromUnstructured(*obj)}, nil
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

	updateLabels, updateHub := updatePaths(req.GetUpdateMask(), object)

	patchBody := map[string]interface{}{}
	if updateLabels {
		mergedLabels := map[string]string{}
		for k, v := range current.GetLabels() {
			mergedLabels[k] = v
		}
		if object.GetMetadata() != nil {
			for k, v := range object.GetMetadata().GetLabels() {
				mergedLabels[k] = v
			}
		}
		patchBody["metadata"] = map[string]interface{}{
			"labels": stringMapToInterfaceMap(mergedLabels),
		}
	}
	if updateHub {
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
	return &aigridv1.ClustersUpdateResponse{Object: managedClusterFromUnstructured(*updated)}, nil
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

// updatePaths reports which mutable fields an Update changes. Without a mask
// these are the parts the request sets: a request with no spec leaves
// hubAcceptsClient as it is instead of setting it to false.
func updatePaths(mask *fieldmaskpb.FieldMask, object *aigridv1.ManagedCluster) (labels, hub bool) {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return object.GetMetadata() != nil, object.GetSpec() != nil
	}
	for _, path := range mask.GetPaths() {
		p := strings.TrimPrefix(path, "object.")
		switch {
		case p == "metadata.labels" || p == "metadata" || strings.HasPrefix(p, "metadata.labels"):
			labels = true
		case p == "spec.hub_accepts_client" || p == "spec" || strings.HasPrefix(p, "spec."):
			hub = true
		}
	}
	return labels, hub
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

func mapManagedClusterAPIError(op, id string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return status.Errorf(codes.NotFound, "ManagedCluster %q not found", id)
	case apierrors.IsAlreadyExists(err):
		return status.Errorf(codes.AlreadyExists, "ManagedCluster %q already exists", id)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return status.Errorf(codes.InvalidArgument, "%s ManagedCluster %q: %v", op, id, err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return status.Errorf(codes.PermissionDenied, "%s ManagedCluster %q: %v", op, id, err)
	default:
		return fmt.Errorf("%s ManagedCluster %q: %w", op, id, err)
	}
}
