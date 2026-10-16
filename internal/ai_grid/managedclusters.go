package aigrid

import (
	"context"
	"fmt"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var managedClustersResource = schema.GroupVersionResource{
	Group: "cluster.open-cluster-management.io", Version: "v1", Resource: "managedclusters",
}

// ListManagedClusters returns ManagedCluster objects from the Kubernetes API.
func (s Service) ListManagedClusters(ctx context.Context, _ *aigridv1.ListManagedClustersRequest) (*aigridv1.ListManagedClustersResponse, error) {
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &aigridv1.ListManagedClustersResponse{}, nil
		}
		return nil, fmt.Errorf("list ManagedCluster resources: %w", err)
	}

	clusters := make([]*aigridv1.ManagedCluster, 0, len(list.Items))
	for _, item := range list.Items {
		clusters = append(clusters, managedClusterFromUnstructured(item))
	}
	return &aigridv1.ListManagedClustersResponse{Clusters: clusters}, nil
}

func managedClusterFromUnstructured(item unstructured.Unstructured) *aigridv1.ManagedCluster {
	accepts, _, _ := unstructured.NestedBool(item.Object, "spec", "hubAcceptsClient")
	return &aigridv1.ManagedCluster{
		Name:             item.GetName(),
		Labels:           item.GetLabels(),
		HubAcceptsClient: accepts,
	}
}
