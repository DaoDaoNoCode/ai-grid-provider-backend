# AI Grid Provider Backend

Go 1.26.3 service exposing gRPC APIs and a grpc-gateway REST surface for AI Grid provider operations. It currently manages OCM/RHACM `ManagedCluster` resources and Keycloak Organizations. Local integration uses Kind and Podman; OpenShift deployment is supported.

## Instruction precedence

1. Follow this file for repository-wide invariants and validation.
2. For API, protobuf, REST, resource-shape, server-layer, error, logging, testing, and security work, read and follow [`docs/API.md`](docs/API.md).
3. Use [`README.md`](README.md) for supported setup and operational workflows.
4. If documentation and executable configuration disagree, stop and report the mismatch; do not silently choose one.

The OSAC fulfillment-service standards are the reference architecture. Apply their API and service invariants where this repository implements the same concern. Do not copy fulfillment-service-specific PostgreSQL, event, reconciler, GoReleaser, or public/private API patterns unless the change intentionally introduces those capabilities.

## Non-negotiable invariants

- Protobuf is the API contract. Edit sources under `proto/`; never hand-edit generated files under `gen/`, `docs/openapi/`, or `internal/restgateway/openapi/`.
- After any protobuf change, run `make generate`, commit all generated outputs, and verify regeneration leaves the worktree clean.
- Managed resources use the OSAC shape: `id`, `metadata`, `spec`, and `status`.
  - `spec` is caller-controlled desired state.
  - `status` is service-controlled observed state.
- Resource services use plural names and standard `List`, `Get`, `Create`, `Update`, and `Delete` methods. Use `{Service}{Method}Request` and `{Service}{Method}Response` message names.
- REST collections remain flat under `/api/v1/`; do not introduce nested collection URLs.
- Use `CamelCase` for protobuf types, `snake_case` for fields, and type-prefixed `UPPER_SNAKE_CASE` enum values. Every enum starts with an `_UNSPECIFIED = 0` value.
- Use typed reference messages rather than unstructured ID strings when introducing resource relationships.
- Put field and cross-field rules in protobuf validation annotations where possible. Put external lookups, uniqueness, quota, and other cross-resource checks in server logic.
- Preserve gRPC status semantics. Match errors by status code or typed error, never by message text. Wrap Go errors with `%w`.
- Use `log/slog` with structured fields. Never log credentials, bearer tokens, kubeconfigs, Keycloak secrets, or complete sensitive request bodies.
- Do not commit credentials, tokens, private keys, kubeconfigs containing credentials, or production Keycloak administrator passwords.
- Local `admin` / `admin` Keycloak credentials are development-only. Non-local deployments use a least-privilege service account.
- Prefer in-cluster Kubernetes ServiceAccount credentials. Use `--kubeconfig` only for local, out-of-cluster development.
- Prefer Podman for local container and Kind workflows.

## Repository map

```text
cmd/ai-grid-provider-backend/   service entrypoint
client/                         reusable gRPC client
proto/                          protobuf sources and buf configuration
gen/ai_grid/v1/                 generated protobuf, gRPC, and gateway code
docs/API.md                     authoritative API/service standards for this repo
docs/openapi/                   generated OpenAPI snapshot
internal/servers/               per-resource gRPC implementations
internal/keycloak/              Keycloak Admin API client
internal/restgateway/           REST gateway and Swagger UI
integration/                    Kind-backed integration tests
deploy/kind/                    Kind manifests
deploy/openshift/               OpenShift manifests
deploy/acm-hub/                 RHACM hub development assets
hack/                           development, smoke, and deployment helpers
```

Go module: `github.com/osac-project/ai-grid-provider-backend`.

## Change workflow

1. Read the relevant source, tests, `docs/API.md`, and nearby patterns before editing.
2. Keep changes scoped; do not refactor unrelated code.
3. For a new managed resource, update every applicable layer:
   - protobuf type and service contract;
   - generated Go, gRPC, gateway, and OpenAPI outputs;
   - server implementation and dependency wiring;
   - REST registration;
   - unit tests and integration/smoke coverage;
   - deployment/RBAC/configuration when new permissions or settings are required;
   - user and API documentation.
4. Add or update tests with production changes. Cover success, validation, not-found, conflict/already-exists, authorization, and dependency-failure behavior as applicable.
5. Run the smallest relevant checks while iterating, then the required final checks below.
6. Re-read the complete diff before committing. Confirm generated files, docs, manifests, and tests agree with the contract.

## Generated files

Run:

```bash
make generate
```

Commit the protobuf sources and regenerated outputs in:

- `gen/ai_grid/v1/`
- `docs/openapi/`
- `internal/restgateway/openapi/`

After generation, rerun `make generate` and require `git diff --exit-code` to succeed. Generated files must be changed through their source and generator, never patched directly.

## Validation

Run commands from the repository root.

### Required for every Go change

```bash
make build
make test
make lint
make help             # list targets
make prereqs          # toolchain check (Podman, Kind, kubectl, grpcurl, Go, clusteradm)
make build            # bin/ai-grid-provider-backend
make unit             # unit tests (excludes ./integration)
make smoke            # live gRPC/REST smoke (needs make run; optional keycloak-up)
make test             # alias for unit
make lint             # gofmt check, go vet; golangci-lint when installed
make verify           # build + unit + lint
make generate         # regenerate proto / gateway / OpenAPI
make run              # local API (gRPC :8080, REST/Swagger :8081) against kubeconfig
make keycloak-up      # local Keycloak on :8180 + seed tenant-a/tenant-b
make keycloak-seed    # re-seed ai-grid realm / Organizations (idempotent)
make keycloak-down    # remove local Keycloak container
make kind-test        # full Kind integration (KIND_EXPERIMENTAL_PROVIDER=podman)
make image            # build container image (IMAGE/TAG, default Quay :latest)
make push             # push container (podman login quay.io first)
make build-and-push   # image + push
make openshift-up     # deploy to current oc context (Quay image by default)
make openshift-down   # remove OpenShift deploy
make acm-hub-up       # RHACM via Helm/OLM + playground ManagedClusters
make acm-hub-down     # tear down ACM hub Helm release
```

`make verify` is the aggregate check and must pass before a change is considered ready.

### Required for protobuf or API changes

```bash
cd proto && buf lint
cd proto && buf generate
make generate
git diff --exit-code
make verify
```

Also verify the generated OpenAPI and REST routes match the protobuf contract.

### Required for shell-script changes

```bash
bash -n path/to/changed-script.sh
shellcheck path/to/changed-script.sh
```

If `shellcheck` is unavailable, report that gap; do not claim shell validation passed.

### Required for container or deployment changes

```bash
make image
```

Use an explicit non-production image destination for development. Do not push images or deploy to a cluster unless the user explicitly requests it and the destination/context has been confirmed.

### Required for integration-sensitive changes

```bash
export KIND_EXPERIMENTAL_PROVIDER=podman
make kind-test
```

Run this for changes affecting Kubernetes behavior, OCM/RHACM resources, Keycloak integration, generated API behavior, deployment manifests, or end-to-end request flow. If the environment cannot run it, state exactly what was not validated.

## Testing conventions

- Keep unit tests beside the package under test using `*_test.go`.
- Keep Kind-backed integration coverage under `integration/` and its supporting scripts.
- Prefer deterministic tests and typed assertions over matching human-readable error strings.
- Test gRPC status codes and response contracts.
- Do not weaken or delete tests merely to make a change pass.
- Do not introduce a new test framework without an explicit repository decision; follow the framework already used by neighboring tests.

## Error handling and logging

- Map invalid input to `codes.InvalidArgument`, missing resources to `codes.NotFound`, duplicates to `codes.AlreadyExists`, authorization failures to `codes.PermissionDenied`, and retryable version/conflict failures to `codes.Aborted` where applicable.
- Preserve underlying errors with `fmt.Errorf("context: %w", err)`.
- Log with `log/slog` and stable structured keys.
- Include useful resource identifiers and operation names, but redact secrets and sensitive payload fields.
## CI

[`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) runs Check on pull
requests and on pushes to `main` / `development` (`make build`, `fmt-check`,
`vet`, pinned golangci-lint + `.golangci.yml`, `unit`, `make generate` dirty
diff). Image build/push to Quay runs only on push (never on PRs).

Tags: `latest` on `main`, `development` on `development`, plus branch and
`sha-*` tags. Repo secrets required: `QUAY_USERNAME`, `QUAY_PASSWORD`.

## Security

## Security and tenancy

- Treat tenant identity and authorization as server-side concerns; never trust caller-supplied ownership fields without validation.
- Use least-privilege Kubernetes RBAC and Keycloak roles. Any new API operation that touches Kubernetes or Keycloak must include a review of required permissions.
- Do not expose internal credentials or administrative APIs through generated REST routes.
- Validate authentication and authorization behavior for every new externally reachable method.

## Build and image rules

- `make build` builds `bin/ai-grid-provider-backend`.
- `make image` builds the container image; `make push` publishes it.
- Keep `Containerfile` multi-stage and the runtime non-root.
- Pin tool and action versions used by CI. Do not use moving `latest` versions for linters or build tools.
- Automated publication must target a team-owned image namespace, not a personal namespace.

## Git and review hygiene

- Use focused commits with DCO sign-off.
- Include the repository-required AI-assistance trailer for AI-assisted commits.
- Never rewrite or discard unrelated user changes.
- Do not commit generated artifacts without their source changes.
- Before reporting completion, provide the exact checks run and their outcomes, plus any validation that could not be performed.

## Documentation

Update documentation in the same change when behavior, API contracts, configuration, permissions, deployment steps, or developer commands change. Keep `README.md`, `docs/API.md`, generated OpenAPI, manifests, and this file consistent with executable behavior.
