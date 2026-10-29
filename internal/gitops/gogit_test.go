package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// newBareRepo returns the URL of a local bare repository whose main branch
// has one commit holding files.
func newBareRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	if _, err := git.PlainInitWithOptions(bare, &git.PlainInitOptions{
		Bare:        true,
		InitOptions: git.InitOptions{DefaultBranch: "refs/heads/main"},
	}); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	seed := filepath.Join(root, "seed")
	repo, err := git.PlainInitWithOptions(seed, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: "refs/heads/main"}})
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	wt, _ := repo.Worktree()
	for p, data := range files {
		full := filepath.Join(seed, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(p); err != nil {
			t.Fatalf("add %s: %v", p, err)
		}
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	if _, err := wt.Commit("seed", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatalf("remote: %v", err)
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	return bare
}

func newTestRepoWriter(t *testing.T, url string) *RepoWriter {
	t.Helper()
	w, err := NewRepoWriter(context.Background(), RepoConfig{
		URL: url, Branch: "main", AuthorName: "ai-grid-provider-backend", AuthorEmail: "backend@example.com",
	})
	if err != nil {
		t.Fatalf("NewRepoWriter() error = %v", err)
	}
	return w
}

func TestRepoWriterCommitAndRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"README.md": "demo", "catalog/subscriptions/a.yaml": "a"})
	w := newTestRepoWriter(t, url)

	head, err := w.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	if names, err := head.List("catalog/subscriptions"); err != nil || !slices.Equal(names, []string{"a.yaml"}) {
		t.Fatalf("List() = %v, %v", names, err)
	}
	rev, err := w.Commit(ctx, head.Revision(), Change{
		Put:     map[string][]byte{"catalog/subscriptions/b.yaml": []byte("b")},
		Delete:  []string{"catalog/subscriptions/a.yaml"},
		Message: "subscriptions: create b, delete a",
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	// A fresh clone sees the pushed commit.
	other := newTestRepoWriter(t, url)
	head, err = other.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	if head.Revision() != rev {
		t.Fatalf("remote head = %s, want %s", head.Revision(), rev)
	}
	if data, err := head.Read("catalog/subscriptions/b.yaml"); err != nil || string(data) != "b" {
		t.Fatalf("Read(b) = %q, %v", data, err)
	}
	if _, err := head.Read("catalog/subscriptions/a.yaml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(a) error = %v, want ErrNotFound", err)
	}
	if _, err := head.Read("missing/dir/x.yaml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(missing dir) error = %v, want ErrNotFound", err)
	}
	if names, err := head.List("missing"); err != nil || len(names) != 0 {
		t.Fatalf("List(missing) = %v, %v", names, err)
	}
	if names, err := head.List("catalog"); err != nil || len(names) != 0 {
		t.Fatalf("List(catalog) = %v, %v; want no files, only a directory", names, err)
	}
}

func TestRepoWriterConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w := newTestRepoWriter(t, url)
	other := newTestRepoWriter(t, url)

	base, err := w.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	// Another backend changes a after w read the head.
	otherHead, _ := other.Head(ctx)
	if _, err := other.Commit(ctx, otherHead.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "other"}); err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}

	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"a": []byte("a3")}, Message: "m"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit(a) from a stale base error = %v, want ErrConflict", err)
	}
	// A file the other write didn't touch commits on top of it.
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "m"}); err != nil {
		t.Fatalf("Commit(b) from a stale base error = %v", err)
	}
	head, _ := other.Head(ctx)
	for p, want := range map[string]string{"a": "a2", "b": "b2"} {
		if data, err := head.Read(p); err != nil || string(data) != want {
			t.Fatalf("Read(%s) = %q, %v; want %q", p, data, err, want)
		}
	}
}

// A read set conflicts on a file added under a directory since base.
func TestRepoWriterReadSetConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"subs/a.yaml": "a", "models/m": "m", "x": "x"})
	w := newTestRepoWriter(t, url)
	other := newTestRepoWriter(t, url)
	base, _ := w.Head(ctx)
	otherHead, _ := other.Head(ctx)
	if _, err := other.Commit(ctx, otherHead.Revision(), Change{Put: map[string][]byte{"subs/b.yaml": []byte("b")}, Message: "other"}); err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"x": []byte("x2")}, Read: []string{"subs"}, Message: "m"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit(read subs) error = %v, want ErrConflict", err)
	}
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"x": []byte("x2")}, Read: []string{"models/m", "models"}, Message: "m"}); err != nil {
		t.Fatalf("Commit(read unchanged) error = %v", err)
	}
}

// A push the remote refuses because the branch moved after the fetch, as
// when two replicas commit at once, is ErrConflict.
func TestRepoWriterPushRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w := newTestRepoWriter(t, url)
	other := newTestRepoWriter(t, url)
	w.beforePush = func() {
		head, _ := other.Head(ctx)
		if _, err := other.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "other"}); err != nil {
			t.Errorf("other Commit() error = %v", err)
		}
	}
	head, _ := w.Head(ctx)
	if _, err := w.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit() racing a push error = %v, want ErrConflict", err)
	}

	// The writer still works: the commit that lost the race is gone, and the
	// other replica's is the head.
	w.beforePush = nil
	head, err := w.Head(ctx)
	if err != nil {
		t.Fatalf("Head() after the lost push error = %v", err)
	}
	if data, _ := head.Read("a"); string(data) != "a" {
		t.Fatalf("Read(a) after the lost push = %q, want the unchanged a", data)
	}
	if data, _ := head.Read("b"); string(data) != "b2" {
		t.Fatalf("Read(b) after the lost push = %q, want the other replica's b2", data)
	}
	if _, err := w.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"}); err != nil {
		t.Fatalf("Commit() after the lost push error = %v", err)
	}
}

func TestRepoWriterDeleteMissingFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newTestRepoWriter(t, newBareRepo(t, map[string]string{"a": "a"}))
	head, _ := w.Head(ctx)
	if _, err := w.Commit(ctx, head.Revision(), Change{Delete: []string{"missing"}, Message: "m"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Commit(delete missing) error = %v, want ErrNotFound", err)
	}
	// The failed change left nothing behind for the next commit.
	if _, err := w.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"b": []byte("b")}, Message: "m"}); err != nil {
		t.Fatalf("Commit() after a failed change error = %v", err)
	}
	head, _ = w.Head(ctx)
	if _, err := head.Read("a"); err != nil {
		t.Fatalf("Read(a) error = %v", err)
	}
}

func TestNewRepoWriterRequiresConfig(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]RepoConfig{
		"no url":    {Branch: "main", AuthorName: "a", AuthorEmail: "a@example.com"},
		"no branch": {URL: "file:///nowhere", AuthorName: "a", AuthorEmail: "a@example.com"},
		"no author": {URL: "file:///nowhere", Branch: "main"},
		// A token goes only over https, never in the clear.
		"token over http": {URL: "http://127.0.0.1:1/catalog.git", Branch: "main", Token: "t", AuthorName: "a", AuthorEmail: "a@example.com"},
		"token over file": {URL: "file:///nowhere", Branch: "main", Token: "t", AuthorName: "a", AuthorEmail: "a@example.com"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() succeeded", name)
		}
		if _, err := NewRepoWriter(context.Background(), cfg); err == nil {
			t.Errorf("%s: NewRepoWriter() succeeded", name)
		}
	}
	ok := RepoConfig{URL: "https://github.com/example/catalog.git", Branch: "main", Token: "t", AuthorName: "a", AuthorEmail: "a@example.com"}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate(https with token) error = %v", err)
	}
}

// A file:// repository is served in-process: no git binary on PATH.
func TestRepoWriterFileURLWithoutGitBinary(t *testing.T) {
	url := "file://" + newBareRepo(t, map[string]string{"catalog/subscriptions/README": "keep"})
	t.Setenv("PATH", "")
	ctx := context.Background()
	w, err := NewRepoWriter(ctx, RepoConfig{URL: url, Branch: "main", AuthorName: "t", AuthorEmail: "t@example.com"})
	if err != nil {
		t.Fatalf("NewRepoWriter() error = %v", err)
	}
	head, err := w.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	if _, err := w.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"catalog/subscriptions/a.yaml": []byte("a")}, Message: "m"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	other, err := NewRepoWriter(ctx, RepoConfig{URL: url, Branch: "main", AuthorName: "t", AuthorEmail: "t@example.com"})
	if err != nil {
		t.Fatalf("second NewRepoWriter() error = %v", err)
	}
	head, _ = other.Head(ctx)
	if data, err := head.Read("catalog/subscriptions/a.yaml"); err != nil || string(data) != "a" {
		t.Fatalf("Read(a) = %q, %v", data, err)
	}
}

// TestRepoWriterConcurrentReadsAndCommits reads snapshots while other requests
// fetch and commit. Run it under -race: a snapshot read must not race with the
// writes to the in-memory clone.
func TestRepoWriterConcurrentReadsAndCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"catalog/subscriptions/a.yaml": "a"})
	w := newTestRepoWriter(t, url)

	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := range 3 {
				head, err := w.Head(ctx)
				if err != nil {
					errs <- err
					return
				}
				p := fmt.Sprintf("catalog/subscriptions/w%d-%d.yaml", i, j)
				_, err = w.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{p: []byte("x")}, Message: "add " + p})
				if err != nil && !errors.Is(err, ErrConflict) {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				head, err := w.Head(ctx)
				if err != nil {
					errs <- err
					return
				}
				names, err := head.List("catalog/subscriptions")
				if err != nil {
					errs <- err
					return
				}
				for _, name := range names {
					if _, err := head.Read("catalog/subscriptions/" + name); err != nil {
						errs <- err
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
