package servers

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/osac-project/ai-grid-provider-backend/internal/acmsearch"
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

// resourceSearch finds the resources of one kind on a managed cluster: ACM
// Search (acmsearch.Client) in production, which indexes the custom
// resources of every managed cluster, MaaSSubscriptions included.
type resourceSearch interface {
	Resources(ctx context.Context, kind, apigroup, cluster string) ([]acmsearch.Resource, error)
}

// maasAPIGroup is the API group of MaaS's kinds
// (opendatahub-io/models-as-a-service, maas.opendatahub.io/v1alpha1).
const maasAPIGroup = "maas.opendatahub.io"

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
//     ingress, or leaving the grid) while it serves ingress for any
//     subscription (checkNoSubscriptions: what ACM Search finds on the
//     cluster, else the catalog). Every ingress cluster gets the catalog's
//     subscriptions (catalog/subscriptions) and, from the gitops repository
//     itself, each organization's own, such as tenant-<org-id>-users
//     (ai-grid-cloud/ai-grid-cluster-gitops,
//     apps/acm-models/hub/maas-03-applicationset.yaml). Their API keys are
//     lost in a different way in each case:
//     - leaving the grid, or losing the role while staying in it, takes the
//     cluster out of the ai-grid-ingress Placement, and Argo CD deletes its
//     copies of the subscriptions (the resources finalizer on their
//     Applications, without Delete=false). maas-controller revokes the API
//     keys of each deleted MaaSSubscription in the maas-db every cluster
//     shares (opendatahub-io/models-as-a-service,
//     maas-controller/pkg/controller/maas/maassubscription_controller.go,
//     handleDeletion), so they stop working on every cluster. MaaS itself
//     stays: the rhoai ApplicationSet preserves what it installed
//     (apps/rhoai/hub/01-applicationset.yaml);
//     - a change to the inference role does the same, and also turns MaaS
//     off on the cluster: that role's profile sets modelsAsAService to
//     Removed (opendatahub-io/odh-gitops, rhai-on-openshift-chart README).
//     ai-gateway-operator would then tear maas-controller down
//     (internal/controller/aigateway/aigateway_remove.go), which would
//     delete the AITenant (maas-controller/pkg/controller/maas/
//     self_teardown.go), and deleting an AITenant revokes every API key of
//     that tenant in the shared maas-db (aitenant_controller.go;
//     maas-api/internal/api_keys/store_postgres.go). The grid has one
//     AITenant, models-as-a-service, so that can be every key in the grid,
//     not only those of the cluster's subscriptions. This is read from those
//     sources, not tried on a cluster;
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
// The rules are decided on what the hub, ACM Search and the catalog hold
// now, and nothing stops another writer changing that before the cluster is
// patched: a subscription created, a model placed on the cluster, or another
// cluster's role or membership changed, between the check and the patch is
// not refused. Search also lags the cluster by its collector's schedule. The
// patch is a ManagedCluster write and the catalog is Git, so there is no
// single commit to put both in.
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
		if err := s.checkNoSubscriptions(ctx, id, catalog, leaves, to); err != nil {
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
// Unavailable: the change waits until the catalog can be checked. The one
// exception is the wording of a refusal ACM Search already decided
// (checkNoSubscriptions).
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

// checkNoSubscriptions refuses when cluster id serves ingress for any
// subscription. They are the MaaSSubscriptions ACM Search indexes on the
// cluster, whoever delivered them: the catalog's, an organization's from the
// gitops repository, or one made by hand. When Search has none there, or the
// backend has no Search (--search-api-url=off), the catalog's subscriptions
// refuse instead, the files catalog/subscriptions/<id>.yaml
// (subscriptionsGitDir) that Argo CD applies to every ingress cluster: a
// subscription committed but not delivered yet, or a cluster whose Search
// collector isn't reporting, still counts. A Search query that fails is
// Unavailable, as a catalog that can't be read for that fallback is: the
// change waits until the subscriptions can be checked.
//
// The message names the subscriptions of whichever source found them and
// says where they are managed, because the console lists and deletes only
// the catalog's (Subscriptions/List and Delete). Of the subscriptions Search
// finds, those with a catalog file (a MaaSSubscription of the file's name)
// are the console's; the others are delivered from elsewhere, such as each
// organization's tenant-<org-id>-users (ai-grid-cloud/ai-grid-tenant,
// apps/acm-models/maas/policies/20-tenant-<org-id>-maas-policies.yaml).
// Without a catalog, none are the console's. A catalog that can't be read
// doesn't make the refusal Unavailable, since Search's answer alone refuses:
// the message then doesn't say which subscription is managed where. The
// catalog fallback's subscriptions are all the console's.
func (s *ClustersServer) checkNoSubscriptions(ctx context.Context, id string, catalog *catalogHead, leaves bool, to string) error {
	names, err := clusterSubscriptions(ctx, id, s.search)
	if err != nil {
		return err
	}
	var held heldSubscriptions
	if len(names) > 0 {
		held = s.splitByCatalog(ctx, id, names, catalog)
	} else if held.inConsole, err = catalogSubscriptions(ctx, id, catalog); err != nil {
		return err
	}
	if held.count() == 0 {
		return nil
	}
	return held.refusal(id, leaves, to)
}

// heldSubscriptions are the subscriptions that hold a cluster in ingress, by
// where they are managed: inConsole have a catalog file, outside don't, and
// unknown holds them all when the catalog couldn't be read to tell.
type heldSubscriptions struct {
	inConsole, outside, unknown []string
}

func (h heldSubscriptions) count() int {
	return len(h.inConsole) + len(h.outside) + len(h.unknown)
}

// splitByCatalog sorts the subscriptions Search found on cluster id by
// whether the catalog has a file for them, catalog/subscriptions/<name>.yaml.
// It reads the head the other checks of the update share.
func (s *ClustersServer) splitByCatalog(ctx context.Context, id string, names []string, catalog *catalogHead) heldSubscriptions {
	head, err := catalog.get(ctx)
	var files []string
	if err == nil && head != nil {
		files, err = head.List(subscriptionsGitDir)
	}
	if err != nil {
		s.logger.WarnContext(ctx, "read the catalog's subscriptions for a refusal", "cluster", id, "error", err)
		return heldSubscriptions{unknown: names}
	}
	var held heldSubscriptions
	for _, name := range names {
		if slices.Contains(files, name+".yaml") {
			held.inConsole = append(held.inConsole, name)
		} else {
			held.outside = append(held.outside, name)
		}
	}
	return held
}

// refusal is the FailedPrecondition for cluster id. The console shows its
// message to provider admins as it is, so it says why for what the admin is
// doing (checkRoleChange, rule 1), and where to delete the subscriptions.
func (h heldSubscriptions) refusal(id string, leaves bool, to string) error {
	total := h.count()
	count, the, them, isnt := fmt.Sprintf("%d subscriptions", total), "the subscriptions", "them", "aren't"
	copies := "its copies of those subscriptions, and their API keys are"
	if total == 1 {
		count, the, them, isnt = "1 subscription", "the subscription", "it", "isn't"
		copies = "its copy of that subscription, and its API keys are"
	}
	why := "Changing its role turns MaaS off there, which can revoke every API key in AI Grid."
	keep := fmt.Sprintf("keep the %s or %s role", roleIngress, roleCombined)
	switch {
	case leaves:
		why = fmt.Sprintf("Removing it from AI Grid deletes %s then revoked on every cluster.", copies)
		keep = "keep the cluster in AI Grid"
	case to == "":
		why = fmt.Sprintf("Removing its role deletes %s then revoked on every cluster.", copies)
	}
	var serves, act string
	switch {
	case len(h.unknown) > 0:
		serves = fmt.Sprintf("%s (%s)", count, nameIDs(h.unknown))
		where := "where it is managed (in the console, or in the GitOps repository that delivers it)"
		if total > 1 {
			where = "where each is managed (in the console, or in the GitOps repository that delivers it)"
		}
		act = fmt.Sprintf("Delete %s first, %s, or %s.", the, where, keep)
	case len(h.inConsole) > 0 && len(h.outside) > 0:
		serves = fmt.Sprintf("%s: %d managed in the console (%s) and %d managed outside it (%s)",
			count, len(h.inConsole), nameIDs(h.inConsole), len(h.outside), nameIDs(h.outside))
		console, other := "the one", "the other in the GitOps repository that delivers it"
		if len(h.inConsole) > 1 {
			console = "those"
		}
		if len(h.outside) > 1 {
			other = "the others in the GitOps repository that delivers them"
		}
		act = fmt.Sprintf("First delete %s in the console, and remove or move %s, or %s.", console, other, keep)
	case len(h.outside) > 0:
		serves = fmt.Sprintf("%s (%s)", count, nameIDs(h.outside))
		act = fmt.Sprintf("The %s %s managed in the console: first remove or move %s in the GitOps repository that delivers %s, or %s.",
			strings.TrimPrefix(the, "the "), isnt, them, them, keep)
	default:
		serves = fmt.Sprintf("%s (%s)", count, nameIDs(h.inConsole))
		act = fmt.Sprintf("Delete %s first, or %s.", the, keep)
	}
	return connect.Errorf(connect.CodeFailedPrecondition, "Cluster %q serves ingress for %s. %s %s", id, serves, why, act)
}

// clusterSubscriptions returns the names of the MaaSSubscriptions ACM Search
// indexes on cluster id, sorted, each once; none without a Search.
func clusterSubscriptions(ctx context.Context, id string, search resourceSearch) ([]string, error) {
	if search == nil {
		return nil, nil
	}
	found, err := search.Resources(ctx, kindMaaSSubscription, maasAPIGroup, id)
	if err != nil {
		return nil, connect.Errorf(connect.CodeUnavailable, "cluster %q: read its subscriptions from ACM Search: %v", id, err).WithCause(err)
	}
	names := make([]string, 0, len(found))
	for _, r := range found {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// catalogSubscriptions returns the ids of the catalog's subscriptions, the
// files catalog/subscriptions/<id>.yaml; none without a catalog.
func catalogSubscriptions(ctx context.Context, id string, catalog *catalogHead) ([]string, error) {
	head, err := catalog.get(ctx)
	if err != nil {
		return nil, connect.Errorf(connect.CodeUnavailable, "cluster %q: read the subscriptions: git: %v", id, err).WithCause(err)
	}
	if head == nil {
		return nil, nil
	}
	files, err := head.List(subscriptionsGitDir)
	if err != nil {
		return nil, subscriptionGitError("list", "", err)
	}
	var ids []string
	for _, file := range files {
		if sub, ok := strings.CutSuffix(file, ".yaml"); ok {
			ids = append(ids, sub)
		}
	}
	return ids, nil
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
