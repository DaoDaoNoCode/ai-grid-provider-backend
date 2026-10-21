#!/usr/bin/env bash
# Runs the delete logic of the PR-image workflows (cleanup-pr-image, publish-pr-image,
# prune-images) against stubbed gh, skopeo and date, and checks what they delete. The scripts
# are read from the workflow files with yq, so this tests what CI runs.
#
# Usage: hack/test-image-workflows.sh   (needs bash, jq and mikefarah yq v4)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
wf="$root/.github/workflows"
ext=yaml
[ -f "$wf/cleanup-pr-image.$ext" ] || ext=yml
image=$(yq '.env.ALLOWED_IMAGES' "$wf/publish-pr-image.$ext")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"

# gh api <versions path> --paginate [--jq expr] prints $FIX/versions.json, filtered by --jq;
# gh api --method DELETE <path> records the version id.
cat > "$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
if [ "$2" = "--method" ] && [ "$3" = "DELETE" ]; then
  echo "${4##*/}" >> "$FIX/deleted"
  exit 0
fi
filter=.
while [ $# -gt 0 ]; do
  [ "$1" = "--jq" ] && filter=$2
  shift
done
jq -r "$filter" "$FIX/versions.json"
STUB
# skopeo: login reads the token; inspect --raw prints $FIX/raw/<digest> (or {}), and fails when
# $FIX/fail-inspect exists; inspect --format prints the pr tag's digest, old before copy and new
# after; copy records that it ran.
cat > "$work/bin/skopeo" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  login) cat > /dev/null ;;
  copy) touch "$FIX/copied" ;;
  inspect)
    if [ "$2" = "--raw" ]; then
      [ -e "$FIX/fail-inspect" ] && exit 1
      digest=${3##*@}
      if [ -f "$FIX/raw/$digest" ]; then cat "$FIX/raw/$digest"; else echo '{}'; fi
    elif [ -e "$FIX/copied" ]; then
      cat "$FIX/new_digest"
    else
      cat "$FIX/old_digest" 2>/dev/null || exit 1
    fi
    ;;
esac
STUB
# date -d "N days ago" +%s: a fixed cutoff, so every fixture version counts as old.
cat > "$work/bin/date" <<'STUB'
#!/usr/bin/env bash
echo 4102444800
STUB
chmod +x "$work/bin/"*

run_block() { # $1 workflow, $2 yq path to the step's run script
  yq "$2" "$wf/$1.$ext" | sed "s#/tmp/pr-images#$FIX/pr-images#g" > "$FIX/script.sh"
  PATH="$work/bin:$PATH" OWNER=org ACTOR=ci GH_TOKEN=x IMAGES="$image" ALLOWED_IMAGES="$image" \
    PR_NUMBER=7 RUN_ID=1 MAX_AGE_DAYS=30 DRY_RUN='' bash -e "$FIX/script.sh" > "$FIX/out" 2>&1
}

new_fixture() {
  FIX="$work/$1"
  export FIX
  mkdir -p "$FIX/raw"
  : > "$FIX/deleted"
}

# versions.json entries: version(id, digest, tags...)
version() {
  local id=$1 name=$2; shift 2
  jq -cn --argjson id "$id" --arg name "$name" --args \
    '{id: $id, name: $name, updated_at: "2020-01-01T00:00:00Z", metadata: {container: {tags: $ARGS.positional}}}' "$@"
}
versions() { jq -s '.' > "$FIX/versions.json"; }
# A tagged index at $1 whose children are $2.. ; with "many" as $2, sha256:1 to sha256:20000.
index() {
  local digest=$1; shift
  if [ "$1" = many ]; then
    seq 1 20000 | sed 's/^/sha256:/' | jq -R . | jq -s '{manifests: map({digest: .})}' > "$FIX/raw/$digest"
  else
    printf '%s\n' "$@" | jq -R . | jq -s '{manifests: map({digest: .})}' > "$FIX/raw/$digest"
  fi
}

fails=0
expect() { # $1 description, $2 expected deleted ids (space separated), $3 expected exit status
  local got status_ok=1
  got=$(sort "$FIX/deleted" | tr '\n' ' ' | sed 's/ $//')
  [ "${3:-0}" = "$status" ] || status_ok=0
  if [ "$got" = "$2" ] && [ "$status_ok" = 1 ]; then
    echo "ok   $1"
  else
    echo "FAIL $1: deleted [$got], want [$2]; exit $status, want ${3:-0}"
    sed 's/^/     | /' "$FIX/out"
    fails=$((fails + 1))
  fi
}

cleanup='.jobs.delete.steps[-1].run'
publish='.jobs.push.steps[] | select(.name == "Push to ghcr.io") | .run'
prune='.jobs.prune.steps[-1].run'

new_fixture cleanup-basic
index sha256:idx sha256:child
{ version 1 sha256:idx latest sha-a; version 2 sha256:child pr-7; version 3 sha256:own pr-7
  version 4 sha256:shared pr-7 sha-b; version 5 sha256:other pr-8; } | versions
status=0; run_block cleanup-pr-image "$cleanup" || status=$?
expect "cleanup deletes only an unprotected pr-7-only version" "3"

new_fixture cleanup-many
index sha256:idx many
{ version 1 sha256:idx latest; version 2 sha256:1 pr-7; version 3 sha256:own pr-7; } | versions
status=0; run_block cleanup-pr-image "$cleanup" || status=$?
expect "cleanup keeps a child among 20000 protected digests" "3"

new_fixture cleanup-inspect-fails
index sha256:idx sha256:child
{ version 1 sha256:idx latest; version 3 sha256:own pr-7; } | versions
touch "$FIX/fail-inspect"
status=0; run_block cleanup-pr-image "$cleanup" || status=$?
expect "cleanup deletes nothing when an index can't be read" "" 1

new_fixture publish-old-is-child
mkdir -p "$FIX/pr-images/pr-image-$image"
index sha256:idx sha256:child
{ version 1 sha256:idx latest; version 2 sha256:child; version 3 sha256:new pr-7; } | versions
echo sha256:child > "$FIX/old_digest"; echo sha256:new > "$FIX/new_digest"
status=0; run_block publish-pr-image "$publish" || status=$?
expect "publish keeps a superseded digest that a tagged index lists" ""

new_fixture publish-old-orphan
mkdir -p "$FIX/pr-images/pr-image-$image"
index sha256:idx sha256:child
{ version 1 sha256:idx latest; version 2 sha256:old; version 3 sha256:new pr-7; } | versions
echo sha256:old > "$FIX/old_digest"; echo sha256:new > "$FIX/new_digest"
status=0; run_block publish-pr-image "$publish" || status=$?
expect "publish deletes the untagged digest pr-7 moved off" "2"

new_fixture prune-many
index sha256:idx many
{ version 1 sha256:idx latest sha-a; version 2 sha256:1; version 3 sha256:orphan
  version 4 sha256:oldpr pr-3; version 5 sha256:rel sha-b 1.2.3; } | versions
status=0; run_block prune-images "$prune" || status=$?
expect "prune keeps a child among 20000 protected digests, deletes orphans and pr-only" "3 4"

new_fixture prune-inspect-fails
index sha256:idx sha256:child
{ version 1 sha256:idx latest; version 3 sha256:orphan; } | versions
touch "$FIX/fail-inspect"
status=0; run_block prune-images "$prune" || status=$?
expect "prune deletes nothing when an index can't be read" "" 1

[ "$fails" = 0 ] || { echo "$fails failed"; exit 1; }
echo "all passed"
