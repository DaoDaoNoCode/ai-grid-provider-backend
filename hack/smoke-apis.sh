#!/usr/bin/env bash
# Call every ai-grid-provider-backend gRPC and REST API against a running service.
#
# Prerequisite (pick one):
#   KEYCLOAK_URL=http://127.0.0.1:8180 make run   # local process + Keycloak for Tenants
#   make smoke-openshift
#   kubectl -n ai-grid port-forward svc/ai-grid-provider-backend 8080:8080 8081:8081
#
# Usage:
#   make smoke
#   SKIP_CLUSTERS=1 make smoke
#   SKIP_TENANTS=1 make smoke
#
# Environment:
#   GRPC_ADDR / HTTP_ADDR
#   SKIP_CLUSTERS / SKIP_TENANTS
set -euo pipefail

addr="${GRPC_ADDR:-localhost:8080}"
http_addr="${HTTP_ADDR:-http://localhost:8081}"
skip_clusters="${SKIP_CLUSTERS:-0}"
skip_tenants="${SKIP_TENANTS:-0}"
failed=0

smoke_suffix="$(date +%s | tail -c 6)"
smoke_cluster_grpc="ai-grid-smk-g-${smoke_suffix}"
smoke_cluster_rest="ai-grid-smk-r-${smoke_suffix}"
smoke_tenant_grpc="ai-grid-smk-g-${smoke_suffix}"
smoke_tenant_rest="ai-grid-smk-r-${smoke_suffix}"

# Resources created during smoke; deleted on EXIT even if a mid-CRUD call fails.
cleanup_cluster_ids=()
cleanup_tenant_ids=()

smoke_cleanup() {
  local id
  for id in "${cleanup_cluster_ids[@]+"${cleanup_cluster_ids[@]}"}"; do
    [[ -n "$id" ]] || continue
    grpcurl -plaintext -d "{\"id\":\"${id}\"}" "$addr" ai_grid.v1.Clusters/Delete >/dev/null 2>&1 || true
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/clusters/${id}" >/dev/null 2>&1 || true
  done
  for id in "${cleanup_tenant_ids[@]+"${cleanup_tenant_ids[@]}"}"; do
    [[ -n "$id" ]] || continue
    grpcurl -plaintext -d "{\"id\":\"${id}\"}" "$addr" ai_grid.v1.Tenants/Delete >/dev/null 2>&1 || true
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/tenants/${id}" >/dev/null 2>&1 || true
  done
}
trap smoke_cleanup EXIT

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: $1 required" >&2; exit 1; }; }
need grpcurl
need curl
need python3

json_get() {
  local raw="$1" expr="$2"
  printf '%s' "$raw" | python3 -c "
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
print(${expr})
" 2>/dev/null || true
}

# Helpers always return 0 so set -e does not abort mid-CRUD before cleanup/Delete.
# Failures increment failed and are summarized at the end.
call_grpc() {
  local label="$1" request="$2" method="$3" out="" ec=0
  printf '\n==> %s\n' "$label"
  set +e
  out="$(grpcurl -plaintext -d "$request" "$addr" "$method" 2>&1)"
  ec=$?
  set -e
  if (( ec == 0 )); then
    printf '%s\n' "$out"
    printf 'ok   %s\n' "$label"
    CALL_OUT="$out"
    return 0
  fi
  printf '%s\n' "$out" >&2
  printf 'fail %s\n' "$label" >&2
  failed=$((failed + 1))
  CALL_OUT=""
  return 0
}

call_http() {
  local label="$1" method="$2" path="$3" body="${4:-}"
  local url="${http_addr}${path}"
  local tmp status
  printf '\n==> %s\n' "$label"
  tmp="$(mktemp)"
  if [[ -n "$body" ]]; then
    status="$(curl -sS -o "$tmp" -w '%{http_code}' -X "$method" \
      -H 'Content-Type: application/json' -d "$body" "$url" || true)"
  else
    status="$(curl -sS -o "$tmp" -w '%{http_code}' -X "$method" "$url" || true)"
  fi
  CALL_OUT="$(cat "$tmp")"
  rm -f "$tmp"
  printf '%s\n' "$CALL_OUT"
  if [[ "$status" =~ ^2 ]]; then
    printf 'ok   %s (HTTP %s)\n' "$label" "$status"
    return 0
  fi
  printf 'fail %s (HTTP %s)\n' "$label" "$status" >&2
  failed=$((failed + 1))
  CALL_OUT=""
  return 0
}

tenant_id_by_name() {
  local name="$1" list_out
  list_out="$(grpcurl -plaintext -d '{}' "$addr" ai_grid.v1.Tenants/List 2>/dev/null || true)"
  printf '%s' "$list_out" | python3 -c '
import json, sys
name = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
for item in data.get("items") or []:
    meta = item.get("metadata") or {}
    if meta.get("name") == name:
        print(item.get("id") or "")
        break
' "$name" 2>/dev/null || true
}

printf 'gRPC target: %s\n' "$addr"
printf 'HTTP target: %s\n' "$http_addr"

printf '\n==> list services\n'
if grpcurl -plaintext "$addr" list; then
  printf 'ok   list services\n'
else
  printf 'fail list services (is the API listening on %s?)\n' "$addr" >&2
  exit 1
fi

call_grpc 'AiGrid/Ping' '{"name":"smoke"}' 'ai_grid.v1.AiGrid/Ping'
call_http 'GET /api/v1/ping?name=smoke' GET '/api/v1/ping?name=smoke'

if [[ "$skip_clusters" == "1" ]]; then
  printf '\n==> skipping Clusters (SKIP_CLUSTERS=1)\n'
else
  printf '    cluster ids: grpc=%s rest=%s\n' "$smoke_cluster_grpc" "$smoke_cluster_rest"
  call_grpc 'Clusters/List' '{}' 'ai_grid.v1.Clusters/List'
  call_grpc 'Clusters/List (limit=1)' '{"limit":1}' 'ai_grid.v1.Clusters/List'
  grpcurl -plaintext -d "{\"id\":\"${smoke_cluster_grpc}\"}" "$addr" \
    ai_grid.v1.Clusters/Delete >/dev/null 2>&1 || true

  before_failed=$failed
  call_grpc 'Clusters/Create' \
    "{\"object\":{\"id\":\"${smoke_cluster_grpc}\",\"metadata\":{\"name\":\"${smoke_cluster_grpc}\",\"labels\":{\"environment\":\"smoke\"}},\"spec\":{\"hub_accepts_client\":true}}}" \
    'ai_grid.v1.Clusters/Create'
  if (( failed == before_failed )); then
    cleanup_cluster_ids+=("$smoke_cluster_grpc")
  fi
  call_grpc 'Clusters/Get' "{\"id\":\"${smoke_cluster_grpc}\"}" 'ai_grid.v1.Clusters/Get'
  call_grpc 'Clusters/Update (update_mask=metadata.labels)' \
    "{\"object\":{\"id\":\"${smoke_cluster_grpc}\",\"metadata\":{\"labels\":{\"environment\":\"smoke\",\"ai-grid/enabled\":\"true\"}},\"spec\":{\"hub_accepts_client\":true}},\"update_mask\":{\"paths\":[\"metadata.labels\"]}}" \
    'ai_grid.v1.Clusters/Update'
  call_grpc 'Clusters/Delete' "{\"id\":\"${smoke_cluster_grpc}\"}" 'ai_grid.v1.Clusters/Delete'

  call_http 'GET /api/v1/clusters' GET '/api/v1/clusters'
  call_http 'GET /api/v1/clusters?limit=1' GET '/api/v1/clusters?limit=1'
  curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/clusters/${smoke_cluster_rest}" || true

  before_failed=$failed
  call_http 'POST /api/v1/clusters' POST '/api/v1/clusters' \
    "{\"id\":\"${smoke_cluster_rest}\",\"metadata\":{\"name\":\"${smoke_cluster_rest}\",\"labels\":{\"environment\":\"smoke\"}},\"spec\":{\"hub_accepts_client\":true}}"
  if (( failed == before_failed )); then
    cleanup_cluster_ids+=("$smoke_cluster_rest")
  fi
  call_http "GET /api/v1/clusters/${smoke_cluster_rest}" GET "/api/v1/clusters/${smoke_cluster_rest}"
  call_http "PATCH /api/v1/clusters/${smoke_cluster_rest}" PATCH "/api/v1/clusters/${smoke_cluster_rest}" \
    "{\"id\":\"${smoke_cluster_rest}\",\"metadata\":{\"labels\":{\"environment\":\"smoke\",\"ai-grid/enabled\":\"true\"}},\"spec\":{\"hub_accepts_client\":true}}"
  call_http "DELETE /api/v1/clusters/${smoke_cluster_rest}" DELETE "/api/v1/clusters/${smoke_cluster_rest}"
fi

if [[ "$skip_tenants" == "1" ]]; then
  printf '\n==> skipping Tenants (SKIP_TENANTS=1)\n'
else
  printf '    tenant names: grpc=%s rest=%s\n' "$smoke_tenant_grpc" "$smoke_tenant_rest"
  call_grpc 'Tenants/List' '{}' 'ai_grid.v1.Tenants/List'
  call_grpc 'Tenants/List (limit=1)' '{"limit":1}' 'ai_grid.v1.Tenants/List'

  prior_id="$(tenant_id_by_name "$smoke_tenant_grpc")"
  if [[ -n "$prior_id" ]]; then
    grpcurl -plaintext -d "{\"id\":\"${prior_id}\"}" "$addr" \
      ai_grid.v1.Tenants/Delete >/dev/null 2>&1 || true
  fi

  CALL_OUT=""
  before_failed=$failed
  call_grpc 'Tenants/Create' \
    "{\"object\":{\"metadata\":{\"name\":\"${smoke_tenant_grpc}\"},\"spec\":{\"domains\":[\"smoke.example.com\"]}}}" \
    'ai_grid.v1.Tenants/Create'
  smoke_tenant_id=""
  if (( failed == before_failed )); then
    smoke_tenant_id="$(json_get "$CALL_OUT" '(data.get("object") or data).get("id") or ""')"
  fi
  if [[ -n "$smoke_tenant_id" ]]; then
    cleanup_tenant_ids+=("$smoke_tenant_id")
    call_grpc 'Tenants/Get' "{\"id\":\"${smoke_tenant_id}\"}" 'ai_grid.v1.Tenants/Get'
    call_grpc 'Tenants/ListMembers' "{\"id\":\"${smoke_tenant_id}\"}" 'ai_grid.v1.Tenants/ListMembers'
    call_grpc 'Tenants/Update (update_mask=spec.domains)' \
      "{\"object\":{\"id\":\"${smoke_tenant_id}\",\"metadata\":{\"name\":\"${smoke_tenant_grpc}\"},\"spec\":{\"domains\":[\"smoke-updated.example.com\"]}},\"update_mask\":{\"paths\":[\"spec.domains\"]}}" \
      'ai_grid.v1.Tenants/Update'
    call_grpc 'Tenants/Delete' "{\"id\":\"${smoke_tenant_id}\"}" 'ai_grid.v1.Tenants/Delete'
  else
    printf 'fail Tenants gRPC CRUD (missing created id)\n' >&2
    failed=$((failed + 1))
  fi

  call_http 'GET /api/v1/tenants' GET '/api/v1/tenants'
  call_http 'GET /api/v1/tenants?limit=1' GET '/api/v1/tenants?limit=1'
  prior_id="$(tenant_id_by_name "$smoke_tenant_rest")"
  if [[ -n "$prior_id" ]]; then
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/tenants/${prior_id}" || true
  fi
  CALL_OUT=""
  before_failed=$failed
  call_http 'POST /api/v1/tenants' POST '/api/v1/tenants' \
    "{\"metadata\":{\"name\":\"${smoke_tenant_rest}\"},\"spec\":{\"domains\":[\"smoke-rest.example.com\"]}}"
  smoke_tenant_id=""
  if (( failed == before_failed )); then
    smoke_tenant_id="$(json_get "$CALL_OUT" 'data.get("id") or ""')"
  fi
  if [[ -n "$smoke_tenant_id" ]]; then
    cleanup_tenant_ids+=("$smoke_tenant_id")
    call_http "GET /api/v1/tenants/${smoke_tenant_id}" GET "/api/v1/tenants/${smoke_tenant_id}"
    call_http "GET /api/v1/tenants/${smoke_tenant_id}:listMembers" GET "/api/v1/tenants/${smoke_tenant_id}:listMembers"
    call_http "PATCH /api/v1/tenants/${smoke_tenant_id}" PATCH "/api/v1/tenants/${smoke_tenant_id}" \
      "{\"id\":\"${smoke_tenant_id}\",\"metadata\":{\"name\":\"${smoke_tenant_rest}\"},\"spec\":{\"domains\":[\"smoke-rest-updated.example.com\"]}}"
    call_http "DELETE /api/v1/tenants/${smoke_tenant_id}" DELETE "/api/v1/tenants/${smoke_tenant_id}"
  else
    printf 'fail Tenants REST CRUD (missing created id)\n' >&2
    failed=$((failed + 1))
  fi
fi

call_grpc 'Health/Check' '{}' 'grpc.health.v1.Health/Check'
call_http 'GET /openapi.json' GET '/openapi.json'
call_http 'GET /docs/' GET '/docs/'

printf '\n'
if (( failed > 0 )); then
  printf '%d API call(s) failed.\n' "$failed" >&2
  exit 1
fi
printf 'All API calls succeeded.\n'
