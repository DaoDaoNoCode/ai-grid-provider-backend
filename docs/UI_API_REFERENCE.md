# AI Grid Provider API — UI reference

Handout for console / BFF work against **ai-grid-provider-backend** as shipped
today. This describes the **REST** surface (`grpc-gateway`). Field names are
**snake_case** in JSON.

| | |
| --- | --- |
| Base path | `/api/v1` |
| Content-Type | `application/json` |
| OpenAPI | [`openapi/ai_grid.swagger.json`](./openapi/ai_grid.swagger.json) |
| Live Swagger UI | `http://<host>:8081/docs/` (on OpenShift, through `oc port-forward`) |
| Proto source of truth | `proto/ai_grid/v1/` |
| Auth | None (out of scope for this MVP) |

**Object shape (OSAC):** every resource is `id` + `metadata` + `spec` + `status`.

**List envelope:** list endpoints return `{ size, total, items }` — not a bare array.

**Get / Create / Update REST responses** return the **resource object directly**
(not wrapped in `{ "object": ... }`). gRPC wrappers use `object`; the gateway
unwraps them via `response_body: "object"`.

**Create / Update REST request bodies** are the **resource object** (not
`{ "object": ... }`). Path id for PATCH is `{object.id}`.

---

## Quick index

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/ping` | Liveness / hello |
| `GET` | `/api/v1/clusters` | List ManagedClusters |
| `GET` | `/api/v1/clusters/{id}` | Get one cluster |
| `POST` | `/api/v1/clusters` | Create hub ManagedCluster stub |
| `PATCH` | `/api/v1/clusters/{id}` | Update labels and/or `hub_accepts_client` |
| `DELETE` | `/api/v1/clusters/{id}` | Delete ManagedCluster |
| `GET` | `/api/v1/tenants` | List tenants (Keycloak Organizations) |
| `GET` | `/api/v1/tenants/{id}` | Get one tenant |
| `POST` | `/api/v1/tenants` | Create tenant |
| `PATCH` | `/api/v1/tenants/{id}` | Update name and/or domains |
| `DELETE` | `/api/v1/tenants/{id}` | Delete tenant |
| `GET` | `/api/v1/tenants/{id}:listMembers` | List users in the tenant Organization |
| `GET` | `/api/v1/models` | List models (from the Git catalog) |
| `GET` | `/api/v1/models:listLocations` | Unique sorted names of the provisioned, Available inference and combined clusters |
| `GET` | `/api/v1/models/{id}` | Get one model |
| `POST` | `/api/v1/models` | Create model (commits it to Git) |
| `PATCH` | `/api/v1/models/{id}` | Update model (FieldMask) |
| `DELETE` | `/api/v1/models/{id}` | Delete model (its files and index entries); optional If-Match / resource_version |
| `GET` | `/api/v1/subscriptions` | List subscriptions |
| `GET` | `/api/v1/subscriptions/{id}` | Get subscription + its managed auth policy + model registrations |
| `POST` | `/api/v1/subscriptions` | Create subscription (+ managed AuthPolicy in response) |
| `PATCH` | `/api/v1/subscriptions/{id}` | Update subscription (syncs managed AuthPolicy) |
| `DELETE` | `/api/v1/subscriptions/{id}` | Delete subscription (+ managed AuthPolicy); optional If-Match / resource_version |

Also served (not resource APIs): `GET /openapi.json`, `GET /docs/`.

API request authentication and authorization are out of scope for this MVP.
Endpoints are open; Keycloak is used only as the Tenants Organizations backend
(Admin API). `KEYCLOAK_REALM` (default `ai-grid`) is the **shared platform
realm** — not a per-tenant identifier.

---

## Shared types

### `Metadata`

| Field | Type | Notes |
| --- | --- | --- |
| `name` | string | RFC 1123 DNS label (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`), 1–63 chars |
| `labels` | map\<string,string\> | User/system labels |
| `annotations` | map\<string,string\> | Opaque annotations |
| `creation_timestamp` | string (RFC 3339) | System-set |
| `deletion_timestamp` | string (RFC 3339) | Set while deleting |
| `creator` | string | Reserved; often empty today |
| `tenant` | string | Reserved; often empty today |
| `version` | int32 | Optimistic lock field; **not enforced yet** |

### List query parameters (Clusters + Tenants + Models + Subscriptions)

| Param | Type | Notes |
| --- | --- | --- |
| `offset` | int32 | Start index; default `0` |
| `limit` | int32 | Max items; omit = all |
| `filter` | string | CEL filter. **Models and Subscriptions evaluate it**. Clusters and Tenants accept but do not evaluate yet. |
| `order` | string | Sort. **Models and Subscriptions use allowlists**. Clusters and Tenants accept but do not evaluate yet. |

### Error body

Non-2xx responses use gRPC status JSON, for example:

```json
{
  "code": 5,
  "message": "ManagedCluster \"missing\" not found",
  "details": []
}
```

Common `code` values (gRPC status numbers): `3` InvalidArgument, `5` NotFound,
`6` AlreadyExists, `7` PermissionDenied, `13` Internal.

---

## 1. Ping

### `GET /api/v1/ping`

| | |
| --- | --- |
| Query | `name` (optional string; default greeting uses `"world"`) |

**Response `200`**

```json
{
  "message": "Welcome, Ada!"
}
```

---

## 2. Clusters (hub ManagedCluster inventory)

Hub-side ACM/OCM `ManagedCluster` CRs. **Does not provision spoke clusters.**
`id` equals the Kubernetes object name (`metadata.name`).

### `ManagedCluster` resource

```json
{
  "id": "playground-east",
  "metadata": {
    "name": "playground-east",
    "creation_timestamp": "2026-10-16T16:40:00Z",
    "labels": {
      "environment": "development",
      "ai-grid/enabled": "true",
      "cluster.open-cluster-management.io/clusterset": "ai-grid"
    },
    "annotations": {}
  },
  "spec": {
    "hub_accepts_client": true
  },
  "status": {
    "available": false,
    "joined": false,
    "available_condition": ""
  }
}
```

| Field | Meaning for UI |
| --- | --- |
| `spec.hub_accepts_client` | Hub accepts spoke registration |
| `status.available` / `joined` | Derived from OCM conditions |
| `status.available_condition` | Raw `"True"` / `"False"` / `"Unknown"` / `""` — useful for Ready/Offline/Unknown |
| `status.conditions` | The hub's conditions (`type`, `status`, `reason`, `message`, `last_transition_time`), in the hub's order |
| `status.kubernetes_version` | `status.version.kubernetes`, for example `"v1.34.9"`; empty until the agent reports it |
| `status.capacity` / `allocatable` | Kubernetes quantity strings by resource name, for example `"cpu": "68"`, `"memory": "273509824Ki"`, `"nvidia.com/gpu": "1"`; absent until the agent reports them |
| `status.cluster_claims` | `{name, value}` pairs the agent reports, for example the console URL (`consoleurl.cluster.open-cluster-management.io`), platform and product |
| `status.gpus` | The cluster's GPUs from ACM Search (its nodes' NVIDIA GPU Feature Discovery labels); absent when Search is off, unavailable or hasn't indexed the cluster (`capacity["nvidia.com/gpu"]` still counts schedulable GPUs then). `total` GPU devices as GPU Feature Discovery counts them (physical GPUs; MIG devices on a node with MIG strategy `"single"`, where GFD reports no physical count) and `node_count` GPU nodes; `configurations[]`: `{vendor, product, memory_mib, gpus_per_node, node_count, sharing}`, most nodes first (for a list cell such as "NVIDIA-L4 · 1 per node"); `nodes[]`: `{name, vendor, product, count, memory_mib, family, compute_capability, driver_version, cuda_runtime_version, sharing, instance_type, ready}` by name. `sharing`: `{strategy` (`"none"`, `"time-slicing"`, `"mps"` or `"mig"`), `replicas` (schedulable units per GPU), `mig_capable`, `mig_strategy` (`"single"` / `"mixed"`)`}`. With time-slicing, `capacity["nvidia.com/gpu"]` is `total` × `replicas`. |
| `metadata.labels["ai-grid/enabled"]` | Enroll-style flag (string `"true"` / `"false"`) |

---

### `GET /api/v1/clusters`

List ManagedClusters.

**Response `200`**

```json
{
  "size": 2,
  "total": 2,
  "items": [
    {
      "id": "playground-east",
      "metadata": {
        "name": "playground-east",
        "labels": { "ai-grid/enabled": "true" }
      },
      "spec": { "hub_accepts_client": true },
      "status": {
        "available": false,
        "joined": false,
        "available_condition": ""
      }
    },
    {
      "id": "playground-west",
      "metadata": {
        "name": "playground-west",
        "labels": { "ai-grid/enabled": "false" }
      },
      "spec": { "hub_accepts_client": true },
      "status": {
        "available": false,
        "joined": false,
        "available_condition": ""
      }
    }
  ]
}
```

---

### `GET /api/v1/clusters/{id}`

**Response `200`** — bare `ManagedCluster` (see resource shape above).

**Response `404`** — unknown id.

---

### `POST /api/v1/clusters`

Create a hub ManagedCluster stub.

**Request body**

```json
{
  "id": "demo-cluster",
  "metadata": {
    "name": "demo-cluster",
    "labels": {
      "environment": "dev",
      "ai-grid/enabled": "false"
    }
  },
  "spec": {
    "hub_accepts_client": true
  }
}
```

Notes:

- `id` (or `metadata.name`) is required and must be an RFC 1123 DNS label.
- Prefer setting both `id` and `metadata.name` to the same value.
- `status` on create is ignored (system-owned).

**Response `200`** — created `ManagedCluster` (includes server-filled metadata/status).

---

### `PATCH /api/v1/clusters/{id}`

Merge-update mutable fields. Path `{id}` must match the object’s id.

**Request body** (resource; id in path is authoritative for routing)

```json
{
  "id": "demo-cluster",
  "metadata": {
    "labels": {
      "ai-grid/enabled": "true",
      "ai-grid/region": "us-east"
    }
  },
  "spec": {
    "hub_accepts_client": true
  }
}
```

**Behavior today:**

- **Labels** are **merged** onto existing labels (keys in the body overwrite; other keys kept).
- **`hub_accepts_client`** is replaced when included in the update.
- Empty update mask (typical REST PATCH with no `update_mask` query) updates **all mutable fields** (labels + hub accepts).
- Optional query `lock` exists for future optimistic locking — **not enforced yet**.

**Enroll pattern (PoC-compatible intent):**

```http
PATCH /api/v1/clusters/playground-west
Content-Type: application/json

{
  "id": "playground-west",
  "metadata": {
    "labels": {
      "ai-grid/enabled": "true"
    }
  },
  "spec": {
    "hub_accepts_client": true
  }
}
```

**Response `200`** — updated `ManagedCluster`.

---

### `DELETE /api/v1/clusters/{id}`

**Response `200`**

```json
{}
```

Note for UI: after delete, OCM may keep a same-named namespace in `Terminating`
for a while. Recreating the same `id` immediately can fail until that clears;
prefer waiting or using a new name.

---

## 3. Tenants (Keycloak Organizations)

**Model:** one shared Keycloak realm (`KEYCLOAK_REALM`, default `ai-grid`) holds
all tenants. Each **Tenant is a Keycloak Organization** in that realm — **not**
a Keycloak realm. Admin API paths look like
`/admin/realms/{platform-realm}/organizations/...`; the `{platform-realm}`
segment is the shared realm name, not the tenant id.

Organization **members** can be listed via `GET .../tenants/{id}:listMembers`.
Full user CRUD and identity providers remain out of scope for this API today.

| Field | Meaning for UI |
| --- | --- |
| `id` | Keycloak organization **UUID** (server-assigned on create) |
| `metadata.name` | Organization name (RFC 1123 DNS label); required on create |
| `metadata.version` | Optimistic-concurrency version, kept in the organization attribute `ai-grid/resource-version`: 1 on create, bumped by every update through this API. An organization created in Keycloak directly reads as 1. Edits made in Keycloak directly don't bump it. Output only |
| `metadata.creation_timestamp` | From the attribute `ai-grid/create-time` (RFC 3339), stamped on create; absent for an organization created in Keycloak directly. Output only |
| `spec.domains` | Email domains for the org (DNS hostnames, ≥2 labels, lowercase) |
| `status` | Empty object today |

UI should treat **`id`** as the stable identifier for Get/Update/Delete, and
**`metadata.name`** as the display / create name.

**vs OSAC:** OSAC stores Tenants in a DB and a reconciler provisions the
Keycloak Organization (plus break-glass / IdP). AI Grid maps Tenants **directly**
to Keycloak Organizations Admin CRUD. Same Keycloak model (org-in-shared-realm);
do not “fix” by creating a realm per tenant.

---

### `GET /api/v1/tenants`

**Response `200`**

```json
{
  "size": 2,
  "total": 2,
  "items": [
    {
      "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "metadata": { "name": "tenant-a", "version": 3, "creation_timestamp": "2026-10-22T13:30:00Z" },
      "spec": { "domains": ["tenant-a.example.com"] },
      "status": {}
    },
    {
      "id": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
      "metadata": { "name": "tenant-b", "version": 1 },
      "spec": { "domains": ["tenant-b.example.com"] },
      "status": {}
    }
  ]
}
```

---

### `GET /api/v1/tenants/{id}`

`{id}` is the Keycloak organization UUID (not the name).

**Response `200`** — bare `Tenant`.

**Response `404`** — unknown id.

---

### `POST /api/v1/tenants`

**Request body**

```json
{
  "metadata": {
    "name": "acme"
  },
  "spec": {
    "domains": ["acme.example.com"]
  }
}
```

Notes:

- Do **not** send a client-chosen `id`; Keycloak assigns it.
- `metadata.name` is required (RFC 1123 DNS label).
- `spec.domains` entries must be lowercase DNS hostnames with at least two
  labels (e.g. `acme.example.com`); IPs and single-label names are rejected.

**Response `200`**

```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "metadata": { "name": "acme", "version": 1, "creation_timestamp": "2026-10-22T13:30:00Z" },
  "spec": { "domains": ["acme.example.com"] },
  "status": {}
}
```

---

### `PATCH /api/v1/tenants/{id}`

**Request body**

```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "metadata": {
    "name": "acme",
    "version": 1
  },
  "spec": {
    "domains": ["acme.example.com", "acme.io"]
  }
}
```

**Behavior today:**

- An empty update mask (gRPC without `update_mask`, or a REST PATCH without
  `?update_mask=`) is inferred from the object: the name when `metadata.name` is
  set, the domains when `spec` is present.
- Supported mask paths: `metadata`, `metadata.name`, `spec`, `spec.domains`
  (optional `object.` prefix). `id`, `metadata.version` and
  `metadata.creation_timestamp` are accepted and change nothing. Unsupported
  paths return `InvalidArgument`.
- Domains in the body **replace** the domain list when domains are updated.
  Blank/whitespace-only domain entries are rejected (`InvalidArgument`), not dropped.
- `metadata.version` is required (`400`, `InvalidArgument`, without one) and
  must equal the stored version, or the update is `409` (`Aborted`) and writes
  nothing. Every organization reads as version 1 or more. Every update that
  writes bumps the version.
- Everything this API doesn't model (other attributes, alias, enabled,
  description, redirect URL, identity provider links, each kept domain's
  `verified` and identity provider alias) is written back as read.
- A mask that names only `id`, `metadata.version` or
  `metadata.creation_timestamp`, or a body with no name and no `spec`, writes nothing and returns the tenant as stored,
  version unchanged.
- The backend serializes its own writes per tenant (one replica), so of two
  edits at the same version the second gets `409`. Keycloak has no conditional
  write, so an edit made in Keycloak directly isn't versioned.
- A new name or domain that another organization already has is `409`
  (`AlreadyExists`): "the name or a domain is already used by another
  organization".

**Response `200`** — updated `Tenant`.

---

### `DELETE /api/v1/tenants/{id}`

Required query `version`: without it the delete is `400` (`InvalidArgument`).
It must equal the stored `metadata.version`, or the delete is `409`
(`Aborted`) and removes nothing.

**Response `200`**

```json
{}
```

---

### `GET /api/v1/tenants/{id}:listMembers`

Lists Keycloak users that belong to the tenant’s Organization. Members are **not**
embedded in `GET /api/v1/tenants/{id}` so tenant reads stay cheap.

Optional query: `offset`, `limit` (same semantics as other List RPCs).

**Response `200`**

```json
{
  "size": 2,
  "total": 2,
  "items": [
    {
      "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "username": "alice",
      "email": "alice@acme.example.com",
      "first_name": "Alice",
      "last_name": "Admin",
      "enabled": true
    },
    {
      "id": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
      "username": "bob",
      "email": "bob@acme.example.com",
      "first_name": "Bob",
      "last_name": "Builder",
      "enabled": true
    }
  ]
}
```

gRPC: `ai_grid.v1.Tenants/ListMembers` with `{"id":"<org-uuid>"}`.

---


## 4. Models (Git catalog)

Every write commits to the backend's Git repository (`AI_GRID_GIT_URL`), and
Argo CD delivers it; the backend never writes to a cluster. Each model is an
`LLMInferenceService` and a NetworkPolicy in `AI_GRID_MODELS_NAMESPACE`
(default `ai-grid-internal-models`) on the clusters its placement names, and an
`ExternalModel` and `MaaSModelRef` in `AI_GRID_SUBSCRIPTION_NAMESPACE` on the
ingress clusters. See README "Git catalog" for the repository layout.

`spec.placement` is required and names at least one cluster, by the ACM
`name` label only (`name In [...]` or a `name` match label); cluster sets, other labels,
`number_of_clusters` and any rollout other than strategy `All` alone are
`InvalidArgument`. Status comes from the hub's status Policies, or reason
`SavedToGit` with no phase when no cluster has reported. Labels and
annotations take only unprefixed keys plus `openshift.io/display-name` and
`openshift.io/description`.

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/api/v1/models` | List; CEL `filter`; `order` allowlist `id`, `-id`, `metadata.name`, `-metadata.name` |
| GET | `/api/v1/models:listLocations` | Unique sorted names of the provisioned (ai-grid-ready Compliant), Available inference and combined clusters, the values `spec.placement` takes (P7) |
| GET | `/api/v1/models/{id}` | Get one (unwraps to the Model object); `status.placement.locations` when placed (P8) |
| POST | `/api/v1/models` | Create (= deploy); body is the Model object |
| PATCH | `/api/v1/models/{id}` | FieldMask update (`spec.replicas`, `spec.main.image`, `spec.main.env`, `spec.placement`, `spec.rollout`, `metadata.labels`, `metadata.annotations`) |
| DELETE | `/api/v1/models/{id}` | Refuses while a subscription in the repository references the model |

### Create body (key fields)

```json
{
  "id": "facebook-opt-125m",
  "metadata": {
    "name": "facebook-opt-125m",
    "annotations": {
      "openshift.io/display-name": "Facebook OPT 125M"
    }
  },
  "spec": {
    "model": {"uri": "hf://facebook/opt-125m", "name": "facebook/opt-125m"},
    "replicas": 1,
    "base_refs": ["v3-6-0-kserve-config-llm-template-cpu"],
    "main": {
      "image": "quay.io/example/vllm:latest",
      "requests": {"cpu": "100m", "memory": "8Gi"},
      "limits": {"cpu": "1", "memory": "10Gi"}
    },
    "placement": {
      "label_selector": {
        "match_expressions": [
          {"key": "name", "operator": "In", "values": ["site-a", "site-b"]}
        ]
      }
    }
  }
}
```

Without `placement`, the model is on no cluster (only its catalog entry is
delivered). Gateway defaults to
`openshift-ingress/openshift-ai-inference`. Display name / description live in
`openshift.io/display-name` and `openshift.io/description` annotations.

gRPC: `ai_grid.v1.Models/{List,Get,Create,Update,Delete}`.

---

## 5. Subscriptions (MaaS entitlements)

Each subscription is a `MaaSSubscription` (`maas.opendatahub.io/v1alpha1`)
committed to the backend's Git repository, `catalog/subscriptions/<id>.yaml`,
which Argo CD delivers to the ingress clusters in
`AI_GRID_SUBSCRIPTION_NAMESPACE` (default `models-as-a-service`).
OSAC shape: `id` + `metadata` + `spec` + `status`. `id` equals `metadata.name`.

`spec.organization.id` is required. `spec.owner.groups` take bare group names
(a group already prefixed with the same organization is accepted; another
organization's prefix is `InvalidArgument`); the file holds
`<organization>/<group>`, and reads return the bare names. A model ref without
a namespace gets the subscription namespace, and a ref in that namespace must
name an existing model (`FailedPrecondition` otherwise). Labels and annotations take only
unprefixed keys plus `openshift.io/display-name` and
`openshift.io/description`.

**AuthPolicy pairing (server side-effect):** the same file holds a managed
`MaaSAuthPolicy` named `<id>-policy` (subjects from `owner`, model refs
without rate limits). Labels:
`app.kubernetes.io/managed-by=ai-grid-provider-backend` and
`ai-grid/subscription=<id>`. Update rewrites it; Delete removes the file and
both objects with it. There is no separate AuthPolicies RPC and no
`create_auth_policy=false` path.

**Get / Create response shape (REST):** unlike Clusters/Tenants unwrap,
these return the full gRPC response wrapper:

- `GET /subscriptions/{id}` → `{ object, authPolicies, modelRegistrations }`
- `POST /subscriptions` → `{ object, authPolicy }`
- `PATCH` still unwraps to the Subscription (`response_body: object`)
- `GET /subscriptions` list remains `{ size, total, items }`

`authPolicies` on Get is the subscription's managed policy. `modelRegistrations`
has one entry per model ref, with phase `Ready` or `Pending` from the hub's
status Policies, or reason `SavedToGit` and no phase when no cluster has
reported.

**Go client note:** `client.GetSubscription` returns only the Subscription
`object` (same unwrap as Clusters/Tenants helpers). REST `GET
/api/v1/subscriptions/{id}` returns the full join (`object` + `authPolicies` +
`modelRegistrations`). Callers that need the join should use REST or the raw
gRPC `Subscriptions/Get` response, not the Go helper.

Display name / description for the subscription itself stay in
`metadata.annotations` (`openshift.io/display-name`,
`openshift.io/description`). Optimistic concurrency uses `metadata.version`
(int32, the file's `ai-grid/version` annotation, incremented on every write);
BFF maps that to string ETags.

Subscription ids must leave room for the `-policy` suffix (max 56 chars).

**List `filter` / `order`:** applied as filter → order → offset/limit.
`total` is the match count after filter.

- `filter`: CEL over `this` (a Subscription, proto field names) and `now`, with
  cel-go plus `ext.Strings()`. Empty means all. Example:
  `this.spec.priority >= 10 && this.id.lowerAscii().contains("eng")`.
  Indexing a missing map key fails the whole List (`InvalidArgument`); guard with
  `"k" in this.metadata.labels && …`.
- `order`: empty means `id` ascending. Allowed: `id`, `-id`, `priority`,
  `-priority`, `metadata.name`, `-metadata.name` (`-` = descending). Anything
  else is `InvalidArgument`.

### `Subscription.spec`

| Field | Type | Notes |
| --- | --- | --- |
| `owner.groups` | string[] | IdP / Kubernetes group names |
| `owner.users` | string[] | Optional user names |
| `model_refs[]` | object | See below |
| `priority` | int32 | Relative priority among subscriptions |
| `organization.id` | string | Optional; set once, then immutable |
| `token_metadata` | object | `organization_id`, `cost_center`, `labels` |
| `inference_priority` | int32 (optional) | Inference scheduling priority |
| `guardrails[]` | object | `{ ref_name, checks[] }` |

### `Subscription.spec.model_refs[]`

| Field | Type | Notes |
| --- | --- | --- |
| `name` | string | MaaSModelRef name |
| `namespace` | string | Defaults to the subscription namespace on write |
| `token_rate_limits[]` | object | Required unless `unlimited` |
| `unlimited` | bool | Mutually exclusive with `token_rate_limits` |
| `billing_rate.per_token` | string | Optional |
| `guardrails[]` | object | Per-model guardrails |

### `Subscription.status`

| Field | Type | Notes |
| --- | --- | --- |
| `phase` | string | `Active` or `Pending` from the hub's status Policies; empty when no ingress cluster has reported |
| `conditions[]` | Condition | Not read back; empty |
| `model_ref_statuses[]` | ResourceRefStatus | Not read back; empty |
| `token_rate_limit_statuses[]` | object | Not read back; empty |

### Example create

```bash
curl -sS -X POST 'http://localhost:8081/api/v1/subscriptions' \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "free-subscription",
    "metadata": {"name": "free-subscription"},
    "spec": {
      "owner": {"groups": ["system:authenticated"]},
      "organization": {"id": "org-uuid"},
      "model_refs": [{
        "name": "facebook-opt-125m-simulated",
        "token_rate_limits": [{"limit": "100", "window": "1m"}]
      }],
      "priority": 10
    }
  }' | jq .
# → { "object": { ... }, "authPolicy": { "id": "free-subscription-policy", ... } }
```

gRPC: `ai_grid.v1.Subscriptions/{List,Get,Create,Update,Delete}`.

Planning notes: [`../context/maas-subscriptions-planning.md`](../context/maas-subscriptions-planning.md).

---

## 6. What is not in this API (yet)

Do not expect these from provider-backend today:

- Browser login / token exchange / session APIs
- User create/update/delete, groups (read-only org member list exists)
- Separate AuthPolicies RPC / optional `create_auth_policy=false`
- API keys (dataplane `maas-api`)
- Nested collection URLs (everything is flat under `/api/v1/...`; member list uses `:listMembers`)
- Evaluated `filter` / `order` on Clusters and Tenants (Models and Subscriptions evaluate both)
- Console PoC-flat Model / Subscription shapes (OSAC `id`/`metadata`/`spec`/`status` is the API)
- PoC-shaped flat `GridCluster` arrays or PoC tenant + IdP config blobs
- Per-model ExternalModel / grid-gateway publication (platform bootstrap via acm-models)
- `flowControlStatuses` on Subscription status

For PoC → production mapping notes see
[`POC_API_PAYLOAD_UI_MAPPING.md`](./POC_API_PAYLOAD_UI_MAPPING.md) and
[`POC_API_CONFORMANCE.md`](./POC_API_CONFORMANCE.md) (conformance doc may lag;
this file is the current contract snapshot).

---

## 7. Local / OpenShift try-out

```bash
# Local without a cluster: commits to bin/dev-catalog.git (README "Developing")
make dev-fake
# REST :9092, gRPC :9090 — curl /api/v1/models, /api/v1/subscriptions

# Local against a real kubeconfig + Keycloak, committing to a repository
AI_GRID_GIT_URL=file:///path/to/catalog.git KEYCLOAK_URL=http://127.0.0.1:8180 make run
# Swagger: http://localhost:8081/docs/

# OpenShift (after make openshift-up)
make smoke-openshift
# or:
oc -n ai-grid port-forward svc/ai-grid-provider-backend 8080:8080 8081:8081
```

Example curls:

```bash
curl -sS 'http://localhost:8081/api/v1/ping?name=UI'
curl -sS 'http://localhost:8081/api/v1/clusters'
curl -sS 'http://localhost:8081/api/v1/tenants'
curl -sS 'http://localhost:8081/api/v1/models'
curl -sS 'http://localhost:8081/api/v1/subscriptions'

curl -sS -X POST 'http://localhost:8081/api/v1/clusters' \
  -H 'Content-Type: application/json' \
  -d '{"id":"demo-cluster","metadata":{"name":"demo-cluster","labels":{"ai-grid/enabled":"false"}},"spec":{"hub_accepts_client":true}}'

curl -sS -X PATCH 'http://localhost:8081/api/v1/clusters/demo-cluster' \
  -H 'Content-Type: application/json' \
  -d '{"id":"demo-cluster","metadata":{"labels":{"ai-grid/enabled":"true"}},"spec":{"hub_accepts_client":true}}'

# Models (Create commits the model to the Git catalog)
curl -sS -X POST 'http://localhost:8081/api/v1/models' \
  -H 'Content-Type: application/json' \
  -d '{"id":"facebook-opt-125m","metadata":{"name":"facebook-opt-125m","annotations":{"openshift.io/display-name":"Facebook OPT 125M"}},"spec":{"model":{"uri":"hf://facebook/opt-125m","name":"facebook/opt-125m"},"replicas":1,"main":{"image":"quay.io/example/vllm:latest","requests":{"cpu":"100m","memory":"1Gi"},"limits":{"cpu":"1","memory":"2Gi"}}}}'

curl -sS 'http://localhost:8081/api/v1/models/facebook-opt-125m'
curl -sS -X PATCH 'http://localhost:8081/api/v1/models/facebook-opt-125m?update_mask=spec.replicas' \
  -H 'Content-Type: application/json' \
  -d '{"id":"facebook-opt-125m","metadata":{"name":"facebook-opt-125m"},"spec":{"replicas":2}}'
curl -sS -X DELETE 'http://localhost:8081/api/v1/models/facebook-opt-125m'

# Subscriptions create returns { object, authPolicy }; see Example create above
curl -sS -X POST 'http://localhost:8081/api/v1/subscriptions' \
  -H 'Content-Type: application/json' \
  -d '{"id":"smoke-sub","metadata":{"name":"smoke-sub"},"spec":{"organization":{"id":"smoke-org"},"owner":{"groups":["smoke"]},"model_refs":[{"name":"tiny-llm","token_rate_limits":[{"limit":50,"window":"1m"}]}],"priority":5}}'
```

---

## 8. gRPC names (optional)

If the BFF speaks gRPC instead of REST:

| Service | RPC |
| --- | --- |
| `ai_grid.v1.AiGrid` | `Ping` |
| `ai_grid.v1.Clusters` | `List`, `Get`, `Create`, `Update`, `Delete` |
| `ai_grid.v1.Tenants` | `List`, `Get`, `Create`, `Update`, `Delete` |
| `ai_grid.v1.Models` | `List`, `Get`, `Create`, `Update`, `Delete` |
| `ai_grid.v1.Subscriptions` | `List`, `Get`, `Create`, `Update`, `Delete` |

gRPC request/response messages wrap resources as `{ "object": { ... } }` for
Get/Create/Update; List uses `{ size, total, items }`. REST unwraps
Get/Create/Update as described above.
