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
	if err := RemoveWritten(path, first); err != nil {
		t.Fatalf("RemoveWritten(stale) error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("RemoveWritten removed a file it did not write: %v", err)
	}
	if err := RemoveWritten(path, second); err != nil {
		t.Fatalf("RemoveWritten() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}
	if err := RemoveWritten(path, second); err != nil {
		t.Fatalf("RemoveWritten(missing) error = %v", err)
	}
}

// WriteHubCA never clobbers a file that isn't its own, never writes through
// a link, and RemoveWritten removes only the bytes it wrote.
func TestWriteHubCA(t *testing.T) {
	_, caPEM, err := SelfSignedCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// Someone else's file stays, unchanged.
	other := filepath.Join(dir, "other.crt")
	if err := os.WriteFile(other, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteHubCA(other, caPEM); err == nil {
		t.Fatal("WriteHubCA replaced a file it didn't write")
	}
	if raw, _ := os.ReadFile(other); string(raw) != "not ours" {
		t.Fatalf("other file now %q", raw)
	}

	// A link is refused, so its target isn't written either.
	link := filepath.Join(dir, "link.crt")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteHubCA(link, caPEM); err == nil {
		t.Fatal("WriteHubCA wrote through a symbolic link")
	}

	// A fresh path, then a second run over the CA the first left behind.
	path := filepath.Join(dir, "bin", "fake-hub-ca.crt")
	written, err := WriteHubCA(path, caPEM)
	if err != nil {
		t.Fatal(err)
	}
	_, nextPEM, err := SelfSignedCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nextWritten, err := WriteHubCA(path, nextPEM)
	if err != nil {
		t.Fatalf("replacing our own CA: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, err %v", info, err)
	}

	// The first run's cleanup leaves the second run's file alone.
	if err := RemoveWritten(path, written); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed a CA another run wrote")
	}
	if err := RemoveWritten(path, nextWritten); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("CA still there: %v", err)
	}
}

// WriteKubeconfig, like WriteHubCA, refuses a link even to a kubeconfig it
// wrote itself, so it never replaces the link or writes through it.
func TestWriteKubeconfigRefusesALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "kubeconfig")
	if _, err := WriteKubeconfig(target, "https://127.0.0.1:9091", []byte("ca"), "token"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(target)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteKubeconfig(link, "https://127.0.0.1:9999", []byte("ca"), "other"); err == nil {
		t.Fatal("WriteKubeconfig replaced a symbolic link")
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v", err)
	}
	if after, _ := os.ReadFile(target); string(after) != string(before) {
		t.Fatal("WriteKubeconfig wrote through a symbolic link")
	}
}
