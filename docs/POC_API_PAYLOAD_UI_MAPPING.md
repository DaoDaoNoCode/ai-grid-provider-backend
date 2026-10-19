# PoC API ↔ UI payload mapping (for ai-grid-provider-backend)

Translation reference: maps **every** PoC admin-portal page to the REST contracts it uses, so the same capabilities can be implemented under `ai-grid-provider-backend` (`/api/v1/`, proto + grpc-gateway).

| Item | Path / note |
| --- | --- |
| PoC folder used | `ai-grid-cloud-provider-poc` (**cloud**, not “clouse”) |
| PoC Node Git API | `ai-grid-cloud-provider-poc/backend/` (default `127.0.0.1:18080`, base `/api`) |
| PoC UI | `ai-grid-cloud-provider-poc/src/` (Vite; default `http://127.0.0.1:15173`) |
| Shared Zod contracts | `ai-grid-cloud-provider-poc/shared/*.mjs` |
| PoC OpenAPI | `ai-grid-cloud-provider-poc/docs/openapi.json` |
| Sibling Users/Keycloak API | Outside PoC repo: `RedHat/keycloak/api` (proxied as `/api/users` → `:18082`) |
| This backend today | Ping, Clusters (ManagedCluster CRUD), Tenants under `/api/v1/` — see [`API.md`](./API.md) |
| Source of truth | **Code** (handlers + Zod + UI clients). OpenAPI/README noted where they disagree. |

---

## Table of contents

1. [PoC architecture overview](#1-poc-architecture-overview)
2. [Cross-cutting conventions](#2-cross-cutting-conventions)
3. [Domain: Health & docs](#3-domain-health--docs)
4. [Domain: Cluster management](#4-domain-cluster-management)
5. [Domain: Tenant management](#5-domain-tenant-management)
6. [Domain: Auth (login)](#6-domain-auth-login)
7. [Domain: User management](#7-domain-user-management)
8. [Domain: Model management](#8-domain-model-management)
9. [Domain: Locations](#9-domain-locations)
10. [Domain: Subscription management](#10-domain-subscription-management)
11. [Translation notes for ai-grid-provider-backend](#11-translation-notes-for-ai-grid-provider-backend)
12. [Appendix: shared types & removed routes](#12-appendix-shared-types--removed-routes)
13. [Gaps & discrepancies](#13-gaps--discrepancies)

---

## 1. PoC architecture overview

```text
Browser (React/PatternFly SPA)
  │
  ├─ /auth/token  ──Vite proxy──► Keycloak token endpoint (:18081)
  ├─ /api/users*  ──Vite proxy──► Go Keycloak Users API (:18082)
  └─ /api/*       ──Vite proxy──► Node Git portal API (:18080)
         │
         ├─ GitResourceStore (data/maas-git) — subscriptions, models, tenants, policies
         └─ ACM Hub or GitClusterClient — ManagedCluster list / enroll labels
```

| Layer | Tech |
| --- | --- |
| UI | React 18, Vite 7, React Router 7, PatternFly 6, Zod |
| Portal API | Node.js ≥22 ESM (`node:http`), Zod validation, Git filesystem store |
| Users API | Go HTTP + Keycloak Admin API (sibling repo) |
| Service selection | Portal: `VITE_SUBSCRIPTION_SERVICE=http` → real API; else mocks. Users always HTTP. |
| API base (UI) | `VITE_API_BASE_URL` default `/api` |

**UI routes** (`src/app/App.tsx`):

| Path | Page |
| --- | --- |
| `/` | Redirect → `/clusters` |
| `/clusters` | Clusters |
| `/tenants`, `/tenants/create`, `/tenants/:tenantName/edit`, `/tenants/view/:tenantName` | Tenants |
| `/login`, `/users`, `/users/create`, `/users/:username` | Auth / Users |
| `/models`, `/models/deploy` | Models |
| `/subscriptions`, `/subscriptions/create`, `/subscriptions/edit/:name`, `/subscriptions/view/:name` | Subscriptions |

Clients: `HttpSubscriptionService` (portal), `HttpUserService` (users), Keycloak password grant (login).

---

## 2. Cross-cutting conventions

### 2.1 Portal API (Node `/api`)

| Convention | Behavior |
| --- | --- |
| Auth | **None** on browser → Node API (PoC loopback). ACM Hub uses server-side bearer. |
| Content-Type | Body must be `application/json*` → else **415** |
| Body size | >1 MiB → **413** |
| Invalid JSON | **400** |
| Namespace | Backend-only (`MAAS_NAMESPACE`, default `maas`). Request `namespace` → **422**. Responses strip `namespace`. |
| Concurrency | Mutating PUT/DELETE send `If-Match: JSON.stringify(revision)`. Missing → **428**, stale → **412**. |
| Create success | **201** + `ETag` + `Location` |
| Delete success | **204** empty |
| Pagination | **None** — full arrays |

**Error shape** (portal):

```json
{
  "code": "validation|not_found|conflict|unavailable|unauthorized|forbidden",
  "message": "Human-readable message",
  "fieldErrors": { "path.to.field": "message" }
}
```

`fieldErrors` optional. OpenAPI `Error.code` enum omits `unauthorized` / `forbidden` (used for ACM Hub failures). UI maps these to `ServiceError` and usually shows `message` only.

### 2.2 Users API (Go `/api/users`)

| Convention | Behavior |
| --- | --- |
| Auth | `Authorization: Bearer <Keycloak JWT>` |
| Admin | Group `grid-admins` or realm role `ai-admin` |
| Error shape | `{ "error": "string" }` (not portal `code`/`message`) |
| UI client | `authFetch` attaches Bearer from `sessionStorage` key `ai-grid-keycloak-token` |

### 2.3 Field naming

PoC JSON is predominantly **camelCase** (`aiGridEnabled`, `displayName`, `modelRefs`). Users API mixes camelCase profile fields with **snake_case** `tenant_id`. Production `ai-grid-provider-backend` standards use **snake_case** proto JSON — map deliberately when translating.

---

## 3. Domain: Health & docs

### UI usage

No dedicated page. Gear menu → **REST API** opens `/api/docs`. Health is for ops/smoke tests.

### Endpoints

#### `GET /api/health`

| | |
| --- | --- |
| Request | None |
| Response 200 | `{ "status": "ok", "storage": "git", "autoCommit": false }` |

#### `GET /api/openapi.json` / `GET /api/docs`

Serve OpenAPI + Swagger UI. Not listed in OpenAPI `paths`.

### Translation notes

Map to existing `Ping` / readiness patterns under `/api/v1/ping`. Do not require Git-specific `storage` / `autoCommit` fields in production.

---

## 4. Domain: Cluster management

### UI pages

| Route | Component | Behavior |
| --- | --- | --- |
| `/clusters` | `ClustersPage.tsx` | Lists clusters; **Add to AI Grid** enrolls |

### Endpoints

#### `GET /api/clusters`

**Response 200:** `GridCluster[]`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `name` | string | yes | DNS-ish cluster name |
| `status` | `Ready` \| `Offline` \| `Unknown` | yes | From ManagedCluster Available condition |
| `aiGridEnabled` | boolean | yes | Label `ai-grid/enabled` == `"true"` |
| `provisioned` | boolean | yes | Label `ai-grid/provisioned` == `"true"` |
| `provisioningStatus` | `Provisioned` \| `Provisioning` \| `Not provisioned` | yes | |
| `location` | string | no | Location label for model placement |

Example:

```json
[
  {
    "name": "site1",
    "status": "Ready",
    "aiGridEnabled": true,
    "provisioned": true,
    "provisioningStatus": "Provisioned",
    "location": "site1"
  }
]
```

**UI mapping:** Table columns bind 1:1. Enroll button shown when not `aiGridEnabled`.

#### `PATCH /api/clusters/{name}`

**Path:** `name` — cluster name  
**Body (required, strict):**

```json
{ "aiGridEnabled": true }
```

Only literal `true` accepted; extra fields → 422.

**Response 200:** single `GridCluster`  
**Errors:** 404, 409 (Hub), 401/403 (Hub), 422, 503

**UI mapping:** Button “Add to AI Grid” → fixed body `{ aiGridEnabled: true }` (no form). Optimistic local “confirmed” set on success.

### Source

- Backend: `backend/clusters.mjs`, Zod `shared/cluster-validation.mjs`
- UI: `HttpSubscriptionService.listClusters` / `enrollCluster`

### Translation notes

**Implemented** in `ai-grid-provider-backend` as a console presentation facade (Option A):

| PoC | provider-backend |
| --- | --- |
| `GET /api/clusters` → `GridCluster[]` | `GET /api/v1/clusters` → OSAC `ManagedCluster` list envelope (`items`/`size`/`total`) |
| `PATCH /api/clusters/{name}` `{ "aiGridEnabled": true }` | `PATCH /api/v1/clusters/{id}` with ManagedCluster body (label merge via `metadata.labels`) |
| Flat camelCase fields | Flat **snake_case** JSON (`UseProtoNames`); same field set |

Mapping (ManagedCluster → GridCluster): `status` from Available condition (`True`→Ready, `False`→Offline, else Unknown); `ai_grid_enabled` / `provisioned` / `location` from labels (`ai-grid/enabled`, `ai-grid/provisioned`, `location`); `provisioning_status` = Provisioned \| Provisioning \| Not provisioned (same rules as PoC).

OSAC ManagedCluster CRUD is at `/api/v1/clusters`. Enroll-style updates are label merges on `metadata.labels` (`ai-grid/enabled=true`).

**Remaining gaps vs PoC:** strict Zod reject of extra enroll fields (gateway `DiscardUnknown`); snake_case vs camelCase wire names (console must map or prefer snake_case).

---

## 5. Domain: Tenant management

### UI pages

| Route | Component | APIs |
| --- | --- | --- |
| `/tenants` | `TenantListPage` | `GET` list, `DELETE` |
| `/tenants/create` | `TenantWizardPage` | `POST` |
| `/tenants/:tenantName/edit` | `TenantWizardPage` | `GET` + `PUT` |
| `/tenants/view/:tenantName` | `TenantDetailsPage` | `GET` |

Wizard steps: details (name, domains) → OIDC IdP config → review.

### Endpoints

#### `GET /api/tenants` → `Tenant[]`

#### `GET /api/tenants/{name}` → `Tenant` + `ETag`

#### `POST /api/tenants` → **201** `Tenant` + `ETag` + `Location`

#### `PUT /api/tenants/{name}` + `If-Match` → **200** `Tenant` + `ETag`

#### `DELETE /api/tenants/{name}` + `If-Match` → **204**

On create/delete, Node may sync Keycloak org via sibling Users API (`POST/DELETE /api/tenants` on `:18082`). Sync failure → **502** `{ "code": "unavailable", ... }` (not in OpenAPI).

### Request: `TenantInput` (strict)

| Field | Type | Required | UI form field |
| --- | --- | --- | --- |
| `name` | string 1–63, `[a-z0-9]([-a-z0-9]*[a-z0-9])?` | yes | Tenant name (disabled on edit) |
| `domains` | string[] ≤64, unique lowercase FQDNs (may be empty) | yes | Domains multi-value |
| `identityProvider.title` | string ≤253 | no (default `""`) | IdP title |
| `identityProvider.description` | string ≤4000 | no | IdP description |
| `identityProvider.enabled` | literal `true` | no (default true) | Always enabled |
| `identityProvider.config.clientId` | string 1–253 | yes | Client ID |
| `identityProvider.config.clientSecretSecret.name` | secret ref name | yes | Secret **reference** (never raw secret) |
| `identityProvider.config.issuer` | http(s) URL | yes | Issuer |
| `identityProvider.config.authorizationUrl` | http(s) URL | yes | Authorization URL |
| `identityProvider.config.tokenUrl` | http(s) URL | yes | Token URL |
| `identityProvider.config.defaultScopes` | string | no | Default scopes |
| `identityProvider.config.userInfoUrl` | `""` or URL | no | UserInfo URL |
| `identityProvider.config.jwksUrl` | `""` or URL | no | JWKS URL |
| `identityProvider.config.logoutUrl` | `""` or URL | no | Logout URL |
| `identityProvider.config.validateSignature` | boolean | no (default true) | Validate signature |

Example request:

```json
{
  "name": "tenant1",
  "domains": ["tenant1.example.com"],
  "identityProvider": {
    "title": "Tenant 1 SSO",
    "description": "Tenant 1 identity provider",
    "config": {
      "clientId": "tenant1-client",
      "clientSecretSecret": { "name": "tenant1-oidc-secret" },
      "issuer": "https://idp.example.com/realms/tenant1",
      "authorizationUrl": "https://idp.example.com/auth",
      "tokenUrl": "https://idp.example.com/token"
    }
  }
}
```

### Response: `Tenant`

`TenantInput` fields plus:

| Field | Type | Notes |
| --- | --- | --- |
| `status` | `"Saved"` | PoC Git-only status |
| `revision` | string | Opaque; use in `If-Match` |
| `creationTimestamp` | ISO-8601 | |
| `updatedAt` | ISO-8601 | |

### Errors

409 duplicate name / overlapping domains; 422 validation / rename; 428/412 concurrency; 502 Keycloak sync; 503 store.

### Source

- Zod: `shared/tenant-validation.mjs`
- Backend: `backend/tenants.mjs`, `backend/keycloakSync.mjs`
- UI: `TenantFormWizard`, `IdentityProviderFields`

### Translation notes

Current provider-backend `Tenant` is Keycloak Organization–backed with `spec.domains` only — **no IdP block**. PoC portal tenants are Git CRs with full OIDC config + optional Keycloak org sync. Decide whether production:

1. Extends `TenantSpec` with IdP config, or  
2. Keeps org CRUD separate from IdP onboarding, or  
3. Unifies with sibling Users API `/api/tenants` org endpoints.

Do not accept raw client secrets; keep secret **name** references.

---

## 6. Domain: Auth (login)

### UI page

| Route | Component |
| --- | --- |
| `/login` | `LoginPage.tsx` |

### Endpoint (dev helper, not Node portal)

#### `POST /auth/token` (Vite rewrite → Keycloak)

**Content-Type:** `application/x-www-form-urlencoded`

| Form field | Value |
| --- | --- |
| `grant_type` | `password` |
| `client_id` | `VITE_KEYCLOAK_CLIENT_ID` (default `ai-model-gateway`) |
| `client_secret` | `VITE_KEYCLOAK_CLIENT_SECRET` |
| `username` | Login form username |
| `password` | Login form password |
| `scope` | `openid organization:*` |

**Response used:** `{ access_token, expires_in }` → stored as `{ accessToken, expiresAt, username }`.  
**Errors:** Keycloak `error` / `error_description` shown in Alert.

### Translation notes

Password-grant login is **dev-only**. Production should use proper OIDC (browser redirect / device flow). Users API still expects Bearer JWTs.

---

## 7. Domain: User management

Implemented by sibling Go API (`RedHat/keycloak/api`), not the PoC Node backend. Documented here because the PoC UI depends on it and provider-backend may absorb or replace it.

### UI pages

| Route | Component | APIs used |
| --- | --- | --- |
| `/users` | `UsersPage` | `GET /api/users`, `DELETE /api/users/{username}` |
| `/users/create` | `UserFormPage` | `POST /api/users` |
| `/users/:username` | `UserDetailsPage` | `GET`, `PUT .../claims`, `PUT .../groups` |

**Client methods exist but unused by UI:** `GET /api/users/me`, `PUT /api/users/{username}` (profile update).

### Auth header

```http
Authorization: Bearer <access_token>
Accept: application/json
Content-Type: application/json
```

### Types

**`UserView` response:**

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | Keycloak user id |
| `username` | string | |
| `email` | string? | |
| `firstName` | string? | |
| `lastName` | string? | |
| `enabled` | boolean | |
| `tenant_id` | string? | Allowlisted claim / org |
| `groups` | string[] | |
| `claims` | `Record<string,string>`? | |

**`Me` response** (`GET /api/users/me` — typed in UI, unused by pages):

```json
{
  "subject": "...",
  "username": "alice",
  "email": "alice@example.com",
  "tenant_id": "acme",
  "organizations": ["acme"],
  "organization": {},
  "groups": ["ai-engineers"],
  "roles": ["ai-admin"]
}
```

### Endpoints

#### `GET /api/users` → `UserView[]` (admin)

#### `POST /api/users` → **201** `UserView` (admin)

**Request `CreateUserRequest`:**

| Field | Type | Required | UI form |
| --- | --- | --- | --- |
| `username` | string | yes | Username |
| `email` | string | no | Email (omit if empty in UI) |
| `firstName` | string | no | First name |
| `lastName` | string | no | Last name |
| `password` | string | yes | Password |
| `temporaryPassword` | boolean | no | Typed in client; **not sent by UI** |
| `enabled` | boolean | no | Enabled checkbox |
| `tenant_id` | string | no | Tenant |
| `groups` | string[] | no | Group checkboxes |

Example:

```json
{
  "username": "bob",
  "email": "bob@acme.com",
  "firstName": "Bob",
  "lastName": "Builder",
  "password": "password123",
  "temporaryPassword": false,
  "enabled": true,
  "tenant_id": "acme-corp",
  "groups": ["ai-engineers", "acme-corp:model-granite-users"]
}
```

#### `GET /api/users/{username}` → `UserView` (self or admin)

#### `PUT /api/users/{username}` → `UserView` (admin) — **not used by UI**

Body: optional `email`, `firstName`, `lastName`, `enabled`, `tenant_id`.

#### `DELETE /api/users/{username}` → **204** (admin)

#### `PUT /api/users/{username}/groups` → `UserView` (admin)

```json
{ "groups": ["ai-engineers", "acme-corp:model-llama-users"] }
```

**UI:** group checkboxes on details page → `groups`.

#### `PUT /api/users/{username}/claims` → `UserView` (admin)

```json
{ "tenant_id": "acme-corp" }
```

Only `tenant_id` allowed; other keys → 400.  
**UI:** Tenant text field → `tenant_id`. Save order: claims, then groups.

#### Also on sibling API (not used by PoC Users UI)

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/users/me/groups` | `{ "groups": [...] }` |
| `PUT` | `/api/users/{username}/password` | `{ "password", "temporary" }` |
| `GET/POST/DELETE` | `/api/tenants`, `/api/tenants/{name}` | Keycloak Orgs; used by PoC `keycloakSync` |
| `PUT` | `/api/tenants/{name}/members` | `{ "usernames": [...] }` |
| `GET` | `/healthz` | Liveness |

Allowlisted groups: `grid-admins`, `ai-engineers`, `{tenant}:model-*-users`.

### Errors (Users)

```json
{ "error": "Sign in with Keycloak to manage users." }
```

HTTP: 401 unauthorized, 403 forbidden, 404 not found, 409 conflict, 400 validation.

### Translation notes

provider-backend already talks to Keycloak for Tenants. Absorbing Users means aligning with OSAC object shape (`id`/`metadata`/`spec`/`status`) and flat REST under `/api/ai-grid/v1/`, while preserving claim/group semantics the console needs.

---

## 8. Domain: Model management

### UI pages

| Route | Component | APIs |
| --- | --- | --- |
| `/models` | `ModelsPage` | `GET /api/models`, `DELETE /api/models/{name}` |
| `/models/deploy` | `ModelWizard` | `GET /api/locations`, `POST /api/models` |

### Endpoints

#### `GET /api/models` → `ManagedModel[]`

#### `GET /api/models/{name}` → `ManagedModel` + `ETag`  
*(UI list/delete only; get exists on backend)*

#### `POST /api/models` → **201** `ManagedModel` + `ETag` + `Location`

#### `DELETE /api/models/{name}` + `If-Match` → **204**  
**409** if referenced by any subscription/policy `modelRefs`.

### Request: `CreateModel` (strict)

| Field | Type | Required | UI form field |
| --- | --- | --- | --- |
| `name` | resource name ≤63 | yes | K8s name |
| `displayName` | string 1–253 | yes | Display name |
| `description` | ≤4000 | no | Description |
| `modelUri` | `hf\|oci\|s3\|pvc\|http(s)://...` | yes | Model URI |
| `modelName` | 1–253 | yes | Model name |
| `runtimeImage` | non-whitespace | yes | Runtime image |
| `replicas` | int 1–99 | yes | Replica count |
| `cpu` | cores/millicores | yes | CPU (e.g. `"2"`, `"500m"`) |
| `memory` | `Mi\|Gi\|Ti` | yes | Memory (e.g. `"4Gi"`) |
| `gpuCount` | int 0–64 | yes | GPU count (0 omits GPU) |
| `runtimeArgs` | string[] ≤64 | yes | Runtime args (`#` comments / blanks stripped) |
| `locations` | unique nonempty ≤64 | yes | Multi-select from `GET /locations` |

Example:

```json
{
  "name": "new-model",
  "displayName": "New model",
  "description": "Test deployment",
  "modelUri": "hf://org/model",
  "modelName": "org/model",
  "runtimeImage": "registry.example/vllm:test",
  "replicas": 2,
  "cpu": "500m",
  "memory": "4Gi",
  "gpuCount": 1,
  "locations": ["site1", "site2"],
  "runtimeArgs": ["--max-model-len=2048"]
}
```

### Response: `ManagedModel` (key fields)

| Field | Type | Notes |
| --- | --- | --- |
| `name`, `displayName`, `description` | string | |
| `phase` / `status` | string | PoC: `"Saved"` / `"Registered"` |
| `statusMessage`, `reason` | string | Git-only messaging |
| `creationTimestamp` | string | |
| `modelRef` | `{ kind, name }` | e.g. `LLMInferenceService` |
| `revision` | string | For `If-Match` |
| `locations` | string[] | |
| `modelUri`, `modelName`, `runtimeImage`, `replicas` | | Present when deployed |
| `hasDeployment` | boolean | false for catalog-only seeds |
| `availableForSubscriptions` | boolean | Filters subscription model picker |

Example create response:

```json
{
  "name": "new-model",
  "displayName": "New model",
  "description": "Test deployment",
  "phase": "Saved",
  "status": "Saved",
  "statusMessage": "Saved to Git. This PoC does not apply resources or run cluster reconciliation.",
  "reason": "GitOnly",
  "creationTimestamp": "2026-01-01T00:00:00.000Z",
  "modelRef": { "kind": "LLMInferenceService", "name": "new-model" },
  "revision": "sha256...",
  "locations": ["site1", "site2"],
  "modelUri": "hf://org/model",
  "modelName": "org/model",
  "runtimeImage": "registry.example/vllm:test",
  "replicas": 2,
  "hasDeployment": true,
  "availableForSubscriptions": true
}
```

### Source

- Zod: `shared/model-validation.mjs`
- Backend: `backend/models.mjs`
- UI: `ModelWizard.tsx`, types `src/app/types/models.ts`

### Translation notes

PoC create writes Git CRs (`LLMInferenceService`, `MaaSModelRef`, Placement, ManifestWorkReplicaSet) without applying to cluster. Production should define real lifecycle status vs PoC `"Saved"` / `GitOnly`. Legacy type file `src/app/types/maas-model.ts` is **not** the deploy path — use `CreateModelRequest`.

---

## 9. Domain: Locations

### UI usage

Loaded by **Deploy model** wizard (`ModelWizard`) for the locations multi-select. Not a standalone nav page.

### Endpoint

#### `GET /api/locations` → `string[]`

Eligible = Ready + `ai-grid/enabled=true` + `ai-grid/provisioned=true` + valid location label.

Example: `["site1", "site2", "site3"]`

### Translation notes

May be a derived view over ManagedClusters rather than a separate resource. Keep contract as a simple string array for the console wizard.

---

## 10. Domain: Subscription management

### UI pages

| Route | Component | APIs |
| --- | --- | --- |
| `/subscriptions` | `SubscriptionsTab` + table | Context `GET /api/subscriptions` (+ models for create), delete modal |
| `/subscriptions/create` | `CreateSubscriptionPage` / `CreateSubscriptionForm` | `POST /api/subscriptions` |
| `/subscriptions/edit/:subscriptionName` | `EditSubscriptionPage` | `GET` info + `PUT` |
| `/subscriptions/view/:subscriptionName` | `ViewSubscriptionPage` | `GET` info; delete from actions |

Global context (`MaaSGovernanceProvider`) polls `list()` + `listModels()` (~30s).

### Endpoints

#### `GET /api/subscriptions` → `Subscription[]`

#### `GET /api/subscriptions/{name}` → `SubscriptionInfo` + `ETag`

#### `POST /api/subscriptions` → **201** `MutationResult` + `ETag` + `Location`

#### `PUT /api/subscriptions/{name}` + `If-Match` → **200** `{ "subscription": Subscription }` + `ETag`  
*(Does not mutate separate auth policies)*

#### `DELETE /api/subscriptions/{name}` + `If-Match` → **204**  
*(Also deletes generated matching `-policy`)*

### Request: `CreateSubscription`

| Field | Type | Required | UI mapping |
| --- | --- | --- | --- |
| `name` | resource name ≤253 (≤246 if `createAuthPolicy`) | yes | K8s name |
| `createAuthPolicy` | boolean | yes | **Hardcoded `true`** (no UI control) |
| `priority` | int −1e6…1e6 | yes | Priority field |
| `owner.groups` | `{ name }[]` min 1 | yes | Creatable multi-select groups |
| `owner.users` | string[] | no | **Not edited in UI** |
| `modelRefs` | unique models, min 1 | yes | Models + rate-limit modal |
| `modelRefs[].name` | DNS name ≤63 | yes | Selected model |
| `modelRefs[].tokenRateLimits` | `{ limit, window }[]` | yes* | From UI `count`/`time`/`unit` via `toTokenRateLimit` |
| `modelRefs[].unlimited` | boolean | no | Preserved; **no editor** |
| `modelRefs[].billingRate.perToken` | string | no | Preserved; **no editor** |
| `modelRefs[].guardrails` | Guardrail[] | no | Preserved; **no editor** |
| `tokenMetadata.organizationId` | tenant name | yes | **Derived** from group prefix `{org}:model-…` |
| `tokenMetadata.costCenter` | string | no | Not in UI |
| `tokenMetadata.labels` | map | no | Not in UI |
| `displayName` / `description` | string | no | Form fields |
| `inferencePriority` | int32 | no | Preserved on edit; not in create form |
| `guardrails` | ≤64 | no | Preserved on edit |

\* Empty `tokenRateLimits` only when `unlimited === true`.

**Group rule (enforced):** each group name must match  
`{organizationId}:model-{something}-users`.

**Rate window:** `^[1-9]\d{0,3}[smh]$` (e.g. `"1h"`, `"30m"`).  
**UI RateLimit form** `{ count, time, unit }` → API `{ limit, window }`.

Example create:

```json
{
  "name": "engineering",
  "displayName": "Engineering",
  "description": "Original description",
  "priority": 10,
  "owner": {
    "groups": [{ "name": "osac:model-granite-users" }],
    "users": ["alice"]
  },
  "modelRefs": [
    {
      "name": "granite-3-8b",
      "displayName": "Granite",
      "tokenRateLimits": [{ "limit": 1000, "window": "1h" }],
      "billingRate": { "perToken": "0.000002" },
      "guardrails": [{ "ref": { "name": "model-safety" } }]
    }
  ],
  "tokenMetadata": {
    "organizationId": "osac",
    "labels": { "department": "engineering" }
  },
  "inferencePriority": -5,
  "guardrails": [{ "ref": { "name": "safety" }, "checks": ["pii"] }],
  "createAuthPolicy": true
}
```

### Request: `UpdateSubscription`

Same editable fields as create **without** `name` / `createAuthPolicy`. Path name is authoritative.  
**UI edit mapping:** spreads prior `owner`, `modelRefs` entries, `tokenMetadata`, `inferencePriority`, `guardrails` so unedited optional fields are preserved.

### Response: `MutationResult` (create)

```json
{
  "subscription": { /* Subscription */ },
  "authPolicy": { /* present when createAuthPolicy true */ }
}
```

### Response: `Subscription` (list item / embedded)

Includes create fields plus read-only: `phase`, `status`, `statusMessage`, `reason`, timestamps, `revision`, optional `deletionTimestamp`.

### Response: `SubscriptionInfo` (get/view)

```json
{
  "subscription": { /* Subscription */ },
  "modelRefs": [ /* ModelSummary[] joined from catalog */ ],
  "authPolicies": [ /* policies whose modelRefs intersect */ ],
  "meteringMetadata": { /* optional; from tokenMetadata */ }
}
```

### Response: `AuthorizationPolicy` (embedded)

| Field | Type |
| --- | --- |
| `name` | string |
| `displayName`, `description`, status fields | optional strings |
| `modelRefs` | `{ kind?, name }[]` |
| `subjects` | `{ groups?: {name}[], users?: string[] }` |
| `meteringMetadata` | TokenMetadata? |
| timestamps | optional |

There is **no** standalone list/create API for authorization policies in the PoC.

### Errors

409 duplicate / colliding policy / dirty Git; 422 validation (incl. tenant-scoped groups); 428/412; 404; 503.

**Doc discrepancy:** `docs/subscription-api-contract.md` mentions stale revision “409 or 412”; **code always returns 412**.

### Source

- Zod: `shared/subscription-validation.mjs` (`.passthrough()` — unknown fields accepted)
- Backend: `backend/api.mjs`, `backend/resources.mjs`
- UI: `CreateSubscriptionForm.tsx`, `src/app/types/subscriptions.ts`, `src/app/utilities/rateLimits.ts`

---

## 11. Translation notes for ai-grid-provider-backend

Follow [`API.md`](./API.md) / [`AGENTS.md`](../AGENTS.md) when implementing:

| PoC convention | Production target |
| --- | --- |
| Flat `/api/...` | Flat `/api/v1/...` |
| camelCase JSON | Proto `snake_case` JSON (map at gateway/client) |
| Ad-hoc DTOs | OSAC shape: `id` + `metadata` + `spec` + `status` |
| `revision` + `If-Match` | Align with resource version / etag strategy in API.md |
| No auth on portal API | Real authz (JWT / mesh) like Users API / OSAC |
| Git-only `"Saved"` / `GitOnly` | Real reconciliation statuses |
| Separate Users Go service | Fold into this service or keep clear boundary |
| Sibling Keycloak org sync on tenant CRUD | Explicit orchestration in server layer |
| OpenAPI hand-written in PoC | Generated from proto (`make generate`) |

**Suggested domain → RPC mapping (conceptual):**

| PoC domain | Likely service |
| --- | --- |
| Clusters (+ enroll labels) | Extend `ManagedClusters` |
| Tenants (+ IdP?) | Extend `Tenants` |
| Users / claims / groups | New `Users` (or absorb Keycloak API) |
| Models + locations | New `Models` (+ derived Locations) |
| Subscriptions (+ embedded policies) | New `Subscriptions` |
| Health | Existing `Ping` / healthz |

Preserve **behavioral** contracts the console needs even if wire shape changes: enroll as label patch, IdP secret-by-reference, tenant-scoped group naming, create-always-auth-policy, rate-limit window format, model location eligibility rules.

---

## 12. Appendix: shared types & removed routes

### Shared Zod modules (`ai-grid-cloud-provider-poc/shared/`)

| File | Role |
| --- | --- |
| `cluster-validation.mjs` | `GridCluster`, enroll body |
| `tenant-validation.mjs` | Tenant + OIDC IdP |
| `model-validation.mjs` | Create model + locations |
| `subscription-validation.mjs` | Create/update subscription, rates, guardrails |
| `public-data.mjs` | Strip `namespace` |
| `grid-clusters.mjs` | Seed/helpers |

### Removed / 404 in PoC (do not reintroduce as separate APIs without product decision)

- `/api/groups`
- `/api/authorization-policies` (list)
- `/api/deployments` (folded into `/api/models`)

### Dead / unused UI

- `MaaSGovernanceYamlTab.tsx` — not routed
- `UserService.me` / `UserService.update` — implemented, unused by pages
- `src/app/types/maas-model.ts` — legacy; not wired to deploy wizard

---

## 13. Gaps & discrepancies

| Gap | Detail |
| --- | --- |
| Users API not in PoC repo | Documented from `RedHat/keycloak/api` + PoC client types; confirm path if sibling checkout differs |
| OpenAPI vs code: Keycloak sync **502** | Not listed in PoC OpenAPI responses |
| OpenAPI vs code: Error codes | Runtime uses `unauthorized`/`forbidden`; OpenAPI enum incomplete |
| OpenAPI vs code: model Zod message | Shared `validate()` says *"The subscription configuration is invalid."* for model failures |
| Contract doc vs code: stale `If-Match` | Always **412**, not 409 |
| UI vs schema: subscription optionals | `unlimited`, `billingRate`, `guardrails`, `inferencePriority`, `owner.users`, `costCenter`/`labels` preserved but not edited |
| `organizationId` derivation | Inferred from group name prefix; empty/mismatch can fail backend refine |
| Portal API auth | Deferred in PoC; must be designed for production |
| provider-backend Tenant vs PoC Tenant | Domains-only org vs full IdP Git tenant — product decision required |
| provider-backend ManagedCluster vs PoC GridCluster | Production uses OSAC ManagedCluster at `/api/v1/clusters`; PoC flat `GridCluster` list/enroll is not exposed |

---

## Quick endpoint index (PoC)

| Method | Path | Domain | Primary UI |
| --- | --- | --- | --- |
| GET | `/api/health` | Health | — |
| GET | `/api/clusters` | Clusters | `/clusters` |
| PATCH | `/api/clusters/{name}` | Clusters | `/clusters` |
| GET/POST | `/api/tenants` | Tenants | `/tenants*` |
| GET/PUT/DELETE | `/api/tenants/{name}` | Tenants | `/tenants*` |
| GET | `/api/locations` | Locations | `/models/deploy` |
| GET/POST | `/api/models` | Models | `/models*` |
| GET/DELETE | `/api/models/{name}` | Models | `/models` |
| GET/POST | `/api/subscriptions` | Subscriptions | `/subscriptions*` |
| GET/PUT/DELETE | `/api/subscriptions/{name}` | Subscriptions | `/subscriptions*` |
| POST | `/auth/token` | Auth | `/login` |
| GET/POST | `/api/users` | Users | `/users*` |
| GET/PUT/DELETE | `/api/users/{username}` | Users | `/users*` |
| PUT | `/api/users/{username}/groups` | Users | `/users/:username` |
| PUT | `/api/users/{username}/claims` | Users | `/users/:username` |
| GET | `/api/users/me` | Users | (client only) |
