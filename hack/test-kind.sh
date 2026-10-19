#!/usr/bin/env bash
set -euo pipefail

bash hack/check-prereqs.sh

bash hack/kind-up.sh
bash hack/ocm-up.sh
kubectl -n ai-grid rollout status deployment/ai-grid-provider-backend --timeout=5m

port_forward_log="$(mktemp)"
port_forward_pid=""
cleanup() {
  if [[ -n "$port_forward_pid" ]]; then
    kill "$port_forward_pid" 2>/dev/null || true
  fi
  rm -f "$port_forward_log"
}
trap cleanup EXIT

kubectl -n ai-grid port-forward service/ai-grid-provider-backend 18080:8080 >"$port_forward_log" 2>&1 &
port_forward_pid=$!

for _ in $(seq 1 30); do
  if grpcurl -plaintext 127.0.0.1:18080 list >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

AI_GRID_GRPC_ADDRESS=127.0.0.1:18080 go test ./integration -run 'Test(Ping|List(Clusters|Tenants))$' -v
