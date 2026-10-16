#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
missing=0

require_command() {
  local command_name="$1"
  local hint="$2"
  if command -v "$command_name" >/dev/null 2>&1; then
    printf 'ok   %-12s %s\n' "$command_name" "$(command -v "$command_name")"
  else
    printf 'miss %-12s %s\n' "$command_name" "$hint" >&2
    missing=$((missing + 1))
  fi
}

require_command go 'install Go 1.26.3 or newer'
require_command podman 'install Podman'
require_command kind 'install Kind'
require_command kubectl 'install kubectl'
require_command grpcurl 'install grpcurl'

clusteradm_bin="${CLUSTERADM_BIN:-}"
if [[ -z "$clusteradm_bin" ]] && command -v clusteradm >/dev/null 2>&1; then
  clusteradm_bin="$(command -v clusteradm)"
fi
if [[ -z "$clusteradm_bin" && -x "$(go env GOPATH 2>/dev/null)/bin/clusteradm" ]]; then
  clusteradm_bin="$(go env GOPATH)/bin/clusteradm"
fi
if [[ -n "$clusteradm_bin" && -x "$clusteradm_bin" ]]; then
  printf 'ok   %-12s %s\n' clusteradm "$clusteradm_bin"
else
  printf 'miss %-12s install with: go install open-cluster-management.io/clusteradm/cmd/clusteradm@v1.4.0\n' clusteradm >&2
  missing=$((missing + 1))
fi

if command -v go >/dev/null 2>&1; then
  go_version="$(go version | awk '{print $3}')"
  printf 'info Go version   %s (fulfillment-service alignment: go1.26.3)\n' "$go_version"
fi

if command -v podman >/dev/null 2>&1; then
  if ! podman info >/dev/null 2>&1; then
    printf 'miss Podman       Podman is installed but its service/machine is unavailable\n' >&2
    missing=$((missing + 1))
  fi
fi

if (( missing > 0 )); then
  printf '\n%d prerequisite(s) missing. Install them and rerun this script.\n' "$missing" >&2
  exit 1
fi

printf '\nAll ai-grid-provider-backend Kind prerequisites are available.\n'
printf 'Repository: %s\n' "$repo_root"
