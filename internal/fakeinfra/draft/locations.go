// Ported from krisv/ai-grid-cloud-provider-poc shared/grid-clusters.mjs at 62319b8.

package draft

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
