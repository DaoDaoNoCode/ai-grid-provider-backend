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
- `ai_grid.v1.Tenants/Get`
- `ai_grid.v1.Tenants/Create`
- `ai_grid.v1.Tenants/Update`
- `ai_grid.v1.Tenants/Delete`

REST (grpc-gateway) and Swagger UI are also exposed on HTTP `:8081`:

- Swagger UI: `http://localhost:8081/docs/`
- OpenAPI (Swagger 2.0): `http://localhost:8081/openapi.json`
- REST routes: `/api/v1/ping`, `/api/v1/clusters`, `/api/v1/tenants`
- UI handout (payloads + examples): [`docs/UI_API_REFERENCE.md`](docs/UI_API_REFERENCE.md)

Regenerate protobuf, gateway stubs, and OpenAPI with `make generate` (requires [buf](https://buf.build)).

ManagedCluster RPCs use the Kubernetes dynamic client against:

```text
cluster.open-cluster-management.io/v1/managedclusters
```

The service runs in-cluster using its Kubernetes ServiceAccount. Its RBAC role
allows `get`, `list`, `watch`, `create`, `update`, `patch`, and `delete` on
`ManagedCluster` resources.

Tenants CRUD maps to **Keycloak Organizations** in one shared platform realm
(`KEYCLOAK_REALM`, default `ai-grid`) through the Keycloak Admin REST API.
A Tenant is an Organization — **not** a Keycloak realm. `id` is the
organization UUID; `metadata.name` is the organization name (and alias). The
Kind setup creates two sample Organizations, `tenant-a` and `tenant-b`, and
configures the backend with the development Keycloak admin credentials. Use a
dedicated service account with organization manage/query roles instead of these
credentials outside local testing.

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

API request authn/authz is out of scope for this MVP — gRPC and REST endpoints
are open. Keycloak is used only as the Tenants Organizations Admin API backend.
See [`docs/UI_API_REFERENCE.md`](docs/UI_API_REFERENCE.md).

Call the Clusters API (hub ManagedCluster list / get / create / update / delete):

```sh
grpcurl -plaintext -d '{}' localhost:8080 \
  ai_grid.v1.Clusters/List

grpcurl -plaintext -d '{"id":"playground-east"}' localhost:8080 \
  ai_grid.v1.Clusters/Get

grpcurl -plaintext \
  -d '{"object":{"id":"demo-cluster","metadata":{"name":"demo-cluster","labels":{"environment":"dev"}},"spec":{"hub_accepts_client":true}}}' \
  localhost:8080 ai_grid.v1.Clusters/Create

grpcurl -plaintext \
  -d '{"object":{"id":"demo-cluster","metadata":{"labels":{"ai-grid/enabled":"true"}},"spec":{"hub_accepts_client":true}},"update_mask":{"paths":["metadata.labels"]}}' \
  localhost:8080 ai_grid.v1.Clusters/Update

grpcurl -plaintext -d '{"id":"demo-cluster"}' localhost:8080 \
  ai_grid.v1.Clusters/Delete
```

REST equivalents use `/api/v1/clusters` (GET list, GET/`PATCH`/`DELETE` by id, POST create).
Objects use OSAC shape: `id` + `metadata` + `spec` + `status`.

Call the Tenants API:

```sh
grpcurl -plaintext -d '{}' localhost:8080 \
  ai_grid.v1.Tenants/List

grpcurl -plaintext \
  -d '{"object":{"metadata":{"name":"demo-tenant"},"spec":{"domains":["demo.example.com"]}}}' \
  localhost:8080 ai_grid.v1.Tenants/Create
```

REST equivalents use `/api/v1/tenants` (GET list, GET/`PATCH`/`DELETE` by id, POST create).

Call the service ping API:

```sh
grpcurl -plaintext -d '{"name":"Ada"}' localhost:8080 \
  ai_grid.v1.AiGrid/Ping
```

Or run the full gRPC + REST smoke suite:

```sh
# Local: KEYCLOAK_URL=http://127.0.0.1:8180 make run   # then in another shell:
make smoke

# OpenShift (port-forwards backend):
make smoke-openshift
# SKIP_CLUSTERS=1 make smoke-openshift
```

`make smoke-openshift` expects the backend already deployed (`make openshift-up`)
with an image that exposes `ai_grid.v1.Clusters` and full `ai_grid.v1.Tenants`
CRUD. If `grpcurl list` still shows `ManagedClusters`, rebuild/push and redeploy.

Keycloak is available inside the cluster at `keycloak.ai-grid.svc:8080`.
For local browser access, forward it with:

```sh
kubectl -n ai-grid port-forward service/keycloak 8180:8080
```

The development Keycloak credentials are `admin` / `admin`. The initialization
Job creates the `ai-grid` realm and Organizations `tenant-a` / `tenant-b`.

## Run Go tests

```sh
go test ./...
```

The repository also provides standard developer targets (`make help` lists them):

```sh
make build            # build bin/ai-grid-provider-backend
make unit             # unit tests (excludes Kind integration)
make smoke            # live API smoke (needs make run)
make lint             # gofmt check and go vet; uses golangci-lint when installed
make verify           # build, unit, and lint
make run              # run the API locally against your kubeconfig
make image            # build container image (Podman); alias: container
make push             # push container image (ghcr.io)
make build-and-push   # image + push
make kind-test        # full Podman/Kind integration test
make dev-fake         # real backend on fake hub + fake Keycloak (see below)
make dev-fake-poc     # the same on the PoC fixtures
```

## Run the API locally

Requires a kubeconfig that can reach a hub with the ManagedCluster CRD (Kind or
OpenShift after `oc login`). gRPC listens on `:8080`; REST and Swagger UI on
`:8081`. For Tenants, start a local Keycloak first (Podman):

```sh
make keycloak-up      # :8180, seeds realm ai-grid + tenant-a / tenant-b
make run              # KEYCLOAK_URL=http://127.0.0.1:8180 by default
# LISTEN_ADDRESS=:9090 HTTP_LISTEN_ADDRESS=:9091 make run
make keycloak-seed    # re-seed orgs if needed
make keycloak-down
```

See [deploy/keycloak/README.md](deploy/keycloak/README.md). Swagger UI:
`http://localhost:8081/docs/`. With the API up, smoke every RPC/REST route:

```sh
make smoke
# GRPC_ADDR=localhost:8080 HTTP_ADDR=http://localhost:8081 make smoke
```

The unit test uses a Kubernetes dynamic fake client. The Kind integration test
uses the real Kubernetes API server, real OCM CRD, real `ManagedCluster`
objects, Keycloak Organizations, and the deployed gRPC service.

## Dev mode with fake infrastructure

`make dev-fake` runs the real, unmodified backend on a laptop without Kind,
a hub or Keycloak. It starts `cmd/ai-grid-fake-infra`, which fakes the two
systems the backend talks to, then the backend itself against them, in one
terminal; Ctrl-C stops both.

```text
make dev-fake
├── bin/ai-grid-fake-infra --fixtures fixtures/default
│   ├── fake hub API   127.0.0.1:9091  ManagedClusters + SelfSubjectAccessReview
│   │                                  (TLS for the backend, plain HTTP for the console BFF)
│   └── fake Keycloak  127.0.0.1:9093  token endpoint + Organizations Admin API
└── bin/ai-grid-provider-backend --kubeconfig bin/fake-kubeconfig
    ├── gRPC           127.0.0.1:9090  (what the console BFF dials by default)
    └── REST + Swagger 127.0.0.1:9092  (/api/v1/clusters, /api/v1/tenants, /docs/)
```

The fakes answer the way the real systems do: the hub with the Kubernetes API
server's status codes and `metav1.Status` messages, optimistic concurrency on
`resourceVersion`, finalizers and the ACM controllers' conditions, taints and
labels; Keycloak with 26.8's status codes and verbatim error messages. The
backend gets `KEYCLOAK_URL=http://127.0.0.1:9093` with the fake's admin
credentials and a kubeconfig carrying the fake ServiceAccount token. The data
is fictional; see [fixtures/README.md](fixtures/README.md) for what each
preset contains and where it comes from.

```sh
make dev-fake                         # fixtures/default: ten clusters in every ACM state, six organizations,
                                      # and a timeline (a cluster joins at 20s, one detaches at 30s, ...)
make dev-fake-poc                     # fixtures/poc: the PoC's four sites and three organizations, static
make dev-fake DEV_GRPC_PORT=19090 DEV_HUB_PORT=19091 DEV_HTTP_PORT=19092 DEV_KEYCLOAK_PORT=19093
                                      # when the default ports are taken

curl -s http://127.0.0.1:9092/api/v1/clusters
curl -s http://127.0.0.1:9092/api/v1/tenants
grpcurl -plaintext -d '{"id":"site-paris-1"}' 127.0.0.1:9090 ai_grid.v1.Clusters/Get
```

Writes go to the fakes' memory and are gone on restart. A cluster deleted
through the API stays detaching for 30 seconds (divided by `speed` in
`settings.json`) while the hub controllers drain it; then they remove their
own finalizers, and the cluster disappears unless a finalizer of someone
else is still on it.

The kubeconfig is written to `bin/fake-kubeconfig` (`DEV_KUBECONFIG`) with the
context `ai-grid-fake-hub`, and removed on exit. An existing file at that path
that is not such a kubeconfig is never replaced or removed: `make dev-fake`
stops with an error instead.
To see the console as a non-admin or with a rejected token, use the tokens in
`fixtures/default/access.json` (`viewer-token`, `expired-token`).

### With the console

Two terminals:

```sh
# terminal 1, this repository: fake hub + fake Keycloak + real backend
make dev-fake

# terminal 2, ai-grid-provider-console: its BFF (dials 127.0.0.1:9090 and sends its
# SelfSubjectAccessReview to http://127.0.0.1:9091) and its frontend
make -j2 dev-bff-fake dev-frontend-fake
```

Then open http://127.0.0.1:9000. Do not run the console's own `make dev-fake`
at the same time: it starts the console's fake backend on the same 9090 and
9091. The console's `dev-bff-fake` passes `--fake-backend`, so the UI shows its
fake-backend banner although the backend is real here. The console's BFF
is built on the console's draft `ai_grid.v1` proto, whose services are named
differently from this backend's (`ManagedClusters/ListManagedClusters` and
`Tenants/ListTenants` there, `Clusters/List` and `Tenants/List` here; checked
at console commit `537df38`), so until the two protos converge the gRPC calls
it makes for clusters and tenants answer `Unimplemented` and those pages show
"not available on this backend". The sign-in and provider-admin check
(SelfSubjectAccessReview on the fake hub) work as with the console's own
fake.

## Clean up

Delete the local Kind cluster when finished:

```sh
kind delete cluster --name ai-grid-hub
```

## Install ACM hub on OpenShift (playground)

`make openshift-up` installs RHACM automatically when the `ManagedCluster` CRD
is missing (needs `helm` + OperatorHub; allow 15–20+ minutes). To install ACM
alone, or re-run it explicitly:

```sh
make acm-hub-up
# CHANNEL=release-2.13 make acm-hub-up   # OCP 4.18; default chart channel is release-2.13
# CHANNEL=release-2.16 make acm-hub-up   # OCP ≥ 4.19
# SKIP_SAMPLES=1 make acm-hub-up
```

See [deploy/acm-hub/README.md](deploy/acm-hub/README.md). Tear down with
`make acm-hub-down` (not part of `make openshift-down`). On hubs that already
have real ACM clusters, skip backend sample CRs with
`APPLY_SAMPLES=0 make openshift-up` (and `SKIP_ACM=1` if you must not touch ACM).

## Deploy to OpenShift

Kind-parity stack for a logged-in OpenShift cluster (`oc whoami`): backend,
optional embedded Keycloak + Postgres (Tenants API), Routes for HTTP and gRPC,
RHACM when needed (Clusters API), and sample ManagedClusters.

Default image: [`ghcr.io/ai-grid-cloud/ai-grid-provider-backend`](https://github.com/ai-grid-cloud/ai-grid-provider-backend/pkgs/container/ai-grid-provider-backend).

CI (`.github/workflows/ci.yaml`) runs the Check job on pull requests and on
pushes to `main` and `v*` tags (build, lint, unit tests, generated-file
verify). After Check passes, pushes, tags and manual runs publish the image to
ghcr.io: `sha-<full sha>` every time (pin deployments to it), `latest` on
`main`, and semver from `v*` tags. It pushes with `GITHUB_TOKEN`; no repository
secrets are needed.
A new ghcr package starts private: when CI first publishes a new image, an
organization owner sets the package's visibility to Public, or clusters need a
pull secret.

Pull requests also get an image: `publish-image.yml` builds it without
credentials, `publish-pr-image.yml` pushes it as `pr-<number>` (never `sha-*`,
`latest` or semver), `cleanup-pr-image.yml` deletes it when the PR closes, and
`prune-images.yml` removes `pr-*` and untagged versions older than 30 days
every Monday.

Publish locally (login required once: `podman login ghcr.io`):

```sh
make image              # build linux/amd64 → ghcr.io/ai-grid-cloud/ai-grid-provider-backend:latest
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
# Embedded Keycloak + ACM (if ManagedCluster CRD missing) + sample clusters
make openshift-up

# Or build/push and deploy in one step
BUILD_IMAGE=1 make openshift-up

# Use an existing Keycloak instead of deploying one
EXTERNAL_KEYCLOAK=1 \
  KEYCLOAK_URL=https://keycloak.example.com \
  KEYCLOAK_ADMIN_USERNAME=admin \
  KEYCLOAK_ADMIN_PASSWORD=... \
  make openshift-up

# Skip ACM install (Clusters API needs the CRD already present)
SKIP_ACM=1 make openshift-up

# Force ACM install even if the CRD already exists (idempotent helm upgrade)
INSTALL_ACM=1 make openshift-up

# Skip sample ManagedClusters on hubs that already have real ACM clusters
APPLY_SAMPLES=0 make openshift-up
```

Clusters `List`, `Get` and `Update` report each cluster's `status.provisioning`
from the hub Policy named by `AI_GRID_READY_POLICY` (`namespace/name`, default
`ai-grid-policies/ai-grid-ready`). The ClusterRole in
`deploy/openshift/rbac.yaml` grants `get` on Policies with that name only, in
any namespace; change its `resourceNames` if you change the variable. Without
that permission, or without the Policy, `provisioning` is empty.

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
docs/UI_API_REFERENCE.md         REST payloads for UI/BFF consumers
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
hack/smoke-apis.sh               gRPC + REST smoke for Ping/Clusters/Tenants (make smoke)
hack/test-kind.sh                end-to-end integration test
integration/kind_test.go         typed gRPC integration test
cmd/ai-grid-fake-infra/          fake hub API + fake Keycloak for make dev-fake
internal/fakeinfra/              the fakes (hub, keycloak, devserve) and their tests
fixtures/                        fake infrastructure fixtures (default, poc)
hack/dev-fake.sh                 runs the fakes and the backend for make dev-fake
hack/convert-console-fixtures/   regenerates fixtures/ from the console's fixtures
Makefile                         build, test, lint, generate, Kind, OpenShift, ACM targets
```


