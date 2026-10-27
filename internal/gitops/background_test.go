package gitops

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A writer whose clone fails answers ErrNotReady, with the clone's error,
// until a retry succeeds; then it reads and commits as the clone does.
func TestBackgroundWriterRetriesTheClone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cloneErr := errors.New("clone: authentication required")
	var attempts atomic.Int32
	release := make(chan struct{})
	ready := NewMemoryWriter(map[string][]byte{"a.yaml": []byte("a")})
	w := StartBackgroundWriter(ctx, func(context.Context) (Writer, error) {
		if attempts.Add(1) < 3 {
			return nil, cloneErr
		}
		<-release
		return ready, nil
	}, time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := w.Head(ctx); !errors.Is(err, ErrNotReady) || !errors.Is(err, cloneErr) {
		t.Fatalf("Head() before the clone error = %v, want ErrNotReady wrapping the clone error", err)
	}
	if _, err := w.Commit(ctx, "0", Change{Put: map[string][]byte{"b.yaml": []byte("b")}, Message: "add b"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Commit() before the clone error = %v, want ErrNotReady", err)
	}

	close(release)
	var head Snapshot
	var err error
	for time.Now().Before(deadline) {
		if head, err = w.Head(ctx); !errors.Is(err, ErrNotReady) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Head() after the clone error = %v", err)
	}
	if _, err := head.Read("a.yaml"); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if _, err := w.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"b.yaml": []byte("b")}, Message: "add b"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

// Cancelling the context stops the retries.
func TestBackgroundWriterStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	StartBackgroundWriter(ctx, func(context.Context) (Writer, error) {
		attempts.Add(1)
		return nil, errors.New("unreachable")
	}, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	stopped := attempts.Load()
	time.Sleep(50 * time.Millisecond)
	if got := attempts.Load(); got != stopped {
		t.Fatalf("attempts after cancel = %d, want %d", got, stopped)
	}
}
