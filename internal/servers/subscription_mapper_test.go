package servers

import (
	"testing"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSubscriptionRoundTrip(t *testing.T) {
	t.Parallel()

	in := &aigridv1.Subscription{
		Id: "free-subscription",
		Metadata: &aigridv1.Metadata{
			Name: "free-subscription",
			Annotations: map[string]string{
				"openshift.io/display-name": "Free",
			},
			Labels: map[string]string{"tier": "free"},
		},
		Spec: &aigridv1.SubscriptionSpec{
			Owner: &aigridv1.SubscriptionOwner{
				Groups: []string{"system:authenticated", "free-users"},
				Users:  []string{"sa:test"},
			},
			ModelRefs: []*aigridv1.SubscriptionModelRef{
				{
					Name: "sim-model",
					TokenRateLimits: []*aigridv1.TokenRateLimit{
						{Limit: 100, Window: "1m"},
					},
				},
			},
			Priority: 10,
		},
	}

	obj := subscriptionToUnstructured(in, defaultSubscriptionNamespace)
	if got := obj.GetName(); got != "free-subscription" {
		t.Fatalf("name = %q", got)
	}
	if got := obj.GetNamespace(); got != defaultSubscriptionNamespace {
		t.Fatalf("namespace = %q", got)
	}
	refs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "modelRefs")
	if len(refs) != 1 {
		t.Fatalf("modelRefs len = %d", len(refs))
	}
	ref := refs[0].(map[string]interface{})
	if ref["namespace"] != defaultModelNamespace {
		t.Fatalf("default model namespace = %v, want %q", ref["namespace"], defaultModelNamespace)
	}
	groups, _, _ := unstructured.NestedSlice(obj.Object, "spec", "owner", "groups")
	g0 := groups[0].(map[string]interface{})
	if g0["name"] != "system:authenticated" {
		t.Fatalf("owner.groups[0] = %v", g0)
	}

	_ = unstructured.SetNestedField(obj.Object, "Active", "status", "phase")
	obj.SetResourceVersion("42")

	out := subscriptionFromUnstructured(*obj)
	if out.GetId() != "free-subscription" {
		t.Fatalf("id = %q", out.GetId())
	}
	if out.GetStatus().GetPhase() != "Active" {
		t.Fatalf("phase = %q", out.GetStatus().GetPhase())
	}
	if out.GetMetadata().GetVersion() != 42 {
		t.Fatalf("version = %d", out.GetMetadata().GetVersion())
	}
	if got := out.GetSpec().GetOwner().GetGroups(); len(got) != 2 || got[0] != "system:authenticated" {
		t.Fatalf("groups = %v", got)
	}
	if got := out.GetSpec().GetModelRefs()[0].GetNamespace(); got != defaultModelNamespace {
		t.Fatalf("model namespace = %q", got)
	}
	if got := out.GetSpec().GetModelRefs()[0].GetTokenRateLimits()[0].GetLimit(); got != 100 {
		t.Fatalf("limit = %d", got)
	}
}

func TestAuthPolicyFromSubscription(t *testing.T) {
	t.Parallel()

	in := &aigridv1.Subscription{
		Id: "team-a",
		Metadata: &aigridv1.Metadata{
			Annotations: map[string]string{
				"openshift.io/display-name": "Team A",
				"other":                     "ignored",
			},
		},
		Spec: &aigridv1.SubscriptionSpec{
			Owner: &aigridv1.SubscriptionOwner{
				Groups: []string{"team-a"},
				Users:  []string{"alice"},
			},
			ModelRefs: []*aigridv1.SubscriptionModelRef{
				{Name: "granite", Namespace: "llm"},
			},
		},
	}

	policy := authPolicyFromSubscription(in, defaultSubscriptionNamespace)
	if policy.GetName() != "team-a-policy" {
		t.Fatalf("policy name = %q", policy.GetName())
	}
	if policy.GetKind() != "MaaSAuthPolicy" {
		t.Fatalf("kind = %q", policy.GetKind())
	}
	labels := policy.GetLabels()
	if labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("managed-by = %q", labels[managedByLabelKey])
	}
	if labels[subscriptionLabelKey] != "team-a" {
		t.Fatalf("subscription label = %q", labels[subscriptionLabelKey])
	}
	anns := policy.GetAnnotations()
	if anns["openshift.io/display-name"] != "Team A" {
		t.Fatalf("display-name = %q", anns["openshift.io/display-name"])
	}
	if _, ok := anns["other"]; ok {
		t.Fatal("unexpected other annotation copied")
	}
	subjects, _, _ := unstructured.NestedMap(policy.Object, "spec", "subjects")
	groups := subjects["groups"].([]interface{})
	if groups[0].(map[string]interface{})["name"] != "team-a" {
		t.Fatalf("subjects.groups = %v", groups)
	}
	users := subjects["users"].([]interface{})
	if users[0] != "alice" {
		t.Fatalf("subjects.users = %v", users)
	}
	refs, _, _ := unstructured.NestedSlice(policy.Object, "spec", "modelRefs")
	ref := refs[0].(map[string]interface{})
	if ref["name"] != "granite" || ref["namespace"] != "llm" {
		t.Fatalf("modelRefs = %v", ref)
	}
	if _, hasLimits := ref["tokenRateLimits"]; hasLimits {
		t.Fatal("auth policy must not carry tokenRateLimits")
	}
}

func TestIsManagedAuthPolicy(t *testing.T) {
	t.Parallel()
	managed := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{managedByLabelKey: managedByLabelValue},
		},
	}}
	if !isManagedAuthPolicy(managed) {
		t.Fatal("expected managed")
	}
	unmanaged := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{"app": "other"},
		},
	}}
	if isManagedAuthPolicy(unmanaged) {
		t.Fatal("expected unmanaged")
	}
	if isManagedAuthPolicy(nil) {
		t.Fatal("nil should be false")
	}
}

func TestAuthPolicyNameForSubscription(t *testing.T) {
	t.Parallel()
	if got := authPolicyNameForSubscription("free"); got != "free-policy" {
		t.Fatalf("got %q", got)
	}
	if maxSubscriptionIDLen+len(authPolicyNameSuffix) != 63 {
		t.Fatalf("max id + suffix must be 63, got %d", maxSubscriptionIDLen+len(authPolicyNameSuffix))
	}
}
