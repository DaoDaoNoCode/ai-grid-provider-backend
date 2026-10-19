// Package hub is a fake of the slice of an OCM/RHACM hub's Kubernetes API that
// ai-grid-provider-backend and the console BFF call: the cluster-scoped
// ManagedCluster resource (cluster.open-cluster-management.io/v1) and
// SelfSubjectAccessReview. It answers with the API server's own status codes,
// reasons and messages (built with k8s.io/apimachinery/pkg/api/errors, the
// package the API server itself uses), so the real backend's dynamic client
// and its apierrors.IsNotFound / IsConflict / ... checks behave as against a
// hub. It also plays the hub controllers that write ManagedCluster status, on
// a timeline taken from scenarios.json (scenarios.go, acm.go).
package hub

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// API coordinates of the ManagedCluster resource.
const (
	Group      = "cluster.open-cluster-management.io"
	Version    = "v1"
	Resource   = "managedclusters"
	Kind       = "ManagedCluster"
	ListKind   = "ManagedClusterList"
	APIVersion = Group + "/" + Version
)

var (
	groupResource = schema.GroupResource{Group: Group, Resource: Resource}
	groupKind     = schema.GroupKind{Group: Group, Kind: Kind}
)

// ManagedCluster mirrors open-cluster-management.io/api cluster/v1
// ManagedCluster field for field (api:cluster/v1/types.go). Decoding a request
// into it drops unknown fields, as the API server prunes fields a structural
// CRD schema does not declare.
type ManagedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManagedClusterSpec    `json:"spec"`
	Status *ManagedClusterStatus `json:"status,omitempty"`
}

// ManagedClusterSpec is `spec`.
type ManagedClusterSpec struct {
	ManagedClusterClientConfigs []ClientConfig `json:"managedClusterClientConfigs,omitempty"`
	HubAcceptsClient            bool           `json:"hubAcceptsClient"`
	// LeaseDurationSeconds is a pointer so that the CRD default (60) applies
	// only when the field is absent, as API server defaulting does.
	LeaseDurationSeconds *int32  `json:"leaseDurationSeconds,omitempty"`
	Taints               []Taint `json:"taints,omitempty"`
}

// ClientConfig is one entry of `spec.managedClusterClientConfigs`.
type ClientConfig struct {
	URL      string `json:"url"`
	CABundle []byte `json:"caBundle,omitempty"`
}

// Taint is one entry of `spec.taints`.
type Taint struct {
	Key       string      `json:"key"`
	Value     string      `json:"value,omitempty"`
	Effect    string      `json:"effect"`
	TimeAdded metav1.Time `json:"timeAdded"`
}

// ManagedClusterStatus is `status`, written by the hub controllers and the
// spoke agent; never by a client through the main resource.
type ManagedClusterStatus struct {
	Conditions []metav1.Condition `json:"conditions"`
	// Capacity and Allocatable are resource.Quantity maps upstream. They are
	// kept as strings so that values round-trip byte for byte, as a CR does
	// in etcd (no quantity canonicalisation for custom resources).
	Capacity      map[string]string     `json:"capacity,omitempty"`
	Allocatable   map[string]string     `json:"allocatable,omitempty"`
	Version       ManagedClusterVersion `json:"version,omitempty"`
	ClusterClaims []ManagedClusterClaim `json:"clusterClaims,omitempty"`
}

// ManagedClusterVersion is `status.version`.
type ManagedClusterVersion struct {
	Kubernetes string `json:"kubernetes,omitempty"`
}

// ManagedClusterClaim is one entry of `status.clusterClaims`.
type ManagedClusterClaim struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// ManagedClusterList is the list response.
type ManagedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedCluster `json:"items"`
}

// DeepCopy returns an independent copy (a JSON round trip: the type is
// plain data, and the fake is not on a hot path).
func (c *ManagedCluster) DeepCopy() *ManagedCluster {
	raw, err := json.Marshal(c)
	if err != nil {
		panic("marshal ManagedCluster: " + err.Error())
	}
	out := &ManagedCluster{}
	if err := json.Unmarshal(raw, out); err != nil {
		panic("unmarshal ManagedCluster: " + err.Error())
	}
	return out
}

// jsonEqual reports whether a and b encode to the same JSON, which is how the
// API server decides that an update changes nothing (it compares the encoded
// object with the stored bytes).
func jsonEqual(a, b any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ra) == string(rb)
}
