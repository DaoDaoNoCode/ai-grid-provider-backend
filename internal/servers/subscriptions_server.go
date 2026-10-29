package servers

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/celfilter"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// SubscriptionsServerBuilder configures a SubscriptionsServer.
type SubscriptionsServerBuilder struct {
	logger    *slog.Logger
	writer    gitops.Writer
	status    *CatalogStatusReader
	namespace string
}

// SubscriptionsServer implements ai_grid.v1.Subscriptions over a Git
// repository: each subscription is a MaaSSubscription and its managed
// MaaSAuthPolicy <id>-policy, committed to one file that Argo CD delivers to
// the ingress clusters (subscriptions_git.go). It never writes to the cluster;
// it reads status from the hub through its CatalogStatusReader.
type SubscriptionsServer struct {
	aigridv1connect.UnimplementedSubscriptionsHandler
	logger    *slog.Logger
	writer    gitops.Writer
	status    *CatalogStatusReader
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

// SetWriter sets the Git repository subscriptions are read from and committed
// to. Mandatory.
func (b *SubscriptionsServerBuilder) SetWriter(writer gitops.Writer) *SubscriptionsServerBuilder {
	b.writer = writer
	return b
}

// SetCatalogStatus sets where status is read from the spokes. Optional;
// without it every subscription has no phase and every model ref is saved to
// Git.
func (b *SubscriptionsServerBuilder) SetCatalogStatus(status *CatalogStatusReader) *SubscriptionsServerBuilder {
	b.status = status
	return b
}

// SetNamespace sets the namespace of the MaaSSubscription and MaaSAuthPolicy
// objects, and the default namespace of model refs. Optional; defaults to
// models-as-a-service.
func (b *SubscriptionsServerBuilder) SetNamespace(namespace string) *SubscriptionsServerBuilder {
	b.namespace = namespace
	return b
}

// Build creates the SubscriptionsServer.
func (b *SubscriptionsServerBuilder) Build() (*SubscriptionsServer, error) {
	if b.writer == nil {
		return nil, errors.New("git writer is mandatory")
	}
	logger := b.logger
	if logger == nil {
		logger = slog.Default()
	}
	namespace := b.namespace
	if namespace == "" {
		namespace = defaultSubscriptionNamespace
	}
	return &SubscriptionsServer{logger: logger, writer: b.writer, status: b.status, namespace: namespace}, nil
}

// List returns the repository's subscriptions.
// Pipeline: read files → CEL filter → allowlisted order → offset/limit.
func (s *SubscriptionsServer) List(ctx context.Context, req *aigridv1.SubscriptionsListRequest) (*aigridv1.SubscriptionsListResponse, error) {
	if err := validateListLimit(req.Limit); err != nil {
		return nil, err
	}
	items, err := s.gitList(ctx)
	if err != nil {
		return nil, err
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
	return &aigridv1.SubscriptionsListResponse{
		Size:  int32(len(page)),
		Total: total,
		Items: page,
	}, nil
}

// Get returns a subscription, its managed AuthPolicy and one ModelRegistration
// per model ref.
func (s *SubscriptionsServer) Get(ctx context.Context, req *aigridv1.SubscriptionsGetRequest) (*aigridv1.SubscriptionsGetResponse, error) {
	id := req.GetId()
	if err := validateSubscriptionID(id); err != nil {
		return nil, err
	}
	return s.gitGet(ctx, id)
}

// Create commits a subscription and its managed AuthPolicy <id>-policy. A
// commit that conflicts reruns the whole create on the new head
// (gitops.RetryOnConflict): a subscription created meanwhile under the same
// id is AlreadyExists, and a model ref to a model deleted meanwhile is
// FailedPrecondition.
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
	var resp *aigridv1.SubscriptionsCreateResponse
	err := gitops.RetryOnConflict(ctx, func() (err error) {
		resp, err = s.gitCreate(ctx, proto.Clone(object).(*aigridv1.Subscription))
		return err
	})
	return resp, err
}

// Update commits the fields the mask selects, to the subscription and its
// managed AuthPolicy. A commit that conflicts reruns the whole update on the
// new head, from the request as sent: a version that no longer matches is
// still Aborted, and an update without one merges onto the latest
// subscription.
func (s *SubscriptionsServer) Update(ctx context.Context, req *aigridv1.SubscriptionsUpdateRequest) (*aigridv1.SubscriptionsUpdateResponse, error) {
	object := req.GetObject()
	if object == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "object is required")
	}
	if err := validateSubscriptionID(subscriptionID(object)); err != nil {
		return nil, err
	}
	var resp *aigridv1.SubscriptionsUpdateResponse
	err := gitops.RetryOnConflict(ctx, func() (err error) {
		resp, err = s.gitUpdate(ctx, proto.Clone(req).(*aigridv1.SubscriptionsUpdateRequest))
		return err
	})
	return resp, err
}

// Delete removes the subscription's file, and with it the managed AuthPolicy.
// A nonzero resource_version must equal the stored metadata.version, or the
// delete is Aborted; zero skips the check. A commit that conflicts reruns the
// whole delete on the new head, so the version is checked again.
func (s *SubscriptionsServer) Delete(ctx context.Context, req *aigridv1.SubscriptionsDeleteRequest) (*aigridv1.SubscriptionsDeleteResponse, error) {
	id := req.GetId()
	if err := validateSubscriptionID(id); err != nil {
		return nil, err
	}
	err := gitops.RetryOnConflict(ctx, func() error {
		return s.gitDelete(ctx, id, req.GetResourceVersion())
	})
	if err != nil {
		return nil, err
	}
	return &aigridv1.SubscriptionsDeleteResponse{}, nil
}

type subUpdatePaths struct {
	labels, annotations bool
	wholeSpec           bool
	owner               bool
	modelRefs           bool
	priority            bool
	organization        bool
	tokenMetadata       bool
	inferencePriority   bool
	guardrails          bool
}

func (p subUpdatePaths) anySpec() bool {
	return p.wholeSpec || p.owner || p.modelRefs || p.priority || p.organization ||
		p.tokenMetadata || p.inferencePriority || p.guardrails
}

// subscriptionUpdatePaths decides which fields an Update writes.
//
// An empty mask is inferred from the object (REST PATCH often arrives without
// one). Explicit masks must use supported paths; unsupported paths are rejected.
func subscriptionUpdatePaths(mask *fieldmaskpb.FieldMask, object *aigridv1.Subscription) (subUpdatePaths, error) {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return subUpdatePaths{
			labels:      len(object.GetMetadata().GetLabels()) > 0,
			annotations: len(object.GetMetadata().GetAnnotations()) > 0,
			wholeSpec:   object.GetSpec() != nil,
		}, nil
	}
	var paths subUpdatePaths
	for _, path := range mask.GetPaths() {
		p := strings.TrimPrefix(path, "object.")
		switch {
		case p == "id":
			// Identity is taken from the path / object.id; ignore in the mask.
		case p == "metadata":
			paths.labels, paths.annotations = true, true
		case p == "metadata.labels" || strings.HasPrefix(p, "metadata.labels."):
			paths.labels = true
		case p == "metadata.annotations" || strings.HasPrefix(p, "metadata.annotations."):
			paths.annotations = true
		case p == "spec":
			paths.wholeSpec = true
		case p == "spec.owner":
			paths.owner = true
		case strings.HasPrefix(p, "spec.owner."):
			// Nested owner paths would replace the whole owner (clearing the
			// sibling users/groups). Reject until nested merge is supported.
			return subUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "update_mask path %q is not supported; use spec.owner", path)
		case p == "spec.model_refs":
			paths.modelRefs = true
		case strings.HasPrefix(p, "spec.model_refs."):
			return subUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "update_mask path %q is not supported; use spec.model_refs", path)
		case p == "spec.priority":
			paths.priority = true
		case p == "spec.organization":
			paths.organization = true
		case strings.HasPrefix(p, "spec.organization."):
			return subUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "update_mask path %q is not supported; use spec.organization", path)
		case p == "spec.token_metadata":
			paths.tokenMetadata = true
		case strings.HasPrefix(p, "spec.token_metadata."):
			return subUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "update_mask path %q is not supported; use spec.token_metadata", path)
		case p == "spec.inference_priority":
			paths.inferencePriority = true
		case p == "spec.guardrails":
			paths.guardrails = true
		case strings.HasPrefix(p, "spec.guardrails."):
			return subUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "update_mask path %q is not supported; use spec.guardrails", path)
		default:
			return subUpdatePaths{}, connect.Errorf(connect.CodeInvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	return paths, nil
}

// mergeSubscriptionUpdate builds the candidate by applying only selected mask
// paths from the request onto the stored subscription.
func mergeSubscriptionUpdate(stored, incoming *aigridv1.Subscription, paths subUpdatePaths) *aigridv1.Subscription {
	out := proto.Clone(stored).(*aigridv1.Subscription)
	if out == nil {
		out = &aigridv1.Subscription{Id: subscriptionID(stored)}
	}
	if paths.labels || paths.annotations {
		meta := out.GetMetadata()
		if meta == nil {
			meta = &aigridv1.Metadata{}
		} else {
			meta = proto.Clone(meta).(*aigridv1.Metadata)
		}
		if paths.labels {
			if incoming.GetMetadata() != nil {
				meta.Labels = incoming.GetMetadata().GetLabels()
			} else {
				meta.Labels = nil
			}
		}
		if paths.annotations {
			if incoming.GetMetadata() != nil {
				meta.Annotations = incoming.GetMetadata().GetAnnotations()
			} else {
				meta.Annotations = nil
			}
		}
		out.Metadata = meta
	}
	if !paths.anySpec() {
		return out
	}
	if out.Spec == nil {
		out.Spec = &aigridv1.SubscriptionSpec{}
	} else {
		out.Spec = proto.Clone(out.Spec).(*aigridv1.SubscriptionSpec)
	}
	in := incoming.GetSpec()
	if paths.wholeSpec {
		if in == nil {
			out.Spec = &aigridv1.SubscriptionSpec{}
		} else {
			out.Spec = proto.Clone(in).(*aigridv1.SubscriptionSpec)
		}
		return out
	}
	if in == nil {
		in = &aigridv1.SubscriptionSpec{}
	}
	if paths.owner {
		out.Spec.Owner = in.GetOwner()
	}
	if paths.modelRefs {
		out.Spec.ModelRefs = in.GetModelRefs()
	}
	if paths.priority {
		out.Spec.Priority = in.GetPriority()
	}
	if paths.organization {
		out.Spec.Organization = in.GetOrganization()
	}
	if paths.tokenMetadata {
		out.Spec.TokenMetadata = in.GetTokenMetadata()
	}
	if paths.inferencePriority {
		out.Spec.InferencePriority = in.InferencePriority
	}
	if paths.guardrails {
		out.Spec.Guardrails = in.GetGuardrails()
	}
	return out
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
			seconds, ok := tokenWindowSeconds(lim.GetWindow())
			if !ok {
				return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].token_rate_limits[%d].window %q is invalid: want 1 to 9999 followed by s, m or h, at most %s", i, j, lim.GetWindow(), maxTokenWindow)
			}
			if seconds > maxTokenWindowSeconds {
				return connect.Errorf(connect.CodeInvalidArgument, "spec.model_refs[%d].token_rate_limits[%d].window %q is longer than the maximum, %s (366 days)", i, j, lim.GetWindow(), maxTokenWindow)
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
