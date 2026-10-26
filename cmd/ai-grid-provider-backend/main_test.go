package main

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
)

// The backend's credential goes with every Search query, so SEARCH_API_URL
// must be https; "off" turns GPUs off.
func TestNewGPUInventoryURL(t *testing.T) {
	config := &rest.Config{BearerToken: "token"}
	for _, tc := range []struct {
		url     string
		wantErr bool
		wantNil bool
	}{
		{url: "off", wantNil: true},
		{url: "https://search.example:4010/searchapi/graphql"},
		{url: "http://search.example:4010/searchapi/graphql", wantErr: true},
		{url: "search.example:4010", wantErr: true},
		{url: "https://", wantErr: true},
	} {
		t.Setenv("SEARCH_API_URL", tc.url)
		t.Setenv("SEARCH_CA_FILE", "")
		inv, err := newGPUInventory(config)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: error = %v, want error %v", tc.url, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && (inv == nil) != tc.wantNil {
			t.Errorf("%q: inventory = %v, want nil %v", tc.url, inv, tc.wantNil)
		}
	}
}

// A token file builds an authenticated Search client; a client certificate
// alone still builds one (anonymous, with a warning) rather than failing.
func TestNewGPUInventoryCredentials(t *testing.T) {
	t.Setenv("SEARCH_API_URL", "https://search.example:4010/searchapi/graphql")
	t.Setenv("SEARCH_CA_FILE", "")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]*rest.Config{
		"token file":  {BearerTokenFile: tokenFile},
		"client cert": {TLSClientConfig: rest.TLSClientConfig{CertData: []byte("cert"), KeyData: []byte("key")}},
	} {
		if inv, err := newGPUInventory(config); err != nil || inv == nil {
			t.Errorf("%s: inventory %v, error %v", name, inv, err)
		}
	}
}
