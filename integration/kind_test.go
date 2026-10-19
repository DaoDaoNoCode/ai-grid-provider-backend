package integration

import (
	"context"
	"os"
	"testing"
	"time"

	clientpkg "github.com/osac-project/ai-grid-provider-backend/client"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

func TestPing(t *testing.T) {
	address := os.Getenv("AI_GRID_GRPC_ADDRESS")
	if address == "" {
		t.Skip("AI_GRID_GRPC_ADDRESS is not set; run via hack/test-kind.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := clientpkg.NewBuilder().SetAddress(address).SetPlaintext(true).Build(ctx)
	if err != nil {
		t.Fatalf("build gRPC client: %v", err)
	}
	defer client.Close()

	message, err := client.Ping(ctx, "Ada")
	if err != nil {
		t.Fatalf("ping backend: %v", err)
	}
	if message != "Welcome, Ada!" {
		t.Fatalf("expected Welcome, Ada!, got %q", message)
	}
}

func TestListClusters(t *testing.T) {
	address := os.Getenv("AI_GRID_GRPC_ADDRESS")
	if address == "" {
		t.Skip("AI_GRID_GRPC_ADDRESS is not set; run via hack/test-kind.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := clientpkg.NewBuilder().
		SetAddress(address).
		SetPlaintext(true).
		Build(ctx)
	if err != nil {
		t.Fatalf("build gRPC client: %v", err)
	}
	defer client.Close()

	clusters, err := client.ListClusters(ctx)
	if err != nil {
		t.Fatalf("list managed clusters: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("expected 2 managed clusters, got %d", len(clusters))
	}

	byName := make(map[string]struct{}, len(clusters))
	for _, cluster := range clusters {
		byName[cluster.GetId()] = struct{}{}
	}
	for _, name := range []string{"ai-grid-sample-a", "ai-grid-sample-b"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("managed cluster %q was not returned; got %v", name, byName)
		}
	}
}

func TestManagedClusterCRUD(t *testing.T) {
	address := os.Getenv("AI_GRID_GRPC_ADDRESS")
	if address == "" {
		t.Skip("AI_GRID_GRPC_ADDRESS is not set; run via hack/test-kind.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := clientpkg.NewBuilder().
		SetAddress(address).
		SetPlaintext(true).
		Build(ctx)
	if err != nil {
		t.Fatalf("build gRPC client: %v", err)
	}
	defer client.Close()

	const name = "ai-grid-crud-temp"
	_ = client.DeleteCluster(ctx, name)

	created, err := client.CreateCluster(ctx, &aigridv1.ManagedCluster{
		Id: name,
		Metadata: &aigridv1.Metadata{
			Name:   name,
			Labels: map[string]string{"environment": "test"},
		},
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
	})
	if err != nil {
		t.Fatalf("create managed cluster: %v", err)
	}
	if created.GetId() != name {
		t.Fatalf("created id = %q, want %q", created.GetId(), name)
	}
	defer func() { _ = client.DeleteCluster(ctx, name) }()

	got, err := client.GetCluster(ctx, name)
	if err != nil {
		t.Fatalf("get managed cluster: %v", err)
	}
	if got.GetMetadata().GetLabels()["environment"] != "test" {
		t.Fatalf("get labels = %v", got.GetMetadata().GetLabels())
	}

	updated, err := client.UpdateCluster(ctx, &aigridv1.ManagedCluster{
		Id: name,
		Metadata: &aigridv1.Metadata{
			Labels: map[string]string{"environment": "updated", "ai-grid/enabled": "true"},
		},
		Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true},
	}, nil)
	if err != nil {
		t.Fatalf("update managed cluster: %v", err)
	}
	if updated.GetMetadata().GetLabels()["environment"] != "updated" {
		t.Fatalf("updated environment = %q", updated.GetMetadata().GetLabels()["environment"])
	}
	if updated.GetMetadata().GetLabels()["ai-grid/enabled"] != "true" {
		t.Fatalf("updated ai-grid/enabled = %q", updated.GetMetadata().GetLabels()["ai-grid/enabled"])
	}

	if err := client.DeleteCluster(ctx, name); err != nil {
		t.Fatalf("delete managed cluster: %v", err)
	}
	if _, err := client.GetCluster(ctx, name); err == nil {
		t.Fatal("get after delete succeeded, want error")
	}
}

func TestListTenants(t *testing.T) {
	address := os.Getenv("AI_GRID_GRPC_ADDRESS")
	if address == "" {
		t.Skip("AI_GRID_GRPC_ADDRESS is not set; run via hack/test-kind.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := clientpkg.NewBuilder().SetAddress(address).SetPlaintext(true).Build(ctx)
	if err != nil {
		t.Fatalf("build gRPC client: %v", err)
	}
	defer client.Close()

	tenants, err := client.ListTenants(ctx)
	if err != nil {
		t.Fatalf("list tenants: %v", err)
	}
	if len(tenants) != 2 {
		t.Fatalf("expected 2 tenants, got %d", len(tenants))
	}
	byName := make(map[string]struct{}, len(tenants))
	for _, tenant := range tenants {
		byName[tenant.GetMetadata().GetName()] = struct{}{}
	}
	for _, name := range []string{"tenant-a", "tenant-b"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("tenant %q was not returned; got %v", name, byName)
		}
	}
}
