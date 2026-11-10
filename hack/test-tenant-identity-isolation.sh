#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

cluster_name="${KIND_CLUSTER_NAME:-ai-grid-hub}"
kube_context="kind-${cluster_name}"
run_id="${IDENTITY_ISOLATION_RUN_ID:-$(date +%s)}"
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"

keycloak_port="${IDENTITY_ISOLATION_KEYCLOAK_PORT:-18080}"
internal_keycloak_url="${IDENTITY_ISOLATION_INTERNAL_KEYCLOAK_URL:-http://keycloak.ai-grid.svc.cluster.local:8080}"
backend_port="${IDENTITY_ISOLATION_BACKEND_PORT:-19090}"
callback_port="${IDENTITY_ISOLATION_CALLBACK_PORT:-19092}"
target_realm="${KEYCLOAK_REALM:-ai-grid-identity-isolation-${run_id}}"
upstream_realm_a="${IDENTITY_ISOLATION_UPSTREAM_REALM_A:-ai-grid-identity-isolation-upstream-a-${run_id}}"
upstream_realm_b="${IDENTITY_ISOLATION_UPSTREAM_REALM_B:-ai-grid-identity-isolation-upstream-b-${run_id}}"
broker_client_id="${IDENTITY_ISOLATION_BROKER_CLIENT_ID:-identity-isolation-broker}"
upstream_client_a="${IDENTITY_ISOLATION_UPSTREAM_CLIENT_A:-identity-isolation-upstream-a}"
upstream_client_b="${IDENTITY_ISOLATION_UPSTREAM_CLIENT_B:-identity-isolation-upstream-b}"
shared_client_1="${IDENTITY_ISOLATION_SHARED_CLIENT_1:-identity-isolation-shared-1}"
shared_client_2="${IDENTITY_ISOLATION_SHARED_CLIENT_2:-identity-isolation-shared-2}"
no_profile_client="${IDENTITY_ISOLATION_NO_PROFILE_CLIENT:-identity-isolation-no-profile}"
profile_client="${IDENTITY_ISOLATION_PROFILE_CLIENT:-identity-isolation-profile}"
upstream_secret_a="${IDENTITY_ISOLATION_UPSTREAM_SECRET_A:-identity-isolation-upstream-a-secret}"
upstream_secret_b="${IDENTITY_ISOLATION_UPSTREAM_SECRET_B:-identity-isolation-upstream-b-secret}"
shared_secret_1="${IDENTITY_ISOLATION_SHARED_SECRET_1:-identity-isolation-shared-1-secret}"
shared_secret_2="${IDENTITY_ISOLATION_SHARED_SECRET_2:-identity-isolation-shared-2-secret}"
no_profile_secret="${IDENTITY_ISOLATION_NO_PROFILE_SECRET:-identity-isolation-no-profile-secret}"
profile_secret="${IDENTITY_ISOLATION_PROFILE_SECRET:-identity-isolation-profile-secret}"
admin_username="${KEYCLOAK_ADMIN_USERNAME:-admin}"
admin_password="${KEYCLOAK_ADMIN_PASSWORD:-admin}"
admin_realm="${KEYCLOAK_ADMIN_REALM:-master}"

for command_name in go podman kind kubectl curl; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "error: $command_name is required" >&2
    exit 1
  fi
done

if ! kind get clusters 2>/dev/null | grep -qx "$cluster_name"; then
  kind create cluster --name "$cluster_name" --wait 5m
fi
kubectl config use-context "$kube_context"

namespace_manifest="$(mktemp)"
printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ai-grid\n' >"$namespace_manifest"
kubectl apply -f "$namespace_manifest"
rm -f "$namespace_manifest"

kubectl apply -f deploy/kind/keycloak.yaml
kubectl -n ai-grid rollout status deployment/keycloak --timeout=5m

namespace_manifest="$(mktemp)"
printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ai-grid-idp-secrets\n' >"$namespace_manifest"
kubectl apply -f "$namespace_manifest"
rm -f "$namespace_manifest"

create_secret() {
  local name="$1"
  local value="$2"
  kubectl -n ai-grid-idp-secrets create secret generic "$name" \
    --from-literal=client-secret="$value" \
    --dry-run=client -o yaml | kubectl apply -f -
}
create_secret "$upstream_secret_a" "$upstream_secret_a"
create_secret "$upstream_secret_b" "$upstream_secret_b"
create_secret "$shared_secret_1" "$shared_secret_1"
create_secret "$shared_secret_2" "$shared_secret_2"
create_secret "$no_profile_secret" "$no_profile_secret"
create_secret "$profile_secret" "$profile_secret"

work="$(mktemp -d)"
port_forward_pid=""
backend_pid=""
cleanup() {
  if [[ -n "$backend_pid" ]]; then
    kill "$backend_pid" 2>/dev/null || true
    wait "$backend_pid" 2>/dev/null || true
  fi
  if [[ -n "$port_forward_pid" ]]; then
    kill "$port_forward_pid" 2>/dev/null || true
    wait "$port_forward_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

kubectl -n ai-grid port-forward service/keycloak "${keycloak_port}:8080" >"$work/keycloak-port-forward.log" 2>&1 &
port_forward_pid=$!

for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:${keycloak_port}/realms/${admin_realm}/.well-known/openid-configuration" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! curl -fsS "http://127.0.0.1:${keycloak_port}/realms/${admin_realm}/.well-known/openid-configuration" >/dev/null 2>&1; then
  echo "error: Keycloak did not become ready on port ${keycloak_port}" >&2
  exit 1
fi

kubectl config view --minify --raw >"$work/kubeconfig"
go build -o "$work/ai-grid-provider-backend" ./cmd/ai-grid-provider-backend

KEYCLOAK_URL="http://127.0.0.1:${keycloak_port}" \
KEYCLOAK_ADMIN_REALM="$admin_realm" \
KEYCLOAK_REALM="$target_realm" \
KEYCLOAK_ADMIN_USERNAME="$admin_username" \
KEYCLOAK_ADMIN_PASSWORD="$admin_password" \
AI_GRID_IDP_SECRET_NAMESPACE=ai-grid-idp-secrets \
"$work/ai-grid-provider-backend" \
  --listen-address="127.0.0.1:${backend_port}" \
  --http-listen-address="127.0.0.1:$((backend_port + 1))" \
  --search-api-url=off \
  --kubeconfig="$work/kubeconfig" \
  >"$work/backend.log" 2>&1 &
backend_pid=$!

for _ in $(seq 1 60); do
  if grpcurl -plaintext "127.0.0.1:${backend_port}" list >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! grpcurl -plaintext "127.0.0.1:${backend_port}" list >/dev/null 2>&1; then
  echo "error: backend did not become ready on port ${backend_port}" >&2
  cat "$work/backend.log" >&2
  exit 1
fi

status=0
if env \
  AI_GRID_GRPC_ADDRESS="127.0.0.1:${backend_port}" \
  KEYCLOAK_URL="http://127.0.0.1:${keycloak_port}" \
  IDENTITY_ISOLATION_INTERNAL_KEYCLOAK_URL="$internal_keycloak_url" \
  KEYCLOAK_ADMIN_REALM="$admin_realm" \
  KEYCLOAK_REALM="$target_realm" \
  KEYCLOAK_ADMIN_USERNAME="$admin_username" \
  KEYCLOAK_ADMIN_PASSWORD="$admin_password" \
  IDENTITY_ISOLATION_UPSTREAM_REALM_A="$upstream_realm_a" \
  IDENTITY_ISOLATION_UPSTREAM_REALM_B="$upstream_realm_b" \
  IDENTITY_ISOLATION_BROKER_CLIENT_ID="$broker_client_id" \
  IDENTITY_ISOLATION_UPSTREAM_CLIENT_A="$upstream_client_a" \
  IDENTITY_ISOLATION_UPSTREAM_CLIENT_B="$upstream_client_b" \
  IDENTITY_ISOLATION_UPSTREAM_SECRET_A="$upstream_secret_a" \
  IDENTITY_ISOLATION_UPSTREAM_SECRET_B="$upstream_secret_b" \
  IDENTITY_ISOLATION_SHARED_CLIENT_1="$shared_client_1" \
  IDENTITY_ISOLATION_SHARED_CLIENT_2="$shared_client_2" \
  IDENTITY_ISOLATION_SHARED_SECRET_1="$shared_secret_1" \
  IDENTITY_ISOLATION_SHARED_SECRET_2="$shared_secret_2" \
  IDENTITY_ISOLATION_NO_PROFILE_CLIENT="$no_profile_client" \
  IDENTITY_ISOLATION_NO_PROFILE_SECRET="$no_profile_secret" \
  IDENTITY_ISOLATION_PROFILE_CLIENT="$profile_client" \
  IDENTITY_ISOLATION_PROFILE_SECRET="$profile_secret" \
  IDENTITY_ISOLATION_CALLBACK_PORT="$callback_port" \
  go test ./integration -count=1 -run '^TestTenantBrokerIdentityIsolation$' -v; then
  :
else
  status=$?
  echo "backend log:" >&2
  cat "$work/backend.log" >&2
fi
exit "$status"
