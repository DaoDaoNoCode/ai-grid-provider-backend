package servers

import (
	"context"
	"errors"

	connect "connectrpc.com/connect/v2"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
)

// mapGitopsError maps gitops sentinel errors to Connect codes. noun is the
// singular resource name ("Model", "Subscription"); api is the plural API
// name used when the repository isn't configured ("Models", "Subscriptions").
// stale builds the Aborted conflict message for the resource.
func mapGitopsError(noun, api, op, id string, err error, stale func(op, id string) *connect.Error) error {
	switch {
	case errors.Is(err, gitops.ErrNotFound):
		return connect.Errorf(connect.CodeNotFound, "%s %q not found", noun, id).WithCause(err)
	case errors.Is(err, gitops.ErrConflict):
		return stale(op, id).WithCause(err)
	case errors.Is(err, gitops.ErrNotConfigured):
		// Not one object's problem, so the message names the API, not the id.
		return connect.Errorf(connect.CodeFailedPrecondition, "%s: %v", api, err).WithCause(err)
	default:
		return connect.Errorf(connect.CodeUnavailable, "%s %s %q: git: %v", op, noun, id, err).WithCause(err)
	}
}

// decideOnHead runs decide, the read-decide-write of one attempt, on the
// branch head. Head may serve a snapshot up to the Git read window old
// (AI_GRID_GIT_READ_MAX_AGE), so a refusal decided on such a snapshot may
// rest on a state the remote has left: the object exists, doesn't, or is at
// another version, or a model a check looked for is there or not. When decide
// refuses on a snapshot that isn't fresh (gitops.Snapshot.Fresh), it decides
// once more on gitops.Writer.FreshHead, a fetch, and answers that. A refusal
// on a fresh snapshot is answered at once. headErr maps a Head error.
//
// A conflict is not a refusal: RetryOnConflict, around this, reruns it.
func decideOnHead(ctx context.Context, writer gitops.Writer, headErr func(error) error, decide func(gitops.Snapshot) error) error {
	head, err := writer.Head(ctx)
	if err != nil {
		return headErr(err)
	}
	err = decide(head)
	if head.Fresh() || !refusedOnSnapshot(err) {
		return err
	}
	if head, err = writer.FreshHead(ctx); err != nil {
		return headErr(err)
	}
	return decide(head)
}

// refusedOnSnapshot reports whether err refuses a write on what the snapshot
// showed: NotFound, AlreadyExists, Aborted for a version that doesn't match,
// or FailedPrecondition. A conflict on commit and a missing repository are
// not.
func refusedOnSnapshot(err error) bool {
	if err == nil || errors.Is(err, gitops.ErrConflict) || errors.Is(err, gitops.ErrNotConfigured) || errors.Is(err, gitops.ErrNotReady) {
		return false
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeAlreadyExists, connect.CodeAborted, connect.CodeFailedPrecondition:
		return true
	}
	return false
}
