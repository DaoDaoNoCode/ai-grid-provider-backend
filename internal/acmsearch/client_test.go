package acmsearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			{"cluster":"site-b","name":"ip-10-0-25-128","kind":"Node","condition":"DiskPressure=False; MemoryPressure=False; PIDPressure=False; Ready=True",
			 "label":"node-role.kubernetes.io/gpu=; nvidia.com/gpu.count=1; nvidia.com/gpu.product=NVIDIA-L4"},
			{"cluster":"site-a","name":"ip-10-0-21-83","kind":"Node","condition":"Ready=False","label":"nvidia.com/gpu.present=true"},
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
