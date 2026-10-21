# AI Grid Provider API — UI reference

Handout for console / BFF work against **ai-grid-provider-backend** as shipped
today. This describes the **REST** surface (`grpc-gateway`). Field names are
**snake_case** in JSON.

| | |
| --- | --- |
| Base path | `/api/v1` |
| Content-Type | `application/json` |
| OpenAPI | [`openapi/ai_grid.swagger.json`](./openapi/ai_grid.swagger.json) |
| Live Swagger UI | `http://<host>:8081/docs/` (or the OpenShift HTTP Route `/docs/`) |
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

### List query parameters (Clusters + Tenants)

| Param | Type | Notes |
| --- | --- | --- |
| `offset` | int32 | Start index; default `0` |
| `limit` | int32 | Max items; omit = all |
| `filter` | string | CEL filter — **accepted, not evaluated yet** |
| `order` | string | Sort — **accepted, not evaluated yet** |

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
      "metadata": { "name": "tenant-a" },
      "spec": { "domains": ["tenant-a.example.com"] },
      "status": {}
    },
    {
      "id": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
      "metadata": { "name": "tenant-b" },
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
  "metadata": { "name": "acme" },
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
    "name": "acme"
  },
  "spec": {
    "domains": ["acme.example.com", "acme.io"]
  }
}
```

**Behavior today:**

- Empty update mask (typical gRPC with no `update_mask`) updates **name + domains**.
  REST PATCH with no explicit mask typically derives a body mask from fields present
  in the JSON — that is **not** identical to an empty-mask Go/gRPC update.
- Supported mask paths: `metadata`, `metadata.name`, `spec`, `spec.domains`
  (optional `object.` prefix). Unsupported paths return `InvalidArgument`.
- Domains in the body **replace** the domain list when domains are updated.
  Blank/whitespace-only domain entries are rejected (`InvalidArgument`), not dropped.
- Optional query `lock` — **not enforced yet**.

**Response `200`** — updated `Tenant`.

---

### `DELETE /api/v1/tenants/{id}`

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

## 4. What is not in this API (yet)

Do not expect these from provider-backend today:

- Browser login / token exchange / session APIs
- User create/update/delete, groups, subscriptions (read-only org member list exists)
- Models / placements / locations
- Nested collection URLs (everything is flat under `/api/v1/...`; member list uses `:listMembers`)
- Evaluated `filter` / `order` / optimistic `lock`
- PoC-shaped flat `GridCluster` arrays or PoC tenant + IdP config blobs

For PoC → production mapping notes see
[`POC_API_PAYLOAD_UI_MAPPING.md`](./POC_API_PAYLOAD_UI_MAPPING.md) and
[`POC_API_CONFORMANCE.md`](./POC_API_CONFORMANCE.md) (conformance doc may lag;
this file is the current contract snapshot).

---

## 5. Local / OpenShift try-out

```bash
# Local (point KEYCLOAK_URL at a Keycloak with Organizations for Tenants)
KEYCLOAK_URL=http://127.0.0.1:8180 make run
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

curl -sS -X POST 'http://localhost:8081/api/v1/clusters' \
  -H 'Content-Type: application/json' \
  -d '{"id":"demo-cluster","metadata":{"name":"demo-cluster","labels":{"ai-grid/enabled":"false"}},"spec":{"hub_accepts_client":true}}'

curl -sS -X PATCH 'http://localhost:8081/api/v1/clusters/demo-cluster' \
  -H 'Content-Type: application/json' \
  -d '{"id":"demo-cluster","metadata":{"labels":{"ai-grid/enabled":"true"}},"spec":{"hub_accepts_client":true}}'
```

---

## 6. gRPC names (optional)

If the BFF speaks gRPC instead of REST:

| Service | RPC |
| --- | --- |
| `ai_grid.v1.AiGrid` | `Ping` |
| `ai_grid.v1.Clusters` | `List`, `Get`, `Create`, `Update`, `Delete` |
| `ai_grid.v1.Tenants` | `List`, `Get`, `Create`, `Update`, `Delete` |

gRPC request/response messages wrap resources as `{ "object": { ... } }` for
Get/Create/Update; List uses `{ size, total, items }`. REST unwraps
Get/Create/Update as described above.
