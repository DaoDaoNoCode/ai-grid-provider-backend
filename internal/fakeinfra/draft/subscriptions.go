package draft

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"

	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/draft/subscription"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/draft/validation"
)

// The PoC's subscription messages (MockSubscriptionService and backend/api.mjs at 62319b8).
const (
	msgSubscriptionTaken    = "A subscription with this resource name already exists."
	msgSubscriptionStale    = "This subscription changed. Reload it before saving or deleting."
	msgSubscriptionDeleting = "This subscription is being deleted."
	msgPolicyTaken          = "An independently managed authorization policy already uses this name. Choose another subscription name."
	msgPolicyNameTooLong    = "The resource name must be at most 246 characters when creating a matching authorization policy."
	msgPolicyNameField      = "Leave room for the -policy suffix."
)

// DefaultModelNamespace is the namespace of a subscription's model reference
// that names none (P6): the console's models namespace.
const DefaultModelNamespace = "ai-grid-models"

// refKey is the model store key of a subscription's model reference.
func refKey(r *aigriddraftv1.ModelSubscriptionRef) string {
	ns := r.GetNamespace()
	if ns == "" {
		ns = DefaultModelNamespace
	}
	return modelKey(ns, r.GetName())
}

// policyKey is the model store key of an auth policy's model reference; a
// reference without a namespace is to the console's models namespace, like
// a subscription's.
func policyKey(r *aigriddraftv1.AuthPolicyModelRef) string {
	ns := r.GetNamespace()
	if ns == "" {
		ns = DefaultModelNamespace
	}
	return modelKey(ns, r.GetName())
}

// subscriptionRefers and policyRefers say whether a subscription or a
// policy references the model with the store key.
func subscriptionRefers(sub *aigriddraftv1.Subscription, key string) bool {
	return slices.ContainsFunc(sub.GetModelRefs(), func(r *aigriddraftv1.ModelSubscriptionRef) bool { return refKey(r) == key })
}

func policyRefers(p *aigriddraftv1.AuthPolicy, key string) bool {
	return slices.ContainsFunc(p.GetModelRefs(), func(m *aigriddraftv1.AuthPolicyModelRef) bool { return policyKey(m) == key })
}

// maasReferencing is DeleteModel's in-use check (SetReferencedBy): the auth
// policies and subscriptions that reference namespace/name, sorted. The
// caller holds s.mu.
func (s *Server) maasReferencing(namespace, name string) []string {
	key := modelKey(namespace, name)
	var out []string
	for _, p := range s.authPolicies {
		if policyRefers(p, key) {
			out = append(out, "auth policy "+p.GetName())
		}
	}
	for _, sub := range s.subscriptions {
		if subscriptionRefers(sub, key) {
			out = append(out, "subscription "+sub.GetName())
		}
	}
	sort.Strings(out)
	return out
}

// Stage offsets of the MaaS controller, in scenario seconds (divided by the
// settings' `speed`), from a subscription's or policy's creation: the first
// reconcile, and Kuadrant accepting the generated TokenRateLimitPolicies.
// The controller reads only their Accepted condition
// (getTRLPAcceptedCondition), never Enforced, so acceptance is what ends
// Degraded.
const (
	subscriptionReconcile = 1 * time.Second
	trlpAccepted          = 10 * time.Second
)

// MaaS phases, condition types, reasons and messages, verbatim from
// docs/draft/research/models-maas.md 3.4-3.6 (maas-controller's
// maassubscription_controller.go deriveFinalPhase, maasmodelref_controller.go
// deriveModelPhase, maasauthpolicy_controller.go deriveAuthPolicyPhase,
// conflict_detection.go; checked against models-as-a-service d2c4fc1).
const (
	phasePending   = "Pending"
	phaseActive    = "Active"
	phaseDegraded  = "Degraded"
	phaseFailed    = "Failed"
	phaseReady     = "Ready"
	phaseUnhealthy = "Unhealthy"

	condReady              = "Ready"
	condConflictingPolicy  = "ConflictingAuthPolicy"
	condGovernanceAttached = "GovernanceAttached"
	condRuntimeReady       = "RuntimeReady"

	msgReconciled       = "successfully reconciled"
	msgRefsInvalidFm    = "%d of %d model references are invalid or unavailable"
	msgAllRefsInvalidFm = "all %d model references are invalid or unavailable"
	msgTRLPsPendingFm   = "%d of %d TokenRateLimitPolicies not accepted"
	msgRefNotFoundFm    = "MaaSModelRef %s/%s not found"
	// msgTRLPNoRouteFm is checkTokenRateLimitHealth's message for a missing
	// model (findHTTPRouteForModel's ErrModelNotFound, providers.go:120-128).
	msgTRLPNoRouteFm = "HTTPRoute not found yet; TokenRateLimitPolicy cannot be created: MaaSModelRef not found: %s/%s"
	msgNoConflict    = "No conflicting AuthPolicies detected on MaaS-managed HTTPRoutes"
	// deriveAuthPolicyPhase (maasauthpolicy_controller.go:710-743).
	msgPolicyAllRefsMissingFm = "all %d model references are invalid or missing"
	msgPolicyRefsMissingFm    = "%d of %d model references are missing"
	msgPolicyCreatedFm        = "Auth policy created for subscription %q"

	msgGovernancePaired = "Active governance pairing found"
	msgNoPairingFound   = "No active subscription and auth policy pairing found"
	msgRuntimeHealthy   = "Backend is healthy"
	msgBackendNotReady  = "Backend is not ready"
	msgModelReady       = "Governed and runtime-healthy"
	msgModelUnhealthy   = "Governed but backend is not ready"
	// msgGatewayPolicyWait is maas-controller's Pending message for an auth
	// policy (maasauthpolicy_controller.go:628) on the default gateway, with
	// gatewayAuthPolicyReady's detail when Kuadrant gave no message (L1377).
	msgGatewayPolicyWait = "Waiting for gateway AuthPolicy openshift-ingress/maas-gateway-auth to be accepted and enforced: Accepted and Enforced conditions are not both True"
	msgAwaitingPairing   = "Awaiting governance pairing"
	msgAwaitingPairingRt = "Awaiting governance pairing and backend readiness"
)

// trlpPrefix names the TokenRateLimitPolicy maas-controller generates per model.
const trlpPrefix = "maas-trlp-"

// maasCondition is a condition whose last transition is at scenario time at
// (zero: unknown, left unset).
func (s *Server) maasCondition(typ string, st aigriddraftv1.ConditionStatus, reason, message string, at time.Time) *aigriddraftv1.Condition {
	c := &aigriddraftv1.Condition{Type: typ, Status: st, Reason: proto.String(reason), Message: proto.String(message)}
	if !at.IsZero() {
		c.LastTransitionTime = timestamppb.New(s.wallTime(at))
	}
	return c
}

// subscriptionStatus fills a subscription's phase, Ready condition and
// status lists at scenario time now, as maas-controller's deriveFinalPhase
// leaves them: no status at all until the first reconcile (status.phase has
// no default and the controller never writes Pending); then Failed when
// every model reference is missing, Degraded when some are, Degraded while
// a generated TokenRateLimitPolicy isn't Accepted, and Active. As in
// checkTokenRateLimitHealth, every reference, unlimited or missing, has a
// TokenRateLimitPolicy status: a missing model's is BackendNotReady. (Upstream
// also counts a reference once by namespace and name; the update rules
// already refuse two references to one model name.) The
// timer runs from creation; an update doesn't restart it. The caller holds
// s.mu.
func (s *Server) subscriptionStatus(sub *aigriddraftv1.Subscription, now time.Time) {
	created := s.scenarioTime(sub.GetCreateTime().AsTime())
	sub.ModelRefStatuses, sub.TokenRateLimitStatuses = nil, nil
	if now.Sub(created) < subscriptionReconcile {
		sub.Phase, sub.Conditions = "", nil
		return
	}
	reconciled := created.Add(subscriptionReconcile)
	accepted := now.Sub(created) >= trlpAccepted
	missing := 0
	for _, r := range sub.GetModelRefs() {
		ns, key := r.GetNamespace(), refKey(r)
		trlp := &aigriddraftv1.ResourceRefStatus{Name: trlpPrefix + r.GetName(), Namespace: ns, Ready: true, Reason: "Accepted"}
		if _, ok := s.models[key]; !ok {
			missing++
			msg := fmt.Sprintf(msgRefNotFoundFm, ns, r.GetName())
			sub.ModelRefStatuses = append(sub.ModelRefStatuses, &aigriddraftv1.ResourceRefStatus{Name: r.GetName(), Namespace: ns, Reason: "NotFound", Message: msg})
			trlp.Ready, trlp.Reason, trlp.Message = false, "BackendNotReady", fmt.Sprintf(msgTRLPNoRouteFm, ns, r.GetName())
		} else {
			sub.ModelRefStatuses = append(sub.ModelRefStatuses, &aigriddraftv1.ResourceRefStatus{Name: r.GetName(), Namespace: ns, Ready: true, Reason: "Valid"})
			if !accepted {
				trlp.Ready, trlp.Reason = false, "NotAccepted"
			}
		}
		sub.TokenRateLimitStatuses = append(sub.TokenRateLimitStatuses, &aigriddraftv1.TokenRateLimitStatus{Ref: trlp, Model: r.GetName()})
	}
	n := len(sub.GetModelRefs())
	switch {
	case missing == n:
		sub.Phase = phaseFailed
		sub.Conditions = []*aigriddraftv1.Condition{s.maasCondition(condReady, cFalse, "ReconcileFailed", fmt.Sprintf(msgAllRefsInvalidFm, n), reconciled)}
	case missing > 0:
		sub.Phase = phaseDegraded
		sub.Conditions = []*aigriddraftv1.Condition{s.maasCondition(condReady, cFalse, "PartialFailure", fmt.Sprintf(msgRefsInvalidFm, missing, n), reconciled)}
	case !accepted:
		sub.Phase = phaseDegraded
		sub.Conditions = []*aigriddraftv1.Condition{s.maasCondition(condReady, cFalse, "PartialFailure", fmt.Sprintf(msgTRLPsPendingFm, n, n), reconciled)}
	default:
		sub.Phase = phaseActive
		sub.Conditions = []*aigriddraftv1.Condition{s.maasCondition(condReady, cTrue, "Reconciled", msgReconciled, created.Add(trlpAccepted))}
	}
}

// viewSubscription is a stored subscription with its status at scenario time
// now, each model reference with its namespace (the default when it names none).
func (s *Server) viewSubscription(sub *aigriddraftv1.Subscription, now time.Time) *aigriddraftv1.Subscription {
	out := withDefaultNamespaces(sub)
	s.subscriptionStatus(out, now)
	return out
}

// withDefaultNamespaces is a copy of sub with each model reference's namespace filled in.
func withDefaultNamespaces(sub *aigriddraftv1.Subscription) *aigriddraftv1.Subscription {
	out := proto.Clone(sub).(*aigriddraftv1.Subscription)
	for _, r := range out.GetModelRefs() {
		if r.GetNamespace() == "" {
			r.Namespace = DefaultModelNamespace
		}
	}
	return out
}

// viewPolicy is an auth policy with its status at scenario time now, each model
// reference with its namespace (the default when it names none), as
// maas-controller's deriveAuthPolicyPhase leaves it: Pending, Ready Unknown,
// until the first reconcile after it was created (reconcilePending); then
// Failed when every model reference is missing, Degraded when some are, and
// Active, each with ConflictingAuthPolicy. The fake's group AuthPolicies are
// accepted at the first reconcile, so it never derives their Degraded. A
// fixture policy existed before the fake started, so it is past Pending.
func (s *Server) viewPolicy(p *aigriddraftv1.AuthPolicy, now time.Time) *aigriddraftv1.AuthPolicy {
	out := proto.Clone(p).(*aigriddraftv1.AuthPolicy)
	for _, r := range out.GetModelRefs() {
		if r.GetNamespace() == "" {
			r.Namespace = DefaultModelNamespace
		}
	}
	var created time.Time
	if wall, ok := s.policyCreated[p.GetName()]; ok {
		created = s.scenarioTime(wall)
	}
	if !created.IsZero() && now.Sub(created) < subscriptionReconcile {
		out.Phase = phasePending
		out.Conditions = []*aigriddraftv1.Condition{s.maasCondition(condReady, cUnknown, "Unknown", msgGatewayPolicyWait, created)}
		return out
	}
	var since time.Time
	if !created.IsZero() {
		since = created.Add(subscriptionReconcile)
	}
	n, missing := len(p.GetModelRefs()), 0
	for _, m := range p.GetModelRefs() {
		if s.models[policyKey(m)] == nil {
			missing++
		}
	}
	out.Phase = phaseActive
	ready := s.maasCondition(condReady, cTrue, "Reconciled", msgReconciled, since)
	switch {
	case missing == n:
		out.Phase, ready = phaseFailed, s.maasCondition(condReady, cFalse, "ReconcileFailed", fmt.Sprintf(msgPolicyAllRefsMissingFm, n), since)
	case missing > 0:
		out.Phase, ready = phaseDegraded, s.maasCondition(condReady, cFalse, "PartialFailure", fmt.Sprintf(msgPolicyRefsMissingFm, missing, n), since)
	}
	out.Conditions = []*aigriddraftv1.Condition{ready, s.maasCondition(condConflictingPolicy, cFalse, "NoConflict", msgNoConflict, since)}
	return out
}

// modelRegistration is the status of the MaaSModelRef that registers a
// model: every model counts as registered under its own namespace and name.
// It is derived as deriveModelPhase does: governed when at least one
// subscription and one auth policy reference the model, whatever their
// phase; runtime-ready when the model's phase is Ready. Ready is False with
// BackendNotReady in every phase but Ready (updateStatusWithReason), and
// RuntimeReady False with RuntimeHealthFailure, the default for an
// LLMInferenceService (markRuntimeNotReady). The fake keeps no
// pairing history, so it never reports GovernanceGap. The caller holds s.mu.
func (s *Server) modelRegistration(m *aigriddraftv1.Model) *aigriddraftv1.ModelRegistration {
	ns := m.GetMetadata().GetNamespace()
	key := modelKey(ns, m.GetName())
	subscribed := slices.ContainsFunc(slices.Collect(maps.Values(s.subscriptions)), func(sub *aigriddraftv1.Subscription) bool { return subscriptionRefers(sub, key) })
	allowed := slices.ContainsFunc(slices.Collect(maps.Values(s.authPolicies)), func(p *aigriddraftv1.AuthPolicy) bool { return policyRefers(p, key) })
	governed := subscribed && allowed
	ready := m.GetStatus().GetPhase() == aigriddraftv1.ModelPhase_MODEL_PHASE_READY
	var zero time.Time
	out := &aigriddraftv1.ModelRegistration{Name: m.GetName(), Namespace: ns}
	var readyCond *aigriddraftv1.Condition
	switch {
	case governed && ready:
		out.Phase, out.Endpoint = phaseReady, m.GetStatus().GetUrl()
		readyCond = s.maasCondition(condReady, cTrue, "Reconciled", msgModelReady, zero)
	case governed:
		out.Phase = phaseUnhealthy
		readyCond = s.maasCondition(condReady, cFalse, "BackendNotReady", msgModelUnhealthy, zero)
	case ready:
		out.Phase = phasePending
		readyCond = s.maasCondition(condReady, cFalse, "BackendNotReady", msgAwaitingPairing, zero)
	default:
		out.Phase = phasePending
		readyCond = s.maasCondition(condReady, cFalse, "BackendNotReady", msgAwaitingPairingRt, zero)
	}
	governance := s.maasCondition(condGovernanceAttached, cFalse, "NoPairingFound", msgNoPairingFound, zero)
	if governed {
		governance = s.maasCondition(condGovernanceAttached, cTrue, "GovernancePaired", msgGovernancePaired, zero)
	}
	runtime := s.maasCondition(condRuntimeReady, cFalse, "RuntimeHealthFailure", msgBackendNotReady, zero)
	if ready {
		runtime = s.maasCondition(condRuntimeReady, cTrue, "RuntimeHealthy", msgRuntimeHealthy, zero)
	}
	out.Conditions = []*aigriddraftv1.Condition{readyCond, governance, runtime}
	return out
}

// policySuffix names the authorization policy CreateSubscription pairs with
// a subscription; a resource name is at most 253 characters, so a
// subscription that gets one is at most 246.
const policySuffix = "-policy"

// ListSubscriptions implements Subscriptions, sorted by name.
func (s *Server) ListSubscriptions(ctx context.Context, _ *aigriddraftv1.ListSubscriptionsRequest) (*aigriddraftv1.ListSubscriptionsResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.advance()
	out := make([]*aigriddraftv1.Subscription, 0, len(s.subscriptions))
	for _, sub := range s.subscriptions {
		out = append(out, s.viewSubscription(sub, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &aigriddraftv1.ListSubscriptionsResponse{Subscriptions: out}, nil
}

// GetSubscription implements Subscriptions: the subscription, the models it
// references, the policies that cover any of them, as the PoC's getInfo, and
// the referenced models' registrations (P12).
func (s *Server) GetSubscription(ctx context.Context, req *aigriddraftv1.GetSubscriptionRequest) (*aigriddraftv1.GetSubscriptionResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.advance()
	sub, ok := s.subscriptions[req.GetName()]
	if !ok {
		return nil, notFound(req.GetName())
	}
	out := &aigriddraftv1.GetSubscriptionResponse{Subscription: s.viewSubscription(sub, now)}
	// The models come from the model store as GetModel answers them, by
	// namespace and name; a reference to a missing model has none.
	for _, r := range sub.GetModelRefs() {
		if m, ok := s.models[refKey(r)]; ok {
			out.ModelRefs = append(out.ModelRefs, s.viewModel(m, now))
		}
	}
	// A policy covers the subscription when it references one of its models
	// by namespace and name (policyKey).
	for _, p := range s.authPolicies {
		if slices.ContainsFunc(p.GetModelRefs(), func(m *aigriddraftv1.AuthPolicyModelRef) bool { return subscriptionRefers(sub, policyKey(m)) }) {
			out.AuthPolicies = append(out.AuthPolicies, s.viewPolicy(p, now))
		}
	}
	sort.Slice(out.ModelRefs, func(i, j int) bool {
		return modelKey(out.ModelRefs[i].GetMetadata().GetNamespace(), out.ModelRefs[i].GetName()) <
			modelKey(out.ModelRefs[j].GetMetadata().GetNamespace(), out.ModelRefs[j].GetName())
	})
	for _, m := range out.ModelRefs {
		out.ModelRegistrations = append(out.ModelRegistrations, s.modelRegistration(m))
	}
	sort.Slice(out.AuthPolicies, func(i, j int) bool { return out.AuthPolicies[i].GetName() < out.AuthPolicies[j].GetName() })
	return out, nil
}

// CreateSubscription implements Subscriptions: it validates the subscription
// with the PoC's rules and the CRD's messages and saves it normalized as the
// PoC's zod schemas output it (the display name and group names trimmed), at
// version 1, and answers it without status, as it is before the first
// reconcile. With create_auth_policy it also saves <name>-policy for the
// same groups and models, described as the hub describes it, and records
// the policy as its own.
func (s *Server) CreateSubscription(ctx context.Context, req *aigriddraftv1.CreateSubscriptionRequest) (*aigriddraftv1.CreateSubscriptionResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "subscriptions"); err != nil {
		return nil, err
	}
	withPolicy := req.GetCreateAuthPolicy()
	in := subscription.InputFromSubscription(req.GetObject(), &withPolicy)
	if err := validateSubscription(in, validation.CreateSubscription); err != nil {
		return nil, err
	}
	if withPolicy && len(in.Name) > 253-len(policySuffix) {
		return nil, invalid(msgPolicyNameTooLong, map[string]string{"name": msgPolicyNameField})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.subscriptions[in.Name]; taken {
		return nil, status.Error(codes.AlreadyExists, msgSubscriptionTaken)
	}
	policyName := in.Name + policySuffix
	if _, exists := s.authPolicies[policyName]; withPolicy && exists && !s.managedPolicies[policyName] {
		return nil, status.Error(codes.AlreadyExists, msgPolicyTaken)
	}
	now := s.advance()
	created := s.clock()
	sub := in.Normalized().Subscription()
	sub.ResourceVersion, sub.CreateTime = "1", timestamppb.New(created)
	s.subscriptions[sub.GetName()] = sub
	resp := &aigriddraftv1.CreateSubscriptionResponse{Subscription: s.viewSubscription(sub, now)}
	if withPolicy {
		p := mirrorPolicy(sub)
		s.authPolicies[policyName] = p
		s.managedPolicies[policyName] = true
		s.policyCreated[policyName] = created
		resp.AuthPolicy = s.viewPolicy(p, now)
	}
	return resp, nil
}

// UpdateSubscription implements Subscriptions. With lock a stale
// resource_version is Aborted, and a subscription being deleted is
// FailedPrecondition. The sent subscription, normalized as on create,
// replaces the stored one, as a full update mask does for clusters,
// organizations and models: a field sent empty is cleared. The name, status,
// version and times stay the backend's, and an update that changes nothing
// keeps the stored version. The <name>-policy create made follows the edit
// (one policy per subscription, Cloud Provider Working Group, 2026-10-16);
// other policies are never touched.
func (s *Server) UpdateSubscription(ctx context.Context, req *aigriddraftv1.UpdateSubscriptionRequest) (*aigriddraftv1.UpdateSubscriptionResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "subscriptions"); err != nil {
		return nil, err
	}
	obj := req.GetObject()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.subscriptions[obj.GetName()]
	if !ok {
		return nil, notFound(obj.GetName())
	}
	if req.GetLock() && obj.GetResourceVersion() != current.GetResourceVersion() {
		return nil, status.Error(codes.Aborted, msgSubscriptionStale)
	}
	if current.GetDeletionTime() != nil {
		return nil, status.Error(codes.FailedPrecondition, msgSubscriptionDeleting)
	}
	in := subscription.InputFromSubscription(obj, nil)
	in.Name = ""
	if err := validateSubscription(in, validation.UpdateSubscription); err != nil {
		return nil, err
	}
	next := in.Normalized().Subscription()
	next.Name, next.ResourceVersion, next.CreateTime = current.GetName(), current.GetResourceVersion(), current.GetCreateTime()
	if !proto.Equal(withDefaultNamespaces(next), withDefaultNamespaces(current)) {
		next.ResourceVersion = nextVersion(current.GetResourceVersion())
		s.subscriptions[next.GetName()] = next
		if policyName := next.GetName() + policySuffix; s.managedPolicies[policyName] {
			s.authPolicies[policyName] = mirrorPolicy(next)
		}
	}
	return &aigriddraftv1.UpdateSubscriptionResponse{Subscription: s.viewSubscription(s.subscriptions[next.GetName()], s.advance())}, nil
}

// mirrorPolicy is the <name>-policy CreateSubscription pairs with sub: its
// display name, groups and models, described as the hub describes it.
func mirrorPolicy(sub *aigriddraftv1.Subscription) *aigriddraftv1.AuthPolicy {
	p := &aigriddraftv1.AuthPolicy{
		Name:          sub.GetName() + policySuffix,
		DisplayName:   sub.GetDisplayName(),
		Description:   fmt.Sprintf(msgPolicyCreatedFm, sub.GetName()),
		SubjectGroups: slices.Clone(sub.GetOwner().GetGroups()),
	}
	for _, r := range sub.GetModelRefs() {
		p.ModelRefs = append(p.ModelRefs, &aigriddraftv1.AuthPolicyModelRef{Name: r.GetName(), Namespace: r.GetNamespace()})
	}
	return p
}

// DeleteSubscription implements Subscriptions. With resource_version set, a
// stale one is Aborted; a subscription being deleted is FailedPrecondition.
// Its <name>-policy goes too, but only when CreateSubscription made it.
func (s *Server) DeleteSubscription(ctx context.Context, req *aigriddraftv1.DeleteSubscriptionRequest) (*aigriddraftv1.DeleteSubscriptionResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProviderAdmin(ctx, token, "subscriptions"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.subscriptions[req.GetName()]
	if !ok {
		return nil, notFound(req.GetName())
	}
	if v := req.GetResourceVersion(); v != "" && v != current.GetResourceVersion() {
		return nil, status.Error(codes.Aborted, msgSubscriptionStale)
	}
	if current.GetDeletionTime() != nil {
		return nil, status.Error(codes.FailedPrecondition, msgSubscriptionDeleting)
	}
	delete(s.subscriptions, req.GetName())
	delete(s.liveSubscriptions, req.GetName())
	if policyName := req.GetName() + policySuffix; s.managedPolicies[policyName] {
		delete(s.authPolicies, policyName)
		delete(s.managedPolicies, policyName)
		delete(s.policyCreated, policyName)
	}
	return &aigriddraftv1.DeleteSubscriptionResponse{}, nil
}

// validateSubscription checks a model reference's namespace (P6, which the
// PoC's models don't have) as a namespace name, and runs one of the PoC's
// subscription rules, which take it as any string. The rule runs on the
// subscription in the API's JSON shape, so the fake rejects what the UI
// rejects, with the same messages. The rules trim as zod does, so they judge
// the normalized value; they run on the input as sent so that a display name
// of only spaces is still present and gets zod's message.
func validateSubscription(in subscription.Input, rule func(json.RawMessage) map[string]string) error {
	fields := map[string]string{}
	for i, r := range in.ModelRefs {
		if ns := r.Namespace; ns != "" && (len(ns) > maxDNSLabelLength || !dns1123Label.MatchString(ns)) {
			fields["modelRefs."+strconv.Itoa(i)+".namespace"] = fmt.Sprintf(MsgDNS1123LabelFm, "namespace", ns)
		}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal subscription: %v", err)
	}
	maps.Copy(fields, rule(raw))
	if len(fields) > 0 {
		return invalid(joinMessages(fields), fields)
	}
	return nil
}

// nextVersion is a subscription's next resource_version.
func nextVersion(v string) string {
	n, _ := strconv.Atoi(v)
	return strconv.Itoa(n + 1)
}
