package deploycheck

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The backend's ServiceAccount writes nothing but ManagedClusters: Models and
// Subscriptions commit to Git. These are the only rules a write verb may
// appear in, by API group and resource.
var allowedWrites = map[string][]string{
	"cluster.open-cluster-management.io":  {"managedclusters", "managedclustersets/join"},
	"register.open-cluster-management.io": {"managedclusters/accept"},
}

var readVerbs = []string{"get", "list", "watch"}

// TestRBACWritesOnlyManagedClusters parses every ClusterRole and Role in the
// deployment manifests and fails on a write verb outside the ManagedCluster
// rules, or a wildcard.
func TestRBACWritesOnlyManagedClusters(t *testing.T) {
	t.Parallel()
	files := []string{
		filepath.Join("..", "..", "deploy", "openshift", "rbac.yaml"),
		filepath.Join("..", "..", "deploy", "kind", "ai-grid-provider-backend.yaml"),
	}
	for _, file := range files {
		roles := readRoles(t, file)
		if len(roles) == 0 {
			t.Fatalf("%s: no ClusterRole or Role found", file)
		}
		for _, role := range roles {
			for i, rule := range role.Rules {
				for _, verb := range rule.Verbs {
					if slices.Contains(readVerbs, verb) {
						continue
					}
					if verb == "*" || !writeAllowed(rule) {
						t.Errorf("%s: %s rule %d grants %q on %v in %v; only ManagedCluster rules may write",
							file, role.Name, i, verb, rule.Resources, rule.APIGroups)
					}
				}
			}
		}
	}
}

// writeAllowed reports whether every group and resource the rule names is one
// that may be written.
func writeAllowed(rule rbacv1.PolicyRule) bool {
	if len(rule.APIGroups) == 0 || len(rule.Resources) == 0 {
		return false
	}
	for _, group := range rule.APIGroups {
		for _, resource := range rule.Resources {
			if !slices.Contains(allowedWrites[group], resource) {
				return false
			}
		}
	}
	return true
}

type role struct {
	Name  string
	Rules []rbacv1.PolicyRule
}

func readRoles(t *testing.T, file string) []role {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var out []role
	for _, doc := range bytes.Split(data, []byte("\n---")) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		switch head.Kind {
		case "ClusterRole":
			var r rbacv1.ClusterRole
			if err := yaml.UnmarshalStrict(doc, &r); err != nil {
				t.Fatalf("%s: ClusterRole: %v", file, err)
			}
			out = append(out, role{Name: r.Name, Rules: r.Rules})
		case "Role":
			var r rbacv1.Role
			if err := yaml.UnmarshalStrict(doc, &r); err != nil {
				t.Fatalf("%s: Role: %v", file, err)
			}
			out = append(out, role{Name: r.Name, Rules: r.Rules})
		}
	}
	return out
}

func TestIdpSecretRBAC(t *testing.T) {
	t.Parallel()
	files := []string{
		filepath.Join("..", "..", "deploy", "openshift", "rbac.yaml"),
		filepath.Join("..", "..", "deploy", "kind", "ai-grid-provider-backend.yaml"),
	}
	for _, file := range files {
		roles := readRoles(t, file)
		var found bool
		for _, role := range roles {
			if role.Name != "ai-grid-provider-backend-idp-secrets" {
				continue
			}
			found = true
			if len(role.Rules) != 1 {
				t.Fatalf("%s: idp Role rules = %d, want 1", file, len(role.Rules))
			}
			rule := role.Rules[0]
			if !slices.Equal(rule.APIGroups, []string{""}) || !slices.Equal(rule.Resources, []string{"secrets"}) || !slices.Equal(rule.Verbs, []string{"get"}) {
				t.Fatalf("%s: idp Role rule = %+v, want get on secrets", file, rule)
			}
			for _, verb := range []string{"list", "watch", "create", "update", "patch", "delete", "*"} {
				if slices.Contains(rule.Verbs, verb) {
					t.Fatalf("%s: idp Role must not grant %q", file, verb)
				}
			}
		}
		if !found {
			t.Fatalf("%s: missing Role ai-grid-provider-backend-idp-secrets", file)
		}
		assertIdpSecretRoleBinding(t, file)
	}
}

func assertIdpSecretRoleBinding(t *testing.T, file string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var found bool
	for _, doc := range bytes.Split(data, []byte("\n---")) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil || head.Kind != "RoleBinding" {
			continue
		}
		var rb rbacv1.RoleBinding
		if err := yaml.UnmarshalStrict(doc, &rb); err != nil {
			t.Fatalf("%s: RoleBinding: %v", file, err)
		}
		if rb.Name != "ai-grid-provider-backend-idp-secrets" {
			continue
		}
		found = true
		if rb.Namespace != "ai-grid-idp-secrets" {
			t.Fatalf("%s: RoleBinding namespace = %q, want ai-grid-idp-secrets", file, rb.Namespace)
		}
		if rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != "ai-grid-provider-backend-idp-secrets" {
			t.Fatalf("%s: RoleBinding roleRef = %+v", file, rb.RoleRef)
		}
		if len(rb.Subjects) != 1 || rb.Subjects[0].Kind != "ServiceAccount" || rb.Subjects[0].Name != "ai-grid-provider-backend" {
			t.Fatalf("%s: RoleBinding subjects = %+v", file, rb.Subjects)
		}
	}
	if !found {
		t.Fatalf("%s: missing RoleBinding ai-grid-provider-backend-idp-secrets", file)
	}
}

// The test fails on the rules cluster mode needed.
func TestWriteAllowedRejectsClusterModeRules(t *testing.T) {
	t.Parallel()
	for _, rule := range []rbacv1.PolicyRule{
		{APIGroups: []string{"maas.opendatahub.io"}, Resources: []string{"maassubscriptions"}, Verbs: []string{"create"}},
		{APIGroups: []string{"serving.kserve.io"}, Resources: []string{"llminferenceservices"}, Verbs: []string{"patch"}},
		{APIGroups: []string{"cluster.open-cluster-management.io"}, Resources: []string{"managedclusters", "placements"}, Verbs: []string{"delete"}},
		{APIGroups: []string{"*"}, Resources: []string{"managedclusters"}, Verbs: []string{"update"}},
	} {
		if writeAllowed(rule) {
			t.Errorf("writeAllowed(%v) = true", rule)
		}
	}
	if !writeAllowed(rbacv1.PolicyRule{APIGroups: []string{"cluster.open-cluster-management.io"}, Resources: []string{"managedclusters"}}) {
		t.Error("writeAllowed(managedclusters) = false")
	}
}
