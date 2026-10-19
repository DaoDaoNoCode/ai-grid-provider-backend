# AI Grid backend

`ai-grid-provider-backend` is a Go gRPC service that manages Open Cluster
Management (OCM)/RHACM `ManagedCluster` resources via the Kubernetes API
(hub-side CRUD; it does not provision spoke clusters).

The current API includes:

- `ai_grid.v1.AiGrid/Ping`
- `ai_grid.v1.Clusters/List`
- `ai_grid.v1.Clusters/Get`
- `ai_grid.v1.Clusters/Create`
- `ai_grid.v1.Clusters/Update`
- `ai_grid.v1.Clusters/Delete`
- `ai_grid.v1.Tenants/List`

REST (grpc-gateway) and Swagger UI are also exposed on HTTP `:8081`:

- Swagger UI: `http://localhost:8081/docs/`
- OpenAPI (Swagger 2.0): `http://localhost:8081/openapi.json`
- REST routes: `/api/v1/ping`, `/api/v1/clusters`, `/api/v1/tenants`

Regenerate protobuf, gateway stubs, and OpenAPI with `make generate` (requires [buf](https://buf.build)).

ManagedCluster RPCs use the Kubernetes dynamic client against:

```text
cluster.open-cluster-management.io/v1/managedclusters
```

The service runs in-cluster using its Kubernetes ServiceAccount. Its RBAC role
allows `get`, `list`, `watch`, `create`, `update`, `patch`, and `delete` on
`ManagedCluster` resources.

`ListTenants` reads Keycloak Organizations from the `ai-grid` realm through the
Keycloak Admin REST API. The Kind setup creates two sample Organizations,
`tenant-a` and `tenant-b`, and configures the backend with the development
Keycloak admin credentials. Use a dedicated service account with the
`query-organizations` role instead of these credentials outside local testing.

## Local architecture

The local environment is Kubernetes-native:

```text
Kind cluster using Podman
├── OCM Cluster Manager
├── ai-grid-provider-backend
├── Keycloak
└── PostgreSQL for Keycloak
```

The backend and Keycloak are deployed as Kubernetes workloads. Podman is used
by Kind to build and run the cluster nodes; Podman Compose is not required.

## Prerequisites

Install or make available:

- Go 1.26.3
- Podman
- Kind
- kubectl
- clusteradm
- grpcurl

The repository is aligned with fulfillment-service and declares `go 1.26.3`.

Install upstream OCM's CLI with Go 1.26.3 if needed:

```sh
go install open-cluster-management.io/clusteradm/cmd/clusteradm@v1.4.0
```

Ensure `$(go env GOPATH)/bin` is on `PATH`.

Kind supports Podman through:

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman
```

## Run the complete integration test

From the repository root:

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman
bash hack/test-kind.sh
```

The test starts by running the prerequisite check directly. You can run that
check without changing the cluster:

```sh
bash hack/check-prereqs.sh
```

It validates Podman, Kind, kubectl, grpcurl, Go, and clusteradm. `clusteradm`
may be installed with:

```sh
go install open-cluster-management.io/clusteradm/cmd/clusteradm@v1.4.0
```

The scripts also recognize an explicit `CLUSTERADM_BIN` or the Go bin
directory.

The script:

1. Creates or reuses a Kind cluster named `ai-grid-hub`.
2. Builds `ai-grid-provider-backend` with Podman and loads it into Kind.
3. Deploys the backend, Keycloak, and PostgreSQL.
4. Installs the OCM hub control plane with `clusteradm init`.
5. Creates two real `ManagedCluster` custom resources.
6. Creates the `ai-grid` Keycloak realm and two sample Organizations.
7. Port-forwards the backend gRPC Service.
8. Runs typed Go gRPC integration tests and verifies ping, clusters, and tenants.

The sample resources are defined in
[deploy/kind/sample-managedclusters.yaml](deploy/kind/sample-managedclusters.yaml).

## Run the environment manually

Create the cluster, build the image, and deploy the workloads:

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman
bash hack/kind-up.sh
```

Install OCM and apply the sample clusters:

```sh
bash hack/ocm-up.sh
```

Expose the backend locally:

```sh
kubectl -n ai-grid port-forward service/ai-grid-provider-backend 8080:8080 8081:8081
```

Open Swagger UI at [http://localhost:8081/docs/](http://localhost:8081/docs/).

List available gRPC services:

```sh
grpcurl -plaintext localhost:8080 list
```

Call the Clusters API (hub ManagedCluster list / get / create / update / delete):

```sh
grpcurl -plaintext -d '{}' localhost:8080 \
  ai_grid.v1.Clusters/List

grpcurl -plaintext -d '{"id":"playground-east"}' localhost:8080 \
  ai_grid.v1.Clusters/Get

grpcurl -plaintext -d '{"object":{"id":"demo-cluster","metadata":{"name":"demo-cluster","labels":{"environment":"dev"}},"spec":{"hub_accepts_client":true}}}' \
  localhost:8080 ai_grid.v1.Clusters/Create

grpcurl -plaintext -d '{"object":{"id":"demo-cluster","metadata":{"labels":{"ai-grid/enabled":"true"}},"spec":{"hub_accepts_client":true}}}' \
  localhost:8080 ai_grid.v1.Clusters/Update

grpcurl -plaintext -d '{"id":"demo-cluster"}' localhost:8080 \
  ai_grid.v1.Clusters/Delete
```

REST equivalents use `/api/v1/clusters` (GET list, GET/`PATCH`/`DELETE` by id, POST create).
Objects use OSAC shape: `id` + `metadata` + `spec` + `status`.

Call the tenant API:

```sh
grpcurl -plaintext -d '{}' localhost:8080 \
  ai_grid.v1.Tenants/List
```

Call the service ping API:

```sh
grpcurl -plaintext -d '{"name":"Ada"}' localhost:8080 \
  ai_grid.v1.AiGrid/Ping
```

Keycloak is available inside the cluster at `keycloak.ai-grid.svc:8080`.
For local browser access, forward it with:

```sh
kubectl -n ai-grid port-forward service/keycloak 8081:8080
```

The development Keycloak credentials are `admin` / `admin`. The initialization
Job creates the `ai-grid` realm and Organizations `tenant-a` and `tenant-b`.

## Run Go tests

```sh
go test ./...
```

The repository also provides standard developer targets:

```sh
make build            # build bin/ai-grid-provider-backend
make test             # run all Go tests
make lint             # gofmt check and go vet; uses golangci-lint when installed
make verify           # build, test, and lint
make image            # build container image (Podman)
make push             # push container image to Quay
make build-and-push   # image + push
make kind-test        # full Podman/Kind integration test
```

The unit test uses a Kubernetes dynamic fake client. The Kind integration test
uses the real Kubernetes API server, real OCM CRD, real `ManagedCluster`
objects, Keycloak Organizations, and the deployed gRPC service.

## Clean up

Delete the local Kind cluster when finished:

```sh
kind delete cluster --name ai-grid-hub
```

## Install ACM hub on OpenShift (playground)

To get a real RHACM hub plus queryable sample `ManagedCluster`s on a logged-in
OpenShift cluster (`oc`, `helm`, OperatorHub required):

```sh
make acm-hub-up
# CHANNEL=release-2.13 make acm-hub-up   # override default release-2.16
# SKIP_SAMPLES=1 make acm-hub-up
```

See [deploy/acm-hub/README.md](deploy/acm-hub/README.md). Tear down with
`make acm-hub-down`. After the hub is Running, deploy this backend with
`APPLY_SAMPLES=0 make openshift-up` so you do not duplicate sample clusters.

## Deploy to OpenShift

Kind-parity stack for a logged-in OpenShift cluster (`oc whoami`): backend,
optional embedded Keycloak + Postgres, Routes for HTTP and gRPC, and sample
ManagedClusters when the ACM/OCM CRD is present.

Default image: [`quay.io/rh-ee-rjjohnso/ai-grid-provider-backend`](https://quay.io/repository/rh-ee-rjjohnso/ai-grid-provider-backend).

Publish the image first (Quay login required once: `podman login quay.io`):

```sh
make image              # build linux/amd64 → quay.io/rh-ee-rjjohnso/ai-grid-provider-backend:latest
make push               # push that tag
make build-and-push     # image + push
# TAG=dev make build-and-push
# PLATFORM=linux/arm64 make build-and-push   # only if the cluster is arm64
```

Images default to `--platform=linux/amd64` so builds on Apple Silicon run on typical
OpenShift nodes. Rebuild and push after changing the Containerfile, then restart
the deployment (`make openshift-up` or `oc -n ai-grid rollout restart deploy/ai-grid-provider-backend`).

Then deploy:

```sh
# Embedded Keycloak (default, Kind parity)
make openshift-up

# Or build/push and deploy in one step
BUILD_IMAGE=1 make openshift-up

# Use an existing Keycloak instead of deploying one
EXTERNAL_KEYCLOAK=1 \
  KEYCLOAK_URL=https://keycloak.example.com \
  KEYCLOAK_ADMIN_USERNAME=admin \
  KEYCLOAK_ADMIN_PASSWORD=... \
  make openshift-up

# Skip sample ManagedClusters on hubs that already have real ACM clusters
APPLY_SAMPLES=0 make openshift-up
```

If Keycloak or Postgres pods are rejected by SCC, re-run with
`GRANT_ANYUID=1` (or grant `anyuid` to the `default` SA in `ai-grid`).

After deploy, the HTTP Route serves REST and Swagger UI. For plaintext
`grpcurl`, prefer port-forward:

```sh
oc -n ai-grid port-forward svc/ai-grid-provider-backend 8080:8080 8081:8081
```

Tear down with `make openshift-down` (keeps the namespace unless
`DELETE_NAMESPACE=1`).

Manifests live under [deploy/openshift/](deploy/openshift/).

## Repository layout

```text
cmd/ai-grid-provider-backend/             service entrypoint
client/                          reusable gRPC client
proto/                           protobuf + buf config
proto/ai_grid/v1/                per-resource *_type.proto / *_service.proto + metadata
gen/ai_grid/v1/                  generated protobuf, gRPC, and gateway code
docs/openapi/                    generated OpenAPI (Swagger 2.0) JSON
internal/servers/                Per-resource gRPC servers (OSAC-style builders)
internal/servers/clusters_server.go         Kubernetes-backed Clusters API
internal/servers/tenants_server.go Keycloak-backed tenant API
internal/keycloak/               Keycloak Admin API client
internal/restgateway/            HTTP gateway + Swagger UI
deploy/kind/                     Kind Kubernetes manifests
deploy/openshift/                OpenShift manifests (Routes, kustomize)
deploy/acm-hub/                  Helm chart + scripts for RHACM hub playground
hack/kind-up.sh                  build/load/deploy helper
hack/ocm-up.sh                   OCM bootstrap helper
hack/openshift-up.sh             OpenShift deploy helper
hack/openshift-down.sh           OpenShift teardown helper
hack/check-prereqs.sh            local toolchain and runtime preflight
hack/smoke-apis.sh               grpcurl smoke test for every RPC
hack/test-kind.sh                end-to-end integration test
integration/kind_test.go         typed gRPC integration test
Makefile                         build, test, lint, generate, Kind, OpenShift, ACM targets
```


