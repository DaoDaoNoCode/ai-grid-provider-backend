#!/usr/bin/env bash
# Tear down the acm-hub Helm release and RHACM operator install.
#
# Order matters: MultiClusterHub first (while its webhook still works), then
# Helm/OLM objects, then namespace. Use --force-clean when the namespace is
# stuck Terminating (dead webhook / leftover Search finalizers).
#
# Usage:
#   bash deploy/acm-hub/scripts/acm-hub-down.sh
#   bash deploy/acm-hub/scripts/acm-hub-down.sh --force-clean
#   DELETE_NAMESPACE=0 bash deploy/acm-hub/scripts/acm-hub-down.sh
#
# Environment:
#   RELEASE_NAME        Helm release name (default acm-hub)
#   NAMESPACE           ACM operator namespace (default open-cluster-management)
#   DELETE_NAMESPACE    1 (default) delete the namespace after uninstall
#   FORCE_CLEAN         1 strip finalizers / remove stale ACM webhooks so a
#                       Terminating namespace can finish
#
# Needs: oc logged in, helm on PATH.
set -euo pipefail

RELEASE_NAME="${RELEASE_NAME:-acm-hub}"
NAMESPACE="${NAMESPACE:-open-cluster-management}"
DELETE_NAMESPACE="${DELETE_NAMESPACE:-1}"
FORCE_CLEAN="${FORCE_CLEAN:-0}"

for arg in "$@"; do
  case "$arg" in
    --keep-namespace) DELETE_NAMESPACE=0 ;;
    --force-clean) FORCE_CLEAN=1 ;;
    -h|--help)
      sed -n '2,28p' "$0"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }

command -v oc >/dev/null 2>&1 || die "oc is required and must be on PATH"
command -v helm >/dev/null 2>&1 || die "helm is required and must be on PATH"
oc whoami >/dev/null 2>&1 || die "oc is not logged in; run oc login first"

force_clean_terminating() {
  echo "==> force-clean: removing ACM webhooks that target ${NAMESPACE}"
  # Cluster-scoped webhooks whose clientConfig.service.namespace is our NS
  # block deletion once the webhook Service is gone.
  local wh
  for wh in $(oc get validatingwebhookconfiguration -o name 2>/dev/null || true); do
    if oc get "$wh" -o jsonpath='{range .webhooks[*]}{.clientConfig.service.namespace}{"\n"}{end}' 2>/dev/null \
      | grep -qx "${NAMESPACE}"; then
      echo "    deleting ${wh}"
      oc delete "$wh" --ignore-not-found=true || true
    fi
  done
  for wh in $(oc get mutatingwebhookconfiguration -o name 2>/dev/null || true); do
    if oc get "$wh" -o jsonpath='{range .webhooks[*]}{.clientConfig.service.namespace}{"\n"}{end}' 2>/dev/null \
      | grep -qx "${NAMESPACE}"; then
      echo "    deleting ${wh}"
      oc delete "$wh" --ignore-not-found=true || true
    fi
  done

  if ! oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    echo "==> namespace ${NAMESPACE} already gone"
    return 0
  fi

  echo "==> force-clean: stripping finalizers on leftovers in ${NAMESPACE}"
  local kind name
  for kind in searches.search.open-cluster-management.io multiclusterhubs.operator.open-cluster-management.io; do
    for name in $(oc get "${kind}" -n "${NAMESPACE}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true); do
      [[ -z "$name" ]] && continue
      echo "    clearing finalizers on ${kind}/${name}"
      oc patch "${kind}" "${name}" -n "${NAMESPACE}" --type=merge \
        -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
      oc delete "${kind}" "${name}" -n "${NAMESPACE}" --wait=false --ignore-not-found=true 2>/dev/null || true
    done
  done

  echo "==> force-clean: force-deleting stuck pods"
  oc delete pods --all -n "${NAMESPACE}" --force --grace-period=0 --ignore-not-found=true 2>/dev/null || true
}

echo "==> removing playground ManagedClusters (if any)"
for name in playground-east playground-west; do
  oc delete managedcluster "${name}" --ignore-not-found=true --wait=false 2>/dev/null || true
done

echo "==> removing ManagedClusterSet ai-grid (if any)"
oc delete managedclusterset ai-grid --ignore-not-found=true --wait=false 2>/dev/null || true

# Delete MCH while its webhook Service still exists when possible.
if oc get multiclusterhub -n "${NAMESPACE}" >/dev/null 2>&1; then
  echo "==> deleting MultiClusterHub in ${NAMESPACE} (wait up to 10m)"
  if ! oc delete multiclusterhub --all -n "${NAMESPACE}" --wait=true --timeout=600s; then
    echo "warning: MultiClusterHub delete failed; enabling force-clean"
    FORCE_CLEAN=1
  fi
fi

if [[ "$FORCE_CLEAN" == "1" ]]; then
  force_clean_terminating
fi

if helm status "${RELEASE_NAME}" -n "${NAMESPACE}" >/dev/null 2>&1; then
  echo "==> helm uninstall ${RELEASE_NAME} -n ${NAMESPACE}"
  helm uninstall "${RELEASE_NAME}" -n "${NAMESPACE}" || true
else
  echo "==> no Helm release ${RELEASE_NAME} in ${NAMESPACE}"
fi

# OLM leftovers if helm did not own them fully.
oc delete subscription advanced-cluster-management -n "${NAMESPACE}" --ignore-not-found=true --wait=false 2>/dev/null || true
oc delete csv -l operators.coreos.com/advanced-cluster-management."${NAMESPACE}" -n "${NAMESPACE}" --ignore-not-found=true --wait=false 2>/dev/null || true
oc delete csv --all -n "${NAMESPACE}" --ignore-not-found=true --wait=false 2>/dev/null || true
oc delete operatorgroup open-cluster-management-group -n "${NAMESPACE}" --ignore-not-found=true --wait=false 2>/dev/null || true

if [[ "$DELETE_NAMESPACE" == "1" ]]; then
  if oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    echo "==> deleting namespace ${NAMESPACE} (may take several minutes)"
    oc delete namespace "${NAMESPACE}" --wait=false || true
    if [[ "$FORCE_CLEAN" == "1" ]]; then
      force_clean_terminating
    fi
    echo "==> waiting for namespace ${NAMESPACE} to disappear (up to 10m)"
    local_deadline=$((SECONDS + 600))
    while (( SECONDS < local_deadline )); do
      if ! oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
        echo "    namespace deleted"
        break
      fi
      sleep 5
    done
    if oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
      echo "warning: namespace ${NAMESPACE} still present (phase=$(oc get ns "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo unknown))"
      echo "         re-run: bash deploy/acm-hub/scripts/acm-hub-down.sh --force-clean"
    fi
  fi
fi

echo "==> acm-hub teardown done. Check:"
echo "    oc get ns ${NAMESPACE}"
echo "    oc get managedcluster"
echo "    helm list -A | grep ${RELEASE_NAME} || true"
