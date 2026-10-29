package gitops

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRetryOnConflict(t *testing.T) {
	t.Parallel()
	errOther := errors.New("other")
	for _, tc := range []struct {
		name string
		// results are what each attempt returns; the last repeats.
		results      []error
		wantErr      error
		wantAttempts int
	}{
		{"success", []error{nil}, nil, 1},
		{"another error is not retried", []error{errOther}, errOther, 1},
		{"a conflict is retried", []error{ErrConflict, ErrConflict, nil}, nil, 3},
		{"a wrapped conflict is retried", []error{fmt.Errorf("push: %w", ErrConflict), nil}, nil, 2},
		{"another error after a conflict ends it", []error{ErrConflict, errOther}, errOther, 2},
		{"conflicts until the attempts are used up", []error{ErrConflict}, ErrConflict, ConflictAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attempts := 0
			err := RetryOnConflict(context.Background(), func() error {
				result := tc.results[min(attempts, len(tc.results)-1)]
				attempts++
				return result
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RetryOnConflict() error = %v, want %v", err, tc.wantErr)
			}
			if attempts != tc.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
		})
	}
}

// A request whose context is done isn't run again.
func TestRetryOnConflictStopsWhenContextIsDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := RetryOnConflict(ctx, func() error {
		attempts++
		cancel()
		return ErrConflict
	})
	if !errors.Is(err, ErrConflict) || attempts != 1 {
		t.Fatalf("RetryOnConflict() = %v after %d attempts, want ErrConflict after 1", err, attempts)
	}
}

// Another replica's commit lands between this one's commit and its push, to
// a file this write doesn't touch. The push is refused; the rerun reads the
// new head and commits on top of it.
func TestRetryOnConflictAfterLosingAPush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := newBareRepo(t, map[string]string{"a": "a", "b": "b"})
	w := newTestRepoWriter(t, url)
	other := newTestRepoWriter(t, url)
	w.beforePush = func() {
		w.beforePush = nil
		head, _ := other.Head(ctx)
		if _, err := other.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "other"}); err != nil {
			t.Errorf("other Commit() error = %v", err)
		}
	}
	attempts := 0
	err := RetryOnConflict(ctx, func() error {
		attempts++
		head, err := w.Head(ctx)
		if err != nil {
			return err
		}
		_, err = w.Commit(ctx, head.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"})
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("RetryOnConflict() = %v after %d attempts, want success after 2", err, attempts)
	}
	head, _ := newTestRepoWriter(t, url).Head(ctx)
	for p, want := range map[string]string{"a": "a2", "b": "b2"} {
		if data, err := head.Read(p); err != nil || string(data) != want {
			t.Fatalf("Read(%s) = %q, %v; want %q", p, data, err, want)
		}
	}
}
