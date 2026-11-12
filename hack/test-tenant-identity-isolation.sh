#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

cluster_name="${KIND_CLUSTER_NAME:-ai-grid-hub}"
kube_context="kind-${cluster_name}"
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"

required_inputs=(
  IDENTITY_ISOLATION_KEYCLOAK_PORT
  IDENTITY_ISOLATION_INTERNAL_KEYCLOAK_URL
  IDENTITY_ISOLATION_BACKEND_PORT
  IDENTITY_ISOLATION_CALLBACK_PORT
  IDENTITY_ISOLATION_TARGET_REALM
  IDENTITY_ISOLATION_UPSTREAM_REALM_A
  IDENTITY_ISOLATION_UPSTREAM_REALM_B
  IDENTITY_ISOLATION_BROKER_CLIENT_ID
  IDENTITY_ISOLATION_UPSTREAM_CLIENT_A
  IDENTITY_ISOLATION_UPSTREAM_CLIENT_B
  IDENTITY_ISOLATION_SHARED_CLIENT_1
  IDENTITY_ISOLATION_SHARED_CLIENT_2
  IDENTITY_ISOLATION_NO_PROFILE_CLIENT
  IDENTITY_ISOLATION_PROFILE_CLIENT
  IDENTITY_ISOLATION_UPSTREAM_SECRET_A
  IDENTITY_ISOLATION_UPSTREAM_SECRET_B
  IDENTITY_ISOLATION_SHARED_SECRET_1
  IDENTITY_ISOLATION_SHARED_SECRET_2
  IDENTITY_ISOLATION_NO_PROFILE_SECRET
  IDENTITY_ISOLATION_PROFILE_SECRET
  KEYCLOAK_ADMIN_REALM
  KEYCLOAK_ADMIN_USERNAME
  KEYCLOAK_ADMIN_PASSWORD
)
missing_inputs=()
for input_name in "${required_inputs[@]}"; do
  if [[ -z "${!input_name:-}" ]]; then
    missing_inputs+=("$input_name")
  fi
done
if ((${#missing_inputs[@]} > 0)); then
  printf 'error: missing required runtime inputs:\n' >&2
  printf '  %s\n' "${missing_inputs[@]}" >&2
  exit 1
fi

keycloak_port="$IDENTITY_ISOLATION_KEYCLOAK_PORT"
internal_keycloak_url="$IDENTITY_ISOLATION_INTERNAL_KEYCLOAK_URL"
backend_port="$IDENTITY_ISOLATION_BACKEND_PORT"
callback_port="$IDENTITY_ISOLATION_CALLBACK_PORT"
target_realm="$IDENTITY_ISOLATION_TARGET_REALM"
upstream_realm_a="$IDENTITY_ISOLATION_UPSTREAM_REALM_A"
upstream_realm_b="$IDENTITY_ISOLATION_UPSTREAM_REALM_B"
broker_client_id="$IDENTITY_ISOLATION_BROKER_CLIENT_ID"
upstream_client_a="$IDENTITY_ISOLATION_UPSTREAM_CLIENT_A"
upstream_client_b="$IDENTITY_ISOLATION_UPSTREAM_CLIENT_B"
shared_client_1="$IDENTITY_ISOLATION_SHARED_CLIENT_1"
shared_client_2="$IDENTITY_ISOLATION_SHARED_CLIENT_2"
no_profile_client="$IDENTITY_ISOLATION_NO_PROFILE_CLIENT"
profile_client="$IDENTITY_ISOLATION_PROFILE_CLIENT"
upstream_secret_a="$IDENTITY_ISOLATION_UPSTREAM_SECRET_A"
upstream_secret_b="$IDENTITY_ISOLATION_UPSTREAM_SECRET_B"
shared_secret_1="$IDENTITY_ISOLATION_SHARED_SECRET_1"
shared_secret_2="$IDENTITY_ISOLATION_SHARED_SECRET_2"
no_profile_secret="$IDENTITY_ISOLATION_NO_PROFILE_SECRET"
profile_secret="$IDENTITY_ISOLATION_PROFILE_SECRET"
admin_username="$KEYCLOAK_ADMIN_USERNAME"
admin_password="$KEYCLOAK_ADMIN_PASSWORD"
admin_realm="$KEYCLOAK_ADMIN_REALM"

for command_name in go podman kind kubectl curl grpcurl; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "error: $command_name is required" >&2
    exit 1
  fi
done

if ! kind get clusters 2>/dev/null | grep -qx "$cluster_name"; then
  kind create cluster --name "$cluster_name" --wait 5m
fi
kubectl config use-context "$kube_context"

apply_namespace() {
  kubectl create namespace "$1" --dry-run=client -o yaml | kubectl apply -f -
}
apply_namespace ai-grid

kubectl apply -f deploy/kind/keycloak.yaml
kubectl -n ai-grid rollout status deployment/keycloak --timeout=5m

apply_namespace ai-grid-idp-secrets

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
  IDENTITY_ISOLATION_TARGET_REALM="$target_realm" \
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
