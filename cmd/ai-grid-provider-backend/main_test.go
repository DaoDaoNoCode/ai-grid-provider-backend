package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"k8s.io/client-go/rest"
)

// The backend's credential goes with every Search query, so --search-api-url
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
		inv, err := newGPUInventory(config, searchSettings{url: tc.url})
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: error = %v, want error %v", tc.url, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && (inv == nil) != tc.wantNil {
			t.Errorf("%q: inventory = %v, want nil %v", tc.url, inv, tc.wantNil)
		}
	}
}

// Skipping the certificate check is only for a port-forward on this
// machine: anywhere else, whoever answered would receive the credential.
func TestNewGPUInventorySkipVerifyOnlyOnLoopback(t *testing.T) {
	config := &rest.Config{BearerToken: "token"}
	for _, tc := range []struct {
		url     string
		wantErr bool
	}{
		{url: "https://127.0.0.1:14012/searchapi/graphql"},
		{url: "https://localhost:4010/searchapi/graphql"},
		{url: "https://[::1]:4010/searchapi/graphql"},
		{url: "https://search-search-api.open-cluster-management.svc:4010/searchapi/graphql", wantErr: true},
		{url: "https://10.0.0.5:4010/searchapi/graphql", wantErr: true},
		{url: "https://127.0.0.1.example.com/searchapi/graphql", wantErr: true},
	} {
		_, err := newGPUInventory(config, searchSettings{url: tc.url, insecureSkipVerify: true})
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: error = %v, want error %v", tc.url, err, tc.wantErr)
		}
	}
}

// A token file builds an authenticated Search client; a client certificate
// alone still builds one (anonymous, with a warning) rather than failing.
func TestNewGPUInventoryCredentials(t *testing.T) {
	search := searchSettings{url: "https://search.example:4010/searchapi/graphql"}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]*rest.Config{
		"token file":  {BearerTokenFile: tokenFile},
		"client cert": {TLSClientConfig: rest.TLSClientConfig{CertData: []byte("cert"), KeyData: []byte("key")}},
	} {
		if inv, err := newGPUInventory(config, search); err != nil || inv == nil {
			t.Errorf("%s: inventory %v, error %v", name, inv, err)
		}
	}
}

// Without AI_GRID_GIT_URL the backend starts: newGitWriter answers a writer
// that refuses every call instead of an error that stops the process.
func TestNewGitWriterWithoutURL(t *testing.T) {
	t.Setenv("AI_GRID_GIT_URL", "")
	t.Setenv("AI_GRID_GIT_TOKEN_FILE", "")
	writer, err := newGitWriter()
	if err != nil {
		t.Fatalf("newGitWriter() error = %v, want none", err)
	}
	if writer == nil {
		t.Fatal("newGitWriter() = nil writer")
	}
	if _, err := writer.Head(context.Background()); !errors.Is(err, gitops.ErrNotConfigured) {
		t.Errorf("Head() error = %v, want ErrNotConfigured", err)
	}
}

// A token file without a URL is most likely a Secret missing its url key, so
// it still stops the process at startup.
func TestNewGitWriterRefusesTokenFileWithoutURL(t *testing.T) {
	t.Setenv("AI_GRID_GIT_URL", "")
	t.Setenv("AI_GRID_GIT_TOKEN_FILE", "/var/run/secrets/ai-grid-git/token")
	if _, err := newGitWriter(); err == nil {
		t.Fatal("newGitWriter() succeeded with a token file and no URL")
	}
}

// A URL with credentials is still refused at startup.
func TestNewGitWriterRefusesCredentialsInURL(t *testing.T) {
	t.Setenv("AI_GRID_GIT_URL", "https://user:token@example.com/catalog.git")
	if _, err := newGitWriter(); err == nil {
		t.Fatal("newGitWriter() succeeded with credentials in the URL")
	}
}
