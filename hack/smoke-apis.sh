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
#   SKIP_MODELS=1 make smoke
#   SKIP_SUBSCRIPTIONS=1 make smoke
#
# Environment:
#   GRPC_ADDR / HTTP_ADDR
#   SKIP_CLUSTERS / SKIP_TENANTS / SKIP_MODELS / SKIP_SUBSCRIPTIONS
# Models need LLMInferenceService / MaaS CRDs (or make dev-fake). Subscriptions
# need MaaS CRDs. When List fails because the CRDs are missing, the suite skips
# that section instead of failing the run.
set -euo pipefail

addr="${GRPC_ADDR:-localhost:8080}"
http_addr="${HTTP_ADDR:-http://localhost:8081}"
skip_clusters="${SKIP_CLUSTERS:-0}"
skip_tenants="${SKIP_TENANTS:-0}"
skip_models="${SKIP_MODELS:-0}"
skip_subscriptions="${SKIP_SUBSCRIPTIONS:-0}"
failed=0

smoke_suffix="$(date +%s | tail -c 6)"
smoke_cluster_grpc="ai-grid-smk-g-${smoke_suffix}"
smoke_cluster_rest="ai-grid-smk-r-${smoke_suffix}"
smoke_tenant_grpc="ai-grid-smk-g-${smoke_suffix}"
smoke_tenant_rest="ai-grid-smk-r-${smoke_suffix}"
smoke_model_grpc="ai-grid-smk-g-${smoke_suffix}"
smoke_model_rest="ai-grid-smk-r-${smoke_suffix}"
smoke_sub_grpc="ai-grid-smk-g-${smoke_suffix}"
smoke_sub_rest="ai-grid-smk-r-${smoke_suffix}"

# Resources created during smoke; deleted on EXIT even if a mid-CRUD call fails.
cleanup_cluster_ids=()
cleanup_tenant_ids=()
cleanup_model_ids=()
cleanup_subscription_ids=()

smoke_cleanup() {
  local id
  for id in "${cleanup_cluster_ids[@]+"${cleanup_cluster_ids[@]}"}"; do
    [[ -n "$id" ]] || continue
    grpcurl -plaintext -d "{\"id\":\"${id}\"}" "$addr" ai_grid.v1.Clusters/Delete >/dev/null 2>&1 || true
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/clusters/${id}" >/dev/null 2>&1 || true
  done
  for id in "${cleanup_tenant_ids[@]+"${cleanup_tenant_ids[@]}"}"; do
    [[ -n "$id" ]] || continue
    delete_tenant_quietly "$id"
  done
  for id in "${cleanup_model_ids[@]+"${cleanup_model_ids[@]}"}"; do
    [[ -n "$id" ]] || continue
    grpcurl -plaintext -d "{\"id\":\"${id}\"}" "$addr" ai_grid.v1.Models/Delete >/dev/null 2>&1 || true
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/models/${id}" >/dev/null 2>&1 || true
  done
  for id in "${cleanup_subscription_ids[@]+"${cleanup_subscription_ids[@]}"}"; do
    [[ -n "$id" ]] || continue
    grpcurl -plaintext -d "{\"id\":\"${id}\"}" "$addr" ai_grid.v1.Subscriptions/Delete >/dev/null 2>&1 || true
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/subscriptions/${id}" >/dev/null 2>&1 || true
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

# tenant_version prints a tenant's metadata.version as Get answers it, or
# nothing when Get fails. Update and Delete require it.
tenant_version() {
  local out
  out="$(grpcurl -plaintext -d "{\"id\":\"$1\"}" "$addr" ai_grid.v1.Tenants/Get 2>/dev/null || true)"
  json_get "$out" '((data.get("object") or {}).get("metadata") or {}).get("version") or ""'
}

# delete_tenant_quietly deletes a tenant at its current version, ignoring
# failures: a gRPC Delete, then a REST one in case gRPC is down.
delete_tenant_quietly() {
  local id="$1" version
  version="$(tenant_version "$id")"
  [[ -n "$version" ]] || return 0
  grpcurl -plaintext -d "{\"id\":\"${id}\",\"version\":${version}}" "$addr" \
    ai_grid.v1.Tenants/Delete >/dev/null 2>&1 \
    || curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/tenants/${id}?version=${version}" >/dev/null 2>&1 \
    || true
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
    delete_tenant_quietly "$prior_id"
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
    version="$(json_get "$CALL_OUT" '((data.get("object") or {}).get("metadata") or {}).get("version") or 0')"
    call_grpc 'Tenants/ListMembers' "{\"id\":\"${smoke_tenant_id}\"}" 'ai_grid.v1.Tenants/ListMembers'
    call_grpc 'Tenants/Update (update_mask=spec.domains)' \
      "{\"object\":{\"id\":\"${smoke_tenant_id}\",\"metadata\":{\"name\":\"${smoke_tenant_grpc}\",\"version\":${version:-0}},\"spec\":{\"domains\":[\"smoke-updated.example.com\"]}},\"update_mask\":{\"paths\":[\"spec.domains\"]}}" \
      'ai_grid.v1.Tenants/Update'
    version="$(json_get "$CALL_OUT" '((data.get("object") or {}).get("metadata") or {}).get("version") or 0')"
    call_grpc 'Tenants/Delete' "{\"id\":\"${smoke_tenant_id}\",\"version\":${version:-0}}" 'ai_grid.v1.Tenants/Delete'
  else
    printf 'fail Tenants gRPC CRUD (missing created id)\n' >&2
    failed=$((failed + 1))
  fi

  call_http 'GET /api/v1/tenants' GET '/api/v1/tenants'
  call_http 'GET /api/v1/tenants?limit=1' GET '/api/v1/tenants?limit=1'
  prior_id="$(tenant_id_by_name "$smoke_tenant_rest")"
  if [[ -n "$prior_id" ]]; then
    delete_tenant_quietly "$prior_id"
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
    version="$(json_get "$CALL_OUT" '(data.get("metadata") or {}).get("version") or 0')"
    call_http "GET /api/v1/tenants/${smoke_tenant_id}:listMembers" GET "/api/v1/tenants/${smoke_tenant_id}:listMembers"
    call_http "PATCH /api/v1/tenants/${smoke_tenant_id}" PATCH "/api/v1/tenants/${smoke_tenant_id}" \
      "{\"id\":\"${smoke_tenant_id}\",\"metadata\":{\"name\":\"${smoke_tenant_rest}\",\"version\":${version:-0}},\"spec\":{\"domains\":[\"smoke-rest-updated.example.com\"]}}"
    version="$(json_get "$CALL_OUT" '(data.get("metadata") or {}).get("version") or 0')"
    call_http "DELETE /api/v1/tenants/${smoke_tenant_id}" DELETE "/api/v1/tenants/${smoke_tenant_id}?version=${version:-0}"
  else
    printf 'fail Tenants REST CRUD (missing created id)\n' >&2
    failed=$((failed + 1))
  fi
fi

if [[ "$skip_models" == "1" ]]; then
  printf '\n==> skipping Models (SKIP_MODELS=1)\n'
else
  printf '    model ids: grpc=%s rest=%s\n' "$smoke_model_grpc" "$smoke_model_rest"
  if ! grpcurl -plaintext -d '{}' "$addr" ai_grid.v1.Models/List >/dev/null 2>&1; then
    printf '\n==> skipping Models (List failed; need LLMInferenceService/MaaS CRDs or make dev-fake)\n'
  else
    call_grpc 'Models/List' '{}' 'ai_grid.v1.Models/List'
    call_grpc 'Models/List (limit=1)' '{"limit":1}' 'ai_grid.v1.Models/List'
    grpcurl -plaintext -d "{\"id\":\"${smoke_model_grpc}\"}" "$addr" \
      ai_grid.v1.Models/Delete >/dev/null 2>&1 || true

    before_failed=$failed
    call_grpc 'Models/Create' \
      "{\"object\":{\"id\":\"${smoke_model_grpc}\",\"metadata\":{\"name\":\"${smoke_model_grpc}\",\"annotations\":{\"openshift.io/display-name\":\"Smoke OPT\"}},\"spec\":{\"model\":{\"uri\":\"hf://facebook/opt-125m\",\"name\":\"facebook/opt-125m\"},\"replicas\":1,\"main\":{\"image\":\"quay.io/example/vllm:smoke\",\"requests\":{\"cpu\":\"100m\",\"memory\":\"1Gi\"},\"limits\":{\"cpu\":\"1\",\"memory\":\"2Gi\"}}}}}" \
      'ai_grid.v1.Models/Create'
    if (( failed == before_failed )); then
      cleanup_model_ids+=("$smoke_model_grpc")
      call_grpc 'Models/Get' "{\"id\":\"${smoke_model_grpc}\"}" 'ai_grid.v1.Models/Get'
      call_grpc 'Models/Update (update_mask=spec.replicas)' \
        "{\"object\":{\"id\":\"${smoke_model_grpc}\",\"metadata\":{\"name\":\"${smoke_model_grpc}\"},\"spec\":{\"replicas\":2}},\"update_mask\":{\"paths\":[\"spec.replicas\"]}}" \
        'ai_grid.v1.Models/Update'
      call_grpc 'Models/Delete' "{\"id\":\"${smoke_model_grpc}\"}" 'ai_grid.v1.Models/Delete'
    fi

    call_http 'GET /api/v1/models' GET '/api/v1/models'
    call_http 'GET /api/v1/models?limit=1' GET '/api/v1/models?limit=1'
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/models/${smoke_model_rest}" || true

    before_failed=$failed
    call_http 'POST /api/v1/models' POST '/api/v1/models' \
      "{\"id\":\"${smoke_model_rest}\",\"metadata\":{\"name\":\"${smoke_model_rest}\",\"annotations\":{\"openshift.io/display-name\":\"Smoke OPT REST\"}},\"spec\":{\"model\":{\"uri\":\"hf://facebook/opt-125m\",\"name\":\"facebook/opt-125m\"},\"replicas\":1,\"main\":{\"image\":\"quay.io/example/vllm:smoke\",\"requests\":{\"cpu\":\"100m\",\"memory\":\"1Gi\"},\"limits\":{\"cpu\":\"1\",\"memory\":\"2Gi\"}}}}"
    if (( failed == before_failed )); then
      cleanup_model_ids+=("$smoke_model_rest")
      call_http "GET /api/v1/models/${smoke_model_rest}" GET "/api/v1/models/${smoke_model_rest}"
      # HTTP body is the Model (proto body: "object"); FieldMask is a query param.
      call_http "PATCH /api/v1/models/${smoke_model_rest}" PATCH "/api/v1/models/${smoke_model_rest}?update_mask=spec.replicas" \
        "{\"id\":\"${smoke_model_rest}\",\"metadata\":{\"name\":\"${smoke_model_rest}\"},\"spec\":{\"replicas\":2}}"
      call_http "DELETE /api/v1/models/${smoke_model_rest}" DELETE "/api/v1/models/${smoke_model_rest}"
    fi
  fi
fi

if [[ "$skip_subscriptions" == "1" ]]; then
  printf '\n==> skipping Subscriptions (SKIP_SUBSCRIPTIONS=1)\n'
else
  printf '    subscription ids: grpc=%s rest=%s\n' "$smoke_sub_grpc" "$smoke_sub_rest"
  if ! grpcurl -plaintext -d '{}' "$addr" ai_grid.v1.Subscriptions/List >/dev/null 2>&1; then
    printf '\n==> skipping Subscriptions (List failed; need MaaS CRDs or make dev-fake)\n'
  else
    call_grpc 'Subscriptions/List' '{}' 'ai_grid.v1.Subscriptions/List'
    call_grpc 'Subscriptions/List (limit=1)' '{"limit":1}' 'ai_grid.v1.Subscriptions/List'
    grpcurl -plaintext -d "{\"id\":\"${smoke_sub_grpc}\"}" "$addr" \
      ai_grid.v1.Subscriptions/Delete >/dev/null 2>&1 || true

    before_failed=$failed
    call_grpc 'Subscriptions/Create' \
      "{\"object\":{\"id\":\"${smoke_sub_grpc}\",\"metadata\":{\"name\":\"${smoke_sub_grpc}\"},\"spec\":{\"owner\":{\"groups\":[\"smoke\"]},\"model_refs\":[{\"name\":\"tiny-llm\",\"namespace\":\"llm\",\"token_rate_limits\":[{\"limit\":50,\"window\":\"1m\"}]}],\"priority\":5}}}" \
      'ai_grid.v1.Subscriptions/Create'
    if (( failed == before_failed )); then
      cleanup_subscription_ids+=("$smoke_sub_grpc")
      call_grpc 'Subscriptions/Get' "{\"id\":\"${smoke_sub_grpc}\"}" 'ai_grid.v1.Subscriptions/Get'
      call_grpc 'Subscriptions/Update (update_mask=spec)' \
        "{\"object\":{\"id\":\"${smoke_sub_grpc}\",\"metadata\":{\"name\":\"${smoke_sub_grpc}\"},\"spec\":{\"owner\":{\"groups\":[\"smoke-admins\"]},\"model_refs\":[{\"name\":\"tiny-llm\",\"namespace\":\"llm\",\"token_rate_limits\":[{\"limit\":50,\"window\":\"1m\"}]}],\"priority\":9}},\"update_mask\":{\"paths\":[\"spec\"]}}" \
        'ai_grid.v1.Subscriptions/Update'
      call_grpc 'Subscriptions/Delete' "{\"id\":\"${smoke_sub_grpc}\"}" 'ai_grid.v1.Subscriptions/Delete'
    fi

    call_http 'GET /api/v1/subscriptions' GET '/api/v1/subscriptions'
    call_http 'GET /api/v1/subscriptions?limit=1' GET '/api/v1/subscriptions?limit=1'
    curl -sS -o /dev/null -X DELETE "${http_addr}/api/v1/subscriptions/${smoke_sub_rest}" || true

    before_failed=$failed
    call_http 'POST /api/v1/subscriptions' POST '/api/v1/subscriptions' \
      "{\"id\":\"${smoke_sub_rest}\",\"metadata\":{\"name\":\"${smoke_sub_rest}\"},\"spec\":{\"owner\":{\"groups\":[\"smoke\"]},\"model_refs\":[{\"name\":\"tiny-llm\",\"namespace\":\"llm\",\"token_rate_limits\":[{\"limit\":50,\"window\":\"1m\"}]}],\"priority\":5}}"
    if (( failed == before_failed )); then
      cleanup_subscription_ids+=("$smoke_sub_rest")
      call_http "GET /api/v1/subscriptions/${smoke_sub_rest}" GET "/api/v1/subscriptions/${smoke_sub_rest}"
      call_http "PATCH /api/v1/subscriptions/${smoke_sub_rest}" PATCH "/api/v1/subscriptions/${smoke_sub_rest}" \
        "{\"id\":\"${smoke_sub_rest}\",\"metadata\":{\"name\":\"${smoke_sub_rest}\"},\"spec\":{\"owner\":{\"groups\":[\"smoke-admins\"]},\"model_refs\":[{\"name\":\"tiny-llm\",\"namespace\":\"llm\",\"token_rate_limits\":[{\"limit\":50,\"window\":\"1m\"}]}],\"priority\":9}}"
      call_http "DELETE /api/v1/subscriptions/${smoke_sub_rest}" DELETE "/api/v1/subscriptions/${smoke_sub_rest}"
    fi
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
