package policystatus

import (
	"maps"
	"testing"
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
			got, ok := ParseCompliance(tc.message)
			if ok != tc.wantOK || !maps.Equal(got, tc.want) {
				t.Fatalf("ParseCompliance() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
