#!/usr/bin/env bash
# Run hack/smoke-apis.sh against ai-grid-provider-backend on the current
# OpenShift cluster via oc port-forward (plaintext gRPC + HTTP).
#
# Prefer port-forward over Routes: the gRPC Route is TLS passthrough while the
# pod still serves plaintext gRPC, so grpcurl -plaintext works best locally.
#
# Usage:
#   bash hack/smoke-openshift.sh
#   make smoke-openshift
#   SKIP_TENANTS=1 bash hack/smoke-openshift.sh
#   NAMESPACE=ai-grid GRPC_LOCAL_PORT=18080 HTTP_LOCAL_PORT=18081 bash hack/smoke-openshift.sh
#
# Environment:
#   NAMESPACE          default ai-grid
#   GRPC_LOCAL_PORT    local gRPC port (default 8080)
#   HTTP_LOCAL_PORT    local HTTP port (default 8081)
#   SKIP_CLUSTERS / SKIP_TENANTS  forwarded to smoke-apis.sh
#   SMOKE_HTTP_ROUTE   1 to also print the public HTTP Route URL (smoke still uses port-forward)
#
# Needs: oc logged in, grpcurl, curl, python3. Deploy first with make openshift-up.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NAMESPACE="${NAMESPACE:-ai-grid}"
GRPC_LOCAL_PORT="${GRPC_LOCAL_PORT:-8080}"
HTTP_LOCAL_PORT="${HTTP_LOCAL_PORT:-8081}"
SMOKE_HTTP_ROUTE="${SMOKE_HTTP_ROUTE:-0}"
SERVICE="${SERVICE:-ai-grid-provider-backend}"
PF_LOG="$(mktemp -t ai-grid-smoke-pf.XXXXXX)"
PF_PID=""

cleanup() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  rm -f "${PF_LOG}"
}
trap cleanup EXIT

die() { echo "error: $*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required and must be on PATH"
}

need_cmd oc
need_cmd grpcurl
need_cmd curl
need_cmd python3

oc whoami >/dev/null 2>&1 || die "oc is not logged in; run oc login first"

if ! oc -n "${NAMESPACE}" get svc "${SERVICE}" >/dev/null 2>&1; then
  die "service ${SERVICE} not found in namespace ${NAMESPACE} (deploy with: make openshift-up)"
fi

echo "==> OpenShift context: $(oc whoami -c 2>/dev/null || oc config current-context)"
echo "==> namespace ${NAMESPACE}, service ${SERVICE}"

if [[ "${SMOKE_HTTP_ROUTE}" == "1" ]]; then
  http_host="$(oc get route ai-grid-provider-backend-http -n "${NAMESPACE}" -o jsonpath='{.spec.host}' 2>/dev/null || true)"
  if [[ -n "${http_host}" ]]; then
    echo "==> HTTP Route (info only): https://${http_host}/docs/"
  fi
fi

echo "==> waiting for deployment/${SERVICE} available"
oc -n "${NAMESPACE}" rollout status "deployment/${SERVICE}" --timeout=3m

echo "==> port-forward ${SERVICE}  ${GRPC_LOCAL_PORT}:8080  ${HTTP_LOCAL_PORT}:8081"
oc -n "${NAMESPACE}" port-forward "svc/${SERVICE}" \
  "${GRPC_LOCAL_PORT}:8080" "${HTTP_LOCAL_PORT}:8081" \
  >"${PF_LOG}" 2>&1 &
PF_PID=$!

ready=0
deadline=$((SECONDS + 60))
while (( SECONDS < deadline )); do
  if ! kill -0 "${PF_PID}" 2>/dev/null; then
    echo "port-forward exited early:" >&2
    cat "${PF_LOG}" >&2 || true
    die "port-forward failed"
  fi
  if curl -fsS "http://127.0.0.1:${HTTP_LOCAL_PORT}/api/v1/ping?name=ready" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done
if (( ready != 1 )); then
  echo "port-forward log:" >&2
  cat "${PF_LOG}" >&2 || true
  die "API did not become ready on localhost:${HTTP_LOCAL_PORT} within 60s"
fi

echo "==> running smoke-apis.sh against port-forward"
export GRPC_ADDR="127.0.0.1:${GRPC_LOCAL_PORT}"
export HTTP_ADDR="http://127.0.0.1:${HTTP_LOCAL_PORT}"
bash "${ROOT}/hack/smoke-apis.sh"
