package servers

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/acmsearch"
	"google.golang.org/protobuf/proto"
)

// GPUNodeSource lists the GPU nodes of every managed cluster; the ACM Search
// client in production (acmsearch.Client).
type GPUNodeSource interface {
	GPUNodes(ctx context.Context) ([]acmsearch.Node, error)
}

// DefaultGPUInventoryTTL is how long one Search answer serves List and Get.
// GPU Feature Discovery relabels a node about once a minute, and Search
// indexes changes on its collector's schedule, so a fresher answer would not
// be fresher data.
const DefaultGPUInventoryTTL = 60 * time.Second

// gpuQueryTimeout bounds one Search query, so a slow Search never holds up
// a List or Get for long: the call answers without GPUs instead.
const gpuQueryTimeout = 10 * time.Second

// GPUInventory caches each cluster's GPUs from a GPUNodeSource. A failed
// query is cached for the same time as an answer, so an unavailable Search
// is asked (and logged) once per TTL, not on every call.
type GPUInventory struct {
	source GPUNodeSource
	ttl    time.Duration
	logger *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	fetched time.Time
	byName  map[string]*aigridv1.ClusterGPUs
}

// NewGPUInventory returns an inventory over source. A zero ttl is
// DefaultGPUInventoryTTL; a nil logger is slog.Default().
func NewGPUInventory(source GPUNodeSource, ttl time.Duration, logger *slog.Logger) *GPUInventory {
	if ttl <= 0 {
		ttl = DefaultGPUInventoryTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GPUInventory{source: source, ttl: ttl, logger: logger, now: time.Now}
}

// Clusters returns each cluster's GPUs by cluster name, from the cache while
// it is fresh. When Search fails it returns an empty map: clusters then carry
// no `gpus`, and the call that asked still succeeds.
func (inv *GPUInventory) Clusters(ctx context.Context) map[string]*aigridv1.ClusterGPUs {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.byName != nil && inv.now().Sub(inv.fetched) < inv.ttl {
		return inv.byName
	}
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gpuQueryTimeout)
	defer cancel()
	nodes, err := inv.source.GPUNodes(queryCtx)
	inv.fetched = inv.now()
	if err != nil {
		inv.logger.Warn("read cluster GPUs from ACM Search", "error", err)
		inv.byName = map[string]*aigridv1.ClusterGPUs{}
		return inv.byName
	}
	inv.byName = clusterGPUsFromNodes(nodes)
	return inv.byName
}

// gpuSharingKey identifies a GPU configuration: nodes with the same vendor,
// product, memory, GPUs per node and sharing are one configuration.
type gpuSharingKey struct {
	vendor, product       string
	memoryMiB             int64
	perNode               int32
	strategy, migStrategy string
	replicas              int32
	migCapable            bool
}

// clusterGPUsFromNodes groups GPU nodes by cluster and derives each
// cluster's totals and configurations (the contract in
// proto/ai_grid/v1/managed_cluster_type.proto, ClusterGPUs).
func clusterGPUsFromNodes(nodes []acmsearch.Node) map[string]*aigridv1.ClusterGPUs {
	out := map[string]*aigridv1.ClusterGPUs{}
	for _, n := range nodes {
		node := gpuNodeFromLabels(n)
		gpus := out[n.Cluster]
		if gpus == nil {
			gpus = &aigridv1.ClusterGPUs{}
			out[n.Cluster] = gpus
		}
		gpus.Nodes = append(gpus.Nodes, node)
		gpus.Total += node.GetCount()
		gpus.NodeCount++
	}
	for _, gpus := range out {
		sort.Slice(gpus.Nodes, func(i, j int) bool { return gpus.Nodes[i].GetName() < gpus.Nodes[j].GetName() })
		configs := map[gpuSharingKey]*aigridv1.GPUConfiguration{}
		for _, node := range gpus.Nodes {
			s := node.GetSharing()
			key := gpuSharingKey{
				vendor: node.GetVendor(), product: node.GetProduct(), memoryMiB: node.GetMemoryMib(), perNode: node.GetCount(),
				strategy: s.GetStrategy(), migStrategy: s.GetMigStrategy(), replicas: s.GetReplicas(), migCapable: s.GetMigCapable(),
			}
			c := configs[key]
			if c == nil {
				c = &aigridv1.GPUConfiguration{
					Vendor: node.GetVendor(), Product: node.GetProduct(), MemoryMib: node.GetMemoryMib(),
					GpusPerNode: node.GetCount(), Sharing: proto.Clone(s).(*aigridv1.GPUSharing),
				}
				configs[key] = c
				gpus.Configurations = append(gpus.Configurations, c)
			}
			c.NodeCount++
		}
		// Most nodes first; then by product, and by GPUs per node so two sizes
		// of one product keep a stable order.
		sort.SliceStable(gpus.Configurations, func(i, j int) bool {
			a, b := gpus.Configurations[i], gpus.Configurations[j]
			if a.GetNodeCount() != b.GetNodeCount() {
				return a.GetNodeCount() > b.GetNodeCount()
			}
			if a.GetProduct() != b.GetProduct() {
				return a.GetProduct() < b.GetProduct()
			}
			return a.GetGpusPerNode() > b.GetGpusPerNode()
		})
	}
	return out
}

// NVIDIA GPU Feature Discovery's node labels
// (github.com/NVIDIA/k8s-device-plugin, docs/gpu-feature-discovery and
// internal/lm), as seen on the dev hub's spokes through Search.
const (
	labelGPUProduct      = "nvidia.com/gpu.product"
	labelGPUCount        = "nvidia.com/gpu.count"
	labelGPUMemory       = "nvidia.com/gpu.memory"
	labelGPUFamily       = "nvidia.com/gpu.family"
	labelComputeMajor    = "nvidia.com/gpu.compute.major"
	labelComputeMinor    = "nvidia.com/gpu.compute.minor"
	labelDriverFull      = "nvidia.com/cuda.driver-version.full"
	labelDriverMajor     = "nvidia.com/cuda.driver.major"
	labelDriverMinor     = "nvidia.com/cuda.driver.minor"
	labelDriverRev       = "nvidia.com/cuda.driver.rev"
	labelRuntimeFull     = "nvidia.com/cuda.runtime-version.full"
	labelRuntimeMajor    = "nvidia.com/cuda.runtime.major"
	labelRuntimeMinor    = "nvidia.com/cuda.runtime.minor"
	labelSharingStrategy = "nvidia.com/gpu.sharing-strategy"
	labelReplicas        = "nvidia.com/gpu.replicas"
	labelMIGCapable      = "nvidia.com/mig.capable"
	labelMIGStrategy     = "nvidia.com/mig.strategy"
	labelMachine         = "nvidia.com/gpu.machine"
	labelInstanceType    = "node.kubernetes.io/instance-type"
)

// gpuNodeFromLabels reads one node's GPU Feature Discovery labels.
func gpuNodeFromLabels(n acmsearch.Node) *aigridv1.GPUNode {
	l := n.Labels
	node := &aigridv1.GPUNode{
		Name:      n.Name,
		Vendor:    "NVIDIA",
		Product:   l[labelGPUProduct],
		Count:     atoi32(l[labelGPUCount]),
		MemoryMib: atoi64(l[labelGPUMemory]),
		Family:    l[labelGPUFamily],
		Ready:     n.Ready,
	}
	if major, minor := l[labelComputeMajor], l[labelComputeMinor]; major != "" && minor != "" {
		node.ComputeCapability = major + "." + minor
	}
	node.DriverVersion = l[labelDriverFull]
	if node.DriverVersion == "" && l[labelDriverMajor] != "" {
		node.DriverVersion = joinVersion(l[labelDriverMajor], l[labelDriverMinor], l[labelDriverRev])
	}
	node.CudaRuntimeVersion = l[labelRuntimeFull]
	if node.CudaRuntimeVersion == "" && l[labelRuntimeMajor] != "" {
		node.CudaRuntimeVersion = joinVersion(l[labelRuntimeMajor], l[labelRuntimeMinor])
	}
	node.InstanceType = l[labelMachine]
	if node.InstanceType == "" {
		node.InstanceType = l[labelInstanceType]
	}
	node.Sharing = gpuSharingFromLabels(l)
	return node
}

// gpuSharingFromLabels: the strategy is GPU Feature Discovery's
// sharing-strategy ("none", "time-slicing", "mps"), or "mig" when a MIG
// strategy ("single" or "mixed") is set; replicas default to 1, GFD's value
// without sharing.
func gpuSharingFromLabels(l map[string]string) *aigridv1.GPUSharing {
	s := &aigridv1.GPUSharing{
		Strategy:   l[labelSharingStrategy],
		Replicas:   atoi32(l[labelReplicas]),
		MigCapable: l[labelMIGCapable] == "true",
	}
	if s.Replicas < 1 {
		s.Replicas = 1
	}
	if m := l[labelMIGStrategy]; m == "single" || m == "mixed" {
		s.MigStrategy = m
		s.Strategy = "mig"
	}
	return s
}

func joinVersion(parts ...string) string {
	out := ""
	for i, p := range parts {
		if p == "" {
			break
		}
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}

func atoi32(s string) int32 {
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil || v < 0 {
		return 0
	}
	return int32(v)
}

func atoi64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
