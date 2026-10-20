package servers

import (
	"fmt"

	connect "connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Every error a handler returns must be a *connect.Error, built here or with
// connect.Errorf: connect-go reads the code and the wire message from nothing
// else. An ordinary Go error reaches the caller as "unknown" with no message,
// its text kept only as a local cause.

// staleClusterError is the Aborted error for a write against an old version.
func staleClusterError(op, id string) *connect.Error {
	return connect.Errorf(connect.CodeAborted,
		"%s ManagedCluster %q: the cluster changed since it was read; reload and retry", op, id)
}

// mapManagedClusterAPIError translates a Kubernetes API error into the code
// the caller sees. The default is deliberately uncoded: an unrecognized
// failure is the service's own, not something the caller can act on.
func mapManagedClusterAPIError(op, id string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return connect.Errorf(connect.CodeNotFound, "ManagedCluster %q not found", id)
	case apierrors.IsAlreadyExists(err):
		return connect.Errorf(connect.CodeAlreadyExists, "ManagedCluster %q already exists", id)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return connect.Errorf(connect.CodeInvalidArgument, "%s ManagedCluster %q: %v", op, id, err)
	case apierrors.IsConflict(err):
		return staleClusterError(op, id)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return connect.Errorf(connect.CodePermissionDenied, "%s ManagedCluster %q: %v", op, id, err)
	default:
		return fmt.Errorf("%s ManagedCluster %q: %w", op, id, err)
	}
}
