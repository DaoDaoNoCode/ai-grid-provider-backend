#!/usr/bin/env bash
# Remove the OpenShift deploy created by hack/openshift-up.sh.
#
# Usage:
#   bash hack/openshift-down.sh
#   DELETE_NAMESPACE=1 bash hack/openshift-down.sh
#
# Environment:
#   NAMESPACE          default ai-grid
#   DELETE_NAMESPACE   1 to delete the namespace after resources
#   DELETE_SAMPLES     1 (default) delete sample ManagedClusters
#
# Leaves the namespace by default so shared hub contents are preserved.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MANIFESTS="${ROOT}/deploy/openshift"
NAMESPACE="${NAMESPACE:-ai-grid}"
DELETE_NAMESPACE="${DELETE_NAMESPACE:-0}"
DELETE_SAMPLES="${DELETE_SAMPLES:-1}"

for arg in "$@"; do
  case "$arg" in
    --delete-namespace) DELETE_NAMESPACE=1 ;;
    --keep-samples) DELETE_SAMPLES=0 ;;
    -h|--help)
      sed -n '2,16p' "$0"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

if ! command -v oc >/dev/null 2>&1; then
  echo "oc is required and must be on PATH" >&2
  exit 1
fi
if ! oc whoami >/dev/null 2>&1; then
  echo "oc is not logged in; run oc login first" >&2
  exit 1
fi

echo "==> deleting OpenShift ai-grid-provider-backend resources in ${NAMESPACE}"

if [[ "$DELETE_SAMPLES" == "1" ]]; then
  oc delete -f "${MANIFESTS}/sample-managedclusters.yaml" --ignore-not-found || true
fi

oc delete -f "${MANIFESTS}/keycloak.yaml" --ignore-not-found || true
oc -n "${NAMESPACE}" delete job keycloak-init --ignore-not-found || true
oc -n "${NAMESPACE}" delete secret keycloak-admin keycloak-db ai-grid-provider-backend-keycloak --ignore-not-found || true

if command -v kustomize >/dev/null 2>&1; then
  kustomize build "${MANIFESTS}" | oc delete -f - --ignore-not-found || true
else
  oc kustomize "${MANIFESTS}" | oc delete -f - --ignore-not-found || true
fi

# Cluster-scoped RBAC names from the manifests (also removed via kustomize delete).
oc delete clusterrole ai-grid-provider-backend-managedclusters --ignore-not-found || true
oc delete clusterrolebinding ai-grid-provider-backend-managedclusters --ignore-not-found || true

if [[ "$DELETE_NAMESPACE" == "1" ]]; then
  echo "==> deleting namespace ${NAMESPACE}"
  oc delete namespace "${NAMESPACE}" --ignore-not-found
else
  echo "==> left namespace ${NAMESPACE} in place (set DELETE_NAMESPACE=1 to remove it)"
fi

echo "OpenShift deploy removed."
