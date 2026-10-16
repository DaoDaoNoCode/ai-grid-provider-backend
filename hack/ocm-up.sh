#!/usr/bin/env bash
set -euo pipefail

clusteradm_bin="${CLUSTERADM_BIN:-}"
if [[ -z "$clusteradm_bin" ]] && command -v clusteradm >/dev/null 2>&1; then
  clusteradm_bin="$(command -v clusteradm)"
fi
if [[ -z "$clusteradm_bin" && -x "$(go env GOPATH)/bin/clusteradm" ]]; then
  clusteradm_bin="$(go env GOPATH)/bin/clusteradm"
fi

if [[ -z "$clusteradm_bin" ]]; then
  echo "clusteradm is required: https://github.com/open-cluster-management-io/clusteradm" >&2
  echo "Set CLUSTERADM_BIN=/path/to/clusteradm or add it to PATH." >&2
  exit 1
fi

"$clusteradm_bin" init --wait
kubectl apply -f deploy/kind/sample-managedclusters.yaml
kubectl -n ai-grid wait --for=condition=complete job/keycloak-init --timeout=5m
kubectl -n ai-grid rollout status deployment/ai-grid-provider-backend --timeout=5m
