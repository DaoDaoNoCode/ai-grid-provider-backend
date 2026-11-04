#!/usr/bin/env bash
# Compare local Go / buf / golangci-lint against the pins CI uses
# (.github/workflows/ci.yaml and go.mod). Missing tools fail; version drift
# warns and exits 0 so a slightly newer local install does not block day-to-day
# work — upgrade (or pin) when CI and local disagree on lint/proto results.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ci_yaml="${repo_root}/.github/workflows/ci.yaml"
go_mod="${repo_root}/go.mod"

missing=0
warned=0

pin_from_ci() {
  local key="$1"
  # GOLANGCI_LINT_VERSION: v2.12.2  /  BUF_VERSION: "1.50.0"
  awk -F': ' -v k="$key" '
    $1 ~ "^[[:space:]]*"k"$" {
      gsub(/["'\'' ]/, "", $2)
      print $2
      exit
    }
  ' "$ci_yaml"
}

go_pin() {
  awk '/^go / { print $2; exit }' "$go_mod"
}

# Strip a leading v from versions so 1.50.0 and v1.50.0 compare equal.
normalize_ver() {
  local v="$1"
  v="${v#v}"
  printf '%s' "$v"
}

# True when major.minor match (patch may differ).
same_major_minor() {
  local a b
  a="$(normalize_ver "$1")"
  b="$(normalize_ver "$2")"
  local a_mm b_mm
  a_mm="$(printf '%s' "$a" | awk -F. '{print $1"."$2}')"
  b_mm="$(printf '%s' "$b" | awk -F. '{print $1"."$2}')"
  [[ "$a_mm" == "$b_mm" ]]
}

require_command() {
  local command_name="$1"
  local hint="$2"
  if command -v "$command_name" >/dev/null 2>&1; then
    printf 'ok   %-14s %s\n' "$command_name" "$(command -v "$command_name")"
    return 0
  fi
  printf 'miss %-14s %s\n' "$command_name" "$hint" >&2
  missing=$((missing + 1))
  return 1
}

warn_drift() {
  local tool="$1"
  local have="$2"
  local want="$3"
  printf 'warn %-14s have %s; CI pin is %s\n' "$tool" "$have" "$want" >&2
  printf '     install/upgrade to match CI, or expect local lint/proto results to differ.\n' >&2
  warned=$((warned + 1))
}

buf_pin="$(pin_from_ci BUF_VERSION)"
golangci_pin="$(pin_from_ci GOLANGCI_LINT_VERSION)"
go_want="$(go_pin)"

printf 'CI pins: go %s, buf %s, golangci-lint %s\n' "$go_want" "$buf_pin" "$golangci_pin"

if require_command go "install Go ${go_want} (see go.mod)"; then
  # go version → "go version go1.27.3 darwin/arm64"
  go_have="$(go version | awk '{print $3}')"
  go_have="${go_have#go}"
  if ! same_major_minor "$go_have" "$go_want"; then
    warn_drift go "$go_have" "$go_want"
  else
    printf 'ok   %-14s %s (go.mod %s)\n' "go version" "$go_have" "$go_want"
  fi
fi

if require_command buf "install buf ${buf_pin} (https://buf.build/docs/installation) or: go run github.com/bufbuild/buf/cmd/buf@v${buf_pin}"; then
  buf_have="$(normalize_ver "$(buf --version 2>/dev/null | head -n1 | awk '{print $NF}')")"
  if [[ "$buf_have" != "$(normalize_ver "$buf_pin")" ]]; then
    warn_drift buf "$buf_have" "$buf_pin"
  else
    printf 'ok   %-14s %s\n' "buf version" "$buf_have"
  fi
fi

if require_command golangci-lint "install golangci-lint ${golangci_pin} (https://golangci-lint.run/welcome/install/)"; then
  # "golangci-lint has version 2.15.0 built with ..."
  lint_have="$(golangci-lint version 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="version") {print $(i+1); exit}}')"
  if [[ -z "$lint_have" ]]; then
    lint_have="$(golangci-lint version --short 2>/dev/null || true)"
  fi
  if [[ "$(normalize_ver "$lint_have")" != "$(normalize_ver "$golangci_pin")" ]]; then
    warn_drift golangci-lint "$lint_have" "$golangci_pin"
  else
    printf 'ok   %-14s %s\n' "golangci-lint" "$lint_have"
  fi
fi

if (( missing > 0 )); then
  printf '\n%d tooling prerequisite(s) missing. Install the CI pins above and rerun.\n' "$missing" >&2
  exit 1
fi

if (( warned > 0 )); then
  printf '\nTooling is present with %d version drift warning(s); make verify still runs.\n' "$warned"
else
  printf '\nLocal Go / buf / golangci-lint match CI pins (or major.minor for Go).\n'
fi
printf 'Repository: %s\n' "$repo_root"
