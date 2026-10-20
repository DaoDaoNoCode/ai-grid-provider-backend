#!/usr/bin/env bash
# Stop and remove the local Keycloak container started by keycloak-up.sh.
#
# Usage:
#   bash deploy/keycloak/scripts/keycloak-down.sh
#   CONTAINER_NAME=ai-grid-keycloak bash deploy/keycloak/scripts/keycloak-down.sh
#
# Environment:
#   CONTAINER_ENGINE   podman or docker (default: podman, else docker)
#   CONTAINER_NAME     container name (default ai-grid-keycloak)
set -euo pipefail

CONTAINER_ENGINE="${CONTAINER_ENGINE:-$(command -v podman 2>/dev/null || command -v docker || true)}"
CONTAINER_NAME="${CONTAINER_NAME:-ai-grid-keycloak}"

for arg in "$@"; do
  case "$arg" in
    -h|--help)
      sed -n '2,14p' "$0"
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

if ! "${CONTAINER_ENGINE}" inspect "${CONTAINER_NAME}" >/dev/null 2>&1; then
  echo "==> ${CONTAINER_NAME} not found; nothing to do"
  exit 0
fi

echo "==> removing ${CONTAINER_NAME}"
"${CONTAINER_ENGINE}" rm -f "${CONTAINER_NAME}" >/dev/null
echo "    removed"
