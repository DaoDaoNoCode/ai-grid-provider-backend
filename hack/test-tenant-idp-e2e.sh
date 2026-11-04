#!/usr/bin/env bash
# One-shot end-to-end test of the tenant identity-provider flow against a real
# Kind cluster: real Keycloak 26.8, real backend image built from the current
# tree, real Kubernetes Secret read with the ai-grid-idp-secrets RBAC.
#
# What it does:
#   1. Prerequisites check (Podman, Kind, kubectl, grpcurl, jq)
#   2. Kind cluster up (reuses an existing ai-grid-hub cluster) + backend + Keycloak
#   3. Applies the demo IdP client Secret
#   4. Port-forwards backend (gRPC + REST) and Keycloak
#   5. Runs hack/smoke-tenant-idp.sh (create IdP tenant, verify realm IdP, org
#      link, maas_org_id = generated org UUID, immutability, delete cleanup)
#   6. REST checks: list tenants, create/list/delete an IdP-less tenant
#
# Usage:
#   bash hack/test-tenant-idp-e2e.sh
#
# Env:
#   GRPC_PORT         default 18080 (host port for backend gRPC)
#   HTTP_PORT         default 18081 (host port for backend REST)
#   KEYCLOAK_PORT     default 18180 (host port for Keycloak)
#   SKIP_UP=1         reuse the running cluster without kind-up.sh
set -euo pipefail

cd "$(dirname "$0")/.."

grpc_port="${GRPC_PORT:-18080}"
http_port="${HTTP_PORT:-18081}"
kc_port="${KEYCLOAK_PORT:-18180}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: $1 required" >&2; exit 1; }; }
need jq
need grpcurl
need curl
need kubectl

if [[ "${SKIP_UP:-0}" != "1" ]]; then
  bash hack/check-prereqs.sh
  bash hack/kind-up.sh
fi

echo "==> waiting for backend and Keycloak deployments"
kubectl -n ai-grid rollout status deployment/ai-grid-provider-backend --timeout=5m
kubectl -n ai-grid rollout status deployment/keycloak --timeout=5m

echo "==> waiting for keycloak-init (creates the ai-grid realm)"
# The Keycloak deployment has no readiness probe, so rollout success only
# means the container started; Keycloak itself takes ~90s to boot and the
# init job seeds the realm after that.
kubectl -n ai-grid wait --for=condition=complete job/keycloak-init --timeout=5m

echo "==> applying demo IdP client Secret"
kubectl -n ai-grid-idp-secrets apply -f deploy/kind/idp-client-secret.example.yaml

pf_pids=()
cleanup() {
  for pid in "${pf_pids[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

echo "==> port-forwarding backend (:${grpc_port} gRPC, :${http_port} REST) and Keycloak (:${kc_port})"
kubectl -n ai-grid port-forward service/ai-grid-provider-backend "${grpc_port}:8080" "${http_port}:8081" >/dev/null 2>&1 &
pf_pids+=("$!")
kubectl -n ai-grid port-forward service/keycloak "${kc_port}:8080" >/dev/null 2>&1 &
pf_pids+=("$!")

wait_ready() {
  local what="$1" tries="$2" cmd="$3"
  for _ in $(seq 1 "$tries"); do
    if eval "$cmd" >/dev/null 2>&1; then
      echo "==> ${what} ready"
      return 0
    fi
    sleep 2
  done
  echo "error: ${what} not ready" >&2
  return 1
}

wait_ready "backend gRPC" 30 "grpcurl -plaintext 127.0.0.1:${grpc_port} list"
wait_ready "Keycloak" 90 "curl -fsS http://127.0.0.1:${kc_port}/realms/master/.well-known/openid-configuration"

echo "==> running tenant IdP smoke"
GRPC_ADDR="127.0.0.1:${grpc_port}" \
KEYCLOAK_URL="http://127.0.0.1:${kc_port}" \
  bash hack/smoke-tenant-idp.sh

echo "==> REST: list tenants"
curl -fsS "http://127.0.0.1:${http_port}/api/v1/tenants" | jq '{count: (.items | length), names: [.items[].metadata.name]}'

echo "==> REST: IdP-less tenant create, list, delete (existing tenants stay valid)"
name="idp-less-$(date +%s | tail -c 6)"
create="$(curl -fsS -X POST "http://127.0.0.1:${http_port}/api/v1/tenants" \
  -H 'Content-Type: application/json' \
  -d "{\"metadata\":{\"name\":\"${name}\"},\"spec\":{\"domains\":[\"${name}.example.com\"]}}")"
org_id="$(echo "$create" | jq -r '.id')"
version="$(echo "$create" | jq -r '.metadata.version')"
curl -fsS "http://127.0.0.1:${http_port}/api/v1/tenants/${org_id}" | jq -e --arg n "$name" '.metadata.name == $n and .spec.identityProvider == null' >/dev/null
curl -fsS -X DELETE "http://127.0.0.1:${http_port}/api/v1/tenants/${org_id}?version=${version}" >/dev/null
echo "    ${name}: created, listed without identityProvider, deleted"

echo
echo "OK: tenant IdP end-to-end passed (gRPC ${grpc_port}, REST ${http_port}, Keycloak ${kc_port})"
