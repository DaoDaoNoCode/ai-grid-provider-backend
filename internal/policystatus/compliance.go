// Package policystatus parses ACM ConfigurationPolicy compliance messages
// into per-object ready reports. It stays free of Kubernetes clients so
// CatalogStatusReader can own I/O while this package owns message shape.
package policystatus

import (
	"regexp"
	"strings"
)

// complianceClauseRE matches one clause of a ConfigurationPolicy message,
// such as "violation - llminferenceservices [a, b] found but not as specified
// in namespace models", "violation - maassubscriptions [c] not found in
// namespace maas" or "notification - maasauthpolicies [d] found as specified
// in namespace maas". The controller writes "<resource> [<names>] found as
// specified", "... found but not as specified" and "... not found" for a
// musthave object template (createStatus and generateMessageWithReason in
// open-cluster-management-io/config-policy-controller,
// controllers/configurationpolicy_utils.go, at 9aeeb71).
var complianceClauseRE = regexp.MustCompile(`^(?:(?:notification|violation) - )?[a-z0-9.]+ \[([^\]]*)\] (?:found (but not )?as specified|(not found))\b`)

// ParseCompliance reads the objects a ConfigurationPolicy message names. It
// returns false when the message says nothing usable: empty, a template
// failure, or no clause it recognises. A policy with nothing to check reports
// no objects. Clauses it doesn't recognise, such as the leading
// "NonCompliant", are skipped; an object named both ready and not ready is
// not ready.
func ParseCompliance(message string) (map[string]bool, bool) {
	if message == "" || strings.Contains(message, "failed to resolve the template") {
		return nil, false
	}
	if strings.Contains(message, "contains no object templates to check") {
		return map[string]bool{}, true
	}
	objects := map[string]bool{}
	recognised := false
	for _, clause := range strings.Split(message, "; ") {
		m := complianceClauseRE.FindStringSubmatch(strings.TrimSpace(clause))
		if m == nil {
			continue
		}
		recognised = true
		ready := m[2] == "" && m[3] == ""
		for _, name := range strings.Split(m[1], ", ") {
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			if previous, seen := objects[name]; !seen || previous {
				objects[name] = ready
			}
		}
	}
	return objects, recognised
}
