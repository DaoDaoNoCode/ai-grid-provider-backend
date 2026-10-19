// Ported from krisv/ai-grid-cloud-provider-poc shared/grid-clusters.mjs at 62319b8.

package draft

import (
	"context"
	"slices"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Labels the console reads and writes on a ManagedCluster, the conventions of
// the PoC (shared/grid-clusters.mjs at 62319b8); the BFF names the same ones
// (api.LabelEnabled and its neighbours).
const (
	LabelAIGridEnabled     = "ai-grid/enabled"
	LabelAIGridProvisioned = "ai-grid/provisioned"
	LabelLocation          = "location"
	LabelAIGridSite        = "ai-grid/site"
	LabelAIGridRegion      = "ai-grid/region"
)

// ListLocations implements Models (proposal P7): the PoC's eligibleLocations
// (krisv/ai-grid-cloud-provider-poc shared/grid-clusters.mjs at 62319b8) over
// the clusters as ListManagedClusters reads them, so scenario events apply:
// the `location` labels of the AI Grid members that are provisioned and
// Available, unique and sorted. The API server already guarantees a label
// value's syntax, so an empty value is the only one left out.
func (s *Server) ListLocations(ctx context.Context, _ *aigriddraftv1.ListLocationsRequest) (*aigriddraftv1.ListLocationsResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	out := []string{}
	for _, c := range s.clusters {
		labels := c.GetLabels()
		l := labels[LabelLocation]
		if labels[LabelAIGridEnabled] != "true" || labels[LabelAIGridProvisioned] != "true" || l == "" || slices.Contains(out, l) {
			continue
		}
		if state, _ := deriveState(c); state != aigriddraftv1.ManagedClusterState_MANAGED_CLUSTER_STATE_AVAILABLE {
			continue
		}
		out = append(out, l)
	}
	slices.Sort(out)
	return &aigriddraftv1.ListLocationsResponse{Locations: out}, nil
}
