package servers

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// withPruning marks a status.resources entry as waiting to be pruned.
func withPruning(res map[string]interface{}) map[string]interface{} {
	res["requiresPruning"] = true
	return res
}

// ownedResource is a status.resources entry for kind namespace/name.
func ownedResource(kind, namespace, name string) map[string]interface{} {
	return map[string]interface{}{"kind": kind, "namespace": namespace, "name": name, "status": "Synced"}
}

func TestCatalogStatusSharedResources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	external := ObjectKey{Kind: kindExternalModel, Namespace: defaultSubscriptionNamespace, Name: "m"}
	ref := ObjectKey{Kind: kindMaaSModelRef, Namespace: defaultSubscriptionNamespace, Name: "m"}
	for name, tc := range map[string]struct {
		apps []runtime.Object
		want []SharedResource
	}{
		"an Application outside the catalog owns the object": {
			apps: []runtime.Object{applicationNamed("demo-maas-site-a", "demo-maas",
				ownedResource(kindExternalModel, defaultSubscriptionNamespace, "m"),
				ownedResource(kindMaaSModelRef, defaultSubscriptionNamespace, "m"))},
			want: []SharedResource{
				{Object: external, Application: "demo-maas-site-a"},
				{Object: ref, Application: "demo-maas-site-a"},
			},
		},
		"several owners sort by Application, then object, and carry pruning": {
			apps: []runtime.Object{
				applicationNamed("zz-demo", "zz-demo", ownedResource(kindExternalModel, defaultSubscriptionNamespace, "m")),
				applicationNamed("aa-demo", "aa-demo",
					ownedResource(kindMaaSModelRef, defaultSubscriptionNamespace, "m"),
					withPruning(ownedResource(kindExternalModel, defaultSubscriptionNamespace, "m"))),
			},
			want: []SharedResource{
				{Object: external, Application: "aa-demo", Pruning: true},
				{Object: ref, Application: "aa-demo"},
				{Object: external, Application: "zz-demo"},
			},
		},
		"an Application a catalog ApplicationSet owns under another name shares the object": {
			apps: []runtime.Object{applicationNamed("renamed-site-a", DefaultCatalogIngressAppSet, ownedResource(kindExternalModel, defaultSubscriptionNamespace, "m"))},
			want: []SharedResource{{Object: external, Application: "renamed-site-a"}},
		},
		"a hand-made Application with no ApplicationSet owns the object": {
			apps: []runtime.Object{applicationNamed("by-hand", "", ownedResource(kindExternalModel, defaultSubscriptionNamespace, "m"))},
			want: []SharedResource{{Object: external, Application: "by-hand"}},
		},
		"the catalog's own Applications share nothing": {
			apps: []runtime.Object{
				application(DefaultCatalogIngressAppSet, "site-a", ownedResource(kindExternalModel, defaultSubscriptionNamespace, "m")),
				application(DefaultCatalogModelsAppSet, "site-a", ownedResource(kindLLMInferenceService, defaultModelsNamespace, "m")),
			},
		},
		"a catalog-named Application no catalog ApplicationSet owns shares the object": {
			apps: []runtime.Object{applicationNamed(DefaultCatalogIngressAppSet+"-site-a", "", ownedResource(kindExternalModel, defaultSubscriptionNamespace, "m"))},
			want: []SharedResource{{Object: external, Application: DefaultCatalogIngressAppSet + "-site-a"}},
		},
		"the same name in another namespace or of another kind is not the object": {
			apps: []runtime.Object{applicationNamed("demo", "demo",
				ownedResource(kindExternalModel, "elsewhere", "m"),
				ownedResource(kindMaaSSubscription, defaultSubscriptionNamespace, "m"))},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := NewCatalogStatusReader(newAppClient(tc.apps...), CatalogApps{}).SharedResources(ctx, []ObjectKey{external, ref})
			if err != nil {
				t.Fatalf("SharedResources() error = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("SharedResources() = %v, want %v", got, tc.want)
			}
		})
	}

	for name, apiErr := range map[string]error{
		"not found": apierrors.NewNotFound(applicationsResource.GroupResource(), ""),
		"forbidden": apierrors.NewForbidden(applicationsResource.GroupResource(), "", errors.New("no access")),
	} {
		t.Run(name+" reports none", func(t *testing.T) {
			t.Parallel()
			client := newAppClient()
			client.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apiErr
			})
			if got, err := NewCatalogStatusReader(client, CatalogApps{}).SharedResources(ctx, []ObjectKey{external}); got != nil || err != nil {
				t.Fatalf("SharedResources() = %v, %v; want nil, nil", got, err)
			}
		})
	}
	t.Run("other errors are returned", func(t *testing.T) {
		t.Parallel()
		client := newAppClient()
		client.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("down")
		})
		if _, err := NewCatalogStatusReader(client, CatalogApps{}).SharedResources(ctx, []ObjectKey{external}); !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("SharedResources() error = %v, want the wrapped API error", err)
		}
	})
}

// A model whose objects another Application already delivers is refused
// before anything is committed, naming the object and the Application.
func TestModelsGitCreateRefusesSharedResource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		app      runtime.Object
		wantCode connect.Code
		wantText []string
	}{
		"a sample model's MaaS objects": {
			app: applicationNamed("acm-models-maas-ing-spoke", "acm-models-maas",
				ownedResource(kindExternalModel, defaultSubscriptionNamespace, "sim"),
				ownedResource(kindMaaSModelRef, defaultSubscriptionNamespace, "sim")),
			wantCode: connect.CodeAlreadyExists,
			wantText: []string{"ExternalModel models-as-a-service/sim", "MaaSModelRef models-as-a-service/sim", "acm-models-maas-ing-spoke"},
		},
		"a sample model's deployment": {
			app: applicationNamed("acm-models-sim-inf-spoke", "acm-models-sim",
				ownedResource(kindLLMInferenceService, defaultModelsNamespace, "sim")),
			wantCode: connect.CodeAlreadyExists,
			wantText: []string{"LLMInferenceService ai-grid-internal-models/sim", "acm-models-sim-inf-spoke"},
		},
		"a sample model's NetworkPolicy": {
			app: applicationNamed("acm-models-sim-inf-spoke", "acm-models-sim",
				ownedResource(kindNetworkPolicy, defaultModelsNamespace, modelNetworkPolicyName("sim"))),
			wantCode: connect.CodeAlreadyExists,
			wantText: []string{"NetworkPolicy ai-grid-internal-models/sim-grid"},
		},
		"objects removed from the sample but not yet pruned": {
			app: applicationNamed("acm-models-maas-ing-spoke", "acm-models-maas",
				withPruning(ownedResource(kindExternalModel, defaultSubscriptionNamespace, "sim"))),
			wantCode: connect.CodeAlreadyExists,
			wantText: []string{"waiting to be pruned", "let Argo CD prune it"},
		},
		"only the catalog's own copy": {
			app: application(DefaultCatalogIngressAppSet, "ing-spoke",
				ownedResource(kindExternalModel, defaultSubscriptionNamespace, "sim")),
		},
		"another model's objects": {
			app: applicationNamed("acm-models-maas-ing-spoke", "acm-models-maas",
				ownedResource(kindExternalModel, defaultSubscriptionNamespace, "other")),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := newAppClient(managedCluster("site-a", map[string]string{"location": "us-east"}, true), tc.app)
			writer := gitops.NewMemoryWriter(nil)
			srv, err := NewModelsServer().SetLogger(slog.New(slog.DiscardHandler)).
				SetWriter(writer).SetDynamicClient(client).
				SetCatalogStatus(NewCatalogStatusReader(client, CatalogApps{})).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")})
			if tc.wantCode == 0 {
				if err != nil {
					t.Fatalf("Create() error = %v, want none", err)
				}
				return
			}
			if got := connect.CodeOf(err); got != tc.wantCode {
				t.Fatalf("Create() code = %v (%v), want %v", got, err, tc.wantCode)
			}
			for _, text := range tc.wantText {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("Create() error %q does not name %q", err, text)
				}
			}
			if msgs := writer.Messages(); len(msgs) != 0 {
				t.Errorf("Create() committed %v, want nothing written", msgs)
			}
		})
	}
}

// When the Applications can't be listed for a reason other than a missing
// Argo CD or RBAC, the create is refused as retryable rather than written
// unchecked.
func TestModelsGitCreateSharedResourceCheckUnavailable(t *testing.T) {
	t.Parallel()
	client := newAppClient(managedCluster("site-a", map[string]string{"location": "us-east"}, true))
	client.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	writer := gitops.NewMemoryWriter(nil)
	srv, err := NewModelsServer().SetLogger(slog.New(slog.DiscardHandler)).
		SetWriter(writer).SetDynamicClient(client).
		SetCatalogStatus(NewCatalogStatusReader(client, CatalogApps{})).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	_, err = srv.Create(context.Background(), &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")})
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("Create() code = %v (%v), want Unavailable", got, err)
	}
	if msgs := writer.Messages(); len(msgs) != 0 {
		t.Errorf("Create() committed %v, want nothing written", msgs)
	}
}
