package acmsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGPUNodesQueryAndAnswer(t *testing.T) {
	t.Parallel()
	var got graphQLRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %q", r.Method, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body: %v", err)
		}
		// The shape an ACM hub's search-v2 answered for a GPU node, trimmed.
		_, _ = w.Write([]byte(`{"data":{"searchResult":[{"items":[
			{"cluster":"site-b","name":"gpu-node-b","kind":"Node","condition":"DiskPressure=False; MemoryPressure=False; PIDPressure=False; Ready=True",
			 "label":"node-role.kubernetes.io/gpu=; nvidia.com/gpu.count=1; nvidia.com/gpu.product=NVIDIA-L4"},
			{"cluster":"site-a","name":"gpu-node-a","kind":"Node","condition":"Ready=False","label":"nvidia.com/gpu.present=true",
			 "cpu":8,"_uid":"local-cluster/abc","extra":{"nested":[1,2,3]}},
			{"cluster":"site-c","name":["not","a","string"],"label":42},
			{"cluster":"","name":"no-cluster","label":""}
		]}]}}`))
	}))
	defer srv.Close()

	nodes, err := New(srv.URL, srv.Client()).GPUNodes(context.Background(),
		"nvidia.com/gpu.product", "nvidia.com/gpu.count", "node-role.kubernetes.io/gpu")
	if err != nil {
		t.Fatal(err)
	}
	in := got.Variables.Input
	if len(in) != 1 || in[0].Limit != -1 || len(in[0].Filters) != 2 ||
		in[0].Filters[0].Property != "kind" || in[0].Filters[0].Values[0] != "Node" ||
		in[0].Filters[1].Property != "label" || in[0].Filters[1].Values[0] != "nvidia.com/gpu.present=true" {
		t.Fatalf("query input = %+v", in)
	}
	if !strings.Contains(got.Query, "searchResult: search(input: $input)") {
		t.Fatalf("query = %q", got.Query)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %+v, want 2 (an item without a cluster or a string name is skipped)", nodes)
	}
	// Sorted by cluster, then name.
	if nodes[0].Cluster != "site-a" || nodes[0].Ready || nodes[1].Cluster != "site-b" || !nodes[1].Ready {
		t.Fatalf("nodes = %+v", nodes)
	}
	if l := nodes[1].Labels; l["nvidia.com/gpu.product"] != "NVIDIA-L4" || l["nvidia.com/gpu.count"] != "1" {
		t.Fatalf("labels = %v", l)
	}
	if v, ok := nodes[1].Labels["node-role.kubernetes.io/gpu"]; !ok || v != "" {
		t.Fatalf("an empty-valued label must be kept: %v", nodes[1].Labels)
	}
	// Only the named labels are kept.
	if len(nodes[0].Labels) != 0 || len(nodes[1].Labels) != 3 {
		t.Fatalf("labels = %v and %v, want only the named ones", nodes[0].Labels, nodes[1].Labels)
	}
}

// A node's labels are written by its cluster's administrators. However many
// a node carries, its Node keeps only the ones the caller named.
func TestGPUNodesKeepsOnlyTheNamedLabels(t *testing.T) {
	t.Parallel()
	var label strings.Builder
	label.WriteString("nvidia.com/gpu.count=8")
	for i := range 10000 {
		fmt.Fprintf(&label, "; nvidia.com/junk-%d=x", i)
	}
	item, _ := json.Marshal(map[string]string{"cluster": "site-a", "name": "n", "label": label.String(), "condition": "Ready=True"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"data":{"searchResult":[{"items":[%s]}]}}`, item)
	}))
	defer srv.Close()
	nodes, err := New(srv.URL, srv.Client()).GPUNodes(context.Background(), "nvidia.com/gpu.count")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || len(nodes[0].Labels) != 1 || nodes[0].Labels["nvidia.com/gpu.count"] != "8" || !nodes[0].Ready {
		t.Fatalf("nodes = %d, labels = %d", len(nodes), len(nodes[0].Labels))
	}
}

// A node is Ready when Search renders its Ready condition True, read as
// ManagedCluster conditions are (case-insensitive). The first case is what
// hf299's search-v2 answered for a ready node (read-only query, 2026-10-28).
func TestGPUNodesReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		condition string
		ready     bool
	}{
		{"DiskPressure=False; MemoryPressure=False; PIDPressure=False; Ready=True", true},
		{"Ready=True", true},
		{"Ready=true", true},
		{"Ready=False", false},
		{"Ready=Unknown", false},
		{"DiskPressure=True", false},
		{"", false},
	} {
		t.Run(tc.condition, func(t *testing.T) {
			t.Parallel()
			item, _ := json.Marshal(map[string]string{"cluster": "site-a", "name": "n", "condition": tc.condition})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"data":{"searchResult":[{"items":[%s]}]}}`, item)
			}))
			defer srv.Close()
			nodes, err := New(srv.URL, srv.Client()).GPUNodes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(nodes) != 1 || nodes[0].Ready != tc.ready {
				t.Fatalf("nodes = %+v, want one with Ready %v", nodes, tc.ready)
			}
		})
	}
}

func TestGPUNodesErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"Invalid token"}`, func(err error) bool {
			var se *StatusError
			return errors.As(err, &se) && se.Code == http.StatusUnauthorized
		}},
		{"GraphQL error", http.StatusOK, `{"errors":[{"message":"incorrect label format"}]}`, func(err error) bool {
			var ge *GraphQLError
			return errors.As(err, &ge) && len(ge.Messages) == 1 && ge.Messages[0] == "incorrect label format"
		}},
		{"not JSON", http.StatusOK, `<html>`, func(err error) bool { return errors.Is(err, ErrDecode) }},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(srv.URL, srv.Client()).GPUNodes(context.Background())
		srv.Close()
		if !tc.check(err) {
			t.Errorf("%s: error = %v (%T)", tc.name, err, err)
		}
	}
}

func TestParsePairs(t *testing.T) {
	t.Parallel()
	got := parsePairs("a=1; b=; c=x=y; ; d; skipped=1", map[string]bool{"a": true, "b": true, "c": true, "d": true, "missing": true})
	want := map[string]string{"a": "1", "b": "", "c": "x=y", "d": ""}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

// tokenTransport adds a bearer token to every request, as the backend's
// credential transport does (main.newGPUInventory).
type tokenTransport struct{ base http.RoundTripper }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer secret-token")
	return t.base.RoundTrip(r)
}

// A redirect from Search is never followed, so the token never reaches the
// place it points to.
func TestGPUNodesRefusesRedirects(t *testing.T) {
	t.Parallel()
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		if r.Header.Get("Authorization") != "" {
			t.Error("the token reached the redirect target")
		}
		_, _ = w.Write([]byte(`{"data":{"searchResult":[{"items":[]}]}}`))
	}))
	defer elsewhere.Close()
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/steal", code)
		}))
		client := New(search.URL, &http.Client{Transport: tokenTransport{base: http.DefaultTransport}})
		_, err := client.GPUNodes(context.Background())
		search.Close()
		if !errors.Is(err, ErrRedirect) {
			t.Errorf("%d: error = %v, want ErrRedirect", code, err)
		}
	}
	if reached.Load() {
		t.Fatal("a redirect was followed")
	}
}

// New doesn't change the http.Client it is given.
func TestNewKeepsTheCallersClient(t *testing.T) {
	t.Parallel()
	hc := &http.Client{}
	_ = New("https://search.invalid", hc)
	if hc.CheckRedirect != nil {
		t.Fatal("New changed the caller's http.Client")
	}
}

// An answer larger than maxResponseBytes is refused rather than decoded.
func TestGPUNodesRefusesAnOversizedAnswer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"searchResult":[{"items":[{"name":"`))
		chunk := []byte(strings.Repeat("x", 1<<20))
		for written := 0; written <= maxResponseBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	_, err := New(srv.URL, srv.Client()).GPUNodes(context.Background())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

// A query ends when its context does, even while Search hasn't answered.
func TestGPUNodesStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(srv.URL, srv.Client()).GPUNodes(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the query outlived its context")
	}
}
