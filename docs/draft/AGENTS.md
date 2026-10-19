# Building UI against the draft fake

The entry point for a coding agent (or a person) building console pages against the fake of
the console's draft API (`ai_grid.draft.v1`, served by `make dev-fake` on `:9094`). It says
what runs, what the console's BFF answers on top of it and what the fixtures seed, and points
to the detail. It was written in the console (`mock/AGENTS.md` at `ac80c25`); the BFF, its
HTTP contract and its golden responses stay in the console repository, so links to them point
there. [README.md](README.md), "In this repository", says what moved and the rules for the
draft here.

| For | Read |
|---|---|
| What the BFF may do (transport only) and what belongs to the backend and so to the fake | [the console's AGENTS.md](https://github.com/ai-grid-cloud/ai-grid-provider-console/blob/main/AGENTS.md) |
| Every endpoint, JSON shape, body rule and error mapping | [the console's docs/plan/bff-api.md](https://github.com/ai-grid-cloud/ai-grid-provider-console/blob/main/docs/plan/bff-api.md) |
| The draft proto the BFF and the fake compile against, and the deliberate stubs | [proto/README.md](proto/README.md) |
| The fixture files and what each one seeds | [fixtures/default.md](fixtures/default.md), [fixtures/poc.md](fixtures/poc.md) |
| What the fake and the BFF implement per area, deviations, invented texts, what remains | [areas/clusters.md](areas/clusters.md), [areas/tenants.md](areas/tenants.md), [areas/models.md](areas/models.md) |
| What the BFF returns for the shipped fixtures | [the console's mock/examples/](https://github.com/ai-grid-cloud/ai-grid-provider-console/blob/main/mock/examples) (below) |

Background reading, not needed to build against the fake: [README.md](README.md) on why the
console runs against a fake, and `research/` for the sources behind the fake's strings and
timings.

## Run it

In this repository:

```sh
make dev-fake    # real backend gRPC :9090 and REST :9092, fake hub :9091, fake Keycloak :9093, draft fake gRPC :9094
```

Then the console's BFF and Vite dev server in the console repository (its README has the
targets; pointing its BFF's draft calls at the draft port, `127.0.0.1:9094`, is the console's
side of this move). No cluster and no `oc login` are
needed. The Vite dev proxy sends `AUTH_TOKEN` as `x-forwarded-access-token` and `AUTH_USER`
as `x-forwarded-user`; the fake hub and the draft fake decide by token alone
(`fixtures/default/access.json` for the hub, `fixtures/draft/default/access.json` for the
draft fake, the same tokens):

| `AUTH_TOKEN` | Who you are |
|---|---|
| `dev-token` (default, user `dev-user`) or any token not listed below | provider admin: reads and writes |
| `viewer-token` (`AUTH_USER=viewer`) | signed in, not a provider admin: reads work; a write that reaches the backend is 403 (see "Errors" for the exceptions) |
| `expired-token` | rejected by the hub and the backend: 401 from every endpoint but `/api/v1/config` (see "Errors") |

Without the frontend, call the BFF directly:

```sh
curl -s -H 'x-forwarded-access-token: dev-token' -H 'x-forwarded-user: dev-user' \
  http://127.0.0.1:8080/api/v1/clusters
```

`make dev-fake-poc` is the same stack on the PoC preset `fixtures/draft/poc/` (the PoC's four sites, three
models, three organizations and three subscriptions, no scenario events), for comparing screens with
the PoC; the console's browser tests use it. Ports are overridable with `DEV_*_PORT`
(`DEV_DRAFT_PORT` for the draft fake).

The draft fake (`internal/fakeinfra/draft`, served by `cmd/ai-grid-fake-infra --draft-address`)
serves the draft's gRPC services from `fixtures/draft/default/`; the fake hub answers the
BFF's access review like a hub would. Its gRPC services need
`authorization` metadata like the real backend; the BFF forwards it from the proxy headers.
Edit the fixtures and restart to change the fleet, the tenants or the models: `tenants.json`
can reproduce a Keycloak that is unreachable or has no `ai-grid` realm, and
`manifestWorkReplicaSetEnabled: false` in `settings.json` mirrors the dev hub: creating a
model with a `placement`, or adding one by PATCH, is refused with 412, the seeded placed models
read `pending` with an empty rollout status, and `GET /api/v1/capabilities` reports the gate as
off. The fake moves clusters and models over time (see "Timed scenarios").

## The HTTP contract

[The console's docs/plan/bff-api.md](https://github.com/ai-grid-cloud/ai-grid-provider-console/blob/main/docs/plan/bff-api.md) is the contract: every endpoint, the JSON
shapes with examples, the body rules and the error table. The BFF's structs in
`ai-grid-provider-console/bff/internal/api/*.go` are what it encodes. Every success is `{"data": ...}`; a paged list
adds `"meta": {"size", "total", "offset", "limit"}` (`limit` only when the request sent one,
`total` omitted when it is unknown); every error is
`{"error": {"code": "<status>", "message": "..."}}`; `DELETE` answers 204 with no body. Every
`/api/v1` path needs `x-forwarded-access-token` (401 without it); `x-forwarded-user`,
`-email` and `-groups` are optional.

| Method | Path | Request body | Response `data` |
|---|---|---|---|
| GET | `/api/v1/user` | | `{username, email?, groups, isProviderAdmin}` |
| GET | `/api/v1/config` | | `{gridEndpoint, mapTileUrl, mapTileAttribution, billingUrl, version, modelsNamespace, fakeBackend}` |
| GET | `/api/v1/clusters` | | `GridCluster[]` by name |
| GET | `/api/v1/clusters/{name}` | | `GridCluster`, `ETag` |
| PATCH | `/api/v1/clusters/{name}` | `{labels?, annotations?}` (whole maps; only `ai-grid/*` keys and the `location` label may change) | `GridCluster` after the write, `ETag` |
| GET | `/api/v1/tenants?offset=&limit=&filter=` | | `Tenant[]` by name, plus `meta` |
| GET | `/api/v1/tenants/{id}` | | `Tenant`, `ETag` |
| POST | `/api/v1/tenants` | `{name, alias?, description?, enabled?, domains?, attributes?, identityProvider?}` | 201, `Tenant`, `Location` and `ETag` headers |
| PATCH | `/api/v1/tenants/{id}` | `{name?, description?, enabled?, domains?, attributes?, identityProvider?, alias?}` (the backend refuses a changed alias) | `Tenant` after the write, `ETag` |
| DELETE | `/api/v1/tenants/{id}` | | 204 |
| GET | `/api/v1/models?offset=&limit=&filter=` | | `Model[]` by namespace and name, plus `meta` |
| GET | `/api/v1/models/{namespace}/{name}` | | `Model`, `ETag` |
| POST | `/api/v1/models` | `{name, namespace, modelUri, modelName?, runtimeImage?, runtime?, replicas? or scaling?, baseRefs?, cpu?, memory?, gpuCount?, env?, gateway?, placement?, rollout?, labels?, annotations?}` | 201, `Model` with no conditions yet, `Location` and `ETag` headers |
| PATCH | `/api/v1/models/{namespace}/{name}` | `{replicas?, runtimeImage?, env?, placement? (null removes it), rollout?, labels?, annotations?}` (whole maps) | `Model` after the write, `ETag` |
| DELETE | `/api/v1/models/{namespace}/{name}` | | 204 |
| GET | `/api/v1/capabilities` | | `{manifestWorkReplicaSetEnabled, observabilityInstalled, acmVersion}` |
| GET | `/api/v1/locations` | | `string[]`: the `location` labels of the provisioned, Available AI Grid members, sorted |
| GET | `/api/v1/subscriptions` | | `Subscription[]` by name |
| GET | `/api/v1/subscriptions/{name}` | | `{subscription, modelRefs, authPolicies, meteringMetadata?, modelRegistrations}` |
| POST | `/api/v1/subscriptions` | `{name, displayName?, description?, priority, owner, modelRefs, tokenMetadata?, inferencePriority?, guardrails?, createAuthPolicy?}` | 201, `{subscription, authPolicy?}`, `Location` header |
| PUT | `/api/v1/subscriptions/{name}` | the POST body without `name` and `createAuthPolicy` | `{subscription}` after the write |
| DELETE | `/api/v1/subscriptions/{name}` | | 204 |
| GET | `/api/v1/grid/overview?range=&site=&metrics=false` | | `{clusters, metrics, metricsSource?, inventorySource?, demo, updatedAt?}` |

Request bodies must be sent with `Content-Type: application/json` (charset allowed), or the
POST, PUT or PATCH is 415 `request body must be JSON: send Content-Type: application/json`. They
are exactly one JSON object of at most 1 MiB: `null`, an array or a scalar is 422 (`invalid
request body: must be a JSON object`), so are unknown fields, wrong types and anything after
the object, and a larger body is 413. The BFF checks the body before it calls the hub or the
backend. Field semantics (what an absent field, an empty string or `null` inside the object
means) are in `bff-api.md`.

List filters. `GET /tenants` and `GET /models` take `filter`, a CEL expression over `this` (the
proto message, proto field names) that the page writes and the BFF forwards unchanged; the fake
evaluates it with cel-go and its strings extension before paging, so `meta.total` counts the
matches. For example `this.name.lowerAscii().contains("acme") || this.domains.exists(d,
d.lowerAscii().contains("acme"))` (lower the term yourself) or `"ai-grid/family" in
this.metadata.labels && this.metadata.labels["ai-grid/family"] == "granite"` (the `in` guard is
needed: CEL fails on a missing key instead of answering false). `bff-api.md`, "Writes, versions
and lists", has more examples and the error texts.

What a PATCH writes. One update, with every field the body names, and nothing read first: the
BFF does not merge, compare or validate. `labels` and `annotations` are whole maps that replace
the stored ones, so a page edits the map it loaded and sends all of it: to enrol
`site-frankfurt-1`, send its `labels` with `"ai-grid/enabled": "true"` added; to remove a label,
leave it out. Only the console's keys may change: a cluster PATCH that adds, changes or removes
a label or annotation outside `ai-grid/*`, `ai-grid-mock/*` and the `location` label is 422
(keys the hub puts back by itself, such as `name`, may be left out), and hub acceptance is not
writable. The display name and description of a model are its `openshift.io/display-name` and
`openshift.io/description` annotations, on create as on PATCH; the cluster's site name and
coordinates are its `ai-grid/site-name`, `ai-grid/latitude` and `ai-grid/longitude` annotations,
and the fake refuses a coordinate that is not a number in range (422; surrounding whitespace and
an empty value pass, as the read side ignores them). A body that names no field (`{}`) is 422
before any call. The fake decides what an update changes: one that leaves the object as stored
(the stored maps sent back, or a tenant's unchanged `alias`) is not written and keeps the
`resourceVersion`, like the API server.

Stale tabs (`ETag`, `If-Match`). `GET` of one cluster, tenant or model and the answer to its
POST or PATCH carry `ETag: "<resourceVersion>"`, as do `GET` of one subscription and the answer
to its PUT. Every PATCH, PUT and DELETE must send it back as `If-Match`: a write against an object
that changed since is 409 (the backend's `Aborted`) instead of overwriting it, and a PATCH, PUT
or DELETE without `If-Match` (or with `If-Match: *`, which names no version) is 428 before
anything is called. The BFF checks a write in this order: 415 for a body that is not JSON, 428,
then the body (422, 413). Anything but one quoted tag is 422. A tenant's `resourceVersion` is
kept by the backend (Keycloak has none); every tenant starts at `"1"` in the fixtures. Details in `bff-api.md`, "Writes, versions and lists".

## Golden responses (`examples/`)

What the BFF returns for the shipped fixtures at scenario time zero, one file per request:
`{"request", "requestHeaders"?, "requestBody"?, "token", "user", "status",
"responseHeaders"?, "body"}`, with `body` as the BFF encoded it (re-indented),
`requestHeaders` the headers sent besides the identity ones and a body's default
`Content-Type: application/json`, and `responseHeaders` the `ETag` and `Location` headers
when the BFF set them.

| File | Request |
|---|---|
| `get-user.json` | `GET /api/v1/user` as `dev-user` |
| `get-config.json` | `GET /api/v1/config` (every setting at its default) |
| `get-clusters.json` | `GET /api/v1/clusters`, all ten clusters |
| `get-cluster.json` | `GET /api/v1/clusters/site-paris-1` |
| `get-tenants.json` | `GET /api/v1/tenants`, all six tenants with `meta` |
| `get-tenant.json` | `GET /api/v1/tenants/{id}` for `acme-robotics` |
| `get-models.json` | `GET /api/v1/models`, all five models with `meta` |
| `get-model.json` | `GET /api/v1/models/ai-grid-models/mistral-7b-instruct`, a placed model selected for two sites at the start of its rollout: the first work applied, not yet available |
| `get-capabilities.json` | `GET /api/v1/capabilities` |
| `error-401-expired-token.json` | `GET /api/v1/user` with `expired-token` |
| `error-403-viewer-patch.json` | `PATCH /api/v1/clusters/site-frankfurt-1` with `viewer-token` and the cluster's `If-Match` |
| `error-404-unknown-cluster.json` | `GET /api/v1/clusters/site-unknown-1` |
| `error-409-stale-if-match.json` | `PATCH /api/v1/clusters/site-paris-1` with an `If-Match` older than the cluster |
| `error-428-missing-if-match.json` | `PATCH /api/v1/tenants/{id}` for `acme-robotics` without `If-Match` |
| `error-415-not-json.json` | `POST /api/v1/tenants` with `Content-Type: text/plain` |

They are produced by `ai-grid-provider-console/bff/internal/examples/golden_test.go`: the fake backend loaded from
the console's `mock/fixtures/` (`fixtures/draft/default/` here) over an in-memory gRPC connection with its clock frozen at
2026-10-15T12:00:00Z, no latency, deterministic tenant ids and work names (`g0001`, ...), the
fake hub over `httptest`, and the real BFF handler. `go test ./...` (and so CI) fails when a
file no longer matches. After changing a fixture or the BFF's JSON, regenerate and review the
diff, in the console repository:

```sh
make -C bff golden    # go test -count=1 ./internal/examples -run Golden -update
```

The running fake differs from the files only in what moves: times, `resourceVersion` (and so
`ETag`), the scenario state, work names (a random five-character suffix instead of `g0001`),
the ids of tenants you create (random UUIDs instead of `00000000-0000-4000-8000-...`) and
`version` in `/api/v1/config` (the git revision under `make dev-fake`, `dev` in the files).
Never hard-code those.

## What the fixtures seed (`fixtures/draft/default/`)

The summary below is what a page needs; [fixtures/default.md](fixtures/default.md) has every
field, and `areas/*.md` what the fake derives from them.

**Clusters** (`managedclusters.json`), every state the ACM console shows at least once, as
`status` (`ready` three times; no cluster lacks conditions, so `status` is never omitted):

| Cluster | `status` | Notes |
|---|---|---|
| `local-cluster` | `ready` | the hub itself |
| `site-paris-1` | `ready` | enrolled and provisioned, 16 GPUs, site name and coordinates |
| `site-frankfurt-1` | `ready` | not enrolled (no `ai-grid/*` labels), 8 GPUs, add-on `application-manager` `unhealthy` |
| `site-madrid-1` | `unknown` | lease expired, `unreachable` taint, add-ons `unreachable`; enrolled and provisioned |
| `site-ashburn-1` | `offline` | API server down, `unavailable` taint; add-ons still `available` (the hub changes them only on a lost lease) |
| `site-berlin-1` | `importing` | no claims yet |
| `site-oslo-1` | `pendingimport` | |
| `site-dublin-1` | `importfailed` | |
| `site-lisbon-1` | `notaccepted` | `hubAcceptsClient: false` |
| `edge-lyon-2` | `detaching` | `Deleting=False/ResourceRemaining` while its add-ons drain |

**Tenants** (`tenants.json`): `umbrella-biotech` is disabled; `acme-robotics` has two domains
(one verified, with `autoRedirect`) and attributes; `vandelay-industries` has a wildcard
domain (`*.vandelay.example.com`); `initech-labs` has no domains; `globex-research` has the
alias `globex`, different from its name; `wayne-aerospace` has one verified domain. No domain
names an identity provider: the fake realm has none, so the fake refuses to start on one. Every
tenant has `resourceVersion` `"1"`. Replace the file with `{"unavailable": "<text>"}`,
`{"realmMissing": true}` or `{"organizationsDisabled": true}` for Keycloak's failure modes
(503, 412, 412).

**Models** (`models.json`, namespace `ai-grid-models`):

| Model | `phase` at start | What it shows |
|---|---|---|
| `tiny-llm` | `ready` | a hub deployment, no placement |
| `granite-8b-instruct` | `ready` (`Complete`) | placed on enrolled and provisioned sites: `site-paris-1` at start; `site-madrid-1` joins at 300 s, when its lease recovers (`pending` until about 345 s, then `ready` on two sites) |
| `mistral-7b-instruct` | `pending` (`NotAsExpected` at 0 s, then `Processing`) | created when the fake starts; a Progressive rollout one site at a time in cluster-name order: `site-frankfurt-1` at once (ready at about 45 s), `site-berlin-1` (selected once it joins at 20 s; applied at about 75 s, after `minSuccessTime` 30 s), `site-paris-1` (applied at about 150 s), `ready` at about 195 s; `site-madrid-1` is added at 300 s (`pending` again until about 345 s) |
| `llama-3-70b` | `failed` (`ConfigNotFound`) | a preset the hub lacks |
| `codellama-13b` | `pending` (`PlacementDecisionEmpty`) | a placement that matches no cluster |

`settings.json` holds `latency` (40ms per RPC), `speed` and the hub capabilities (see
[fixtures/default.md](fixtures/default.md) for what a missing file defaults to);
`access.json` the tokens; `presets.json` the installed KServe presets; `scenarios.json` the
cluster events below. A fixture that fails to load stops the fake with the reason.

## Timed scenarios

Times below are scenario seconds after the fake starts (for a model: after it was created; for a
placed model's site: after its agent first reported). `speed` in `settings.json` scales
wall-clock time: an event at scenario time `t` happens `t / speed` real seconds in, so `10` plays
the 300 s cluster scenario in 30 s (default `1`). It applies to the cluster events and the model
lifecycles alike. Restart the fake to replay.

| At | What changes |
|---|---|
| 20 s | `site-berlin-1` joins: `status` `importing` to `ready`; claims, capacity (8 GPUs) and version appear |
| 30 s | `edge-lyon-2` finishes detaching and disappears from the list |
| 70 s | `site-berlin-1`'s `ManagedClusterImportSucceeded` condition turns True (and its `resourceVersion` moves); `status` stays `ready` |
| 90 s, 130 s | `site-berlin-1` add-ons become available (three, then all seven) |
| 300 s | `site-madrid-1` recovers its lease: `unknown` to `ready`; `granite-8b-instruct` and `mistral-7b-instruct` roll out to it (both `pending` until its work is ready, about 45 s later) |

A model created with `POST /api/v1/models` (and `mistral-7b-instruct` at start) walks
KServe's conditions, counted from its creation:

| At | `Ready` and what else changes |
|---|---|
| 0 s | no conditions yet |
| 5 s | `False/WaitingForGateway`: nine conditions; presets combined (`appliedConfigs` filled), gateways ready, the Deployment and the route created, so `url` and the six `addresses` appear now, before the model is ready |
| 15 s | `False/Progressing`: route, pool and scheduler accepted and ready |
| 20 s | `False/MinimumReplicasUnavailable` |
| 45 s | `True`, so `phase` reads `ready` (the Deployment's ready replica count is in the gRPC `status.workloads` only; the BFF does not return it yet) |

A hub deployment's `phase` reads `pending` while it starts, with the reason KServe reports
(`WaitingForGateway`, `Progressing`, `MinimumReplicasUnavailable`), and `failed` only for a
reason that does not recover on its own (`ConfigNotFound`, `RefsInvalid`, `GatewaysNotReady`). A
placed model's sites follow the same table, counted from the moment the site's agent reports its
work: `Applied` at once, `Available` 2 s later, the spoke ready at 45 s; `Progressive` moves to
the next site once the current one is ready and `minSuccessTime` has passed. Failures on demand:
an unknown `baseRefs` entry stops at 5 s with `ConfigNotFound` (no `url`); a gateway other than
`openshift-ingress/openshift-ai-inference` or `openshift-ingress/maas-default-gateway` is
`RefsInvalid` "Gateway <ns>/<name> does not exist" from 5 s, with no `url`, while the workload
still deploys; a `runtimeImage` or `modelName` (default: the model's `name`) containing `oom` or
`fail` stays at `MinimumReplicasUnavailable` from 20 s for good, as KServe does (it never turns
into `ProgressDeadlineExceeded`; `docs/draft/areas/models.md` has the source lines), so the model
reads `pending` with that reason for good: no condition tells it apart from a slow start; on a
spoke such a model keeps its work `Progressing` and the placed model reads
`pending`/`Processing` for good; the rollout's `progressDeadline` times that cluster out (a
`Progressive` rollout moves on to the next one), which no condition shows; a cluster labelled
`ai-grid-mock/fail-apply=true` fails to apply its work: the model reads `failed`/`NotAsExpected`
and the work `applied: "False"`, `available: "Unknown"`, with
`Applied=False/AppliedManifestWorkFailed` "Failed to apply manifest work" (the manifest's own
text, `Failed to apply manifest: the server doesn't have a resource type "LLMInferenceService"`,
is in the gRPC status only).

## Errors worth building UI for

| Status | How to get it from the fake | Message |
|---|---|---|
| 401 | `AUTH_TOKEN=expired-token` on an endpoint that asks the hub or the backend: `GET /user` (the hub's access review) and every `/clusters`, `/tenants`, `/models` and `/capabilities` endpoint (the backend). `GET /config` asks neither and answers 200; a write that fails the BFF's own checks is 415, 428, 422 or 413 first. A request without the token header is 401 on every `/api/v1` path, `/config` included | `the hub rejected the credentials; log in again` (`/user`), `the AI Grid backend rejected the credentials; log in again` (the others), `missing x-forwarded-access-token header` |
| 403 | `viewer-token` on a request that passes the BFF's checks (415, 428, the body) and reaches a mutating RPC: every POST and DELETE, and every PATCH that names a field, even one that would change nothing (the BFF reads nothing first, and the backend checks the permission before it looks the object up, so a PATCH of an unknown name is 403 too); a PATCH whose body names no field is 422 first | the backend's text: the API server's `... is forbidden: User "viewer" cannot patch resource "managedclusters" ...` for clusters, `User "viewer" is not a provider admin, and only provider admins can change tenants (a provider admin may patch ManagedClusters on the hub)` (`models` for models) |
| 404 | an unknown cluster, tenant or model | `object with identifier '<name>' not found` for a cluster, `'<id>'` for a tenant, `'<namespace>/<name>'` for a model |
| 409 | a taken tenant name or alias, or model name; a blank `name` in a tenant PATCH (Keycloak's own answer); an `If-Match` older than the object (also for an update that would change nothing) | `A organization with the same name already exists.`, `... same alias already exists`, the API server's `... already exists` for a model, `object with identifier '<id>' has resource version <v>, the request carries "<v>"; fetch it again and retry` |
| 412 | `manifestWorkReplicaSetEnabled: false` in `settings.json`, then a model with `placement` (a POST, or a PATCH that adds one); `{"realmMissing": true}` or `{"organizationsDisabled": true}` in `tenants.json` | `ManifestWorkReplicaSet feature gate disabled on hub`; `Keycloak realm "ai-grid": Realm not found.`, `Keycloak realm "ai-grid": Organizations not enabled for this realm.` |
| 413 | a body over 1 MiB | `request body must not exceed 1048576 bytes` |
| 415 | a POST, PUT or PATCH without `Content-Type: application/json` (an HTML form, `text/plain`) | `request body must be JSON: send Content-Type: application/json` |
| 428 | a PATCH, PUT or DELETE without `If-Match`, or with `If-Match: *` | `this write needs an If-Match header: send the ETag that GET answered (the object's resourceVersion), for example If-Match: "42"`, `If-Match: * names no version: ...`: send the `ETag` of the GET the page showed |
| 422 | a body the BFF rejects (unknown field, such as the removed `aiGridEnabled` or `hubAcceptsClient` or a model's `displayName`, wrong type, trailing content) or that names no field (`the request body names no field to change`), a cluster PATCH that changes a label or annotation outside the console's keys (`label "cluster.open-cluster-management.io/clusterset" cannot be changed through the console; only ai-grid/* labels and annotations and the location label can`), a repeated `filter`, an `ai-grid/latitude` or `ai-grid/longitude` annotation that is not a number in range (`annotation "ai-grid/latitude" must be a number between -90 and 90, got "91"`), a malformed `If-Match`, a list query with the removed `search`, `namespace` or `label`, a `filter` the backend cannot evaluate (`GET /api/v1/models?filter=this.metadata.labels[%22x%22]==%22y%22`: no model has `x`, and the index is not guarded), or a value the backend rejects | `query parameter "search" is not supported; send a CEL filter in "filter"`, `filter <f> is not a valid CEL expression: ...`, `filter <f> failed on '<id>': no such key: <k>`, Keycloak's texts (`Name can not be null`, `Invalid domain format: <d>`, `Cannot change the alias`, `Domain <d> is already linked to organization <o> in realm ai-grid`), the CRD's (`replicas and scaling are mutually exclusive; ...`) and the console's own checks (`spec.rollout requires spec.placement`, `attribute "ai-grid/resource-version" is reserved ...`); Keycloak's `Identity provider with alias '<a>' does not exist in realm ai-grid` exists in the fake but no BFF body can send a domain's identity provider |
| 501 | not reachable through today's endpoints on the fake. The shipped backend answers it for every RPC it lacks: `PATCH /clusters/{name}`, `GET /tenants/{id}`, every tenant write, every `/models` endpoint and `/capabilities` (`GET /clusters/{name}` falls back to the list); and the BFF answers it for `GET /tenants` with `filter`, `limit` or an `offset` above 0 there, because that backend ignores them | `the AI Grid backend does not support this operation yet`, or `the AI Grid backend does not support filtering or paging tenants yet; ...`: show "not available on this backend yet", not an error page |
| 503 | `{"unavailable": "..."}` in `tenants.json`, then any tenants endpoint as a provider admin (a viewer's write stays 403, an expired token 401: the token checks come first) | `the AI Grid backend is unavailable; try again` |

The full table, including 502 and 504, is in `bff-api.md`, "Errors".

## Deliberate stubs

The fake answers `Unimplemented` for exactly the methods in `intentionallyUnimplemented`
(`internal/fakeinfra/draft/fakebackend_test.go`); the test fails when the list and the fake
disagree ([proto/README.md](proto/README.md) explains the allowlist). Today:
`AiGrid/GetCurrentUser` (the BFF derives `isProviderAdmin` from a hub access review instead),
`Grid/GetNetwork`, and every method of `CatalogItems`. They are placeholders until the shapes
are agreed with the backend team; the BFF has no endpoint for them yet. `Subscriptions` and
`Grid/GetOverview` are implemented (`/api/v1/subscriptions`, `/api/v1/grid/overview`); their
proto is a proposal for the backend owners (`proto/README.md`, "Proposals from the port").

## Rules

1. Never invent a field. The shapes are the draft proto in `proto/ai_grid/draft/v1/*.proto` and the
   BFF JSON (`ai-grid-provider-console/docs/plan/bff-api.md`, `ai-grid-provider-console/bff/internal/api`, `ai-grid-provider-console/mock/examples/`). A page that needs more
   starts with the proto, the fake and the BFF, not with the UI.
2. No real data in fixtures: `example.com` hosts, documentation IP ranges, invented UUIDs and
   fictional names only. This repository is public.
3. `isProviderAdmin` only decides what to show; the backend enforces every write, so handle
   403 anyway.
4. Do not hard-code what moves: times, `resourceVersion`, work names, scenario state.
5. After changing a fixture or the BFF's JSON, run `make -C bff golden` in the console and review
   the diff. Never edit `gen/ai_grid/draft/v1` by hand: change `proto/ai_grid/draft/v1` and run
   `make generate` here (`proto/buf.gen.draft.yaml`), then `git diff --exit-code` after a second
   run.
6. When an area is implemented in the real backend, its draft proto, fake code, fixtures and
   page are deleted ([README.md](README.md), "When an area moves to the real backend"); for
   the real services `make dev-fake` fakes only the infrastructure (hub and Keycloak).
