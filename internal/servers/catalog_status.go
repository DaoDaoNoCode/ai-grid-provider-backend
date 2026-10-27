package servers

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// DefaultPolicyNamespace is the hub namespace that holds the inform-only ACM
// Policies git mode reads status from (AI_GRID_POLICY_NAMESPACE).
const DefaultPolicyNamespace = "ai-grid-policies"

// CatalogStatusReader reads what spoke clusters report about the objects git
// mode delivers, from inform-only ACM Policies on the hub. Each Policy holds
// ConfigurationPolicies that check one kind of object each; ACM lists the
// clusters it reports for in the root Policy's status.status, and copies the
// Policy into each cluster's namespace on the hub as <namespace>.<policy>,
// where status.details[] holds each ConfigurationPolicy's latest message.
type CatalogStatusReader struct {
	client    dynamic.Interface
	namespace string
}

// NewCatalogStatusReader returns a reader for the Policies in namespace, or
// DefaultPolicyNamespace when it is empty.
func NewCatalogStatusReader(client dynamic.Interface, namespace string) *CatalogStatusReader {
	if namespace == "" {
		namespace = DefaultPolicyNamespace
	}
	return &CatalogStatusReader{client: client, namespace: namespace}
}

// ObjectReports maps each cluster that reported to the objects its
// ConfigurationPolicy named: true when found as specified (ready), false when
// found but not as specified. A cluster that is absent has no report.
type ObjectReports map[string]map[string]bool

// Read returns the reports of the ConfigurationPolicy check in Policy policy.
// A Policy or cluster copy that is missing or forbidden (the Policies aren't
// deployed, or the ServiceAccount can't read them) gives no report for it
// rather than an error, so git mode works without them. A nil reader reports
// nothing. Read calls the hub on every request: the root plus one copy per
// cluster, which is fine for the handful of clusters a grid has.
func (r *CatalogStatusReader) Read(ctx context.Context, policy, check string) (ObjectReports, error) {
	if r == nil {
		return nil, nil
	}
	policies := r.client.Resource(policiesResource)
	root, err := policies.Namespace(r.namespace).Get(ctx, policy, metav1.GetOptions{})
	if statusUnavailable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Policy %s/%s: %w", r.namespace, policy, err)
	}
	out := ObjectReports{}
	entries, _, _ := unstructured.NestedSlice(root.Object, "status", "status")
	for _, raw := range entries {
		entry, _ := raw.(map[string]interface{})
		cluster, _ := entry["clustername"].(string)
		namespace, _ := entry["clusternamespace"].(string)
		if cluster == "" {
			continue
		}
		if namespace == "" {
			namespace = cluster
		}
		copyName := r.namespace + "." + policy
		replica, err := policies.Namespace(namespace).Get(ctx, copyName, metav1.GetOptions{})
		if statusUnavailable(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read Policy %s/%s: %w", namespace, copyName, err)
		}
		if objects, ok := parseCompliance(latestMessage(*replica, check)); ok {
			out[cluster] = objects
		}
	}
	return out, nil
}

func statusUnavailable(err error) bool {
	return apierrors.IsNotFound(err) || apierrors.IsForbidden(err)
}

// latestMessage returns the newest history message of ConfigurationPolicy
// check in a cluster's copy of a Policy, or "" when it has none.
func latestMessage(replica unstructured.Unstructured, check string) string {
	details, _, _ := unstructured.NestedSlice(replica.Object, "status", "details")
	for _, raw := range details {
		detail, _ := raw.(map[string]interface{})
		if name, _, _ := unstructured.NestedString(detail, "templateMeta", "name"); name != check {
			continue
		}
		history, _, _ := unstructured.NestedSlice(detail, "history")
		if len(history) == 0 {
			return ""
		}
		latest, _ := history[0].(map[string]interface{})
		message, _ := latest["message"].(string)
		return message
	}
	return ""
}

// complianceClauseRE matches one clause of a ConfigurationPolicy message,
// such as "violation - llminferenceservices [a, b] found but not as specified
// in namespace models".
var complianceClauseRE = regexp.MustCompile(`^(?:(?:notification|violation) - )?[a-z0-9.]+ \[([^\]]*)\] found (but not )?as specified\b`)

// parseCompliance reads the objects a ConfigurationPolicy message names. It
// returns false when the message says nothing usable: empty, a template
// failure, or no clause it recognises. A policy with nothing to check reports
// no objects. Clauses it doesn't recognise, such as the leading
// "NonCompliant", are skipped; an object named both ready and not ready is
// not ready.
func parseCompliance(message string) (map[string]bool, bool) {
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
		ready := m[2] == ""
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
