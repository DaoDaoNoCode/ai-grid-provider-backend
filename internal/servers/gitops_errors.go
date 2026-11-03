package servers

import (
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
