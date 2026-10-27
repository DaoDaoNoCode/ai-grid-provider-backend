package gitops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrNotReady reports a call before the repository's first clone succeeded.
// It wraps the clone's latest error.
var ErrNotReady = errors.New("gitops: repository not cloned yet")

// maxCloneRetry caps the wait between clone attempts.
const maxCloneRetry = time.Minute

// BackgroundWriter is a Writer whose repository is cloned in the background,
// so the process starts, and serves its other APIs, while the Git host is
// unreachable or the token is wrong. Until a clone succeeds, Head and Commit
// fail with ErrNotReady.
type BackgroundWriter struct {
	mu      sync.RWMutex
	writer  Writer
	lastErr error
}

var _ Writer = (*BackgroundWriter)(nil)

// StartBackgroundWriter calls open until it succeeds or ctx is done, waiting
// retry after the first failure and doubling the wait up to a minute.
func StartBackgroundWriter(ctx context.Context, open func(context.Context) (Writer, error), retry time.Duration) *BackgroundWriter {
	w := &BackgroundWriter{lastErr: errors.New("clone in progress")}
	go func() {
		wait := retry
		for {
			writer, err := open(ctx)
			w.mu.Lock()
			w.writer, w.lastErr = writer, err
			w.mu.Unlock()
			if err == nil {
				slog.Info("git repository cloned")
				return
			}
			slog.Warn("git clone failed; retrying", "error", err, "retry", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(2*wait, maxCloneRetry)
		}
	}()
	return w
}

func (w *BackgroundWriter) ready() (Writer, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.writer == nil {
		return nil, fmt.Errorf("%w: %w", ErrNotReady, w.lastErr)
	}
	return w.writer, nil
}

// Head returns the snapshot at the branch head, or ErrNotReady.
func (w *BackgroundWriter) Head(ctx context.Context) (Snapshot, error) {
	writer, err := w.ready()
	if err != nil {
		return nil, err
	}
	return writer.Head(ctx)
}

// Commit applies c on top of the branch head, or fails with ErrNotReady.
func (w *BackgroundWriter) Commit(ctx context.Context, base string, c Change) (string, error) {
	writer, err := w.ready()
	if err != nil {
		return "", err
	}
	return writer.Commit(ctx, base, c)
}
