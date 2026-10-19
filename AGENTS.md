# AI Grid Provider Backend

Go gRPC + REST (grpc-gateway) service for AI Grid provider APIs. Today it
exposes Ping, Clusters CRUD (OCM/RHACM hub ManagedCluster CRs), and Tenants (Keycloak Organizations).
Local runtime is Kind + Podman; OpenShift deploy is also supported. See
[`README.md`](README.md).

## Required context

Before changing this component, identify the documents relevant to the change,
then read and follow them. These documents are authoritative for their areas.

- API or proto work: [`docs/API.md`](docs/API.md) (building standards)
- Local setup, Kind, and smoke calls: [`README.md`](README.md)
- Generated OpenAPI snapshot: [`docs/openapi/ai_grid.swagger.json`](docs/openapi/ai_grid.swagger.json)

## Invariants

- Proto is the contract. Edit `proto/`; never hand-edit `gen/` or committed
  OpenAPI under `docs/openapi/` / `internal/restgateway/openapi/`.
- After proto changes, run `make generate` and commit generated outputs.
- New resource APIs follow OSAC object shape and RPC conventions in
  [`docs/API.md`](docs/API.md): `id` + `metadata` + `spec` + `status`, standard
  List/Get/Create/Update/Delete naming, flat REST under `/api/v1/`.
- Prefer `buf.validate` (or equivalent proto validation) for field rules; put
  cross-resource checks in server logic.
- Match on gRPC status codes, never on error message text. Wrap with `%w`.
- Use `log/slog` for logging; do not introduce zap/zerolog as the primary logger.
- Do not commit credentials, tokens, or private keys. Kind Keycloak `admin` /
  `admin` is local-dev only.
- Prefer Podman over Docker and `kubectl`/`oc` against Kind or OpenShift for
  local verification.

## Project layout

```text
cmd/ai-grid-provider-backend/   service entrypoint
client/                         reusable gRPC client
proto/                          protobuf + buf config
gen/ai_grid/v1/                 generated protobuf, gRPC, gateway
docs/openapi/                   generated OpenAPI (Swagger 2.0)
docs/API.md                     API building standards (authoritative)
internal/servers/               Per-resource gRPC servers (OSAC-style builders)
internal/keycloak/              Keycloak Admin API client
internal/restgateway/           HTTP gateway + Swagger UI
deploy/kind/                    Kind Kubernetes manifests
deploy/openshift/               OpenShift manifests (kustomize + Routes)
deploy/acm-hub/                 Helm chart + scripts for RHACM hub playground
hack/                           Kind/OCM/OpenShift/prereq helpers
integration/                    Kind gRPC integration tests
```

Go module: `github.com/osac-project/ai-grid-provider-backend`.

## Generated files

- Proto → Go / gateway / OpenAPI: `make generate` (requires [buf](https://buf.build)).
- Commit `proto/` sources plus regenerated `gen/`, `docs/openapi/`, and
  `internal/restgateway/openapi/`.
- Never hand-edit generated trees.

## API standards (summary)

Full detail: [`docs/API.md`](docs/API.md). When adding or changing APIs:

| Topic | Rule |
|-------|------|
| Object shape | `id`, `metadata`, `spec`, `status` (see OSAC_OBJECT_SHAPE) |
| Spec vs status | Spec is user-controlled; status is system-controlled |
| Service RPCs | List, Get, Create, Update, Delete (+ private Signal if needed) |
| Request names | `{Service}{Method}Request` / `{Service}{Method}Response` |
| REST | Flat paths under `/api/v1/`; no nested collections |
| Types / fields | `CamelCase` types; `snake_case` fields |
| Enums | `TYPE_PREFIX_VALUE`; first value `_UNSPECIFIED = 0` |
| References | Typed reference messages, not bare ID strings |

Current MVP messages (Ping, ManagedCluster, Tenant) are intentionally lean.
Evolve them toward the standard shape when they become full managed resources.

## Validation

Run from the repository root as applicable.

```bash
make prereqs          # toolchain check (Podman, Kind, kubectl, grpcurl, Go, clusteradm)
make build            # bin/ai-grid-provider-backend
make test             # go test ./...
make lint             # gofmt check, go vet; golangci-lint when installed
make verify           # build + test + lint
make generate         # regenerate proto / gateway / OpenAPI
make kind-test        # full Kind integration (KIND_EXPERIMENTAL_PROVIDER=podman)
make image            # build container (IMAGE/TAG, default Quay :latest)
make push             # push container (podman login quay.io first)
make build-and-push   # image + push
make openshift-up     # deploy to current oc context (Quay image by default)
make openshift-down   # remove OpenShift deploy
make acm-hub-up       # RHACM via Helm/OLM + playground ManagedClusters
make acm-hub-down     # tear down ACM hub Helm release
```


Manual Kind bring-up:

```bash
export KIND_EXPERIMENTAL_PROVIDER=podman
bash hack/kind-up.sh
bash hack/ocm-up.sh
kubectl -n ai-grid port-forward service/ai-grid-provider-backend 8080:8080 8081:8081
```

Swagger UI: `http://localhost:8081/docs/`.

OpenShift bring-up (requires `oc login`):

```bash
make openshift-up
# EXTERNAL_KEYCLOAK=1 KEYCLOAK_URL=... KEYCLOAK_ADMIN_USERNAME=... KEYCLOAK_ADMIN_PASSWORD=... make openshift-up
```

Default image: `quay.io/rh-ee-rjjohnso/ai-grid-provider-backend:latest`.
Embedded Keycloak is the default; set `EXTERNAL_KEYCLOAK=1` to point at an
existing Keycloak instead.

## Security

- Never commit secrets, kubeconfigs with credentials, or production Keycloak
  admin passwords.
- Outside local Kind, use a dedicated Keycloak service account with least
  privilege (not realm-admin credentials). OpenShift deploy stores Keycloak
  settings in Secret `ai-grid-provider-backend-keycloak`.
- Prefer in-cluster ServiceAccount credentials; only use `--kubeconfig` for
  local out-of-cluster debugging.
