#!/usr/bin/env bash
# Deploy ai-grid-provider-backend to the current OpenShift cluster (Kind parity).
#
# Default: embedded Keycloak + Postgres + init Job in namespace ai-grid.
# External Keycloak: EXTERNAL_KEYCLOAK=1 or --external-keycloak, plus KEYCLOAK_*.
#
# Usage:
#   bash hack/openshift-up.sh
#   BUILD_IMAGE=1 bash hack/openshift-up.sh
#   EXTERNAL_KEYCLOAK=1 KEYCLOAK_URL=https://... KEYCLOAK_ADMIN_USERNAME=... \
#     KEYCLOAK_ADMIN_PASSWORD=... bash hack/openshift-up.sh
#
# Environment:
#   IMAGE                 full image ref (default quay.io/rh-ee-rjjohnso/ai-grid-provider-backend:latest)
#   IMAGE_TAG             overrides tag when IMAGE is unset (default latest)
#   BUILD_IMAGE           1 to podman build + push IMAGE before apply
#   PLATFORM              build platform (default linux/amd64; required for Apple Silicon → OpenShift)
#   EXTERNAL_KEYCLOAK     1 to skip embedded Keycloak manifests
#   KEYCLOAK_URL          required when EXTERNAL_KEYCLOAK=1
#   KEYCLOAK_ADMIN_USERNAME / KEYCLOAK_ADMIN_PASSWORD
#   KEYCLOAK_REALM        default ai-grid
#   KEYCLOAK_ADMIN_REALM  default master
#   APPLY_SAMPLES         1 (default) apply sample ManagedClusters; 0 to skip
#   NAMESPACE             default ai-grid
#   GRANT_ANYUID          1 to grant anyuid SCC to default SA (Keycloak/Postgres)
#
# Needs oc logged in. Idempotent.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MANIFESTS="${ROOT}/deploy/openshift"
NAMESPACE="${NAMESPACE:-ai-grid}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
IMAGE="${IMAGE:-quay.io/rh-ee-rjjohnso/ai-grid-provider-backend:${IMAGE_TAG}}"
BUILD_IMAGE="${BUILD_IMAGE:-0}"
PLATFORM="${PLATFORM:-linux/amd64}"
EXTERNAL_KEYCLOAK="${EXTERNAL_KEYCLOAK:-0}"
APPLY_SAMPLES="${APPLY_SAMPLES:-1}"
GRANT_ANYUID="${GRANT_ANYUID:-0}"
KEYCLOAK_REALM="${KEYCLOAK_REALM:-ai-grid}"
KEYCLOAK_ADMIN_REALM="${KEYCLOAK_ADMIN_REALM:-master}"
DEFAULT_IMAGE="quay.io/rh-ee-rjjohnso/ai-grid-provider-backend:latest"

for arg in "$@"; do
  case "$arg" in
    --external-keycloak) EXTERNAL_KEYCLOAK=1 ;;
    --build-image) BUILD_IMAGE=1 ;;
    --no-samples) APPLY_SAMPLES=0 ;;
    --grant-anyuid) GRANT_ANYUID=1 ;;
    -h|--help)
      sed -n '2,30p' "$0"
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

if [[ "$EXTERNAL_KEYCLOAK" == "1" ]]; then
  if [[ -z "${KEYCLOAK_URL:-}" || -z "${KEYCLOAK_ADMIN_USERNAME:-}" || -z "${KEYCLOAK_ADMIN_PASSWORD:-}" ]]; then
    echo "EXTERNAL_KEYCLOAK=1 requires KEYCLOAK_URL, KEYCLOAK_ADMIN_USERNAME, and KEYCLOAK_ADMIN_PASSWORD" >&2
    exit 1
  fi
  if [[ "${KEYCLOAK_URL}" == http://* ]]; then
    echo "warning: KEYCLOAK_URL is plaintext; the admin password will cross the network unencrypted" >&2
  fi
else
  KEYCLOAK_URL="${KEYCLOAK_URL:-http://keycloak.${NAMESPACE}.svc:8080}"
  KEYCLOAK_ADMIN_USERNAME="${KEYCLOAK_ADMIN_USERNAME:-admin}"
  KEYCLOAK_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD:-admin}"
fi

kustomize_build() {
  if command -v kustomize >/dev/null 2>&1; then
    kustomize build "${MANIFESTS}"
  else
    oc kustomize "${MANIFESTS}"
  fi
}

if [[ "$BUILD_IMAGE" == "1" ]]; then
  if ! command -v podman >/dev/null 2>&1; then
    echo "BUILD_IMAGE=1 requires podman" >&2
    exit 1
  fi
  target_os="${PLATFORM%%/*}"
  target_arch="${PLATFORM##*/}"
  echo "==> building and pushing ${IMAGE} (platform ${PLATFORM})"
  podman build \
    --platform="${PLATFORM}" \
    --build-arg "TARGETOS=${target_os}" \
    --build-arg "TARGETARCH=${target_arch}" \
    -t "${IMAGE}" \
    -f "${ROOT}/Containerfile" \
    "${ROOT}"
  podman push "${IMAGE}"
fi

echo "==> applying backend manifests to namespace ${NAMESPACE} with image ${IMAGE}"
kustomize_build \
  | sed -e "s#${DEFAULT_IMAGE}#${IMAGE}#g" \
  | oc apply -f -

if [[ "$EXTERNAL_KEYCLOAK" != "1" ]]; then
  if [[ "$GRANT_ANYUID" == "1" ]]; then
    echo "==> granting anyuid SCC to system:serviceaccount:${NAMESPACE}:default (Keycloak/Postgres)"
    oc adm policy add-scc-to-user anyuid -z default -n "${NAMESPACE}" || true
  fi

  echo "==> creating secret keycloak-admin"
  oc create secret generic keycloak-admin -n "${NAMESPACE}" \
    --from-literal=username="${KEYCLOAK_ADMIN_USERNAME}" \
    --from-literal=password="${KEYCLOAK_ADMIN_PASSWORD}" \
    --dry-run=client -o yaml | oc apply -f -

  # Recreate the init Job when re-running so realm/org setup stays idempotent.
  oc -n "${NAMESPACE}" delete job keycloak-init --ignore-not-found --wait=true

  echo "==> applying embedded Keycloak stack"
  oc apply -f "${MANIFESTS}/keycloak.yaml"
fi

echo "==> creating secret ai-grid-provider-backend-keycloak for ${KEYCLOAK_URL} (realm ${KEYCLOAK_REALM})"
oc create secret generic ai-grid-provider-backend-keycloak -n "${NAMESPACE}" \
  --from-literal=url="${KEYCLOAK_URL}" \
  --from-literal=realm="${KEYCLOAK_REALM}" \
  --from-literal=admin-realm="${KEYCLOAK_ADMIN_REALM}" \
  --from-literal=username="${KEYCLOAK_ADMIN_USERNAME}" \
  --from-literal=password="${KEYCLOAK_ADMIN_PASSWORD}" \
  --dry-run=client -o yaml | oc apply -f -

# Ensure the deployment picks up a newly created/rotated secret.
oc rollout restart deployment/ai-grid-provider-backend -n "${NAMESPACE}" >/dev/null 2>&1 || true

if [[ "$EXTERNAL_KEYCLOAK" != "1" ]]; then
  echo "==> waiting for Keycloak and init Job"
  oc -n "${NAMESPACE}" rollout status deployment/keycloak-db --timeout=5m
  oc -n "${NAMESPACE}" rollout status deployment/keycloak --timeout=5m
  oc -n "${NAMESPACE}" wait --for=condition=complete job/keycloak-init --timeout=5m
fi

echo "==> waiting for backend rollout"
oc -n "${NAMESPACE}" rollout status deployment/ai-grid-provider-backend --timeout=5m

if [[ "$APPLY_SAMPLES" == "1" ]]; then
  if oc get crd managedclusters.cluster.open-cluster-management.io >/dev/null 2>&1; then
    echo "==> applying sample ManagedClusters"
    oc apply -f "${MANIFESTS}/sample-managedclusters.yaml"
  else
    echo "warning: ManagedCluster CRD not found; skip samples (install ACM/OCM or set APPLY_SAMPLES=0)" >&2
  fi
fi

HTTP_HOST="$(oc get route ai-grid-provider-backend-http -n "${NAMESPACE}" -o jsonpath='{.spec.host}' 2>/dev/null || true)"
GRPC_HOST="$(oc get route ai-grid-provider-backend-grpc -n "${NAMESPACE}" -o jsonpath='{.spec.host}' 2>/dev/null || true)"

echo ""
echo "OpenShift deploy ready (namespace ${NAMESPACE})."
if [[ -n "${HTTP_HOST}" ]]; then
  echo "  REST/Swagger: https://${HTTP_HOST}/docs/"
  echo "  curl example: curl -sk https://${HTTP_HOST}/api/v1/ping"
fi
if [[ -n "${GRPC_HOST}" ]]; then
  echo "  gRPC Route:   ${GRPC_HOST} (passthrough; prefer port-forward for plaintext grpcurl)"
fi
echo "  Port-forward: oc -n ${NAMESPACE} port-forward svc/ai-grid-provider-backend 8080:8080 8081:8081"
echo "  grpcurl:      grpcurl -plaintext localhost:8080 ai_grid.v1.AiGrid/Ping"
if [[ "$EXTERNAL_KEYCLOAK" != "1" && "$GRANT_ANYUID" != "1" ]]; then
  echo ""
  echo "If Keycloak or Postgres pods are rejected by SCC, re-run with GRANT_ANYUID=1 or:"
  echo "  oc adm policy add-scc-to-user anyuid -z default -n ${NAMESPACE}"
fi
