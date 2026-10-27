package servers

import (
	"context"
	"errors"
	"maps"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// Messages as ACM reports them on demo1 and site-b.
const (
	msgReadyPolicy        = "Compliant; notification - datascienceclusters [default-dsc] found as specified; notification - deployments [grid-operator] found as specified in namespace grid"
	msgReadyPolicyViolate = "NonCompliant; notification - datascienceclusters [default-dsc] found as specified; violation - deployments [grid-operator] found but not as specified in namespace grid"
	msgModelReady         = "llminferenceservices [granite-4-tiny] found as specified in namespace ai-grid-internal-models"
	msgModelNotReady      = "llminferenceservices [granite-4-tiny] found but not as specified in namespace ai-grid-internal-models"
	msgEmpty              = "models-ready contains no object templates to check, and thus has no violations"
	msgTemplateFailure    = "failed to resolve the template {\"apiVersion\":\"policy.open-cluster-management.io/v1\"}: template: tmpl:3: function \"lookup\" not defined"
)

func TestParseCompliance(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		message string
		want    map[string]bool
		wantOK  bool
	}{
		"compliant":        {msgReadyPolicy, map[string]bool{"default-dsc": true, "grid-operator": true}, true},
		"mixed clauses":    {msgReadyPolicyViolate, map[string]bool{"default-dsc": true, "grid-operator": false}, true},
		"ready":            {msgModelReady, map[string]bool{"granite-4-tiny": true}, true},
		"not ready":        {msgModelNotReady, map[string]bool{"granite-4-tiny": false}, true},
		"several names":    {"NonCompliant; violation - llminferenceservices [a, b] found but not as specified in namespace m; notification - llminferenceservices [c] found as specified in namespace m", map[string]bool{"a": false, "b": false, "c": true}, true},
		"not found":        {"NonCompliant; violation - maassubscriptions [a] not found in namespace m; notification - maassubscriptions [b] found as specified in namespace m", map[string]bool{"a": false, "b": true}, true},
		"not ready wins":   {"llminferenceservices [a] found but not as specified in namespace m; llminferenceservices [a] found as specified in namespace m", map[string]bool{"a": false}, true},
		"auth policies":    {"Compliant; notification - maasauthpolicies [granite-org1-policy] found as specified in namespace models-as-a-service; notification - maasauthpolicies [opt-125m-org1] found as specified in namespace models-as-a-service", map[string]bool{"granite-org1-policy": true, "opt-125m-org1": true}, true},
		"nothing to check": {msgEmpty, map[string]bool{}, true},
		"template failure": {msgTemplateFailure, nil, false},
		"empty":            {"", nil, false},
		"unrecognised":     {"Compliant; something else entirely", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseCompliance(tc.message)
			if ok != tc.wantOK || !maps.Equal(got, tc.want) {
				t.Fatalf("parseCompliance() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestCatalogStatusRead(t *testing.T) {
	t.Parallel()
	client := newPolicyClient(
		rootPolicy("ai-grid-models-status", "site-a", "site-b", "site-c", "site-d"),
		clusterPolicy("site-a", "ai-grid-models-status", map[string]string{"models-ready": msgModelReady, "other": msgModelNotReady}),
		clusterPolicy("site-b", "ai-grid-models-status", map[string]string{"models-ready": msgModelNotReady}),
		clusterPolicy("site-c", "ai-grid-models-status", map[string]string{"models-ready": msgTemplateFailure}),
		// site-d has no copy yet.
	)
	got, err := NewCatalogStatusReader(client, "").Read(context.Background(), "ai-grid-models-status", "models-ready")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	// Every cluster the Policy is placed on is in the result; one without a
	// usable report maps to nil.
	want := ObjectReports{"site-a": {"granite-4-tiny": true}, "site-b": {"granite-4-tiny": false}, "site-c": nil, "site-d": nil}
	if !maps.EqualFunc(got, want, func(a, b map[string]bool) bool { return (a == nil) == (b == nil) && maps.Equal(a, b) }) {
		t.Fatalf("Read() = %v, want %v", got, want)
	}
}

// TestCatalogStatusReadHub reads each check the backend uses from the
// Policies of a real hub (testdata/hub-status-policies.json), so a check name
// the Policies don't have fails here.
func TestCatalogStatusReadHub(t *testing.T) {
	t.Parallel()
	reader := NewCatalogStatusReader(newPolicyClient(hubStatusPolicies(t)...), "")
	for _, tc := range []struct {
		policy, check string
		want          ObjectReports
	}{
		{modelsStatusPolicy, modelsReadyCheck, ObjectReports{"site-a": {}, "site-b": {"granite-4-tiny": true}}},
		{catalogStatusPolicy, modelRefsReadyCheck, ObjectReports{"site-a": {"facebook-opt-125m": true, "granite-4-tiny": true}}},
		{catalogStatusPolicy, subscriptionsActiveCheck, ObjectReports{"site-a": {"granite-org1": true, "opt-125m-org1": true}}},
		{catalogStatusPolicy, authPoliciesActiveCheck, ObjectReports{"site-a": {"granite-org1-policy": true, "opt-125m-org1": true}}},
	} {
		t.Run(tc.check, func(t *testing.T) {
			t.Parallel()
			got, err := reader.Read(context.Background(), tc.policy, tc.check)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if !maps.EqualFunc(got, tc.want, func(a, b map[string]bool) bool { return (a == nil) == (b == nil) && maps.Equal(a, b) }) {
				t.Fatalf("Read(%s, %s) = %v, want %v", tc.policy, tc.check, got, tc.want)
			}
		})
	}
}

func TestCatalogStatusReadUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	forbidden := apierrors.NewForbidden(policiesResource.GroupResource(), "x", errors.New("no access"))

	t.Run("nil reader", func(t *testing.T) {
		t.Parallel()
		var r *CatalogStatusReader
		if got, err := r.Read(ctx, "p", "c"); got != nil || err != nil {
			t.Fatalf("Read() = %v, %v", got, err)
		}
	})
	t.Run("root not found", func(t *testing.T) {
		t.Parallel()
		got, err := NewCatalogStatusReader(newPolicyClient(), "").Read(ctx, "ai-grid-models-status", "models-ready")
		if got != nil || err != nil {
			t.Fatalf("Read() = %v, %v; want no status", got, err)
		}
	})
	t.Run("root forbidden", func(t *testing.T) {
		t.Parallel()
		client := newPolicyClient()
		client.PrependReactor("get", "policies", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
		got, err := NewCatalogStatusReader(client, "").Read(ctx, "ai-grid-models-status", "models-ready")
		if got != nil || err != nil {
			t.Fatalf("Read() = %v, %v; want no status", got, err)
		}
	})
	t.Run("copy forbidden", func(t *testing.T) {
		t.Parallel()
		client := newPolicyClient(rootPolicy("ai-grid-models-status", "site-a"))
		client.PrependReactor("get", "policies", func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.GetNamespace() == "site-a" {
				return true, nil, forbidden
			}
			return false, nil, nil
		})
		got, err := NewCatalogStatusReader(client, "").Read(ctx, "ai-grid-models-status", "models-ready")
		if err != nil || len(got) != 1 || got["site-a"] != nil {
			t.Fatalf("Read() = %v, %v; want site-a with no report", got, err)
		}
	})
	t.Run("other error", func(t *testing.T) {
		t.Parallel()
		client := newPolicyClient()
		client.PrependReactor("get", "policies", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("down")
		})
		if _, err := NewCatalogStatusReader(client, "").Read(ctx, "ai-grid-models-status", "models-ready"); err == nil {
			t.Fatal("Read() error = nil, want the API error")
		}
	})
}

func newPolicyClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{policiesResource: "PolicyList"},
		objects...,
	)
}

// rootPolicy is a root Policy in ai-grid-policies that lists clusters in
// status.status, each in a namespace of its own name.
func rootPolicy(name string, clusters ...string) *unstructured.Unstructured {
	status := make([]interface{}, 0, len(clusters))
	for _, cluster := range clusters {
		status = append(status, map[string]interface{}{"clustername": cluster, "clusternamespace": cluster, "compliant": "NonCompliant"})
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "policy.open-cluster-management.io/v1",
		"kind":       "Policy",
		"metadata":   map[string]interface{}{"name": name, "namespace": DefaultPolicyNamespace},
		"status":     map[string]interface{}{"status": status},
	}}
}

// clusterPolicy is a cluster's copy of a root Policy, with the latest message
// of each ConfigurationPolicy, after an older one that must be ignored.
func clusterPolicy(cluster, policy string, messages map[string]string) *unstructured.Unstructured {
	details := []interface{}{}
	for check, message := range messages {
		details = append(details, map[string]interface{}{
			"compliant":    "NonCompliant",
			"templateMeta": map[string]interface{}{"name": check},
			"history": []interface{}{
				map[string]interface{}{"message": message, "eventName": "e2"},
				map[string]interface{}{"message": msgTemplateFailure, "eventName": "e1"},
			},
		})
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "policy.open-cluster-management.io/v1",
		"kind":       "Policy",
		"metadata":   map[string]interface{}{"name": DefaultPolicyNamespace + "." + policy, "namespace": cluster},
		"status":     map[string]interface{}{"details": details},
	}}
}
