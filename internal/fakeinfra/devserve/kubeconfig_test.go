package devserve

import (
	"os"
	"path/filepath"
	"testing"
)

const someoneElsesKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://api.example.com:6443
  name: real
contexts:
- context:
    cluster: real
    user: me
  name: real
current-context: real
users:
- name: me
  user:
    token: not-ours
`

func TestWriteKubeconfigNeverReplacesAForeignFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"kubeconfig": someoneElsesKubeconfig, "garbage": "not yaml: [", "empty": ""} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := WriteKubeconfig(path, "https://127.0.0.1:9091", nil, "t"); err == nil {
			t.Fatalf("%s: WriteKubeconfig replaced a file it did not write", name)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Fatalf("%s: file changed to %q", name, got)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatalf("left temporary files behind: %v", entries)
	}
}

func TestKubeconfigLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "fake-kubeconfig")
	first, err := WriteKubeconfig(path, "https://127.0.0.1:9091", nil, "t1")
	if err != nil {
		t.Fatalf("WriteKubeconfig() error = %v", err)
	}
	if ours, err := IsFakeKubeconfig(path); err != nil || !ours {
		t.Fatalf("IsFakeKubeconfig() = %v, %v", ours, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	// A stale fake kubeconfig (from an earlier run) may be replaced.
	second, err := WriteKubeconfig(path, "https://127.0.0.1:19091", nil, "t2")
	if err != nil {
		t.Fatalf("WriteKubeconfig(again) error = %v", err)
	}
	// Removing what an earlier writer wrote leaves the newer file alone.
	if err := RemoveKubeconfig(path, first); err != nil {
		t.Fatalf("RemoveKubeconfig(stale) error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("RemoveKubeconfig removed a file it did not write: %v", err)
	}
	if err := RemoveKubeconfig(path, second); err != nil {
		t.Fatalf("RemoveKubeconfig() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}
	if err := RemoveKubeconfig(path, second); err != nil {
		t.Fatalf("RemoveKubeconfig(missing) error = %v", err)
	}
}
