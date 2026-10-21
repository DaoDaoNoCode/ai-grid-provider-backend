#!/usr/bin/env bash
# Exercise ManagedCluster (Clusters) CRUD against a running ai-grid-provider-backend.
#
# Prerequisite (local):
#   make run   # or: go run ./cmd/... --kubeconfig "$HOME/.kube/config"
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

call "2. Get ${name}" \
  "{\"id\":\"${name}\"}" \
  Get

call "3. List" '{}' List

call "4. Update ${name} (update_mask=metadata.labels)" \
  "{\"object\":{\"id\":\"${name}\",\"metadata\":{\"labels\":{\"ai-grid/enabled\":\"true\",\"ai-grid/region\":\"us-east\"}},\"spec\":{\"hub_accepts_client\":true}},\"update_mask\":{\"paths\":[\"metadata.labels\"]}}" \
  Update

call "5. Get ${name} (after update)" \
  "{\"id\":\"${name}\"}" \
  Get

call "6. Delete ${name}" "{\"id\":\"${name}\"}" Delete

printf '\n==> done (created, got, listed, updated, got, deleted %s)\n' "$name"
