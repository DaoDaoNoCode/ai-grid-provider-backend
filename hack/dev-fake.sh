#!/usr/bin/env bash
# Runs the fake infrastructure (bin/ai-grid-fake-infra: fake hub API and fake
# Keycloak) and the real, unmodified ai-grid-provider-backend against it, in
# one terminal. Ctrl-C or SIGTERM stops both. `make dev-fake` and
# `make dev-fake-poc` build both binaries and call this script; the DEV_*
# variables come from the Makefile.
set -euo pipefail

DEV_HOST="${DEV_HOST:-127.0.0.1}"
DEV_GRPC_PORT="${DEV_GRPC_PORT:-9090}"
DEV_HUB_PORT="${DEV_HUB_PORT:-9091}"
DEV_HTTP_PORT="${DEV_HTTP_PORT:-9092}"
DEV_KEYCLOAK_PORT="${DEV_KEYCLOAK_PORT:-9093}"
DEV_FIXTURES="${DEV_FIXTURES:-fixtures/default}"
DEV_KUBECONFIG="${DEV_KUBECONFIG:-bin/fake-kubeconfig}"
FAKE_INFRA_BIN="${FAKE_INFRA_BIN:-bin/ai-grid-fake-infra}"
BACKEND_BIN="${BACKEND_BIN:-bin/ai-grid-provider-backend}"

# The fake Keycloak's realms and admin credentials. Local fake only; the
# backend gets the same values below, never the caller's KEYCLOAK_* settings.
KC_ADMIN_REALM=master
KC_REALM=ai-grid
KC_ADMIN_USERNAME=admin
KC_ADMIN_PASSWORD=admin

infra_pid=""
backend_pid=""

cleanup() {
  trap - EXIT INT TERM
  for pid in "$backend_pid" "$infra_pid"; do
    [[ -n "$pid" ]] && kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "$backend_pid" "$infra_pid"; do
    [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
  done
  echo "dev-fake: stopped"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

rm -f "$DEV_KUBECONFIG"
"$FAKE_INFRA_BIN" \
  --fixtures "$DEV_FIXTURES" \
  --hub-address "$DEV_HOST:$DEV_HUB_PORT" \
  --keycloak-address "$DEV_HOST:$DEV_KEYCLOAK_PORT" \
  --kubeconfig-out "$DEV_KUBECONFIG" \
  --keycloak-admin-realm "$KC_ADMIN_REALM" \
  --keycloak-realm "$KC_REALM" \
  --keycloak-admin-username "$KC_ADMIN_USERNAME" \
  --keycloak-admin-password "$KC_ADMIN_PASSWORD" &
infra_pid=$!

# The kubeconfig appears once both fakes are listening.
for _ in $(seq 1 100); do
  [[ -f "$DEV_KUBECONFIG" ]] && break
  if ! kill -0 "$infra_pid" 2>/dev/null; then
    echo "dev-fake: ai-grid-fake-infra exited before it was ready" >&2
    exit 1
  fi
  sleep 0.1
done
if [[ ! -f "$DEV_KUBECONFIG" ]]; then
  echo "dev-fake: $DEV_KUBECONFIG did not appear within 10s" >&2
  exit 1
fi

KEYCLOAK_URL="http://$DEV_HOST:$DEV_KEYCLOAK_PORT" \
KEYCLOAK_ADMIN_REALM="$KC_ADMIN_REALM" \
KEYCLOAK_REALM="$KC_REALM" \
KEYCLOAK_ADMIN_USERNAME="$KC_ADMIN_USERNAME" \
KEYCLOAK_ADMIN_PASSWORD="$KC_ADMIN_PASSWORD" \
"$BACKEND_BIN" \
  --listen-address "$DEV_HOST:$DEV_GRPC_PORT" \
  --http-listen-address "$DEV_HOST:$DEV_HTTP_PORT" \
  --kubeconfig "$DEV_KUBECONFIG" &
backend_pid=$!

cat <<EOF
dev-fake: fixtures $DEV_FIXTURES
  backend gRPC      $DEV_HOST:$DEV_GRPC_PORT
  backend REST      http://$DEV_HOST:$DEV_HTTP_PORT/api/v1/clusters  (Swagger UI /docs/)
  fake hub API      https://$DEV_HOST:$DEV_HUB_PORT for the backend ($DEV_KUBECONFIG),
                    http://$DEV_HOST:$DEV_HUB_PORT for the console BFF (same port)
  fake Keycloak     http://$DEV_HOST:$DEV_KEYCLOAK_PORT
Press Ctrl-C to stop both.
EOF

# Stop everything as soon as either process exits.
while kill -0 "$infra_pid" 2>/dev/null && kill -0 "$backend_pid" 2>/dev/null; do
  sleep 0.5
done
echo "dev-fake: a process exited; stopping" >&2
exit 1
