package gitops

import (
	"context"
	"errors"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// fakeClock is a RepoWriter's now that moves only when a test says so.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newCachingRepoWriter clones url with reads served from a fetch at most
// maxAge old, on a fake clock, and counts the fetches it makes after the
// clone: the shared fetches of Head and the fetch every Commit makes.
func newCachingRepoWriter(t *testing.T, url string, maxAge time.Duration) (*RepoWriter, *fakeClock, *atomic.Int32) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	w, err := newRepoWriter(context.Background(), RepoConfig{
		URL: url, Branch: "main", AuthorName: "ai-grid-provider-backend", AuthorEmail: "backend@example.com",
		ReadMaxAge: maxAge,
	}, clock.Now)
	if err != nil {
		t.Fatalf("newRepoWriter() error = %v", err)
	}
	fetches := new(atomic.Int32)
	w.beforeFetch = func() { fetches.Add(1) }
	return w, clock, fetches
}

func mustHead(t *testing.T, w Writer) Snapshot {
	t.Helper()
	head, err := w.Head(context.Background())
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	return head
}

func mustRead(t *testing.T, s Snapshot, p, want string) {
	t.Helper()
	if data, err := s.Read(p); err != nil || string(data) != want {
		t.Fatalf("Read(%s) at %s = %q, %v; want %q", p, s.Revision(), data, err, want)
	}
}

// Reads within the window are served from the clone's last fetch, without
// going to the remote; the clone itself counts as that fetch.
func TestRepoWriterReadsWithinMaxAgeDoNotFetch(t *testing.T) {
	t.Parallel()
	w, clock, fetches := newCachingRepoWriter(t, newBareRepo(t, map[string]string{"a": "a"}), 10*time.Second)
	first := mustHead(t, w)
	for range 3 {
		clock.Advance(3 * time.Second)
		head := mustHead(t, w)
		if head.Revision() != first.Revision() {
			t.Fatalf("Head() = %s, want the cloned head %s", head.Revision(), first.Revision())
		}
		mustRead(t, head, "a", "a")
	}
	if n := fetches.Load(); n != 0 {
		t.Fatalf("%d fetches for reads within the window, want 0", n)
	}
}

// A commit another writer pushes shows once the window has passed: before,
// reads keep the head this writer fetched; after, one read fetches and the
// reads after it reuse that fetch.
func TestRepoWriterOtherWritersChangeAppearsAfterMaxAge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, clock, fetches := newCachingRepoWriter(t, url, 10*time.Second)
	other := newTestRepoWriter(t, url)

	before := mustHead(t, w)
	rev, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "other"})
	if err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}

	clock.Advance(10*time.Second - time.Nanosecond)
	if head := mustHead(t, w); head.Revision() != before.Revision() {
		t.Fatalf("Head() within the window = %s, want the fetched %s", head.Revision(), before.Revision())
	}
	if n := fetches.Load(); n != 0 {
		t.Fatalf("%d fetches within the window, want 0", n)
	}

	clock.Advance(time.Nanosecond)
	head := mustHead(t, w)
	if head.Revision() != rev {
		t.Fatalf("Head() after the window = %s, want the other writer's %s", head.Revision(), rev)
	}
	mustRead(t, head, "a", "a2")
	mustHead(t, w)
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1: the read after the window, reused by the next", n)
	}
}

// Reads that find the last fetch too old while another read is fetching
// wait for that fetch instead of making their own.
func TestRepoWriterConcurrentReadsShareOneFetch(t *testing.T) {
	t.Parallel()
	const readers = 8
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, clock, fetches := newCachingRepoWriter(t, url, 10*time.Second)
	other := newTestRepoWriter(t, url)
	rev, err := other.Commit(context.Background(), mustHead(t, other).Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "other"})
	if err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}
	clock.Advance(time.Minute)

	// The first reader's fetch waits until every other reader has joined it.
	release := make(chan struct{})
	w.beforeFetch = func() {
		fetches.Add(1)
		<-release
	}
	var joined sync.WaitGroup
	joined.Add(readers - 1)
	w.onShare = joined.Done

	revs := make(chan string, readers)
	errs := make(chan error, readers)
	var fresh atomic.Int32
	var done sync.WaitGroup
	for range readers {
		done.Go(func() {
			head, err := w.Head(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if head.Fresh() {
				fresh.Add(1)
			}
			if data, err := head.Read("a"); err != nil || string(data) != "a2" {
				errs <- errors.New("a reader did not see the fetched a2")
				return
			}
			revs <- head.Revision()
		})
	}
	joined.Wait()
	close(release)
	done.Wait()
	close(revs)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for got := range revs {
		if got != rev {
			t.Errorf("Head() = %s, want %s", got, rev)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches for %d concurrent reads, want 1", n, readers)
	}
	// Only the reader that started the fetch knows it began after its call;
	// the others joined a fetch that may have started before theirs, though
	// the clock didn't move between them.
	if n := fresh.Load(); n != 1 {
		t.Fatalf("%d of %d snapshots Fresh, want 1: the one whose call started the fetch", n, readers)
	}
}

// A commit within the window builds on the head the last fetch saw, without
// a fetch of its own, and lands on the remote; it is the head the next read
// sees, again without a fetch.
func TestRepoWriterCommitIsReadWithoutFetch(t *testing.T) {
	t.Parallel()
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, _, fetches := newCachingRepoWriter(t, url, 10*time.Second)
	rev, err := w.Commit(context.Background(), mustHead(t, w).Revision(), Change{Put: map[string][]byte{"b": []byte("b")}, Message: "m"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if n := fetches.Load(); n != 0 {
		t.Fatalf("%d fetches for a commit within the window, want 0: it pushes only", n)
	}
	head := mustHead(t, w)
	if head.Revision() != rev {
		t.Fatalf("Head() after Commit() = %s, want the commit %s", head.Revision(), rev)
	}
	mustRead(t, head, "b", "b")
	if n := fetches.Load(); n != 0 {
		t.Fatalf("%d fetches after reading the commit, want still 0", n)
	}
	if remote := mustHead(t, newTestRepoWriter(t, url)); remote.Revision() != rev {
		t.Fatalf("remote head = %s, want the commit %s", remote.Revision(), rev)
	}
}

// A commit made after the window has passed fetches first, and lands on top
// of what another writer pushed meanwhile, in one attempt.
func TestRepoWriterCommitAfterMaxAgeFetches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w, clock, fetches := newCachingRepoWriter(t, url, 10*time.Second)
	other := newTestRepoWriter(t, url)
	base := mustHead(t, w)
	if _, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "other"}); err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}
	clock.Advance(10 * time.Second)
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"}); err != nil {
		t.Fatalf("Commit() after the window error = %v", err)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches for a commit after the window, want 1", n)
	}
	head := mustHead(t, newTestRepoWriter(t, url))
	mustRead(t, head, "a", "a2")
	mustRead(t, head, "b", "b2")
}

// Reads that need a fetch while a Commit runs wait for it and take the head
// it pushed: it is as recent as their own fetch would be.
func TestRepoWriterReadsWaitingOnCommitTakeItsHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w, clock, fetches := newCachingRepoWriter(t, newBareRepo(t, map[string]string{"a": "a"}), 10*time.Second)
	base := mustHead(t, w)
	clock.Advance(time.Minute)

	// The commit holds the clone, in its own fetch, until the reads wait.
	commitFetching := make(chan struct{})
	release := make(chan struct{})
	w.beforeFetch = func() {
		if fetches.Add(1) == 1 {
			close(commitFetching)
			<-release
		}
	}
	committed := make(chan string, 1)
	go func() {
		rev, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"})
		if err != nil {
			t.Errorf("Commit() error = %v", err)
		}
		committed <- rev
	}()
	<-commitFetching

	// Two reads: whichever comes second joins the first one's fetch, so once
	// one has joined, that fetch exists and waits for the commit.
	var joined sync.WaitGroup
	joined.Add(1)
	w.onShare = joined.Done
	revs := make(chan string, 2)
	for range 2 {
		go func() {
			head, err := w.Head(ctx)
			if err != nil {
				t.Errorf("Head() error = %v", err)
				revs <- ""
				return
			}
			revs <- head.Revision()
		}()
	}
	joined.Wait()
	close(release)

	rev := <-committed
	for range 2 {
		if got := <-revs; got != rev {
			t.Errorf("Head() waiting on a commit = %s, want the commit %s", got, rev)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1: the commit's, which the reads reuse", n)
	}
}

// writeAppending appends '+' to each of files in one commit on w, rerunning
// on conflict, and returns the error of each attempt.
func writeAppending(ctx context.Context, w Writer, files ...string) ([]error, error) {
	var attempts []error
	err := RetryOnConflict(ctx, func() error {
		err := func() error {
			head, err := w.Head(ctx)
			if err != nil {
				return err
			}
			put := map[string][]byte{}
			for _, p := range files {
				data, err := head.Read(p)
				if err != nil {
					return err
				}
				put[p] = append(data, '+')
			}
			_, err = w.Commit(ctx, head.Revision(), Change{Put: put, Message: "m"})
			return err
		}()
		attempts = append(attempts, err)
		return err
	})
	return attempts, err
}

// A commit within the window builds on the head it last saw. When another
// writer has pushed since, even to files the commit doesn't touch, the push,
// which requires the remote branch to be at that head, is refused, and
// Commit answers ErrConflict and forgets the head: the rerun (RetryOnConflict) reads a
// fetched head and commits on top of the other writer's change, which stays.
func TestRepoWriterCommitOnOutdatedHeadConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w, _, fetches := newCachingRepoWriter(t, url, time.Hour)
	other := newTestRepoWriter(t, url)
	if _, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "other"}); err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}

	attempts, err := writeAppending(ctx, w, "a")
	if err != nil {
		t.Fatalf("RetryOnConflict() error = %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want 2: one on the outdated head, then one on the fetched head", len(attempts))
	}
	if first := attempts[0]; !errors.Is(first, ErrConflict) || !strings.Contains(first.Error(), "required to be") {
		t.Fatalf("first attempt error = %v, want ErrConflict from the refused push", first)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1: the rerun's read; neither commit fetches", n)
	}
	head := mustHead(t, newTestRepoWriter(t, url))
	mustRead(t, head, "a", "a+")
	mustRead(t, head, "b", "b2")
}

// A branch reset to an older commit within the window is respected: a commit
// on the head this writer last saw would be a fast-forward of the reset
// branch, but the push requires the branch to be at that head, so it is
// refused as ErrConflict, and the rerun commits on the reset branch without
// bringing back what the reset removed.
func TestRepoWriterCommitRespectsRemoteReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	remote, err := git.PlainOpen(url)
	if err != nil {
		t.Fatalf("open the remote: %v", err)
	}
	main := plumbing.NewBranchReferenceName("main")
	seeded, err := remote.Reference(main, true)
	if err != nil {
		t.Fatalf("remote main: %v", err)
	}
	other := newTestRepoWriter(t, url)
	if _, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "to be reset"}); err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}
	w, _, fetches := newCachingRepoWriter(t, url, time.Hour)
	mustRead(t, mustHead(t, w), "b", "b2")
	if err := remote.Storer.SetReference(plumbing.NewHashReference(main, seeded.Hash())); err != nil {
		t.Fatalf("reset the remote: %v", err)
	}

	attempts, err := writeAppending(ctx, w, "a")
	if err != nil {
		t.Fatalf("RetryOnConflict() error = %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want 2: one on the head before the reset, then one on the reset branch", len(attempts))
	}
	if first := attempts[0]; !errors.Is(first, ErrConflict) || !strings.Contains(first.Error(), "required to be") {
		t.Fatalf("first attempt error = %v, want ErrConflict from the refused push", first)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1: the rerun's read", n)
	}
	head := mustHead(t, newTestRepoWriter(t, url))
	mustRead(t, head, "a", "a+")
	mustRead(t, head, "b", "b")
}

// FreshHead fetches within the window, and shows a commit another writer
// pushed that Head doesn't show yet; the head it fetched serves the reads
// after it. Head's snapshots are Fresh only when they come from a fetch that
// started after the call began.
func TestRepoWriterFreshHeadFetchesWithinMaxAge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, clock, fetches := newCachingRepoWriter(t, url, time.Hour)
	other := newTestRepoWriter(t, url)
	rev, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "other"})
	if err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}

	cached := mustHead(t, w)
	if cached.Revision() == rev || cached.Fresh() {
		t.Fatalf("Head() within the window = %s, Fresh() = %v; want the cloned head, not fresh", cached.Revision(), cached.Fresh())
	}
	clock.Advance(time.Nanosecond)
	fresh, err := w.FreshHead(ctx)
	if err != nil {
		t.Fatalf("FreshHead() error = %v", err)
	}
	if fresh.Revision() != rev || !fresh.Fresh() {
		t.Fatalf("FreshHead() = %s, Fresh() = %v; want the other writer's %s, fresh", fresh.Revision(), fresh.Fresh(), rev)
	}
	mustRead(t, fresh, "a", "a2")
	if head := mustHead(t, w); head.Revision() != rev {
		t.Fatalf("Head() after FreshHead() = %s, want %s", head.Revision(), rev)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1: FreshHead's", n)
	}

	clock.Advance(time.Hour)
	if head := mustHead(t, w); !head.Fresh() {
		t.Fatal("Head() that fetched after the window is not Fresh")
	}
	zero, _, _ := newCachingRepoWriter(t, url, 0)
	if head := mustHead(t, zero); !head.Fresh() {
		t.Fatal("Head() with no window is not Fresh")
	}
}

// With no window a commit fetches first, as before the window existed: a
// commit on a base another writer has moved past, to files it didn't touch,
// lands in one attempt.
func TestRepoWriterMaxAgeZeroCommitFetchesFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w, _, fetches := newCachingRepoWriter(t, url, 0)
	other := newTestRepoWriter(t, url)
	base := mustHead(t, w)
	if _, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "other"}); err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}
	before := fetches.Load()
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"}); err != nil {
		t.Fatalf("Commit() on a base the remote moved past error = %v", err)
	}
	if n := fetches.Load() - before; n != 1 {
		t.Fatalf("%d fetches for a commit with no window, want 1", n)
	}
	head := mustHead(t, newTestRepoWriter(t, url))
	mustRead(t, head, "a", "a2")
	mustRead(t, head, "b", "b2")
}

// A push that loses a race leaves the remote at a head this writer never
// saw, so the next read fetches instead of serving the head the commit was
// made on.
func TestRepoWriterLostPushForgetsHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w, _, fetches := newCachingRepoWriter(t, url, time.Hour)
	other := newTestRepoWriter(t, url)
	w.beforePush = func() {
		head, _ := other.Head(ctx)
		if _, err := other.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "other"}); err != nil {
			t.Errorf("other Commit() error = %v", err)
		}
	}
	if _, err := w.Commit(ctx, mustHead(t, w).Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit() racing a push error = %v, want ErrConflict", err)
	}
	w.beforePush = nil
	before := fetches.Load()
	head := mustHead(t, w)
	mustRead(t, head, "a", "a")
	mustRead(t, head, "b", "b2")
	if n := fetches.Load() - before; n != 1 {
		t.Fatalf("%d fetches for the read after a lost push, want 1", n)
	}
}

// A failed fetch is not remembered: every reader waiting on it gets its
// error, and the next read tries again.
func TestRepoWriterFailedFetchIsRetried(t *testing.T) {
	t.Parallel()
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, clock, fetches := newCachingRepoWriter(t, url, 10*time.Second)
	clock.Advance(time.Minute)

	if err := os.Rename(url, url+".gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Head(context.Background()); err == nil {
		t.Fatal("Head() with the remote gone succeeded")
	}
	if err := os.Rename(url+".gone", url); err != nil {
		t.Fatal(err)
	}
	mustRead(t, mustHead(t, w), "a", "a")
	if n := fetches.Load(); n != 2 {
		t.Fatalf("%d fetches, want 2: the failed one and the retry", n)
	}
}

// A reader that gives up while waiting for a shared fetch gets its own
// context's error; the fetch goes on for the others, and its head serves
// the reads after it.
func TestRepoWriterReaderGivesUpOnSharedFetch(t *testing.T) {
	t.Parallel()
	w, clock, fetches := newCachingRepoWriter(t, newBareRepo(t, map[string]string{"a": "a"}), 10*time.Second)
	clock.Advance(time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	w.beforeFetch = func() {
		fetches.Add(1)
		close(started)
		<-release
	}

	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := w.Head(ctx)
		gaveUp <- err
	}()
	<-started
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("Head() with its context canceled error = %v, want context.Canceled", err)
	}

	var joined sync.WaitGroup
	joined.Add(1)
	w.onShare = joined.Done
	result := make(chan error, 1)
	go func() {
		_, err := w.Head(context.Background())
		result <- err
	}()
	joined.Wait()
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Head() sharing the fetch error = %v", err)
	}
	mustRead(t, mustHead(t, w), "a", "a")
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
}

// A shared fetch's bound covers its wait for the clone: one that runs out
// while a Commit holds the clone ends with that error, its readers get it,
// and the next read starts a fetch of its own instead of joining the expired
// one. A Commit waiting for the clone gives up with its own context too.
func TestRepoWriterSharedFetchExpiresWaitingForCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w, clock, _ := newCachingRepoWriter(t, newBareRepo(t, map[string]string{"a": "a"}), 10*time.Second)
	base := mustHead(t, w)

	// The commit holds the clone, before its push, until released.
	pushing := make(chan struct{})
	release := make(chan struct{})
	w.beforePush = func() {
		close(pushing)
		<-release
	}
	committed := make(chan string, 1)
	go func() {
		rev, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"})
		if err != nil {
			t.Errorf("Commit() error = %v", err)
		}
		committed <- rev
	}()
	<-pushing
	// The head the commit fetched is too old for the reads below.
	clock.Advance(time.Minute)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.Commit(canceled, base.Revision(), Change{Put: map[string][]byte{"b": []byte("b")}, Message: "m"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit() waiting for the clone with its context canceled error = %v, want context.Canceled", err)
	}

	// Each shared fetch's bound is one the test ends, as if it had run out.
	expire := make(chan context.CancelCauseFunc, 2)
	w.fetchContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancelCause(ctx)
		expire <- cancel
		return ctx, func() { cancel(context.Canceled) }
	}
	joined := make(chan struct{}, 1)
	w.onShare = func() { joined <- struct{}{} }

	first := make(chan error, 1)
	go func() {
		_, err := w.Head(ctx)
		first <- err
	}()
	(<-expire)(context.DeadlineExceeded)
	if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Head() whose fetch ran out waiting for the commit error = %v, want context.DeadlineExceeded", err)
	}

	second := make(chan string, 1)
	go func() {
		head, err := w.Head(ctx)
		if err != nil {
			t.Errorf("Head() after the expired fetch error = %v", err)
			second <- ""
			return
		}
		second <- head.Revision()
	}()
	select {
	case <-expire:
	case <-joined:
		t.Fatal("the read after the expired fetch joined it instead of starting its own")
	}

	close(release)
	rev := <-committed
	if got := <-second; got != rev {
		t.Fatalf("Head() after the commit = %s, want the commit %s", got, rev)
	}
}

// With no window every read fetches, as before the window existed, and a
// commit another writer pushed shows at once.
func TestRepoWriterMaxAgeZeroFetchesEveryRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, _, fetches := newCachingRepoWriter(t, url, 0)
	other := newTestRepoWriter(t, url)

	mustHead(t, w)
	mustHead(t, w)
	if n := fetches.Load(); n != 2 {
		t.Fatalf("%d fetches for 2 reads, want 2", n)
	}
	rev, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "other"})
	if err != nil {
		t.Fatalf("other Commit() error = %v", err)
	}
	if head := mustHead(t, w); head.Revision() != rev {
		t.Fatalf("Head() = %s, want the other writer's %s at once", head.Revision(), rev)
	}
}

// One snapshot read from many goroutines at once: object.Tree fills its
// lookup map on first use, which treeSnapshot's own lock serializes. Run it
// under -race.
func TestRepoWriterSnapshotConcurrentReadAndList(t *testing.T) {
	t.Parallel()
	const readers = 8
	files := map[string]string{}
	for _, dir := range []string{"catalog/models", "catalog/subscriptions", "catalog/clusters/site-a"} {
		for _, name := range []string{"a.yaml", "b.yaml", "c.yaml"} {
			files[dir+"/"+name] = dir + "/" + name
		}
	}
	w, _, _ := newCachingRepoWriter(t, newBareRepo(t, files), time.Hour)
	head := mustHead(t, w)

	start := make(chan struct{})
	var done sync.WaitGroup
	for i := range readers {
		done.Go(func() {
			<-start
			for p, want := range files {
				if data, err := head.Read(p); err != nil || string(data) != want {
					t.Errorf("reader %d: Read(%s) = %q, %v; want %q", i, p, data, err, want)
					return
				}
				if names, err := head.List(path.Dir(p)); err != nil || !slices.Contains(names, path.Base(p)) {
					t.Errorf("reader %d: List(%s) = %v, %v; want %s in it", i, path.Dir(p), names, err, path.Base(p))
					return
				}
			}
		})
	}
	close(start)
	done.Wait()
}

// BenchmarkRepoWriterConcurrentStaleReads measures ten reads that arrive
// together after the window has passed, with each fetch slowed down to
// stand in for a remote Git host. Without a window the reads fetch one
// after another; with one they share a single fetch.
//
//	go test ./internal/gitops -run '^$' -bench ConcurrentStaleReads
func BenchmarkRepoWriterConcurrentStaleReads(b *testing.B) {
	const readers = 10
	const fetchDelay = 20 * time.Millisecond
	url := newBareRepo(b, map[string]string{"a": "a"})
	for _, maxAge := range []time.Duration{0, 10 * time.Second} {
		b.Run("max-age="+maxAge.String(), func(b *testing.B) {
			clock := &fakeClock{t: time.Now()}
			w, err := newRepoWriter(context.Background(), RepoConfig{
				URL: url, Branch: "main", AuthorName: "a", AuthorEmail: "a@example.com", ReadMaxAge: maxAge,
			}, clock.Now)
			if err != nil {
				b.Fatal(err)
			}
			w.beforeFetch = func() { time.Sleep(fetchDelay) }
			for b.Loop() {
				clock.Advance(time.Minute)
				var wg sync.WaitGroup
				for range readers {
					wg.Go(func() {
						if _, err := w.Head(context.Background()); err != nil {
							b.Error(err)
						}
					})
				}
				wg.Wait()
			}
		})
	}
}

// BenchmarkRepoWriterParallelReads measures reads that find a fresh fetch,
// from many goroutines at once: no network, no shared lock across reads.
func BenchmarkRepoWriterParallelReads(b *testing.B) {
	url := newBareRepo(b, map[string]string{"catalog/models/a.yaml": "a"})
	w, err := newRepoWriter(context.Background(), RepoConfig{
		URL: url, Branch: "main", AuthorName: "a", AuthorEmail: "a@example.com", ReadMaxAge: time.Hour,
	}, time.Now)
	if err != nil {
		b.Fatal(err)
	}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			head, err := w.Head(context.Background())
			if err != nil {
				b.Error(err)
				return
			}
			if _, err := head.Read("catalog/models/a.yaml"); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// A push the server refuses after it advertised the branch, because another
// writer moved it in between, is a conflict whatever the server's git says
// about it ("incorrect old value provided" from git 2.54, "failed to update
// ref" from older ones): Commit reads where the remote branch is instead of
// matching the error's text, and the rerun commits on top of the other
// writer's change.
func TestRepoWriterServerSideRaceIsAConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w, _, _ := newCachingRepoWriter(t, url, time.Hour)
	other := newTestRepoWriter(t, url)
	w.push = func(context.Context, *git.PushOptions) error {
		w.push = nil
		if _, err := other.Commit(ctx, mustHead(t, other).Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "other"}); err != nil {
			t.Errorf("other Commit() error = %v", err)
		}
		return errors.New("command error on refs/heads/main: incorrect old value provided")
	}

	attempts, err := writeAppending(ctx, w, "a")
	if err != nil {
		t.Fatalf("RetryOnConflict() error = %v", err)
	}
	if len(attempts) != 2 || !errors.Is(attempts[0], ErrConflict) {
		t.Fatalf("attempts = %v, want ErrConflict, then success on the moved branch", attempts)
	}
	head := mustHead(t, newTestRepoWriter(t, url))
	mustRead(t, head, "a", "a+")
	mustRead(t, head, "b", "b2")
}

// A push that landed although its answer was lost, cut off by a timeout say,
// is the commit: Commit reads the remote branch at it and returns it, rather
// than an error that would have the caller write it again.
func TestRepoWriterPushThatLandedIsTheCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, _, _ := newCachingRepoWriter(t, url, time.Hour)
	w.push = func(ctx context.Context, opts *git.PushOptions) error {
		if err := w.repo.PushContext(ctx, opts); err != nil {
			return err
		}
		return errors.New("context deadline exceeded")
	}

	rev, err := w.Commit(ctx, mustHead(t, w).Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "landed"})
	if err != nil {
		t.Fatalf("Commit() error = %v, want the commit that landed", err)
	}
	remote := mustHead(t, newTestRepoWriter(t, url))
	if remote.Revision() != rev {
		t.Fatalf("remote head = %s, want the returned commit %s", remote.Revision(), rev)
	}
	mustRead(t, remote, "a", "a2")
	if head := mustHead(t, w); head.Revision() != rev {
		t.Fatalf("Head() after the commit = %s, want %s", head.Revision(), rev)
	}
}

// A push that failed with the remote branch still at the commit's head, a
// network error say, is that error and not a conflict: nothing moved, so a
// rerun would only fail the same way.
func TestRepoWriterFailedPushOnUnmovedBranchIsAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a"})
	w, _, _ := newCachingRepoWriter(t, url, time.Hour)
	w.push = func(context.Context, *git.PushOptions) error { return errors.New("connection reset by peer") }

	_, err := w.Commit(ctx, mustHead(t, w).Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "lost"})
	if err == nil || errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("Commit() error = %v, want the push's error, not ErrConflict", err)
	}
	mustRead(t, mustHead(t, newTestRepoWriter(t, url)), "a", "a")
}
