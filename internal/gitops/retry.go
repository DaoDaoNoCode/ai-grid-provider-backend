package gitops

import (
	"context"
	"errors"
)

// ConflictAttempts is how many times RetryOnConflict runs a write in all, as
// many as client-go's retry.DefaultRetry steps.
const ConflictAttempts = 5

// RetryOnConflict runs write, and runs it again while it fails with
// ErrConflict, at most ConflictAttempts times in all, like client-go's
// retry.RetryOnConflict. write must do the whole read-decide-write: take a
// new Head, make every check against it, and Commit on its revision. Then a
// rerun keeps every guarantee a single attempt gives, because a conflict on
// a file the caller depends on, such as the object it read at a version, is
// decided again on the new head and answered as it would have been without
// the race. Only a conflict on what the caller didn't depend on, such as an
// index file another object's write also changed, goes away.
//
// It returns write's last error: ErrConflict still, after the last attempt,
// or when ctx is done. Any other error ends it at once. It doesn't wait
// between attempts: each one fetches the branch again, and a conflict means
// another write was committed, so the next attempt starts from that one.
func RetryOnConflict(ctx context.Context, write func() error) error {
	var err error
	for range ConflictAttempts {
		if err = write(); !errors.Is(err, ErrConflict) || ctx.Err() != nil {
			return err
		}
	}
	return err
}
