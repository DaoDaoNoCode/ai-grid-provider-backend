#!/usr/bin/env bash
# Start a local Keycloak (Podman preferred) for tenant API development.
#
# Listens on host KEYCLOAK_PORT (default 8180) so it does not collide with
# `make run` gRPC (:8080) or REST (:8081). Seeds the ai-grid realm and sample
# Organizations unless SKIP_SEED=1.
#
# Usage:
#   bash deploy/keycloak/scripts/keycloak-up.sh
#   SKIP_SEED=1 bash deploy/keycloak/scripts/keycloak-up.sh
#   KEYCLOAK_PORT=8280 bash deploy/keycloak/scripts/keycloak-up.sh
#
# Environment:
#   CONTAINER_ENGINE   podman or docker (default: podman, else docker)
#   CONTAINER_NAME     container name (default ai-grid-keycloak)
#   KEYCLOAK_IMAGE     image (default quay.io/keycloak/keycloak:26.3)
#   KEYCLOAK_PORT      host port mapped to container 8080 (default 8180)
#   KEYCLOAK_ADMIN_USERNAME / KEYCLOAK_ADMIN_PASSWORD  (default admin/admin)
#   SKIP_SEED          1 to skip realm/org seeding
#
# Needs: podman or docker. Idempotent if the container is already running.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-$(command -v podman 2>/dev/null || command -v docker || true)}"
CONTAINER_NAME="${CONTAINER_NAME:-ai-grid-keycloak}"
KEYCLOAK_IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.3}"
KEYCLOAK_PORT="${KEYCLOAK_PORT:-8180}"
KEYCLOAK_ADMIN_USERNAME="${KEYCLOAK_ADMIN_USERNAME:-admin}"
KEYCLOAK_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD:-admin}"
SKIP_SEED="${SKIP_SEED:-0}"

for arg in "$@"; do
  case "$arg" in
    --skip-seed) SKIP_SEED=1 ;;
    -h|--help)
      sed -n '2,28p' "$0"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }

[[ -n "${CONTAINER_ENGINE}" ]] || die "podman or docker is required"
command -v curl >/dev/null 2>&1 || die "curl is required"

container_running() {
  "${CONTAINER_ENGINE}" inspect -f '{{.State.Running}}' "${CONTAINER_NAME}" 2>/dev/null | grep -qx true
}

container_exists() {
  "${CONTAINER_ENGINE}" inspect "${CONTAINER_NAME}" >/dev/null 2>&1
}

if container_running; then
  echo "==> ${CONTAINER_NAME} already running"
elif container_exists; then
  echo "==> starting existing ${CONTAINER_NAME}"
  "${CONTAINER_ENGINE}" start "${CONTAINER_NAME}" >/dev/null
else
  echo "==> creating ${CONTAINER_NAME} (${KEYCLOAK_IMAGE}) on localhost:${KEYCLOAK_PORT}"
  "${CONTAINER_ENGINE}" run -d \
    --name "${CONTAINER_NAME}" \
    -p "${KEYCLOAK_PORT}:8080" \
    -e KC_BOOTSTRAP_ADMIN_USERNAME="${KEYCLOAK_ADMIN_USERNAME}" \
    -e KC_BOOTSTRAP_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD}" \
    "${KEYCLOAK_IMAGE}" \
    start-dev --http-port=8080 >/dev/null
fi

KEYCLOAK_URL="http://127.0.0.1:${KEYCLOAK_PORT}"
export KEYCLOAK_URL KEYCLOAK_ADMIN_USERNAME KEYCLOAK_ADMIN_PASSWORD

echo "==> waiting for Keycloak at ${KEYCLOAK_URL}"
deadline=$((SECONDS + 180))
until curl -fsS "${KEYCLOAK_URL}/realms/master/.well-known/openid-configuration" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    die "Keycloak did not become ready within 180s; check: ${CONTAINER_ENGINE} logs ${CONTAINER_NAME}"
  fi
  sleep 2
done
echo "    ready"

if [[ "${SKIP_SEED}" != "1" ]]; then
  bash "${ROOT}/deploy/keycloak/scripts/keycloak-seed.sh"
fi

cat <<EOF

Keycloak admin console: ${KEYCLOAK_URL}  (admin / admin)
Point the local API at it:

  KEYCLOAK_URL=${KEYCLOAK_URL} make run

EOF
