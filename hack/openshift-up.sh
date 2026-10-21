#!/usr/bin/env bash
# Deploy ai-grid-provider-backend to the current OpenShift cluster (Kind parity).
#
# Default: embedded Keycloak + Postgres + init Job in namespace ai-grid.
# When the ManagedCluster CRD is missing, installs RHACM (deploy/acm-hub) so
# Clusters APIs are testable. Tenants APIs use the embedded (or external) Keycloak.
#
# External Keycloak: EXTERNAL_KEYCLOAK=1 or --external-keycloak, plus KEYCLOAK_*.
#
# Usage:
#   bash hack/openshift-up.sh
#   BUILD_IMAGE=1 bash hack/openshift-up.sh
#   SKIP_ACM=1 bash hack/openshift-up.sh
#   EXTERNAL_KEYCLOAK=1 KEYCLOAK_URL=https://... KEYCLOAK_ADMIN_USERNAME=... \
#     KEYCLOAK_ADMIN_PASSWORD=... bash hack/openshift-up.sh
#
# Environment:
#   IMAGE                 full image ref (default ghcr.io/ai-grid-cloud/ai-grid-provider-backend:latest)
#   IMAGE_TAG             overrides tag when IMAGE is unset (default latest)
#   BUILD_IMAGE           1 to podman build + push IMAGE before apply
#   PLATFORM              build platform (default linux/amd64; required for Apple Silicon → OpenShift)
#   EXTERNAL_KEYCLOAK     1 to skip embedded Keycloak manifests
#   KEYCLOAK_URL          required when EXTERNAL_KEYCLOAK=1
#   KEYCLOAK_ADMIN_USERNAME / KEYCLOAK_ADMIN_PASSWORD
#   KEYCLOAK_REALM        default ai-grid
#   KEYCLOAK_ADMIN_REALM  default master
#   INSTALL_ACM           auto (default): install RHACM when ManagedCluster CRD is missing
#                         1: always run acm-hub-up (idempotent)
#                         0: never install ACM (same as SKIP_ACM=1)
#   SKIP_ACM              1 to skip ACM install (alias for INSTALL_ACM=0)
#   APPLY_SAMPLES         1 (default) apply sample ManagedClusters; 0 to skip
#   NAMESPACE             backend/Keycloak namespace (default ai-grid)
#   ACM_NAMESPACE         RHACM operator namespace (default open-cluster-management)
#   GRANT_ANYUID          1 to grant anyuid SCC to default SA (Keycloak/Postgres)
#   CHANNEL               ACM Subscription channel (passed through to acm-hub-up)
#
# Needs oc logged in. helm required when ACM is installed. Idempotent.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MANIFESTS="${ROOT}/deploy/openshift"
NAMESPACE="${NAMESPACE:-ai-grid}"
# ACM must not inherit the backend NAMESPACE; chart templates also use .Values.namespace.
ACM_NAMESPACE="${ACM_NAMESPACE:-open-cluster-management}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
IMAGE="${IMAGE:-ghcr.io/ai-grid-cloud/ai-grid-provider-backend:${IMAGE_TAG}}"
BUILD_IMAGE="${BUILD_IMAGE:-0}"
PLATFORM="${PLATFORM:-linux/amd64}"
EXTERNAL_KEYCLOAK="${EXTERNAL_KEYCLOAK:-0}"
APPLY_SAMPLES="${APPLY_SAMPLES:-1}"
GRANT_ANYUID="${GRANT_ANYUID:-0}"
INSTALL_ACM="${INSTALL_ACM:-auto}"
SKIP_ACM="${SKIP_ACM:-0}"
KEYCLOAK_REALM="${KEYCLOAK_REALM:-ai-grid}"
KEYCLOAK_ADMIN_REALM="${KEYCLOAK_ADMIN_REALM:-master}"
DEFAULT_API_IMAGE="quay.io/rh-ee-rjjohnso/ai-grid-provider-backend:latest"
MANAGED_CLUSTER_CRD="managedclusters.cluster.open-cluster-management.io"
DEFAULT_IMAGE="ghcr.io/ai-grid-cloud/ai-grid-provider-backend:latest"

for arg in "$@"; do
  case "$arg" in
    --external-keycloak) EXTERNAL_KEYCLOAK=1 ;;
    --build-image) BUILD_IMAGE=1 ;;
    --no-samples) APPLY_SAMPLES=0 ;;
    --grant-anyuid) GRANT_ANYUID=1 ;;
    --skip-acm) SKIP_ACM=1; INSTALL_ACM=0 ;;
    --install-acm) INSTALL_ACM=1 ;;
    -h|--help)
      sed -n '2,40p' "$0"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

if [[ "$SKIP_ACM" == "1" ]]; then
  INSTALL_ACM=0
fi

if ! command -v oc >/dev/null 2>&1; then
  echo "oc is required and must be on PATH" >&2
  exit 1
fi
if ! oc whoami >/dev/null 2>&1; then
  echo "oc is not logged in; run oc login first" >&2
  exit 1
fi

managed_cluster_crd_present() {
  oc get crd "${MANAGED_CLUSTER_CRD}" >/dev/null 2>&1
}

should_install_acm() {
  case "${INSTALL_ACM}" in
    0|false|no) return 1 ;;
    1|true|yes) return 0 ;;
    auto)
      if managed_cluster_crd_present; then
        return 1
      fi
      return 0
      ;;
    *)
      echo "INSTALL_ACM must be auto, 0, or 1 (got ${INSTALL_ACM})" >&2
      exit 1
      ;;
  esac
}

if should_install_acm; then
  if ! command -v helm >/dev/null 2>&1; then
    echo "helm is required to install ACM (or set SKIP_ACM=1 / INSTALL_ACM=0)" >&2
    exit 1
  fi
  echo "==> ManagedCluster CRD missing or INSTALL_ACM=1; installing RHACM hub (15–20+ minutes)"
  echo "    ACM namespace: ${ACM_NAMESPACE} (override with ACM_NAMESPACE=...; backend stays in ${NAMESPACE})"
  echo "    Override channel with CHANNEL=release-2.13 (OCP 4.18) or CHANNEL=release-2.16 (OCP ≥ 4.19)"
  # Keep acm-hub playground samples; openshift APPLY_SAMPLES adds ai-grid-sample-* as well.
  # Force ACM_NAMESPACE so a caller's NAMESPACE=ai-grid does not poison helm --namespace.
  env NAMESPACE="${ACM_NAMESPACE}" bash "${ROOT}/deploy/acm-hub/scripts/acm-hub-up.sh"
elif managed_cluster_crd_present; then
  echo "==> ManagedCluster CRD present; skipping ACM install"
else
  echo "==> skipping ACM install (INSTALL_ACM=${INSTALL_ACM}); Clusters API needs the ManagedCluster CRD" >&2
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
  if managed_cluster_crd_present; then
    echo "==> applying sample ManagedClusters (ai-grid-sample-a/b)"
    oc apply -f "${MANIFESTS}/sample-managedclusters.yaml"
  else
    echo "warning: ManagedCluster CRD not found; skip samples (re-run without SKIP_ACM=1, or make acm-hub-up)" >&2
  fi
fi

HTTP_HOST="$(oc get route ai-grid-provider-backend-http -n "${NAMESPACE}" -o jsonpath='{.spec.host}' 2>/dev/null || true)"
GRPC_HOST="$(oc get route ai-grid-provider-backend-grpc -n "${NAMESPACE}" -o jsonpath='{.spec.host}' 2>/dev/null || true)"

echo ""
echo "OpenShift deploy ready (namespace ${NAMESPACE})."
if [[ -n "${HTTP_HOST}" ]]; then
  echo "  REST/Swagger: https://${HTTP_HOST}/docs/"
  echo "  curl example: curl -sk https://${HTTP_HOST}/api/v1/ping"
  echo "  Clusters:     curl -sk https://${HTTP_HOST}/api/v1/clusters"
  echo "  Tenants:      curl -sk https://${HTTP_HOST}/api/v1/tenants"
fi
if [[ -n "${GRPC_HOST}" ]]; then
  echo "  gRPC Route:   ${GRPC_HOST} (passthrough; prefer port-forward for plaintext grpcurl)"
fi
echo "  Port-forward: oc -n ${NAMESPACE} port-forward svc/ai-grid-provider-backend 8080:8080 8081:8081"
echo "  grpcurl:      grpcurl -plaintext localhost:8080 ai_grid.v1.AiGrid/Ping"
echo "  Smoke:        make smoke-openshift"
echo "  Rebuild tip:  BUILD_IMAGE=1 make openshift-up"
if managed_cluster_crd_present; then
  echo "  Clusters CRD: present (oc get managedcluster)"
else
  echo "  Clusters CRD: missing — install with make acm-hub-up or INSTALL_ACM=1 make openshift-up"
fi
if [[ "$EXTERNAL_KEYCLOAK" != "1" && "$GRANT_ANYUID" != "1" ]]; then
  echo ""
  echo "If Keycloak or Postgres pods are rejected by SCC, re-run with GRANT_ANYUID=1 or:"
  echo "  oc adm policy add-scc-to-user anyuid -z default -n ${NAMESPACE}"
fi
