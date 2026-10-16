package aigrid

import (
	"context"
	"fmt"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// organizationLister is the Keycloak Organizations surface used by Tenants.
// Tests inject fakes; production wires *keycloak.Client.
type organizationLister interface {
	ListOrganizations(ctx context.Context) ([]keycloak.Organization, error)
}

// Service implements the ai_grid.v1 APIs.
type Service struct {
	aigridv1.UnimplementedAiGridServer
	aigridv1.UnimplementedManagedClustersServer
	aigridv1.UnimplementedTenantsServer
	client  dynamic.Interface
	tenants organizationLister
}

// NewServiceFromConfig creates a service backed by a Kubernetes API server.
// In a pod, config should be nil so in-cluster service-account credentials are used.
func NewServiceFromConfig(config *rest.Config) (*Service, error) {
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return &Service{client: client}, nil
}

// SetKeycloakClient configures the Keycloak-backed tenant API.
func (s *Service) SetKeycloakClient(client organizationLister) { s.tenants = client }

// NewServiceFromKubeconfig creates a service backed by the selected kubeconfig context.
func NewServiceFromKubeconfig(path string) (*Service, error) {
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return NewServiceFromConfig(config)
}

// Ping returns a response for the requested name. An empty name is treated as
// "world", making the API convenient to call with an empty protobuf object.
func (Service) Ping(_ context.Context, request *aigridv1.PingRequest) (*aigridv1.PingResponse, error) {
	name := request.GetName()
	if name == "" {
		name = "world"
	}

	return &aigridv1.PingResponse{Message: fmt.Sprintf("Welcome, %s!", name)}, nil
}
