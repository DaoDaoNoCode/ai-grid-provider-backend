// Package subscription maps ai_grid.draft.v1.Subscription and AuthPolicy to and
// from the BFF's JSON shapes. The BFF answers with it, and the fake backend
// validates the subscriptions it stores in it, so both share one mapping. This
// is a copy of the console's bff/internal/subscription (ac80c25) for the draft
// fake; the console keeps its own.
//
// The shapes are the PoC's MaaSSubscription, CreateSubscriptionRequest,
// UpdateSubscriptionRequest, MaaSAuthPolicy and SubscriptionInfoResponse
// (krisv/ai-grid-cloud-provider-poc src/app/types/subscriptions.ts at
// 62319b8), with resourceVersion for the PoC's revision. The proto keeps
// groups, subjects and guardrail references flat; the JSON has the PoC's
// {name} and {ref: {name}} objects and billingRate.perToken.
package subscription

import (
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Group is a group reference.
type Group struct {
	Name string `json:"name"`
}

// Owner is who may use a subscription.
type Owner struct {
	Users  []string `json:"users,omitempty"`
	Groups []Group  `json:"groups"`
}

// GuardrailRef names a guardrail.
type GuardrailRef struct {
	Name string `json:"name"`
}

// Guardrail is a guardrail attached to a subscription or a model.
type Guardrail struct {
	Ref    GuardrailRef `json:"ref"`
	Checks []string     `json:"checks,omitempty"`
}

// TokenRateLimit is at most Limit tokens per Window (`<n><s|m|h>`).
type TokenRateLimit struct {
	Limit  int64  `json:"limit"`
	Window string `json:"window"`
}

// BillingRate is the price per token, as a decimal string.
type BillingRate struct {
	PerToken string `json:"perToken"`
}

// ModelRef is a model in a subscription, the PoC's ModelSubscriptionRef.
type ModelRef struct {
	Name string `json:"name"`
	// Namespace is the model's namespace; empty is the console's models
	// namespace (the backend's default).
	Namespace       string           `json:"namespace,omitempty"`
	DisplayName     string           `json:"displayName,omitempty"`
	Description     string           `json:"description,omitempty"`
	Unlimited       bool             `json:"unlimited,omitempty"`
	TokenRateLimits []TokenRateLimit `json:"tokenRateLimits"`
	BillingRate     *BillingRate     `json:"billingRate,omitempty"`
	Guardrails      []Guardrail      `json:"guardrails,omitempty"`
}

// TokenMetadata is a subscription's metering metadata; OrganizationID is the
// organization (tenant id) it serves, set by the console's Organization field.
type TokenMetadata struct {
	OrganizationID string            `json:"organizationId,omitempty"`
	CostCenter     string            `json:"costCenter,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// Update is the body of PUT /api/v1/subscriptions/{name}, the PoC's
// UpdateSubscriptionRequest.
type Update struct {
	DisplayName       string         `json:"displayName,omitempty"`
	Description       string         `json:"description,omitempty"`
	Priority          int32          `json:"priority"`
	Owner             Owner          `json:"owner"`
	ModelRefs         []ModelRef     `json:"modelRefs"`
	TokenMetadata     *TokenMetadata `json:"tokenMetadata,omitempty"`
	InferencePriority *int32         `json:"inferencePriority,omitempty"`
	Guardrails        []Guardrail    `json:"guardrails,omitempty"`
}

// Input is the body of POST /api/v1/subscriptions, the PoC's
// CreateSubscriptionRequest: an Update with a name and createAuthPolicy.
type Input struct {
	Name string `json:"name,omitempty"`
	Update
	CreateAuthPolicy *bool `json:"createAuthPolicy,omitempty"`
}

// Subscription is a subscription as the API returns it, the PoC's MaaSSubscription.
type Subscription struct {
	Name              string         `json:"name"`
	DisplayName       string         `json:"displayName,omitempty"`
	Description       string         `json:"description,omitempty"`
	Priority          int32          `json:"priority"`
	Owner             Owner          `json:"owner"`
	ModelRefs         []ModelRef     `json:"modelRefs"`
	TokenMetadata     *TokenMetadata `json:"tokenMetadata,omitempty"`
	InferencePriority *int32         `json:"inferencePriority,omitempty"`
	Guardrails        []Guardrail    `json:"guardrails,omitempty"`
	// Phase is the MaaSSubscription's status.phase: Active, Degraded,
	// Failed or Invalid, and absent before the controller's first reconcile.
	Phase             string `json:"phase,omitempty"`
	ResourceVersion   string `json:"resourceVersion"`
	CreationTimestamp string `json:"creationTimestamp,omitempty"`
	DeletionTimestamp string `json:"deletionTimestamp,omitempty"`
	// Conditions, ModelRefStatuses and TokenRateLimitStatuses are the
	// MaaSSubscription's status lists, as the CRD has them (P12).
	Conditions             []Condition            `json:"conditions"`
	ModelRefStatuses       []ResourceRefStatus    `json:"modelRefStatuses"`
	TokenRateLimitStatuses []TokenRateLimitStatus `json:"tokenRateLimitStatuses"`
}

// Condition is a Kubernetes status condition; Status is True, False or Unknown.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// ResourceRefStatus is the MaaS ResourceRefStatus: the state of an object a
// MaaS resource references or generates.
type ResourceRefStatus struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Ready     bool   `json:"ready"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// TokenRateLimitStatus is a generated TokenRateLimitPolicy's status and the
// model it limits, flat as in the CRD.
type TokenRateLimitStatus struct {
	ResourceRefStatus
	Model string `json:"model"`
}

// ModelRegistration is a model's MaaSModelRef status (P12).
type ModelRegistration struct {
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace"`
	Phase      string      `json:"phase"`
	Endpoint   string      `json:"endpoint,omitempty"`
	Conditions []Condition `json:"conditions"`
}

// PolicyModelRef is a model an authorization policy covers; an empty
// Namespace is the console's models namespace.
type PolicyModelRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Subjects are the groups an authorization policy admits.
type Subjects struct {
	Groups []Group `json:"groups"`
}

// AuthPolicy is the PoC's MaaSAuthPolicy.
type AuthPolicy struct {
	Name        string           `json:"name"`
	DisplayName string           `json:"displayName,omitempty"`
	Description string           `json:"description,omitempty"`
	Phase       string           `json:"phase,omitempty"`
	ModelRefs   []PolicyModelRef `json:"modelRefs"`
	Subjects    Subjects         `json:"subjects"`
	Conditions  []Condition      `json:"conditions"`
}

// Info is GET /api/v1/subscriptions/{name}, the PoC's SubscriptionInfoResponse
// without its modelRefs, which the BFF adds in its own Model JSON.
type Info struct {
	Subscription     Subscription   `json:"subscription"`
	AuthPolicies     []AuthPolicy   `json:"authPolicies"`
	MeteringMetadata *TokenMetadata `json:"meteringMetadata,omitempty"`
	// ModelRegistrations are the MaaSModelRef statuses of the referenced
	// models that exist (P12).
	ModelRegistrations []ModelRegistration `json:"modelRegistrations"`
}

// Mutation is what POST and PUT return, the PoC's CreateSubscriptionResponse.
type Mutation struct {
	Subscription Subscription `json:"subscription"`
	AuthPolicy   *AuthPolicy  `json:"authPolicy,omitempty"`
}

// Normalized is the input as the PoC's zod schemas output it: the display
// name and group names trimmed, the rest untouched. The fake backend stores
// the normalized input, so what is validated (the validator trims the same
// way) is what is stored.
func (in Input) Normalized() Input {
	out := in
	out.DisplayName = trim(in.DisplayName)
	out.Owner.Groups = make([]Group, len(in.Owner.Groups))
	for i, g := range in.Owner.Groups {
		out.Owner.Groups[i] = Group{Name: trim(g.Name)}
	}
	return out
}

// trim is zod's trim, String.prototype.trim.
func trim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF })
}

// Subscription is the proto for an input; the backend sets the status
// fields, the version and the times.
func (in Input) Subscription() *aigriddraftv1.Subscription {
	out := &aigriddraftv1.Subscription{
		Name:              in.Name,
		DisplayName:       in.DisplayName,
		Description:       in.Description,
		Priority:          in.Priority,
		Owner:             &aigriddraftv1.Owner{Users: in.Owner.Users, Groups: groupNames(in.Owner.Groups)},
		InferencePriority: in.InferencePriority,
		Guardrails:        guardrailsToProto(in.Guardrails),
	}
	for _, r := range in.ModelRefs {
		ref := &aigriddraftv1.ModelSubscriptionRef{
			Name:        r.Name,
			Namespace:   r.Namespace,
			DisplayName: r.DisplayName,
			Description: r.Description,
			Unlimited:   r.Unlimited,
			Guardrails:  guardrailsToProto(r.Guardrails),
		}
		for _, l := range r.TokenRateLimits {
			ref.TokenRateLimits = append(ref.TokenRateLimits, &aigriddraftv1.TokenRateLimit{Limit: l.Limit, Window: l.Window})
		}
		if r.BillingRate != nil {
			rate := r.BillingRate.PerToken
			ref.BillingRatePerToken = &rate
		}
		out.ModelRefs = append(out.ModelRefs, ref)
	}
	if m := in.TokenMetadata; m != nil {
		out.TokenMetadata = &aigriddraftv1.TokenMetadata{OrganizationId: m.OrganizationID, CostCenter: m.CostCenter, Labels: m.Labels}
	}
	return out
}

// InputFromSubscription is the part of a subscription a client sends, for
// the fake backend to validate with the BFF's rules. createAuthPolicy is set
// on create only.
func InputFromSubscription(s *aigriddraftv1.Subscription, createAuthPolicy *bool) Input {
	out := FromSubscription(s)
	return Input{
		Name: out.Name,
		Update: Update{
			DisplayName:       out.DisplayName,
			Description:       out.Description,
			Priority:          out.Priority,
			Owner:             out.Owner,
			ModelRefs:         out.ModelRefs,
			TokenMetadata:     out.TokenMetadata,
			InferencePriority: out.InferencePriority,
			Guardrails:        out.Guardrails,
		},
		CreateAuthPolicy: createAuthPolicy,
	}
}

// FromSubscription is the JSON shape of a subscription.
func FromSubscription(s *aigriddraftv1.Subscription) Subscription {
	if s == nil {
		s = &aigriddraftv1.Subscription{}
	}
	out := Subscription{
		Name:                   s.GetName(),
		DisplayName:            s.GetDisplayName(),
		Description:            s.GetDescription(),
		Priority:               s.GetPriority(),
		Owner:                  Owner{Users: s.GetOwner().GetUsers(), Groups: groups(s.GetOwner().GetGroups())},
		ModelRefs:              []ModelRef{},
		InferencePriority:      s.InferencePriority,
		Guardrails:             guardrailsFromProto(s.GetGuardrails()),
		Phase:                  s.GetPhase(),
		ResourceVersion:        s.GetResourceVersion(),
		CreationTimestamp:      timestamp(s.GetCreateTime()),
		DeletionTimestamp:      timestamp(s.GetDeletionTime()),
		Conditions:             conditions(s.GetConditions()),
		ModelRefStatuses:       []ResourceRefStatus{},
		TokenRateLimitStatuses: []TokenRateLimitStatus{},
	}
	for _, r := range s.GetModelRefStatuses() {
		out.ModelRefStatuses = append(out.ModelRefStatuses, refStatus(r))
	}
	for _, t := range s.GetTokenRateLimitStatuses() {
		out.TokenRateLimitStatuses = append(out.TokenRateLimitStatuses, TokenRateLimitStatus{ResourceRefStatus: refStatus(t.GetRef()), Model: t.GetModel()})
	}
	for _, r := range s.GetModelRefs() {
		ref := ModelRef{
			Name:            r.GetName(),
			Namespace:       r.GetNamespace(),
			DisplayName:     r.GetDisplayName(),
			Description:     r.GetDescription(),
			Unlimited:       r.GetUnlimited(),
			TokenRateLimits: []TokenRateLimit{},
			Guardrails:      guardrailsFromProto(r.GetGuardrails()),
		}
		for _, l := range r.GetTokenRateLimits() {
			ref.TokenRateLimits = append(ref.TokenRateLimits, TokenRateLimit{Limit: l.GetLimit(), Window: l.GetWindow()})
		}
		if r.BillingRatePerToken != nil {
			ref.BillingRate = &BillingRate{PerToken: r.GetBillingRatePerToken()}
		}
		out.ModelRefs = append(out.ModelRefs, ref)
	}
	if m := s.GetTokenMetadata(); m != nil {
		out.TokenMetadata = &TokenMetadata{OrganizationID: m.GetOrganizationId(), CostCenter: m.GetCostCenter(), Labels: m.GetLabels()}
	}
	return out
}

// FromAuthPolicy is the JSON shape of an authorization policy.
func FromAuthPolicy(p *aigriddraftv1.AuthPolicy) AuthPolicy {
	out := AuthPolicy{
		Name:        p.GetName(),
		DisplayName: p.GetDisplayName(),
		Description: p.GetDescription(),
		Phase:       p.GetPhase(),
		ModelRefs:   []PolicyModelRef{},
		Subjects:    Subjects{Groups: groups(p.GetSubjectGroups())},
		Conditions:  conditions(p.GetConditions()),
	}
	for _, m := range p.GetModelRefs() {
		out.ModelRefs = append(out.ModelRefs, PolicyModelRef{Name: m.GetName(), Namespace: m.GetNamespace()})
	}
	return out
}

// FromGetResponse is the JSON shape of GetSubscription's answer, except its
// model_refs.
func FromGetResponse(resp *aigriddraftv1.GetSubscriptionResponse) Info {
	out := Info{
		Subscription:       FromSubscription(resp.GetSubscription()),
		AuthPolicies:       []AuthPolicy{},
		ModelRegistrations: []ModelRegistration{},
	}
	for _, r := range resp.GetModelRegistrations() {
		out.ModelRegistrations = append(out.ModelRegistrations, ModelRegistration{
			Name: r.GetName(), Namespace: r.GetNamespace(), Phase: r.GetPhase(), Endpoint: r.GetEndpoint(), Conditions: conditions(r.GetConditions()),
		})
	}
	out.MeteringMetadata = out.Subscription.TokenMetadata
	for _, p := range resp.GetAuthPolicies() {
		out.AuthPolicies = append(out.AuthPolicies, FromAuthPolicy(p))
	}
	return out
}

// conditionStatus is the Kubernetes word for a condition status.
var conditionStatus = map[aigriddraftv1.ConditionStatus]string{
	aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE:        "True",
	aigriddraftv1.ConditionStatus_CONDITION_STATUS_FALSE:       "False",
	aigriddraftv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED: "Unknown",
}

func conditions(in []*aigriddraftv1.Condition) []Condition {
	out := make([]Condition, 0, len(in))
	for _, c := range in {
		out = append(out, Condition{
			Type: c.GetType(), Status: conditionStatus[c.GetStatus()], Reason: c.GetReason(), Message: c.GetMessage(),
			LastTransitionTime: timestamp(c.GetLastTransitionTime()),
		})
	}
	return out
}

func refStatus(r *aigriddraftv1.ResourceRefStatus) ResourceRefStatus {
	return ResourceRefStatus{Name: r.GetName(), Namespace: r.GetNamespace(), Ready: r.GetReady(), Reason: r.GetReason(), Message: r.GetMessage()}
}

func groupNames(in []Group) []string {
	out := make([]string, 0, len(in))
	for _, g := range in {
		out = append(out, g.Name)
	}
	return out
}

func groups(names []string) []Group {
	out := make([]Group, 0, len(names))
	for _, n := range names {
		out = append(out, Group{Name: n})
	}
	return out
}

func guardrailsToProto(in []Guardrail) []*aigriddraftv1.GuardrailAttachment {
	var out []*aigriddraftv1.GuardrailAttachment
	for _, g := range in {
		out = append(out, &aigriddraftv1.GuardrailAttachment{RefName: g.Ref.Name, Checks: g.Checks})
	}
	return out
}

func guardrailsFromProto(in []*aigriddraftv1.GuardrailAttachment) []Guardrail {
	var out []Guardrail
	for _, g := range in {
		out = append(out, Guardrail{Ref: GuardrailRef{Name: g.GetRefName()}, Checks: g.GetChecks()})
	}
	return out
}

func timestamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}
