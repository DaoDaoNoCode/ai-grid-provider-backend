package gitops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// RepoConfig names the repository and branch a RepoWriter commits to.
type RepoConfig struct {
	URL    string
	Branch string
	// Token, when set, is sent as HTTPS basic auth, as GitHub expects a
	// personal access token. It is never logged.
	Token string
	// AuthorName and AuthorEmail sign every commit.
	AuthorName  string
	AuthorEmail string
}

// RepoWriter is a Writer over a remote repository. It keeps an in-memory
// clone, fetches before every Head and Commit, and pushes each commit. A push
// the remote rejects because the branch moved is ErrConflict.
type RepoWriter struct {
	mu     sync.Mutex
	cfg    RepoConfig
	auth   transport.AuthMethod
	repo   *git.Repository
	remote plumbing.ReferenceName
	local  plumbing.ReferenceName
}

var _ Writer = (*RepoWriter)(nil)

// NewRepoWriter clones cfg.Branch of cfg.URL into memory. The branch must
// exist and have at least one commit.
func NewRepoWriter(ctx context.Context, cfg RepoConfig) (*RepoWriter, error) {
	if cfg.URL == "" || cfg.Branch == "" {
		return nil, errors.New("gitops: repository URL and branch are required")
	}
	if cfg.AuthorName == "" || cfg.AuthorEmail == "" {
		return nil, errors.New("gitops: commit author name and email are required")
	}
	w := &RepoWriter{
		cfg:    cfg,
		remote: plumbing.NewRemoteReferenceName(git.DefaultRemoteName, cfg.Branch),
		local:  plumbing.NewBranchReferenceName(cfg.Branch),
	}
	if cfg.Token != "" {
		w.auth = &githttp.BasicAuth{Username: "x-access-token", Password: cfg.Token}
	}
	repo, err := git.CloneContext(ctx, memory.NewStorage(), memfs.New(), &git.CloneOptions{
		URL:           cfg.URL,
		Auth:          w.auth,
		ReferenceName: w.local,
		SingleBranch:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("gitops: clone branch %s: %w", cfg.Branch, err)
	}
	w.repo = repo
	return w, nil
}

// Head fetches the branch and returns its snapshot.
func (w *RepoWriter) Head(ctx context.Context) (Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	head, err := w.fetch(ctx)
	if err != nil {
		return nil, err
	}
	return w.snapshot(head)
}

// Commit fetches the branch, checks c's files against base, commits c on top
// of the branch head and pushes it.
func (w *RepoWriter) Commit(ctx context.Context, base string, c Change) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	head, err := w.fetch(ctx)
	if err != nil {
		return "", err
	}
	if !plumbing.IsHash(base) {
		return "", fmt.Errorf("gitops: invalid base revision %q", base)
	}
	baseHash := plumbing.NewHash(base)
	headTree, err := w.tree(head)
	if err != nil {
		return "", err
	}
	if baseHash != head {
		baseTree, err := w.tree(baseHash)
		if err != nil {
			return "", err
		}
		for _, p := range c.paths() {
			if blobHash(baseTree, p) != blobHash(headTree, p) {
				return "", ErrConflict
			}
		}
	}
	for _, p := range c.Delete {
		if blobHash(headTree, p) == plumbing.ZeroHash {
			return "", fmt.Errorf("%w: %s", ErrNotFound, p)
		}
	}

	if err := w.repo.Storer.SetReference(plumbing.NewHashReference(w.local, head)); err != nil {
		return "", fmt.Errorf("gitops: set branch: %w", err)
	}
	wt, err := w.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("gitops: worktree: %w", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: w.local, Force: true}); err != nil {
		return "", fmt.Errorf("gitops: checkout: %w", err)
	}
	for p, data := range c.Put {
		if err := util.WriteFile(wt.Filesystem, p, data, 0o644); err != nil {
			return "", fmt.Errorf("gitops: write %s: %w", p, err)
		}
		if _, err := wt.Add(p); err != nil {
			return "", fmt.Errorf("gitops: add %s: %w", p, err)
		}
	}
	for _, p := range c.Delete {
		if _, err := wt.Remove(p); err != nil {
			return "", fmt.Errorf("gitops: remove %s: %w", p, err)
		}
	}
	sig := &object.Signature{Name: w.cfg.AuthorName, Email: w.cfg.AuthorEmail, When: time.Now()}
	commit, err := wt.Commit(c.Message, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return "", fmt.Errorf("gitops: commit: %w", err)
	}

	err = w.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(w.local + ":" + w.local)},
		Auth:       w.auth,
	})
	if err != nil {
		if errors.Is(err, git.ErrNonFastForwardUpdate) {
			return "", ErrConflict
		}
		return "", fmt.Errorf("gitops: push: %w", err)
	}
	return commit.String(), nil
}

// fetch updates the remote-tracking branch and returns its head.
func (w *RepoWriter) fetch(ctx context.Context) (plumbing.Hash, error) {
	err := w.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + w.local + ":" + w.remote)},
		Auth:       w.auth,
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return plumbing.ZeroHash, fmt.Errorf("gitops: fetch: %w", err)
	}
	ref, err := w.repo.Reference(w.remote, true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitops: resolve %s: %w", w.remote, err)
	}
	return ref.Hash(), nil
}

func (w *RepoWriter) tree(h plumbing.Hash) (*object.Tree, error) {
	commit, err := w.repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("gitops: commit %s: %w", h, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("gitops: tree of %s: %w", h, err)
	}
	return tree, nil
}

func (w *RepoWriter) snapshot(h plumbing.Hash) (Snapshot, error) {
	tree, err := w.tree(h)
	if err != nil {
		return nil, err
	}
	return treeSnapshot{rev: h.String(), tree: tree}, nil
}

// blobHash returns the file's blob hash, or the zero hash when it is absent.
func blobHash(tree *object.Tree, p string) plumbing.Hash {
	entry, err := tree.FindEntry(p)
	if err != nil {
		return plumbing.ZeroHash
	}
	return entry.Hash
}

// treeSnapshot reads one commit's tree. Git objects are immutable, so it
// stays valid after later fetches and commits.
type treeSnapshot struct {
	rev  string
	tree *object.Tree
}

func (s treeSnapshot) Revision() string { return s.rev }

func (s treeSnapshot) Read(p string) ([]byte, error) {
	file, err := s.tree.File(p)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		return nil, fmt.Errorf("gitops: read %s: %w", p, err)
	}
	r, err := file.Reader()
	if err != nil {
		return nil, fmt.Errorf("gitops: read %s: %w", p, err)
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func (s treeSnapshot) List(dir string) ([]string, error) {
	sub, err := s.tree.Tree(path.Clean(dir))
	if err != nil {
		if errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("gitops: list %s: %w", dir, err)
	}
	var out []string
	for _, entry := range sub.Entries {
		if entry.Mode.IsFile() {
			out = append(out, entry.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}
