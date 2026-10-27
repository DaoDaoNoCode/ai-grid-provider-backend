package servers

import (
	"context"
	"errors"
	"testing"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestModelsGitStatus(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		objects     []runtime.Object
		forbid      bool
		wantPhase   aigridv1.ModelPhase
		wantReason  string
		wantMessage string
	}{
		"ready": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "hub"),
				clusterPolicy("hub", catalogStatusPolicy, modelRefReady(true)),
			},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_READY, wantReason: "Ready", wantMessage: "Ready on hub, site-a, site-b",
		},
		"not ready on one cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(false)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
			},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "NotReady", wantMessage: "Not ready on site-a; ready on site-b",
		},
		"MaaSModelRef not ready on a combined cluster": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, modelReady(true)),
				rootPolicy(catalogStatusPolicy, "site-a"),
				clusterPolicy("site-a", catalogStatusPolicy, modelRefReady(false)),
			},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "NotReady", wantMessage: "Not ready on site-a; ready on site-b",
		},
		"a placed cluster hasn't reported": {
			objects: []runtime.Object{
				rootPolicy(modelsStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", modelsStatusPolicy, modelReady(true)),
				clusterPolicy("site-b", modelsStatusPolicy, map[string]string{modelsReadyCheck: "models-ready contains no object templates to check, and thus has no violations"}),
			},
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "NotReady", wantMessage: "Ready on site-a; no report from site-b",
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
			wantPhase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, wantReason: "NotReady", wantMessage: "Ready on hub, site-a, site-b; no report from edge",
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
	for name, tc := range map[string]struct {
		objects        []runtime.Object
		forbid         bool
		wantPhase      string
		wantModelPhase string
		wantReason     string
	}{
		"active": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: subscriptionActive(true), modelRefsReadyCheck: refReady(true)}),
				clusterPolicy("site-b", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: subscriptionActive(true), modelRefsReadyCheck: refReady(true)}),
			},
			wantPhase: "Active", wantModelPhase: "Ready", wantReason: "Ready",
		},
		"not active on one cluster": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: subscriptionActive(false), modelRefsReadyCheck: refReady(false)}),
				clusterPolicy("site-b", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: subscriptionActive(true), modelRefsReadyCheck: refReady(true)}),
			},
			wantPhase: "Pending", wantModelPhase: "Pending", wantReason: "NotReady",
		},
		// An ingress cluster that doesn't have the objects yet, or hasn't
		// reported, keeps them from Active and Ready.
		"missing on one cluster": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: subscriptionActive(true), modelRefsReadyCheck: refReady(true)}),
				clusterPolicy("site-b", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: msgEmpty, modelRefsReadyCheck: msgEmpty}),
			},
			wantPhase: "Pending", wantModelPhase: "Pending", wantReason: "NotReady",
		},
		"one cluster hasn't reported": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a", "site-b"),
				clusterPolicy("site-a", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: subscriptionActive(true), modelRefsReadyCheck: refReady(true)}),
			},
			wantPhase: "Pending", wantModelPhase: "Pending", wantReason: "NotReady",
		},
		"no reports": {
			objects:    []runtime.Object{rootPolicy(catalogStatusPolicy, "site-a")},
			wantReason: "SavedToGit",
		},
		"forbidden": {
			objects: []runtime.Object{
				rootPolicy(catalogStatusPolicy, "site-a"),
				clusterPolicy("site-a", catalogStatusPolicy, map[string]string{subscriptionsActiveCheck: subscriptionActive(true)}),
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
		})
	}
}
