# The console's draft API and its fake

`ai_grid.draft.v1` is the console's proposal for the APIs this backend does not implement
yet: managed cluster reads and edits, tenant CRUD, models, subscriptions, the grid overview
and the catalog. This directory holds what came with it from the console: the research that
says how the real systems behave, a plan per area for keeping the fake faithful, and the
reference for the fake itself. The fixtures are fictional: every name, id, host and number in
them is invented.

To build pages or clients against the fake, start at [AGENTS.md](AGENTS.md): it lists the
endpoints, the tokens, the seeded states, the timed scenarios and the errors you can provoke.
This file explains why the fake exists and how it grows.

## In this repository

These pages, the draft proto and the fake were moved here from the console repository
([ai-grid-cloud/ai-grid-provider-console](https://github.com/ai-grid-cloud/ai-grid-provider-console)
at `ac80c25`), so that the console can drop its copies and the backend team owns the
proposal next to the real API:

| Here | In the console at `ac80c25` |
|---|---|
| `proto/ai_grid/draft/v1/` (package `ai_grid.draft.v1`), generated into `gen/ai_grid/draft/v1/` (`aigriddraftv1`) | `mock/proto/ai_grid/v1/` (package `ai_grid.v1`), `bff/gen/ai_grid/v1/` |
| `internal/fakeinfra/draft/` (with `celfilter/`, `subscription/`, `validation/`) | `bff/internal/fakebackend/`, `bff/internal/subscription/`, `bff/internal/validation/` |
| `cmd/ai-grid-fake-infra --draft-address` (default `127.0.0.1:9094`) | `bff/cmd/fake-backend` (`127.0.0.1:9090`) |
| `fixtures/draft/default/`, `fixtures/draft/poc/` | `mock/fixtures/`, `mock/fixtures-poc/` |
| `internal/fakeinfra/draft/validation/cases.json` | `mock/validation/cases.json` (the console keeps its copy for its frontend tests; the two must stay in sync until the console no longer needs it) |
| `docs/draft/` (this directory; `fixtures/default.md` and `fixtures/poc.md` are the fixture READMEs) | `mock/*.md`, `mock/areas/`, `mock/research/`, `mock/proto/README.md`, `mock/fixtures*/README.md` |

The draft's package is `ai_grid.draft.v1`, not `ai_grid.v1`: this backend's `ai_grid.v1`
already has files at the same paths (`managed_cluster_type.proto`, `metadata_type.proto`,
`tenant_type.proto`, `tenants_service.proto`) and messages with the same names, and
protobuf-go refuses to link two registrations of one name. Every message, field, number and
type is otherwise the console's (only the file headers and two comments about the console's drift
test were rewritten for this repository), so the protojson and the field encoding are unchanged;
the gRPC method paths (`/ai_grid.draft.v1.Models/ListModels`) and the qualified names in CEL
filters (`ai_grid.draft.v1.ModelPhase.MODEL_PHASE_READY`) carry the new package. The pages
below were written in the console, where the draft was "`ai_grid.v1`"; read that as
`ai_grid.draft.v1` wherever it means the draft. Paths starting with `ai-grid-provider-console/`
are in the console repository, and `research/` is a dated record of what the research found,
left as it was written.

Three rules hold while the draft lives here:

1. **The draft is not this backend's API.** Nothing in `cmd/ai-grid-provider-backend`,
   `internal/servers` or `client` imports `gen/ai_grid/draft/v1`; the draft has no REST or
   OpenAPI surface (`proto/buf.gen.draft.yaml` generates Go and gRPC only) and is excluded
   from the real API's generation (`proto/buf.gen.yaml`).
2. **Each area is deleted when its real implementation lands.** See
   [When an area moves to the real backend](#when-an-area-moves-to-the-real-backend).
3. **For the real services, only infrastructure is faked.** Once an area is implemented in
   this backend, `make dev-fake` runs the real server for it against the fake hub and the fake
   Keycloak (`internal/fakeinfra/hub`, `internal/fakeinfra/keycloak`); nothing fakes the
   real server itself. The draft fake exists only for the areas that have no real server yet.
   One temporary overlap: Tenants is real since #3, and the draft's tenant RPCs stay until the
   console's Organizations page moves to the real ones (ai-grid-cloud/ai-grid-provider-console#42).
   They keep their own state, separate from the fake Keycloak's Organizations.

### When an area moves to the real backend

When the backend implements an area (for example `Tenants` CRUD or `ManagedClusters` reads and
edits) in `ai_grid.v1`:

1. The real proto, server, REST routes and tests land as for any resource (`docs/API.md`,
   `AGENTS.md`), using the area's page in `areas/` and its research report as the
   specification.
2. In the same change or the next one, delete that area from the draft: its messages and
   methods in `proto/ai_grid/draft/v1` (whole files once empty), the generated code
   (`make generate`), its code and tests in `internal/fakeinfra/draft`, its fixture file in
   `fixtures/draft/*`, and its page in `areas/`. Shrink `intentionallyUnimplemented` and
   `TestFakeImplementsEveryProtoMethod`'s expectations to what is left.
3. The console switches that area's calls from the draft client to the real `ai_grid.v1`
   client, and its development setup reaches the area through the real backend on
   `make dev-fake`'s gRPC port, not the draft port.

When nothing is left in the draft, `proto/ai_grid/draft`, `gen/ai_grid/draft`,
`internal/fakeinfra/draft`, `fixtures/draft` and this directory are deleted, along with the
`--draft-*` flags and `DEV_DRAFT_*` variables.

### Not wired to the fake hub yet

The draft fake keeps its own state, seeded from `fixtures/draft/*`, exactly as the console's
fake did. Its `ManagedClusters` and `Tenants` are not the fake hub's clusters or the fake
Keycloak's organizations: a cluster enrolled through the draft API does not change what the
real backend's `/api/v1/clusters` returns, and the other way round. Serving the draft's
cluster and tenant reads from the fake hub and the fake Keycloak (or, better, from the real
backend once it implements them) is a later step.

## Why this exists

The frontend and the BFF live in this repository. The backend is written by another team
in another repository and lands RPC by RPC. If the console only worked against a real
backend on a real hub, every page would wait for its RPC, every developer would need a
cluster, and every demo would depend on a hub being healthy.

So the console runs against a fake. The point of the fake is not convenience; it is
**faithfulness**. When the real backend arrives, the BFF mapping and the UI must need only
small changes, because the fake already produced the same field names, the same condition
and reason strings, the same error codes and the same timing the hub produces. Anything
the fake invents is a future bug, so nothing in it is invented without a source: every
behaviour comes from a CRD on the dev hub, from upstream controller source, from vendor
documentation, from the POC, or from the backend's own conventions. The sources are in
`research/`.

## Principles

1. **The proto is the contract.** The BFF and the fake compile against one generated
   draft package (`gen/ai_grid/draft/v1` here, `ai-grid-provider-console/bff/gen` in the
   console), built from the draft proto in `proto/ai_grid/draft/v1`. In the console the
   draft carried the backend's shipped services unchanged, and `ai-grid-provider-console/bff/internal/protodrift`
   fails if the backend's proto, vendored in `ai-grid-provider-console/mock/proto/upstream`, has anything the draft lacks
   or defines differently. `TestFakeImplementsEveryProtoMethod` calls every method of the
   draft over an in-memory connection and fails on any that answers `Unimplemented`, except
   the deliberate stubs listed with a reason in `intentionallyUnimplemented` (today
   `GetCurrentUser`, `Grid/GetNetwork` and the `CatalogItems` service), so the fake
   can never silently fall behind the proto. It guards against stubs, not behaviour; the
   per-area tests cover behaviour. `proto/README.md` has the details.
2. **Mirror the real objects, not the POC's inventions.** ACM `ManagedCluster`, `Placement`,
   `ManifestWorkReplicaSet`, Keycloak Organizations, KServe `LLMInferenceService`, MaaS
   `MaaSModelRef`, `MaaSSubscription`, `MaaSAuthPolicy`, Kuadrant policies and the Praxis
   grid CRDs are the objects the console's pages stand for. Wherever the fake models one of
   them (today clusters, tenants and models), it carries their fields, phases, condition
   types and reason strings verbatim. Fields the POC invented (`revision`, `status: Saved`,
   `hasDeployment`) do not exist here.
3. **"Not implemented yet" is a normal state.** RPCs that are not agreed yet answer
   `Unimplemented`, exactly as the real server does for a stub. The BFF maps it to 501 and
   the UI shows "not available on this backend yet", so a page can ship before its RPC.
4. **Errors use the same codes the backend will use.** `NotFound`, `AlreadyExists`,
   `InvalidArgument` with the CRD's own validation message, `FailedPrecondition` when the
   hub lacks a feature gate or Keycloak lacks the realm, `Unavailable` when Keycloak is down,
   `Aborted` for a stale version, `PermissionDenied`, `Unauthenticated`, and `Unimplemented`
   for stubs. The BFF maps all of them to HTTP, so the UI's error paths are exercised against
   the fake, not discovered in production. The shipped backend still answers every Keycloak
   failure `Unknown` (`502`); the fake's `tenants.json` modes answer what the backend should,
   `Unavailable` (`503`) or `FailedPrecondition` (`412`).
5. **Time passes.** Objects do not appear Ready. They go through the same transitions the
   controllers drive, on a clock the fixtures control (`settings.json` `speed`): a cluster
   joins, imports and gains its add-ons in stages, a lost lease recovers, a cluster detaches
   and goes away (`scenarios.json`); a model walks KServe's conditions to `Ready` or stops at
   a failure, and a placed model rolls out site by site. Once the subscriptions area lands (plan 4
   below), a subscription will wait for its models' rate-limit policies to be accepted.
6. **No real data, ever.** Fixtures use `example.com`, invented UUIDs and fictional sites.
   Research notes quote field paths, string constants, versions and in-cluster object names
   from the real systems, never the dev hub's routes, cluster ids or people.

## How to run it

In this repository:

```sh
make dev-fake      # real backend gRPC :9090, REST :9092; fake hub :9091; fake Keycloak :9093; draft fake :9094
make dev-fake-poc  # the same on fixtures/poc and fixtures/draft/poc
```

```
real backend :9090 gRPC, :9092 REST ──→ fake hub API :9091, fake Keycloak :9093
draft fake   :9094 gRPC (ai_grid.draft.v1, own state from fixtures/draft/*)
```

Every port is overridable (`DEV_GRPC_PORT`, `DEV_HUB_PORT`, `DEV_HTTP_PORT`,
`DEV_KEYCLOAK_PORT`, `DEV_DRAFT_PORT`), `DEV_DRAFT_ADDRESS=` (empty) runs without the draft, and
`DEV_DRAFT_FIXTURES` picks the draft fixtures.
`cmd/ai-grid-fake-infra` serves the draft on `--draft-address` (empty disables it) from
`--draft-fixtures`. No `oc login`, no cluster. The console runs its BFF and frontend in a
second terminal (see the console's README for the targets; pointing its BFF's draft calls at the draft
port is the console's side of this move); its access check (`SelfSubjectAccessReview`) goes to the fake hub on `:9091`,
which reads `fixtures/default/access.json`, converted from the same tokens as
`fixtures/draft/default/access.json`. A token decides by itself:

| Token (the console's `AUTH_TOKEN`) | You get |
|---|---|
| any other, for example `dev-token` | A provider admin |
| `viewer-token` | A signed-in user who isn't a provider admin |
| `expired-token` | A rejected token: `Unauthenticated` from every draft RPC, `401` from the hub |

`make dev-fake-poc` starts the same on the PoC preset (`fixtures/draft/poc/`): the PoC's four
sites (`site1` San Francisco, `site2` New York, `site3` Brussels and the unenrolled `site4`
Sydney), its three models, three organizations and three subscriptions, with no scenario
events, so nothing changes over time. Organizations that a fixture lists without a version are
loaded at version "1" (PR 12's loader), so a stale save is refused on every seeded
organization. `fixtures/poc.md` lists the files. The console's browser tests run on this
preset.

The draft fake (`internal/fakeinfra/draft`) serves the `ai_grid.draft.v1` gRPC services with
health and reflection registered like the real server, a per-RPC latency that honours
cancellation, and each call logged with its method, `x-forwarded-user`, whether a token was
forwarded, and the result code. In the console it also served the one slice of the Kubernetes
API the BFF calls, `SelfSubjectAccessReview`; here the fake hub (`internal/fakeinfra/hub`)
serves that endpoint, and the draft fake reads its own copy of the tokens only to decide which
writes are allowed. `ai-grid-provider-console/bff/internal/backend/backendtest` is the
console's in-memory sibling of the fake for its Go tests.

To call the draft by hand:

```sh
grpcurl -plaintext -H 'authorization: Bearer dev-token' 127.0.0.1:9094 ai_grid.draft.v1.Models/ListModels
```

## Fixtures (`fixtures/draft/default/`)

`fixtures/default.md` describes every file and field; `fixtures/draft/README.md` says where
they came from. In short:

| File | Shape | What it seeds |
|---|---|---|
| `managedclusters.json` | `{"clusters": [ai_grid.v1.ManagedCluster...]}` in protojson | Full hub objects: the labels ACM writes, the console's `ai-grid/*` and `location` labels and `ai-grid/*` annotations, and a `status` with every condition. The hub's own `local-cluster` and at least one cluster in each state the ACM console distinguishes (ready, unknown, offline, importing, pending import, import failed, not accepted, detaching). |
| `tenants.json` | `{"tenants": [ai_grid.v1.Tenant...]}` in protojson, or one error mode | Six fictional Keycloak organizations. In place of `tenants`, `{"unavailable": "<text>"}` makes every tenant RPC that passes the token and permission checks `Unavailable` (Keycloak cannot be reached), `{"realmMissing": true}` makes it `FailedPrecondition` as on the dev hub, where the `ai-grid` realm does not exist, and `{"organizationsDisabled": true}` does the same for a realm without Organizations. |
| `models.json` | `{"models": [ai_grid.v1.Model...]}` in protojson, plus `bindings` | `LLMInferenceService` models, some placed on clusters, and the cluster sets bound into each model namespace. |
| `presets.json` | plain JSON | The installed `LLMInferenceServiceConfig` presets that a model's `baseRefs` resolve against. |
| `scenarios.json` | `{"events": [...]}` | Timed cluster transitions: an import in stages, a lease recovery, a detach. |
| `access.json` | plain JSON | Who the fake hub treats as a provider admin; `viewer-token` is denied and `expired-token` rejected. |
| `settings.json` | plain JSON | The per-RPC latency, the scenario clock's `speed`, and the hub capabilities `GetHubCapabilities` reports (the ManifestWorkReplicaSet gate, observability, ACM version). |

The cluster, tenant and model entries are protojson, so they use the field names grpcurl
shows and fail to load on any unknown field; a fixture that fails to load stops the fake with
an error. The file envelopes and the other files are plain JSON and ignore unknown keys, so
check their spelling: while `defaultAllowed` is true, a misspelled `deniedTokens` makes that
token a provider admin. A missing file leaves its part empty or at its default, except
`access.json`: without it every token is a provider admin. The directory itself must exist.
A test loads the shipped directory and checks the ACM labels are present. Fixture names,
labels and ids must never be real: this repository is public.

## What the research found, in one paragraph each

The full reports are in `research/`. Read them before changing a fixture or adding a
simulated behaviour; they cite the CRD path or source file for every string.

**ACM hub** (`research/acm.md`). A `ManagedCluster`'s health is five conditions with fixed
reason strings, written by three actors (the hub's registration controller, the spoke agent
and ACM's import controller); the ACM console derives
`pendingimport | importing | importfailed | ready | offline | unknown | detaching | notaccepted`
from them, and the fake derives the same state on every read. ACM auto-labels every imported
cluster; the hub adds `unreachable`/`unavailable` taints, which a Placement must tolerate to
keep such a cluster selected through an outage. Placements only see
cluster sets bound into their namespace, write `PlacementDecision`s named
`<placement>-decision-<n>` sorted by cluster name, and recompute within seconds when labels
change. `ManifestWorkReplicaSet` fans one `ManifestWork` per selected cluster with
`Applied`/`Available` and rule-driven `Progressing`/`Degraded` conditions and a summary. On
the dev hub the MWRS feature gate is off: the CRD accepts objects that nothing reconciles,
so the backend must detect that and answer `FailedPrecondition`.

**Keycloak tenants** (`research/keycloak.md`). The hub runs Keycloak 26.8 with Organizations
available but no `ai-grid` realm. The Organizations admin API lists by name with offset
paging (default page 10), returns 201 plus a `Location` header on create, replaces `name`,
`enabled`, `description` and `redirectUrl` on update but keeps the `attributes` and domains a
body omits, enforces unique `name`, `alias` and domains with exact 409 and 400 messages, and
needs `view-organizations` to return anything (`query-organizations` alone yields an empty
list). The backend today logs in as the master realm admin with a password
grant on every call and returns every failure as `Unknown`. The report proposes the
`Tenants` CRUD proto, the gRPC code for each Keycloak outcome, and the bootstrap steps for a
realm plus a scoped service-account client.

**Models, MaaS, Kuadrant, Praxis** (`research/models-maas.md`). Models are KServe
`LLMInferenceService` (v1alpha2) with Knative-style conditions; there is no phase, so
Pending/Ready/Failed is derived from `Ready`, and the reason strings on failure are known.
MaaS entitlement is a dual check: `MaaSAuthPolicy` grants access by group or user,
`MaaSSubscription` sets quota per model; a `MaaSModelRef` stays `Pending/NoPairingFound` until
both reference it, and the controller materialises one Kuadrant `TokenRateLimitPolicy` per model
(`maas-trlp-<model>`), shared by every subscription that lists the model. On the hub the
model MaaS serves is an `ExternalModel` pointing at the Praxis grid gateway, not a local
model. Praxis grid CRDs are documented but not installed; their phase machines are in the
report. API keys live in `maas-api`, not in a CRD. The POC stores CR-shaped JSON in Git and
does not use ConfigMaps for the catalog.

**Observability** (`research/observability.md`). The fleet Thanos behind ACM's
`rbac-query-proxy` is the right endpoint and labels every series with `cluster=<ManagedCluster
name>`. AI metrics reach it through RHOAI's OpenTelemetry collector at a 300 second cadence,
so rate windows must be 15 minutes or more. vLLM metrics with `model_name` are real and
fleet-visible today, but two scrape configs collect them, so every unfiltered collector
counter is doubled: every query on collector metrics must pin
`scrape_source="rhai-metrics-sourced"`. The gateway request, error and latency metrics the POC
assumed do not exist; Istio ones are wired but empty, and GPU utilisation arrives divided by
100. No metric carries a tenant label yet, so per-tenant usage is `unavailable` until a Kuadrant
`TelemetryPolicy` or the Praxis metering service exists. The POC's overview JSON and its
deterministic demo generator are transcribed so the fake can reproduce them sample for sample.

**Backend conventions** (`research/backend-conventions.md`). When that research was written,
the backend was a single-file proto with three RPCs, generated code committed by hand, no interceptors and errors returned
as plain Go errors. It says it is aligned with OSAC's fulfillment-service, whose conventions
are precise:
`List/Get/Create/Update/Delete`, offset and limit paging with `size` and `total`, CEL
filters, field masks with optimistic locking, `id/metadata/spec/status` objects, a shared
condition type, and a fixed gRPC error table. The report proposes a style guide that keeps
the shipped method names, drafts the next proto files in that style, and describes the
two-repository workflow: proto first with `Unimplemented` stubs, tag, console bumps, fake
implements, nightly drift build.

## Plan per area

Each area lists what the fake simulates, what the frontend builds against it, and what the
backend has to agree to. Areas 1 to 3 are implemented in the fake and the BFF;
`areas/<area>.md` is the reference for what each implements, where it deviates from the real
systems and what remains, and the UI for them is being ported separately. Areas 4 to 6 are
plans: their RPCs in the draft are stubs that answer `Unimplemented`.

### 1. Clusters (ACM ManagedCluster), implemented

Fake: `ManagedCluster` with annotations, conditions, claims, capacity, taints, finalizers and
a `status.state` derived on every read in the ACM console's order; seeded clusters in every
state the console distinguishes; `scenarios.json` drives join, import, add-ons, lease loss and
recovery, an API-server outage, detaching and removal; `UpdateManagedCluster` edits labels,
annotations and hub acceptance, then puts back the labels the hub owns. See
`areas/clusters.md`.
Frontend: cluster status badges from the derived state, console link from the claim, GPU
capacity, enrol and location editing. The map belongs to area 5.
Backend ask: `GetManagedCluster`, `UpdateManagedCluster` and the richer message as drafted in
`proto/ai_grid/draft/v1/managed_cluster*.proto`, with the state derived server-side as in
`research/acm.md`.

### 2. Tenants (Keycloak Organizations), implemented

Fake: an in-memory organization store with UUID ids that answers like the backend's mapping of
Keycloak 26.8: Keycloak's checks in Keycloak's order with its messages (unique name, alias and
domains, alias defaulting to the name and immutable, domain format); without a field mask an
update replaces `name`, `enabled` (true when omitted) and `description` and keeps everything
else; case-insensitive search on name and domains, offset paging, delete; a version kept in an
organization attribute, checked on update and delete; and the `tenants.json` error modes for
an unreachable Keycloak (`Unavailable`) and a missing realm or Organizations feature
(`FailedPrecondition`). Members are not drafted yet. See `areas/tenants.md`.
Frontend: tenant list with search and paging, create and edit forms whose validation errors
are the ones Keycloak would send, delete with confirmation, and a members view once the member
RPCs exist.
Backend ask: the `Tenants` CRUD proto as drafted in `proto/ai_grid/draft/v1/tenant*.proto`, keyed by
`id`; the gRPC code table in `research/keycloak.md` section 8.2 (today every failure is
`Unknown`); a scoped service-account client instead of the master admin.

### 3. Models (LLMInferenceService, Placement, ManifestWorkReplicaSet), implemented

Fake: `CreateModel` admits a model with the API server's and the CRD's own validation messages
and returns it with no conditions; the model then walks KServe's condition set to `Ready` on
timers, or stops at a failure: an unknown `baseRef` (`ConfigNotFound`), an image or model name
containing `oom` or `fail` (`MinimumReplicasUnavailable`), a Gateway that does not exist
(`RefsInvalid`). A placed model gets a Placement whose decision is recomputed from cluster
labels, taints and the cluster sets bound into its namespace, and a ManifestWorkReplicaSet
whose per-cluster works are applied all at once or progressively; a cluster labelled
`ai-grid-mock/fail-apply=true` fails to apply its work. `settings.json` reproduces the dev
hub's disabled ManifestWorkReplicaSet gate with `FailedPrecondition`. See `areas/models.md`.
Frontend: model list and detail with per-site status from the works, create wizard with the
POC's inputs mapped to the real spec, delete blocked while a subscription references the
model.
Backend ask: the `Models` service and `Model` message as drafted in
`proto/ai_grid/draft/v1/model*.proto`, with the phase derived server-side;
`AiGrid/GetHubCapabilities` so the UI can explain a disabled gate; the ManifestWorkReplicaSet
feature gate on the hub.

### 4. Catalog and subscriptions (MaaSModelRef, MaaSSubscription, MaaSAuthPolicy)

Fake: a catalog entry starts `Pending/NoPairingFound` and becomes `Ready` once a
subscription and an auth policy reference it (in any phase; the pairing check ignores
`status.phase`) and its model is ready, turns `Unhealthy` when its model fails; a
subscription validates the CRD rules (`tokenRateLimits` or `unlimited`, window regex, limit
range), has no phase until its first reconcile, is `Degraded`
(`Ready=False/PartialFailure`) while the `TokenRateLimitPolicy` of any of its models isn't
accepted, flips to `Active` once they all are, and optionally creates its `<name>-policy` auth
policy with the same description annotation the hub uses. Whether the console creates the auth
policy together with the subscription or leaves it to be managed separately is a product
decision the research does not settle, so the fake should make the pairing a mode switch.

The port implements this area from the research (`research/models-maas.md` 3.4 to 3.6, 8.1
and 8.2), not from the PoC, whose `Saved` phase does not exist on the hub. What the fake does:

- **Subscription lifecycle.** No phase is stored; it is derived on every read from the
  scenario clock (`speed` in `settings.json` scales it). The controller never writes a
  `Pending` phase: `status.phase` has no default, so a new subscription has none until its
  first reconcile, which maas-api treats as unreconciled. After that it is `Degraded`
  (`Ready=False`, `PartialFailure`, "1 of 1 TokenRateLimitPolicies not accepted"; the count
  is the number of model references) only while its generated `TokenRateLimitPolicy` is not
  `Accepted`, and then `Active`. Nothing checks that the policy is enforced, so an accepted
  policy reads `Active`. The console shows `Active` as "Ready", as odh-dashboard does. A subscription whose models are all
  missing is `Failed`, and one with some missing is `Degraded` with "N of M model references
  are invalid or unavailable" (each missing one `NotFound`, "MaaSModelRef <ns>/<name> not
  found"). Every reference has a `TokenRateLimitPolicy` status, as `checkTokenRateLimitHealth`
  reports it: an unlimited one too (its policy carries the shared unlimited limit), and a
  missing model's is `BackendNotReady`. One timer runs from create; an update does not restart
  it. An update replaces the subscription: a field sent empty is cleared, and an update that
  changes nothing keeps the version. A fixture
  subscription without `createTime` plays the same lifecycle from the fake's start, and one
  with a past `createTime` reads as settled. The fake never derives `Invalid`, and it models
  only the `Ready` condition.
- **Auth policies and registrations.** A managed policy (`<name>-policy`) goes `Pending`, then
  `Active`; a policy whose models are all missing is `Failed`, and one with some missing is
  `Degraded` ("N of M model references are missing"). Every model counts as registered through
  a `MaaSModelRef` of the same name, whose status is returned with a subscription: `Pending`
  until a subscription and an auth policy both name it, `Ready` when its model is ready and
  `Unhealthy` when the model has failed (`Ready=False/BackendNotReady` in both, and
  `RuntimeReady=False/RuntimeHealthFailure` while the model isn't ready).
- **CRD messages, unverified.** The token rate limit rules (the window pattern
  `^[1-9]\d{0,3}(s|m|h)$`, so `1d` is refused, and the limit range) use the wording of the
  Kubernetes schema validator, because the research gives none. The two CEL messages
  ("tokenRateLimits is required unless unlimited is true" and "tokenRateLimits must not be set
  when unlimited is true") are the CRD's. **The window and limit messages are unverified
  against a hub**: check them with a server-side dry run once one is at hand.
- **Models are undeletable while referenced.** `DeleteModel` is `FailedPrecondition` while a
  subscription or an auth policy references the model, including the fixtures' unmanaged
  policies, which are never deleted with a subscription. The message names each referrer, for
  example "auth policy engineering-policy, subscription engineering". A policy's bare model
  names, and a reference without a namespace, count as the console's models namespace.

Frontend: subscriptions table keyed on phase, create and edit with per-model quotas,
tenant linkage through `tokenMetadata.organizationId`, a visible consistency check between a
subscription's owners and the auth policies that cover its models.
Backend ask: the `Catalog` and `Subscriptions` services in `research/models-maas.md`
section 8.1, and a decision on the subscription and auth policy pairing.

### 5. Grid overview and monitoring (Thanos)

Fake: the POC's `GetOverview` contract with the four metric series, per-metric status
(`ok`, `no-data`, `unavailable`, `not-configured`), the deterministic demo generator ported
verbatim so browser mock and fake agree, 300 second sample cadence when emulating the fleet,
clusters that are not Ready emitting nothing, and a fake Thanos HTTP server that reproduces
the observed error cases (JSON `bad_data`, plain-text resolution error, 302 on a bad token,
warnings, more than one series), with every series both labelled
`scrape_source="rhai-metrics-sourced"` and as an unlabelled twin, so the queries' filter is
exercised. Per-model usage from the vLLM metric names that exist;
per-tenant usage `unavailable` until the platform has a tenant label.
Frontend: map and monitoring pages with a simulation banner driven by `metricsSource`, charts
that show gaps rather than zeros.
Backend ask: the `Grid` RPCs and PromQL in `research/observability.md`, the fleet endpoint
and credential choice, and the two gateway and tenant-label questions listed there.

### 6. Grid network (Praxis CRDs)

Fake: default to "CRDs not installed" with the `AIGateway` condition the hub shows; a flag
flips to a network with one local site stuck `Pending/AwaitingDiscovery`, remote sites
cycling through the documented phases, and one `InferenceProvider` per Ready model with the
values the ODH bridge writes.
Frontend: a grid topology view that renders the not-installed state as information.
Backend ask: `Grid/GetNetwork` once the CRDs ship through the AI Gateway component.

## Working in parallel with the backend team

`proto/README.md` has the full steps. Since the draft moved into this repository, the
workflow is:

1. **Proposals are PRs here.** A new or changed draft RPC is a PR in this repository that
   edits `proto/ai_grid/draft/v1`, regenerates (`make generate`) and updates the fake in
   `internal/fakeinfra/draft` (or lists the method as an intentional stub), with the
   behaviour from the research. Both teams review the shape in one place.
2. **The console follows.** The console generates its client from this repository's draft,
   its BFF handler lands, and the page switches from mock to real types. Each step is a
   small PR.
3. **Stubs are fine in production.** A deployed console in front of a backend that lacks an
   RPC shows "not available yet" on that page and works everywhere else.
4. **Implementing an area retires its draft.** The real RPCs land in `ai_grid.v1` and the
   area leaves the draft ([When an area moves to the real backend](#when-an-area-moves-to-the-real-backend)).
   The console's drift test (`ai-grid-provider-console/bff/internal/protodrift`, run nightly
   by `ai-grid-provider-console/.github/workflows/drift.yaml`) still compares the real proto
   with its draft until the console drops it.
5. **The research is the shared source.** When the backend team implements an area, the
   report for that area is the specification both sides test against; disagreements go into
   the report's open questions and are resolved there before code.

## Open questions that affect the mock

- Subscription and auth policy: created together by the provider console (the POC's
  `<name>-policy` convention), or managed separately? The UI differs; the fake should
  support both.
- Which Keycloak realm holds tenants: a new `ai-grid` realm, or the organization-enabled
  realm the hub already imports?
- Will the backend act as the caller on the hub (so hub RBAC decides) or validate tokens
  and apply its own policy? This decides what `GetCurrentUser.is_provider_admin` means.
- Method naming: keep `ListManagedClusters` or move to OSAC's bare `List` while the console
  is the only consumer?
- Field masks on maps: whole-map replacement or per-key paths for a single label toggle?
- Which gateway carries AI Grid traffic, and who adds the tenant label to metrics?
- Can the MWRS feature gate be enabled on an MCE-owned `ClusterManager` without being
  reverted?

The other lists, with context, are at the end of each report in `research/`; the
subscription question is the console's own.
