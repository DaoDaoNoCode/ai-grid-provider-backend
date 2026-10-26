package acmsearch

import (
	"context"
	"encoding/json"
	"errors"
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
			{"cluster":"site-a","name":"gpu-node-a","kind":"Node","condition":"Ready=False","label":"nvidia.com/gpu.present=true"},
			{"cluster":"","name":"no-cluster","label":""}
		]}]}}`))
	}))
	defer srv.Close()

	nodes, err := New(srv.URL, srv.Client()).GPUNodes(context.Background())
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
		t.Fatalf("nodes = %+v, want 2 (an item without a cluster is skipped)", nodes)
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
}

func TestGPUNodesErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"Invalid token"}`, "401"},
		{"GraphQL error", http.StatusOK, `{"errors":[{"message":"incorrect label format"}]}`, "incorrect label format"},
		{"not JSON", http.StatusOK, `<html>`, "decode search answer"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(srv.URL, srv.Client()).GPUNodes(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestParsePairs(t *testing.T) {
	t.Parallel()
	got := ParsePairs("a=1; b=; c=x=y; ; d")
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
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %v, want the size limit", err)
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
