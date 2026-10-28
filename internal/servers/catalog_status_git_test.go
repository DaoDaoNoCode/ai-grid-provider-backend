package servers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"buf.build/go/protovalidate"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// statusMessage is a ConfigurationPolicy message about one object.
func statusMessage(plural, name, namespace string, ready bool) string {
	if ready {
		return plural + " [" + name + "] found as specified in namespace " + namespace
	}
	return plural + " [" + name + "] found but not as specified in namespace " + namespace
}

func modelReady(ready bool) map[string]string {
	return map[string]string{modelsReadyCheck: statusMessage("llminferenceservices", "granite", "ai-grid-internal-models", ready)}
}

func modelRefReady(ready bool) map[string]string {
	return map[string]string{modelRefsReadyCheck: statusMessage("maasmodelrefs", "granite", "models-as-a-service", ready)}
}

// forbidPolicies makes every Policy get Forbidden.
func forbidPolicies(client *fake.FakeDynamicClient) {
	client.PrependReactor("get", "policies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(policiesResource.GroupResource(), "x", errors.New("no access"))
	})
}

// clusterStatus is a ModelClusterStatus a test expects.
func clusterStatus(cluster string, phase aigridv1.ModelPhase, reason, message string) *aigridv1.ModelClusterStatus {
	return &aigridv1.ModelClusterStatus{Cluster: cluster, Phase: phase, Reason: reason, Message: message}
}

const (
	phaseReady   = aigridv1.ModelPhase_MODEL_PHASE_READY
	phasePending = aigridv1.ModelPhase_MODEL_PHASE_PENDING
	phasePartial = aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE
)

// TestModelsGitStatus checks the phase, reason and message of a model placed
// on site-a and site-b, and its status on each of them (status.clusters),
// from the status Policies' reports. Every ModelClusterStatus must pass the
// buf.validate rules declared on it.
func TestModelsGitStatus(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		objects      []runtime.Object
		forbid       bool
		wantPhase    aigridv1.ModelPhase
		wantReason   string
		wantMessage  string
		wantClusters []*aigridv1.ModelClusterStatus
	}{
		"ready": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "hub"),
				clusterPolicy("hub", catalogStatusPolicy, modelRefReady(true)),
			},
			wantPhase: phaseReady, wantReason: "Ready", wantMessage: "Ready on hub, site-a, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", "LLMInferenceService ready"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"ready on a combined cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "site-a"),
				clusterPolicy("site-a", catalogStatusPolicy, modelRefReady(true)),
			},
			wantPhase: phaseReady, wantReason: "Ready", wantMessage: "Ready on site-a, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", "LLMInferenceService ready; MaaSModelRef ready"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"not ready on one cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(false)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable", wantMessage: "Not ready on site-a; ready on site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", "LLMInferenceService not ready"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"not ready on one cluster, ready on the ingress": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(false)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "hub"),
				clusterPolicy("hub", catalogStatusPolicy, modelRefReady(true)),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable", wantMessage: "Not ready on site-a; ready on hub, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", "LLMInferenceService not ready"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"not ready on one cluster and on the ingress": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(false)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "hub"),
				clusterPolicy("hub", catalogStatusPolicy, modelRefReady(false)),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Not ready on hub, site-a; ready on site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", "LLMInferenceService not ready"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"not ready anywhere": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(false)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(false)),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Not ready on site-a, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", "LLMInferenceService not ready"),
				clusterStatus("site-b", phasePending, "NotReady", "LLMInferenceService not ready"),
			},
		},
		"MaaSModelRef not ready on a combined cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "site-a"),
				clusterPolicy("site-a", catalogStatusPolicy, modelRefReady(false)),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Not ready on site-a; ready on site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", "LLMInferenceService ready; MaaSModelRef not ready"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"a placed cluster hasn't reported": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, map[string]string{modelsReadyCheck: "models-ready contains no object templates to check, and thus has no violations"}),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable", wantMessage: "Ready on site-a; no report from site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", "LLMInferenceService ready"),
				clusterStatus("site-b", phasePending, "NoReport", "No LLMInferenceService report"),
			},
		},
		"a placed cluster the Policy isn't placed on": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable", wantMessage: "Ready on site-a; no report from site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", "LLMInferenceService ready"),
				clusterStatus("site-b", phasePending, "NoReport", "No LLMInferenceService report"),
			},
		},
		"MaaSModelRef not reported on a combined cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "site-a"),
				clusterPolicy("site-a", catalogStatusPolicy, map[string]string{modelRefsReadyCheck: msgEmpty}),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Ready on site-b; no report from site-a",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NoReport", "LLMInferenceService ready; no MaaSModelRef report"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"MaaSModelRef missing on an ingress cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "hub", "edge"),
				clusterPolicy("hub", catalogStatusPolicy, modelRefReady(true)),
				clusterPolicy("edge", catalogStatusPolicy, map[string]string{modelRefsReadyCheck: msgEmpty}),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Ready on hub, site-a, site-b; no report from edge",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", "LLMInferenceService ready"),
				clusterStatus("site-b", phaseReady, "Ready", "LLMInferenceService ready"),
			},
		},
		"no reports": {
			objects:    []runtime.Object{rootPolicy(modelsStatusPolicy, "site-a")},
			wantReason: "SavedToGit", wantMessage: "Saved to Git; no cluster has reported the model's status",
		},
		"forbidden": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
			},
			forbid:     true,
			wantReason: "SavedToGit", wantMessage: "Saved to Git; no cluster has reported the model's status",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			client := newPolicyClient(tc.objects...)
			if tc.forbid {
				forbidPolicies(client)
			}
			srv, err := NewModelsServer().SetWriter(gitops.NewMemoryWriter(nil)).
				SetCatalogStatus(NewCatalogStatusReader(client, "")).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("granite", "site-a", "site-b")})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "granite"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			for op, model := range map[string]*aigridv1.Model{"Create": created.GetObject(), "Get": got.GetObject(), "List": list.GetItems()[0]} {
				st := model.GetStatus()
				if st.GetPhase() != tc.wantPhase || st.GetReason() != tc.wantReason || st.GetMessage() != tc.wantMessage {
					t.Errorf("%s status = %v, %q, %q; want %v, %q, %q", op, st.GetPhase(), st.GetReason(), st.GetMessage(), tc.wantPhase, tc.wantReason, tc.wantMessage)
				}
				if st.GetPlacement().GetNumberOfSelectedClusters() != 2 {
					t.Errorf("%s placement = %v, want site-a and site-b", op, st.GetPlacement())
				}
				if !slices.EqualFunc(st.GetClusters(), tc.wantClusters, func(a, b *aigridv1.ModelClusterStatus) bool { return proto.Equal(a, b) }) {
					t.Errorf("%s status.clusters = %v; want %v", op, st.GetClusters(), tc.wantClusters)
				}
				for _, cluster := range st.GetClusters() {
					if err := protovalidate.Validate(cluster); err != nil {
						t.Errorf("%s status.clusters[%s] fails its buf.validate rules: %v", op, cluster.GetCluster(), err)
					}
				}
			}
		})
	}
}

// TestModelClusterStatusValidationRules checks the buf.validate rules on
// ModelClusterStatus refuse what the server never writes: a cluster that is
// not a DNS label, an UNSPECIFIED, PARTIALLY_AVAILABLE or undefined phase,
// and an empty reason.
func TestModelClusterStatusValidationRules(t *testing.T) {
	t.Parallel()
	valid := clusterStatus("site-a", phaseReady, "Ready", "LLMInferenceService ready")
	if err := protovalidate.Validate(valid); err != nil {
		t.Fatalf("protovalidate(%v) error = %v, want none", valid, err)
	}
	for name, mutate := range map[string]func(*aigridv1.ModelClusterStatus){
		"empty cluster":       func(c *aigridv1.ModelClusterStatus) { c.Cluster = "" },
		"cluster not a label": func(c *aigridv1.ModelClusterStatus) { c.Cluster = "Site_A" },
		"unspecified phase":   func(c *aigridv1.ModelClusterStatus) { c.Phase = aigridv1.ModelPhase_MODEL_PHASE_UNSPECIFIED },
		"partial phase":       func(c *aigridv1.ModelClusterStatus) { c.Phase = phasePartial },
		"undefined phase":     func(c *aigridv1.ModelClusterStatus) { c.Phase = 99 },
		"empty reason":        func(c *aigridv1.ModelClusterStatus) { c.Reason = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := proto.Clone(valid).(*aigridv1.ModelClusterStatus)
			mutate(c)
			var verr *protovalidate.ValidationError
			if err := protovalidate.Validate(c); !errors.As(err, &verr) {
				t.Fatalf("protovalidate(%v) error = %v, want a ValidationError", c, err)
			}
		})
	}
}

func TestSubscriptionsGitStatus(t *testing.T) {
	t.Parallel()
	subscriptionActive := func(ready bool) string {
		return statusMessage("maassubscriptions", "team-a", "models-as-a-service", ready)
	}
	refReady := func(ready bool) string {
		return statusMessage("maasmodelrefs", "sim-model", "models-as-a-service", ready)
	}
	policyActive := func(ready bool) string {
		return statusMessage("maasauthpolicies", "team-a-policy", "models-as-a-service", ready)
	}
	catalog := func(ready bool) map[string]string {
		return map[string]string{
			subscriptionsActiveCheck: subscriptionActive(ready),
			modelRefsReadyCheck:      refReady(ready),
			authPoliciesActiveCheck:  policyActive(ready),
		}
	}
	for name, tc := range map[string]struct {
		objects           []runtime.Object
		forbid            bool
		wantPhase         string
		wantModelPhase    string
		wantReason        string
		wantPolicyPhase   string
		wantPolicyMessage string
	}{
		"active": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, catalog(true)),
				clusterPolicy("site-b", catalogStatusPolicy, catalog(true)),
			},
			wantPhase: "Active", wantModelPhase: "Ready", wantReason: "Ready",
			wantPolicyPhase: "Active", wantPolicyMessage: "Ready on site-a, site-b",
		},
		"not active on one cluster": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, catalog(false)),
				clusterPolicy("site-b", catalogStatusPolicy, catalog(true)),
			},
			wantPhase: "Pending", wantModelPhase: "Pending", wantReason: "NotReady",
			wantPolicyPhase: "Pending", wantPolicyMessage: "Not ready on site-a; ready on site-b",
		},
		// An ingress cluster that doesn't have the objects yet, or hasn't
		// reported, keeps them from Active and Ready.
		"missing on one cluster": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, catalog(true)),
				clusterPolicy("site-b", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: msgEmpty, modelRefsReadyCheck: msgEmpty, authPoliciesActiveCheck: msgEmpty}),
			},
			wantPhase: "Pending", wantModelPhase: "Pending", wantReason: "NotReady",
			wantPolicyPhase: "Pending", wantPolicyMessage: "Ready on site-a; no report from site-b",
		},
		"one cluster hasn't reported": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, catalog(true)),
			},
			wantPhase: "Pending", wantModelPhase: "Pending", wantReason: "NotReady",
			wantPolicyPhase: "Pending", wantPolicyMessage: "Ready on site-a; no report from site-b",
		},
		// Subscriptions and their policies go to the ingress clusters, the
		// ones the catalog Policy is placed on. An inference-only cluster,
		// which only the models Policy reports for, has no say in their
		// status.
		"inference-only cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-b", modelsStatusPolicy, map[string]string{modelsReadyCheck: msgEmpty}),
				rootPolicy(catalogStatusPolicy, "site-a"),
				clusterPolicy("site-a", catalogStatusPolicy, catalog(true)),
			},
			wantPhase: "Active", wantModelPhase: "Ready", wantReason: "Ready",
			wantPolicyPhase: "Active", wantPolicyMessage: "Ready on site-a",
		},
		"no reports": {
			objects:    []runtime.Object{rootPolicy(catalogStatusPolicy, "site-a")},
			wantReason: "SavedToGit",
		},
		"forbidden": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a"),
				clusterPolicy("site-a", catalogStatusPolicy, catalog(true)),
			},
			forbid:     true,
			wantReason: "SavedToGit",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			client := newPolicyClient(tc.objects...)
			if tc.forbid {
				forbidPolicies(client)
			}
			srv, err := NewSubscriptionsServer().SetWriter(gitops.NewMemoryWriter(gitModelSeed("sim-model"))).
				SetCatalogStatus(NewCatalogStatusReader(client, "")).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			for op, sub := range map[string]*aigridv1.Subscription{"Create": created.GetObject(), "Get": got.GetObject(), "List": list.GetItems()[0]} {
				if phase := sub.GetStatus().GetPhase(); phase != tc.wantPhase {
					t.Errorf("%s status.phase = %q, want %q", op, phase, tc.wantPhase)
				}
			}
			regs := got.GetModelRegistrations()
			if len(regs) != 1 || regs[0].GetName() != "sim-model" || regs[0].GetPhase() != tc.wantModelPhase || regs[0].GetConditions()[0].GetReason() != tc.wantReason {
				t.Fatalf("Get model_registrations = %v, want phase %q, reason %q", regs, tc.wantModelPhase, tc.wantReason)
			}
			if len(got.GetAuthPolicies()) != 1 {
				t.Fatalf("Get auth_policies = %v, want team-a-policy", got.GetAuthPolicies())
			}
			for op, policy := range map[string]*aigridv1.AuthPolicy{"Create": created.GetAuthPolicy(), "Get": got.GetAuthPolicies()[0]} {
				checkAuthPolicyStatus(t, op, policy, "team-a-policy", tc.wantPolicyPhase, tc.wantPolicyMessage)
			}
		})
	}
}

// checkAuthPolicyStatus checks an AuthPolicy's phase and its Ready
// condition: none without a phase, True with Active, False with Pending.
func checkAuthPolicyStatus(t *testing.T, op string, policy *aigridv1.AuthPolicy, wantID, wantPhase, wantMessage string) {
	t.Helper()
	if policy.GetId() != wantID || policy.GetPhase() != wantPhase {
		t.Errorf("%s auth policy %q phase = %q, want %q phase %q", op, policy.GetId(), policy.GetPhase(), wantID, wantPhase)
	}
	conditions := policy.GetConditions()
	if wantPhase == "" {
		if len(conditions) != 0 {
			t.Errorf("%s auth policy conditions = %v, want none", op, conditions)
		}
		return
	}
	wantStatus, wantReason := aigridv1.ConditionStatus_CONDITION_STATUS_TRUE, "Ready"
	if wantPhase != "Active" {
		wantStatus, wantReason = aigridv1.ConditionStatus_CONDITION_STATUS_FALSE, "NotReady"
	}
	if len(conditions) != 1 || conditions[0].GetType() != "Ready" || conditions[0].GetStatus() != wantStatus ||
		conditions[0].GetReason() != wantReason || conditions[0].GetMessage() != wantMessage {
		t.Errorf("%s auth policy conditions = %v, want Ready %v, reason %q, message %q", op, conditions, wantStatus, wantReason, wantMessage)
	}
}

// TestSubscriptionsGitStatusFromHub reads the status Policies as a real hub
// reports them (testdata/hub-status-policies.json). site-a, a combined
// cluster, reports subscription granite-org1, its policy granite-org1-policy
// and model granite-4-tiny active; site-b, an inference-only cluster, has no
// copy of the catalog Policy and so no say in the subscription's status.
func TestSubscriptionsGitStatusFromHub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	status := NewCatalogStatusReader(newPolicyClient(hubStatusPolicies(t)...), "")
	srv, err := NewSubscriptionsServer().SetWriter(gitops.NewMemoryWriter(gitModelSeed("granite-4-tiny"))).
		SetCatalogStatus(status).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	object := gitSampleSubscription("granite-org1")
	object.Spec.ModelRefs[0].Name = "granite-4-tiny"
	created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: object})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "granite-org1"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if phase := got.GetObject().GetStatus().GetPhase(); phase != "Active" {
		t.Errorf("Get status.phase = %q, want Active", phase)
	}
	if len(got.GetAuthPolicies()) != 1 {
		t.Fatalf("Get auth_policies = %v, want granite-org1-policy", got.GetAuthPolicies())
	}
	for op, policy := range map[string]*aigridv1.AuthPolicy{"Create": created.GetAuthPolicy(), "Get": got.GetAuthPolicies()[0]} {
		checkAuthPolicyStatus(t, op, policy, "granite-org1-policy", "Active", "Ready on site-a")
	}
	regs := got.GetModelRegistrations()
	if len(regs) != 1 || regs[0].GetPhase() != "Ready" || regs[0].GetConditions()[0].GetMessage() != "Ready on site-a" {
		t.Errorf("Get model_registrations = %v, want granite-4-tiny Ready on site-a", regs)
	}
}

// hubStatusPolicies returns the Policies in testdata/hub-status-policies.json.
func hubStatusPolicies(t *testing.T) []runtime.Object {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hub-status-policies.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var list struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	objects := make([]runtime.Object, 0, len(list.Items))
	for _, item := range list.Items {
		objects = append(objects, &unstructured.Unstructured{Object: item})
	}
	return objects
}
