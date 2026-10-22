package servers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/celfilter"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// SubscriptionsServerBuilder configures a SubscriptionsServer.
type SubscriptionsServerBuilder struct {
	logger    *slog.Logger
	client    dynamic.Interface
	namespace string
}

// SubscriptionsServer implements ai_grid.v1.Subscriptions against namespaced
// MaaSSubscription CRs. Create/Update/Delete also maintain a managed
// MaaSAuthPolicy named <id>-policy in the same namespace.
type SubscriptionsServer struct {
	aigridv1connect.UnimplementedSubscriptionsHandler
	logger    *slog.Logger
	client    dynamic.Interface
	namespace string
}

var _ aigridv1connect.SubscriptionsHandler = (*SubscriptionsServer)(nil)

// NewSubscriptionsServer creates a builder for SubscriptionsServer.
func NewSubscriptionsServer() *SubscriptionsServerBuilder {
	return &SubscriptionsServerBuilder{}
}

// SetLogger sets the logger. Optional; a default is used when unset.
func (b *SubscriptionsServerBuilder) SetLogger(logger *slog.Logger) *SubscriptionsServerBuilder {
	b.logger = logger
	return b
}

// SetDynamicClient sets the Kubernetes dynamic client. Mandatory.
func (b *SubscriptionsServerBuilder) SetDynamicClient(client dynamic.Interface) *SubscriptionsServerBuilder {
	b.client = client
	return b
}

// SetNamespace sets the namespace for MaaSSubscription and MaaSAuthPolicy CRs.
// Optional; defaults to models-as-a-service.
func (b *SubscriptionsServerBuilder) SetNamespace(namespace string) *SubscriptionsServerBuilder {
	b.namespace = namespace
	return b
}

// Build creates the SubscriptionsServer.
func (b *SubscriptionsServerBuilder) Build() (*SubscriptionsServer, error) {
	if b.client == nil {
		return nil, errors.New("kubernetes dynamic client is mandatory")
	}
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	namespace := b.namespace
	if namespace == "" {
		namespace = defaultSubscriptionNamespace
	}
	return &SubscriptionsServer{logger: logger, client: b.client, namespace: namespace}, nil
}

// List returns MaaSSubscription objects from the configured namespace.
// Pipeline: map CRs → CEL filter → allowlisted order → offset/limit → enrich page.
func (s *SubscriptionsServer) List(ctx context.Context, req *aigridv1.SubscriptionsListRequest) (*aigridv1.SubscriptionsListResponse, error) {
	list, err := s.client.Resource(maasSubscriptionsResource).Namespace(s.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &aigridv1.SubscriptionsListResponse{}, nil
		}
		return nil, connect.Errorf(connect.CodeInternal, "list MaaSSubscription resources: %v", err).WithCause(err)
	}
	items := make([]*aigridv1.Subscription, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, subscriptionFromUnstructured(item))
	}

	filter, err := celfilter.Compile(req.GetFilter(), &aigridv1.Subscription{})
	if err != nil {
		return nil, err
	}
	if filter != nil {
		now := time.Now().UTC()
		matched := make([]*aigridv1.Subscription, 0, len(items))
		for _, sub := range items {
			ok, matchErr := filter.Match(ctx, sub, subscriptionSortID(sub), now)
			if matchErr != nil {
				return nil, matchErr
			}
			if ok {
				matched = append(matched, sub)
			}
		}
		items = matched
	}

	if err := applySubscriptionOrder(items, req.Order); err != nil {
		return nil, err
	}

	page, total := applyListWindow(items, req.Offset, req.Limit)
	for _, sub := range page {
		enrichModelRefsFromAnnotations(sub, s.loadModelRefs(ctx, sub))
	}
	return &aigridv1.SubscriptionsListResponse{
		Size:  int32(len(page)),
		Total: total,
		Items: page,
	}, nil
}

// Get returns a MaaSSubscription plus covering AuthPolicies and ModelRegistrations.
func (s *SubscriptionsServer) Get(ctx context.Context, req *aigridv1.SubscriptionsGetRequest) (*aigridv1.SubscriptionsGetResponse, error) {
	id := req.GetId()
	if err := validateSubscriptionID(id); err != nil {
		return nil, err
	}
	obj, err := s.client.Resource(maasSubscriptionsResource).Namespace(s.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, mapSubscriptionAPIError("get", id, err)
	}
	sub := subscriptionFromUnstructured(*obj)
	modelObjs := s.loadModelRefs(ctx, sub)
	enrichModelRefsFromAnnotations(sub, modelObjs)

	policies, err := s.coveringAuthPolicies(ctx, sub)
	if err != nil {
		return nil, err
	}
	regs := make([]*aigridv1.ModelRegistration, 0, len(modelObjs))
	for _, ref := range sub.GetSpec().GetModelRefs() {
		if ref == nil {
			continue
		}
		if obj := modelObjs[modelRefKey(ref.GetName(), ref.GetNamespace())]; obj != nil {
			regs = append(regs, modelRegistrationFromUnstructured(*obj))
		}
	}
	return &aigridv1.SubscriptionsGetResponse{
		Object:             sub,
		AuthPolicies:       policies,
		ModelRegistrations: regs,
	}, nil
}

// Create creates a MaaSSubscription and a managed MaaSAuthPolicy <id>-policy.
func (s *SubscriptionsServer) Create(ctx context.Context, req *aigridv1.SubscriptionsCreateRequest) (*aigridv1.SubscriptionsCreateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "object is required")
	}
	id := subscriptionID(object)
	if err := validateSubscriptionID(id); err != nil {
		return nil, err
	}
	if err := validateSubscriptionSpec(object.GetSpec()); err != nil {
		return nil, err
	}

	policyName := authPolicyNameForSubscription(id)
	if existing, err := s.client.Resource(maasAuthPoliciesResource).Namespace(s.namespace).Get(ctx, policyName, metav1.GetOptions{}); err == nil {
		if !isManagedAuthPolicy(existing) {
			return nil, connect.Errorf(connect.CodeFailedPrecondition, "auth policy %q already exists and is not managed by ai-grid-provider-backend", policyName)
		}
		return nil, connect.Errorf(connect.CodeAlreadyExists, "auth policy %q already exists", policyName)
	} else if !apierrors.IsNotFound(err) {
		return nil, mapSubscriptionAPIError("get auth policy", id, err)
	}

	created, err := s.client.Resource(maasSubscriptionsResource).Namespace(s.namespace).Create(
		ctx, subscriptionToUnstructured(object, s.namespace), metav1.CreateOptions{},
	)
	if err != nil {
		return nil, mapSubscriptionAPIError("create", id, err)
	}

	policy := authPolicyFromSubscription(object, s.namespace)
	createdPolicy, err := s.client.Resource(maasAuthPoliciesResource).Namespace(s.namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err != nil {
		if delErr := s.client.Resource(maasSubscriptionsResource).Namespace(s.namespace).Delete(ctx, id, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			s.logger.Warn("compensate: delete subscription after auth policy create failed", "id", id, "error", delErr)
		}
		return nil, mapSubscriptionAPIError("create auth policy", id, err)
	}

	return &aigridv1.SubscriptionsCreateResponse{
		Object:     subscriptionFromUnstructured(*created),
		AuthPolicy: authPolicyFromUnstructured(*createdPolicy),
	}, nil
}

// Update merge-patches the subscription and syncs a managed <id>-policy when present.
func (s *SubscriptionsServer) Update(ctx context.Context, req *aigridv1.SubscriptionsUpdateRequest) (*aigridv1.SubscriptionsUpdateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "object is required")
	}
	id := subscriptionID(object)
	if err := validateSubscriptionID(id); err != nil {
		return nil, err
	}

	current, err := s.client.Resource(maasSubscriptionsResource).Namespace(s.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, mapSubscriptionAPIError("get", id, err)
	}

	var version int32
	if object.GetMetadata() != nil {
		version = object.GetMetadata().GetVersion()
	}
	if req.GetLock() && version == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "lock requires metadata.version")
	}
	if version != 0 && strconv.FormatInt(int64(version), 10) != current.GetResourceVersion() {
		return nil, staleSubscriptionError("update", id)
	}

	paths := subscriptionUpdatePaths(req.GetUpdateMask(), object)
	if paths.spec {
		if err := validateSubscriptionSpec(object.GetSpec()); err != nil {
			return nil, err
		}
		currentSub := subscriptionFromUnstructured(*current)
		if err := validateOrganizationImmutable(currentSub.GetSpec(), object.GetSpec()); err != nil {
			return nil, err
		}
	}

	patchBody := map[string]interface{}{}
	patchMeta := map[string]interface{}{}
	if paths.labels {
		if object.GetMetadata() != nil {
			patchMeta["labels"] = stringMapToInterfaceMap(object.GetMetadata().GetLabels())
		} else {
			patchMeta["labels"] = map[string]interface{}{}
		}
	}
	if paths.annotations {
		if object.GetMetadata() != nil {
			patchMeta["annotations"] = stringMapToInterfaceMap(object.GetMetadata().GetAnnotations())
		} else {
			patchMeta["annotations"] = map[string]interface{}{}
		}
	}
	if len(patchMeta) > 0 {
		patchBody["metadata"] = patchMeta
	}
	if paths.spec {
		for k, v := range subscriptionSpecPatch(object) {
			patchBody[k] = v
		}
	}
	if len(patchBody) == 0 {
		return &aigridv1.SubscriptionsUpdateResponse{Object: subscriptionFromUnstructured(*current)}, nil
	}
	if version != 0 {
		meta, _ := patchBody["metadata"].(map[string]interface{})
		if meta == nil {
			meta = map[string]interface{}{}
			patchBody["metadata"] = meta
		}
		meta["resourceVersion"] = strconv.FormatInt(int64(version), 10)
	}

	patchBytes, err := json.Marshal(patchBody)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "marshal MaaSSubscription patch: %v", err).WithCause(err)
	}
	updated, err := s.client.Resource(maasSubscriptionsResource).Namespace(s.namespace).Patch(
		ctx, id, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		return nil, mapSubscriptionAPIError("update", id, err)
	}

	if paths.spec {
		if err := s.syncManagedAuthPolicy(ctx, object); err != nil {
			return nil, err
		}
	}

	return &aigridv1.SubscriptionsUpdateResponse{Object: subscriptionFromUnstructured(*updated)}, nil
}

// Delete deletes a MaaSSubscription and its managed <id>-policy when present.
func (s *SubscriptionsServer) Delete(ctx context.Context, req *aigridv1.SubscriptionsDeleteRequest) (*aigridv1.SubscriptionsDeleteResponse, error) {
	id := req.GetId()
	if err := validateSubscriptionID(id); err != nil {
		return nil, err
	}
	err := s.client.Resource(maasSubscriptionsResource).Namespace(s.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if err != nil {
		return nil, mapSubscriptionAPIError("delete", id, err)
	}

	policyName := authPolicyNameForSubscription(id)
	existing, getErr := s.client.Resource(maasAuthPoliciesResource).Namespace(s.namespace).Get(ctx, policyName, metav1.GetOptions{})
	if getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			s.logger.Warn("get auth policy after subscription delete", "policy", policyName, "error", getErr)
		}
		return &aigridv1.SubscriptionsDeleteResponse{}, nil
	}
	if !isManagedAuthPolicy(existing) {
		return &aigridv1.SubscriptionsDeleteResponse{}, nil
	}
	if delErr := s.client.Resource(maasAuthPoliciesResource).Namespace(s.namespace).Delete(ctx, policyName, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
		s.logger.Warn("delete managed auth policy", "policy", policyName, "error", delErr)
	}
	return &aigridv1.SubscriptionsDeleteResponse{}, nil
}

func (s *SubscriptionsServer) loadModelRefs(ctx context.Context, sub *aigridv1.Subscription) map[string]*unstructured.Unstructured {
	out := map[string]*unstructured.Unstructured{}
	if sub == nil || sub.GetSpec() == nil {
		return out
	}
	for _, ref := range sub.GetSpec().GetModelRefs() {
		if ref == nil || ref.GetName() == "" {
			continue
		}
		ns := ref.GetNamespace()
		if ns == "" {
			ns = defaultModelNamespace
		}
		key := modelRefKey(ref.GetName(), ns)
		if _, ok := out[key]; ok {
			continue
		}
		obj, err := s.client.Resource(maasModelRefsResource).Namespace(ns).Get(ctx, ref.GetName(), metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				s.logger.Warn("get MaaSModelRef", "name", ref.GetName(), "namespace", ns, "error", err)
			}
			continue
		}
		out[key] = obj
	}
	return out
}

func (s *SubscriptionsServer) coveringAuthPolicies(ctx context.Context, sub *aigridv1.Subscription) ([]*aigridv1.AuthPolicy, error) {
	modelKeys := map[string]struct{}{}
	for _, ref := range sub.GetSpec().GetModelRefs() {
		if ref == nil {
			continue
		}
		modelKeys[modelRefKey(ref.GetName(), ref.GetNamespace())] = struct{}{}
	}
	list, err := s.client.Resource(maasAuthPoliciesResource).Namespace(s.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, connect.Errorf(connect.CodeInternal, "list MaaSAuthPolicy resources: %v", err).WithCause(err)
	}
	out := make([]*aigridv1.AuthPolicy, 0)
	for i := range list.Items {
		item := &list.Items[i]
		if !authPolicyCoversModels(item, modelKeys) {
			continue
		}
		out = append(out, authPolicyFromUnstructured(*item))
	}
	return out, nil
}

func (s *SubscriptionsServer) syncManagedAuthPolicy(ctx context.Context, object *aigridv1.Subscription) error {
	id := subscriptionID(object)
	policyName := authPolicyNameForSubscription(id)
	existing, err := s.client.Resource(maasAuthPoliciesResource).Namespace(s.namespace).Get(ctx, policyName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return mapSubscriptionAPIError("get auth policy", id, err)
	}
	if !isManagedAuthPolicy(existing) {
		return nil
	}
	patchBytes, err := json.Marshal(authPolicySpecPatch(object))
	if err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal MaaSAuthPolicy patch: %v", err).WithCause(err)
	}
	if _, err := s.client.Resource(maasAuthPoliciesResource).Namespace(s.namespace).Patch(
		ctx, policyName, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	); err != nil {
		return mapSubscriptionAPIError("update auth policy", id, err)
	}
	return nil
}

type subUpdatePaths struct {
	labels, annotations, spec bool
}

func subscriptionUpdatePaths(mask *fieldmaskpb.FieldMask, object *aigridv1.Subscription) subUpdatePaths {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return subUpdatePaths{
			labels:      len(object.GetMetadata().GetLabels()) > 0,
			annotations: len(object.GetMetadata().GetAnnotations()) > 0,
			spec:        object.GetSpec() != nil,
		}
	}
	var paths subUpdatePaths
	for _, path := range mask.GetPaths() {
		p := strings.TrimPrefix(path, "object.")
		switch {
		case p == "metadata":
			paths.labels, paths.annotations = true, true
		case p == "metadata.labels" || strings.HasPrefix(p, "metadata.labels."):
			paths.labels = true
		case p == "metadata.annotations" || strings.HasPrefix(p, "metadata.annotations."):
			paths.annotations = true
		case p == "spec" || strings.HasPrefix(p, "spec."):
			paths.spec = true
		}
	}
	return paths
}

func validateSubscriptionID(id string) error {
	if id == "" {
		return connect.NewError(connect.CodeInvalidArgument, "id is required")
	}
	if !subscriptionNameRE.MatchString(id) {
		return connect.Errorf(connect.CodeInvalidArgument, "id %q is not a valid RFC 1123 DNS label", id)
	}
	if len(id) > maxSubscriptionIDLen {
		return connect.Errorf(connect.CodeInvalidArgument, "id %q is too long; leave room for the -policy suffix", id)
	}
	return nil
}

func validateSubscriptionSpec(spec *aigridv1.SubscriptionSpec) error {
	if spec == nil {
		return connect.NewError(connect.CodeInvalidArgument, "spec is required")
	}
	owner := spec.GetOwner()
	if owner == nil || (len(owner.GetGroups()) == 0 && len(owner.GetUsers()) == 0) {
		return connect.NewError(connect.CodeInvalidArgument, "spec.owner must include at least one group or user")
	}
	if org := spec.GetOrganization(); org != nil && strings.TrimSpace(org.GetId()) == "" {
		return connect.NewError(connect.CodeInvalidArgument, "spec.organization.id must be non-empty when organization is set")
	}
	refs := spec.GetModelRefs()
	if len(refs) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, "spec.model_refs must include at least one model")
	}
	for i, ref := range refs {
		if ref == nil || ref.GetName() == "" {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].name is required", i)
		}
		if !subscriptionNameRE.MatchString(ref.GetName()) {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].name %q is not a valid RFC 1123 DNS label", i, ref.GetName())
		}
		if ns := ref.GetNamespace(); ns != "" && !subscriptionNameRE.MatchString(ns) {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].namespace %q is not a valid RFC 1123 DNS label", i, ns)
		}
		limits := ref.GetTokenRateLimits()
		if ref.GetUnlimited() {
			if len(limits) > 0 {
				return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d]: token_rate_limits must not be set when unlimited is true", i)
			}
			continue
		}
		if len(limits) == 0 {
			return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d]: token_rate_limits is required unless unlimited is true", i)
		}
		for j, lim := range limits {
			if lim == nil {
				return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].token_rate_limits[%d] is required", i, j)
			}
			if lim.GetLimit() < 1 || lim.GetLimit() > 1_000_000_000 {
				return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].token_rate_limits[%d].limit must be between 1 and 1000000000", i, j)
			}
			if !tokenWindowRE.MatchString(lim.GetWindow()) {
				return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].token_rate_limits[%d].window %q is invalid", i, j, lim.GetWindow())
			}
		}
	}
	return nil
}

func validateOrganizationImmutable(current, next *aigridv1.SubscriptionSpec) error {
	var curID string
	if current != nil && current.GetOrganization() != nil {
		curID = current.GetOrganization().GetId()
	}
	if curID == "" {
		return nil
	}
	var nextID string
	if next != nil && next.GetOrganization() != nil {
		nextID = next.GetOrganization().GetId()
	}
	if nextID == "" {
		return connect.NewError(connect.CodeInvalidArgument, "spec.organization.id is immutable and cannot be removed")
	}
	if nextID != curID {
		return connect.NewError(connect.CodeInvalidArgument, "spec.organization.id is immutable")
	}
	return nil
}

func staleSubscriptionError(op, id string) *connect.Error {
	return connect.Errorf(connect.CodeAborted, "%s Subscription %q: the subscription changed since it was read; reload and retry", op, id)
}

func mapSubscriptionAPIError(op, id string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return connect.Errorf(connect.CodeNotFound, "Subscription %q not found", id).WithCause(err)
	case apierrors.IsAlreadyExists(err):
		return connect.Errorf(connect.CodeAlreadyExists, "Subscription %q already exists", id).WithCause(err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return connect.Errorf(connect.CodeInvalidArgument, "%s Subscription %q: %v", op, id, err).WithCause(err)
	case apierrors.IsConflict(err):
		return staleSubscriptionError(op, id).WithCause(err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return connect.Errorf(connect.CodePermissionDenied, "%s Subscription %q: %v", op, id, err).WithCause(err)
	default:
		return connect.Errorf(connect.CodeInternal, "%s Subscription %q: %v", op, id, err).WithCause(err)
	}
}
