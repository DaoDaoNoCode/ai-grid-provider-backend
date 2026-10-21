#!/usr/bin/env bash
# Three-phase RHACM install on the current OpenShift cluster.
#
# Phase 1: Namespace + OperatorGroup + Subscription (OLM).
# Phase 2: MultiClusterHub (after operator CRDs exist).
# Phase 3: ManagedClusterSet + sample ManagedClusters (after MCH Running).
#
# Usage:
#   bash deploy/acm-hub/scripts/acm-hub-up.sh
#   CHANNEL=release-2.13 bash deploy/acm-hub/scripts/acm-hub-up.sh
#   RELEASE_NAME=acm-hub NAMESPACE=open-cluster-management bash ...
#
# Environment:
#   RELEASE_NAME          Helm release name (default acm-hub)
#   NAMESPACE             ACM operator namespace (default open-cluster-management)
#   CHANNEL               Subscription channel (default from values.yaml / release-2.12)
#   HELM_EXTRA_ARGS       Extra args appended to helm upgrade (quoted string)
#   CSV_TIMEOUT           Wait for CSV Succeeded (default 600s)
#   CRD_TIMEOUT           Wait for MultiClusterHub CRD (default 600s)
#   MCH_TIMEOUT           Wait for MultiClusterHub Running (default 1200s)
#   SKIP_SAMPLES          1 to skip sample ManagedClusters / ClusterSet
#
# Needs: oc logged in, helm on PATH. Idempotent.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
CHART="${ROOT}/deploy/acm-hub"
RELEASE_NAME="${RELEASE_NAME:-acm-hub}"
NAMESPACE="${NAMESPACE:-open-cluster-management}"
CSV_TIMEOUT="${CSV_TIMEOUT:-600s}"
CRD_TIMEOUT="${CRD_TIMEOUT:-600s}"
MCH_TIMEOUT="${MCH_TIMEOUT:-1200s}"
SKIP_SAMPLES="${SKIP_SAMPLES:-0}"
CHANNEL="${CHANNEL:-}"
HELM_EXTRA_ARGS="${HELM_EXTRA_ARGS:-}"

for arg in "$@"; do
  case "$arg" in
    --skip-samples) SKIP_SAMPLES=1 ;;
    -h|--help)
      sed -n '2,30p' "$0"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required and must be on PATH"
}

need_cmd oc
need_cmd helm

oc whoami >/dev/null 2>&1 || die "oc is not logged in; run oc login first"

if ! oc get catalogsource redhat-operators -n openshift-marketplace >/dev/null 2>&1; then
  die "CatalogSource redhat-operators not found in openshift-marketplace"
fi

# Namespace is created outside the Helm release so a pre-existing
# open-cluster-management NS (common after a partial ACM install) does not
# fail with Helm ownership / managed-by errors.
ensure_namespace() {
  local phase=""
  if oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    phase="$(oc get namespace "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "$phase" == "Terminating" ]]; then
      echo "==> namespace ${NAMESPACE} is Terminating; waiting for it to finish (up to 10m)"
      echo "    If this hangs, run: bash deploy/acm-hub/scripts/acm-hub-down.sh --force-clean"
      local deadline=$((SECONDS + 600))
      while (( SECONDS < deadline )); do
        if ! oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
          echo "    namespace deleted"
          break
        fi
        phase="$(oc get namespace "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        [[ "$phase" != "Terminating" ]] && break
        sleep 5
      done
      if oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
        phase="$(oc get namespace "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        if [[ "$phase" == "Terminating" ]]; then
          die "namespace ${NAMESPACE} still Terminating; run: bash deploy/acm-hub/scripts/acm-hub-down.sh --force-clean"
        fi
      fi
    else
      echo "==> namespace ${NAMESPACE} already exists"
    fi
  fi
  if ! oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    echo "==> creating namespace ${NAMESPACE}"
    oc create namespace "${NAMESPACE}"
  fi
  oc label namespace "${NAMESPACE}" openshift.io/cluster-monitoring=true --overwrite >/dev/null
}

# Adopt objects that already exist so helm upgrade --install can manage them.
# Use fully-qualified resource names so oc resolves the OLM API correctly.
adopt() {
  local resource="$1" name="$2" ns="${3:-}"
  if [[ -n "$ns" ]]; then
    if ! oc get "${resource}" "${name}" -n "${ns}" >/dev/null 2>&1; then
      return 0
    fi
    echo "    adopting ${resource}/${name} in ${ns}"
    oc annotate -n "${ns}" "${resource}" "${name}" \
      "meta.helm.sh/release-name=${RELEASE_NAME}" \
      "meta.helm.sh/release-namespace=${NAMESPACE}" \
      --overwrite
    oc label -n "${ns}" "${resource}" "${name}" \
      app.kubernetes.io/managed-by=Helm \
      --overwrite
  else
    if ! oc get "${resource}" "${name}" >/dev/null 2>&1; then
      return 0
    fi
    echo "    adopting ${resource}/${name}"
    oc annotate "${resource}" "${name}" \
      "meta.helm.sh/release-name=${RELEASE_NAME}" \
      "meta.helm.sh/release-namespace=${NAMESPACE}" \
      --overwrite
    oc label "${resource}" "${name}" \
      app.kubernetes.io/managed-by=Helm \
      --overwrite
  fi
}

# Helm 3.14+ can take ownership of existing resources in one shot.
helm_take_ownership_args=()
if helm upgrade --help 2>/dev/null | grep -q -- '--take-ownership'; then
  helm_take_ownership_args=(--take-ownership)
fi

ensure_namespace

echo "==> phase 1: install OLM Subscription (operator only)"
helm_set_channel=()
if [[ -n "$CHANNEL" ]]; then
  helm_set_channel=(--set "subscription.channel=${CHANNEL}")
fi

adopt operatorgroups.operators.coreos.com open-cluster-management-group "${NAMESPACE}"
adopt subscriptions.operators.coreos.com advanced-cluster-management "${NAMESPACE}"

# shellcheck disable=SC2086
helm upgrade --install "${RELEASE_NAME}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  --create-namespace \
  "${helm_take_ownership_args[@]}" \
  --set "namespace=${NAMESPACE}" \
  --set multiclusterhub.create=false \
  --set clusterSet.create=false \
  --set samples.create=false \
  "${helm_set_channel[@]}" \
  ${HELM_EXTRA_ARGS}

timeout_to_seconds() {
  local t="$1"
  case "$t" in
    *s) echo "${t%s}" ;;
    *m) echo $(( ${t%m} * 60 )) ;;
    *) echo "$t" ;;
  esac
}

# Prefer the fully-qualified OLM resource. Bare "subscription" resolves to
# apps.open-cluster-management.io when ACM Application Subscription CRDs are
# present (short-name collision), which has no installedCSV.
sub_name="advanced-cluster-management"
sub_resource="subscriptions.operators.coreos.com"
echo "==> waiting for OLM Subscription ${sub_name} in ${NAMESPACE}"
oc wait --for=jsonpath='{.status.state}'=AtLatestKnown \
  "${sub_resource}/${sub_name}" -n "${NAMESPACE}" --timeout="${CSV_TIMEOUT}" \
  || echo "warning: Subscription not yet AtLatestKnown; continuing to CSV wait"

echo "==> waiting for CSV Succeeded (timeout ${CSV_TIMEOUT})"
csv_deadline=$((SECONDS + $(timeout_to_seconds "${CSV_TIMEOUT}")))
csv_name=""
while (( SECONDS < csv_deadline )); do
  csv_name="$(oc get "${sub_resource}" "${sub_name}" -n "${NAMESPACE}" \
    -o jsonpath='{.status.installedCSV}' 2>/dev/null || true)"
  if [[ -n "$csv_name" ]]; then
    phase="$(oc get csv "${csv_name}" -n "${NAMESPACE}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "$phase" == "Succeeded" ]]; then
      echo "    csv=${csv_name} phase=Succeeded"
      break
    fi
    echo "    csv=${csv_name} phase=${phase:-pending}"
  else
    echo "    waiting for installedCSV..."
  fi
  sleep 10
done
[[ -n "$csv_name" ]] || die "Subscription never reported installedCSV within ${CSV_TIMEOUT}"
phase="$(oc get csv "${csv_name}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
[[ "$phase" == "Succeeded" ]] || die "CSV ${csv_name} phase=${phase} (want Succeeded)"

echo "==> waiting for MultiClusterHub CRD (timeout ${CRD_TIMEOUT})"
oc wait --for=condition=Established \
  crd/multiclusterhubs.operator.open-cluster-management.io \
  --timeout="${CRD_TIMEOUT}"

# ACM 2.13 CRDs do not define spec.localClusterName (added in later releases).
# Keep MulticlusterHub as spec: {} unless values explicitly set the field.
echo "==> phase 2: MultiClusterHub (samples wait until CRDs exist)"
adopt multiclusterhubs.operator.open-cluster-management.io multiclusterhub "${NAMESPACE}"
# shellcheck disable=SC2086
helm upgrade --install "${RELEASE_NAME}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  "${helm_take_ownership_args[@]}" \
  --set "namespace=${NAMESPACE}" \
  --set multiclusterhub.create=true \
  --set clusterSet.create=false \
  --set samples.create=false \
  "${helm_set_channel[@]}" \
  ${HELM_EXTRA_ARGS}

mch_name="$(oc get multiclusterhub -n "${NAMESPACE}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
mch_name="${mch_name:-multiclusterhub}"

echo "==> waiting for MultiClusterHub ${mch_name} Running (timeout ${MCH_TIMEOUT})"
mch_deadline=$((SECONDS + $(timeout_to_seconds "${MCH_TIMEOUT}")))
while (( SECONDS < mch_deadline )); do
  mch_phase="$(oc get multiclusterhub "${mch_name}" -n "${NAMESPACE}" \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [[ "$mch_phase" == "Running" ]]; then
    echo "    MultiClusterHub phase=Running"
    break
  fi
  echo "    MultiClusterHub phase=${mch_phase:-pending}"
  sleep 20
done
mch_phase="$(oc get multiclusterhub "${mch_name}" -n "${NAMESPACE}" \
  -o jsonpath='{.status.phase}' 2>/dev/null || true)"
[[ "$mch_phase" == "Running" ]] || die "MultiClusterHub ${mch_name} phase=${mch_phase} (want Running) after ${MCH_TIMEOUT}"

if [[ "$SKIP_SAMPLES" != "1" ]]; then
  echo "==> waiting for ManagedCluster / ManagedClusterSet CRDs"
  oc wait --for=condition=Established \
    crd/managedclusters.cluster.open-cluster-management.io \
    --timeout=300s
  oc wait --for=condition=Established \
    crd/managedclustersets.cluster.open-cluster-management.io \
    --timeout=300s

  echo "==> phase 3: ManagedClusterSet + playground ManagedClusters"
  adopt managedclustersets.cluster.open-cluster-management.io ai-grid
  adopt managedclusters.cluster.open-cluster-management.io playground-east
  adopt managedclusters.cluster.open-cluster-management.io playground-west
  # shellcheck disable=SC2086
  helm upgrade --install "${RELEASE_NAME}" "${CHART}" \
    --namespace "${NAMESPACE}" \
    "${helm_take_ownership_args[@]}" \
    --set "namespace=${NAMESPACE}" \
    --set multiclusterhub.create=true \
    --set clusterSet.create=true \
    --set samples.create=true \
    "${helm_set_channel[@]}" \
    ${HELM_EXTRA_ARGS}
fi

echo
echo "==> ACM hub is ready. Smoke queries:"
echo "    oc get multiclusterhub -n ${NAMESPACE}"
echo "    oc get managedcluster"
echo "    oc get managedcluster -o wide"
echo "    oc get managedclusterset"
echo "    oc label managedcluster playground-west ai-grid/enabled=true --overwrite"
echo
echo "Deploy ai-grid-provider-backend without duplicate samples:"
echo "    APPLY_SAMPLES=0 make openshift-up"
echo

oc get multiclusterhub -n "${NAMESPACE}" 2>/dev/null || true
oc get managedclusterset 2>/dev/null || true
oc get managedcluster -o wide 2>/dev/null || true
