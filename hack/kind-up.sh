#!/usr/bin/env bash
set -euo pipefail

cluster_name="${KIND_CLUSTER_NAME:-ai-grid-hub}"
kube_context="kind-${cluster_name}"
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"

if ! kind get clusters | grep -qx "$cluster_name"; then
  kind create cluster --name "$cluster_name" --wait 5m
fi

kubectl config use-context "$kube_context"

podman build -t localhost/ai-grid-provider-backend:dev -f Containerfile .
# kind load docker-image often fails to see Podman-local tags; archive load is reliable.
# macOS mktemp requires XXXXXX at the end of the template.
archive="$(mktemp "${TMPDIR:-/tmp}/ai-grid-provider-backend-dev.XXXXXX").tar"
trap 'rm -f "$archive"' EXIT
podman save -o "$archive" localhost/ai-grid-provider-backend:dev
kind load image-archive "$archive" --name "$cluster_name"

kubectl apply -f deploy/kind/ai-grid-provider-backend.yaml
kubectl apply -f deploy/kind/keycloak.yaml
kubectl -n ai-grid rollout restart deployment/ai-grid-provider-backend

echo "Kind cluster is ready. Install OCM with: clusteradm init --wait"
echo "Then apply sample resources with: kubectl apply -f deploy/kind/sample-managedclusters.yaml"
echo "Expose the backend with: kubectl -n ai-grid port-forward service/ai-grid-provider-backend 8080:8080"
