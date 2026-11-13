package servers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"github.com/osac-project/ai-grid-provider-backend/internal/names"
	"google.golang.org/protobuf/proto"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// Each subscription is one file, catalog/subscriptions/<id>.yaml, holding its
// MaaSSubscription and its managed MaaSAuthPolicy <id>-policy. Argo CD applies
// the directory to the ingress clusters, and prunes both objects together when
// the file goes, so the policy needs no ownerReference (and has no UID to
// point at when it is committed).
const (
	subscriptionsGitDir = "catalog/subscriptions"

	// gitVersionAnnotation counts the changes to a file. metadata.version
	// reads it, as it would read a resourceVersion from a cluster.
	gitVersionAnnotation = "ai-grid/version"

	// retainAPIKeysOnDeleteAnnotation tells maas-controller not to revoke the
	// subscription's API keys when this copy of the MaaSSubscription is
	// deleted. Every ingress cluster gets its own copy, but they share one
	// maas-api database, and the revocation is keyed by tenant and
	// subscription name, so removing one cluster's copy (for example when the
	// cluster leaves the ingress placement) would revoke the keys on every
	// cluster (RHAI-20306). MaaS honours the annotation from
	// opendatahub-io/models-as-a-service#1841. The writer sets it on every
	// file it writes and the API never shows it.
	retainAPIKeysOnDeleteAnnotation = "maas.opendatahub.io/retain-api-keys-on-delete"

	// A Create request token is stored only as hashes and never returned. It
	// lets a client whose Create answer was lost retry without making a second
	// Subscription.
	subscriptionCreateRequestTokenAnnotation       = "ai-grid/create-request-token-sha256"
	subscriptionCreateRequestFingerprintAnnotation = "ai-grid/create-request-fingerprint-sha256"
)

func subscriptionGitPath(id string) string {
	return path.Join(subscriptionsGitDir, id+".yaml")
}

// subscriptionDoc is a parsed subscription file.
type subscriptionDoc struct {
	sub                      *aigridv1.Subscription
	policy                   *aigridv1.AuthPolicy
	createRequestTokenHash   string
	createRequestFingerprint string
}

func (s *SubscriptionsServer) gitList(ctx context.Context) ([]*aigridv1.Subscription, error) {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, subscriptionGitError("list", "", err)
	}
	names, err := head.List(subscriptionsGitDir)
	if err != nil {
		return nil, subscriptionGitError("list", "", err)
	}
	items := make([]*aigridv1.Subscription, 0, len(names))
	active := s.readReports(ctx, kindMaaSSubscription)
	catalog := s.modelCatalog(head)
	for _, name := range names {
		id, ok := strings.CutSuffix(name, ".yaml")
		if !ok {
			continue
		}
		doc, err := readSubscriptionDoc(head, id)
		if err != nil {
			return nil, err
		}
		catalog.join(ctx, doc.sub)
		items = append(items, withSubscriptionPhase(doc.sub, active))
	}
	return items, nil
}

// modelCatalog joins model refs to the Models API's catalog entries in one
// snapshot, reading each model's entry once.
type modelCatalog struct {
	head   gitops.Snapshot
	logger *slog.Logger
	// namespace is what a ref without a namespace means: the subscription
	// namespace, as renderSubscriptionDoc writes it.
	namespace string
	entries   map[string]*unstructured.Unstructured
}

func (s *SubscriptionsServer) modelCatalog(head gitops.Snapshot) *modelCatalog {
	return &modelCatalog{head: head, logger: s.logger, namespace: s.namespace, entries: map[string]*unstructured.Unstructured{}}
}

// modelRef returns the MaaSModelRef of model id's catalog entry,
// catalog/models/<id>/catalog/model.yaml (renderModelCatalog), or nil when
// the repository has no entry for id, or one that can't be read or parsed.
// The join only decorates refs, so a broken entry is logged, not returned:
// it must not fail the subscriptions that name the model, or keep them from
// being edited.
func (c *modelCatalog) modelRef(ctx context.Context, id string) *unstructured.Unstructured {
	if entry, ok := c.entries[id]; ok {
		return entry
	}
	c.entries[id] = nil
	// Only a model id names a catalog entry; a ref in a file edited by hand
	// could hold anything, such as a path.
	if !names.ValidDNSLabel(id) {
		return nil
	}
	p := path.Join(modelCatalogGitDir(id), "model.yaml")
	data, err := c.head.Read(p)
	if errors.Is(err, gitops.ErrNotFound) {
		return nil
	}
	if err == nil {
		var docs []*unstructured.Unstructured
		if docs, err = unmarshalYAMLDocs(data); err == nil {
			for _, doc := range docs {
				if doc.GetKind() == maasModelRefKind {
					c.entries[id] = doc
				}
			}
			return c.entries[id]
		}
	}
	c.logger.WarnContext(ctx, "read model catalog entry; its subscription refs are left without model_id", "model", id, "file", p, "error", err)
	return nil
}

// join sets each of sub's model refs' output fields from the catalog: a ref
// whose name and namespace are those of the MaaSModelRef in a model's catalog
// entry gets model_id, the model's id, and display_name and description from
// that MaaSModelRef's annotations, which renderModelCatalog copies from the
// model's. Any other ref gets none of them, whatever the caller sent.
func (c *modelCatalog) join(ctx context.Context, sub *aigridv1.Subscription) {
	for _, ref := range sub.GetSpec().GetModelRefs() {
		ref.ModelId, ref.DisplayName, ref.Description = "", "", ""
		entry := c.modelRef(ctx, ref.GetName())
		namespace := ref.GetNamespace()
		if namespace == "" {
			namespace = c.namespace
		}
		if entry == nil || entry.GetName() != ref.GetName() || entry.GetNamespace() != namespace {
			continue
		}
		annotations := entry.GetAnnotations()
		ref.ModelId = entry.GetName()
		ref.DisplayName = annotations[annotationDisplayName]
		ref.Description = annotations[annotationDescription]
	}
}

func (s *SubscriptionsServer) gitGet(ctx context.Context, id string) (*aigridv1.SubscriptionsGetResponse, error) {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, subscriptionGitError("get", id, err)
	}
	doc, err := readSubscriptionDoc(head, id)
	if err != nil {
		return nil, err
	}
	s.modelCatalog(head).join(ctx, doc.sub)
	resp := &aigridv1.SubscriptionsGetResponse{
		Object:             withSubscriptionPhase(doc.sub, s.readReports(ctx, kindMaaSSubscription)),
		ModelRegistrations: gitModelRegistrations(doc.sub, s.namespace, s.readIngressReports(ctx, kindMaaSModelRef)),
	}
	if doc.policy != nil {
		resp.AuthPolicies = []*aigridv1.AuthPolicy{withAuthPolicyStatus(doc.policy, s.readReports(ctx, kindMaaSAuthPolicy))}
	}
	return resp, nil
}

// readReports reads kind from the subscriptions ApplicationSet's Applications.
func (s *SubscriptionsServer) readReports(ctx context.Context, kind string) ObjectReports {
	if s.status == nil {
		return nil
	}
	return readReports(ctx, s.logger, s.status, SubscriptionsSet, kind)
}

// readIngressReports reads kind from the ingress ApplicationSet's Applications.
func (s *SubscriptionsServer) readIngressReports(ctx context.Context, kind string) ObjectReports {
	if s.status == nil {
		return nil
	}
	return readReports(ctx, s.logger, s.status, IngressSet, kind)
}

// maasPhase is the MaaS phase the clusters' reports add up to: Degraded when
// any cluster reports the object failed, Pending when any reports it pending
// or doesn't list it yet, Active when every cluster reports it ready, and
// empty when no cluster has an Application to report from.
func maasPhase(reports ObjectReports, name string) string {
	if !reports.any() {
		return ""
	}
	ready, pending, failed, missing := objectClusters(reports, name)
	switch {
	case len(failed) > 0:
		return "Degraded"
	case len(pending) > 0 || len(missing) > 0:
		return "Pending"
	case len(ready) > 0:
		return "Active"
	}
	return ""
}

// withSubscriptionPhase sets status.phase from the clusters' MaaSSubscription
// reports (maasPhase), and leaves it empty when no cluster has reported.
func withSubscriptionPhase(sub *aigridv1.Subscription, reports ObjectReports) *aigridv1.Subscription {
	phase := maasPhase(reports, sub.GetId())
	if phase == "" {
		return sub
	}
	if sub.Status == nil {
		sub.Status = &aigridv1.SubscriptionStatus{}
	}
	sub.Status.Phase = phase
	return sub
}

// withAuthPolicyStatus sets the subscription's MaaSAuthPolicy's phase and
// Ready condition from the subscriptions Applications' MaaSAuthPolicy
// reports, as gitModelRegistrations does for MaaSModelRefs: phase Active when
// every cluster reports the policy ready, Degraded when any reports it
// failed, Pending when any reports it pending or doesn't list it yet, and the
// condition's message says which cluster is which. With no report at all the
// policy has no phase and no condition, as the file has none.
func withAuthPolicyStatus(policy *aigridv1.AuthPolicy, reports ObjectReports) *aigridv1.AuthPolicy {
	if policy == nil || !reports.any() {
		return policy
	}
	ready, pending, failed, missing := objectClusters(reports, policy.GetId())
	condition := clustersReadyCondition(failed, pending, ready, missing)
	policy.Phase = phaseFromCondition(condition, "Active", "Degraded")
	policy.Conditions = []*aigridv1.Condition{condition}
	return policy
}

// clustersReadyCondition is a Ready condition from the clusters that report
// an object failed, not ready, ready, or not at all: True, reason Ready, when
// only ready ones do; False, reason Failed, when any reports it failed; and
// False, reason NotReady, otherwise. Its message says which cluster is which.
func clustersReadyCondition(failed, notReady, ready, missing []string) *aigridv1.Condition {
	condition := &aigridv1.Condition{
		Type:    "Ready",
		Status:  aigridv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason:  proto.String("Ready"),
		Message: proto.String(clusterSummary(failed, notReady, ready, missing)),
	}
	switch {
	case len(failed) > 0:
		condition.Status = aigridv1.ConditionStatus_CONDITION_STATUS_FALSE
		condition.Reason = proto.String("Failed")
	case len(notReady)+len(missing) > 0:
		condition.Status = aigridv1.ConditionStatus_CONDITION_STATUS_FALSE
		condition.Reason = proto.String("NotReady")
	}
	return condition
}

// phaseFromCondition is the phase a clustersReadyCondition gives: readyPhase
// when it is True, failedPhase when its reason is Failed, Pending otherwise.
func phaseFromCondition(condition *aigridv1.Condition, readyPhase, failedPhase string) string {
	switch {
	case condition.GetStatus() == aigridv1.ConditionStatus_CONDITION_STATUS_TRUE:
		return readyPhase
	case condition.GetReason() == "Failed":
		return failedPhase
	}
	return "Pending"
}

// gitModelRegistrations stands in for the MaaSModelRef status a cluster
// reports, from the ingress Applications: phase Ready when every cluster
// reports the MaaSModelRef ready, Failed when any reports it failed, and
// Pending when any reports it pending or doesn't list it yet. A model with no report, or in another namespace
// than the one the Models API writes MaaSModelRefs to, has no phase and a
// Ready condition of unknown status, saying it was saved to Git. Without an
// entry, a client takes the model as missing.
func gitModelRegistrations(sub *aigridv1.Subscription, namespace string, modelRefs ObjectReports) []*aigridv1.ModelRegistration {
	var out []*aigridv1.ModelRegistration
	for _, ref := range sub.GetSpec().GetModelRefs() {
		var ready, notReady, failed, missing []string
		if ref.GetNamespace() == namespace && modelRefs.any() {
			ready, notReady, failed, missing = objectClusters(modelRefs, ref.GetName())
		}
		if len(ready)+len(notReady)+len(failed)+len(missing) > 0 {
			condition := clustersReadyCondition(failed, notReady, ready, missing)
			out = append(out, &aigridv1.ModelRegistration{
				Name:       ref.GetName(),
				Namespace:  ref.GetNamespace(),
				Phase:      phaseFromCondition(condition, "Ready", "Failed"),
				Conditions: []*aigridv1.Condition{condition},
			})
			continue
		}
		out = append(out, &aigridv1.ModelRegistration{
			Name:      ref.GetName(),
			Namespace: ref.GetNamespace(),
			Conditions: []*aigridv1.Condition{{
				Type:    "Ready",
				Status:  aigridv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED,
				Reason:  proto.String("SavedToGit"),
				Message: proto.String("Saved to Git; no cluster has reported the model's status"),
			}},
		})
	}
	return out
}

var errSubscriptionCreateRequestTokenNeedsFreshHead = errors.New("request token matched on a stale head")

func (s *SubscriptionsServer) gitCreate(ctx context.Context, object *aigridv1.Subscription, requestToken string) (*aigridv1.SubscriptionsCreateResponse, error) {
	if err := validateGitMetadata(object.GetMetadata()); err != nil {
		return nil, err
	}
	if _, err := qualifiedOwnerGroups(object.GetSpec()); err != nil {
		return nil, err
	}
	requestTokenHash := ""
	requestFingerprint := ""
	if requestToken != "" {
		requestTokenHash = subscriptionCreateRequestTokenHash(requestToken)
		fingerprint, err := subscriptionCreateFingerprint(object, s.namespace)
		if err != nil {
			return nil, err
		}
		requestFingerprint = fingerprint
	}
	var resp *aigridv1.SubscriptionsCreateResponse
	finalID := subscriptionID(object)
	err := decideOnHead(ctx, s.writer, func(err error) error { return subscriptionGitError("create", finalID, err) }, func(head gitops.Snapshot) (err error) {
		resp, finalID, err = s.gitCreateOn(ctx, head, proto.CloneOf(object), requestTokenHash, requestFingerprint)
		return err
	})
	if errors.Is(err, errSubscriptionCreateRequestTokenNeedsFreshHead) {
		head, freshErr := s.writer.FreshHead(ctx)
		if freshErr != nil {
			return nil, subscriptionGitError("create", finalID, freshErr)
		}
		resp, finalID, err = s.gitCreateOn(ctx, head, proto.CloneOf(object), requestTokenHash, requestFingerprint)
	}
	return resp, err
}

// gitCreateOn decides and commits the create of object on head.
func (s *SubscriptionsServer) gitCreateOn(ctx context.Context, head gitops.Snapshot, object *aigridv1.Subscription, requestTokenHash, requestFingerprint string) (*aigridv1.SubscriptionsCreateResponse, string, error) {
	var read []string
	if requestTokenHash != "" {
		existing, err := findSubscriptionByCreateRequestToken(head, requestTokenHash)
		if err != nil {
			return nil, subscriptionID(object), err
		}
		if existing != nil {
			if !head.Fresh() {
				return nil, existing.sub.GetId(), errSubscriptionCreateRequestTokenNeedsFreshHead
			}
			if existing.createRequestFingerprint != requestFingerprint {
				return nil, existing.sub.GetId(), connect.Errorf(
					connect.CodeAlreadyExists,
					"request_token already created Subscription %q with a different payload",
					existing.sub.GetId(),
				)
			}
			return s.subscriptionCreateResponse(ctx, head, existing), existing.sub.GetId(), nil
		}
		read = append(read, subscriptionsGitDir)
	}
	id, err := assignGeneratedSubscriptionID(head, object)
	if err != nil {
		return nil, subscriptionID(object), err
	}
	modelRead, err := s.gitEnsureModelsExist(head, "create", object)
	if err != nil {
		return nil, id, err
	}
	read = append(read, modelRead...)
	setSubscriptionCreateRequestMetadata(object, requestTokenHash, requestFingerprint)
	doc, err := s.gitCommit(ctx, head.Revision(), "create", object, 1, read)
	if err != nil {
		return nil, id, err
	}
	// The commit changes no catalog entry, so the snapshot read still holds.
	return s.subscriptionCreateResponse(ctx, head, doc), id, nil
}

func (s *SubscriptionsServer) subscriptionCreateResponse(ctx context.Context, head gitops.Snapshot, doc *subscriptionDoc) *aigridv1.SubscriptionsCreateResponse {
	s.modelCatalog(head).join(ctx, doc.sub)
	return &aigridv1.SubscriptionsCreateResponse{
		Object:     withSubscriptionPhase(doc.sub, s.readReports(ctx, kindMaaSSubscription)),
		AuthPolicy: withAuthPolicyStatus(doc.policy, s.readReports(ctx, kindMaaSAuthPolicy)),
	}
}

func findSubscriptionByCreateRequestToken(head gitops.Snapshot, tokenHash string) (*subscriptionDoc, error) {
	names, err := head.List(subscriptionsGitDir)
	if err != nil {
		return nil, subscriptionGitError("create", "", err)
	}
	var found *subscriptionDoc
	for _, name := range names {
		id, ok := strings.CutSuffix(name, ".yaml")
		if !ok {
			continue
		}
		doc, err := readSubscriptionDoc(head, id)
		if err != nil {
			return nil, err
		}
		if doc.createRequestTokenHash != tokenHash {
			continue
		}
		if found != nil {
			return nil, connect.Errorf(
				connect.CodeInternal,
				"Subscription request token is stored by Subscriptions %q and %q",
				found.sub.GetId(),
				doc.sub.GetId(),
			)
		}
		found = doc
	}
	return found, nil
}

func subscriptionCreateRequestTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func subscriptionCreateFingerprint(object *aigridv1.Subscription, namespace string) (string, error) {
	normalized := proto.CloneOf(object)
	normalized.Id = ""
	if normalized.Metadata == nil {
		normalized.Metadata = &aigridv1.Metadata{}
	}
	normalized.Metadata.Name = ""
	normalized.Metadata.Creator = ""
	normalized.Metadata.Tenant = ""
	normalized.Metadata.CreationTimestamp = nil
	normalized.Metadata.DeletionTimestamp = nil
	normalized.Metadata.Version = 0
	normalized.Status = nil

	groups, err := qualifiedOwnerGroups(normalized.GetSpec())
	if err != nil {
		return "", err
	}
	if owner := normalized.GetSpec().GetOwner(); owner != nil {
		owner.Groups = groups
	}
	for _, ref := range normalized.GetSpec().GetModelRefs() {
		if ref.GetNamespace() == "" {
			ref.Namespace = namespace
		}
		ref.ModelId = ""
		ref.DisplayName = ""
		ref.Description = ""
	}

	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(normalized)
	if err != nil {
		return "", connect.Errorf(connect.CodeInternal, "fingerprint Subscription create payload: %v", err).WithCause(err)
	}
	hash := sha256.New()
	hash.Write([]byte("subscription-create-fingerprint-v1"))
	hash.Write([]byte{0})
	hash.Write([]byte(namespace))
	hash.Write([]byte{0})
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func setSubscriptionCreateRequestMetadata(object *aigridv1.Subscription, tokenHash, fingerprint string) {
	if tokenHash == "" {
		return
	}
	if object.Metadata == nil {
		object.Metadata = &aigridv1.Metadata{}
	}
	if object.Metadata.Annotations == nil {
		object.Metadata.Annotations = map[string]string{}
	}
	object.Metadata.Annotations[subscriptionCreateRequestTokenAnnotation] = tokenHash
	object.Metadata.Annotations[subscriptionCreateRequestFingerprintAnnotation] = fingerprint
}

func preserveSubscriptionCreateRequestMetadata(object *aigridv1.Subscription, current *subscriptionDoc) {
	if current.createRequestTokenHash == "" {
		return
	}
	setSubscriptionCreateRequestMetadata(object, current.createRequestTokenHash, current.createRequestFingerprint)
}

func assignGeneratedSubscriptionID(head gitops.Snapshot, object *aigridv1.Subscription) (string, error) {
	base := subscriptionID(object)
	for range subscriptionGeneratedSuffixAttempts {
		suffix, err := generatedSubscriptionSuffix()
		if err != nil {
			return "", connect.Errorf(connect.CodeInternal, "generate Subscription suffix: %v", err).WithCause(err)
		}
		id := base + "-" + suffix
		if _, err := head.Read(subscriptionGitPath(id)); errors.Is(err, gitops.ErrNotFound) {
			object.Id = id
			if object.Metadata == nil {
				object.Metadata = &aigridv1.Metadata{}
			}
			object.Metadata.Name = id
			return id, nil
		} else if err != nil {
			return "", subscriptionGitError("create", id, err)
		}
	}
	return "", connect.Errorf(connect.CodeInternal, "generate an unused Subscription id for base %q", base)
}

func generatedSubscriptionSuffix() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	// 252 is the largest multiple of the alphabet length that fits in a
	// byte. Rejecting values at or above it avoids modulo bias.
	const maxUnbiasedByte = 252
	suffix := make([]byte, subscriptionGeneratedSuffixLength)
	for i := range suffix {
		for {
			var value [1]byte
			if _, err := rand.Read(value[:]); err != nil {
				return "", fmt.Errorf("read random bytes: %w", err)
			}
			if value[0] >= maxUnbiasedByte {
				continue
			}
			suffix[i] = alphabet[int(value[0])%len(alphabet)]
			break
		}
	}
	return string(suffix), nil
}

func (s *SubscriptionsServer) gitUpdate(ctx context.Context, req *aigridv1.SubscriptionsUpdateRequest) (*aigridv1.SubscriptionsUpdateResponse, error) {
	object := req.GetObject()
	id := subscriptionID(object)
	version := object.GetMetadata().GetVersion()
	if req.GetLock() && version == 0 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "lock requires metadata.version")
	}
	paths, err := subscriptionUpdatePaths(req.GetUpdateMask(), object)
	if err != nil {
		return nil, err
	}
	var resp *aigridv1.SubscriptionsUpdateResponse
	err = decideOnHead(ctx, s.writer, func(err error) error { return subscriptionGitError("update", id, err) }, func(head gitops.Snapshot) (err error) {
		resp, err = s.gitUpdateOn(ctx, head, proto.CloneOf(object), version, paths)
		return err
	})
	return resp, err
}

// gitUpdateOn decides and commits an update of object, at version (0 for
// none), to the subscription as head has it.
func (s *SubscriptionsServer) gitUpdateOn(ctx context.Context, head gitops.Snapshot, object *aigridv1.Subscription, version int32, paths subUpdatePaths) (*aigridv1.SubscriptionsUpdateResponse, error) {
	id := subscriptionID(object)
	current, err := readSubscriptionDoc(head, id)
	if err != nil {
		return nil, err
	}
	currentVersion := current.sub.GetMetadata().GetVersion()
	if version != 0 && version != currentVersion {
		return nil, staleSubscriptionError("update", id)
	}
	// A mask that names nothing the file stores changes nothing and commits
	// nothing.
	if !paths.labels && !paths.annotations && !paths.anySpec() {
		s.modelCatalog(head).join(ctx, current.sub)
		return &aigridv1.SubscriptionsUpdateResponse{Object: withSubscriptionPhase(current.sub, s.readReports(ctx, kindMaaSSubscription))}, nil
	}

	candidate := mergeSubscriptionUpdate(current.sub, object, paths)
	candidate.Status = nil
	if err := validateGitMetadata(candidate.GetMetadata()); err != nil {
		return nil, err
	}
	preserveSubscriptionCreateRequestMetadata(candidate, current)
	var read []string
	if paths.anySpec() {
		if err := validateSubscriptionSpec(candidate.GetSpec()); err != nil {
			return nil, err
		}
		if err := validateOrganizationImmutable(current.sub.GetSpec(), candidate.GetSpec()); err != nil {
			return nil, err
		}
		if _, err := qualifiedOwnerGroups(candidate.GetSpec()); err != nil {
			return nil, err
		}
		if read, err = s.gitEnsureModelsExist(head, "update", candidate); err != nil {
			return nil, err
		}
	}
	doc, err := s.gitCommit(ctx, head.Revision(), "update", candidate, currentVersion+1, read)
	if err != nil {
		return nil, err
	}
	// As in gitCreate.
	s.modelCatalog(head).join(ctx, doc.sub)
	return &aigridv1.SubscriptionsUpdateResponse{Object: withSubscriptionPhase(doc.sub, s.readReports(ctx, kindMaaSSubscription))}, nil
}

// gitDelete removes the subscription's file, and with it the managed policy.
// A nonzero version must equal the file's, or the delete is Aborted. The
// commit is made on the revision read, so a concurrent write to the file
// makes it conflict, and Delete's rerun checks the version again.
func (s *SubscriptionsServer) gitDelete(ctx context.Context, id string, version int32) error {
	return decideOnHead(ctx, s.writer, func(err error) error { return subscriptionGitError("delete", id, err) }, func(head gitops.Snapshot) error {
		return s.gitDeleteOn(ctx, head, id, version)
	})
}

// gitDeleteOn decides and commits the delete of subscription id, at version
// (0 for none), as head has it.
func (s *SubscriptionsServer) gitDeleteOn(ctx context.Context, head gitops.Snapshot, id string, version int32) error {
	current, err := readSubscriptionDoc(head, id)
	if err != nil {
		return err
	}
	if version != 0 && version != current.sub.GetMetadata().GetVersion() {
		return staleSubscriptionError("delete", id)
	}
	_, err = s.writer.Commit(ctx, head.Revision(), gitops.Change{
		Delete:  []string{subscriptionGitPath(id)},
		Message: "subscriptions: delete " + id,
	})
	if err != nil {
		return subscriptionGitError("delete", id, err)
	}
	return nil
}

// gitEnsureModelsExist refuses a subscription with a model ref, in the
// subscription namespace, to a model the repository doesn't have: Argo CD
// would apply a MaaSSubscription naming a MaaSModelRef that doesn't exist. A
// ref to another namespace isn't checked, because the Models API doesn't
// write there. It returns the files it read, for the commit's read set, so a
// model deleted before the commit makes it conflict, and the rerun answers
// FailedPrecondition.
func (s *SubscriptionsServer) gitEnsureModelsExist(head gitops.Snapshot, op string, object *aigridv1.Subscription) ([]string, error) {
	var read []string
	for _, ref := range object.GetSpec().GetModelRefs() {
		if ref.GetNamespace() != "" && ref.GetNamespace() != s.namespace {
			continue
		}
		p := path.Join(modelDeploymentGitDir(ref.GetName()), "deployment.yaml")
		_, err := head.Read(p)
		if errors.Is(err, gitops.ErrNotFound) {
			return nil, connect.Errorf(connect.CodeFailedPrecondition, "spec.model_refs: Model %q does not exist", ref.GetName())
		}
		if err != nil {
			return nil, subscriptionGitError(op, subscriptionID(object), err)
		}
		read = append(read, p)
	}
	return read, nil
}

// gitCommit writes object's file at version on top of base and returns the
// subscription as read back from that file. read is the commit's read set.
func (s *SubscriptionsServer) gitCommit(ctx context.Context, base, op string, object *aigridv1.Subscription, version int32, read []string) (*subscriptionDoc, error) {
	id := subscriptionID(object)
	data, err := renderSubscriptionDoc(object, s.namespace, version)
	if err != nil {
		return nil, err
	}
	if _, err := s.writer.Commit(ctx, base, gitops.Change{
		Put:     map[string][]byte{subscriptionGitPath(id): data},
		Read:    read,
		Message: fmt.Sprintf("subscriptions: %s %s", op, id),
	}); err != nil {
		return nil, subscriptionGitError(op, id, err)
	}
	doc, err := parseSubscriptionDoc(id, data)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "%s Subscription %q: %v", op, id, err).WithCause(err)
	}
	return doc, nil
}

// validateGitMetadata allows only the labels and annotations a caller may
// set in git mode, because what the backend commits is applied as is: keys
// without a prefix, such as team or env, and the console's
// openshift.io/display-name and openshift.io/description annotations. Any
// other prefixed key, such as Argo CD's, Helm's, kubectl's or the backend's
// own ai-grid/ keys, is InvalidArgument. The backend adds its own keys after
// this check.
func validateGitMetadata(meta *aigridv1.Metadata) error {
	for key := range meta.GetLabels() {
		if strings.Contains(key, "/") {
			return connect.Errorf(connect.CodeInvalidArgument, "metadata.labels key %q is not allowed: only keys without a prefix", key)
		}
	}
	for key := range meta.GetAnnotations() {
		if strings.Contains(key, "/") && key != annotationDisplayName && key != annotationDescription {
			return connect.Errorf(connect.CodeInvalidArgument, "metadata.annotations key %q is not allowed: only keys without a prefix, %s and %s", key, annotationDisplayName, annotationDescription)
		}
	}
	// Argo CD applies the file as is, so the keys and values must be ones
	// Kubernetes accepts, as cluster mode's API server checked.
	for key, value := range meta.GetLabels() {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return connect.Errorf(connect.CodeInvalidArgument, "metadata.labels key %q: %s", key, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return connect.Errorf(connect.CodeInvalidArgument, "metadata.labels[%s] value %q: %s", key, value, strings.Join(errs, "; "))
		}
	}
	if errs := apivalidation.ValidateAnnotations(meta.GetAnnotations(), field.NewPath("metadata", "annotations")); len(errs) > 0 {
		return connect.Errorf(connect.CodeInvalidArgument, "%v", errs.ToAggregate())
	}
	return nil
}

// qualifiedOwnerGroups returns the owner groups as the file holds them,
// <organization id>/<group>: the tenant-qualified form the gateway puts in
// the token. spec.organization.id is required, and must be a DNS label so it
// can't hold a separator that changes which group is granted. A group may be
// sent bare or already qualified with the same organization; a group
// qualified with another organization, or empty, is InvalidArgument.
// Surrounding spaces are trimmed, and repeats are dropped, keeping the first.
func qualifiedOwnerGroups(spec *aigridv1.SubscriptionSpec) ([]string, error) {
	org := spec.GetOrganization().GetId()
	if org == "" {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.organization.id is required")
	}
	if !names.ValidDNSLabel(org) {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.organization.id %q is not a valid organization id", org)
	}
	groups := spec.GetOwner().GetGroups()
	out := make([]string, 0, len(groups))
	seen := make(map[string]bool, len(groups))
	for i, group := range groups {
		name := strings.TrimSpace(group)
		if prefix, rest, qualified := strings.Cut(name, "/"); qualified {
			if prefix != org {
				return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.owner.groups[%d] %q belongs to another organization; send the bare group name", i, group)
			}
			name = strings.TrimSpace(rest)
		}
		if name == "" || strings.Contains(name, "/") {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.owner.groups[%d] %q is not a valid group name", i, group)
		}
		qualified := org + "/" + name
		if seen[qualified] {
			continue
		}
		seen[qualified] = true
		out = append(out, qualified)
	}
	return out, nil
}

// renderSubscriptionDoc renders the subscription file: the MaaSSubscription,
// with the version and retain-api-keys annotations, and its managed
// MaaSAuthPolicy. Owner groups
// are qualified with the organization in both. A model ref without a
// namespace gets the subscription's namespace, where the Models API writes
// each model's MaaSModelRef.
func renderSubscriptionDoc(object *aigridv1.Subscription, namespace string, version int32) ([]byte, error) {
	id := subscriptionID(object)
	qualified := proto.CloneOf(object)
	groups, err := qualifiedOwnerGroups(qualified.GetSpec())
	if err != nil {
		return nil, err
	}
	if owner := qualified.GetSpec().GetOwner(); owner != nil {
		owner.Groups = groups
	}
	for _, ref := range qualified.GetSpec().GetModelRefs() {
		if ref.GetNamespace() == "" {
			ref.Namespace = namespace
		}
	}

	sub := subscriptionToUnstructured(qualified, namespace)
	annotations := sub.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[gitVersionAnnotation] = strconv.FormatInt(int64(version), 10)
	annotations[retainAPIKeysOnDeleteAnnotation] = "true"
	sub.SetAnnotations(annotations)

	data, err := marshalYAMLDocs(sub, authPolicyFromSubscription(qualified, namespace))
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "render Subscription %q: %v", id, err).WithCause(err)
	}
	return data, nil
}

func readSubscriptionDoc(head gitops.Snapshot, id string) (*subscriptionDoc, error) {
	data, err := head.Read(subscriptionGitPath(id))
	if err != nil {
		return nil, subscriptionGitError("get", id, err)
	}
	doc, err := parseSubscriptionDoc(id, data)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read %s: %v", subscriptionGitPath(id), err).WithCause(err)
	}
	return doc, nil
}

// parseSubscriptionDoc reverses renderSubscriptionDoc: it moves the version
// annotation to metadata.version, drops the retain-api-keys annotation, and
// strips the organization from owner groups.
func parseSubscriptionDoc(id string, data []byte) (*subscriptionDoc, error) {
	docs, err := unmarshalYAMLDocs(data)
	if err != nil {
		return nil, err
	}
	doc := &subscriptionDoc{}
	for _, item := range docs {
		switch item.GetKind() {
		case "MaaSSubscription":
			doc.sub = subscriptionFromUnstructured(*item)
		case "MaaSAuthPolicy":
			doc.policy = authPolicyFromUnstructured(*item)
		}
	}
	if doc.sub == nil || doc.sub.GetId() != id {
		return nil, fmt.Errorf("no MaaSSubscription %q in the file", id)
	}

	meta := doc.sub.GetMetadata()
	doc.createRequestTokenHash = meta.GetAnnotations()[subscriptionCreateRequestTokenAnnotation]
	doc.createRequestFingerprint = meta.GetAnnotations()[subscriptionCreateRequestFingerprintAnnotation]
	version, err := strconv.ParseInt(meta.GetAnnotations()[gitVersionAnnotation], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("annotation %s: %w", gitVersionAnnotation, err)
	}
	meta.Version = int32(version)
	delete(meta.Annotations, gitVersionAnnotation)
	delete(meta.Annotations, retainAPIKeysOnDeleteAnnotation)
	delete(meta.Annotations, subscriptionCreateRequestTokenAnnotation)
	delete(meta.Annotations, subscriptionCreateRequestFingerprintAnnotation)
	if len(meta.Annotations) == 0 {
		meta.Annotations = nil
	}

	prefix := doc.sub.GetSpec().GetOrganization().GetId() + "/"
	if owner := doc.sub.GetSpec().GetOwner(); owner != nil {
		for i, group := range owner.Groups {
			owner.Groups[i] = strings.TrimPrefix(group, prefix)
		}
	}
	return doc, nil
}

// marshalYAMLDocs renders objects as one multi-document YAML file.
func marshalYAMLDocs(objects ...*unstructured.Unstructured) ([]byte, error) {
	var out bytes.Buffer
	for i, obj := range objects {
		data, err := yaml.Marshal(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", obj.GetKind(), err)
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(data)
	}
	return out.Bytes(), nil
}

// unmarshalYAMLDocs reverses marshalYAMLDocs.
func unmarshalYAMLDocs(data []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, part := range bytes.Split(data, []byte("\n---\n")) {
		obj := map[string]interface{}{}
		if err := yaml.Unmarshal(part, &obj); err != nil {
			return nil, err
		}
		out = append(out, &unstructured.Unstructured{Object: obj})
	}
	return out, nil
}

func subscriptionGitError(op, id string, err error) error {
	return mapGitopsError("Subscription", "Subscriptions", op, id, err, staleSubscriptionError)
}
