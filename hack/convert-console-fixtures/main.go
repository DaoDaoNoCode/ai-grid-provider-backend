// Command convert-console-fixtures converts a fixtures directory of the
// console's fake backend (ai-grid-cloud/ai-grid-provider-console,
// mock/fixtures or mock/fixtures-poc) into the files the fake hub and the
// fake Keycloak of internal/fakeinfra read. The console's cluster and tenant
// fixtures are protojson of its draft ai_grid.v1 proto, which mirrors the
// hub's ManagedCluster and Keycloak's Organization field by field; this
// program maps them back to the real objects. fixtures/README.md says how to
// run it.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/hub"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/keycloak"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func main() {
	src := flag.String("src", "", "console fixtures directory (mock/fixtures or mock/fixtures-poc of an export of the console repository)")
	out := flag.String("out", "", "output directory (fixtures/default or fixtures/poc)")
	flag.Parse()
	if *src == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: convert-console-fixtures --src <console fixtures dir> --out <dir>")
		os.Exit(2)
	}
	if err := convert(*src, *out); err != nil {
		fmt.Fprintf(os.Stderr, "convert-console-fixtures: %v\n", err)
		os.Exit(1)
	}
}

func convert(src, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	roles, err := statusPolicyRoles(filepath.Join(out, hub.PoliciesFile))
	if err != nil {
		return fmt.Errorf("%s: %w", hub.PoliciesFile, err)
	}
	steps := []struct {
		file string
		fn   func([]byte) (any, error)
		dst  string
	}{
		{"managedclusters.json", func(raw []byte) (any, error) { return convertClusters(raw, roles) }, hub.ManagedClustersFile},
		{"tenants.json", convertTenants, keycloak.OrganizationsFile},
		{"access.json", convertAccess, hub.AccessFile},
		{"scenarios.json", convertScenarios, hub.ScenariosFile},
		{"settings.json", convertSettings, hub.SettingsFile},
	}
	for _, s := range steps {
		raw, err := os.ReadFile(filepath.Join(src, s.file))
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(filepath.Join(out, s.dst)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		v, err := s.fn(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", s.file, err)
		}
		if err := writeJSON(filepath.Join(out, s.dst), v); err != nil {
			return err
		}
	}
	return nil
}

// decodeStrict fails on any field the target does not know, so a console
// fixture field this program does not map is an error, not a silent loss.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// int64String is a protojson int64, which is a JSON string (or a number).
type int64String int64

func (i *int64String) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		s = string(b)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*i = int64String(v)
	return err
}

func parseTime(s string) (metav1.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	return metav1.NewTime(t.UTC()), err
}

// The console's ai_grid.v1.ManagedCluster in protojson
// (mock/proto/ai_grid/v1/managed_cluster_type.proto).
type pbCluster struct {
	Name                        string            `json:"name"`
	Labels                      map[string]string `json:"labels"`
	HubAcceptsClient            bool              `json:"hubAcceptsClient"`
	Annotations                 map[string]string `json:"annotations"`
	UID                         string            `json:"uid"`
	ResourceVersion             string            `json:"resourceVersion"`
	CreationTimestamp           string            `json:"creationTimestamp"`
	DeletionTimestamp           string            `json:"deletionTimestamp"`
	LeaseDurationSeconds        *int32            `json:"leaseDurationSeconds"`
	ManagedClusterClientConfigs []struct {
		URL      string `json:"url"`
		CABundle []byte `json:"caBundle"`
	} `json:"managedClusterClientConfigs"`
	Status *pbStatus `json:"status"`
	Taints []struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		Effect    string `json:"effect"`
		TimeAdded string `json:"timeAdded"`
	} `json:"taints"`
	Generation int64String `json:"generation"`
	Finalizers []string    `json:"finalizers"`
}

type pbStatus struct {
	State             string            `json:"state"`
	StateMessage      string            `json:"stateMessage"`
	Conditions        []pbCondition     `json:"conditions"`
	KubernetesVersion string            `json:"kubernetesVersion"`
	Capacity          map[string]string `json:"capacity"`
	Allocatable       map[string]string `json:"allocatable"`
	ClusterClaims     []pbClaim         `json:"clusterClaims"`
}

type pbClaim struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type pbCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	LastTransitionTime string      `json:"lastTransitionTime"`
	Reason             string      `json:"reason"`
	Message            string      `json:"message"`
	ObservedGeneration int64String `json:"observedGeneration"`
}

// conditionStatus maps the draft proto's ConditionStatus back to Kubernetes:
// the proto reports `Unknown` as UNSPECIFIED, which protojson omits.
func conditionStatus(s string) (metav1.ConditionStatus, error) {
	switch s {
	case "CONDITION_STATUS_TRUE":
		return metav1.ConditionTrue, nil
	case "CONDITION_STATUS_FALSE":
		return metav1.ConditionFalse, nil
	case "", "CONDITION_STATUS_UNSPECIFIED":
		return metav1.ConditionUnknown, nil
	}
	return "", fmt.Errorf("unknown condition status %q", s)
}

func convertClaims(in []pbClaim) []hub.ManagedClusterClaim {
	var out []hub.ManagedClusterClaim
	for _, c := range in {
		out = append(out, hub.ManagedClusterClaim{Name: c.Name, Value: c.Value})
	}
	return out
}

// The status Policies of policies.json (hand-authored, not converted) and the
// ai-grid/role each one covers. The hub's GitOps binds ai-grid-models-status
// to the inference and combined sites and ai-grid-catalog-status to the
// ingress and combined ones (kahowell/ai-grid-tenant,
// apps/ai-grid-policies/manifests/10-catalog-status.yaml).
const (
	statusPolicyNamespace = "ai-grid-policies"
	modelsStatusPolicy    = "ai-grid-models-status"
	catalogStatusPolicy   = "ai-grid-catalog-status"
	labelEnabled          = "ai-grid/enabled"
	labelRole             = "ai-grid/role"
)

// statusPolicyRoles reads the root status Policies of the policies.json at
// path and answers the role of each cluster they report on: inference for
// ai-grid-models-status only, ingress for ai-grid-catalog-status only,
// combined for both. Without the file it answers none.
func statusPolicyRoles(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Status struct {
				Status []struct {
					ClusterName string `json:"clustername"`
				} `json:"status"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	inference, ingress := map[string]bool{}, map[string]bool{}
	for _, p := range doc.Items {
		if p.Metadata.Namespace != statusPolicyNamespace {
			continue
		}
		var covers map[string]bool
		switch p.Metadata.Name {
		case modelsStatusPolicy:
			covers = inference
		case catalogStatusPolicy:
			covers = ingress
		default:
			continue
		}
		for _, s := range p.Status.Status {
			covers[s.ClusterName] = true
		}
	}
	roles := map[string]string{}
	for name := range inference {
		roles[name] = "inference"
	}
	for name := range ingress {
		if inference[name] {
			roles[name] = "combined"
		} else {
			roles[name] = "ingress"
		}
	}
	return roles, nil
}

// convertClusters converts the console's clusters. An AI Grid member a status
// Policy reports on gets the ai-grid/role that Policy covers (roles), so the
// Policies and the labels agree: the backend reads a member with no role as
// combined, which both Policies would have to report on.
func convertClusters(raw []byte, roles map[string]string) (any, error) {
	var doc struct {
		Comment  string      `json:"_comment"`
		Clusters []pbCluster `json:"clusters"`
	}
	if err := decodeStrict(raw, &doc); err != nil {
		return nil, err
	}
	list := hub.ManagedClusterList{
		TypeMeta: metav1.TypeMeta{APIVersion: hub.APIVersion, Kind: hub.ListKind},
		Items:    []hub.ManagedCluster{},
	}
	for _, pc := range doc.Clusters {
		c, err := convertCluster(pc)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", pc.Name, err)
		}
		if role := roles[c.Name]; role != "" && c.Labels[labelEnabled] == "true" {
			if _, set := c.Labels[labelRole]; !set {
				c.Labels[labelRole] = role
			}
		}
		list.Items = append(list.Items, c)
	}
	return list, nil
}

func convertCluster(pc pbCluster) (hub.ManagedCluster, error) {
	created, err := parseTime(pc.CreationTimestamp)
	if err != nil {
		return hub.ManagedCluster{}, fmt.Errorf("creationTimestamp: %w", err)
	}
	c := hub.ManagedCluster{
		TypeMeta: metav1.TypeMeta{APIVersion: hub.APIVersion, Kind: hub.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name: pc.Name, UID: types.UID(pc.UID), ResourceVersion: pc.ResourceVersion,
			Generation: int64(pc.Generation), CreationTimestamp: created,
			Labels: pc.Labels, Annotations: pc.Annotations, Finalizers: pc.Finalizers,
		},
		Spec: hub.ManagedClusterSpec{HubAcceptsClient: pc.HubAcceptsClient, LeaseDurationSeconds: pc.LeaseDurationSeconds},
	}
	if pc.DeletionTimestamp != "" {
		ts, err := parseTime(pc.DeletionTimestamp)
		if err != nil {
			return c, fmt.Errorf("deletionTimestamp: %w", err)
		}
		// The API server sets deletionGracePeriodSeconds 0 when it marks an
		// object with finalizers deleting (registry store markAsDeleting);
		// the draft proto has no field for it.
		var zero int64
		c.DeletionTimestamp, c.DeletionGracePeriodSeconds = &ts, &zero
	}
	for _, cc := range pc.ManagedClusterClientConfigs {
		c.Spec.ManagedClusterClientConfigs = append(c.Spec.ManagedClusterClientConfigs, hub.ClientConfig{URL: cc.URL, CABundle: cc.CABundle})
	}
	for _, t := range pc.Taints {
		added, err := parseTime(t.TimeAdded)
		if err != nil {
			return c, fmt.Errorf("taint %s timeAdded: %w", t.Key, err)
		}
		c.Spec.Taints = append(c.Spec.Taints, hub.Taint{Key: t.Key, Value: t.Value, Effect: t.Effect, TimeAdded: added})
	}
	if pc.Status != nil {
		if pc.Status.State != "" || pc.Status.StateMessage != "" {
			return c, errors.New("status.state and status.stateMessage are derived by the console's backend and have no place in the CR")
		}
		st := &hub.ManagedClusterStatus{
			Version:       hub.ManagedClusterVersion{Kubernetes: pc.Status.KubernetesVersion},
			Capacity:      pc.Status.Capacity,
			Allocatable:   pc.Status.Allocatable,
			ClusterClaims: convertClaims(pc.Status.ClusterClaims),
			Conditions:    []metav1.Condition{},
		}
		for _, pcond := range pc.Status.Conditions {
			status, err := conditionStatus(pcond.Status)
			if err != nil {
				return c, err
			}
			ltt, err := parseTime(pcond.LastTransitionTime)
			if err != nil {
				return c, fmt.Errorf("condition %s: %w", pcond.Type, err)
			}
			st.Conditions = append(st.Conditions, metav1.Condition{
				Type: pcond.Type, Status: status, ObservedGeneration: int64(pcond.ObservedGeneration),
				LastTransitionTime: ltt, Reason: pcond.Reason, Message: pcond.Message,
			})
		}
		c.Status = st
	}
	return c, nil
}

// The console's ai_grid.v1.Tenant in protojson
// (mock/proto/ai_grid/v1/tenant_type.proto).
type pbTenant struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Alias         string            `json:"alias"`
	Enabled       *bool             `json:"enabled"`
	Description   string            `json:"description"`
	RedirectURL   string            `json:"redirectUrl"`
	Attributes    map[string]string `json:"attributes"`
	Domains       []string          `json:"domains"`
	DomainDetails []struct {
		Name                  string `json:"name"`
		Verified              bool   `json:"verified"`
		IdentityProviderAlias string `json:"identityProviderAlias"`
		AutoRedirect          bool   `json:"autoRedirect"`
	} `json:"domainDetails"`
	// Not Keycloak Organization fields: the console's proposals P1 (an
	// organization identity provider), P2 (a version attribute) and P3 (a
	// creation-time attribute) for the backend. Read and dropped.
	IdentityProvider json.RawMessage `json:"identityProvider"`
	ResourceVersion  string          `json:"resourceVersion"`
	CreateTime       string          `json:"createTime"`
}

func convertTenants(raw []byte) (any, error) {
	var doc struct {
		Comment               string     `json:"_comment"`
		Tenants               []pbTenant `json:"tenants"`
		Unavailable           string     `json:"unavailable"`
		RealmMissing          bool       `json:"realmMissing"`
		OrganizationsDisabled bool       `json:"organizationsDisabled"`
	}
	if err := decodeStrict(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Unavailable != "" || doc.RealmMissing || doc.OrganizationsDisabled {
		return nil, errors.New("only the tenants mode has a Keycloak equivalent fixture; run the fake Keycloak with another realm instead")
	}
	orgs := []keycloak.Organization{}
	for _, t := range doc.Tenants {
		org := keycloak.Organization{ID: t.ID, Name: t.Name, Alias: t.Alias, Enabled: t.Enabled}
		if org.Alias == "" {
			org.Alias = org.Name
		}
		if org.Enabled == nil {
			enabled := true
			org.Enabled = &enabled
		}
		if t.Description != "" {
			org.Description = &t.Description
		}
		if t.RedirectURL != "" {
			org.RedirectURL = &t.RedirectURL
		}
		// The console keeps the first value of each Keycloak attribute.
		attrs := map[string][]string{}
		for k, v := range t.Attributes {
			attrs[k] = []string{v}
		}
		org.Attributes = &attrs
		var domains []keycloak.Domain
		for _, d := range t.DomainDetails {
			domains = append(domains, keycloak.Domain{Name: d.Name, Verified: d.Verified, IdentityProviderAlias: d.IdentityProviderAlias, AutoRedirect: d.AutoRedirect})
		}
		if len(t.DomainDetails) == 0 {
			for _, name := range t.Domains {
				domains = append(domains, keycloak.Domain{Name: name})
			}
		} else if names := domainNames(domains); !slices.Equal(names, t.Domains) && len(t.Domains) > 0 {
			return nil, fmt.Errorf("tenant %q: domains %v and domainDetails %v differ", t.Name, t.Domains, names)
		}
		if len(domains) > 0 {
			org.Domains = &domains
		}
		orgs = append(orgs, org)
	}
	return orgs, nil
}

func domainNames(domains []keycloak.Domain) []string {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		out = append(out, d.Name)
	}
	return out
}

const accessComment = "Who the fake hub accepts. A request without a bearer token or with one of rejectedTokens is 401 Unauthorized, like an expired or revoked token on the hub. Every other token is a user: one of deniedTokens gets allowed: false from SelfSubjectAccessReview and 403 Forbidden on managedclusters; any other token is allowed while defaultAllowed is true. serviceAccount is the identity bin/fake-kubeconfig gives the backend; it is allowed even when defaultAllowed is false. The console BFF sends its AUTH_TOKEN to the SelfSubjectAccessReview endpoint, so pick a token from this file to see the console as that kind of user. Converted from the console fixture directory's access.json; serviceAccount is added here."

func convertAccess(raw []byte) (any, error) {
	var in struct {
		Comment        string   `json:"_comment"`
		DefaultAllowed bool     `json:"defaultAllowed"`
		DeniedTokens   []string `json:"deniedTokens"`
		RejectedTokens []string `json:"rejectedTokens"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	return hub.Access{
		Comment: accessComment, DefaultAllowed: in.DefaultAllowed, DeniedTokens: in.DeniedTokens, RejectedTokens: in.RejectedTokens,
		ServiceAccount: hub.ServiceAccount{Token: hub.DefaultServiceAccountToken, Username: hub.DefaultServiceAccountUsername},
	}, nil
}

const scenariosComment = "Timed transitions the fake hub applies to the ManagedCluster CRs relative to its start, divided by settings.json speed (speed 6 fires the 20s event after about 3s). Each event's condition, taint and label changes are documented, with the hub controller source lines, in internal/fakeinfra/hub/scenarios.go and acm.go; every event is one write with its own resourceVersion. spoke is what the agent publishes on join, in the CR's status shape. The import of site-berlin-1 follows the order observed on the hub (accepted, joined, imported, add-ons); site-madrid-1 recovers its lease; edge-lyon-2 finishes detaching and disappears. Converted from the console fixture directory's scenarios.json."

func convertScenarios(raw []byte) (any, error) {
	var in struct {
		Comment string `json:"_comment"`
		Events  []struct {
			Cluster string       `json:"cluster"`
			At      hub.Duration `json:"at"`
			Event   string       `json:"event"`
			Spoke   *struct {
				KubernetesVersion string            `json:"kubernetesVersion"`
				Capacity          map[string]string `json:"capacity"`
				Allocatable       map[string]string `json:"allocatable"`
				ClusterClaims     []pbClaim         `json:"clusterClaims"`
			} `json:"spoke"`
			Addons     []string `json:"addons"`
			StatusCode int      `json:"statusCode"`
			Detail     string   `json:"detail"`
		} `json:"events"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	out := struct {
		Comment string              `json:"_comment"`
		Events  []hub.ScenarioEvent `json:"events"`
	}{Comment: scenariosComment, Events: []hub.ScenarioEvent{}}
	for _, ev := range in.Events {
		e := hub.ScenarioEvent{Cluster: ev.Cluster, At: ev.At, Event: ev.Event, Addons: ev.Addons, StatusCode: ev.StatusCode, Detail: ev.Detail}
		if ev.Spoke != nil {
			e.Spoke = &hub.SpokeStatus{
				Version:  hub.ManagedClusterVersion{Kubernetes: ev.Spoke.KubernetesVersion},
				Capacity: ev.Spoke.Capacity, Allocatable: ev.Spoke.Allocatable, ClusterClaims: convertClaims(ev.Spoke.ClusterClaims),
			}
		}
		out.Events = append(out.Events, e)
	}
	return out, nil
}

const settingsComment = "speed multiplies the fake hub's scenario clock (scenarios.json) and the time a ManagedCluster deleted through the API takes to finish detaching (30s, or detachAfter when set): 1 is real time, 10 plays a 5 minute scenario in 30 seconds. Converted from the console fixture directory's settings.json; its other keys (RPC latency and the hub capabilities the console's fake backend reports) describe the console's fake gRPC backend, which the real backend replaces here, and are not carried over."

func convertSettings(raw []byte) (any, error) {
	var in struct {
		Speed *float64 `json:"speed"`
	}
	// Plain JSON with keys this side ignores on purpose (see settingsComment).
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	return hub.Settings{Comment: settingsComment, Speed: in.Speed}, nil
}
