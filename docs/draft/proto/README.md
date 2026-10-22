# The draft proto

The console's BFF and the draft fake compile against the console's draft of the next
`ai_grid.v1` proto, kept here as package `ai_grid.draft.v1` (`proto/ai_grid/draft/v1`) until
the backend ships each area in `ai_grid.v1`. This page was written in the console
(`mock/proto/README.md` at `ac80c25`), where the draft was package `ai_grid.v1`; the section
"In this repository" below says what changed with the move. It says what the draft is, what keeps it
honest against the backend's real proto, which methods the fake deliberately leaves
unimplemented, and how a change moves between the two teams. The style it follows and the
shapes it starts from are in `docs/draft/research/backend-conventions.md` sections 5 and 6
(background reading).

## What it is

This backend's real API, `ai_grid.v1`, covers clusters and tenants so far. The console needs more than that, so it carries a
**draft** of the next proto in `proto/ai_grid/draft/v1/`, split by type as the style guide in
`docs/draft/research/backend-conventions.md` section 5 says (`ai_grid.proto`, `condition_type.proto`,
`metadata_type.proto`, `managed_cluster_type.proto`, `managed_clusters_service.proto`,
`tenant_type.proto`, `tenants_service.proto`, `model_type.proto`, `models_service.proto`, and the
`subscriptions_service.proto`, `subscription_type.proto`, and `grid_service.proto`, whose
`Grid/GetNetwork` and the whole of `catalog_items_service.proto` are still placeholders). It is the
console's **proposal** to the backend team, nothing more: the shapes are the ones drafted in
section 6 of that report, every file says so in its header, and the package is the
draft's own (`ai_grid.draft.v1`, `go_package` `gen/ai_grid/draft/v1;aigriddraftv1`), not the
backend's `ai_grid.v1`.

## In this repository

- **Package and paths.** The files are the console's, byte for byte, except for three kinds of
  line: `package ai_grid.draft.v1;`, `option go_package =
  "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1;aigriddraftv1";`,
  and imports of `ai_grid/draft/v1/...`. The package had to change: `ai_grid.v1` here already
  has `managed_cluster_type.proto`, `metadata_type.proto`, `tenant_type.proto` and
  `tenants_service.proto`, and names such as `ai_grid.v1.ManagedCluster` and
  `ai_grid.v1.Tenants`, and protobuf-go panics when two registrations share a file path or a
  full name. Field names, numbers and types are unchanged, so the protojson and the binary
  encoding of every message are the same; the gRPC method paths and the full names (CEL's
  `ai_grid.draft.v1.ModelPhase.MODEL_PHASE_READY`) carry the new package.
  The comments still name the console's paths (`mock/research/...`): here those pages are
  `docs/draft/research/...`.
- **Generation.** `make generate` runs `proto/buf.gen.yaml` for `ai_grid.v1` (which excludes
  `ai_grid/draft`) and then `proto/buf.gen.draft.yaml`, which generates only Go and gRPC
  (`buf.build/protocolbuffers/go`, `buf.build/grpc/go`, the same pinned remote plugins as the
  real API) into `gen/ai_grid/draft/v1`. The draft has no HTTP annotations, so there is no
  grpc-gateway or OpenAPI output and nothing in `docs/openapi`. Without `buf` on `PATH`:
  `make generate BUF='go run github.com/bufbuild/buf/cmd/buf@v1.50.0'`. Run it twice and
  require `git diff --exit-code`, as for the real API.
- **Lint.** The draft is in the same buf module as `ai_grid.v1` (`proto/buf.yaml`) and passes
  its lint configuration (STANDARD minus `PACKAGE_VERSION_SUFFIX` and `SERVICE_SUFFIX`)
  unchanged: `cd proto && buf lint`.
- **No drift test here.** The console's drift test and its vendored copy of the backend's
  proto (`ai-grid-provider-console/bff/internal/protodrift`,
  `ai-grid-provider-console/mock/proto/upstream`) stay in the console. The fake's own guard,
  `TestFakeImplementsEveryProtoMethod`, moved with the fake and runs in `go test ./...` here.
- **Retiring an area.** When the backend implements an area in `ai_grid.v1`, that area's
  messages and methods are deleted from the draft and from `internal/fakeinfra/draft`
  (`docs/draft/README.md`, "When an area moves to the real backend").

What keeps it honest:

- **The backend's three services are in it unchanged.** `AiGrid/Ping`,
  `ManagedClusters/ListManagedClusters` and `Tenants/ListTenants`, with `ManagedCluster`
  fields 1-3 and `Tenant` fields 1-3 byte-for-byte as the backend defines them. New fields
  and methods are only appended.
- **The backend's real file is vendored.** `ai-grid-provider-console/mock/proto/upstream/ai_grid.proto` is a copy of
  `proto/ai_grid/v1/ai_grid.proto` from the backend repository at the commit in
  `ai-grid-provider-console/mock/proto/upstream/COMMIT`. `ai-grid-provider-console/hack/sync-upstream-proto.sh <ref>` refreshes both;
  `ai-grid-provider-console/hack/dev-backend.sh` runs the backend at that commit.
- **A test proves the superset.** `ai-grid-provider-console/bff/internal/protodrift` compiles the vendored file with
  `protocompile` and checks, against the draft's generated descriptors, that every upstream
  service, method (same input and output), message, field (same number, name, cardinality
  and type) and enum value exists. The day the backend changes its proto, the sync script
  plus this test name every difference. `TestFakeImplementsEveryProtoMethod` in
  `internal/fakeinfra/draft` then calls every method of every draft service and fails on an
  `Unimplemented` answer, except for the methods listed in `intentionallyUnimplemented`
  (`fakebackend_test.go`), each with its reason: the placeholder `Grid/GetNetwork` and
  `CatalogItems` methods until they are agreed with the backend team, and `GetCurrentUser`,
  which the BFF does not need yet (`Models` and `GetHubCapabilities` are implemented and
  probed like every other method). An entry whose
  method starts answering fails the test until the entry is removed, so the allowlist is
  always exactly the set of stubs.
- **The allowlist refines `docs/draft/README.md`.** That page says the test fails on any
  `Unimplemented` answer, which held while the fake was built from the backend's three-RPC
  proto. The draft adds services the backend has not agreed yet, so the test now fails on
  any `Unimplemented` answer except from a method in `intentionallyUnimplemented`.
- **Generated code is committed and checked.** In the console, `bff/gen/ai_grid/v1` is
  produced by `make -C bff generate` (`buf generate` with local `protoc-gen-go` and
  `protoc-gen-go-grpc` run through `go run` under the `bff/go.mod` toolchain, because
  protoc-gen-go formats its output with the `go/printer` of the Go that builds it and Go 1.26
  and 1.27 differ on indented lines in field comments; the draft keeps indented lines out of
  field comments for that reason). Here the remote plugins format the output themselves, so
  the local Go version does not matter; see "In this repository" above for `make generate`
  and `buf lint`.

When the backend ships an area of the draft in `ai_grid.v1`, the console switches that
area's calls to the backend's generated `ai_grid.v1` package, and the area is deleted from the
draft and the fake here. When the backend has shipped all of it, the draft
(`proto/ai_grid/draft`, `gen/ai_grid/draft`), the fake (`internal/fakeinfra/draft`) and
`fixtures/draft` are deleted, and `make dev-fake` fakes only the infrastructure.

The Clusters area added, additively, what `docs/draft/research/acm.md`'s richer `ManagedCluster`
proposal needs and the fake now simulates: `taints` (with `time_added`), `generation`,
`finalizers`, `status.state_message`, and the finer `ManagedClusterState` values
(`PENDING_IMPORT`, `IMPORTING`, `IMPORT_FAILED`, `NOT_ACCEPTED`, `UNKNOWN`) with the ACM
console's derivation order documented on the enum. The flattened claim fields and the
`addon_status` map from that proposal were not added: the BFF derives them from
`cluster_claims` and the `feature.open-cluster-management.io/addon-*` labels, which the proto
already carries verbatim.

The Models area filled the placeholder `Models` service (`model_type.proto`,
`models_service.proto`) with `Model { name, metadata, spec, status }`: the spec is the subset
of a KServe `LLMInferenceService` the console writes (`model.uri/name`, `replicas` or
`scaling`, `base_refs`, `runtime`, `router` with gateway refs, a managed route and the
scheduler, the `main` container) plus an ACM `Placement` (one predicate: label selector, claim
expressions, tolerations, `number_of_clusters`, spread constraints) and a `Rollout`
(`All|Progressive|ProgressivePerGroup` with `max_concurrency`, `max_failures`,
`progress_deadline`, `min_success_time`); the status is the LLMInferenceService status of the
hub deployment (`url`, `addresses`, Knative conditions, `applied_configs`, `workloads`,
`observed_generation`), a derived `phase` with its reason and message, and for a placed model
the Placement decisions and conditions plus the ManifestWorkReplicaSet summary, conditions and
one `ManifestWorkStatus` per generated work (conditions, manifests with their conditions and
feedback, generations). The research's `Placements` and `ManifestWorkReplicaSets` services
were not drafted as separate services: the console only ever needs them through a model, so
the `Models` service wraps one Placement and one MWRS per model (`docs/draft/research/acm.md`,
"Proposal"). `AiGrid` gained `GetHubCapabilities` (`manifest_work_replica_set_enabled`,
`observability_installed`, `acm_version`).

The Tenants area first removed two drafted fields: `Tenant.version` (field 8) and
`UpdateTenantRequest.lock` (field 3) are `reserved`, because Keycloak Organizations have no
server-side version, etag or `Last-Modified` (`docs/draft/research/keycloak.md` 8.1). The review of
the fake server PR brought a version back under a new name and number, kept by the backend
itself in an organization attribute (`docs/draft/areas/tenants.md`, "Version"):
`Tenant.resource_version = 10`, checked when `UpdateTenantRequest.object.resource_version` or
the new `DeleteTenantRequest.resource_version = 2` is set (no flag; `lock` stays reserved), and
`Aborted` when stale. `DeleteModelRequest` gained the same precondition (`resource_version =
3`, a Kubernetes delete precondition), and `ListTenantsResponse.total` and
`ListModelsResponse.total` became `optional`, so a backend that ignores paging and filters
(the shipped one) is visible by the missing count. The Tenants area also added `TenantDomain` and
`repeated TenantDomain domain_details = 9` (`name`, `verified`, `identity_provider_alias`,
`auto_redirect`, the fields Keycloak 26.8 keeps per domain), and kept `domains` as the list of
names. `ListTenantsRequest.filter` was first narrowed to the one CEL form that pushes down to
Keycloak's `search`; it is general CEL again, as is `ListModelsRequest.filter`: an expression
over `this` (the message, proto field names) and `now` in cel-go's standard library plus its
strings extension (`ext.Strings()`), which the client writes, the BFF forwards and the backend
evaluates before paging (the fake in memory, `internal/fakeinfra/draft/celfilter`).

## Proposals from the port

The PoC port added the shapes below to the draft. **Each is a proposal for the backend owners**,
implemented in the fake so the console works today and not agreed with anyone: the owners settle
each one, field number included, when the backend work on that area starts. A field's header
comment in the draft says the same. Proposals the port first made and PR 12 later covered
(the tenant version, the delete preconditions) are PR 12's and are described above.

| Id | Proposal | What the fake implements |
|---|---|---|
| P1 | `Tenant.identity_provider = 11`, with the port's `IdentityProvider` and `OidcConfig` messages | Validates the provider with the PoC's rules on create and on an update whose mask names `identity_provider` (`BadRequest` violations under `identityProvider.*`), trims and stores it. An organization without one is accepted. The check runs after the version check, so a stale update is `Aborted` whatever else is wrong |
| P3 | `Tenant.create_time = 12`, output only | Stamps the time at create and keeps it out of `attributes`, as the backend would in the attribute `ai-grid/create-time`; an organization created some other way has none, and the console shows "—" |
| P6 | `ModelSubscriptionRef.namespace = 8` | The namespace of the referenced model; empty means the console's models namespace (`ai-grid-models`). Pairing with auth policies, status and the delete guard all use namespace and name |
| P7 | `Models.ListLocations`, `ListLocationsResponse.locations = 1` | The `location` labels of the clusters that are AI Grid members (`ai-grid/enabled` and `ai-grid/provisioned` both `true`) and Available, unique and sorted: the PoC's eligible locations on PR 12's cluster state |
| P8 | So far `PlacementStatus.locations = 4` | The `location` labels of the decided clusters, unique and sorted; a decided cluster without the label adds nothing |
| P11 | The bodies of `subscriptions_service.proto` and `Grid/GetOverview`, which filled the placeholders | Subscriptions: create, read, update and delete with the CRD's rules and `BadRequest` field violations, `If-Match` as `resource_version` (set means checked, `Aborted` when stale). The overview: the PoC's metric contract with a deterministic demo generator, `range` limited to `1h`, `6h` and `24h`, a metric status per series, and the cluster inventory read from PR 12's cluster state |
| P12 | The MaaS status the research implies (`docs/draft/research/models-maas.md` 8.1). `Subscription.phase = 10` is `Active`, `Degraded`, `Failed` or `Invalid`, and empty until the first reconcile (the controller never writes `Pending`), fields 11 and 12 are `reserved`, and `conditions = 16`, `model_ref_statuses = 17` and `token_rate_limit_statuses = 18` are new. `AuthPolicy` gains `conditions = 7` and `model_refs = 8` (`AuthPolicyModelRef {name, namespace}`, replacing the reserved bare names in 5). New messages `ResourceRefStatus`, `TokenRateLimitStatus` and `ModelRegistration`, and `GetSubscriptionResponse.model_registrations = 4`. All output only | The controller's phases, conditions, reasons and messages, word for word, derived on every read; the timing is in `docs/draft/README.md`, area 4. A model ref with a namespace pairs with a policy that names the same namespace |

P2, P4 and P5 were dropped: PR 12 covers them (`Tenant.resource_version`,
`DeleteTenantRequest.resource_version` and `DeleteModelRequest.resource_version`). P10 (map-key
field masks) was dropped: PR 17 settled on whole maps. P9 (cluster `grid` fields) and the rest
of P8 are not in the draft yet.

## Working with the backend team

1. **Draft changes are PRs here.** A new or changed draft RPC edits `proto/ai_grid/draft/v1`,
   runs `make generate`, and updates `internal/fakeinfra/draft` in the same PR (or lists the
   method in `intentionallyUnimplemented` with its reason). Both teams review the shape.
2. **The real RPC lands in `ai_grid.v1`.** When the backend implements it, it follows
   `docs/API.md` with the draft as the starting proposal, and the area leaves the draft. The
   console's BFF handler and page then switch to the real types. Each step is a small PR.
3. **Stubs are fine in production.** A deployed console in front of a backend that lacks an
   RPC shows "not available yet" on that page and works everywhere else.
4. **Drift is caught nightly, in the console.** `ai-grid-provider-console/.github/workflows/drift.yaml`
   runs every night on a throwaway checkout: `ai-grid-provider-console/hack/sync-upstream-proto.sh main`,
   then the drift test against the console's own copy of the draft. That stays in the console
   until it drops its copy.
5. **The research is the shared source.** When the backend team implements an area, the
   report for that area is the specification both sides test against; disagreements go into
   the report's open questions and are resolved there before code.
