package integration

import (
	"context"
	"os"
	"testing"
	"time"

	clientpkg "github.com/osac-project/ai-grid-provider-backend/client"
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

func TestListManagedClusters(t *testing.T) {
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

	clusters, err := client.ListManagedClusters(ctx)
	if err != nil {
		t.Fatalf("list managed clusters: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("expected 2 managed clusters, got %d", len(clusters))
	}

	byName := make(map[string]struct{}, len(clusters))
	for _, cluster := range clusters {
		byName[cluster.GetName()] = struct{}{}
	}
	for _, name := range []string{"ai-grid-sample-a", "ai-grid-sample-b"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("managed cluster %q was not returned; got %v", name, byName)
		}
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
		byName[tenant.GetName()] = struct{}{}
	}
	for _, name := range []string{"tenant-a", "tenant-b"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("tenant %q was not returned; got %v", name, byName)
		}
	}
}
