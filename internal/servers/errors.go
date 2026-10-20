package servers

import (
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

// mapManagedClusterAPIError translates a Kubernetes API error into the code the
// caller sees.
//
// The default is Internal rather than uncoded. An unrecognized failure is the
// service's own and not something the caller can act on, which argues for
// leaving it uncoded — but connect-go v2 scrubs an uncoded error to "unknown"
// with an empty wire message, where grpc-go serialized the text. That turns an
// etcd timeout or a dropped API server into a blank error in the console, on
// exactly the path someone is debugging when something surprising breaks.
// Internal says the same thing about blame and keeps the message.
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
		return connect.Errorf(connect.CodeInternal, "%s ManagedCluster %q: %v", op, id, err)
	}
}
