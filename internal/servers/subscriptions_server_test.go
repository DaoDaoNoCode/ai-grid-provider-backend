package servers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

var (
	testSubscriptionsGVR = schema.GroupVersionResource{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maassubscriptions",
	}
	testAuthPoliciesGVR = schema.GroupVersionResource{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasauthpolicies",
	}
	testModelRefsGVR = schema.GroupVersionResource{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasmodelrefs",
	}
)

func TestSubscriptionsServerRequiresClient(t *testing.T) {
	t.Parallel()
	if _, err := NewSubscriptionsServer().Build(); err == nil {
		t.Fatal("Build() without client succeeded")
	}
}

func TestSubscriptionsCRUDAndAuthPolicyPairing(t *testing.T) {
	t.Parallel()

	client := newSubscriptionClient(modelRefUnstructured("sim-model", "llm", "Simulator", "Ready"))
	srv := mustSubscriptionsServer(t, client)
	ctx := context.Background()

	created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{
		Object: sampleSubscription("team-a"),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.GetObject().GetId() != "team-a" {
		t.Fatalf("Create id = %q", created.GetObject().GetId())
	}
	if created.GetAuthPolicy() == nil || created.GetAuthPolicy().GetId() != "team-a-policy" {
		t.Fatalf("Create auth_policy = %#v", created.GetAuthPolicy())
	}

	policy, err := client.Resource(testAuthPoliciesGVR).Namespace("models-as-a-service").Get(ctx, "team-a-policy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get paired policy: %v", err)
	}
	if policy.GetLabels()["app.kubernetes.io/managed-by"] != "ai-grid-provider-backend" {
		t.Fatalf("policy labels = %v", policy.GetLabels())
	}
	subjects, _, _ := unstructured.NestedSlice(policy.Object, "spec", "subjects", "groups")
	if len(subjects) != 1 || subjects[0].(map[string]interface{})["name"] != "team-a" {
		t.Fatalf("policy subjects.groups = %v", subjects)
	}

	got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetObject().GetSpec().GetPriority() != 10 {
		t.Fatalf("priority = %d", got.GetObject().GetSpec().GetPriority())
	}
	if len(got.GetAuthPolicies()) != 1 || got.GetAuthPolicies()[0].GetId() != "team-a-policy" {
		t.Fatalf("Get auth_policies = %#v", got.GetAuthPolicies())
	}
	if len(got.GetModelRegistrations()) != 1 || got.GetModelRegistrations()[0].GetName() != "sim-model" {
		t.Fatalf("Get model_registrations = %#v", got.GetModelRegistrations())
	}
	if got.GetObject().GetSpec().GetModelRefs()[0].GetDisplayName() != "Simulator" {
		t.Fatalf("enriched display_name = %q", got.GetObject().GetSpec().GetModelRefs()[0].GetDisplayName())
	}

	list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.GetTotal() != 1 || list.GetSize() != 1 {
		t.Fatalf("List size/total = %d/%d", list.GetSize(), list.GetTotal())
	}

	updatedObj := sampleSubscription("team-a")
	updatedObj.Spec.Priority = 20
	updatedObj.Spec.Owner.Groups = []string{"team-a-admins"}
	updatedObj.Metadata = &aigridv1.Metadata{Name: "team-a"}
	if v := got.GetObject().GetMetadata().GetVersion(); v != 0 {
		updatedObj.Metadata.Version = v
	}
	updated, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: updatedObj})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.GetObject().GetSpec().GetPriority() != 20 {
		t.Fatalf("updated priority = %d", updated.GetObject().GetSpec().GetPriority())
	}

	policy, err = client.Resource(testAuthPoliciesGVR).Namespace("models-as-a-service").Get(ctx, "team-a-policy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy after update: %v", err)
	}
	subjects, _, _ = unstructured.NestedSlice(policy.Object, "spec", "subjects", "groups")
	if len(subjects) != 1 || subjects[0].(map[string]interface{})["name"] != "team-a-admins" {
		t.Fatalf("synced policy groups = %v", subjects)
	}

	// Force a known resourceVersion, then send a mismatched lock version.
	currentSub, err := client.Resource(testSubscriptionsGVR).Namespace("models-as-a-service").Get(ctx, "team-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get for stale test: %v", err)
	}
	currentSub.SetResourceVersion("99")
	if _, err := client.Resource(testSubscriptionsGVR).Namespace("models-as-a-service").Update(ctx, currentSub, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("seed resourceVersion: %v", err)
	}
	stale := sampleSubscription("team-a")
	stale.Metadata = &aigridv1.Metadata{Name: "team-a", Version: 1}
	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: stale, Lock: true})
	assertConnectCode(t, err, connect.CodeAborted)

	if _, err := srv.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a"}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := client.Resource(testSubscriptionsGVR).Namespace("models-as-a-service").Get(ctx, "team-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("subscription after delete: %v", err)
	}
	if _, err := client.Resource(testAuthPoliciesGVR).Namespace("models-as-a-service").Get(ctx, "team-a-policy", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("policy after delete: %v", err)
	}
}

func TestSubscriptionsCreateRejectsUnmanagedPolicyName(t *testing.T) {
	t.Parallel()

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSAuthPolicy",
		"metadata": map[string]interface{}{
			"name":      "team-a-policy",
			"namespace": "models-as-a-service",
			"labels":    map[string]interface{}{"app": "manual"},
		},
		"spec": map[string]interface{}{
			"modelRefs": []interface{}{
				map[string]interface{}{"name": "x", "namespace": "llm"},
			},
			"subjects": map[string]interface{}{
				"groups": []interface{}{map[string]interface{}{"name": "g"}},
			},
		},
	}}
	srv := mustSubscriptionsServer(t, newSubscriptionClient(existing))
	_, err := srv.Create(context.Background(), &aigridv1.SubscriptionsCreateRequest{
		Object: sampleSubscription("team-a"),
	})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}

func TestSubscriptionsDeleteLeavesUnmanagedPolicy(t *testing.T) {
	t.Parallel()

	sub := subscriptionUnstructured("legacy", "models-as-a-service")
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSAuthPolicy",
		"metadata": map[string]interface{}{
			"name":      "legacy-policy",
			"namespace": "models-as-a-service",
		},
		"spec": map[string]interface{}{
			"modelRefs": []interface{}{
				map[string]interface{}{"name": "sim", "namespace": "llm"},
			},
			"subjects": map[string]interface{}{
				"groups": []interface{}{map[string]interface{}{"name": "g"}},
			},
		},
	}}
	client := newSubscriptionClient(sub, policy)
	srv := mustSubscriptionsServer(t, client)

	if _, err := srv.Delete(context.Background(), &aigridv1.SubscriptionsDeleteRequest{Id: "legacy"}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := client.Resource(testAuthPoliciesGVR).Namespace("models-as-a-service").Get(context.Background(), "legacy-policy", metav1.GetOptions{}); err != nil {
		t.Fatalf("unmanaged policy should remain: %v", err)
	}
}

func TestSubscriptionsCreateCompensatesWhenPolicyFails(t *testing.T) {
	t.Parallel()

	client := newSubscriptionClient()
	client.PrependReactor("create", "maasauthpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("policy boom"))
	})
	srv := mustSubscriptionsServer(t, client)

	_, err := srv.Create(context.Background(), &aigridv1.SubscriptionsCreateRequest{
		Object: sampleSubscription("boom"),
	})
	if err == nil {
		t.Fatal("Create() expected error")
	}
	if _, getErr := client.Resource(testSubscriptionsGVR).Namespace("models-as-a-service").Get(context.Background(), "boom", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("subscription should be compensated away: %v", getErr)
	}
}

func TestSubscriptionsValidation(t *testing.T) {
	t.Parallel()

	srv := mustSubscriptionsServer(t, newSubscriptionClient())
	ctx := context.Background()

	tooLong := strings.Repeat("a", 57)
	_, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{
		Object: &aigridv1.Subscription{Id: tooLong, Spec: sampleSubscription("x").Spec},
	})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	bad := sampleSubscription("bad-window")
	bad.Spec.ModelRefs[0].TokenRateLimits[0].Window = "1d"
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: bad})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	unlimitedAndLimits := sampleSubscription("both")
	unlimitedAndLimits.Spec.ModelRefs[0].Unlimited = true
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: unlimitedAndLimits})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	noLimits := sampleSubscription("nolimits")
	noLimits.Spec.ModelRefs[0].TokenRateLimits = nil
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: noLimits})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	_, err = srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "missing"})
	assertConnectCode(t, err, connect.CodeNotFound)
}

func TestSubscriptionsOrganizationImmutable(t *testing.T) {
	t.Parallel()

	client := newSubscriptionClient()
	srv := mustSubscriptionsServer(t, client)
	ctx := context.Background()

	obj := sampleSubscription("org-sub")
	obj.Spec.Organization = &aigridv1.SubscriptionOrganization{Id: "org-1"}
	if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: obj}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	changed := sampleSubscription("org-sub")
	changed.Spec.Organization = &aigridv1.SubscriptionOrganization{Id: "org-2"}
	_, err := srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: changed})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	removed := sampleSubscription("org-sub")
	removed.Spec.Organization = nil
	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{Object: removed})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

func TestSubscriptionsUnlimitedCreate(t *testing.T) {
	t.Parallel()

	srv := mustSubscriptionsServer(t, newSubscriptionClient())
	obj := sampleSubscription("unlim")
	obj.Spec.ModelRefs[0].Unlimited = true
	obj.Spec.ModelRefs[0].TokenRateLimits = nil
	created, err := srv.Create(context.Background(), &aigridv1.SubscriptionsCreateRequest{Object: obj})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !created.GetObject().GetSpec().GetModelRefs()[0].GetUnlimited() {
		t.Fatal("expected unlimited")
	}
}

func TestSubscriptionsUpdateSkipsMissingManagedPolicy(t *testing.T) {
	t.Parallel()

	client := newSubscriptionClient(subscriptionUnstructured("orphan", "models-as-a-service"))
	srv := mustSubscriptionsServer(t, client)
	obj := sampleSubscription("orphan")
	obj.Metadata = &aigridv1.Metadata{Name: "orphan"}
	obj.Spec.Priority = 5
	if _, err := srv.Update(context.Background(), &aigridv1.SubscriptionsUpdateRequest{Object: obj}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
}

func TestSubscriptionsListFilterOrderAndPaging(t *testing.T) {
	t.Parallel()

	srv := mustSubscriptionsServer(t, newSubscriptionClient())
	ctx := context.Background()
	for _, tc := range []struct {
		id       string
		priority int32
	}{
		{"alpha", 10},
		{"bravo", 30},
		{"charlie", 20},
	} {
		obj := sampleSubscription(tc.id)
		obj.Spec.Priority = tc.priority
		if _, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: obj}); err != nil {
			t.Fatalf("Create(%s) error = %v", tc.id, err)
		}
	}

	filtered, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{
		Filter: ptrString(`this.id == "bravo"`),
	})
	if err != nil {
		t.Fatalf("List filter error = %v", err)
	}
	if filtered.GetTotal() != 1 || filtered.GetSize() != 1 || filtered.GetItems()[0].GetId() != "bravo" {
		t.Fatalf("filter by id = %#v", filtered)
	}

	byPriority, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{
		Filter: ptrString(`this.spec.priority >= 20`),
		Order:  ptrString("-priority"),
	})
	if err != nil {
		t.Fatalf("List priority filter error = %v", err)
	}
	if byPriority.GetTotal() != 2 || byPriority.GetItems()[0].GetId() != "bravo" || byPriority.GetItems()[1].GetId() != "charlie" {
		t.Fatalf("filter+order = %#v", byPriority)
	}

	limit := int32(1)
	offset := int32(0)
	page, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{
		Filter: ptrString(`this.spec.priority >= 10`),
		Order:  ptrString("id"),
		Limit:  &limit,
		Offset: &offset,
	})
	if err != nil {
		t.Fatalf("List page error = %v", err)
	}
	if page.GetTotal() != 3 || page.GetSize() != 1 || page.GetItems()[0].GetId() != "alpha" {
		t.Fatalf("paged = size=%d total=%d items=%v", page.GetSize(), page.GetTotal(), page.GetItems())
	}

	_, err = srv.List(ctx, &aigridv1.SubscriptionsListRequest{Order: ptrString("created_at")})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	_, err = srv.List(ctx, &aigridv1.SubscriptionsListRequest{Filter: ptrString(`this.id ==`)})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

func ptrString(s string) *string { return &s }

func mustSubscriptionsServer(t *testing.T, client *fake.FakeDynamicClient) *SubscriptionsServer {
	t.Helper()
	srv, err := NewSubscriptionsServer().SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return srv
}

func newSubscriptionClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			testSubscriptionsGVR: "MaaSSubscriptionList",
			testAuthPoliciesGVR:  "MaaSAuthPolicyList",
			testModelRefsGVR:     "MaaSModelRefList",
		},
		objects...,
	)
}

func sampleSubscription(id string) *aigridv1.Subscription {
	return &aigridv1.Subscription{
		Id: id,
		Metadata: &aigridv1.Metadata{
			Name: id,
			Annotations: map[string]string{
				"openshift.io/display-name": id,
			},
		},
		Spec: &aigridv1.SubscriptionSpec{
			Owner: &aigridv1.SubscriptionOwner{
				Groups: []string{id},
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
}

func subscriptionUnstructured(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSSubscription",
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       namespace,
			"resourceVersion": "1",
		},
		"spec": map[string]interface{}{
			"owner": map[string]interface{}{
				"groups": []interface{}{map[string]interface{}{"name": "g"}},
				"users":  []interface{}{},
			},
			"modelRefs": []interface{}{
				map[string]interface{}{
					"name":      "sim-model",
					"namespace": "llm",
					"tokenRateLimits": []interface{}{
						map[string]interface{}{"limit": int64(100), "window": "1m"},
					},
				},
			},
			"priority": int64(10),
		},
	}}
}

func modelRefUnstructured(name, namespace, displayName, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSModelRef",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"annotations": map[string]interface{}{
				"openshift.io/display-name": displayName,
			},
		},
		"spec": map[string]interface{}{
			"modelRef": map[string]interface{}{
				"kind": "LLMInferenceService",
				"name": name,
			},
		},
		"status": map[string]interface{}{
			"phase":    phase,
			"endpoint": "/llm/" + name,
		},
	}}
}
