package hub

// A fake of ACM Search's GraphQL API (stolostron/search-v2-api), only as far
// as the backend uses it: the `search` query over Node items, filtered by
// kind, cluster, name and label. The real API is a separate Service
// (search-search-api in open-cluster-management, /searchapi/graphql); the
// fake serves the same path on the fake hub's port, so make dev-fake needs no
// other listener.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// SearchPath is where the fake answers Search's GraphQL queries.
const SearchPath = "/searchapi/graphql"

// SearchNode is one Node of a managed cluster as the fake Search indexes it
// (fixtures/README.md, searchnodes.json).
type SearchNode struct {
	Cluster string            `json:"cluster"`
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	// Ready is the node's Ready condition.
	Ready bool `json:"ready"`
}

type searchQuery struct {
	Query     string `json:"query"`
	Variables struct {
		Input []struct {
			Filters []struct {
				Property string   `json:"property"`
				Values   []string `json:"values"`
			} `json:"filters"`
			Limit *int `json:"limit"`
		} `json:"input"`
	} `json:"variables"`
}

// search answers like search-v2-api: no token is 401 and a token the
// TokenReview rejects 403 (pkg/rbac/authnMiddleware.go); a caller that may
// see no managed cluster gets no items rather than an error, because Search
// filters results by what the caller may see (pkg/rbac/userData.go).
func (h *Hub) search(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, `{"message":"Request didn't have a valid authentication token."}`, http.StatusUnauthorized)
		return
	}
	if slices.Contains(h.access.RejectedTokens, token) {
		http.Error(w, `{"message":"Invalid token"}`, http.StatusForbidden)
		return
	}
	var q searchQuery
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &q) != nil {
		http.Error(w, `{"message":"invalid GraphQL request"}`, http.StatusBadRequest)
		return
	}
	type result struct {
		Items []map[string]any `json:"items"`
	}
	results := make([]result, 0, len(q.Variables.Input))
	for _, in := range q.Variables.Input {
		var items []map[string]any
		if h.allowed(token) {
			for _, node := range h.searchNodes() {
				match, err := searchMatches(node, in.Filters)
				if err != nil {
					writeGraphQLError(w, err)
					return
				}
				if match {
					items = append(items, searchItem(node))
				}
			}
		}
		if items == nil {
			items = []map[string]any{}
		}
		results = append(results, result{Items: items})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"searchResult": results}})
}

// searchNodes are the indexed nodes of the clusters the hub still has:
// Search drops a cluster's resources once it is detached.
func (h *Hub) searchNodes() []SearchNode {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance() // finish detaches and scenario events due by now, as list does
	out := make([]SearchNode, 0, len(h.nodes))
	for _, n := range h.nodes {
		if _, ok := h.clusters[n.Cluster]; ok {
			out = append(out, n)
		}
	}
	return out
}

// searchMatches applies the filters the backend sends; each filter matches
// when any of its values does. A label value is "key=value"
// (pkg/resolver/searchHelper.go).
func searchMatches(node SearchNode, filters []struct {
	Property string   `json:"property"`
	Values   []string `json:"values"`
}) (bool, error) {
	for _, f := range filters {
		matched := false
		for _, v := range f.Values {
			switch f.Property {
			case "kind":
				matched = matched || v == "Node"
			case "cluster":
				matched = matched || v == node.Cluster
			case "name":
				matched = matched || v == node.Name
			case "label":
				key, value, ok := strings.Cut(v, "=")
				if !ok {
					return false, fmt.Errorf("incorrect label format, label filters must have the format key=value")
				}
				got, has := node.Labels[key]
				matched = matched || (has && got == value)
			default:
				return false, fmt.Errorf("the fake Search does not filter by %q", f.Property)
			}
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

// searchItem renders a node the way Search's collector indexes it: labels as
// "k=v; k=v" sorted by key, the condition as "Type=Status; ...".
func searchItem(n SearchNode) map[string]any {
	keys := make([]string, 0, len(n.Labels))
	for k := range n.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+n.Labels[k])
	}
	ready := "False"
	status := "NotReady"
	if n.Ready {
		ready, status = "True", "Ready"
	}
	return map[string]any{
		"kind":       "Node",
		"apiversion": "v1",
		"cluster":    n.Cluster,
		"name":       n.Name,
		"label":      strings.Join(pairs, "; "),
		"condition":  "DiskPressure=False; MemoryPressure=False; PIDPressure=False; Ready=" + ready,
		"status":     status,
	}
}

func writeGraphQLError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"message": err.Error()}},
		"data":   map[string]any{"searchResult": nil},
	})
}
