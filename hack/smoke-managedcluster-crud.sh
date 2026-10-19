#!/usr/bin/env bash
# Exercise ManagedCluster CRUD against a running ai-grid-provider-backend.
#
# Prerequisite (local):
#   go run ./cmd/ai-grid-provider-backend --kubeconfig "$HOME/.kube/config"
#
# Usage:
#   bash hack/smoke-managedcluster-crud.sh
#   GRPC_ADDR=localhost:8080 NAME=demo-cluster bash hack/smoke-managedcluster-crud.sh
set -euo pipefail

addr="${GRPC_ADDR:-localhost:8080}"
name="${NAME:-ai-grid-crud-demo}"
svc="ai_grid.v1.Clusters"

if ! command -v grpcurl >/dev/null 2>&1; then
  echo "grpcurl is required" >&2
  exit 1
fi

call() {
  local label="$1" request="$2" method="$3"
  printf '\n==> %s\n' "$label"
  grpcurl -plaintext -d "$request" "$addr" "${svc}/${method}"
}

# Best-effort cleanup if a prior run left the object.
grpcurl -plaintext -d "{\"id\":\"${name}\"}" "$addr" "${svc}/Delete" >/dev/null 2>&1 || true

call "1. Create ${name}" \
  "{\"object\":{\"id\":\"${name}\",\"metadata\":{\"name\":\"${name}\",\"labels\":{\"environment\":\"dev\",\"ai-grid/enabled\":\"false\"}},\"spec\":{\"hub_accepts_client\":true}}}" \
  Create

call "2. List" '{}' List

call "3. Update ${name} (enable + region)" \
  "{\"object\":{\"id\":\"${name}\",\"metadata\":{\"labels\":{\"ai-grid/enabled\":\"true\",\"ai-grid/region\":\"us-east\"}},\"spec\":{\"hub_accepts_client\":true}}}" \
  Update

call "4. List (after update)" '{}' List

call "5. Delete ${name}" "{\"id\":\"${name}\"}" Delete

printf '\n==> done (created, listed, updated, listed, deleted %s)\n' "$name"
