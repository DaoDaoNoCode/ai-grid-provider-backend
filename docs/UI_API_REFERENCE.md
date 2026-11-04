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
| `GET` | `/api/v1/models:listPlacementClusters` | Unique sorted names of the provisioned, Available inference and combined clusters (`clusters`) |
| `GET` | `/api/v1/models:listLocations` | Deprecated: the same cluster names as `locations`; use `:listPlacementClusters` |
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
| `version` | int32 | Optimistic lock field; each resource's update section says how it is checked (Clusters: [PATCH](#patch-apiv1clustersid)) |

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
`6` AlreadyExists, `7` PermissionDenied, `9` FailedPrecondition, `10` Aborted,
`13` Internal, `14` Unavailable.

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
    "available_condition": "",
    "ai_grid_member": true,
    "state": "MANAGED_CLUSTER_STATE_PENDING_IMPORT",
    "state_message": "Wait for importing",
    "location": {
      "source": "MANAGED_CLUSTER_LOCATION_SOURCE_NONE"
    }
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
| `status.ai_grid_member` | The cluster is an AI Grid member: its `ai-grid/enabled` label is exactly `"true"`. Join or leave by setting or removing that label with PATCH; leaving is refused while it would revoke subscriptions' keys, strand users or models ([Roles](#roles)) |
| `status.state` | Lifecycle state, as the ACM console shows it: `MANAGED_CLUSTER_STATE_` + `READY`, `OFFLINE`, `UNKNOWN`, `PENDING_IMPORT`, `IMPORTING`, `IMPORT_FAILED`, `NOT_ACCEPTED` or `DETACHING`; `UNSPECIFIED` before the hub reports any condition (and for a state added after the client was built). Derived from `status.conditions` in the order the comment on `ManagedClusterState` gives |
| `status.state_message` | The message of the condition that decided `state`, for example `"Managed cluster is available"`; may be empty |
| `status.location` | Always set. `site_name` (`ai-grid/site-name`, trimmed); `latitude` and `longitude` in decimal degrees, both present or both absent (absent is not `0`: `0, 0` is a valid position); `source`: `MANAGED_CLUSTER_LOCATION_SOURCE_ANNOTATIONS` (the `ai-grid/latitude` / `ai-grid/longitude` annotations), `..._CLOUD_REGION` (approximate, from a known cloud region; say so in the UI) or `..._NONE` (no coordinates); `problem`: what is wrong with the annotations, to show a provider admin as is (empty when nothing is); `region`: the `region.open-cluster-management.io` claim, for example `"us-east-2"`, empty on most on-premises clusters. Wrong annotations give no coordinates, never the region's. README, "Cluster locations", has every case |
| `metadata.labels["ai-grid/enabled"]` | Enroll-style flag (string `"true"` / `"false"`); read `status.ai_grid_member` instead of comparing it |
| `metadata.labels["ai-grid/role"]` | What the hub installs on a grid member: `ingress`, `inference` or `combined` (both). Absent on a cluster that was never added. PATCH accepts only those three values; see [Roles](#roles) |

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

- **Labels**: the body's console keys (`ai-grid/*` and `location`) are the
  whole set: one on the hub but missing from the body is removed. Other keys
  are left as they are on the hub, and the body may only send them back
  unchanged (`InvalidArgument` otherwise). Annotations work the same way
  for `ai-grid/*`.
- **`hub_accepts_client`** is replaced when included in the update.
- Empty update mask (typical REST PATCH with no `update_mask` query) updates **all mutable fields** (labels + hub accepts).
- `metadata.version` is the hub `resourceVersion`: a stale one answers
  `Aborted` before anything else is checked. Query `lock=true` requires it.
- The role rules below apply.

#### Roles

`ai-grid/role` picks what the hub installs on a grid member. The hub's AI
Grid Placements select `ai-grid/enabled: "true"` clusters by it (in
ai-grid-cloud/ai-grid-cluster-gitops,
`apps/ai-grid-spokes/manifests/02-placements.yaml`, and the per-role values
in `apps/rhoai/roles/<role>.yaml`, delivered by
`apps/rhoai/hub/01-applicationset.yaml`):

| Role | Serves | What the gitops repository installs for it |
| --- | --- | --- |
| `ingress` | ingress | MaaS, the portal and the grid consumer gateway; the models' catalog entries and the subscriptions |
| `inference` | inference | the models placed on it and the grid provider gateway |
| `combined` | both | both |

Changing the role of a grid member makes GitOps install what the new role
has and, with `prune: true`, remove what only the old role had. Removing a
cluster from the grid (`ai-grid/enabled` removed, or anything but `"true"`)
takes it out of every AI Grid Placement, whatever its role. The backend only
writes the labels; it doesn't check what the hub does with them, so read the
gitops repository for what changes on the cluster.

Two consequences the rules below guard against:

- **Subscriptions.** Every ingress or combined cluster gets every
  subscription in the catalog (`catalog/subscriptions/`, the
  `acm-models-catalog-subscriptions` ApplicationSet in
  `apps/acm-models/hub/catalog-04-applicationsets.yaml`). When a cluster
  stops serving ingress, Argo CD deletes its copies of all of them (the
  resources finalizer on that Application), and maas-controller revokes the
  API keys of each deleted `MaaSSubscription` in the maas-db every cluster
  shares (opendatahub-io/models-as-a-service,
  `maas-controller/pkg/controller/maas/maassubscription_controller.go`,
  `handleDeletion`). So the keys of every subscription stop working on all
  clusters, not only on the one that changed.
- **Placed models.** A role change without inference prunes the models
  placed on the cluster. Removing the cluster from the grid leaves them
  running but unmanaged (`preserveResourcesOnDeletion` on the models
  ApplicationSet), and removing it then adding it back with another role
  would get around the role change rule.

"Serves ingress" means role `ingress` or `combined`; "serves inference",
`inference` or `combined`. A grid member is a cluster with
`ai-grid/enabled: "true"` before the PATCH. PATCH checks, in this order,
after the version, and answers the first rule that refuses:

| # | Rule | Code |
| --- | --- | --- |
| 1 | A role the body sets or changes must be `ingress`, `inference` or `combined`. No role is allowed. A role the cluster already has and the body sends back unchanged isn't judged again. | `InvalidArgument` (`3`) |
| 2 | A grid member that serves ingress stops serving it (its role changes to one without ingress, or it is removed from the grid) while the Git catalog has any subscription (`catalog/subscriptions/<id>.yaml`). The message counts the subscriptions and names up to five; delete them first. | `FailedPrecondition` (`9`) |
| 3 | A grid member that serves ingress stops serving it while no other grid member serves ingress, and the grid keeps a member: the cluster itself on a role change, any other cluster on a removal. Users reach models through ingress clusters; give another cluster role `ingress` or `combined` first. The grid's last member may be removed: that retires the grid and strands no one. | `FailedPrecondition` (`9`) |
| 4 | A grid member stops serving inference (its role changes from `inference` or `combined` to one without inference), or is removed from the grid whatever its role, while the Git catalog places models on it (`catalog/clusters/<name>/kustomization.yaml`). The message counts the models and names up to five; move them to other clusters first. | `FailedPrecondition` (`9`) |

The order puts what every cluster loses first: the subscriptions' keys,
then the ingress every model is reached through, then the cluster's own
models. When several apply, only the first is reported; once it is dealt
with, the same PATCH reports the next.

Notes:

- A cluster joining the grid, a role change of a cluster outside the grid,
  and an update that keeps a member in the grid with its role (other
  labels, annotations, `hub_accepts_client`) aren't checked.
- So retiring an ingress cluster, by a role change or a removal, needs
  every subscription deleted first. That revokes their keys too, but as a
  step the admin chooses, not as a side effect of a cluster change.
- The catalog is read once per PATCH, on the remote's head, not on the
  backend's read cache (`AI_GRID_GIT_READ_MAX_AGE`), so a model placed or a
  subscription created by another writer a moment ago counts. A backend
  without `AI_GRID_GIT_URL` has no catalog, so no subscription or model
  refuses a PATCH; one whose repository isn't cloned yet, or can't be
  fetched, answers `Unavailable` (`14`) for a PATCH rules 2 or 4 apply to.
- The checks and the write are not atomic: a subscription created, a model
  placed on the cluster, or another cluster's role or membership changed,
  between the check and the write is not refused.
- Messages, for example:
  - `label "ai-grid/role" must be ingress, inference or combined, got "gateway"`
  - `Cluster "site-a" serves ingress, and 3 subscriptions exist (sub-a, sub-b, sub-c). When a cluster stops serving ingress, the API keys of every subscription are revoked on all clusters. Delete the subscriptions first, or keep the ingress or combined role.`
  - `Cluster "site-a" serves ingress, and 1 subscription exists (sub-a). When a cluster stops serving ingress, the API keys of every subscription are revoked on all clusters. Delete the subscription first, or keep the cluster in AI Grid.` (a removal)
  - `Cluster "site-a" is the only AI Grid cluster that serves ingress, and users reach models through ingress clusters. Give another cluster the ingress or combined role first.`
  - `Cluster "site-a" has 2 models placed on it (model-a, model-b). Move them to other clusters before changing it to a role without inference.`
  - `Cluster "site-b" has 2 models placed on it (model-a, model-b). Move them to other clusters before removing it from AI Grid.`

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

Reads of Models and Subscriptions come from a fetch of the repository at most
`AI_GRID_GIT_READ_MAX_AGE` old (default 10 seconds). The backend's own writes
show in the next read at once. A commit made outside the backend, such as a
hand edit or another replica's write, shows within about that long, plus
the time one fetch takes; until then `List` and `Get` answer from the older
state, so a client may see:

- a model or subscription created elsewhere missing from `List`, and `Get`
  answer `NotFound` for it;
- one deleted elsewhere still listed, and returned by `Get`;
- one changed elsewhere at its older `metadata.version`. An update or delete
  sent with that version answers `Aborted`, as for any version that no
  longer matches; that write fetched, so a reload shows the newer version.

Writes don't answer from such a state. Within the window a write commits on
the head it read without fetching first; the push lands only if the branch
hasn't moved since, and otherwise the backend makes the write again on a
freshly fetched head, so a stale read never overwrites a newer commit. When
the state a write read refuses it (`NotFound`, `AlreadyExists`, `Aborted`
for a version, or `FailedPrecondition`), the backend fetches and decides
once more before answering.

`spec.placement` is required and names at least one cluster, by the ACM
`name` label only (`name In [...]` or a `name` match label); cluster sets, other labels,
`number_of_clusters` and any rollout other than strategy `All` alone are
`InvalidArgument`. Each cluster a create or update adds must be a
ManagedCluster on the hub, or the call is `InvalidArgument` naming it, with a
`google.rpc.ResourceInfo` detail per unknown cluster (`resource_type`
`type.googleapis.com/ai_grid.v1.ManagedCluster`, `resource_name` the cluster); a
cluster the model is already on is not checked again, so a model on a cluster
that has left the hub can still be changed or moved off it.
`:listPlacementClusters` lists the clusters a model can run on. Status comes
from the hub's catalog Argo CD Applications, or reason
`SavedToGit` with no phase when no cluster has reported. Once one has,
`status.phase` is `MODEL_PHASE_READY`, `MODEL_PHASE_PARTIALLY_AVAILABLE`
(ready on some placed clusters, not all, and published on every ingress
cluster), `MODEL_PHASE_FAILED` (a cluster reports it failed) or
`MODEL_PHASE_PENDING`; READY and PARTIALLY_AVAILABLE need at least
one ingress cluster's report, and a cluster labelled `ai-grid/role` `ingress` or
`combined` must report even before it has an ingress Application; in that order of precedence (the comment on
`ModelPhase`), and `status.clusters` gives each placed cluster's own status
(below). `spec.main.requests` and `limits` take Kubernetes quantities with
Kubernetes' container rules: non-negative, whole numbers for extended
resources such as `nvidia.com/gpu`, a request at most its limit and equal to
it for extended resources and hugepages; anything else is `InvalidArgument`.
A GPU request with no limit is saved with the request as its limit, and an
update of `spec.main.requests` alone moves that limit with the request (a
removed GPU request removes the GPU limit too).
`spec.main.image` is optional. Left empty, KServe uses the presets' image,
because an empty image in the model doesn't override theirs: with no
`spec.base_refs`, its default preset's, vLLM for NVIDIA GPUs. An image set
here replaces any preset's. A blank image, or one with a space or tab, is
`InvalidArgument`. A `base_refs` item must be a
name with no whitespace; an empty one is `InvalidArgument`. Labels and
annotations take only unprefixed keys plus `openshift.io/display-name` and
`openshift.io/description`.

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/api/v1/models` | List; CEL `filter`; `order` allowlist `id`, `-id`, `metadata.name`, `-metadata.name` |
| GET | `/api/v1/models:listPlacementClusters` | `clusters`: unique sorted names of the provisioned (ai-grid-ready Compliant), Available inference and combined clusters, the values `spec.placement` takes (P7) |
| GET | `/api/v1/models:listLocations` | Deprecated: the same cluster names, as `locations`. Use `:listPlacementClusters`; removed once clients have moved |
| GET | `/api/v1/models/{id}` | Get one (unwraps to the Model object); `status.placement` when placed: `selected_clusters` (cluster names) and `locations` (their `location` labels, P8) |
| POST | `/api/v1/models` | Create (= deploy); body is the Model object |
| PATCH | `/api/v1/models/{id}` | FieldMask update (`metadata.labels`, `metadata.annotations`, `spec.model`, `spec.replicas`, `spec.base_refs`, `spec.runtime`, `spec.router`, `spec.main`, `spec.main.image`, `spec.main.env`, `spec.main.requests`, `spec.main.limits`, `spec.scaling`, `spec.placement`, `spec.rollout`). Each path replaces its field whole; a subpath such as `metadata.labels.<key>` replaces the whole map. `ModelsUpdateRequest.update_mask` has the full rules |
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
    "model": {"uri": "hf://facebook/opt-125m"},
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

Leave `spec.model.name` out, or send the id: it's deprecated. The model is served as its id,
`facebook-opt-125m`, which is the `model` tenants send through MaaS. Any other value is
`InvalidArgument`. A read returns the name the model is served as: the id, or for a model saved
before this rule, the name it still serves until its next update switches it to the id.

A placed model's status names its clusters and, separately, where they are:

```json
"status": {
  "placement": {
    "number_of_selected_clusters": 2,
    "selected_clusters": ["site-a", "site-b"],
    "locations": ["us-east"]
  }
}
```

Once a cluster reports, the status also has the model on each placed cluster
(proposal P13), in cluster-name order:

```json
"status": {
  "phase": "MODEL_PHASE_PARTIALLY_AVAILABLE",
  "reason": "PartiallyAvailable",
  "message": "Not ready on site-a; ready on site-b; published on hub",
  "deployment": "MODEL_DEPLOYMENT_STATE_STARTING",
  "maas": "MODEL_MAAS_STATE_AWAITING_SUBSCRIPTION",
  "state": "MODEL_STATE_PARTIALLY_READY",
  "clusters": [
    {"cluster": "site-a", "phase": "MODEL_PHASE_PENDING", "reason": "NotReady", "message": "LLMInferenceService not ready",
     "deployment": "MODEL_DEPLOYMENT_STATE_STARTING", "deploymentMessage": "Deployment does not have minimum availability.",
     "sync": "MODEL_SYNC_STATE_SYNCED", "state": "MODEL_STATE_STARTING",
     "viewUrl": "https://console-openshift-console.apps.site-a.example.com/k8s/ns/ai-grid-internal-models/serving.kserve.io~v1alpha2~LLMInferenceService/granite",
     "viewTarget": "MODEL_VIEW_TARGET_OPENSHIFT_CONSOLE"},
    {"cluster": "site-b", "phase": "MODEL_PHASE_READY", "reason": "Ready", "message": "LLMInferenceService ready",
     "deployment": "MODEL_DEPLOYMENT_STATE_RUNNING", "sync": "MODEL_SYNC_STATE_SYNCED", "state": "MODEL_STATE_READY",
     "viewUrl": "https://rh-ai.apps.site-b.example.com/ai-hub/models/deployments/internal/ai-grid-internal-models",
     "viewTarget": "MODEL_VIEW_TARGET_OPENSHIFT_AI"}
  ],
  "ingress": [
    {"cluster": "hub", "state": "MODEL_MAAS_STATE_AWAITING_SUBSCRIPTION", "message": "Awaiting governance pairing"}
  ]
}
```

A cluster's `phase` is `MODEL_PHASE_READY`, `MODEL_PHASE_FAILED` or
`MODEL_PHASE_PENDING`; its `reason` is `Ready`, `Failed`, `NotReady` or
`NoReport` (no report yet); on a combined cluster the `MaaSModelRef` and
`ExternalModel` reports count too, and the message names them all, such as
`LLMInferenceService ready; MaaSModelRef not ready; ExternalModel ready`.
Ingress clusters the model isn't placed on have no entry in `clusters`.
`status.rollout` is never set.

The two statuses keep the model's deployment apart from MaaS, so a list can
show both, such as "Ready" and "Awaiting subscription":

- `deployment` on each `clusters` entry, from the `LLMInferenceService`'s
  health alone: `RUNNING`, `STARTING` (with KServe's message, such as
  `Deployment does not have minimum availability.`), `STOPPED` (KServe's
  `Service is stopped`, from the `serving.kserve.io/stop` annotation),
  `FAILED` (with the message), or `UNSPECIFIED` while it isn't on the
  cluster.
- `sync` on each `clusters` entry: `SYNCED`, `OUT_OF_SYNC` (not applied yet,
  or Argo CD is retrying; `syncMessage` says why), `FAILED` (the sync failed
  and the retries ran out; `syncMessage` says why), or `UNSPECIFIED` when the
  cluster has no Application. An edit not applied yet is `OUT_OF_SYNC` while
  the previous revision is still `RUNNING`.
- `status.deployment` is `FAILED` when one placed cluster fails or its sync
  failed, `STARTING` when one is starting or hasn't reported, `STOPPED` when
  every one is stopped, and `RUNNING` when every one runs it or is stopped,
  with at least one running. A model stopped everywhere is `PENDING`, reason
  `Stopped`, once every cluster has synced; a cluster where it is stopped has
  reason `Stopped`.
- `ingress`: one entry per ingress cluster (combined ones included), with
  `state` `AVAILABLE`, `AWAITING_SUBSCRIPTION` (routed, but no subscription
  covers it, so no tenant can call it), `AWAITING_SUBSCRIPTION_AND_BACKEND`,
  `CONFIGURING_ROUTING`, `FAILED`, or `UNSPECIFIED` with no report, and
  MaaS's message. `status.maas` is the worst of them, in the order `FAILED`,
  `AWAITING_SUBSCRIPTION_AND_BACKEND`, `CONFIGURING_ROUTING` (also while one
  hasn't reported), `AWAITING_SUBSCRIPTION`, `AVAILABLE`; `AVAILABLE` and
  `CONFIGURING_ROUTING` read `UNAVAILABLE` while no placed cluster serves the
  model.
- `state` (model and each `clusters` entry): Git sync and deployment in one
  word, for a Status column: `SAVED_TO_GIT`, `SYNCING`, `SYNCED`,
  `STARTING`, `READY`, `PARTIALLY_READY` (model only), `FAILED`, `STOPPED`.
- `view_url` and `view_target` on each `clusters` entry: OpenShift AI's model
  deployments on an ingress or combined cluster, else the
  `LLMInferenceService` in the OpenShift console; empty without an https
  console URL claim (or, on an ingress or combined cluster, the
  `ai-grid/openshift-ai-url` annotation). The contract validates a non-empty
  link as an HTTPS URI with a host and no user info, paired with a specified
  target; an empty link requires the `UNSPECIFIED` target. The summary
  `deployment`, `maas`, and `state` fields accept only defined enum values.

A model waiting only for a subscription is `MODEL_PHASE_READY` (or
`PARTIALLY_AVAILABLE`) with `status.maas` `MODEL_MAAS_STATE_AWAITING_SUBSCRIPTION`,
not `PENDING`. `STARTING` has no deadline: a pod that keeps crashing, can't
pull its image or finds no GPU also shows as `STARTING`, because Argo CD keeps
only KServe's `Ready` message, which then still says the Deployment is below
its minimum availability.

`selected_clusters` holds cluster names, the values `spec.placement` takes.
`locations` holds the distinct values of those clusters' `location` label;
several clusters can share one, so it can't be turned back into clusters, and
a placement that names a location is `InvalidArgument`.

Without `placement`, the model is on no cluster (only its catalog entry is
delivered). Gateway defaults to
`openshift-ingress/openshift-ai-inference`. Display name / description live in
`openshift.io/display-name` and `openshift.io/description` annotations, which
subscriptions show on their model refs. A subscription's model ref names this
model when its `model_id` equals the model's `id` (section 5).

gRPC: `ai_grid.v1.Models/{List,Get,Create,Update,Delete,ListPlacementClusters}`,
and the deprecated `ai_grid.v1.Models/ListLocations`.

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

**Which model a ref names:** every read, and the answer of every Create and
Update, sets `model_id` on each model ref that names a model's catalog entry
(the `MaaSModelRef` the Models API publishes for it, in the subscription
namespace), with that entry's `display_name` and `description`. Join a ref to
its model by `model_id == Model.id`; a ref with an empty `model_id` names no
model of the Models API. The UI needs no namespace of its own: to add a model,
send its `id` as `name` and leave `namespace` empty. To list the subscriptions
that use a model, filter: `this.spec.model_refs.exists(r, r.model_id == "<id>")`.

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

`authPolicies` on Get (and `authPolicy` on Create) is the subscription's
managed policy, with `phase` `Active`, `Degraded` or `Pending` and a `Ready`
condition saying where it is ready, from the hub's catalog Applications, or
no phase and no conditions when no cluster has reported. `modelRegistrations`
has one entry per model ref, with phase `Ready`, `Failed` or `Pending` from the hub's
catalog Applications, or reason `SavedToGit` and no phase when no cluster has
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
| `organization.id` | string | Required: the organization's id (the Tenant `id`, a Keycloak organization UUID), a DNS label, else `InvalidArgument`. Immutable: an Update that changes or removes it is `InvalidArgument` |
| `token_metadata` | object | `organization_id`, `cost_center`, `labels` |
| `inference_priority` | int32 (optional) | Inference scheduling priority |
| `guardrails[]` | object | `{ ref_name, checks[] }` |

### `Subscription.spec.model_refs[]`

| Field | Type | Notes |
| --- | --- | --- |
| `name` | string | MaaSModelRef name; for a model of the Models API, its `id` |
| `namespace` | string | Defaults to the subscription namespace on write (where the Models API publishes each model's MaaSModelRef, not `AI_GRID_MODELS_NAMESPACE`); reads return it set |
| `model_id` | string | Output only, ignored on write. The `id` of the Model whose catalog entry this ref names; empty for a ref to any other MaaSModelRef |
| `display_name` | string | Output only, ignored on write. The model's `openshift.io/display-name` annotation, from its MaaSModelRef; set only with `model_id`, empty when the model has none |
| `description` | string | Output only, ignored on write. The model's `openshift.io/description` annotation, as `display_name` |
| `token_rate_limits[]` | object | Required unless `unlimited`. `{ limit, window }`: `limit` 1 to 1000000000; `window` matches `^[1-9]\d{0,3}(s\|m\|h)$` and is at most `8784h` (366 days, the MaaS maximum), else `InvalidArgument` |
| `unlimited` | bool | Mutually exclusive with `token_rate_limits` |
| `billing_rate.per_token` | string | Optional |
| `guardrails[]` | object | Per-model guardrails |

### `Subscription.status`

| Field | Type | Notes |
| --- | --- | --- |
| `phase` | string | `Active`, `Pending` or `Degraded` from the hub's catalog Applications; empty when no cluster has reported |
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
  -d '{"id":"facebook-opt-125m","metadata":{"name":"facebook-opt-125m","annotations":{"openshift.io/display-name":"Facebook OPT 125M"}},"spec":{"model":{"uri":"hf://facebook/opt-125m"},"replicas":1,"main":{"image":"quay.io/example/vllm:latest","requests":{"cpu":"100m","memory":"1Gi"},"limits":{"cpu":"1","memory":"2Gi"}}}}'

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
