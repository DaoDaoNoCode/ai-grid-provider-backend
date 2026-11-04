package servers

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// labelAIGridRole picks what the hub installs on a grid cluster. The hub's
// AI Grid Placements select ai-grid/enabled "true" clusters by it
// (ai-grid-cloud/ai-grid-cluster-gitops,
// apps/ai-grid-spokes/manifests/02-placements.yaml): ingress clusters get
// MaaS and the consumer gateway, inference clusters the models and the
// provider gateway, combined clusters both. A cluster with any other role
// gets nothing.
const labelAIGridRole = "ai-grid/role"

const (
	roleIngress   = "ingress"
	roleInference = "inference"
	roleCombined  = "combined"
)

// servesInference reports whether a role runs models: inference or combined,
// the roles Models/ListPlacementClusters offers.
func servesInference(role string) bool {
	return role == roleInference || role == roleCombined
}

// servesIngress reports whether a role takes users' requests: ingress or
// combined, the clusters the models' catalog entries go to.
func servesIngress(role string) bool {
	return role == roleIngress || role == roleCombined
}

// maxNamedModels is how many placed models a refused role change names; the
// message counts the rest.
const maxNamedModels = 5

// catalogReader is the part of the Git catalog the Clusters server reads: the
// models placed on a cluster, on the remote's head. Production wires the
// gitops.Writer the Models server commits with.
type catalogReader interface {
	FreshHead(ctx context.Context) (gitops.Snapshot, error)
}

// checkRoleValue refuses an ai-grid/role label the update writes that isn't
// ingress, inference or combined. No role is allowed: a cluster that was
// never added to the grid has none. A role the cluster already has and the
// request leaves as it is isn't judged again, so a cluster labelled some
// other way outside the API can still have its other labels changed, or
// leave the grid; it gets nothing installed until its role is fixed.
func checkRoleValue(current, result map[string]string) error {
	role, ok := result[labelAIGridRole]
	if !ok {
		return nil
	}
	if stored, had := current[labelAIGridRole]; had && stored == role {
		return nil
	}
	switch role {
	case roleIngress, roleInference, roleCombined:
		return nil
	}
	return connect.Errorf(connect.CodeInvalidArgument, "label %q must be %s, %s or %s, got %q",
		labelAIGridRole, roleIngress, roleInference, roleCombined, role)
}

// checkRoleChange refuses a role change that would take away what the grid
// still needs from the cluster. It applies only to a cluster in the grid
// (ai-grid/enabled "true") both as stored and after the update, whose role
// changes; joining or leaving the grid isn't checked here. The hub installs
// by role and prunes what the old role had, so:
//
//   - a cluster that stops serving inference while models are placed on it
//     would lose them: FailedPrecondition until they move elsewhere;
//   - a cluster that stops serving ingress while it is the last ingress
//     cluster in the grid would leave users no way to reach any model:
//     FailedPrecondition until another cluster serves ingress.
//
// Both are decided on what the hub and the catalog hold now, and nothing
// stops another writer changing that before the cluster is patched: a model
// placed on the cluster, or another cluster's role changed, between the check
// and the patch is not refused. The patch is a ManagedCluster write and the
// catalog is Git, so there is no single commit to put both in.
func (s *ClustersServer) checkRoleChange(ctx context.Context, id string, current, result map[string]string) error {
	if !isAIGridMember(current) || !isAIGridMember(result) {
		return nil
	}
	from, to := current[labelAIGridRole], result[labelAIGridRole]
	if from == to {
		return nil
	}
	if servesIngress(from) && !servesIngress(to) {
		if err := s.checkOtherIngress(ctx, id); err != nil {
			return err
		}
	}
	if servesInference(from) && !servesInference(to) {
		if err := s.checkNoPlacedModels(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// checkOtherIngress refuses when no grid cluster other than id serves
// ingress.
func (s *ClustersServer) checkOtherIngress(ctx context.Context, id string) error {
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return connect.Errorf(connect.CodeInternal, "list ManagedCluster resources: %v", err).WithCause(err)
	}
	for i := range list.Items {
		labels := list.Items[i].GetLabels()
		if list.Items[i].GetName() != id && isAIGridMember(labels) && servesIngress(labels[labelAIGridRole]) {
			return nil
		}
	}
	return connect.Errorf(connect.CodeFailedPrecondition,
		"cluster %q is the last cluster in the grid that serves ingress, and users reach models through ingress clusters; give another cluster role %s or %s before this one stops serving ingress",
		id, roleIngress, roleCombined)
}

// checkNoPlacedModels refuses when the catalog places any model on cluster
// id: its index, catalog/clusters/<id>/kustomization.yaml, lists one.
//
// It reads the remote's head (FreshHead, a fetch), not a snapshot up to
// AI_GRID_GIT_READ_MAX_AGE old, so a model another writer placed in that
// window is seen. Without a catalog (none set, or a backend started without
// AI_GRID_GIT_URL) there is no repository a model could have been placed
// through, so nothing is refused. A repository that is configured but can't
// be read (not cloned yet, a failed fetch) is Unavailable: the change waits
// until the placements can be checked.
func (s *ClustersServer) checkNoPlacedModels(ctx context.Context, id string) error {
	if s.catalog == nil {
		return nil
	}
	head, err := s.catalog.FreshHead(ctx)
	if errors.Is(err, gitops.ErrNotConfigured) {
		return nil
	}
	if err != nil {
		return connect.Errorf(connect.CodeUnavailable, "cluster %q: read the models placed on it: git: %v", id, err).WithCause(err)
	}
	resources, err := readKustomizationResources(head, clusterGitIndex(id))
	if err != nil {
		return err
	}
	if len(resources) == 0 {
		return nil
	}
	ids := make([]string, 0, len(resources))
	for _, resource := range resources {
		// ../../models/<id>/deployment
		ids = append(ids, path.Base(path.Dir(resource)))
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	placed, them := fmt.Sprintf("%d models are", len(ids)), "them"
	if len(ids) == 1 {
		placed, them = "1 model is", "it"
	}
	return connect.Errorf(connect.CodeFailedPrecondition,
		"cluster %q cannot stop serving inference while %s placed on it (%s); move %s to other clusters first",
		id, placed, nameModels(ids), them)
}

// nameModels lists up to maxNamedModels ids and counts the rest.
func nameModels(ids []string) string {
	if len(ids) <= maxNamedModels {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:maxNamedModels], ", "), len(ids)-maxNamedModels)
}

// labelsAfter is the label map a merge patch of the labels leaves: current
// with the patch's keys set, or removed where the patch has nil.
func labelsAfter(current map[string]string, patch map[string]interface{}) map[string]string {
	out := make(map[string]string, len(current)+len(patch))
	for k, v := range current {
		out[k] = v
	}
	for k, v := range patch {
		if s, ok := v.(string); ok {
			out[k] = s
		} else {
			delete(out, k)
		}
	}
	return out
}
