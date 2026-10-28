package servers

import (
	"context"
	"errors"
	"maps"
	"path"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestModelsGitResources checks spec.main.requests and limits on Create.
// Kubernetes refuses a container whose quantities don't parse or are
// negative, whose extended resource (nvidia.com/gpu) isn't a whole number, or
// whose request exceeds its limit, and requires a limit equal to the request
// for the resources it can't overcommit: extended resources and hugepages
// (validateResourceRequirements in kubernetes/kubernetes
// pkg/apis/core/validation/validation.go at v1.35.8). The server refuses
// those, committing nothing, and fills a missing limit of such a resource
// with its request, as the console's form did. buf.validate on
// ModelContainer declares what one value can be checked for (a non-negative
// quantity), and protovalidate must refuse exactly the cases marked
// annotation.
func TestModelsGitResources(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		requests, limits map[string]string
		// wantLimits is the limits the model is saved with; nil when the
		// create is refused.
		wantLimits map[string]string
		// annotation: the buf.validate rules refuse it too.
		annotation bool
		// wantNamed is in the message of a refusal.
		wantNamed string
	}{
		"cpu and memory": {
			requests:   map[string]string{"cpu": "100m", "memory": "8Gi"},
			limits:     map[string]string{"cpu": "1", "memory": "10Gi"},
			wantLimits: map[string]string{"cpu": "1", "memory": "10Gi"},
		},
		"no resources": {wantLimits: map[string]string{}},
		"GPU request only": {
			requests:   map[string]string{"cpu": "2", "memory": "4Gi", "nvidia.com/gpu": "1"},
			wantLimits: map[string]string{"nvidia.com/gpu": "1"},
		},
		"GPU request only, with a cpu limit": {
			requests:   map[string]string{"cpu": "2", "nvidia.com/gpu": "2"},
			limits:     map[string]string{"cpu": "4"},
			wantLimits: map[string]string{"cpu": "4", "nvidia.com/gpu": "2"},
		},
		"GPU request and limit equal": {
			requests:   map[string]string{"nvidia.com/gpu": "1"},
			limits:     map[string]string{"nvidia.com/gpu": "1"},
			wantLimits: map[string]string{"nvidia.com/gpu": "1"},
		},
		"GPU request and limit equal in other units": {
			requests:   map[string]string{"nvidia.com/gpu": "1"},
			limits:     map[string]string{"nvidia.com/gpu": "1000m"},
			wantLimits: map[string]string{"nvidia.com/gpu": "1000m"},
		},
		"GPU limit only": {
			limits:     map[string]string{"nvidia.com/gpu": "1"},
			wantLimits: map[string]string{"nvidia.com/gpu": "1"},
		},
		"zero GPUs": {
			requests:   map[string]string{"nvidia.com/gpu": "0"},
			wantLimits: map[string]string{"nvidia.com/gpu": "0"},
		},
		"another vendor's GPU request only": {
			requests:   map[string]string{"amd.com/gpu": "1"},
			wantLimits: map[string]string{"amd.com/gpu": "1"},
		},
		"hugepages request only": {
			requests:   map[string]string{"memory": "1Gi", "hugepages-2Mi": "100Mi"},
			wantLimits: map[string]string{"hugepages-2Mi": "100Mi"},
		},
		"GPU request differs from its limit": {
			requests:  map[string]string{"nvidia.com/gpu": "1"},
			limits:    map[string]string{"nvidia.com/gpu": "2"},
			wantNamed: "nvidia.com/gpu",
		},
		"GPU limit below its request": {
			requests:  map[string]string{"nvidia.com/gpu": "2"},
			limits:    map[string]string{"nvidia.com/gpu": "1"},
			wantNamed: "nvidia.com/gpu",
		},
		"fractional GPU": {
			requests:  map[string]string{"nvidia.com/gpu": "0.5"},
			wantNamed: "nvidia.com/gpu",
		},
		"fractional GPU limit": {
			limits:    map[string]string{"nvidia.com/gpu": "500m"},
			wantNamed: "nvidia.com/gpu",
		},
		"negative GPU": {
			requests:   map[string]string{"nvidia.com/gpu": "-1"},
			annotation: true, wantNamed: "nvidia.com/gpu",
		},
		"negative cpu": {
			requests:   map[string]string{"cpu": "-1"},
			annotation: true, wantNamed: "cpu",
		},
		"negative zero": {
			limits:     map[string]string{"cpu": "-0"},
			annotation: true, wantNamed: "cpu",
		},
		"memory that doesn't parse": {
			requests:   map[string]string{"memory": "lots"},
			annotation: true, wantNamed: "memory",
		},
		"two decimal points": {
			requests:  map[string]string{"cpu": "1.2.3"},
			wantNamed: "cpu",
		},
		"empty quantity": {
			limits:     map[string]string{"memory": ""},
			annotation: true, wantNamed: "memory",
		},
		"empty resource name": {
			requests:   map[string]string{"": "1"},
			annotation: true,
		},
		"cpu request above its limit": {
			requests:  map[string]string{"cpu": "2"},
			limits:    map[string]string{"cpu": "1"},
			wantNamed: "cpu",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ok := tc.wantLimits != nil
			main := &aigridv1.ModelContainer{Image: "quay.io/example/vllm:latest", Requests: maps.Clone(tc.requests), Limits: maps.Clone(tc.limits)}
			err := protovalidate.Validate(main)
			var verr *protovalidate.ValidationError
			switch {
			case tc.annotation && !errors.As(err, &verr):
				// A compile or runtime error is not a refusal.
				t.Fatalf("protovalidate(%v) error = %v, want a ValidationError", main, err)
			case !tc.annotation && err != nil:
				t.Fatalf("protovalidate(%v) error = %v, want none", main, err)
			}

			ctx := context.Background()
			writer := gitops.NewMemoryWriter(nil)
			srv := mustGitModelsServer(t, writer)
			model := gitSampleModel("res", "site-a")
			model.Spec.Main = main
			created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model})
			if !ok {
				assertConnectCode(t, err, connect.CodeInvalidArgument)
				if !strings.Contains(err.Error(), `"`+tc.wantNamed+`"`) {
					t.Fatalf("Create() error = %v, want it to name %q", err, tc.wantNamed)
				}
				if n := len(writer.Messages()); n != 0 {
					t.Fatalf("Create() committed %d times, want none", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "res"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			for op, m := range map[string]*aigridv1.Model{"Create": created.GetObject(), "Get": got.GetObject()} {
				if limits := m.GetSpec().GetMain().GetLimits(); !maps.Equal(limits, tc.wantLimits) {
					t.Errorf("%s spec.main.limits = %v, want %v", op, limits, tc.wantLimits)
				}
				if requests := m.GetSpec().GetMain().GetRequests(); !maps.Equal(requests, tc.requests) {
					t.Errorf("%s spec.main.requests = %v, want %v", op, requests, tc.requests)
				}
			}
			if limits := committedLimits(t, writer, "res"); !maps.Equal(limits, tc.wantLimits) {
				t.Errorf("committed limits = %v, want %v", limits, tc.wantLimits)
			}
		})
	}
}

// TestModelsGitResourcesUpdate checks the same rules on Update, on the
// merged spec. A mask with spec.main.requests and not spec.main.limits keeps
// the saved limits, except the limit of a resource that can't be
// overcommitted whose saved limit equalled its saved request: that limit came
// from the request (normalizeModelResources), so it follows the new request.
// The GPU count can change, and removing the GPU request removes its limit.
// A mask with both maps, or spec.main, replaces them as sent.
func TestModelsGitResourcesUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)
	model := gitSampleModel("res", "site-a")
	model.Spec.Main.Requests = map[string]string{"cpu": "2", "nvidia.com/gpu": "1"}
	model.Spec.Main.Limits = map[string]string{"cpu": "4"}
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	update := func(requests, limits map[string]string, paths ...string) (*aigridv1.ModelsUpdateResponse, error) {
		object := gitSampleModel("res", "site-a")
		object.Spec.Main.Requests = requests
		object.Spec.Main.Limits = limits
		return srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: object, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	check := func(op string, got *aigridv1.ModelsUpdateResponse, err error, wantLimits map[string]string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s error = %v", op, err)
		}
		if limits := got.GetObject().GetSpec().GetMain().GetLimits(); !maps.Equal(limits, wantLimits) {
			t.Fatalf("%s limits = %v, want %v", op, limits, wantLimits)
		}
		if limits := committedLimits(t, writer, "res"); !maps.Equal(limits, wantLimits) {
			t.Fatalf("%s committed limits = %v, want %v", op, limits, wantLimits)
		}
	}

	// The GPU count changes; the cpu limit the caller set stays.
	got, err := update(map[string]string{"cpu": "2", "nvidia.com/gpu": "2"}, nil, "spec.main.requests")
	check("Update(requests: 2 GPUs)", got, err, map[string]string{"cpu": "4", "nvidia.com/gpu": "2"})
	// Removing the GPU request removes the GPU, not just the request.
	got, err = update(map[string]string{"cpu": "2"}, nil, "spec.main.requests")
	check("Update(requests: no GPU)", got, err, map[string]string{"cpu": "4"})
	got, err = update(map[string]string{}, nil, "object.spec.main.requests")
	check("Update(requests: none)", got, err, map[string]string{"cpu": "4"})

	got, err = update(map[string]string{"nvidia.com/gpu": "2"}, map[string]string{"nvidia.com/gpu": "2"}, "spec.main.requests", "spec.main.limits")
	check("Update(requests and limits)", got, err, map[string]string{"nvidia.com/gpu": "2"})
	got, err = update(map[string]string{"nvidia.com/gpu": "4"}, nil, "spec.main")
	check("Update(spec.main)", got, err, map[string]string{"nvidia.com/gpu": "4"})

	// A GPU limit with no request (Kubernetes defaults the request to it)
	// didn't come from a request, so a requests-only update keeps it.
	got, err = update(nil, map[string]string{"nvidia.com/gpu": "1"}, "spec.main")
	check("Update(spec.main: GPU limit only)", got, err, map[string]string{"nvidia.com/gpu": "1"})
	got, err = update(map[string]string{"cpu": "1"}, nil, "spec.main.requests")
	check("Update(requests: cpu, GPU limit only)", got, err, map[string]string{"nvidia.com/gpu": "1"})

	// A limits-only update is checked against the saved requests.
	if _, err := update(map[string]string{"nvidia.com/gpu": "1"}, map[string]string{"nvidia.com/gpu": "1"}, "spec.main"); err != nil {
		t.Fatalf("Update(spec.main: 1 GPU) error = %v", err)
	}
	commits := len(writer.Messages())
	_, err = update(nil, map[string]string{"nvidia.com/gpu": "2"}, "spec.main.limits")
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if !strings.Contains(err.Error(), `"nvidia.com/gpu"`) {
		t.Fatalf("Update(limits only) error = %v, want it to name nvidia.com/gpu", err)
	}
	if n := len(writer.Messages()); n != commits {
		t.Fatalf("Update(limits only) commits = %d, want %d", n, commits)
	}
}

// committedLimits reads the main container's limits from the model's
// committed LLMInferenceService.
func committedLimits(t *testing.T, writer gitops.Writer, id string) map[string]string {
	t.Helper()
	docs := readGitYAMLDocs(t, writer, path.Join(modelDeploymentGitDir(id), "deployment.yaml"))
	containers, _, err := unstructured.NestedSlice(docs[0].Object, "spec", "template", "containers")
	if err != nil || len(containers) == 0 {
		t.Fatalf("committed containers = %v, %v", containers, err)
	}
	limits, _, err := unstructured.NestedStringMap(containers[0].(map[string]interface{}), "resources", "limits")
	if err != nil {
		t.Fatalf("committed limits: %v", err)
	}
	if limits == nil {
		limits = map[string]string{}
	}
	return limits
}
