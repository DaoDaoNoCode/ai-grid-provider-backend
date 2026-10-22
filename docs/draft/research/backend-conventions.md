# Spike: ai-grid-backend conventions, OSAC alignment, and the next `ai_grid.v1` additions

Date: 2026-10-15. Read-only research. Nothing in the console repo was modified.

Paths used below:

- Console: `<console repo>` (branch `mock-backend`)
- Backend checkout: `<console repo>/.cache/ai-grid-backend`
- OSAC clones (shallow, read only): `<scratchpad>/conv/{osac,fulfillment-service,fulfillment-api,fulfillment-common}`

## 1. Summary

- `ai-grid-backend` is a two-commit, single-file-proto Go gRPC service (`ai_grid.v1`, three services, three unary RPCs). `origin/main` is identical to the commit pinned in `ai-grid-provider-console/bff/go.mod` (`fb419b32653d`); there is nothing newer to summarise and the repo has no tags. The module path is already `github.com/osac-project/ai-grid-backend` while the repo lives under `github.com/yuvalturg/ai-grid-backend`, hence the `replace` in `ai-grid-provider-console/bff/go.mod`.
- The backend says it is "aligned with fulfillment-service", but today the alignment is only Go 1.26.3 and the general shape (gRPC + health + reflection + Kubernetes dynamic client + Keycloak Admin REST). Its proto style is Google-AIP-ish (`ListManagedClusters`, `clusters` field), not OSAC's (`Clusters/List`, `items`/`size`/`total`, `object` envelope, `offset/limit/filter/order`, `update_mask` + `lock`, `id/metadata/spec/status`).
- OSAC's conventions are precise and documented (`fulfillment-service/docs/API.md`, enforced by `buf lint` plus a custom `OSAC_OBJECT_SHAPE` rule). The old `osac-project/fulfillment-service`, `fulfillment-api` and `fulfillment-common` repos are archived/redirected; the live code is the `osac-project/osac` monorepo with one shared `proto/` module and one committed `proto/gen/` Go tree, CI-verified and published to the Buf Schema Registry with semver labels.
- Recommendation: keep the backend's existing method-name style (`<Verb><Noun>`, already consumed by the console) but adopt OSAC's message shapes for everything new (pagination, filter, `object`/`items` envelopes, field masks, condition pattern, enum rules, documentation rules), adopt `buf` (lint STANDARD + breaking FILE) and committed generated code, and version with plain `vX.Y.Z` Go module tags. A draft of the next proto files in that style is in section 6, with a two-repo workflow (section 7) and open questions (section 8).
- Two defects in the current backend worth raising: errors are returned as plain `fmt.Errorf` (the console sees `codes.Unknown`), and the server has no authn/authz interceptor at all (it ignores the `authorization` metadata the console sends and calls the hub as its own ServiceAccount).

## 2. Backend facts (pinned commit `fb419b32653d`)

### Repository layout and module

| Item | Fact | Source |
|---|---|---|
| Module path | `github.com/osac-project/ai-grid-backend`, `go 1.26.3` | `.cache/ai-grid-backend/go.mod` |
| Direct requires | `google.golang.org/grpc v1.61.0`, `google.golang.org/protobuf v1.36.5`, `k8s.io/apimachinery v0.34.1`, `k8s.io/client-go v0.34.1` (no buf, no grpc-gateway, no protovalidate) | `go.mod` |
| Console pin | `require github.com/osac-project/ai-grid-backend v0.0.0-20261014173018-fb419b32653d` with `replace ... => github.com/yuvalturg/ai-grid-backend v0.0.0-20261014173018-fb419b32653d`; console itself uses grpc v1.84.0 | `ai-grid-provider-console/bff/go.mod` |
| History | 2 commits: `4ad27b3 Initial ai-grid backend implementation`, `fb419b3 Add client-server ping integration coverage`; no tags (`git ls-remote --tags` empty) | `git log` |
| Layout | `proto/ai_grid/v1/ai_grid.proto`, `gen/ai_grid/v1/{ai_grid.pb.go,ai_grid_grpc.pb.go}`, `cmd/ai-grid-backend/main.go`, `client/client.go`, `internal/ai_grid/{service,managedclusters,tenants}.go`, `internal/keycloak/client.go`, `integration/kind_test.go`, `deploy/kind/*.yaml`, `hack/*.sh`, `Makefile`, `Containerfile`, `README.md` | `find` |

### Proto

- One file, `syntax = "proto3"; package ai_grid.v1;` with `option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";` (`proto/ai_grid/v1/ai_grid.proto`).
- Services: `AiGrid { Ping }`, `ManagedClusters { ListManagedClusters }`, `Tenants { ListTenants }`.
- Messages: `PingRequest{name}`, `PingResponse{message}`, `ListManagedClustersRequest{}`, `ListManagedClustersResponse{repeated ManagedCluster clusters = 1}`, `ManagedCluster{name=1, map<string,string> labels=2, bool hub_accepts_client=3}`, `ListTenantsRequest{}`, `ListTenantsResponse{repeated Tenant tenants = 1}`, `Tenant{id=1, name=2, repeated string domains=3}`.
- Naming style: service = plural noun (`ManagedClusters`, `Tenants`) like OSAC; method = `<Verb><Noun>` (`ListManagedClusters`) like Google AIP-132, unlike OSAC's bare `List`. Request/response = `<Method>Request/Response`. List response field is the typed plural (`clusters`, `tenants`), not `items`. No pagination, filter, metadata, status, conditions, timestamps, or HTTP annotations. Comments only on services. No `buf.validate`.
- `ManagedCluster` deliberately mirrors the OCM CRD (`// The response shape is intentionally based on Kubernetes ManagedCluster metadata so the backing store can later be replaced with the ACM/OCM Kubernetes API.`).

### Generated code

- Committed under `gen/ai_grid/v1/`. Header: `protoc-gen-go v1.36.6`, `protoc v3.13.0`, `source: proto/ai_grid/v1/ai_grid.proto` (`gen/ai_grid/v1/ai_grid.pb.go` lines 1-5). `ai_grid_grpc.pb.go` declares `grpc.SupportPackageIsVersion7`, i.e. an older `protoc-gen-go-grpc` (current releases emit version 9).
- There is no `buf.yaml`, `buf.gen.yaml`, or Makefile target that regenerates code; `Makefile` targets are `build test vet fmt-check lint verify prereqs kind-test clean-kind`. `fmt-check` gofmt's `client cmd gen integration internal`. `lint` runs `golangci-lint` only if installed (no `.golangci.yml`). Regeneration is therefore manual and not reproducible from the repo.

### Server wiring (`cmd/ai-grid-backend/main.go`)

- Flags: `--listen-address` (default `:8080`), `--kubeconfig` (empty = `rest.InClusterConfig()`).
- `grpc.NewServer()` with **no interceptors, no TLS, no auth**; registers the three services from one `*aigrid.Service` (which embeds the three `Unimplemented*Server`s, `internal/ai_grid/service.go`), then `health.NewServer()` with `SetServingStatus("", SERVING)` and `reflection.Register`.
- Logging: `log/slog` default logger, two Info lines (listening, shutting down); fatal paths print to stderr and `os.Exit(1)`. Graceful stop on SIGINT/SIGTERM via `signal.NotifyContext`.
- Errors: handlers return `fmt.Errorf(...)` or the raw Keycloak error; nothing produces `status.Error`, so clients see `codes.Unknown`. The only mapping is `apierrors.IsNotFound(err)` -> empty list (CRD absent) in `internal/ai_grid/managedclusters.go`.
- Keycloak config from env `KEYCLOAK_URL`, `KEYCLOAK_ADMIN_REALM` (default `master`), `KEYCLOAK_REALM` (default `ai-grid`), `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` with development defaults in code (the console's recent commits move these to a required Secret on the deploy side).

### Kubernetes client

- `dynamic.NewForConfig` (unstructured), GVR `cluster.open-cluster-management.io/v1/managedclusters`, `List(ctx, metav1.ListOptions{})`, fields read with `unstructured.NestedBool(item.Object, "spec", "hubAcceptsClient")` plus `GetName()/GetLabels()` (`internal/ai_grid/managedclusters.go`). `NewServiceFromKubeconfig` uses `clientcmd.BuildConfigFromFlags("", path)`. It always acts as its own identity (in-cluster SA or the kubeconfig user), never as the caller.
- Unit test uses `k8s.io/client-go/dynamic/fake.NewSimpleDynamicClient` (`internal/ai_grid/service_test.go`).

### Keycloak client (`internal/keycloak/client.go`)

- Plain `net/http`, password grant on `admin-cli` in the admin realm, then `GET /admin/realms/<realm>/organizations?first=0&max=100` (hard cap 100, no paging). Tenant = Organization `{id, name, domains[].name}`. Comment says production should use a service account with `query-organizations`.

### Container, deploy, tests, CI

- `Containerfile`: `golang:1.26.3` build, `CGO_ENABLED=0 -trimpath -ldflags='-s -w'`, runtime `gcr.io/distroless/static-debian12:nonroot`, `EXPOSE 8080`.
- `deploy/kind/ai-grid-backend.yaml`: Namespace `ai-grid`, ServiceAccount, ClusterRole `get/list/watch managedclusters`, ClusterRoleBinding, Deployment (1 replica, tcpSocket readiness on `grpc`), Service `grpc` 8080. `deploy/kind/keycloak.yaml`: Keycloak 26.3 `start-dev` + Postgres + an init Job that creates realm `ai-grid` with `organizationsEnabled` and Organizations `tenant-a`, `tenant-b`. `deploy/kind/sample-managedclusters.yaml`: two sample `ManagedCluster`s.
- Tests: one unit test (fake dynamic client); `integration/kind_test.go` typed tests gated on `AI_GRID_GRPC_ADDRESS`, driven by `hack/test-kind.sh` (Kind + Podman + `clusteradm init` + port-forward + `go test ./integration`).
- CI: **none** (no `.github/`).
- `client/client.go`: a `Builder` (`SetAddress`, `SetPlaintext`, `Build`) that refuses non-plaintext because TLS is not configured; uses `grpc.DialContext`, which is not deprecated at the backend's pinned `grpc v1.61.0` but carries `Deprecated: use NewClient instead` at the `v1.84.0` the console resolves, so the console build sees the deprecation. This Builder/Set* style is the OSAC Go idiom (compare `auth.NewGrpcAuthnInterceptor().SetLogger(...).Build()` in the monorepo).

## 3. `origin/main` delta since the pinned commit

`git -C .cache/ai-grid-backend fetch origin` succeeded; `git rev-parse HEAD origin/main` both return `fb419b32653dbb0ccc723531df20e4cb02b417f2`; `git diff --stat HEAD origin/main` is empty; the only remote branch is `origin/main`; no tags. **No new RPCs or changes exist upstream.** The console's "Waiting on backend RPCs" table (`ai-grid-provider-console/docs/plan/bff-api.md`) is still entirely open.

## 4. OSAC conventions (osac-project/osac monorepo, commit `d21dd2b`, 2026-10-15)

Repo state: `github.com/osac-project/fulfillment-api` and `fulfillment-common` carry an archive notice (last commit 2026-03-04, "MGMT-23354: Add archive notice"); `fulfillment-service` last commit 2026-09-09 "Redirect agents to the active OSAC repository". The monorepo `github.com/osac-project/osac` holds `fulfillment-service/`, `osac-operator/`, `osac-ui/`, `proto/`, etc. (`osac/README.md`, `osac/go.work`).

### Directory layout, packages, versioning

- Protos live in a top-level module `proto/` with three buf modules: `proto/private/osac/private/v1/` (**editable source of truth**, package `osac.private.v1`), `proto/public/osac/public/v1/` (**generated** from private by `protoc-gen-cleanapi`, package `osac.public.v1`), `proto/tests/osac/tests/v1/`. Generated Go for all of them is committed in `proto/gen/` and imported as `github.com/osac-project/osac/proto/gen/osac/<layer>/v1` (`proto/AGENTS.md`, `proto/buf.yaml`, `proto/buf.gen.yaml`, `proto/go.mod` = `module github.com/osac-project/osac/proto`).
- Public/private split is done with `cleanapi` annotations: `[(cleanapi.field).private = true]`, `option (cleanapi.message).private = true`, `option (cleanapi.method).private = true`, `option (cleanapi.file).private = true`, `option (cleanapi.file).http_route_prefix_map = "private:fulfillment"` (`fulfillment-service/docs/CLEANAPI.md`, `fulfillment-service/docs/API.md`). Private-only fields must be numbered after all public fields.
- One file per object: `<type>_type.proto` (object, Spec, Status, conditions, enums, references) and `<type>s_service.proto` (service + request/response messages). Shared types in their own files (`metadata_type.proto`, `condition_status_type.proto`) (`API.md`, "File organization").
- Versioning: the package is `v1`; evolution is additive with `reserved` numbers and names when fields are removed (e.g. `ClusterSpec { reserved 4; reserved "pull_secret"; }`, `ClusterStatus { reserved 14; reserved "upgrade"; }`, `Event { reserved 10; reserved "host_type"; }` in `proto/public/osac/public/v1/cluster_type.proto`, `event_type.proto`). `buf.yaml` sets `breaking: use: [FILE]`.

### Service and method naming

- Service = plural of the type, **no `Service` suffix** (`Clusters`, `Tenants`, `Users`, `Hubs`); `buf.yaml` has `lint: except: [SERVICE_SUFFIX]` for this.
- Methods are `List`, `Get`, `Create`, `Update`, `Delete` on most public services; the immutable networking services (`Subnets`, `VirtualNetworks`, `NatGateways`, `ExternalIps`, `ExternalIpAttachments`) omit `Update` by design, and read-only ones such as `StorageTiers` expose only `List`/`Get` (`API.md` L418-423; `proto/public/osac/public/v1/*_service.proto` at `334a01c`); private services add `Signal`. "Services should not have any additional ad-hoc methods"; no imperative verbs (`Start`, `Reboot`) — model them as `spec` fields (`API.md`, "Standard methods", "Declarative, intent-based design"). Non-CRUD singletons are `Create`-only or `Get`-only services: `SelfSubjectAccessReviews/Create`, `ConsoleSessions/Create`, `Capabilities/Get`, `Events/Watch`.
- Request/response messages: `{Service}{Method}Request/Response` (`ClustersListRequest`, `ClustersGetResponse`) (`API.md`, "Request and response messages").

### Request/response shapes

```protobuf
message ThingsListRequest   { optional int32 offset = 1; optional int32 limit = 2; optional string filter = 3; optional string order = 4; }
message ThingsListResponse  { int32 size = 1; int32 total = 2; repeated Thing items = 3; }
message ThingsGetRequest    { string id = 1; }
message ThingsGetResponse   { Thing object = 1; }
message ThingsCreateRequest { Thing object = 1; }            // response: Thing object = 1
message ThingsUpdateRequest { Thing object = 1; google.protobuf.FieldMask update_mask = 2; bool lock = 3; }
message ThingsDeleteRequest { string id = 1; }              // response: empty
```
(`proto/public/osac/public/v1/clusters_service.proto`, `tenants_service.proto`, `users_service.proto`; rules in `API.md`.)

- **Pagination**: offset/limit, not page tokens. `limit` omitted = server default, which is a **page of 100** (`internal/database/dao/generic_dao.go` L116 `defaultLimit: 100`, with a cap `maxLimit: 1000` at L117; both applied in `generic_dao_list.go` L170-175; `GenericServer.List` only forwards the limit when `HasLimit()`), not "all"; `limit = 0` = count only; negative rejected. `size` = items returned, `total` = matches regardless of paging (`API.md`, "List"; `GenericServer.List` sets `SetSize/SetTotal`, `fulfillment-service/internal/servers/generic_server.go` ~L408-414).
- **Filtering**: `filter` is a **CEL** expression over `this` (the object) and `now`; translated to SQL server-side; supported subset documented in `fulfillment-service/docs/FILTER.md` (comparisons, `&& || !`, `in` on lists/arrays/maps, map index on `metadata.labels`, `startsWith/endsWith/contains`, `has()`, timestamps vs `now`/`null`). Invalid filter -> `InvalidArgument`. `order` ("SQL ORDER BY-like") is declared but "not yet implemented".
- **Field masks**: `Update` takes `object` + `google.protobuf.FieldMask update_mask` + `bool lock`; the REST gateway fills the mask from the PATCH body; validation runs on the merged stored object, not the partial request. `lock=true` compares `metadata.version` and fails with `Aborted` on mismatch (`generic_server.go` ~L707-720; `API.md`, "Update").
- "No extra request or response fields" beyond these without a cross-service design discussion (`API.md`, "Additional rules").

### Object shape and metadata

```protobuf
message Thing { string id = 1; Metadata metadata = 2; ThingSpec spec = 3; ThingStatus status = 4; }
```
Enforced by the custom buf lint rule `OSAC_OBJECT_SHAPE` (`fulfillment-service/cmd/buf-plugin-osac-lint/main.go`): if a service's `Get` output and `Create` input both carry `object` (field 1) of the same type, that type must have exactly those four fields. Flat exceptions need `// buf:lint:ignore OSAC_OBJECT_SHAPE` (e.g. `Tenant` has no status: `tenant_type.proto`).

`Metadata` (`proto/public/osac/public/v1/metadata_type.proto`): `creation_timestamp=1`, `deletion_timestamp=2`, `creator=3`, `tenant=4`, `name=5` (RFC 1123 DNS label, `buf.validate` pattern, required/immutable/unique per tenant+project+type), `labels=6` (Kubernetes-style key rules, indexed/searchable), `annotations=7` (opaque, "the system should never read annotations to make decisions"), `version=8` (int32, auto-incremented), `project=9`, `display_name=10`, `description=11`; private-only `repeated string finalizers = 12 [(cleanapi.field).private = true]` (`proto/private/osac/private/v1/metadata_type.proto`). `id` is server-assigned (UUID), distinct from `name`.

`spec` is user-owned, `status` is system-owned; never store relationships in annotations (`API.md`, "Spec and status ownership", "Annotations vs spec/status fields").

### Status and conditions

- `status.state` is a per-type enum (`ClusterState { CLUSTER_STATE_UNSPECIFIED=0, PROGRESSING, READY, FAILED, DELETING, DELETE_FAILED }`) **plus** `repeated <Type>Condition conditions`.
- Condition pattern (`API.md`, "Conditions"; `cluster_type.proto`):
```protobuf
message ThingCondition { ThingConditionType type = 1; ConditionStatus status = 2; google.protobuf.Timestamp last_transition_time = 3; optional string reason = 4; optional string message = 5; }
enum ThingConditionType { THING_CONDITION_TYPE_UNSPECIFIED = 0; ..._PROGRESSING = 1; ..._READY = 2; ..._FAILED = 3; }   // cluster_type.proto L451-479 also has DEGRADED = 4, WORKER_PROVISIONING_FAILED = 5, WORKER_PROVISIONING_BLOCKED = 6
```
  Shared `enum ConditionStatus { CONDITION_STATUS_UNSPECIFIED = 0; CONDITION_STATUS_TRUE = 1; CONDITION_STATUS_FALSE = 2; }` (`condition_status_type.proto`); Kubernetes `Unknown` maps to `UNSPECIFIED`. Condition `type` is a typed enum, except `UserCondition.type`, which is a plain `string` (`user_type.proto` L74-77; the proto gives no reason). No `observed_generation`.
- Enums: `CamelCase` name, values prefixed with the enum name in `UPPER_SNAKE_CASE`, first value `_UNSPECIFIED = 0`, `[(buf.validate.field).enum.defined_only = true]` where input is accepted (`API.md`, "Enums").

### Events / watch

- One streaming RPC for the whole API: `Events/Watch(EventsWatchRequest{optional string filter}) returns (stream EventsWatchResponse{Event event})`. `Event { id, EventType type (OBJECT_CREATED/UPDATED/DELETED), Timestamp timestamp, oneof payload { Cluster cluster = 4; Tenant tenant = 16; ... } }`. No delivery/order guarantees; clients should reconcile periodically (`events_service.proto`, `event_type.proto`). Per-service `Watch` RPCs do not exist.

### REST (grpc-gateway) annotations

- Every method has `google.api.http`: `List GET /api/fulfillment/v1/things`, `Get GET .../things/{id}` with `response_body: "object"`, `Create POST` with `body: "object"`, `Update PATCH .../things/{object.id}` with `body/response_body: "object"`, `Delete DELETE .../things/{id}`. Private prefix `/api/private/v1/`. Flat URL space (no nesting; use filters). JSON uses proto field names (`UseProtoNames: true` in `fulfillment-service/internal/cmd/service/start/restgateway/start_rest_gateway_cmd.go`). OpenAPI metadata in `openapi_options.proto`. `google.api.field_behavior` (`OUTPUT_ONLY`, `IMMUTABLE`) is used on fields.

### Validation

- `buf.validate` (protovalidate) annotations on every constrained field; a `validation.NewProtovalidateInterceptor()` rejects bad Create requests with `InvalidArgument` (`fulfillment-service/internal/validation/protovalidate_interceptor.go` L158); Update is validated after mask merge in the server (`API.md`, "Validation flow").

### Authentication and authorization (server side)

- Interceptor chain (`fulfillment-service/internal/cmd/service/start/grpcserver/start_grpc_server_cmd.go` L599-626): `panic -> metrics -> logging -> protovalidate -> db tx -> authn -> authz -> JIT user provisioning -> reference validation` (`referenceValidator`, which resolves/validates object references after authorization; the stream chain is the same minus tx and also ends with the reference validator). Then `reflection.RegisterV1`, `health.NewServer()` + `healthv1.RegisterHealthServer`.
- **Authn** (`internal/auth/grpc_authn_interceptor.go`): reads exactly one `authorization` metadata value (`auth.Authorization = "authorization"`, `auth_headers.go`), requires `Bearer <jwt>`, validates the JWT against JWKS of trusted issuers, stores the token in context. Missing/duplicate/invalid -> `Unauthenticated`. Anonymous methods by regex: `^/(osac\.public\.v1\.(Capabilities/|JsonWebKeySet/)|grpc\.(reflection|health)\.).*$` (`anonymousMethodsRegex`, `internal/cmd/service/start/grpcserver/start_grpc_server_cmd.go` L909, not the authn interceptor file).
- **Authz** (`internal/auth/grpc_authz_interceptor.go`, with the claim extraction in `internal/auth/auth_context_extraction.go` L46-64 and the policy evaluation in `internal/auth/authorization_evaluator.go`): builds an `AuthContext` from JWT claims (`preferred_username`/`username`, `organization` claim -> tenants, `realm_access.roles`, groups) plus the request (`id` via `GetId()` or reflection on `object.id`, `grpc_authz_interceptor.go` L312-345), evaluates an embedded **OPA/Rego** policy (`internal/auth/policies/authz.rego`) per full method name; denial -> `PermissionDenied "permission denied"` (L283); policy errors -> `Internal` (L246-295). Then a `Subject{User, Tenants}` goes into context and **tenancy logic** filters/assigns `metadata.tenant` (`docs/AUTH.md`, "Tenancy Logic", "Authorization Levels"). Admins get the universal tenant `*`.
- **Who is the caller**: only the JWT. OSAC has no `x-forwarded-*` identity convention; the only `X-Forwarded-*` header it reads is `X-Forwarded-Proto`, which the UI proxy uses to decide whether cookies are `Secure` (`osac-ui/proxy/auth/cookies.go` L40-45). The UI proxy (`osac-ui/proxy/middleware/auth.go`) strips the incoming `Authorization` header and re-sets `Authorization: Bearer <token-from-cookie-session>`. The REST gateway forwards headers with `runtime.DefaultHeaderMatcher` plus one custom dry-run header.
- Permission introspection for UIs: `SelfSubjectAccessReviews/Create` with `spec.service` (`osac.public.v1.Clusters`) and `spec.method` (`Create`), answering `status.allowed` (`self_subject_access_review_type.proto`).

### Error handling

- Status codes from DAO errors (`fulfillment-service/internal/servers/dao_error_converter.go`): not found -> `NotFound`; duplicate name -> `AlreadyExists`; singleton/"single default" invariant -> `FailedPrecondition`; optimistic-lock conflict and deadlock -> `Aborted`; denied -> `PermissionDenied`; immutable field / validation / invalid filter / bad reference on create -> `InvalidArgument`; bad reference on update/delete, object in use -> `FailedPrecondition`; unknown -> `Internal` with a generic message (details logged). Tenant assignment errors -> `PermissionDenied` (`tenant_errors.go`). Missing `id`/`object` -> `InvalidArgument "object identifier is mandatory"` (37 sites; the shorter `"identifier is mandatory"` appears 3 times) / `"object is mandatory"` (46 sites). Code histogram across `internal/servers` excluding `*_test.go` (reproduced 2026-10-15): InvalidArgument 541, Internal 236, FailedPrecondition 69, NotFound 55, PermissionDenied 18, Aborted 11, AlreadyExists 7, Unimplemented 2. `API.md` says, in "Error responses" (L230-231, at `334a01c`): "Consumers must match on the gRPC status code rather than assuming every uniqueness conflict is `AlreadyExists`.", and in "When not to use protovalidate" (L355-356): "Match on the gRPC status code, not on message text."
- Logging interceptor logs, at **Debug** level only (it returns early otherwise, L135), `method`, the peer `address`, `metadata`, then `duration`, `code` (`grpcstatus.Code(err).String()`) and `error` with `slog` (`internal/logging/logging_interceptor.go` L135-196); with body logging enabled it also adds the `request` and `response` messages as JSON (redacted when redaction is on, L159-171, L186-191).

### Generated code publishing

- `proto/buf.gen.yaml`: `managed.enabled: true`, `go_package_prefix github.com/osac-project/osac/proto/gen`, remote plugins pinned (`buf.build/protocolbuffers/go:v1.36.5` with `default_api_level=API_HYBRID`, `buf.build/grpc/go:v1.5.1`, `buf.build/grpc-ecosystem/gateway:v2.26.1`), `paths=source_relative`, output `gen/`. `proto/Makefile generate` = `dev.py build protos` (private -> public) + `buf generate`; `lint` = `buf lint` with the custom plugin.
- CI: `.github/workflows/check-generated-code.yaml` re-runs `make generate` and fails on any diff under `proto/` ("Generated proto code is out of date"); `.pre-commit-config.yaml` runs `buf lint` on `proto/**/*.proto`. No `buf breaking` job was found in `.github/workflows`; the `breaking: FILE` config exists for local/manual use.
- Publishing: `.github/workflows/publish-proto.yaml` is a **`workflow_call`-only** reusable workflow (inputs `version`, `ref`; no trigger of its own) that runs `buf push --label "<version>"` from the `proto/` module root, which pushes **all three** modules declared in `buf.yaml` (`buf.build/osac-project/public-api`, `private-api` and `tests`). It is invoked by `osac-build-and-publish.yaml` in release mode when the fulfillment-service component is bumped (that workflow's `BUF_TOKEN` secret and `publish-fulfillment-service-proto` job). `docs/RELEASING.md` describes per-component versions as `<component>/vX.Y.Z` git tags (L52-55; e.g. `osac-ui/vX.Y.Z`, L61-62) and throwaway nightly tags `<latest-real-base>-nightly.<date>.<sha>.<run>.<attempt>` (L75-76). Go consumers inside the monorepo use `go.work` + `replace`; the UI consumes the BSR module at a pinned version (`osac-ui/libs/types/.buf-api-version` = `v0.0.118`, `buf.gen.yaml.template` with `buf.build/bufbuild/es:v2.12.0`), and `.github/workflows/nightly-ui-buf-compatibility.yaml` nightly checks for a newer BSR label, regenerates TS, runs typecheck/lint/test/build, and opens an auto-merged PR.

### Kubernetes-backed objects: own model, not CRD mirrors

- OSAC defines its own `Cluster` proto (template/catalog refs, node sets, conditions) and stores it in Postgres; the fulfillment service then creates a `ClusterOrder` CRD on a hub (`osac-operator/api/v1alpha1/clusterorder_types.go`, with `metav1.Condition` conditions and a `Phase` enum) and a controller syncs status back into `Cluster.status` (`docs/architecture/cluster-fulfillment.md`). Hubs are themselves API objects (`private/v1/hub_type.proto`: kubeconfig, namespace). So OSAC does **not** expose CRDs 1:1; `ai-grid-backend` does (it proxies OCM `ManagedCluster`). That difference is legitimate: AI Grid manages objects ACM already owns, so mirroring the CRD fields the console needs (with proto-native types) is the right call, as long as raw unstructured JSON is never exposed.

## 5. Style guide for new `ai_grid.v1` RPCs (proposal)

Applies to both the real backend and the console's fake. Where it differs from OSAC, the reason is given.

### 5.1 Files, package, options

- Package stays `ai_grid.v1`; `go_package` stays `github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1`. Never introduce `v2` for additive work.
- Split by type like OSAC: `proto/ai_grid/v1/<type>_type.proto` + `<type>s_service.proto`; shared `metadata_type.proto`, `condition_type.proto`. `ai_grid.proto` keeps `AiGrid` (Ping, GetCurrentUser) and nothing else. Moving the existing messages between files keeps the Go type names but changes the generated `File_*` descriptor variable, the `source:` header, the raw descriptor bytes and the internal `file_<path>_proto_*` symbols, and `buf breaking` with the `FILE` category flags the move (see 5.8).
- Every service, method, message, field and enum value gets a doc comment written for API consumers (OSAC `API.md`, "Documentation").
- Add `buf.yaml` (`version: v2`, `lint: use: [STANDARD], except: [SERVICE_SUFFIX]`, `breaking: use: [FILE]`), `buf.gen.yaml` (pinned remote plugins `buf.build/protocolbuffers/go` and `buf.build/grpc/go`, `paths=source_relative`, `out: gen`), and `make generate` / `make lint-proto`. Keep `gen/` committed and add a CI job that fails on diff after `buf generate` (copy of OSAC `check-generated-code.yaml`) plus `buf breaking --against '.git#branch=main'`.
- Use `buf.validate` annotations for input constraints once a protovalidate interceptor exists; until then document constraints in comments and validate in handlers with `InvalidArgument`.

### 5.2 Naming

- Services: plural noun, no `Service` suffix: `ManagedClusters`, `Tenants`, `Models`, `CatalogItems`, `Subscriptions`, `Grid` (singleton, like OSAC `Capabilities`).
- Methods: keep the backend's shipped `<Verb><Noun>` form (`ListManagedClusters`, `GetManagedCluster`, `UpdateManagedCluster`, `ListTenants`, `CreateTenant`...). Reason: `ListManagedClusters`/`ListTenants` are already consumed by the console (`ai-grid-provider-console/bff/internal/api/clusters.go`, `tenants.go`); renaming would be a breaking change for no functional gain. Verbs allowed: `List`, `Get`, `Create`, `Update`, `Delete`, and `Get<Noun>` for read-only singletons (`GetCurrentUser`, `GetOverview`). No imperative verbs; model actions as spec fields (OSAC rule). If the backend team prefers OSAC's bare `List/Get` form, the only cheap moment is now (one consumer); see open question 1.
- Request/response: `<Method>Request` / `<Method>Response` (consistent with the three existing ones).
- Messages `CamelCase`, fields `snake_case`, enums `<EnumName>_UPPER_SNAKE` with `_UNSPECIFIED = 0` first.

### 5.3 Request and response shapes (OSAC shapes, adapted)

- **List**: request `optional int32 offset = 1; optional int32 limit = 2; optional string filter = 3; optional string order = 4;`. Response `int32 size; int32 total; repeated X items`. The two shipped List responses keep `clusters`/`tenants` (documented exception) and gain `size`/`total` additively; their requests gain the four optional fields. `limit` omitted: OSAC applies a server default page of **100** (`generic_dao.go` `defaultLimit: 100`); the backend today returns everything. Pick one explicitly: either adopt a default page (recommended, e.g. 100, matching OSAC) or document "omitted = all" as a deliberate deviation; the fake must implement the same choice. `limit = 0` = count only; negative -> `InvalidArgument`.
- **Filter** is a CEL expression over `this`, same documented subset as OSAC `FILTER.md`, evaluated in memory by the backend (hub lists are small). Phase 1 may support only `this.name`, `this.labels[...]`, `in`, `startsWith/endsWith/contains`, `&& || !`; unsupported constructs -> `InvalidArgument`. Do not add ad-hoc `label_selector` strings (OSAC "No structured strings"); a Kubernetes label selector can be derived from simple CEL equality terms server-side.
- **Get**: request `string name = 1` for hub-owned, cluster-scoped objects (`ManagedCluster`) because the Kubernetes name is the identity; `string id = 1` for Keycloak-owned objects (`Tenant`, UUID). Response `X object = 1`.
- **Create**: request `X object = 1`; response `X object = 1`.
- **Update**: request `X object = 1; google.protobuf.FieldMask update_mask = 2; bool lock = 3;`. The mask names fields of `object` (`labels`, `annotations`, `labels.ai-grid/enabled` is NOT a valid path; map entries are replaced as a whole per OSAC/FieldMask semantics; see open question 5). `lock = true` requires `object.resource_version` (hub) to match, else `Aborted`. For Keycloak-backed `Tenant` there is **no server-side version** (Organizations have no version, etag or `Last-Modified`), so `lock` cannot be honoured; see 6.6 for the recommendation (remove `version`/`lock` from the tenant messages, or document them as unsupported and return `InvalidArgument` when `lock=true`). Server applies the mask to the stored object, validates the merged result, and for the hub issues a JSON merge patch (or server-side apply) scoped to the masked fields.
- **Delete**: request `string name|id = 1`; response empty message.
- No other fields in request/response messages (OSAC rule). Cross-cutting needs (dry-run, etc.) go in metadata or a generic field adopted everywhere.

### 5.4 Object shape

- Kubernetes-owned objects (`ManagedCluster`, later `Model` over Placement/ManifestWorkReplicaSet, `CatalogItem` over ConfigMap, `Subscription` over MaaSSubscription) mirror the CRD: identity `name`, `labels`, `annotations`, `uid`, `resource_version`, `generation`, `creation_timestamp`, `deletion_timestamp`, `finalizers`, a `spec`-equivalent (for `ManagedCluster` the shipped flat `hub_accepts_client`), and a `status` message with `conditions` and typed status fields. New objects use the OSAC shape exactly (`name/metadata/spec/status`, `ObjectMeta`-like `metadata`); `ManagedCluster` stays flat at fields 1-3 for compatibility and grows additively (section 6).
- Never expose `unstructured` JSON, `google.protobuf.Struct` or opaque YAML strings for object bodies; map the fields the console needs.
- `spec` user-owned, `status` system-owned; `annotations` are opaque to the backend except the documented `ai-grid/*` keys the console writes (site-name, latitude, longitude) — those are still annotations on the hub object, so the proto exposes them as the `annotations` map, and the BFF derives typed JSON. If the backend ever reads them for decisions, promote them to spec fields (OSAC rule).

### 5.5 Conditions

Shared types in `condition_type.proto`:

```protobuf
enum ConditionStatus { CONDITION_STATUS_UNSPECIFIED = 0; CONDITION_STATUS_TRUE = 1; CONDITION_STATUS_FALSE = 2; }
message Condition {
  string type = 1;                                  // Kubernetes condition type verbatim, e.g. "ManagedClusterConditionAvailable"
  ConditionStatus status = 2;                       // Kubernetes "Unknown" -> CONDITION_STATUS_UNSPECIFIED (OSAC mapping)
  google.protobuf.Timestamp last_transition_time = 3;
  optional string reason = 4;
  optional string message = 5;
  int64 observed_generation = 6;                    // metav1.Condition field; 0 when absent
}
```
`type` is a string (not an enum) because OCM/ACM define and extend condition types outside this API; OSAC's `UserCondition.type` is also a plain string, though its proto states no reason. Well-known type names are documented as constants in comments. Each `*Status` also carries a summarising enum `state` where the console needs one (`ManagedClusterState` in `proto/ai_grid/draft/v1/managed_cluster_type.proto`, which has values 0-9; see 6.4).

### 5.6 Identity metadata (what the console sends, what the backend must do)

The console sets, per RPC (`ai-grid-provider-console/bff/internal/backend/client.go`): `authorization: Bearer <token>`, `x-forwarded-user`, `x-forwarded-email`, `x-forwarded-groups` (comma-joined).

Proposed contract:

| Key | Status | Backend behaviour |
|---|---|---|
| `authorization` | **Required, authoritative** | Exactly one value, `Bearer <token>` (OSAC authn rules). The token is an OpenShift OAuth token today or a Keycloak JWT later. The backend MUST derive the caller from it: build the hub `rest.Config` per request with `BearerToken` (act as the caller) so hub RBAC decides, or validate it (TokenReview / JWT) and run its own policy. Missing/malformed -> `Unauthenticated`. |
| `x-forwarded-user`, `x-forwarded-email`, `x-forwarded-groups` | Optional, **advisory** | For logging, audit fields, `GetCurrentUser` display and the Keycloak `creator` attribute only. MUST NOT grant permissions: anyone who can reach the backend port could set them. If the backend verifies the token itself, it should prefer the verified username over the header and log a mismatch. |
| Anonymous methods | | `grpc.health.*`, `grpc.reflection.*`, `ai_grid.v1.AiGrid/Ping` (the console's readiness probe calls Ping without identity). Everything else requires `authorization`. Regex form as in OSAC: `^/(ai_grid\.v1\.AiGrid/Ping|grpc\.(reflection|health)\.).*$`. |

Authorization model for AI Grid (proposal, mirrors how the console already defines `isProviderAdmin` in `ai-grid-provider-console/bff/internal/hub/hub.go`): "provider admin" = may `patch` `managedclusters.cluster.open-cluster-management.io` on the hub, evaluated with the caller's token via `SelfSubjectAccessReview`. Read RPCs require `list/get` on the underlying resource with the caller's token; mutating RPCs require the corresponding verb. Keycloak RPCs use the backend's service credential but are gated by the provider-admin check first (as `ai-grid-provider-console/docs/plan/work-breakdown.md` already states). Hub `401` -> `Unauthenticated`; hub `403` -> `PermissionDenied`.

### 5.7 Error code mapping

| Situation | gRPC code | Notes / OSAC precedent |
|---|---|---|
| Missing/invalid bearer token; hub or Keycloak says 401 for the caller's token | `Unauthenticated` | OSAC authn interceptor |
| Hub RBAC denies the caller (403), provider-admin check fails for a Keycloak mutation | `PermissionDenied` | OSAC authz; `tenant_errors.go` |
| Object not found (hub 404, Keycloak 404), unknown name/id | `NotFound` | `dao_error_converter.go`. Message `object with identifier '<x>' not found`. |
| Create of an existing name (hub 409 AlreadyExists, Keycloak 409 on org name/alias) | `AlreadyExists` | |
| Update conflict (hub 409 on resourceVersion, `lock=true` version mismatch) | `Aborted` (retryable) | OSAC optimistic locking -> `Aborted` |
| Bad input: invalid name, invalid label key, unsupported filter, bad field-mask path, negative limit, mutually exclusive fields | `InvalidArgument` | protovalidate + servers |
| Operation valid but system state forbids it: CRD/feature gate missing (e.g. ManifestWorkReplicaSet), hub not accepting the cluster, object in use | `FailedPrecondition` | OSAC invariants / in-use |
| Hub API server unreachable, timeout, 5xx, 429 | `Unavailable` (429 -> `ResourceExhausted`) | Console maps to 503/429 (`ai-grid-provider-console/bff/internal/api/errors.go`) |
| Keycloak unreachable, token endpoint failing, 5xx | `Unavailable` | distinguish in the message: `Keycloak is unavailable` vs `the hub is unavailable` |
| RPC defined but not implemented yet (stubs, fake) | `Unimplemented` | default from `Unimplemented*Server` |
| Anything else | `Internal`, generic message, detail in the log | OSAC: "failed to <op> object" |

Rule: always return `status.Error(...)`/`status.Errorf(...)`; never a bare `error` (today's `fmt.Errorf` surfaces as `Unknown`). Add `google.rpc.BadRequest` field violations to `InvalidArgument` where field paths are known (OSAC does this for references). A small helper `k8sErrorToStatus(err)` using `k8s.io/apimachinery/pkg/api/errors` (`IsNotFound`, `IsAlreadyExists`, `IsConflict`, `IsInvalid`/`IsBadRequest`, `IsForbidden`, `IsUnauthorized`, `IsTimeout`/`IsServerTimeout`/`IsServiceUnavailable`, `IsTooManyRequests`) and `keycloakErrorToStatus(httpStatus)` should live in the backend and be mirrored in the fake. The console's `classify()` already maps all these codes to HTTP.

### 5.8 Versioning and compatibility

- Additive only within `ai_grid.v1`: new services, methods, messages, fields, enum values (append). Never change a field's number or type, never reuse numbers; on removal, `reserved <n>; reserved "<name>";`. Renaming a field is wire-compatible but breaks generated Go; treat as breaking.
- `buf breaking --against` main in CI with the `FILE` category (OSAC setting), so file moves are also flagged — do the type/service file split in one deliberate PR.
- New RPCs land first as declared-but-`Unimplemented`; the console's fake and the BFF must tolerate `Unimplemented` (BFF already maps it to 501).
- Tagging: single repo, so plain semver `vX.Y.Z` Go module tags on the repo root (OSAC's `<component>/vX.Y.Z` only exists because of the monorepo). `v0.y.z` while the API is unstable; bump `y` for any new RPC/message, `z` for implementation-only changes. Console pins by tag in `ai-grid-provider-console/bff/go.mod` (drop the `replace` when the repo moves to `github.com/osac-project` or `ai-grid-cloud`; a GitHub redirect keeps `yuvalturg/...` working but the module path must match the hosting org for `go get` without `replace`).
- Keep `gen/` committed (OSAC does; the console imports only `gen/ai_grid/v1`). Optionally `buf push --label vX.Y.Z` to a BSR module later for TypeScript consumers; not needed while the BFF is the only client.

## 6. Draft proto additions

**Superseded by `proto/ai_grid/draft/v1`.** The console's draft proto in `proto/ai_grid/draft/v1` (see
`docs/draft/proto/README.md`) is authoritative; the drafts below are the research record and must not be used for
field numbers. Where they differ (2026-10-16): the draft's `go_package` is the console's own,
`github.com/ai-grid-cloud/ai-grid-provider-console/bff/gen/ai_grid/v1;aigridv1`, not the backend's;
`AiGrid` (6.3) also has `GetHubCapabilities` (`HubCapabilities{manifest_work_replica_set_enabled = 1,
observability_installed = 2, acm_version = 3}`); 6.4, 6.5 and 6.6 differ as listed under each; the `Models`
placeholder of 6.7 is filled (`model_type.proto`, `models_service.proto`) while `Grid`, `CatalogItems` and
`Subscriptions` stay empty stubs. `make -C bff lint-proto` runs `buf lint` (STANDARD minus `SERVICE_SUFFIX`) on
the draft and passed on 2026-10-16 (buf v1.57.2).

Package/options header for every file:

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";
```

### 6.1 `proto/ai_grid/v1/condition_type.proto`

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";

import "google/protobuf/timestamp.proto";

// Status of a condition. Kubernetes `Unknown` is reported as UNSPECIFIED.
enum ConditionStatus {
  // The system cannot decide whether the object is in the condition.
  CONDITION_STATUS_UNSPECIFIED = 0;
  // The object is in the condition.
  CONDITION_STATUS_TRUE = 1;
  // The object is not in the condition.
  CONDITION_STATUS_FALSE = 2;
}

// One entry of a Kubernetes-style `status.conditions` list.
message Condition {
  // Condition type as defined by the owning controller, for example
  // `ManagedClusterConditionAvailable`, `HubAcceptedManagedCluster`,
  // `ManagedClusterJoined`. Types are open-ended; clients must ignore unknown ones.
  string type = 1;
  // Whether the object is in the condition.
  ConditionStatus status = 2;
  // Last time the condition changed from one status to another.
  google.protobuf.Timestamp last_transition_time = 3;
  // Machine-readable reason in CamelCase, when the controller provides one.
  optional string reason = 4;
  // Human-readable detail, intended for people debugging the object.
  optional string message = 5;
  // Generation of the object the condition was computed for; zero when unknown.
  int64 observed_generation = 6;
}
```

### 6.2 `proto/ai_grid/v1/metadata_type.proto` (for new Kubernetes-backed objects; `ManagedCluster` keeps its flat fields)

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";

import "google/protobuf/timestamp.proto";

// Metadata of an object stored in the hub. Mirrors the subset of Kubernetes
// ObjectMeta that the console needs; the backend never exposes raw manifests.
message ObjectMeta {
  // Namespace of the object. Empty for cluster-scoped kinds.
  string namespace = 1;
  // Server-assigned unique identifier (Kubernetes UID). Output only.
  string uid = 2;
  // Opaque version used for optimistic concurrency (`lock` on Update). Output only.
  string resource_version = 3;
  // Generation of the desired state. Output only.
  int64 generation = 4;
  // Time the object was created. Output only.
  google.protobuf.Timestamp creation_timestamp = 5;
  // Time deletion was requested; unset while the object is live. Output only.
  google.protobuf.Timestamp deletion_timestamp = 6;
  // Kubernetes labels. Keys follow the Kubernetes label-key rules.
  map<string, string> labels = 7;
  // Kubernetes annotations. Opaque to the backend.
  map<string, string> annotations = 8;
  // Finalizers still blocking deletion. Output only.
  repeated string finalizers = 9;
}
```

### 6.3 `proto/ai_grid/v1/ai_grid.proto` (existing file, additive)

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";

// Health and identity of this service.
service AiGrid {
  // Liveness check. Anonymous: it is called by the console's readiness probe
  // without identity metadata.
  rpc Ping(PingRequest) returns (PingResponse);
  // Returns who the caller is, as the backend resolved it from the
  // `authorization` metadata, and the coarse roles the console uses to show
  // or hide actions. The backend still authorizes every RPC; this is a hint.
  rpc GetCurrentUser(GetCurrentUserRequest) returns (GetCurrentUserResponse);
}

message PingRequest { string name = 1; }
message PingResponse { string message = 1; }

message GetCurrentUserRequest {}

message GetCurrentUserResponse {
  CurrentUser user = 1;
}

// The authenticated caller.
message CurrentUser {
  // Username as the identity provider reports it (OpenShift user or Keycloak
  // `preferred_username`). Never taken from `x-forwarded-user` alone.
  string username = 1;
  // E-mail when the identity provider exposes one.
  string email = 2;
  // Group memberships known to the identity provider.
  repeated string groups = 3;
  // True when the caller may `patch` ManagedClusters on the hub, evaluated with
  // the caller's own token through a SelfSubjectAccessReview. Replaces the
  // check the console does itself today.
  bool is_provider_admin = 4;
  // Tenants (Keycloak Organizations) the caller belongs to, when the token is a
  // Keycloak token carrying the `organization` claim. Empty for OpenShift tokens.
  repeated string tenants = 5;
  // Identity provider that issued the token.
  IdentityProviderKind identity_provider = 6;
}

enum IdentityProviderKind {
  IDENTITY_PROVIDER_KIND_UNSPECIFIED = 0;
  IDENTITY_PROVIDER_KIND_OPENSHIFT_OAUTH = 1;
  IDENTITY_PROVIDER_KIND_KEYCLOAK = 2;
}
```

### 6.4 `proto/ai_grid/v1/managed_cluster_type.proto`

Fields 1-3 are the shipped ones and must not move. New fields are appended.

Differences in the authoritative draft (`proto/ai_grid/draft/v1/managed_cluster_type.proto`): `ManagedCluster`
continues past 11 with `repeated Taint taints = 12` (`Taint{key = 1, value = 2, effect = 3,
google.protobuf.Timestamp time_added = 4}`), `int64 generation = 13` and `repeated string finalizers = 14`;
`ManagedClusterStatus` adds `string state_message = 7`; `ManagedClusterState` adds `PENDING_IMPORT = 5`,
`IMPORTING = 6`, `IMPORT_FAILED = 7`, `NOT_ACCEPTED = 8`, `UNKNOWN = 9` (the ACM console's derivation order is
documented on the enum, and `PENDING` is superseded). The sketch below stops at field 11 and five enum values;
do not reuse its numbering.

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";

import "google/protobuf/timestamp.proto";
import "ai_grid/v1/condition_type.proto";

// An Open Cluster Management ManagedCluster as seen on the hub. The shape
// follows the CRD (`cluster.open-cluster-management.io/v1`) so that labels and
// claims written by other ACM tooling appear verbatim.
message ManagedCluster {
  // Name of the ManagedCluster. Cluster-scoped, unique, immutable; the
  // identifier used by Get/Update/Delete.
  string name = 1;
  // Labels on the hub object. The console relies on `ai-grid/enabled`,
  // `ai-grid/provisioned`, `location`, `ai-grid/site`, `ai-grid/region` and
  // ACM's `openshiftVersion`. Mutable through UpdateManagedCluster.
  map<string, string> labels = 2;
  // `spec.hubAcceptsClient`.
  bool hub_accepts_client = 3;

  // Annotations on the hub object. The console writes `ai-grid/site-name`,
  // `ai-grid/latitude`, `ai-grid/longitude`. Mutable through UpdateManagedCluster.
  map<string, string> annotations = 4;
  // Kubernetes UID. Output only.
  string uid = 5;
  // Opaque version for optimistic concurrency (`lock` on Update). Output only.
  string resource_version = 6;
  // Time the object was created. Output only.
  google.protobuf.Timestamp creation_timestamp = 7;
  // Set while the cluster is being detached. Output only.
  google.protobuf.Timestamp deletion_timestamp = 8;
  // `spec.leaseDurationSeconds`.
  int32 lease_duration_seconds = 9;
  // `spec.managedClusterClientConfigs`.
  repeated ClientConfig managed_cluster_client_configs = 10;
  // Observed state. Output only.
  ManagedClusterStatus status = 11;
}

// `spec.managedClusterClientConfigs[]`.
message ClientConfig {
  // URL of the managed cluster's API server.
  string url = 1;
  // CA bundle (PEM) used to verify the API server. May be empty.
  bytes ca_bundle = 2;
}

// Summary of the cluster's availability, derived from the
// `ManagedClusterConditionAvailable` and `HubAcceptedManagedCluster` conditions.
enum ManagedClusterState {
  MANAGED_CLUSTER_STATE_UNSPECIFIED = 0;
  // Accepted by the hub but the agent has not reported availability yet.
  MANAGED_CLUSTER_STATE_PENDING = 1;
  // `ManagedClusterConditionAvailable` is True.
  MANAGED_CLUSTER_STATE_AVAILABLE = 2;
  // `ManagedClusterConditionAvailable` is False.
  MANAGED_CLUSTER_STATE_UNAVAILABLE = 3;
  // `deletion_timestamp` is set.
  MANAGED_CLUSTER_STATE_DETACHING = 4;
}

// `status` of the ManagedCluster.
message ManagedClusterStatus {
  // Summary state derived from the conditions.
  ManagedClusterState state = 1;
  // `status.conditions`, verbatim.
  repeated Condition conditions = 2;
  // `status.version.kubernetes`.
  string kubernetes_version = 3;
  // `status.capacity`, as Kubernetes quantity strings keyed by resource name
  // (for example `cpu`, `memory`, `nvidia.com/gpu`).
  map<string, string> capacity = 4;
  // `status.allocatable`, same encoding as `capacity`.
  map<string, string> allocatable = 5;
  // `status.clusterClaims`. The console reads `consoleurl.cluster.open-cluster-management.io`,
  // `version.openshift.io`, `platform.open-cluster-management.io`, `product.open-cluster-management.io`.
  repeated ClusterClaim cluster_claims = 6;
}

// `status.clusterClaims[]`.
message ClusterClaim {
  string name = 1;
  string value = 2;
}
```

Rationale for strings in `capacity`/`allocatable`: Kubernetes quantities (`16`, `64Gi`, `500m`) have no lossless numeric proto type; the BFF/UI parse them. OSAC's "No structured strings" rule allows well-known formats as plain strings but names only IP addresses, CIDRs and URLs (`API.md` L809-816); Kubernetes quantities are not in that list, so this is an argued extension of the rule, not something OSAC states.

### 6.5 `proto/ai_grid/v1/managed_clusters_service.proto`

The authoritative draft (`proto/ai_grid/draft/v1/managed_clusters_service.proto`) has the same three RPCs and
message numbers. Its filter comment points to OSAC's `fulfillment-service/docs/FILTER.md` (a CEL subset) and notes
that the fake answers `InvalidArgument` for any filter or order.

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";

import "google/protobuf/field_mask.proto";
import "ai_grid/v1/managed_cluster_type.proto";

// Read and enrol ManagedClusters on the hub. Every RPC acts with the caller's
// token; the hub's RBAC decides.
service ManagedClusters {
  // Lists ManagedClusters the caller may list.
  rpc ListManagedClusters(ListManagedClustersRequest) returns (ListManagedClustersResponse);
  // Returns one ManagedCluster by name. NotFound when absent or not visible.
  rpc GetManagedCluster(GetManagedClusterRequest) returns (GetManagedClusterResponse);
  // Updates the mutable fields of a ManagedCluster: `labels`, `annotations`,
  // `hub_accepts_client`. Any other path in `update_mask` is InvalidArgument.
  // Requires `patch` on managedclusters for the caller.
  rpc UpdateManagedCluster(UpdateManagedClusterRequest) returns (UpdateManagedClusterResponse);
}

message ListManagedClustersRequest {
  // Zero-based index of the first result. Default zero.
  optional int32 offset = 1;
  // Maximum number of results. Omitted means all; zero means count only;
  // negative is InvalidArgument.
  optional int32 limit = 2;
  // CEL expression over `this` (a ManagedCluster) selecting the clusters to
  // return, for example `this.labels["ai-grid/enabled"] == "true"`. Empty
  // means all. See docs/FILTER.md for the supported subset.
  optional string filter = 3;
  // Sort order, `<field> [asc|desc]`, for example `name desc`. Default: by name.
  optional string order = 4;
}

message ListManagedClustersResponse {
  // Clusters returned. Keeps its historical name instead of `items`.
  repeated ManagedCluster clusters = 1;
  // Number of clusters returned.
  int32 size = 2;
  // Number of clusters matching the filter regardless of paging.
  int32 total = 3;
}

message GetManagedClusterRequest {
  // Name of the ManagedCluster.
  string name = 1;
}

message GetManagedClusterResponse {
  ManagedCluster object = 1;
}

message UpdateManagedClusterRequest {
  // The cluster with the new values. `name` identifies it.
  ManagedCluster object = 1;
  // Paths to update. Supported: `labels`, `annotations`, `hub_accepts_client`.
  // A map path replaces the whole map; to change one key, send the full map
  // as returned by Get. (Alternative under discussion: per-key paths such
  // as `labels.ai-grid/enabled`; see open questions.)
  google.protobuf.FieldMask update_mask = 2;
  // When true, the update fails with Aborted unless `object.resource_version`
  // matches the hub's current value.
  bool lock = 3;
}

message UpdateManagedClusterResponse {
  ManagedCluster object = 1;
}
```

### 6.6 `proto/ai_grid/v1/tenant_type.proto` and `tenants_service.proto`

This was the single tenant proto draft of the research (the page-token sketch that used to live in `keycloak.md` 8.1 was removed). The authoritative version is now `proto/ai_grid/draft/v1/tenant_type.proto` and `tenants_service.proto`, which differ from the sketch below: `Tenant` has `reserved 8; reserved "version";` and `UpdateTenantRequest` has `reserved 3; reserved "lock";` (Keycloak has no server-side version, `keycloak.md` 8.1); `enabled = 5` is `optional bool` (unset on create means Keycloak's default, true); `repeated TenantDomain domain_details = 9` was added with `TenantDomain{name = 1, verified = 2, identity_provider_alias = 3, auto_redirect = 4}`, and `domains` stays the list of names; `UpdateTenantRequest.update_mask` also accepts `domain_details`; `ListTenantsRequest.filter` is narrowed to the one form that pushes down to Keycloak's `search`, `this.name.lowerAscii().contains("<term>") || this.domains.exists(d, d.lowerAscii().contains("<term>"))` (case-insensitive; `lowerAscii()` is cel-go's strings extension). `Tenant` adds `string resource_version = 10` (an opaque version the backend keeps in the organization attribute `ai-grid/resource-version`; `UpdateTenantRequest` compares `object.resource_version` instead of `lock`, Aborted on mismatch), `DeleteTenantRequest` adds `string resource_version = 2`, and `ListTenantsResponse.total` is `optional`. Do not reuse `Tenant` fields 8 and 10 or `UpdateTenantRequest` field 3 from this sketch. Keeps the shipped flat `Tenant{id, name, domains}` with field numbers 1-3 frozen and grows it additively (OSAC permits flat objects; `Tenant` in OSAC also has no `status` and carries `// buf:lint:ignore OSAC_OBJECT_SHAPE`).

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";

// (no imports: a `google/protobuf/timestamp.proto` import was removed because it
// was unused, which fails buf STANDARD lint `IMPORT_USED`)

// A consumer organisation. Backed by a Keycloak Organization in the AI Grid realm.
message Tenant {
  // Keycloak Organization id (UUID). Output only; the identifier for Get/Update/Delete.
  string id = 1;
  // Organization name. Required on create, unique within the realm (length limit
  // not verified against Keycloak's schema).
  string name = 2;
  // E-mail domains owned by the tenant, unique across tenants. Keycloak lower-cases
  // them and also accepts one wildcard form `*.base.tld` (see keycloak.md 5.7).
  repeated string domains = 3;
  // URL-safe alias (no whitespace or RFC 3986 reserved chars). Defaults to `name`
  // on create. Immutable.
  string alias = 4;
  // Whether members can log in. Default true on create.
  bool enabled = 5;
  // Free-text description shown in the console.
  string description = 6;
  // Keycloak Organization attributes. Keycloak stores `map<string, list<string>>`;
  // this field carries the first value of each key (lossy; see keycloak.md
  // open question 4).
  map<string, string> attributes = 7;
  // CAVEAT: Keycloak Organizations have no server-side version, etag or
  // Last-Modified, so a Keycloak-backed backend cannot populate this or honour
  // `lock`. Recommended: remove `version` and `UpdateTenantRequest.lock` before
  // the first tagged release, or keep them documented as unsupported and return
  // InvalidArgument when `lock = true`.
  int32 version = 8;
}
```

```protobuf
syntax = "proto3";
package ai_grid.v1;
option go_package = "github.com/osac-project/ai-grid-backend/gen/ai_grid/v1;aigridv1";

import "google/protobuf/field_mask.proto";
import "ai_grid/v1/tenant_type.proto";

// Tenants are Keycloak Organizations in the AI Grid realm. Mutations require
// the caller to be a provider admin; the backend then uses its own Keycloak
// service credential.
service Tenants {
  rpc ListTenants(ListTenantsRequest) returns (ListTenantsResponse);
  rpc GetTenant(GetTenantRequest) returns (GetTenantResponse);
  rpc CreateTenant(CreateTenantRequest) returns (CreateTenantResponse);
  rpc UpdateTenant(UpdateTenantRequest) returns (UpdateTenantResponse);
  rpc DeleteTenant(DeleteTenantRequest) returns (DeleteTenantResponse);
}

message ListTenantsRequest {
  optional int32 offset = 1;
  optional int32 limit = 2;
  // CEL over `this` (a Tenant), for example `this.name.startsWith("acme")`.
  optional string filter = 3;
  optional string order = 4;
}

message ListTenantsResponse {
  // Keeps its historical name instead of `items`.
  repeated Tenant tenants = 1;
  int32 size = 2;
  int32 total = 3;
}

message GetTenantRequest    { string id = 1; }
message GetTenantResponse   { Tenant object = 1; }

message CreateTenantRequest {
  // `id` and `version` must be empty; `name` is required.
  Tenant object = 1;
}
message CreateTenantResponse { Tenant object = 1; }

message UpdateTenantRequest {
  Tenant object = 1;
  // Supported paths: `name`, `domains`, `enabled`, `description`, `attributes`.
  // `alias` is immutable (Keycloak returns 400 "Cannot change the alias").
  google.protobuf.FieldMask update_mask = 2;
  // CAVEAT: cannot be honoured against Keycloak (no server-side version). Either
  // remove this field together with `Tenant.version`, or return InvalidArgument
  // when true. Kept here only so the shape mirrors UpdateManagedClusterRequest.
  bool lock = 3;
}
message UpdateTenantResponse { Tenant object = 1; }

message DeleteTenantRequest  { string id = 1; }
message DeleteTenantResponse {}
```

Keycloak mapping for this service (details and evidence in `keycloak.md` sections 3, 5 and 8.1):

- `ListTenants`: `offset`/`limit` -> `first`/`max` on `GET /organizations`; `total` from a second call to `GET /organizations/count` with the same filters; results ordered by name. `filter` (CEL) is evaluated by the backend; simple name/domain equality or contains terms can be pushed down as `search`/`exact`.
- `GetTenant`: `GET /organizations/{id}` (full representation). 404 -> `NotFound`.
- `CreateTenant`: `POST /organizations` (201, empty body), take the id from `Location`, then `GET /organizations/{id}` and return the object. 409 -> `AlreadyExists` with Keycloak's `errorMessage`; 400 -> `InvalidArgument`.
- `UpdateTenant`: `GET /organizations/{id}`, apply `update_mask` to the full representation, `PUT /organizations/{id}` (204), return the merged object. Never send a different `alias`. Note Keycloak's semantics: omitted `attributes`/`domains` stay unchanged, `{}`/`[]` clear them.
- `DeleteTenant`: `DELETE /organizations/{id}` (204). Unmanaged members remain realm users.
- Serialisation: Keycloak omits unset `description`/`redirectUrl` and empty `domains` (`NON_NULL`); the backend must treat absent keys as empty.

### 6.7 Placeholders (names agreed now, fields later, all `Unimplemented` until agreed)

Empty request/response messages are deliberate: adding fields later is additive, and the fake can already register the services so the drift test (section 7) covers them. In the authoritative draft `Models` is no longer a placeholder (see the note at the top of section 6); the other three are, and the fake's probe test lists their methods in `intentionallyUnimplemented` (`internal/fakeinfra/draft/fakebackend_test.go`).

```protobuf
// proto/ai_grid/v1/grid_service.proto
service Grid {
  // Fleet overview: request rate, error rate, latency, GPU utilisation per site
  // over a range; `metrics_source` tells whether data is live or demo.
  rpc GetOverview(GetOverviewRequest) returns (GetOverviewResponse);
  // Grid network topology from the gateway CRDs, when installed.
  rpc GetNetwork(GetNetworkRequest) returns (GetNetworkResponse);
}
message GetOverviewRequest {}   // to add: range enum, site filter, bool include_metrics
message GetOverviewResponse {}  // to add: sites, per-site metrics, MetricsSource metrics_source
message GetNetworkRequest {}
message GetNetworkResponse {}

// proto/ai_grid/v1/models_service.proto  (Placement + ManifestWorkReplicaSet on the hub)
service Models {
  rpc ListModels(ListModelsRequest) returns (ListModelsResponse);
  rpc GetModel(GetModelRequest) returns (GetModelResponse);
  rpc CreateModel(CreateModelRequest) returns (CreateModelResponse);
  rpc UpdateModel(UpdateModelRequest) returns (UpdateModelResponse);
  rpc DeleteModel(DeleteModelRequest) returns (DeleteModelResponse);
}
message ListModelsRequest {}  message ListModelsResponse {}
message GetModelRequest {}    message GetModelResponse {}
message CreateModelRequest {} message CreateModelResponse {}
message UpdateModelRequest {} message UpdateModelResponse {}
message DeleteModelRequest {} message DeleteModelResponse {}

// proto/ai_grid/v1/catalog_items_service.proto  (ConfigMaps on the hub)
service CatalogItems {
  rpc ListCatalogItems(ListCatalogItemsRequest) returns (ListCatalogItemsResponse);
  rpc GetCatalogItem(GetCatalogItemRequest) returns (GetCatalogItemResponse);
  rpc CreateCatalogItem(CreateCatalogItemRequest) returns (CreateCatalogItemResponse);
  rpc UpdateCatalogItem(UpdateCatalogItemRequest) returns (UpdateCatalogItemResponse);
  rpc DeleteCatalogItem(DeleteCatalogItemRequest) returns (DeleteCatalogItemResponse);
}
// ...matching empty request/response messages...

// proto/ai_grid/v1/subscriptions_service.proto  (MaaSSubscription on the hub)
service Subscriptions {
  rpc ListSubscriptions(ListSubscriptionsRequest) returns (ListSubscriptionsResponse);
  rpc GetSubscription(GetSubscriptionRequest) returns (GetSubscriptionResponse);
  rpc CreateSubscription(CreateSubscriptionRequest) returns (CreateSubscriptionResponse);
  rpc UpdateSubscription(UpdateSubscriptionRequest) returns (UpdateSubscriptionResponse);
  rpc DeleteSubscription(DeleteSubscriptionRequest) returns (DeleteSubscriptionResponse);
}
// ...matching empty request/response messages...
```

When filled in, `Model`, `CatalogItem`, `Subscription` use `{ string name = 1; ObjectMeta metadata = 2; XSpec spec = 3; XStatus status = 4; }` with `Condition` lists, and their List responses use `items`. `CatalogItems` is the OSAC-style plural of `CatalogItem` (OSAC has `ClusterCatalogItems`); the console's `/api/v1/catalog` path is unaffected.

## 7. Workflow between the two repositories

1. **Proto PR first, in the backend repo.** Any new RPC starts as a PR touching only `proto/`, `gen/`, and (automatically, via the embedded `Unimplemented*Server`s in `internal/ai_grid/service.go`) nothing else: the server compiles and answers `Unimplemented`. The PR runs `buf lint`, `buf breaking --against main`, `buf generate` + diff check, `go build ./...`, `go test ./...`. Reviewers from both teams approve the shape. This mirrors OSAC's "commit source + generated code, CI fails if stale" (`check-generated-code.yaml`).
2. **Tag.** On merge, tag `vX.Y.Z` (API addition -> minor). Later, implementation PRs ship under patch tags. Optionally a GitHub Release with the proto diff.
3. **Console bumps.** `go get github.com/osac-project/ai-grid-backend@vX.Y.Z` in `ai-grid-provider-console/bff/` (remove the `replace` once the repo is under the module's org), then: the fake (`ai-grid-provider-console/bff/internal/backend/backendtest`, and the planned standalone fake) implements the method or keeps it `Unimplemented`; the BFF handler lands; `ai-grid-provider-console/docs/plan/bff-api.md` moves the row from "Waiting" to "Implemented".
4. **Drift test in the console** (implemented since: `TestFakeImplementsEveryProtoMethod` in `internal/fakeinfra/draft` calls every method of every draft service and fails on an `Unimplemented` answer unless the method is listed in `intentionallyUnimplemented`, and `ai-grid-provider-console/bff/internal/protodrift` checks the draft against the vendored backend proto; see `docs/draft/proto/README.md`; the original proposal follows): a Go test that starts the fake server and asserts that `srv.GetServiceInfo()` contains every service and every method listed in the generated file descriptors (`aigridv1.File_ai_grid_v1_*_proto.Services()` via `protoregistry.GlobalFiles.RangeFilesByPackage("ai_grid.v1", ...)`), i.e. the fake registers every service the proto declares; and, per method, a table marks it `implemented` or `unimplemented` so adding a method to the proto without a decision in the fake fails the test. A second assertion compares the console's `backend.Client` fields with the registered services so a new service cannot be forgotten in `Dial`.
5. **Nightly drift build in the console** (implemented since as `ai-grid-provider-console/.github/workflows/drift.yaml`, which syncs the backend's `main` proto into a throwaway checkout and runs the drift test; the original proposal, a copy of OSAC's `nightly-ui-buf-compatibility.yaml` idea, follows): a scheduled workflow that runs `go get github.com/osac-project/ai-grid-backend@main && go build ./... && go test ./...` in `ai-grid-provider-console/bff/` on a throwaway branch, and opens/updates a PR or an issue when it fails or when a new tag exists. The backend repo gets the reverse guard only if cheap: a job that checks out the console at `main` with `replace` pointing at the PR's checkout and runs `go build ./bff/...`.
6. **Fake stays faithful to error semantics.** The fake uses the same `k8sErrorToStatus` table (section 5.7); fixture data lives in Go, not YAML, so it compiles against the generated types and breaks loudly on changes.
7. **Metadata contract test.** The fake records incoming metadata (already in `backendtest.Fake.Metadata`); a shared test asserts `authorization` is present and `x-forwarded-*` are optional, so both sides agree on the identity contract in section 5.6.

## 8. Open questions for the backend team

1. **Method-name style.** Keep `ListManagedClusters` (shipped, AIP-style) or move to OSAC's bare `ManagedClusters/List` + `{Service}{Method}Request` now, while the console is the only consumer? This spike drafts the former; the latter would be a one-time breaking release `v0.2.0`.
2. **Repository home and module path.** Will the repo move to `github.com/osac-project/ai-grid-backend` (matching `go.mod`) or to the `ai-grid-cloud` org (then `go.mod` must change to drop the `replace`)? Will it use plain `vX.Y.Z` tags?
3. **Acting as the caller.** Will the backend build the hub client from the `authorization` bearer per request (so hub RBAC decides and the console's ClusterRole can go), validate OpenShift tokens via TokenReview, or expect Keycloak JWTs with JWKS validation like OSAC? This decides what `GetCurrentUser.is_provider_admin` means and whether `x-forwarded-*` can stay advisory.
4. **Keycloak credential.** Password grant on `admin-cli` today; move to a confidential client/service account with `query-organizations`/`manage-organizations` as the README suggests? Which roles do `CreateTenant`/`DeleteTenant` need?
5. **Field-mask semantics for maps.** Whole-map replacement (OSAC/FieldMask default) or per-key paths (`labels.ai-grid/enabled`) so the console can toggle one label without read-modify-write? Per-key paths need a documented escaping rule for `/` and `.` in label keys.
6. **Identifier for `ManagedCluster`.** `name` (proposed; cluster-scoped and immutable) vs a synthetic `id` (OSAC style, would be the UID). The console routes by name today.
7. **Empty list vs `FailedPrecondition` when the OCM CRD is missing.** Today `IsNotFound` on the list returns an empty list, hiding a misconfigured hub. Proposal: `FailedPrecondition "the hub does not serve managedclusters.cluster.open-cluster-management.io"`.
8. **Error codes.** Agree the table in section 5.7, in particular `Aborted` for update conflicts and `Unavailable` for both hub and Keycloak outages (with distinguishing messages), and that all errors are `status.Error`.
9. **Tooling.** Adopt `buf` (lint STANDARD minus `SERVICE_SUFFIX`, breaking FILE, pinned remote plugins) and a `check-generated-code` CI job? The current `gen/` was produced with `protoc 3.13.0` and an older `protoc-gen-go-grpc`; regenerating with pinned plugins will churn `gen/` once. Note that the drafts in section 6 use proto3 `optional`, which `protoc 3.13.0` only accepts with `--experimental_allow_proto3_optional`; buf (or protoc >= 3.15) is required. With the buf module rooted at `proto/`, the generated `source:` header also changes from `proto/ai_grid/v1/...` to `ai_grid/v1/...`.
10. **Validation.** Add protovalidate (`buf.validate`) now, as OSAC does, or document constraints and validate by hand until the API settles?
11. **Streaming.** Does the console need a `Watch`? OSAC exposes one `Events/Watch` stream with a CEL filter; the console polls today. Deferring keeps the fake simple.
12. **Grid overview contract.** Which Thanos (ACM observability or user-workload monitoring) and metric names; is `metrics_source: demo` acceptable in the API so the UI can show a banner?
13. **Models/Catalog/Subscriptions ownership.** Are these hub resources (Placement/ManifestWorkReplicaSet, ConfigMap, MaaSSubscription) and should the proto mirror them like `ManagedCluster`, or does the backend want an OSAC-style own model with its own store?
14. **TypeScript consumers.** If the frontend ever talks to the backend directly, publish to a BSR module (`buf push --label vX.Y.Z`) as OSAC does; otherwise the committed Go `gen/` suffices.

## Appendix: files consulted

Backend: `go.mod`, `Makefile`, `Containerfile`, `README.md`, `proto/ai_grid/v1/ai_grid.proto`, `gen/ai_grid/v1/*.pb.go` (headers), `cmd/ai-grid-backend/main.go`, `internal/ai_grid/*.go`, `internal/keycloak/client.go`, `client/client.go`, `integration/kind_test.go`, `deploy/kind/*.yaml`, `hack/*.sh`.

Console: `ai-grid-provider-console/bff/go.mod`, `ai-grid-provider-console/bff/internal/backend/client.go`, `ai-grid-provider-console/bff/internal/backend/backendtest/server.go`, `ai-grid-provider-console/bff/internal/api/{errors,user,clusters,tenants}.go`, `ai-grid-provider-console/bff/internal/hub/hub.go`, `ai-grid-provider-console/bff/internal/identity/identity.go`, `ai-grid-provider-console/docs/plan/bff-api.md`, `ai-grid-provider-console/docs/plan/work-breakdown.md`, `docs/dev-setup.md`, `ai-grid-provider-console/hack/dev-backend.sh`, `ai-grid-provider-console/frontend/src/api/{types,ConsoleService}.ts`.

OSAC monorepo (`github.com/osac-project/osac`): `README.md`, `go.work`, `proto/{AGENTS.md,buf.yaml,buf.gen.yaml,go.mod,Makefile}`, `proto/public/osac/public/v1/{metadata_type,condition_status_type,openapi_options,cluster_type,clusters_service,cluster_common_type,tenant_type,tenants_service,event_type,events_service,self_subject_access_review_type,self_subject_access_review_service,user_type,users_service,field_policy_type,field_definition_type,console_service,capabilities_service,authn_capabilities_type}.proto`, `proto/private/osac/private/v1/{hub_type,hubs_service,metadata_type}.proto`, `fulfillment-service/{go.mod,.golangci.yml,AGENTS.md}`, `fulfillment-service/docs/{API,CLEANAPI,AUTH,FILTER}.md`, `fulfillment-service/internal/auth/{grpc_authn_interceptor,grpc_authz_interceptor,auth_headers}.go`, `fulfillment-service/internal/servers/{dao_error_converter,tenant_errors,generic_server}.go`, `fulfillment-service/internal/cmd/service/start/grpcserver/start_grpc_server_cmd.go`, `fulfillment-service/internal/cmd/service/start/restgateway/start_rest_gateway_cmd.go`, `fulfillment-service/internal/validation/protovalidate_interceptor.go`, `fulfillment-service/internal/logging/logging_interceptor.go`, `fulfillment-service/cmd/buf-plugin-osac-lint/main.go`, `.github/workflows/{check-generated-code,publish-proto,nightly-ui-buf-compatibility}.yaml`, `.github/scripts/check-ui-buf-release.sh`, `.pre-commit-config.yaml`, `docs/{RELEASING,CONVENTIONS}.md`, `docs/architecture/cluster-fulfillment.md`, `osac-operator/api/v1alpha1/clusterorder_types.go`, `osac-ui/libs/types/{buf.gen.yaml.template,.buf-api-version,scripts/gen-types.mjs}`, `osac-ui/proxy/middleware/auth.go`.

Archived: `github.com/osac-project/fulfillment-service` (redirect notice), `github.com/osac-project/fulfillment-api` and `fulfillment-common` (archive notices, 2026-03-04).

## Verification (2026-10-15)

Independently re-checked on 2026-10-15 against the backend checkout at `fb419b3` (and its remote via `gh api`: 0 tags, only `main`, same head), the `osac-project/osac` monorepo at a 2026-10-15 head (all 56 cited OSAC paths and 13 cited console paths exist), the archived repos' metadata via `gh api`, grpc-go `clientconn.go` at `v1.61.0` and `v1.84.0`, and buf's lint rule/category definitions. Corrections applied in this pass: `grpc.DialContext` is deprecated only at the console's grpc version (2); OSAC `limit` omitted = page of 100 (4, 5.3); the interceptor chain ends with the reference validator and the authz claim handling lives in `auth_context_extraction.go`/`authorization_evaluator.go` (4); dominant mandatory-id message and histogram scope (4); `publish-proto` is `workflow_call`-only and pushes three modules (4); `lock`/`version` cannot be honoured for Keycloak tenants (5.3, 6.6); unused timestamp import removed from `tenant_type.proto` and the single tenant draft now lives here, with the Keycloak mapping appended (6.6); proto3 `optional` vs `protoc 3.13.0` (8.9). The drafts in section 6 were checked by reading against buf's STANDARD rules, not by running `buf lint` (`buf`/`protoc` not installed locally).

Not verified in this pass: the logging interceptor's field list (4); the literal `<component>/vX.Y.Z` tag form in `docs/RELEASING.md` (4); `ClusterConditionType` values beyond `READY` (4); Keycloak's `ORG.NAME` length limit (6.6).

Audit (2026-10-16). Re-checked against the `osac-project/osac` monorepo at `334a01c` (2026-10-16; line numbers
above that were added in this audit refer to it) and this branch's `proto/ai_grid/draft/v1`. Corrected: the
quoted `API.md` sentence did not exist; the two real sentences are now quoted with their lines (4, "Error
handling"); `defaultLimit: 100` is at `generic_dao.go` L116 (the brief's L115 does not match this checkout) with
`maxLimit: 1000` at L117; the anonymous-methods regex lives in `start_grpc_server_cmd.go`, not the authn
interceptor; OSAC does read `X-Forwarded-Proto` (UI proxy cookies), so only an `x-forwarded-*` identity
convention is absent; the immutable networking services omit `Update`, so "exactly the five CRUD verbs" is
narrowed; the reason given for `UserCondition.type` being a string was unsourced and is removed (4, 5.5);
Kubernetes quantity strings are not in `API.md`'s list of well-known string formats (6.4). Verified now that were
marked not verified: the logging interceptor's fields (Debug only), the `<component>/vX.Y.Z` tag form in
`RELEASING.md`, `CLUSTER_CONDITION_TYPE_FAILED` (plus `DEGRADED` and two worker values), and `buf lint` (run on the
console's draft with `make -C bff lint-proto`, passing). Sections 6.3-6.7 are marked as the research record: the
authoritative draft is `proto/ai_grid/draft/v1`, and each section lists how it differs (ManagedCluster fields 12-14,
`state_message = 7`, enum values 5-9; Tenant `version`/`lock` reserved, optional `enabled`, `domain_details = 9`, `resource_version` 10,
narrowed filter; `GetHubCapabilities`; filled `Models`). Section 7 items 4 and 5 note what has been implemented.
Still not verified: Keycloak's `ORG.NAME` length limit (6.6).
