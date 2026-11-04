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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// maxNamedIDs is how many placed models or subscriptions a refusal names; the
// message counts the rest.
const maxNamedIDs = 5

// catalogReader is the part of the Git catalog the Clusters server reads: the
// models placed on a cluster and the subscriptions, on the remote's head.
// Production wires the gitops.Writer the Models and Subscriptions servers
// commit with.
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

// checkRoleChange refuses an update that would take away what the grid still
// needs from a cluster in it (ai-grid/enabled "true" as stored): a role
// change of a cluster that stays in the grid, or the cluster leaving the
// grid, whatever its role. A cluster joining the grid, or outside it before
// the update, isn't checked. The hub installs by role on grid clusters only
// and prunes what a cluster no longer gets, so these are refused with
// FailedPrecondition, checked in this order, the first that applies
// answering:
//
//  1. a cluster that serves ingress stops serving it (a role without
//     ingress, or leaving the grid) while the catalog has any subscription.
//     Every ingress cluster gets every subscription (catalog/subscriptions);
//     Argo CD deletes the cluster's copies when it leaves the ai-grid-ingress
//     Placement (the resources finalizer on the catalog subscriptions'
//     Applications), and maas-controller revokes the API keys of each
//     deleted MaaSSubscription in the maas-db every cluster shares, so the
//     keys stop working on all clusters;
//  2. a cluster that serves ingress stops serving it while no other grid
//     cluster does, and the grid keeps a member: users could reach no model.
//     The grid's last member may leave: that retires the grid and strands
//     no one;
//  3. a cluster stops serving inference (a role without inference, while it
//     stays in the grid), or leaves the grid whatever its role, while the
//     catalog places models on it. The models would lose the cluster that
//     runs them: Argo CD prunes them on a role change, and leaves them
//     running unmanaged when the cluster leaves the grid, so leaving and
//     joining again with another role would sidestep the role change rule.
//
// The grid-wide losses come first: the subscriptions, whose keys every
// cluster loses, then the ingress every model is reached through, then the
// cluster's own models.
//
// The rules are decided on what the hub and the catalog hold now, and
// nothing stops another writer changing that before the cluster is patched:
// a subscription created, a model placed on the cluster, or another
// cluster's role or membership changed, between the check and the patch is
// not refused. The patch is a ManagedCluster write and the catalog is Git,
// so there is no single commit to put both in.
func (s *ClustersServer) checkRoleChange(ctx context.Context, id string, current, result map[string]string) error {
	if !isAIGridMember(current) {
		return nil
	}
	leaves := !isAIGridMember(result)
	from, to := current[labelAIGridRole], result[labelAIGridRole]
	if !leaves && from == to {
		return nil
	}
	catalog := &catalogHead{reader: s.catalog}
	if servesIngress(from) && (leaves || !servesIngress(to)) {
		if err := checkNoSubscriptions(ctx, id, catalog, leaves); err != nil {
			return err
		}
		if err := s.checkOtherIngress(ctx, id, leaves); err != nil {
			return err
		}
	}
	if leaves || (servesInference(from) && !servesInference(to)) {
		if err := checkNoPlacedModels(ctx, id, catalog, leaves); err != nil {
			return err
		}
	}
	return nil
}

// checkOtherIngress refuses when no grid cluster other than id serves
// ingress, unless id leaves the grid and no other cluster is in it. A
// cluster being deleted doesn't count: it is about to leave the grid.
func (s *ClustersServer) checkOtherIngress(ctx context.Context, id string, leaves bool) error {
	list, err := s.client.Resource(managedClustersResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		code := connect.CodeInternal
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			code = connect.CodePermissionDenied
		}
		return connect.Errorf(code, "list ManagedCluster resources: %v", err).WithCause(err)
	}
	others := false
	for i := range list.Items {
		item := &list.Items[i]
		labels := item.GetLabels()
		if item.GetName() == id || item.GetDeletionTimestamp() != nil || !isAIGridMember(labels) {
			continue
		}
		if servesIngress(labels[labelAIGridRole]) {
			return nil
		}
		others = true
	}
	if leaves && !others {
		return nil
	}
	// The console shows this message to provider admins as it is.
	return connect.Errorf(connect.CodeFailedPrecondition,
		"Cluster %q is the only AI Grid cluster that serves ingress, and users reach models through ingress clusters. Give another cluster the %s or %s role first.",
		id, roleIngress, roleCombined)
}

// catalogHead reads the catalog's head once, when a check first needs it,
// so the checks of one update read one snapshot with one fetch.
//
// It reads the remote's head (FreshHead, a fetch), not a snapshot up to
// AI_GRID_GIT_READ_MAX_AGE old, so a model placed or a subscription created
// by another writer in that window is seen. Without a catalog (none set, or
// a backend started without AI_GRID_GIT_URL) the head is nil: there is no
// repository a model or a subscription could have been written to, so
// nothing is refused. A repository that is configured but can't be read
// (not cloned yet, a failed fetch) is an error, which the checks answer as
// Unavailable: the change waits until the catalog can be checked.
type catalogHead struct {
	reader catalogReader
	read   bool
	head   gitops.Snapshot
	err    error
}

func (c *catalogHead) get(ctx context.Context) (gitops.Snapshot, error) {
	if !c.read {
		c.read = true
		if c.reader != nil {
			c.head, c.err = c.reader.FreshHead(ctx)
			if errors.Is(c.err, gitops.ErrNotConfigured) {
				c.head, c.err = nil, nil
			}
		}
	}
	return c.head, c.err
}

// checkNoSubscriptions refuses when the catalog has any subscription: a file
// catalog/subscriptions/<id>.yaml (subscriptionsGitDir), which Argo CD
// applies to every ingress cluster.
func checkNoSubscriptions(ctx context.Context, id string, catalog *catalogHead, leaves bool) error {
	head, err := catalog.get(ctx)
	if err != nil {
		return connect.Errorf(connect.CodeUnavailable, "cluster %q: read the subscriptions: git: %v", id, err).WithCause(err)
	}
	if head == nil {
		return nil
	}
	files, err := head.List(subscriptionsGitDir)
	if err != nil {
		return subscriptionGitError("list", "", err)
	}
	var ids []string
	for _, file := range files {
		if sub, ok := strings.CutSuffix(file, ".yaml"); ok {
			ids = append(ids, sub)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// The console shows this message to provider admins as it is.
	exist, them := fmt.Sprintf("%d subscriptions exist", len(ids)), "the subscriptions"
	if len(ids) == 1 {
		exist, them = "1 subscription exists", "the subscription"
	}
	keep := fmt.Sprintf("keep the %s or %s role", roleIngress, roleCombined)
	if leaves {
		keep = "keep the cluster in AI Grid"
	}
	return connect.Errorf(connect.CodeFailedPrecondition,
		"Cluster %q serves ingress, and %s (%s). When a cluster stops serving ingress, the API keys of every subscription are revoked on all clusters. Delete %s first, or %s.",
		id, exist, nameIDs(ids), them, keep)
}

// checkNoPlacedModels refuses when the catalog places any model on cluster
// id: its index, catalog/clusters/<id>/kustomization.yaml, lists one.
func checkNoPlacedModels(ctx context.Context, id string, catalog *catalogHead, leaves bool) error {
	head, err := catalog.get(ctx)
	if err != nil {
		return connect.Errorf(connect.CodeUnavailable, "cluster %q: read the models placed on it: git: %v", id, err).WithCause(err)
	}
	if head == nil {
		return nil
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
	// The console shows this message to provider admins as it is.
	placed, move := fmt.Sprintf("%d models", len(ids)), "them to other clusters"
	if len(ids) == 1 {
		placed, move = "1 model", "it to another cluster"
	}
	before := "changing it to a role without inference"
	if leaves {
		before = "removing it from AI Grid"
	}
	return connect.Errorf(connect.CodeFailedPrecondition,
		"Cluster %q has %s placed on it (%s). Move %s before %s.",
		id, placed, nameIDs(ids), move, before)
}

// nameIDs lists up to maxNamedIDs ids and counts the rest.
func nameIDs(ids []string) string {
	if len(ids) <= maxNamedIDs {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:maxNamedIDs], ", "), len(ids)-maxNamedIDs)
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
