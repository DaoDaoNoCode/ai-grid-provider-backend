package servers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/acmsearch"
	"google.golang.org/protobuf/proto"
)

// gfdLabels are a GPU node's labels as GPU Feature Discovery writes them,
// taken from a spoke of the dev hub (an AWS g6.xlarge with one NVIDIA L4),
// with overrides.
func gfdLabels(overrides map[string]string) map[string]string {
	l := map[string]string{
		"nvidia.com/gpu.present":                "true",
		"nvidia.com/gpu.product":                "NVIDIA-L4",
		"nvidia.com/gpu.count":                  "1",
		"nvidia.com/gpu.memory":                 "23034",
		"nvidia.com/gpu.family":                 "ada-lovelace",
		"nvidia.com/gpu.compute.major":          "8",
		"nvidia.com/gpu.compute.minor":          "9",
		"nvidia.com/cuda.driver-version.full":   "580.105.08",
		"nvidia.com/cuda.runtime-version.full":  "13.0",
		"nvidia.com/gpu.machine":                "g6.xlarge",
		"nvidia.com/gpu.replicas":               "1",
		"nvidia.com/gpu.sharing-strategy":       "none",
		"nvidia.com/mig.capable":                "false",
		"node.kubernetes.io/instance-type":      "g6.xlarge",
		"node-role.kubernetes.io/worker":        "",
		"nvidia.com/gpu.deploy.device-plugin":   "true",
		"nvidia.com/cuda.driver-version.major":  "580",
		"nvidia.com/cuda.runtime-version.major": "13",
	}
	for k, v := range overrides {
		if v == "-" {
			delete(l, k)
			continue
		}
		l[k] = v
	}
	return l
}

func TestGPUNodeFromLabels(t *testing.T) {
	t.Parallel()
	node := gpuNodeFromLabels(acmsearch.Node{Cluster: "site-a", Name: "gpu-1", Labels: gfdLabels(nil), Ready: true})
	want := &aigridv1.GPUNode{
		Name: "gpu-1", Vendor: "NVIDIA", Product: "NVIDIA-L4", Count: 1, MemoryMib: 23034, Family: "ada-lovelace",
		ComputeCapability: "8.9", DriverVersion: "580.105.08", CudaRuntimeVersion: "13.0", InstanceType: "g6.xlarge", Ready: true,
		Sharing: &aigridv1.GPUSharing{Strategy: "none", Replicas: 1},
	}
	if !proto.Equal(node, want) {
		t.Fatalf("node = %v\nwant   %v", node, want)
	}

	// Older GPU Feature Discovery: no *-version.full labels; no replicas label
	// means 1; no gpu.machine falls back to the node's instance type.
	old := gpuNodeFromLabels(acmsearch.Node{Name: "gpu-2", Labels: gfdLabels(map[string]string{
		"nvidia.com/cuda.driver-version.full": "-", "nvidia.com/cuda.driver.major": "535", "nvidia.com/cuda.driver.minor": "104",
		"nvidia.com/cuda.driver.rev": "05", "nvidia.com/cuda.runtime-version.full": "-", "nvidia.com/cuda.runtime.major": "12",
		"nvidia.com/cuda.runtime.minor": "2", "nvidia.com/gpu.replicas": "-", "nvidia.com/gpu.machine": "-",
	})})
	if old.GetDriverVersion() != "535.104.05" || old.GetCudaRuntimeVersion() != "12.2" || old.GetSharing().GetReplicas() != 1 || old.GetInstanceType() != "g6.xlarge" {
		t.Fatalf("fallbacks: %v", old)
	}
}

func TestGPUSharing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   *aigridv1.GPUSharing
	}{
		{"none", nil, &aigridv1.GPUSharing{Strategy: "none", Replicas: 1}},
		{"time-slicing", map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-L40S-SHARED", "nvidia.com/gpu.sharing-strategy": "time-slicing", "nvidia.com/gpu.replicas": "4",
		}, &aigridv1.GPUSharing{Strategy: "time-slicing", Replicas: 4}},
		{"mps", map[string]string{"nvidia.com/gpu.sharing-strategy": "mps", "nvidia.com/gpu.replicas": "2"},
			&aigridv1.GPUSharing{Strategy: "mps", Replicas: 2}},
		{"MIG single", map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-40GB-MIG-1g.5gb", "nvidia.com/mig.capable": "true", "nvidia.com/mig.strategy": "single",
		}, &aigridv1.GPUSharing{Strategy: "mig", Replicas: 1, MigCapable: true, MigStrategy: "single"}},
		{"MIG capable, not sliced", map[string]string{"nvidia.com/mig.capable": "true", "nvidia.com/mig.strategy": "none"},
			&aigridv1.GPUSharing{Strategy: "none", Replicas: 1, MigCapable: true}},
	} {
		got := gpuSharingFromLabels(gfdLabels(tc.labels))
		if !proto.Equal(got, tc.want) {
			t.Errorf("%s: sharing = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClusterGPUsFromNodes(t *testing.T) {
	t.Parallel()
	h200 := func(name string) acmsearch.Node {
		return acmsearch.Node{Cluster: "big", Name: name, Ready: true, Labels: gfdLabels(map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-H200", "nvidia.com/gpu.count": "8", "nvidia.com/gpu.memory": "143771", "nvidia.com/gpu.family": "hopper",
		})}
	}
	nodes := []acmsearch.Node{
		{Cluster: "site-a", Name: "gpu-a", Ready: true, Labels: gfdLabels(nil)},
		h200("h-4"), h200("h-2"), h200("h-3"), h200("h-1"),
		// A different driver on one node doesn't make it another configuration.
		{Cluster: "mixed", Name: "l4-1", Labels: gfdLabels(map[string]string{"nvidia.com/gpu.count": "2"})},
		{Cluster: "mixed", Name: "l4-2", Labels: gfdLabels(map[string]string{"nvidia.com/gpu.count": "2", "nvidia.com/cuda.driver-version.full": "570.1"})},
		{Cluster: "mixed", Name: "shared-1", Labels: gfdLabels(map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-L40S-SHARED", "nvidia.com/gpu.sharing-strategy": "time-slicing", "nvidia.com/gpu.replicas": "4",
		})},
	}
	got := clusterGPUsFromNodes(nodes)
	if len(got) != 3 {
		t.Fatalf("clusters = %d, want 3", len(got))
	}

	big := got["big"]
	if big.GetTotal() != 32 || big.GetNodeCount() != 4 || len(big.GetConfigurations()) != 1 {
		t.Fatalf("big = %v", big)
	}
	if c := big.GetConfigurations()[0]; c.GetProduct() != "NVIDIA-H200" || c.GetGpusPerNode() != 8 || c.GetNodeCount() != 4 || c.GetMemoryMib() != 143771 {
		t.Fatalf("big configuration = %v", c)
	}
	if names := []string{big.Nodes[0].GetName(), big.Nodes[3].GetName()}; names[0] != "h-1" || names[1] != "h-4" {
		t.Fatalf("nodes not sorted by name: %v", names)
	}

	mixed := got["mixed"]
	if mixed.GetTotal() != 5 || mixed.GetNodeCount() != 3 || len(mixed.GetConfigurations()) != 2 {
		t.Fatalf("mixed = %v", mixed)
	}
	// Most nodes first.
	first, second := mixed.GetConfigurations()[0], mixed.GetConfigurations()[1]
	if first.GetProduct() != "NVIDIA-L4" || first.GetNodeCount() != 2 || first.GetGpusPerNode() != 2 {
		t.Fatalf("first configuration = %v", first)
	}
	if second.GetProduct() != "NVIDIA-L40S-SHARED" || second.GetSharing().GetStrategy() != "time-slicing" || second.GetSharing().GetReplicas() != 4 {
		t.Fatalf("second configuration = %v", second)
	}
}

// countingSource answers from a function and counts the calls.
type countingSource struct {
	mu    sync.Mutex
	calls int
	fn    func() ([]acmsearch.Node, error)
}

func (s *countingSource) GPUNodes(context.Context) ([]acmsearch.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.fn()
}

func TestGPUInventoryCachesAnswersAndFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	fail := true
	src := &countingSource{fn: func() ([]acmsearch.Node, error) {
		if fail {
			return nil, errors.New("search unavailable")
		}
		return []acmsearch.Node{{Cluster: "site-a", Name: "gpu-a", Labels: gfdLabels(nil)}}, nil
	}}
	inv := NewGPUInventory(src, time.Minute, nil)
	inv.now = func() time.Time { return now }

	// A failure is an empty answer, cached like any other.
	if got := inv.Clusters(context.Background()); len(got) != 0 {
		t.Fatalf("failure: %v", got)
	}
	inv.Clusters(context.Background())
	if src.calls != 1 {
		t.Fatalf("calls = %d, want 1 within the TTL", src.calls)
	}

	fail = false
	now = now.Add(time.Minute)
	if got := inv.Clusters(context.Background()); got["site-a"].GetTotal() != 1 || src.calls != 2 {
		t.Fatalf("after the TTL: %v, calls %d", got, src.calls)
	}
	inv.Clusters(context.Background())
	if src.calls != 2 {
		t.Fatalf("calls = %d, want 2", src.calls)
	}
}

func TestClustersListAndGetAttachGPUs(t *testing.T) {
	t.Parallel()
	client := newManagedClusterClient(
		managedCluster("site-a", nil, true),
		managedCluster("site-b", nil, true),
	)
	src := &countingSource{fn: func() ([]acmsearch.Node, error) {
		return []acmsearch.Node{{Cluster: "site-a", Name: "gpu-a", Ready: true, Labels: gfdLabels(nil)}}, nil
	}}
	server, err := NewClustersServer().SetDynamicClient(client).SetGPUInventory(NewGPUInventory(src, 0, nil)).Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := server.List(ctx, &aigridv1.ClustersListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*aigridv1.ManagedCluster{}
	for _, item := range list.GetItems() {
		byID[item.GetId()] = item
	}
	if g := byID["site-a"].GetStatus().GetGpus(); g.GetTotal() != 1 || g.GetNodes()[0].GetProduct() != "NVIDIA-L4" {
		t.Fatalf("site-a gpus = %v", g)
	}
	if byID["site-b"].GetStatus().GetGpus() != nil {
		t.Fatalf("site-b has no GPU nodes, got %v", byID["site-b"].GetStatus().GetGpus())
	}

	got, err := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetObject().GetStatus().GetGpus().GetConfigurations()[0].GetNodeCount() != 1 {
		t.Fatalf("get gpus = %v", got.GetObject().GetStatus().GetGpus())
	}
	// A response holds a copy: changing it doesn't change the cache.
	got.GetObject().GetStatus().GetGpus().Total = 99
	again, _ := server.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-a"})
	if again.GetObject().GetStatus().GetGpus().GetTotal() != 1 {
		t.Fatal("the cached inventory was changed through a response")
	}
	if src.calls != 1 {
		t.Fatalf("Search calls = %d, want 1 for List and both Gets", src.calls)
	}
}

func TestClustersListSucceedsWithoutSearch(t *testing.T) {
	t.Parallel()
	client := newManagedClusterClient(managedCluster("site-a", nil, true))
	src := &countingSource{fn: func() ([]acmsearch.Node, error) { return nil, errors.New("connection refused") }}
	server, err := NewClustersServer().SetDynamicClient(client).SetGPUInventory(NewGPUInventory(src, 0, nil)).Build()
	if err != nil {
		t.Fatal(err)
	}
	list, err := server.List(context.Background(), &aigridv1.ClustersListRequest{})
	if err != nil {
		t.Fatalf("List must not fail on Search: %v", err)
	}
	if len(list.GetItems()) != 1 || list.GetItems()[0].GetStatus().GetGpus() != nil {
		t.Fatalf("items = %v", list.GetItems())
	}

	// No inventory at all (SEARCH_API_URL=off) is the same.
	plain := mustClustersServer(t, client)
	got, err := plain.Get(context.Background(), &aigridv1.ClustersGetRequest{Id: "site-a"})
	if err != nil || got.GetObject().GetStatus().GetGpus() != nil {
		t.Fatalf("get without inventory: %v, %v", got, err)
	}
}
