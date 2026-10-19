# ACM Hub (Helm on OpenShift)

Installs **Red Hat Advanced Cluster Management** on a logged-in OpenShift
cluster via OLM (`Subscription`) plus a `MultiClusterHub`, then seeds
playground `ManagedCluster` stubs you can query and label-patch.

RHACM is not a Red Hat Helm product; this chart wraps the supported OLM
install so you can `helm upgrade` / tear down consistently.

## Prerequisites

- OpenShift cluster with OperatorHub / `redhat-operators` CatalogSource
- `oc` logged in (`oc whoami`)
- `helm` on `PATH`
- Enough CPU/RAM/storage for ACM (full hub is heavy; allow 15–20+ minutes)

### Channel vs OpenShift version

Pick an ACM channel that matches the hub OCP version (`oc get clusterversion`):

| Hub OCP | Suggested ACM channel | Notes |
|---------|----------------------|--------|
| 4.18.x | `release-2.13` (chart default) | ACM 2.16 / MCE 2.11 need OCP ≥ 4.19 |
| ≥ 4.19 | `release-2.16` (catalog default) | `CHANNEL=release-2.16 make acm-hub-up` |

Using ACM 2.16 on OCP 4.18 produces console warnings and MCE stuck at
`OCP … below the minimum supported version 4.19.0`.

## Install

From the `ai-grid-provider-backend` repository root:

```bash
make acm-hub-up
```

Or:

```bash
bash deploy/acm-hub/scripts/acm-hub-up.sh
```

Optional knobs:

| Variable / flag | Meaning |
|-----------------|---------|
| `CHANNEL=release-2.13` | Override Subscription channel (default `release-2.16`) |
| `bash …/acm-hub-down.sh --force-clean` | Unstick a Terminating `open-cluster-management` namespace |

| `--skip-samples` / `SKIP_SAMPLES=1` | Hub only; no playground clusters |
| `CSV_TIMEOUT` / `CRD_TIMEOUT` / `MCH_TIMEOUT` | Wait budgets (`600s`, `1200s`, …) |

List channels available on your cluster:

```bash
oc get packagemanifest advanced-cluster-management -n openshift-marketplace \
  -o jsonpath='{.status.channels[*].name}{"\n"}'
```

## What you get

- Namespace `open-cluster-management` (created or reused by the script; not
  owned by the Helm release, so a pre-existing NS does not block install)
- ACM operator Subscription + MultiClusterHub (phase=Running)
- `ManagedClusterSet` `ai-grid`
- Sample ManagedClusters:
  - `playground-east` — `ai-grid/enabled=true`
  - `playground-west` — `ai-grid/enabled=false` (flip this to experiment)

These samples are **hub-side stubs** (`hubAcceptsClient: true`) for API and
label experiments. They are not imported spoke agents.

## Mess with it

```bash
oc get managedcluster
oc get managedcluster -o wide
oc get managedcluster playground-east -o yaml
oc label managedcluster playground-west ai-grid/enabled=true --overwrite
oc get managedclusterset
oc get multiclusterhub -n open-cluster-management
```

## Point ai-grid-provider-backend at this hub

Skip the backend’s own sample CRs so you do not duplicate names:

```bash
APPLY_SAMPLES=0 make openshift-up
```

## Tear down

```bash
make acm-hub-down
```

Namespace deletion can take several minutes while OLM finalizers clear.

## Chart layout

```text
deploy/acm-hub/
  Chart.yaml
  values.yaml
  templates/          # OperatorGroup, Subscription, MCH, ClusterSet, samples
  scripts/
    acm-hub-up.sh     # three-phase: OLM → MCH → samples (adopts existing objects)
    acm-hub-down.sh
```
