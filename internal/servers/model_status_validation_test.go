package servers

import (
	"testing"

	"buf.build/go/protovalidate"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/proto"
)

func TestModelStatusSummaryValidation(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]*aigridv1.ModelStatus{
		"unknown deployment": {Deployment: 99},
		"unknown MaaS state": {Maas: 99},
		"unknown rollout":    {State: 99},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := protovalidate.Validate(status); err == nil {
				t.Errorf("Validate(%v) succeeded, want an undefined enum violation", status)
			}
		})
	}
	if err := protovalidate.Validate(&aigridv1.ModelStatus{}); err != nil {
		t.Errorf("Validate(unreported status) = %v, want none", err)
	}
}

func TestModelClusterViewValidation(t *testing.T) {
	t.Parallel()
	const console = aigridv1.ModelViewTarget_MODEL_VIEW_TARGET_OPENSHIFT_CONSOLE
	for name, tc := range map[string]struct {
		url     string
		target  aigridv1.ModelViewTarget
		wantErr bool
	}{
		"no link":             {},
		"console link":        {url: "https://console.example.org/k8s/ns/models/granite", target: console},
		"dashboard link":      {url: "https://ai.example.org/models", target: aigridv1.ModelViewTarget_MODEL_VIEW_TARGET_OPENSHIFT_AI},
		"HTTP":                {url: "http://console.example.org", target: console, wantErr: true},
		"script":              {url: "javascript:alert(1)", target: console, wantErr: true},
		"missing host":        {url: "https:///models", target: console, wantErr: true},
		"user info":           {url: "https://user@console.example.org", target: console, wantErr: true},
		"malformed escape":    {url: "https://console.example.org/%zz", target: console, wantErr: true},
		"link without target": {url: "https://console.example.org", wantErr: true},
		"target without link": {target: console, wantErr: true},
		"undefined target":    {url: "https://console.example.org", target: 99, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := &aigridv1.ModelClusterStatus{
				Cluster: "site-a", Phase: aigridv1.ModelPhase_MODEL_PHASE_READY, Reason: "Ready",
				ViewUrl: tc.url, ViewTarget: tc.target,
			}
			if err := protovalidate.Validate(status); (err != nil) != tc.wantErr {
				t.Errorf("Validate(%v) = %v, want error %t", status, err, tc.wantErr)
			}
		})
	}
}

func TestModelStatusSummaryOnlyValues(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		message proto.Message
		wantErr bool
	}{
		"partial cluster": {
			message: &aigridv1.ModelClusterStatus{Cluster: "site-a", Phase: aigridv1.ModelPhase_MODEL_PHASE_READY,
				Reason: "Ready", State: aigridv1.ModelState_MODEL_STATE_PARTIALLY_READY},
			wantErr: true,
		},
		"unavailable ingress": {
			message: &aigridv1.ModelIngressStatus{Cluster: "site-a", State: aigridv1.ModelMaasState_MODEL_MAAS_STATE_UNAVAILABLE},
			wantErr: true,
		},
		"model summary": {
			message: &aigridv1.ModelStatus{State: aigridv1.ModelState_MODEL_STATE_PARTIALLY_READY,
				Maas: aigridv1.ModelMaasState_MODEL_MAAS_STATE_UNAVAILABLE},
		},
		"unreported cluster": {
			message: &aigridv1.ModelClusterStatus{Cluster: "site-a", Phase: aigridv1.ModelPhase_MODEL_PHASE_PENDING, Reason: "NoReport"},
		},
		"unreported ingress": {
			message: &aigridv1.ModelIngressStatus{Cluster: "site-a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := protovalidate.Validate(tc.message); (err != nil) != tc.wantErr {
				t.Errorf("Validate(%v) = %v, want error %t", tc.message, err, tc.wantErr)
			}
		})
	}
}
