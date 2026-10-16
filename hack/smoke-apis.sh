#!/usr/bin/env bash
# Call every ai-grid-provider-backend gRPC and REST API against a running service.
# Prerequisite:
#   kubectl -n ai-grid port-forward service/ai-grid-provider-backend 8080:8080 8081:8081
set -euo pipefail

addr="${GRPC_ADDR:-localhost:8080}"
http_addr="${HTTP_ADDR:-http://localhost:8081}"
failed=0

if ! command -v grpcurl >/dev/null 2>&1; then
  echo "grpcurl is required" >&2
  exit 1
fi

call_grpc() {
  local label="$1"
  local request="$2"
  local method="$3"

  printf '\n==> %s\n' "$label"
  if grpcurl -plaintext -d "$request" "$addr" "$method"; then
    printf 'ok   %s\n' "$label"
  else
    printf 'fail %s\n' "$label" >&2
    failed=$((failed + 1))
  fi
}

call_http() {
  local label="$1"
  local path="$2"

  printf '\n==> %s\n' "$label"
  if curl -fsS "${http_addr}${path}"; then
    printf '\nok   %s\n' "$label"
  else
    printf 'fail %s\n' "$label" >&2
    failed=$((failed + 1))
  fi
}

printf 'gRPC target: %s\n' "$addr"
printf 'HTTP target: %s\n' "$http_addr"

printf '\n==> list services\n'
if grpcurl -plaintext "$addr" list; then
  printf 'ok   list services\n'
else
  printf 'fail list services (is gRPC port-forward running?)\n' >&2
  exit 1
fi

call_grpc 'AiGrid/Ping' '{"name":"smoke"}' 'ai_grid.v1.AiGrid/Ping'
call_grpc 'ManagedClusters/ListManagedClusters' '{}' 'ai_grid.v1.ManagedClusters/ListManagedClusters'
call_grpc 'Tenants/ListTenants' '{}' 'ai_grid.v1.Tenants/ListTenants'
call_grpc 'Health/Check' '{}' 'grpc.health.v1.Health/Check'

call_http 'GET /openapi.json' '/openapi.json'
call_http 'GET /docs/' '/docs/'
call_http 'GET /api/ai-grid/v1/ping?name=smoke' '/api/ai-grid/v1/ping?name=smoke'
call_http 'GET /api/ai-grid/v1/managed_clusters' '/api/ai-grid/v1/managed_clusters'
call_http 'GET /api/ai-grid/v1/tenants' '/api/ai-grid/v1/tenants'

printf '\n'
if (( failed > 0 )); then
  printf '%d API call(s) failed.\n' "$failed" >&2
  exit 1
fi

printf 'All API calls succeeded.\n'
