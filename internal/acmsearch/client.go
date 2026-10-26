// Package acmsearch reads the hub's ACM Search index (search-v2,
// stolostron/search-v2-api): a GraphQL API, by default the Service
// search-search-api in open-cluster-management on port 4010 at
// /searchapi/graphql, that indexes the resources of every managed cluster.
// The backend uses it for what the hub's own objects don't carry: a managed
// cluster's nodes with all their labels (ManagedClusterInfo keeps only the
// instance type, region, zone and node-role labels of each node).
package acmsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// DefaultURL is the in-cluster address of search-v2's API on an ACM hub.
const DefaultURL = "https://search-search-api.open-cluster-management.svc:4010/searchapi/graphql"

// maxResponseBytes caps a Search answer. A node item is about 3 KiB with
// GPU Feature Discovery's labels; this is room for thousands of GPU nodes.
const maxResponseBytes = 32 << 20

// Node is one node of a managed cluster as Search indexes it.
type Node struct {
	// Cluster is the managed cluster's name.
	Cluster string
	// Name is the node's name.
	Name string
	// Labels are the node's labels, parsed from Search's "k=v; k=v" form.
	Labels map[string]string
	// Ready is true when the node's Ready condition is True.
	Ready bool
}

// Client queries Search with a fixed bearer credential. Its http.Client
// carries the TLS settings and the Authorization header (see main).
type Client struct {
	url  string
	http *http.Client
}

// New returns a Client for the GraphQL endpoint url.
func New(url string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{url: url, http: httpClient}
}

// gpuNodesQuery asks for every Node with GPU Feature Discovery's
// nvidia.com/gpu.present=true label on every managed cluster the caller may
// see. A label filter takes "key=value" (search-v2-api
// pkg/resolver/searchHelper.go, label operands), and a limit of -1 returns
// every match instead of the default 1000 (setLimit, same file).
const gpuNodesQuery = `query mySearch($input: [SearchInput]) { searchResult: search(input: $input) { items } }`

type searchFilter struct {
	Property string   `json:"property"`
	Values   []string `json:"values"`
}

type searchInput struct {
	Filters []searchFilter `json:"filters"`
	Limit   int            `json:"limit"`
}

type graphQLRequest struct {
	Query     string `json:"query"`
	Variables struct {
		Input []searchInput `json:"input"`
	} `json:"variables"`
}

type graphQLResponse struct {
	Data struct {
		SearchResult []struct {
			Items []map[string]any `json:"items"`
		} `json:"searchResult"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// GPUNodes returns every node Search knows with nvidia.com/gpu.present=true,
// sorted by cluster and then by name.
func (c *Client) GPUNodes(ctx context.Context) ([]Node, error) {
	var req graphQLRequest
	req.Query = gpuNodesQuery
	req.Variables.Input = []searchInput{{
		Filters: []searchFilter{
			{Property: "kind", Values: []string{"Node"}},
			{Property: "label", Values: []string{"nvidia.com/gpu.present=true"}},
		},
		Limit: -1,
	}}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode search query: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build search request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("query search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read search answer: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("search answer is larger than %d bytes", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search answered %s: %s", resp.Status, snippet(raw))
	}
	var out graphQLResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode search answer: %w", err)
	}
	if len(out.Errors) > 0 {
		msgs := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, errors.New("search: " + strings.Join(msgs, "; "))
	}
	var nodes []Node
	for _, result := range out.Data.SearchResult {
		for _, item := range result.Items {
			node := nodeFromItem(item)
			if node.Cluster == "" || node.Name == "" {
				continue
			}
			nodes = append(nodes, node)
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Cluster != nodes[j].Cluster {
			return nodes[i].Cluster < nodes[j].Cluster
		}
		return nodes[i].Name < nodes[j].Name
	})
	return nodes, nil
}

// nodeFromItem reads a Search item of kind Node: `cluster`, `name`, `label`
// as "k=v; k=v" and `condition` as "DiskPressure=False; ...; Ready=True"
// (the shape search-v2's collector indexes, as an ACM hub answered it).
func nodeFromItem(item map[string]any) Node {
	str := func(key string) string {
		v, _ := item[key].(string)
		return v
	}
	return Node{
		Cluster: str("cluster"),
		Name:    str("name"),
		Labels:  ParsePairs(str("label")),
		Ready:   ParsePairs(str("condition"))["Ready"] == "True",
	}
}

// ParsePairs reads Search's "k=v; k=v" encoding of a map. A pair without
// "=" is a key with an empty value, as a label like
// node-role.kubernetes.io/worker= is written.
func ParsePairs(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, "; ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		out[k] = v
	}
	return out
}

func snippet(b []byte) string {
	const n = 200
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
