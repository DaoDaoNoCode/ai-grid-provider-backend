package servers

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/acmsearch"
	"google.golang.org/protobuf/proto"
)

// GPUNodeSource lists the GPU nodes of every managed cluster, each with the
// labels named in labelKeys; the ACM Search client in production
// (acmsearch.Client).
type GPUNodeSource interface {
	GPUNodes(ctx context.Context, labelKeys ...string) ([]acmsearch.Node, error)
}

// DefaultGPUInventoryTTL is how long one Search answer serves List and Get.
// GPU Feature Discovery relabels a node about once a minute, and Search
// indexes changes on its collector's schedule, so a fresher answer would not
// be fresher data.
const DefaultGPUInventoryTTL = 60 * time.Second

// gpuQueryTimeout bounds one Search query, so a slow Search never holds up
// a List or Get for long: the call answers without GPUs instead.
const gpuQueryTimeout = 10 * time.Second

// gpuInventoryMaxStaleTTLs is how many TTLs an answer may serve once it is
// stale: while a refresh runs, and while Search fails. Past that, a caller
// waits for a fresh answer, and a failure clears the GPUs rather than show
// ones that may be gone.
const gpuInventoryMaxStaleTTLs = 5

// GPUInventory caches each cluster's GPUs from a GPUNodeSource. A failed
// query is cached for the same time as an answer, so an unavailable Search
// is asked (and logged) once per TTL, not on every call, and it keeps the
// last good answer for up to gpuInventoryMaxStaleTTLs TTLs. One query runs
// at a time, outside the lock. A caller that finds the cache stale but
// recent gets it at once and the query refreshes it in the background;
// otherwise the caller waits for the query, only as long as its own context
// allows.
type GPUInventory struct {
	source   GPUNodeSource
	ttl      time.Duration
	maxStale time.Duration
	logger   *slog.Logger
	now      func() time.Time

	mu       sync.Mutex
	fetched  time.Time // when the last query ended, answered or not
	answered time.Time // when the last query that answered ended
	byName   map[string]*aigridv1.ClusterGPUs
	inflight chan struct{} // closed when the running query has stored its answer; nil when none runs
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
	return &GPUInventory{source: source, ttl: ttl, maxStale: gpuInventoryMaxStaleTTLs * ttl, logger: logger, now: time.Now}
}

// Clusters returns each cluster's GPUs by cluster name, from the cache while
// it is fresh. When it isn't, it starts a query (or joins the one running).
// A cache that is stale but within maxStale is returned at once while the
// query refreshes it; otherwise Clusters waits for the query, at most until
// ctx ends: then it returns an empty map and the query carries on for later
// callers. When Search fails and there is no recent answer, it returns an
// empty map too: clusters then carry no `gpus`, and the call that asked
// still succeeds. The map is shared; callers must not change it.
func (inv *GPUInventory) Clusters(ctx context.Context) map[string]*aigridv1.ClusterGPUs {
	inv.mu.Lock()
	age := inv.now().Sub(inv.fetched)
	if inv.byName != nil && age < inv.ttl {
		byName := inv.byName
		inv.mu.Unlock()
		return byName
	}
	done := inv.inflight
	if done == nil {
		done = make(chan struct{})
		inv.inflight = done
		go inv.refresh(done)
	}
	if inv.byName != nil && age < inv.maxStale {
		byName := inv.byName
		inv.mu.Unlock()
		return byName
	}
	inv.mu.Unlock()

	select {
	case <-done:
		inv.mu.Lock()
		defer inv.mu.Unlock()
		return inv.byName
	case <-ctx.Done():
		return map[string]*aigridv1.ClusterGPUs{}
	}
}

// refresh queries the source once, bounded by gpuQueryTimeout rather than by
// any caller's context (it answers every caller waiting on it), and closes
// done. It stores the answer; on failure it keeps the last answer while that
// is within maxStale, and stores an empty one otherwise.
func (inv *GPUInventory) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), gpuQueryTimeout)
	defer cancel()
	nodes, err := inv.source.GPUNodes(ctx, gpuLabelKeys...)
	if err != nil {
		inv.logger.Warn("read cluster GPUs from ACM Search", "error", err)
	}
	inv.mu.Lock()
	now := inv.now()
	switch {
	case err == nil:
		inv.byName = clusterGPUsFromNodes(nodes)
		inv.answered = now
	case inv.byName == nil || now.Sub(inv.answered) >= inv.maxStale:
		inv.byName = map[string]*aigridv1.ClusterGPUs{}
	}
	inv.fetched = now
	inv.inflight = nil
	inv.mu.Unlock()
	close(done)
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
		gpus.Total = addSaturated(gpus.Total, node.GetCount())
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
// internal/lm), as an ACM hub's Search indexes them.
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

// gpuLabelKeys are the labels gpuNodeFromLabels reads, the only ones asked
// of the source.
var gpuLabelKeys = []string{
	labelGPUProduct, labelGPUCount, labelGPUMemory, labelGPUFamily, labelComputeMajor, labelComputeMinor,
	labelDriverFull, labelDriverMajor, labelDriverMinor, labelDriverRev,
	labelRuntimeFull, labelRuntimeMajor, labelRuntimeMinor,
	labelSharingStrategy, labelReplicas, labelMIGCapable, labelMIGStrategy, labelMachine, labelInstanceType,
}

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

// addSaturated adds two counts, stopping at math.MaxInt32 instead of
// wrapping negative. A node's labels come from its cluster's administrators,
// so one could claim any count.
func addSaturated(a, b int32) int32 {
	if sum := int64(a) + int64(b); sum < math.MaxInt32 {
		return int32(sum)
	}
	return math.MaxInt32
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
