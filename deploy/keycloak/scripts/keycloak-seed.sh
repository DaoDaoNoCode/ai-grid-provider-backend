#!/usr/bin/env bash
# Seed the local (or any reachable) Keycloak with the ai-grid realm and sample
# Organizations used by tenant API smoke tests (Kind parity: tenant-a, tenant-b).
#
# Idempotent: realm create tolerates 409; org create tolerates 409.
#
# Usage:
#   bash deploy/keycloak/scripts/keycloak-seed.sh
#   KEYCLOAK_URL=http://127.0.0.1:8180 bash deploy/keycloak/scripts/keycloak-seed.sh
#
# Environment:
#   KEYCLOAK_URL               (default http://127.0.0.1:8180)
#   KEYCLOAK_ADMIN_REALM       (default master)
#   KEYCLOAK_REALM             (default ai-grid)
#   KEYCLOAK_ADMIN_USERNAME    (default admin)
#   KEYCLOAK_ADMIN_PASSWORD    (default admin)
#   TENANTS                    space-separated org names (default "tenant-a tenant-b")
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:-http://127.0.0.1:8180}"
KEYCLOAK_ADMIN_REALM="${KEYCLOAK_ADMIN_REALM:-master}"
KEYCLOAK_REALM="${KEYCLOAK_REALM:-ai-grid}"
KEYCLOAK_ADMIN_USERNAME="${KEYCLOAK_ADMIN_USERNAME:-admin}"
KEYCLOAK_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD:-admin}"
TENANTS="${TENANTS:-tenant-a tenant-b}"

for arg in "$@"; do
  case "$arg" in
    -h|--help)
      sed -n '2,22p' "$0"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required"

base="${KEYCLOAK_URL%/}"

echo "==> seeding Keycloak at ${base} (realm ${KEYCLOAK_REALM})"

deadline=$((SECONDS + 120))
until curl -fsS "${base}/realms/${KEYCLOAK_ADMIN_REALM}/.well-known/openid-configuration" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    die "Keycloak not reachable at ${base}; run make keycloak-up first"
  fi
  sleep 2
done

token="$(
  curl -fsS -X POST "${base}/realms/${KEYCLOAK_ADMIN_REALM}/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode grant_type=password \
    --data-urlencode client_id=admin-cli \
    --data-urlencode username="${KEYCLOAK_ADMIN_USERNAME}" \
    --data-urlencode password="${KEYCLOAK_ADMIN_PASSWORD}" \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
)"
[[ -n "${token}" ]] || die "failed to obtain admin access token"
auth="Authorization: Bearer ${token}"

code="$(
  curl -sS -o /dev/null -w '%{http_code}' -X POST "${base}/admin/realms" \
    -H "${auth}" -H 'Content-Type: application/json' \
    --data "{\"realm\":\"${KEYCLOAK_REALM}\",\"enabled\":true}"
)"
[[ "${code}" =~ ^(201|409)$ ]] || die "create realm ${KEYCLOAK_REALM}: HTTP ${code}"
echo "    realm ${KEYCLOAK_REALM}: HTTP ${code}"

code="$(
  curl -sS -o /dev/null -w '%{http_code}' -X PUT "${base}/admin/realms/${KEYCLOAK_REALM}" \
    -H "${auth}" -H 'Content-Type: application/json' \
    --data "{\"realm\":\"${KEYCLOAK_REALM}\",\"enabled\":true,\"organizationsEnabled\":true}"
)"
[[ "${code}" == "204" ]] || die "enable organizations on ${KEYCLOAK_REALM}: HTTP ${code}"
echo "    organizationsEnabled: HTTP ${code}"

for tenant in ${TENANTS}; do
  code="$(
    curl -sS -o /dev/null -w '%{http_code}' -X POST \
      "${base}/admin/realms/${KEYCLOAK_REALM}/organizations" \
      -H "${auth}" -H 'Content-Type: application/json' \
      --data "{\"name\":\"${tenant}\",\"alias\":\"${tenant}\",\"domains\":[{\"name\":\"${tenant}.example.com\",\"verified\":false}]}"
  )"
  [[ "${code}" =~ ^(201|409)$ ]] || die "create organization ${tenant}: HTTP ${code}"
  echo "    organization ${tenant}: HTTP ${code}"
done

echo "==> seed complete (tenants: ${TENANTS})"
