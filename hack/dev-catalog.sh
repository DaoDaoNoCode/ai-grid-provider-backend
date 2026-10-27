#!/usr/bin/env bash
# Manages the local catalog repository that `make dev-fake` and `make run`
# commit Models and Subscriptions to by default: a bare Git repository,
# bin/dev-catalog.git unless DEV_CATALOG says otherwise.
#
#   hack/dev-catalog.sh init   create it, with one seed commit, if it is missing
#   hack/dev-catalog.sh reset  delete it and create it again
#   hack/dev-catalog.sh log    show its commits and the files at the branch head
set -euo pipefail

DEV_CATALOG="${DEV_CATALOG:-bin/dev-catalog.git}"
BRANCH="${AI_GRID_GIT_BRANCH:-main}"

init() {
  if [[ -e "$DEV_CATALOG" ]]; then
    return
  fi
  mkdir -p "$(dirname "$DEV_CATALOG")"
  git init -q --bare -b "$BRANCH" "$DEV_CATALOG"
  seed="$(mktemp -d)"
  trap 'rm -rf "$seed"' RETURN
  git -C "$seed" init -q -b "$BRANCH"
  mkdir -p "$seed/catalog/subscriptions"
  # Git drops an empty directory: without this file, deleting the last
  # subscription would remove catalog/subscriptions, and Argo CD fails on a
  # missing path instead of pruning.
  echo "Subscriptions the backend commits, one file each. Keep this file." > "$seed/catalog/subscriptions/README"
  git -C "$seed" add .
  git -C "$seed" -c user.name=ai-grid-provider-backend -c user.email=ai-grid-provider-backend@noreply.invalid \
    -c core.hooksPath=/dev/null commit -q -m "catalog: seed"
  git -C "$seed" push -q "$PWD/$DEV_CATALOG" "$BRANCH"
  echo "dev-catalog: created $DEV_CATALOG"
}

case "${1:-}" in
  init)
    init
    ;;
  reset)
    # Only ever remove a bare repository at the configured path.
    if [[ -e "$DEV_CATALOG" ]]; then
      if [[ "$(git -C "$DEV_CATALOG" rev-parse --is-bare-repository 2>/dev/null)" != "true" ]]; then
        echo "dev-catalog: $DEV_CATALOG is not a bare Git repository; refusing to remove it" >&2
        exit 1
      fi
      rm -rf "$DEV_CATALOG"
    fi
    init
    ;;
  log)
    if [[ ! -e "$DEV_CATALOG" ]]; then
      echo "dev-catalog: $DEV_CATALOG does not exist; run make dev-fake or make dev-catalog-reset" >&2
      exit 1
    fi
    git -C "$DEV_CATALOG" log --format='%h %ad %s' --date=format:'%Y-%m-%d %H:%M:%S' "$BRANCH"
    echo
    git -C "$DEV_CATALOG" ls-tree -r --name-only "$BRANCH"
    ;;
  *)
    echo "usage: $0 init|reset|log" >&2
    exit 2
    ;;
esac
