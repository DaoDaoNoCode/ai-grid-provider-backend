#!/usr/bin/env bash
# Runs the backend on this machine against the hub of your `oc login`, set up
# like the console deployed there (make run-hub; README.md, "Run the API
# locally"). It uses the current kubeconfig context and never switches it.
#
#   - listens on LISTEN_ADDRESS (127.0.0.1:9090, gRPC and Connect) and
#     HTTP_LISTEN_ADDRESS (127.0.0.1:9092, REST), where the console's make dev
#     expects it; a non-loopback address is refused: the backend has no
#     authentication;
#   - port-forwards ACM Search (svc/search-search-api in
#     open-cluster-management) for cluster GPUs, or runs with
#     --search-api-url=off when the hub has no such Service;
#   - copies the plain-value Git and namespace settings of the deployed
#     console's ai-grid-backend container (the Deployment labelled
#     app.kubernetes.io/name=ai-grid-provider-console), never its Git token:
#     Git writes fail with the backend's own error;
#   - with KEYCLOAK=hub, reads that console's Keycloak Secret straight into
#     the backend's environment, never printing it; otherwise KEYCLOAK_URL is
#     make run's default.
#
# Ctrl-C or SIGTERM stops the backend and the port-forward together.
set -euo pipefail

LISTEN_ADDRESS="${LISTEN_ADDRESS:-127.0.0.1:9090}"
HTTP_LISTEN_ADDRESS="${HTTP_LISTEN_ADDRESS:-127.0.0.1:9092}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-${KUBECONFIG:-$HOME/.kube/config}}"
# Every oc call below and the backend use the same kubeconfig, so they act on
# the same hub.
export KUBECONFIG="$KUBECONFIG_PATH"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://127.0.0.1:8180}"
KEYCLOAK="${KEYCLOAK:-}"
BACKEND_BIN="${BACKEND_BIN:-bin/ai-grid-provider-backend}"
DEV_CATALOG="${DEV_CATALOG:-bin/dev-catalog.git}"
RUN_ARGS="${RUN_ARGS:-}"
CONSOLE_SELECTOR="app.kubernetes.io/name=ai-grid-provider-console"
SEARCH_NAMESPACE="open-cluster-management"
SEARCH_SERVICE="search-search-api"
# The deployed backend's settings worth copying: plain values only. Its
# AI_GRID_GIT_TOKEN_FILE points at a Secret and is never copied.
COPY_ENV=(AI_GRID_GIT_URL AI_GRID_GIT_BRANCH AI_GRID_GRID_NAMESPACE
  AI_GRID_MODELS_NAMESPACE AI_GRID_SUBSCRIPTION_NAMESPACE)

die() { echo "run-hub: $*" >&2; exit 1; }
say() { echo "run-hub: $*"; }

command -v oc >/dev/null 2>&1 || die "oc is required and must be on PATH"
case "$KEYCLOAK" in
  "" | hub) ;;
  *) die "KEYCLOAK=$KEYCLOAK: the only value is hub" ;;
esac

# 127.0.0.1:9090, localhost:9090 or [::1]:9090; not :9090 or 0.0.0.0:9090.
loopback() {
  case "${1%:*}" in
    127.0.0.1 | localhost | "[::1]") return 0 ;;
    *) return 1 ;;
  esac
}
for address in "$LISTEN_ADDRESS" "$HTTP_LISTEN_ADDRESS"; do
  loopback "$address" ||
    die "$address is not a loopback address; the backend has no authentication, so it listens on 127.0.0.1 only"
done

if ! user="$(oc whoami 2>/dev/null)"; then
  die "Log in to the hub first: oc login <server> ..."
fi
server="$(oc whoami --show-server)"
say "hub $server as $user (context $(oc config current-context))"

# The deployed console, if exactly one.
console_ns=""
console_name=""
# oc's messages go to the terminal, never into the list it prints.
if ! found="$(oc get deployment -A -l "$CONSOLE_SELECTOR" \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}')"; then
  say "cannot list Deployments (oc's message above); using make run's defaults"
  found=""
fi
count="$(grep -c . <<<"$found" || true)"
if [[ "$count" == 1 ]]; then
  console_ns="${found%%/*}"
  console_name="${found#*/}"
  say "deployed console $console_ns/$console_name"
elif [[ "$count" == 0 ]]; then
  say "no Deployment labelled $CONSOLE_SELECTOR; using make run's defaults"
else
  say "$count Deployments labelled $CONSOLE_SELECTOR; using make run's defaults:"
  say "  ${found//$'\n'/ }"
fi

# Never the caller's token either: Git writes must fail.
unset AI_GRID_GIT_TOKEN_FILE
if [[ -n "$console_name" ]]; then
  env_lines="$(oc -n "$console_ns" get deployment "$console_name" -o \
    jsonpath='{range .spec.template.spec.containers[?(@.name=="ai-grid-backend")].env[*]}{.name}={.value}{"\n"}{end}')"
  while IFS= read -r line; do
    name="${line%%=*}"
    value="${line#*=}"
    for wanted in "${COPY_ENV[@]}"; do
      if [[ "$name" == "$wanted" && -n "$value" ]]; then
        export "$name=$value"
        say "  $name=$value"
      fi
    done
  done <<<"$env_lines"
fi
if [[ -z "${AI_GRID_GIT_URL:-}" ]]; then
  DEV_CATALOG="$DEV_CATALOG" bash "$(dirname "$0")/dev-catalog.sh" init
  export AI_GRID_GIT_URL="file://$PWD/$DEV_CATALOG"
  say "git $AI_GRID_GIT_URL (make run's local catalog)"
else
  say "git $AI_GRID_GIT_URL without a token: Models and Subscriptions read it, writes fail"
fi

# KEYCLOAK=hub: the Secret the deployed backend's KEYCLOAK_URL comes from.
keycloak_secret=""
if [[ "$KEYCLOAK" == hub ]]; then
  [[ -n "$console_name" ]] || die "KEYCLOAK=hub needs exactly one deployed console to read its Keycloak Secret from"
  keycloak_secret="$(oc -n "$console_ns" get deployment "$console_name" -o \
    jsonpath='{.spec.template.spec.containers[?(@.name=="ai-grid-backend")].env[?(@.name=="KEYCLOAK_URL")].valueFrom.secretKeyRef.name}')"
  [[ -n "$keycloak_secret" ]] || die "the deployed backend takes KEYCLOAK_URL from no Secret"
  keys=" $(oc -n "$console_ns" get secret "$keycloak_secret" \
    -o go-template='{{range $k, $v := .data}}{{$k}} {{end}}') "
  for key in url admin-realm realm username password; do
    [[ "$keys" == *" $key "* ]] || die "Secret $console_ns/$keycloak_secret has no key $key"
  done
  say "keycloak from Secret $console_ns/$keycloak_secret (values not shown)"
else
  say "keycloak $KEYCLOAK_URL (make run's default); Organizations need KEYCLOAK=hub"
fi
# Prints one key of the Keycloak Secret, only ever into the backend's
# environment below.
keycloak_value() {
  oc -n "$console_ns" get secret "$keycloak_secret" \
    -o go-template="{{index .data \"$1\" | base64decode}}"
}

pf_pid=""
backend_pid=""
pf_log="$(mktemp -t ai-grid-run-hub-pf.XXXXXX)"

# Runs from the EXIT trap below.
# shellcheck disable=SC2329
cleanup() {
  trap - EXIT INT TERM
  for pid in "$backend_pid" "$pf_pid"; do
    [[ -n "$pid" ]] && kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "$backend_pid" "$pf_pid"; do
    [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
  done
  rm -f "$pf_log"
  say "stopped"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

search_args=(--search-api-url=off)
if oc -n "$SEARCH_NAMESPACE" get service "$SEARCH_SERVICE" >/dev/null 2>&1; then
  # :4010 lets oc pick a free local port, which it prints.
  oc -n "$SEARCH_NAMESPACE" port-forward --address 127.0.0.1 \
    "service/$SEARCH_SERVICE" :4010 >"$pf_log" 2>&1 &
  pf_pid=$!
  search_port=""
  for _ in $(seq 1 100); do
    search_port="$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$pf_log")"
    [[ -n "$search_port" ]] && break
    kill -0 "$pf_pid" 2>/dev/null || break
    sleep 0.1
  done
  if [[ -z "$search_port" ]]; then
    cat "$pf_log" >&2
    die "port-forward to $SEARCH_NAMESPACE/$SEARCH_SERVICE failed"
  fi
  search_args=(--search-api-url="https://127.0.0.1:$search_port/searchapi/graphql"
    --search-insecure-skip-verify)
  say "ACM Search $SEARCH_NAMESPACE/$SEARCH_SERVICE on 127.0.0.1:$search_port"
else
  say "no Service $SEARCH_NAMESPACE/$SEARCH_SERVICE: --search-api-url=off, no cluster GPUs"
fi

say "gRPC/Connect $LISTEN_ADDRESS, REST http://$HTTP_LISTEN_ADDRESS (Swagger UI /docs/)"
if [[ "$KEYCLOAK" == hub ]]; then
  say "This acts on $server as $user: adding or removing clusters and organization changes are real."
else
  say "This acts on $server as $user: adding or removing clusters is real."
fi
say "Anything on this machine that can reach $LISTEN_ADDRESS acts as $user too, a web page through DNS rebinding included: stop it when you're done."

backend_args=(--listen-address="$LISTEN_ADDRESS"
  --http-listen-address="$HTTP_LISTEN_ADDRESS"
  --kubeconfig="$KUBECONFIG_PATH" "${search_args[@]}")
# RUN_ARGS as make run takes them: words, split on spaces.
read -r -a run_args <<<"$RUN_ARGS"
backend_args+=(${run_args[@]+"${run_args[@]}"})
if [[ "$KEYCLOAK" == hub ]]; then
  # Read first, where set -e stops on a failed read: inside the command's own
  # assignments a failed read would leave the value empty, and the backend
  # would fall back to its defaults (admin/admin) without saying so.
  kc_url="$(keycloak_value url)"
  kc_admin_realm="$(keycloak_value admin-realm)"
  kc_realm="$(keycloak_value realm)"
  kc_username="$(keycloak_value username)"
  kc_password="$(keycloak_value password)"
  KEYCLOAK_URL="$kc_url" \
  KEYCLOAK_ADMIN_REALM="$kc_admin_realm" \
  KEYCLOAK_REALM="$kc_realm" \
  KEYCLOAK_ADMIN_USERNAME="$kc_username" \
  KEYCLOAK_ADMIN_PASSWORD="$kc_password" \
    "$BACKEND_BIN" "${backend_args[@]}" &
  unset kc_url kc_admin_realm kc_realm kc_username kc_password
else
  KEYCLOAK_URL="$KEYCLOAK_URL" "$BACKEND_BIN" "${backend_args[@]}" &
fi
backend_pid=$!
say "Press Ctrl-C to stop."

# Stop everything as soon as either process exits.
while kill -0 "$backend_pid" 2>/dev/null &&
  { [[ -z "$pf_pid" ]] || kill -0 "$pf_pid" 2>/dev/null; }; do
  sleep 0.5
done
say "a process exited; stopping" >&2
exit 1
