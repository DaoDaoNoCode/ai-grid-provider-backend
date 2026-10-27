package draft

import (
	"context"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// GetHubCapabilities implements AiGrid from the settings fixture.
func (s *Server) GetHubCapabilities(ctx context.Context, _ *aigriddraftv1.GetHubCapabilitiesRequest) (*aigriddraftv1.GetHubCapabilitiesResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &aigriddraftv1.GetHubCapabilitiesResponse{Capabilities: proto.Clone(s.capabilities).(*aigriddraftv1.HubCapabilities)}, nil
}

// msgInvalidRange is the PoC's message for a range other than 1h, 6h or 24h.
const msgInvalidRange = "Choose a time range of 1h, 6h, or 24h."

// overviewRanges are the PoC's ranges in seconds (backend/grid-monitoring.mjs:10).
var overviewRanges = map[string]int64{"1h": 3600, "6h": 21600, "24h": 86400}

// GetOverview implements Grid as the PoC's backend does without Thanos
// (krisv/ai-grid-cloud-provider-poc backend/grid-monitoring.mjs at 62319b8):
// the AI Grid members from the hub inventory, filtered by `ai-grid/site`, and
// the simulated metrics for the range ending now, 61 points each. The range
// is the backend's to check: empty is 1h, the PoC's default, and anything
// but 1h, 6h or 24h is InvalidArgument with the PoC's message.
func (s *Server) GetOverview(ctx context.Context, req *aigriddraftv1.GetOverviewRequest) (*aigriddraftv1.GetOverviewResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	rng := req.GetRange()
	if rng == "" {
		rng = "1h"
	}
	seconds, ok := overviewRanges[rng]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, msgInvalidRange)
	}
	// The clusters as ListManagedClusters reads them, so scenario state applies.
	s.mu.Lock()
	s.advance()
	clusters := make([]*aigriddraftv1.ManagedCluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		if c.GetLabels()[LabelAIGridEnabled] != "true" {
			continue
		}
		if req.GetSite() != "" && c.GetLabels()[LabelAIGridSite] != req.GetSite() {
			continue
		}
		clusters = append(clusters, withDerivedState(c))
	}
	s.mu.Unlock()
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].GetName() < clusters[j].GetName() })

	// backend/grid-monitoring.mjs:15: the range ends at the current second.
	end := s.clock().Unix()
	start, step := end-seconds, (seconds+59)/60
	resp := &aigriddraftv1.GetOverviewResponse{
		UpdatedAt:       timestamppb.New(time.Unix(end, 0)),
		InventorySource: aigriddraftv1.InventorySource_INVENTORY_SOURCE_ACM,
		MetricsSource:   aigriddraftv1.MetricsSource_METRICS_SOURCE_DEMO,
		Clusters:        clusters,
	}
	if req.GetIncludeMetrics() {
		resp.Metrics = demoMetrics(clusters, start, end, step)
	}
	return resp, nil
}
