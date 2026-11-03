package gitops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

// go-git's file transport runs the git-upload-pack and git-receive-pack
// binaries, which the backend's image doesn't have. Serve file:// URLs and
// local paths in-process instead, with go-git's own server, so the backend can
// use a repository on a volume (Kind, make dev-fake) without git installed.
func init() {
	client.InstallProtocol("file", server.NewClient(server.DefaultLoader))
}

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
	// ReadMaxAge is how old a fetch may be for Head to return its head
	// instead of fetching again. Zero fetches on every Head. A commit
	// another writer pushes shows in Head within about ReadMaxAge, plus the
	// time one fetch takes; the RepoWriter's own commits show at once.
	ReadMaxAge time.Duration
}

// fetchTimeout bounds a fetch that Head callers share, waiting for the
// operation lock included. It runs on behalf of all of them, so no one
// caller's context can end it, and without a bound a remote that stops
// answering, or a Commit that holds the lock that long, would hold every
// later read.
const fetchTimeout = time.Minute

// RepoWriter is a Writer over a remote repository. It keeps an in-memory
// clone, fetches before every Commit, and pushes each commit. A push the
// remote rejects because the branch moved is ErrConflict.
//
// Head fetches only when the last fetch is cfg.ReadMaxAge old or older, and
// otherwise returns the head that fetch saw. A fetch can cost seconds even
// when nothing changed: from the demo hub's backend in 2026-11, GitHub's
// info/refs took about 2 s to the first byte and an up-to-date fetch about
// 3 s. Every catalog read used to pay that, one read at a time. Reads that
// need a fetch while one is running wait for it and share its head instead
// of fetching again (fetchCall). A Commit records the
// head it fetched and, once pushed, its own commit, so the next read sees the
// write without a fetch. With ReadMaxAge zero every Head fetches, as before.
//
// Locking, outermost first:
//
//   - op, the operation lock, serializes the operations that change the
//     clone's references, index and worktree: every fetch and every Commit.
//     It is held across the network, so a Commit and a fetch wait for each
//     other, but a read that finds a recent fetch never takes it. It is a
//     one-slot channel rather than a sync.Mutex so that waiting for it ends
//     with the waiter's context (lockOp).
//   - mu guards the read cache: head, headAsOf and fetching. It is never
//     held across the network or while waiting for op.
//   - the storage's own lock guards its maps for one call at a time, so a
//     snapshot reads files while a fetch or a commit adds objects
//     (lockedStorage).
type RepoWriter struct {
	cfg    RepoConfig
	auth   transport.AuthMethod
	repo   *git.Repository
	remote plumbing.ReferenceName
	local  plumbing.ReferenceName
	now    func() time.Time

	// op holds a value while an operation has the clone (lockOp).
	op chan struct{}

	mu sync.Mutex
	// head is the branch head the last fetch or push saw, and headAsOf when
	// that fetch or push started: the remote was at head at headAsOf or
	// later. A zero headAsOf means no head is known, and Head fetches.
	head     plumbing.Hash
	headAsOf time.Time
	// fetching is the fetch Head callers share while it runs, or nil.
	fetching *fetchCall

	// beforePush, when set, runs between the commit and its push. Tests use
	// it to move the remote branch in that gap.
	beforePush func()
	// beforeFetch, when set, runs before each fetch goes to the remote, and
	// onShare whenever a Head waits for a fetch another Head started. Tests
	// use them to count fetches and to hold one while reads pile up.
	beforeFetch func()
	onShare     func()
	// fetchContext, when set, makes the context a shared fetch runs in
	// instead of a fetchTimeout bound, so a test can end it at will.
	fetchContext func(context.Context) (context.Context, context.CancelFunc)
}

// fetchCall is one fetch that every Head arriving while it runs waits for.
// head and err are set before done is closed.
type fetchCall struct {
	started time.Time
	done    chan struct{}
	head    plumbing.Hash
	err     error
}

var _ Writer = (*RepoWriter)(nil)

// Validate checks that cfg names a repository, a branch and an author, and
// that a token is sent only over https.
func (cfg RepoConfig) Validate() error {
	if cfg.URL == "" || cfg.Branch == "" {
		return errors.New("gitops: repository URL and branch are required")
	}
	if cfg.AuthorName == "" || cfg.AuthorEmail == "" {
		return errors.New("gitops: commit author name and email are required")
	}
	if cfg.ReadMaxAge < 0 {
		return errors.New("gitops: the read max age must not be negative")
	}
	if cfg.Token != "" {
		if u, err := url.Parse(cfg.URL); err != nil || u.Scheme != "https" {
			return errors.New("gitops: a token is sent only over https; the repository URL must start with https://")
		}
	}
	return nil
}

// NewRepoWriter clones cfg.Branch of cfg.URL into memory. The branch must
// exist and have at least one commit.
func NewRepoWriter(ctx context.Context, cfg RepoConfig) (*RepoWriter, error) {
	return newRepoWriter(ctx, cfg, time.Now)
}

// newRepoWriter is NewRepoWriter on the clock now, which tells how old a
// fetch is.
func newRepoWriter(ctx context.Context, cfg RepoConfig, now func() time.Time) (*RepoWriter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	w := &RepoWriter{
		cfg:    cfg,
		remote: plumbing.NewRemoteReferenceName(git.DefaultRemoteName, cfg.Branch),
		local:  plumbing.NewBranchReferenceName(cfg.Branch),
		now:    now,
		op:     make(chan struct{}, 1),
	}
	if cfg.Token != "" {
		w.auth = &githttp.BasicAuth{Username: "x-access-token", Password: cfg.Token}
	}
	clonedAt := now()
	repo, err := git.CloneContext(ctx, newLockedStorage(), memfs.New(), &git.CloneOptions{
		URL:           cfg.URL,
		Auth:          w.auth,
		ReferenceName: w.local,
		SingleBranch:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("gitops: clone branch %s: %w", cfg.Branch, err)
	}
	w.repo = repo
	// The clone counts as the first fetch. Without a remote-tracking branch
	// the first Head fetches instead.
	if ref, err := repo.Reference(w.remote, true); err == nil {
		w.remember(ref.Hash(), clonedAt)
	}
	return w, nil
}

// Head returns the snapshot of the branch head. It fetches the branch when
// the last fetch or push is cfg.ReadMaxAge old or older, sharing the fetch
// with every Head that arrives while it runs; with ReadMaxAge zero it always
// fetches.
func (w *RepoWriter) Head(ctx context.Context) (Snapshot, error) {
	if w.cfg.ReadMaxAge <= 0 {
		if err := w.lockOp(ctx); err != nil {
			return nil, fmt.Errorf("gitops: fetch: %w", err)
		}
		head, err := w.fetch(ctx)
		w.unlockOp()
		if err != nil {
			return nil, err
		}
		return w.snapshot(head)
	}

	w.mu.Lock()
	if !w.headAsOf.IsZero() && w.now().Sub(w.headAsOf) < w.cfg.ReadMaxAge {
		head := w.head
		w.mu.Unlock()
		return w.snapshot(head)
	}
	call := w.fetching
	shared := call != nil
	if !shared {
		call = &fetchCall{started: w.now(), done: make(chan struct{})}
		w.fetching = call
		// The fetch serves every Head that arrives while it runs, so it
		// must not end when this caller gives up: it keeps ctx's values but
		// not its cancellation, and has a bound of its own.
		go w.sharedFetch(context.WithoutCancel(ctx), call)
	}
	w.mu.Unlock()
	if shared && w.onShare != nil {
		w.onShare()
	}

	select {
	case <-call.done:
	case <-ctx.Done():
		return nil, fmt.Errorf("gitops: fetch: %w", ctx.Err())
	}
	if call.err != nil {
		return nil, call.err
	}
	return w.snapshot(call.head)
}

// sharedFetch runs call's fetch, then lets its waiters go. fetchTimeout
// bounds the whole call: when it runs out while the call still waits for the
// operation lock, the call ends with that error, and the next Head starts a
// fetch of its own.
func (w *RepoWriter) sharedFetch(ctx context.Context, call *fetchCall) {
	ctx, cancel := w.sharedFetchContext(ctx)
	defer cancel()

	head, err := w.fetchUnlessFresh(ctx, call.started)

	w.mu.Lock()
	call.head, call.err = head, err
	w.fetching = nil
	w.mu.Unlock()
	close(call.done)
}

// fetchUnlessFresh takes the operation lock and fetches, unless an operation
// that held the lock while this one waited fetched or pushed a head at
// started or later: that head is as recent as this fetch would see.
func (w *RepoWriter) fetchUnlessFresh(ctx context.Context, started time.Time) (plumbing.Hash, error) {
	if err := w.lockOp(ctx); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitops: fetch: %w", err)
	}
	defer w.unlockOp()
	w.mu.Lock()
	head := w.head
	fresh := !w.headAsOf.IsZero() && !w.headAsOf.Before(started)
	w.mu.Unlock()
	if fresh {
		return head, nil
	}
	return w.fetch(ctx)
}

// sharedFetchContext returns the context a shared fetch runs in: ctx bounded
// by fetchTimeout, or what the test hook fetchContext makes of ctx.
func (w *RepoWriter) sharedFetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if w.fetchContext != nil {
		return w.fetchContext(ctx)
	}
	return context.WithTimeout(ctx, fetchTimeout)
}

// lockOp takes the operation lock, or gives up with ctx's cause when ctx
// ends first. Every lockOp that returns nil is paired with an unlockOp.
func (w *RepoWriter) lockOp(ctx context.Context) error {
	select {
	case w.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (w *RepoWriter) unlockOp() { <-w.op }

// remember records that the remote branch was at head at asOf or later.
func (w *RepoWriter) remember(head plumbing.Hash, asOf time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.head, w.headAsOf = head, asOf
}

// forget makes the next Head fetch: the remote branch may be at a head this
// clone hasn't seen.
func (w *RepoWriter) forget() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.head, w.headAsOf = plumbing.ZeroHash, time.Time{}
}

// Commit fetches the branch, checks c's files against base, commits c on top
// of the branch head and pushes it. It fetches whatever cfg.ReadMaxAge is:
// the check against base must see the remote's head.
func (w *RepoWriter) Commit(ctx context.Context, base string, c Change) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	if err := w.lockOp(ctx); err != nil {
		return "", fmt.Errorf("gitops: commit: %w", err)
	}
	defer w.unlockOp()

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
		for _, p := range c.checked() {
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

	if w.beforePush != nil {
		w.beforePush()
	}
	pushedAt := w.now()
	err = w.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(w.local + ":" + w.local)},
		Auth:       w.auth,
	})
	if err != nil {
		// The commit never reached the remote: point the branch back at the
		// head it was made on. Otherwise every later fetch offers the server
		// that commit as one the clone has, and go-git's own server, which
		// serves file:// here, fails the fetch on a commit it doesn't know.
		if rerr := w.repo.Storer.SetReference(plumbing.NewHashReference(w.local, head)); rerr != nil {
			err = errors.Join(err, fmt.Errorf("reset branch: %w", rerr))
		}
		// A push that lost a race left the branch at a commit this clone
		// hasn't fetched, and one that failed otherwise, on a timeout say,
		// may still have landed: either way the head fetched above is no
		// longer known to be the remote's.
		w.forget()
		if pushRaced(err) {
			return "", fmt.Errorf("%w: push: %w", ErrConflict, err)
		}
		return "", fmt.Errorf("gitops: push: %w", err)
	}
	// The push moved the remote branch from head to commit, so the remote
	// was at commit when the push started or later.
	w.remember(commit, pushedAt)
	return commit.String(), nil
}

// pushRaced reports whether a push failed because the remote branch moved
// after the fetch. go-git's own check answers a plain error, "non-fast-forward
// update: <ref>", not git.ErrNonFastForwardUpdate (that one is Pull's); a
// server that rejects the ref update reports "non-fast-forward" or "cannot
// lock ref" in the push report.
func pushRaced(err error) bool {
	msg := err.Error()
	return errors.Is(err, git.ErrNonFastForwardUpdate) ||
		strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "cannot lock ref")
}

// fetch updates the remote-tracking branch, records its head for Head and
// returns it. The caller holds the operation lock.
func (w *RepoWriter) fetch(ctx context.Context) (plumbing.Hash, error) {
	if w.beforeFetch != nil {
		w.beforeFetch()
	}
	started := w.now()
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
	w.remember(ref.Hash(), started)
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
	return treeSnapshot{rev: h.String(), tree: tree, mu: new(sync.Mutex)}, nil
}

// blobHash returns the hash of the file or directory at p, or the zero hash
// when it is absent.
func blobHash(tree *object.Tree, p string) plumbing.Hash {
	entry, err := tree.FindEntry(p)
	if err != nil {
		return plumbing.ZeroHash
	}
	return entry.Hash
}

// treeSnapshot reads one commit's tree. Git objects are immutable, so it
// stays valid after later fetches and commits. Reading them goes through the
// clone's in-memory storage, which fetches and commits write to while the
// snapshot reads: lockedStorage makes that safe. mu is the snapshot's own:
// object.Tree fills a lookup map on first use, so two reads of one snapshot
// at once would race on it.
type treeSnapshot struct {
	rev  string
	tree *object.Tree
	mu   *sync.Mutex
}

func (s treeSnapshot) Revision() string { return s.rev }

func (s treeSnapshot) Read(p string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
