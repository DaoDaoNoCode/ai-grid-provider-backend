# Spike: Keycloak Organizations as AI Grid tenants

Read-only research, 2026-10-15. Purpose: specify a fake gRPC backend and frontend mocks that
behave like the real `ai-grid-backend` + Keycloak pair, so the real backend can be swapped in
later with minimal change. All example names, hosts, ids and UUIDs below are fictional; product defaults
(the Keycloak image, the `master` realm, `admin-cli`, `realm-management` roles) are quoted as they are, and
the hub's own namespace and route are replaced by placeholders.

Sources used throughout:

- Keycloak Admin REST API (OpenAPI for "latest"; the document's own `info.version` is `1.0`, so "26.8.x" is not verified from the document, but its content matches 26.8: `identityProvider` list filter, `/invitations`, `membership-type`): https://www.keycloak.org/docs-api/latest/rest-api/index.html (the `openapi.json` next to it was parsed for the Organizations tag).
- Server Administration Guide, Managing organizations: https://www.keycloak.org/docs/latest/server_admin/index.html#_managing_organizations (asciidoc sources at tag `26.8.0`: `docs/documentation/server_admin/topics/organizations/*.adoc`).
- Enabled features list: https://www.keycloak.org/server/features
- Keycloak source at tag `26.8.0` (the deployed version), in particular
  `services/src/main/java/org/keycloak/organization/admin/resource/{OrganizationsResource,OrganizationResource,OrganizationMemberResource,OrganizationIdentityProvidersResource}.java`,
  `model/jpa/src/main/java/org/keycloak/organization/jpa/{JpaOrganizationProvider,OrganizationAdapter}.java`,
  `services/src/main/java/org/keycloak/organization/utils/Organizations.java`,
  `services/src/main/java/org/keycloak/services/resources/admin/fgap/OrganizationPermissions.java`,
  `server-spi-private/src/main/java/org/keycloak/models/AdminRoles.java`,
  `services/src/main/java/org/keycloak/services/{ErrorResponse,error/KeycloakErrorHandler}.java`,
  `services/src/main/java/org/keycloak/authentication/authenticators/directgrant/{ValidateUsername,ValidatePassword}.java`,
  `services/src/main/java/org/keycloak/protocol/oidc/grants/ClientCredentialsGrantType.java`,
  `services/src/main/java/org/keycloak/services/managers/ApplianceBootstrap.java`, `server-spi-private/src/main/java/org/keycloak/models/Constants.java`.
- Backend code: `<console repo>/.cache/ai-grid-backend/internal/keycloak/client.go`,
  `internal/ai_grid/tenants.go`, `internal/ai_grid/service.go`, `cmd/ai-grid-backend/main.go`,
  `proto/ai_grid/v1/ai_grid.proto`, `deploy/kind/keycloak.yaml`, `README.md`.
- Console plan: `<console repo>/docs/plan/bff-api.md`, `ai-grid-provider-console/docs/plan/work-breakdown.md`;
  current shapes in `ai-grid-provider-console/bff/internal/api/tenants.go`, `ai-grid-provider-console/bff/internal/backend/backendtest/server.go`, `ai-grid-provider-console/frontend/src/api/{types.ts,MockConsoleService.ts}`.

---

## 1. Summary

1. The dev hub's Keycloak (in its own namespace, `<keycloak-namespace>` below) runs `quay.io/keycloak/keycloak:26.8.0` with `start --import-realm`; the `organization` feature (`organization:v1`) is GA since 26.0 and on by default in 26.x, nothing disables it, and the master realm's discovery document advertises the `organization` scope. Organizations are available.
2. The `ai-grid` realm does not exist yet (`GET /realms/ai-grid/.well-known/openid-configuration` returns 404 `{"error":"Realm does not exist"}`). A realm-import ConfigMap seeds three other realms, one of which already has `organizationsEnabled: true` with two sample organizations; see section 2.
3. Today's backend does one thing: password grant as the bootstrap admin in `master` via `admin-cli`, then `GET /admin/realms/ai-grid/organizations?first=0&max=100` with the default brief representation, and maps `{id,name,domains[].name}` to proto `Tenant{id,name,domains}`. Every failure becomes a plain Go error, i.e. gRPC code `Unknown`, which the BFF turns into a 502.
4. The Organizations Admin API is a conventional CRUD surface: list (`search`, `exact`, `q`, `first`, `max`, default `max=10`, ordered by name), `POST` 201 + `Location`, `PUT` 204 (replaces `name`, `enabled`, `description`, `redirectUrl` from the body; `attributes` and `domains` are replaced only when present; a missing `alias` becomes the body's `name` and a missing `name` is a 409, so a PUT must carry both; see 5.6), `DELETE` 204, plus `/members`, `/invitations`, `/identity-providers`, `/groups` and `/count` sub-resources.
5. Error bodies come in two shapes: resource-level errors are `{"errorMessage": "..."}` (400/404/409), while framework-level errors are `{"error": "HTTP 401 Unauthorized"}`, `{"error": "HTTP 403 Forbidden"}`, `{"error": "Realm not found."}`.
6. Uniqueness is enforced on `name` and `alias` (409, message literally "A organization with the same name already exists."), and on domains across the realm (400 "Domain acme.example.com is already linked to organization ..."). Alias defaults to the name and is immutable (400 "Cannot change the alias").
7. Authorization is by `realm-management` client roles: `query-organizations` alone yields an empty `200 []` on list, `view-organizations` is needed to see anything, `manage-organizations` for create/update/delete, and members additionally need `query-users`/`view-users`/`manage-users`.
8. The deployed console's `ListTenants` would return disabled organizations too, capped at 100, without alias/enabled/description/attributes/members: all gaps versus what `bff-api.md` Step 6 wants (create, update, delete; members later).
9. Proposed: a `Tenants` service with paged/searchable list, get, create, update, delete, list/add/remove members, and a fixed mapping from Keycloak status codes to gRPC codes (409 AlreadyExists, 404 NotFound, 400 InvalidArgument, 401/403 Internal, network Unavailable), which the BFF's existing error table already maps to HTTP.
10. Bootstrap for production: create the `ai-grid` realm with `organizationsEnabled: true`, a confidential client `ai-grid-backend` with service accounts, assign the minimal `realm-management` roles to its service-account user, and switch the backend to `client_credentials` in the `ai-grid` realm.

---

## 2. Facts about the hub's Keycloak (anonymised)

Observed with `oc get` only (no secret values read, no login attempted):

| Item | Observation |
|---|---|
| Workload | `Deployment/keycloak`, image `quay.io/keycloak/keycloak:26.8.0`; `StatefulSet/keycloak-postgresql` on `quay.io/sclorg/postgresql-16-c9s:latest`. |
| Start mode | `args: ["start", "--import-realm"]` (production mode, not `start-dev`). Env: `KC_DB=postgres`, `KC_DB_URL_HOST=keycloak-postgresql`, `KC_PROXY_HEADERS=xforwarded`, `KC_HTTP_ENABLED=true`, `KC_HEALTH_ENABLED=true`. No `KC_FEATURES` / `--features` flag, so the default feature set applies. |
| Exposure | One OpenShift `Route` with edge TLS and HTTP-to-HTTPS redirect in front of `Service/keycloak:8080`. Call it `https://sso.example.com` below. |
| Version check | `GET /realms/master/.well-known/openid-configuration` returned 200; `grant_types_supported` includes `password` and `client_credentials`; `scopes_supported` includes `organization` (only present when the organization feature is enabled). |
| Organizations feature | https://www.keycloak.org/server/features lists `organization:v1` ("Organization support within realms") as enabled by default (`Profile.java:160`, `Type.DEFAULT`); GA since Keycloak 26.0 (release notes at tag `26.8.0`, `docs/documentation/release_notes/topics/26_0_0.adoc` L1-3: "Organizations supported ... Starting with {project_name} 26, the Organizations feature is fully supported."). Nothing in the deployment disables it. Per realm it is still an opt-in switch (`organizationsEnabled` in `RealmRepresentation`, "Realm Settings > Organizations" in the admin console). |
| Realms | `master` exists. `ai-grid` does not: `GET /realms/ai-grid/.well-known/openid-configuration` returned 404 `{"error":"Realm does not exist"}`. |
| Realm import | ConfigMap `keycloak-realm-import-<hash>` mounted for `--import-realm` holds three realm JSON files: one SaaS-style realm with `organizationsEnabled: true` containing 2 organizations (keys `name, alias, domains, enabled, identityProviders`; each with 1 domain, 1 linked IdP, alias different from name, 0 members, `enabled: true`) and two small per-tenant IdP realms (1 client, 1 user each, no organizations). Only counts and key names were read. Whether these realms were actually imported was not enumerated (out of scope). |
| Unauthenticated admin call | `GET /admin/realms/master/organizations` without a token returned `401 {"error":"HTTP 401 Unauthorized"}` (observed once by the original author; not re-verified, admin calls were excluded from the verification pass). |
| Backend's own dev setup | `deploy/kind/keycloak.yaml` runs `quay.io/keycloak/keycloak:26.3` in `start-dev` with bootstrap admin `admin/admin`, a Job that creates realm `ai-grid` with `organizationsEnabled: true` and two organizations `tenant-a`, `tenant-b` (`alias` = name, one unverified domain each). So the backend was only ever tested against 26.3 dev mode; the hub runs 26.8.0 production mode. **The two versions differ materially for this API** (diff of the 26.3.0 and 26.8.0 sources of `OrganizationsResource`, `OrganizationResource`, `OrganizationAdapter`, `ValidateUsername/ValidatePassword`): (a) 26.3 guards list/get/create/update/delete with `auth.realm().requireManageRealm()`, so **only `manage-realm` works**; the `query/view/manage-organizations` role checks of section 6 and the README's `query-organizations` advice do not exist in 26.3. (b) 26.3 `setDomains` throws `"You must provide at least one domain"` on null/empty; 26.8 skips null and accepts `[]`. (c) 26.3 domain errors read `"The specified domain is invalid: X"` and `"Domain X is already linked to another organization in realm Z"`. (d) 26.3 has no `identityProvider` list filter, no `identityProviderAlias`/`autoRedirect` on domains and no `/invitations`. (e) 26.3 returns **401** for `invalid_grant` on the token endpoint, 26.8 returns 400. The mock targets 26.8.0 only; the backend's Kind pin should move to 26.8. |

---

## 3. Endpoint table: Organizations Admin REST API (Keycloak 26.8)

All paths are under `{base}/admin/realms/{realm}`; auth is `Authorization: Bearer <access token>`. Only the endpoints the console needs are detailed; the rest are listed for completeness. Sources: OpenAPI `Organizations` tag; `OrganizationsResource.java`, `OrganizationResource.java`, `OrganizationMemberResource.java`, `OrganizationIdentityProvidersResource.java` (26.8.0).

### 3.1 Organizations

| Method | Path | Request | Success | Errors |
|---|---|---|---|---|
| GET | `/organizations` | Query: `search` (name or domain), `exact` (bool), `q` (`key1:value1 key2:value2` attribute query), `identityProvider` (alias; cannot be combined with `q`), `first` (default 0), `max` (default **10**), `briefRepresentation` (default `true`; `false` adds `attributes`) | `200` JSON array of `OrganizationRepresentation`, ordered by `name` ascending | `400 {"errorMessage":"The 'identityProvider' and 'q' parameters cannot be combined."}`, `400 {"errorMessage":"Identity provider 'x' not found."}`; `401 {"error":"HTTP 401 Unauthorized"}`; `403 {"error":"HTTP 403 Forbidden"}` when the caller lacks `query-organizations`/`view-organizations`/`manage-organizations`/`manage-realm`; **`200 []`** when the caller has only `query-organizations`; `404 {"errorMessage":"Organizations not enabled for this realm."}` when the realm has `organizationsEnabled=false` (403 instead if the caller cannot even query); `404 {"error":"Realm not found."}` when the realm does not exist |
| GET | `/organizations/count` | Same filters minus paging | `200` integer (JSON number) | as list; `0` for query-only callers |
| POST | `/organizations` | JSON `OrganizationRepresentation` (`name` required; `alias` optional; `domains`, `description`, `redirectUrl`, `attributes`, `enabled` optional) | **`201`**, empty body, `Location: {base}/admin/realms/{realm}/organizations/{new-id}` | `400 {"errorMessage":"Organization cannot be null."}`; `400 {"errorMessage":"Name can not be null"}`; `400 {"errorMessage":"Name cannot be used as alias: Empty Space not allowed."}` / `"Name cannot be used as alias: Character '/' not allowed."` (no alias given and the name contains whitespace **or an RFC 3986 reserved char** `:/?#@!$&()*+,;=[]\`; `JpaOrganizationProvider.create` L107-114 via `ReservedCharValidator.validateNoSpace`); **`400 {"error":"Empty Space not allowed."}`** / `{"error":"Character '/' not allowed."}` when an *explicit* `alias` contains whitespace or a reserved char (`OrganizationsResource.create` L112 calls the validator before anything else; it throws a `BadRequestException`, so the body has the framework `error` shape, not `errorMessage`); `400 {"errorMessage":"Organization redirect URL is not valid."}` (`server-spi-private/.../organization/validation/OrganizationsValidation.java` L22-25, any value the built-in URI validator rejects); `400 {"errorMessage":"Domain name cannot be empty"}`; `400 {"errorMessage":"Invalid domain format: not a domain"}`; `400 {"errorMessage":"Domain acme.example.com is already linked to organization acme-robotics in realm ai-grid"}`; **`409 {"errorMessage":"A organization with the same name already exists."}`**; **`409 {"errorMessage":"A organization with the same alias already exists"}`** (sic, no period); `403` without `manage-organizations`; `404 ... not enabled` |
| GET | `/organizations/{org-id}` | | `200` full `OrganizationRepresentation` (always `briefRepresentation=false`, so `attributes` included) | `400 {"errorMessage":"Id cannot be null."}`; **`404 {"errorMessage":"Organization not found."}`** (403 instead if the caller cannot query) |
| PUT | `/organizations/{org-id}` | JSON `OrganizationRepresentation` (full representation; see 5.6 for replace semantics). Send `name` and the stored `alias` every time | **`204`**, empty body | `409 {"errorMessage":"A organization with the same name already exists."}` when `name` changes to a value that matches another organization's **name or domain** (the check is `getAllStream(name, exact=true)`, which searches both; `OrganizationResource.update` L120-125), and also when the body has **no `name`** while the realm has any organization (a blank search matches every organization, `JpaOrganizationProvider.buildStringSearchPredicate` L462-470); `400 {"errorMessage":"Cannot change the alias"}`, also when `alias` is **omitted** and the body's `name` differs from the stored alias (see 5.4); `400 {"errorMessage":"Identity provider with alias '<a>' does not exist in realm <realm>"}` / `"Identity provider '<a>' is not associated with organization <name>"` for a domain whose `identityProviderAlias` is unknown or not linked to this organization (`OrganizationAdapter.resolveIdentityProvider` L314-329, called for every domain in `setDomains` L203, L224; the same on create); `400` redirect URL / domain validation messages as for create; `404 {"errorMessage":"Organization not found."}`; `403` |
| DELETE | `/organizations/{org-id}` | | **`204`** | `404 {"errorMessage":"Organization not found."}`; `403`; `400 {"errorMessage":"organization couldn't be deleted"}` (provider returned false; not reachable with the JPA provider) |
| GET | `/organizations/members/{member-id}/organizations` | `briefRepresentation` | `200` array of orgs the user belongs to | `400 {"errorMessage":"id cannot be null"}`; `404 {"error":"HTTP 404 Not Found"}` when the user does not exist |

### 3.2 Members (`/organizations/{org-id}/members`)

| Method | Path | Request | Success | Errors |
|---|---|---|---|---|
| GET | `/members` | `search` (username, email, first or last name), `exact`, `first` (0), `max` (**10**), `membershipType` (`MANAGED`/`UNMANAGED`), `briefRepresentation` (true) | `200` array of `MemberRepresentation`, ordered by `username` | `403` without `query-users`; **`200 []`** with `query-users` but no `view-users`; an unknown `membershipType` value hits `MembershipType.valueOf` uncaught (`500`) |
| GET | `/members/count` | | `200` integer | `0` for query-only callers |
| POST | `/members` | Body is the **user id as a JSON string** (`"7f0c2a9e-..."`; quotes optional, whitespace trimmed), `Content-Type: application/json` | **`201`**, `Location: .../members/{user-id}` | `404 {"error":"HTTP 404 Not Found"}` user does not exist (403 if the caller cannot query users); **`409 {"errorMessage":"User is already a member of the organization."}`**; `400 {"errorMessage":"User is already a managed member of another organization"}`; `403` without `manage-organizations` **and** `manage-users` |
| GET | `/members/{member-id}` | | `200` full `MemberRepresentation` (includes `membershipType`) | `400 {"errorMessage":"id cannot be null"}`; `404 {"error":"HTTP 404 Not Found"}` when not a member |
| DELETE | `/members/{member-id}` | | **`204`** | `404 {"error":"HTTP 404 Not Found"}` when user is not a member; `400 {"errorMessage":"Not a member of the organization"}`; `403` |
| PUT | `/members/{member-id}/membership-type` | JSON string `"MANAGED"` or `"UNMANAGED"` | `204` | `400 {"errorMessage":"membershipType cannot be null"}`; `404 {"errorMessage":"Not a member of the organization"}` |
| POST | `/members/invite-user` | `application/x-www-form-urlencoded`: `email` (required), `firstName`, `lastName`; query `client_id` optional | `204` (an email is sent) | `400 {"errorMessage":"Organization is disabled"}` / `"Email is required to invite a member"` / `"Invalid email format"`; `409 {"errorMessage":"User already has a pending invitation"}` / **`"User already a member of the organization"`** (note: different wording from the `POST /members` 409); `500 {"errorMessage":"Failed to send invite email"}` (`OrganizationInvitationResource.java` L110-207) |
| POST | `/members/invite-existing-user` | form `id` | `204` | `400`, `500` |
| GET | `/members/{member-id}/groups`, `/members/{member-id}/organizations` | groups: `first`, `max`, `search`, `briefRepresentation`; organizations: `briefRepresentation` only, no paging (`OrganizationMemberResource.java` L328-357, L365-391) | `200` arrays | `400`, `404` |

### 3.3 Identity providers (`/organizations/{org-id}/identity-providers`)

| Method | Path | Request | Success | Errors |
|---|---|---|---|---|
| GET | `/identity-providers` | | `200` array of `IdentityProviderRepresentation` | `403` |
| POST | `/identity-providers` | Body: IdP **alias as a JSON string** | `204` | `400 {"errorMessage":"Identity provider not found with the given alias"}`; `409 {"errorMessage":"Identity provider already associated to the organization"}` |
| GET | `/identity-providers/{alias}` | | `200` | `404 {"errorMessage":"Identity provider not associated with the organization"}` |
| PUT | `/identity-providers/{alias}` | JSON `OrganizationIdentityProviderLinkRepresentation {autoMembership, membershipType}` | `204` | `400 {"errorMessage":"Invalid membership type: X"}`, `400 {"errorMessage":"Auto-membership must be enabled when membership type is MANAGED"}`, `404` |
| DELETE | `/identity-providers/{alias}` | | `204` | `404 {"errorMessage":"Identity provider not found with the given alias"}`; `400 {"errorMessage":"Identity provider not associated with the organization"}` |

Also present but not needed by the console: `/organizations/{org-id}/groups/**` (organization-scoped groups) and `/organizations/{org-id}/invitations/**` (list, get, resend, delete invitations).

### 3.4 Realm-level

| Method | Path | Request | Success | Errors |
|---|---|---|---|---|
| POST | `/admin/realms` | `RealmRepresentation {"realm":"ai-grid","enabled":true,"organizationsEnabled":true}` | `201`, `Location` | **`409 {"errorMessage":"Realm ai-grid already exists"}`** when the realm exists (`RealmsAdminResource.java` L195-197 throws `ErrorResponse.exists(mde.getMessage())`; the message comes from `RealmManager.java` L605 `"Realm " + name + " already exists"`; "Conflict detected" is only the server log line); `403` |
| PUT | `/admin/realms/{realm}` | `RealmRepresentation` (partial is fine for this endpoint) | `204` | `404 {"error":"Realm not found."}` |

### 3.5 Token endpoint (`{base}/realms/{realm}/protocol/openid-connect/token`)

| Grant | Request (form) | Success | Errors |
|---|---|---|---|
| `password` (what the backend does today, realm `master`) | `grant_type=password&client_id=admin-cli&username=admin&password=...` | `200 {"access_token":"...","expires_in":60,"refresh_expires_in":1800,"refresh_token":"...","token_type":"Bearer","not-before-policy":0,"session_state":"...","scope":"profile email"}` | wrong password / unknown user: **`400 {"error":"invalid_grant","error_description":"Invalid user credentials"}`** (26.8.0 `ValidatePassword`/`ValidateUsername` use `Status.BAD_REQUEST`; 26.3.0 returned 401 for the same body, so a client should accept either; the mock emits 400); disabled user: `400 {"error":"invalid_grant","error_description":"Account disabled"}` **only when the submitted password is valid**; a disabled account with a wrong password gets `"Invalid user credentials"` (`ValidateUsername.java` L122-127); missing `username`: `401 {"error":"invalid_request","error_description":"Missing parameter: username"}` (`ValidateUsername.java` L64); user with pending required actions: `400 {"error":"invalid_grant","error_description":"Account is not fully set up"}`; missing `grant_type`: `400 {"error":"invalid_request","error_description":"Missing form parameter: grant_type"}`; unknown realm: `404 {"error":"Realm does not exist"}` |
| `client_credentials` (proposed, realm `ai-grid`) | `grant_type=client_credentials&client_id=ai-grid-backend&client_secret=...` (or HTTP Basic) | `200 {"access_token":"...","expires_in":300,"refresh_expires_in":0,"token_type":"Bearer","not-before-policy":0,"scope":"profile email"}` (no refresh token by default for service accounts) | unknown or disabled `client_id`: **`401 {"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`** (`AuthenticationProcessor.java` L924-927); **wrong or missing `client_secret`: `401 {"error":"unauthorized_client","error_description":"Invalid client or Invalid client credentials"}`** (`ClientIdAndSecretAuthenticator.java` L136, L249); service accounts not enabled on the client: `401 {"error":"unauthorized_client","error_description":"Client not enabled to retrieve service account"}`; public client: `401 {"error":"unauthorized_client","error_description":"Public client not allowed to retrieve service account"}`; service-account user disabled: `401 {"error":"invalid_request","error_description":"User 'service-account-ai-grid-backend' disabled"}` |

Token lifetimes: the master realm is bootstrapped with `accessTokenLifespan = 60` seconds (`ApplianceBootstrap.java`), SSO idle 1800 s, SSO max 36000 s. New realms default to `Constants.DEFAULT_ACCESS_TOKEN_LIFESPAN = 300` seconds. The token is a JWS; `exp` is authoritative, `expires_in` is the hint to cache against.

---

## 4. JSON shapes (fictional examples)

### 4.1 `OrganizationRepresentation` (full, `briefRepresentation=false`): org with two domains

```json
{
  "id": "3f1c6a52-7d0e-4b1a-9c3e-5a6d7e8f9a01",
  "name": "acme-robotics",
  "alias": "acme-robotics",
  "enabled": true,
  "description": "Acme Robotics, EU region",
  "redirectUrl": "https://console.acme.example.com/welcome",
  "attributes": {
    "ai-grid/region": ["europe-west"],
    "billing-account": ["ACME-0042"]
  },
  "domains": [
    { "name": "acme.example.com", "verified": true,  "identityProviderAlias": "acme-oidc", "autoRedirect": true },
    { "name": "*.acme.example.org", "verified": false, "autoRedirect": false }
  ]
}
```

Notes: `members`, `identityProviders` and `groups` exist in the schema but are **never populated by GET**; they are only read on import. Brief representation (the list default) omits `attributes` only. **Null fields are omitted, never serialised as `null`**: the admin REST ObjectMapper is `ObjectMapperResolver.createStreamSerializer()`, which sets `JsonInclude.Include.NON_NULL` (`services/src/main/java/org/keycloak/services/util/ObjectMapperResolver.java` L47-60, L76; registered on Quarkus by `QuarkusObjectMapperResolver`). So `id`, `name`, `alias`, `enabled` are always present; `description` and `redirectUrl` appear only when set; the second domain above has no `identityProviderAlias` key because it is unset. `domains` is **absent (not `[]`) when the organization has no domains**, because `OrganizationRepresentation.domains` is only created lazily by `addDomain` (`core/.../OrganizationRepresentation.java` L118-123). Clients must treat a missing `domains` as empty (the backend's `client.go` already does: nil slice -> empty `[]string`).

### 4.2 Disabled organization without domains or description (brief, as it appears in a list)

```json
{
  "id": "b9d2e4f6-1a3c-4e5f-8b7d-0c1e2f3a4b05",
  "name": "globex-labs",
  "alias": "globex",
  "enabled": false
}
```

`description`, `redirectUrl` and `domains` are omitted because they are unset/empty (see the `NON_NULL` note above). A list call returns disabled organizations like any other; there is no `enabled` filter on the API.

### 4.3 `MemberRepresentation` (brief, from `GET /members`)

```json
{
  "id": "7f0c2a9e-5b6d-4c8e-9f1a-2b3c4d5e6f07",
  "username": "jane.doe",
  "firstName": "Jane",
  "lastName": "Doe",
  "email": "jane.doe@acme.example.com",
  "emailVerified": true,
  "createdTimestamp": 1760000000000,
  "enabled": true,
  "membershipType": "UNMANAGED"
}
```

`MemberRepresentation` is `UserRepresentation` plus `membershipType` (`"MANAGED"` or `"UNMANAGED"`). The brief form (`OrganizationMemberResource.toRepresentation(member, brief=true)` -> `ModelToRepresentation.toBriefRepresentation`, L359-400) sets only `id`, `username`, `firstName`, `lastName`, `email`, `enabled`, `emailVerified`, `createdTimestamp`, `federationLink` (when set) and `membershipType`. `totp`, `disableableCredentialTypes`, `requiredActions`, `notBefore`, `attributes` appear **only with `briefRepresentation=false`** (`ModelToRepresentation.toRepresentation`, L313-340); in the brief form they are null and therefore omitted. `access` never appears in a `MemberRepresentation`: only the users admin resources set it (`UserResource.java` L368, `UsersResource.java` L582), not `OrganizationMemberResource.toRepresentation` (L440-442).

### 4.4 Error bodies

```json
{"errorMessage": "A organization with the same name already exists."}   // 409 from ErrorResponse.error
{"errorMessage": "Organization not found."}                              // 404
{"errorMessage": "Invalid domain format: not a domain"}                  // 400
{"error": "HTTP 401 Unauthorized"}                                       // 401, no/expired token (KeycloakErrorHandler)
{"error": "HTTP 403 Forbidden"}                                          // 403, missing role
{"error": "Realm not found."}                                            // 404, unknown realm on /admin/realms/{realm}/...
{"error": "Realm does not exist"}                                        // 404, unknown realm on /realms/{realm}/...
{"error": "Empty Space not allowed."}                                    // 400, explicit alias with whitespace (BadRequestException, not ErrorResponse)
{"error": "invalid_grant", "error_description": "Invalid user credentials"}            // token endpoint, password grant
{"error": "invalid_client", "error_description": "Invalid client or Invalid client credentials"}        // token endpoint, unknown/disabled client
{"error": "unauthorized_client", "error_description": "Invalid client or Invalid client credentials"}   // token endpoint, wrong/missing client_secret
```

`ErrorRepresentation` also has optional `field`, `params`, `errors[]` (used by user-profile validation, not by organizations).

---

## 5. Behavioural details a faithful mock must reproduce

Sources: `JpaOrganizationProvider.java`, `OrganizationAdapter.java`, `Organizations.java`, `RepresentationToModel.toModel(OrganizationRepresentation, OrganizationModel)`, `ModelToRepresentation.toRepresentation(OrganizationModel, boolean)`, admin guide "Creating an organization", "Managing organization domains", "Deleting an organization".

1. **Pagination**: `first` and `max` are offset/limit. Defaults `first=0`, `max=10` (not 100; the backend passes `max=100` explicitly). `max=-1` is treated as unlimited by `paginateQuery`. There is no total in the list response; use `GET /organizations/count` with the same filters. Results are ordered by `name` ascending (`orderBy(builder.asc(org.get("name")))`).
2. **Search**: `search` without `exact` is case-insensitive **contains** on `name` OR on any domain `name` (`LIKE %term%` on lower-cased name; domain names are stored lower-case). With `exact=true` it is case-sensitive equality on name or domain. `q=key1:v1 key2:v2` matches attributes (and the special key `alias`). Blank `search` returns everything.
3. **Uniqueness**: `name` unique per realm (named query `getByOrgName`, plain `=` comparison, so case-sensitivity is the database collation's: case-sensitive on the hub's PostgreSQL); `alias` unique per realm (`getByOrgAlias`, same rule); domains unique per realm (compared case-insensitively, stored lower-case). Both name and alias conflicts are 409 on create; on update only a name conflict is checked (409) because the alias cannot change.
4. **Alias**: defaults to `name` when omitted or blank. A name containing whitespace **or an RFC 3986 reserved character** (`ReservedCharValidator`: `:/?#@!$&()*+,;=[]\`) cannot be used as alias (400 `{"errorMessage":"Name cannot be used as alias: Empty Space not allowed."}` / `"... Character '/' not allowed."`), and an explicit alias with such characters is rejected with 400 `{"error":"Empty Space not allowed."}` / `{"error":"Character '/' not allowed."}` (framework `error` shape, see 3.1). The admin guide says the alias "must be URL-friendly" and "cannot be changed afterwards" (update with a different alias gives 400 "Cannot change the alias"). Sending the stored alias is fine; **omitting it is not safe**: `RepresentationToModel.toModel` calls `setName` before `setAlias` (`RepresentationToModel.java` L1944-1945) and `OrganizationAdapter.setAlias` replaces a blank alias with the (new) name (L100-110), so an update without `alias` is 400 "Cannot change the alias" unless the body's `name` equals the stored alias. For the 4.2 example (`name: globex-labs`, `alias: globex`) every PUT without `alias` fails.
5. **`enabled`**: defaults to `true` (`OrganizationRepresentation.enabled = true`, and `create` calls `setEnabled(true)`). Note the Java default means an update body that omits `enabled` re-enables the organization.
6. **Update replaces scalars, but `null` does not clear collections**: `RepresentationToModel.toModel` (L1939-1958) calls `setName`, `setAlias`, `setEnabled`, `setRedirectUrl`, `setDescription`, `setAttributes` unconditionally and `setDomains` only when `domains != null`. Effects: `name` absent -> 409 before anything is written (see 3.1); `alias` absent -> the new name, so 400 unless it equals the stored alias (5.4); `description`/`redirectUrl` absent or `null` -> cleared; `enabled` absent -> `true`; **`attributes` absent or `null` -> unchanged** (`OrganizationAdapter.setAttributes` returns early on null, L144-147), `attributes: {}` -> all removed; **`domains` absent or `null` -> unchanged** (`setDomains` returns early on null, L184-187), `domains: []` -> all removed (entities not in the new set are deleted, the rest claimed/created). Consequence: a brief list entry (no `attributes` key) PUT back unchanged keeps the attributes, while a full `GET /{id}` representation PUT back replaces them. Clients should still GET (full), modify, PUT the whole representation.
7. **Domain validation** (`Organizations.validateDomain`, 26.8.0 L315-343): the name is first **trimmed and lower-cased** by `OrganizationDomainModel`'s constructor (`RepresentationToModel.toModel` L1960-1962 builds the model before `setDomains` validates), so every message below prints the canonical form (`"Not A Domain"` -> `Invalid domain format: not a domain`). Then: non-blank; a name containing `*.` (`WILDCARD_PREFIX`, L78) is a wildcard and is checked in this order: the literal `*.` -> "Wildcard domain must specify a base domain: X"; not starting with `*.` -> "Wildcard domain must start with the wildcard" (so `acme.*.*.com` and `acme*.example.com` get this one); a further `*` in the base -> "Multiple wildcards are not allowed: X"; the base (input with `*.` stripped) must have at least 2 and at most 10 dot-separated parts -> "Domain must have at least 2 parts (e.g. 'example.com'): B" / "Domain has too many parts (max 10 allowed): B", **where B is the base, not the input** (`*.com` -> `...: com`). Everything else, the wildcard's base included, must be valid as the domain part of an e-mail address (`EmailValidationUtil.isValidEmail("user@" + domain)`, a copy of hibernate-validator's `EmailValidator`: labels of letters, digits, `!#$%&'*+/=?^`{|}~` (Keycloak's `EmailValidationUtil` copy of the hibernate pattern has no underscore) and non-ASCII characters with inner hyphens, dot-joined, no trailing dot, bracketed IPv4 and `[IPv6:...]` literals, 255 characters in all after `IDN.toASCII` (`EmailValidationUtil.java` L89-97); the 63-character label limit is not in `EmailValidationUtil` but comes from Java's `IDN.toASCII`, which throws for a longer label, and the validator treats the exception as invalid (L88-93)) -> "Invalid domain format: X". A `*` that is not followed by a dot is an ordinary character of that rule, so `*acme.example.com` and `ac*me.example.com` are **accepted** as plain domains; `a.b.c.d.e.f.g.h.i.j.k` is accepted too (the parts rule is for wildcards only). Cross-organization reuse: "Domain X is already linked to organization Y in realm Z". `verified` is free-form metadata (no verification flow), default `false`.
8. **Ids** are `KeycloakModelUtils.generateId()` = random UUID v4 strings (36 chars, lower-case). A client may supply `id` on import but not through `POST /organizations` (the resource ignores it; `provider.create(name, alias)` generates one).
9. **Delete**: removes the organization, its internal group, its IdP links (IdPs themselves stay) and its invitations. Members: **managed** members (federated through an IdP link with `membershipType=MANAGED`) are deleted from the realm; **unmanaged** members (existing realm users added via `POST /members`) remain as realm users. Deleting a member via `DELETE /members/{id}` follows the same rule.
10. **Disabled org**: still listed and manageable. Its **managed** members are served as disabled read-only users, so they cannot log in at all (`UserStorageManager.java` L126-130 via `Organizations.isReadOnlyOrganizationMember` L522-542); **unmanaged** members still log in as realm users, but the organization is skipped by the identity-first login routing (`OrganizationAuthenticator.java` L262) and by the token mappers, so tokens carry no claim for it (`OrganizationMembershipMapper.java` L213). Invitations are refused with "Organization is disabled" (`OrganizationInvitationResource.java` L109).
11. **`ListTenants` today** includes disabled organizations (no filter), returns at most 100 (one page, no follow-up), never sends `briefRepresentation=false` (so attributes would be absent anyway), and drops `alias`, `enabled`, `description`, `redirectUrl`, `attributes`, `verified`.
12. **Create response**: 201 with empty body and a `Location` header ending in the new id; the client must parse the id from `Location` or do a follow-up `GET`/search (`search=<name>&exact=true`).
13. **Permissions quirk**: `query-organizations` without `view-organizations` gives `200 []` and `count=0`, not 403. The backend README's advice to use "a dedicated service account with the `query-organizations` role" is therefore insufficient; see section 7.
14. **Content type**: all admin responses are `application/json`; a `GET` on a resource that does not exist under `/admin` without a token is 401 before 404 (auth is checked first; not verified against the hub).
15. **Admin events**: every create/update/delete/member change emits an admin event (`ResourceType.ORGANIZATION` / `ORGANIZATION_MEMBERSHIP`) if admin events are enabled on the realm; not visible through this API.

---

## 6. Authorization model

Source: `OrganizationPermissions.java` (26.8.0), `OrganizationMemberResource.java`, `AdminRoles.java`. Roles are **client roles of the `realm-management` client in the same realm** (`ai-grid`), or the corresponding `master` realm roles `<realm>-realm` client roles when administering from `master`.

| Operation | Permission check in code | Minimal realm-management role(s) |
|---|---|---|
| `GET /organizations`, `/count` | `orgs().requireQuery()`, then `canView()` to return non-empty | `view-organizations` (implies query). `query-organizations` alone: 200 `[]`. |
| `GET /organizations/{id}` | `requireView(org)`; 404 vs 403 decided by `canQuery()` | `view-organizations` |
| `POST`, `PUT`, `DELETE /organizations[/{id}]` | `orgs().requireManage()` | `manage-organizations` (implies view and query). `manage-realm` also works. |
| `GET /organizations/{id}/members[/count]` | `users().requireQuery()`, `canView()` for non-empty | `query-users` + `view-users` (plus `view-organizations` to reach the org: 404/403 otherwise) |
| `GET /members/{id}` | `users().requireView(user)` | `view-users` + `view-organizations` |
| `POST /members`, `DELETE /members/{id}`, `PUT .../membership-type` | `orgs().requireManage(org)` **and** `users().requireManage(user)` | `manage-organizations` + `manage-users` |
| `GET .../identity-providers[/{alias}]` | `orgs().requireView(org)` **and** `realm().requireViewIdentityProviders()` (`OrganizationIdentityProvidersResource.java` L133-134, L152-153) | `view-organizations` + `view-identity-providers` |
| `POST`, `PUT`, `DELETE .../identity-providers[/{alias}]` | `orgs().requireManage(org)` **and** `realm().requireManageIdentityProviders()` (L102-103, L223-224, L262-263) | `manage-organizations` + `manage-identity-providers` |
| `/invitations`, `invite-user` | `requireManage(org)`, needs realm SMTP configured | `manage-organizations` |

The admin console's "fine-grained admin permissions v2" (`admin-fine-grained-authz:v2`, enabled by default in 26.8) can scope these further per organization, but with it off (the default for a new realm; it is a realm switch `adminPermissionsEnabled`) the role table above is exact.

**Today's backend** authenticates as the bootstrap admin of `master` with the `password` grant on the public `admin-cli` client. That user has the `admin` realm role in `master` and therefore every `<realm>-realm` role for every realm, including `ai-grid-realm/manage-organizations`; it can also create and delete realms. Drawbacks: a shared human credential, a 60 s token fetched on every RPC (no caching in `client.go`), the password in a Secret that the backend reads at start, and no way to scope to one realm.

**Proposed**: a confidential client `ai-grid-backend` in the `ai-grid` realm with `serviceAccountsEnabled: true`, `standardFlowEnabled: false`, `directAccessGrantsEnabled: false`, `publicClient: false`. Its service-account user `service-account-ai-grid-backend` gets the `realm-management` client roles `manage-organizations`, `view-users`, `query-users`, `manage-users` (the last only once member management ships). Token request: `POST /realms/ai-grid/protocol/openid-connect/token` with `grant_type=client_credentials`, `client_id=ai-grid-backend`, `client_secret=<secret>`; cache the token until `exp - 30s`; on a 401 from the admin API, refetch once and retry.

---

## 7. Backend mapping today and gaps

`internal/keycloak/client.go` (`ListOrganizations`):

1. `POST {KEYCLOAK_URL}/realms/{KEYCLOAK_ADMIN_REALM}/protocol/openid-connect/token` with `grant_type=password`, `client_id=admin-cli`, `username`, `password`. Non-2xx: `fmt.Errorf("Keycloak token request returned %s: %s", status, body)` (first 4 KiB of body, which for a bad password is the `invalid_grant` JSON).
2. `GET {KEYCLOAK_URL}/admin/realms/{KEYCLOAK_REALM}/organizations?first=0&max=100` with `Authorization: Bearer`. Non-2xx: `fmt.Errorf("Keycloak organizations request returned %s: %s", status, body)`. Decodes into `struct{ID, Name string; Domains []struct{Name string}}`.
3. Network/DNS failure: `fmt.Errorf("request Keycloak token: %w", err)` / `"request Keycloak organizations: %w"`. No timeout beyond the gRPC context (`http.DefaultClient`).
4. `Service.ListTenants` returns these errors unchanged; a `nil` client yields `fmt.Errorf("Keycloak tenant client is not configured")`.

Consequence: every failure (unreachable, realm missing 404, bad credentials 400/401, forbidden 403, orgs disabled 404) reaches the BFF as gRPC `codes.Unknown` with a message containing Keycloak's status line and body; `bff-api.md` maps "any other backend code" to 502 with a generic message and logs the detail. Nothing is `Unavailable`, `NotFound` or `FailedPrecondition` today, so the BFF cannot distinguish "Keycloak down" from "misconfigured".

Config: env `KEYCLOAK_URL` (default `http://keycloak.ai-grid.svc:8080`), `KEYCLOAK_ADMIN_REALM` (`master`), `KEYCLOAK_REALM` (`ai-grid`), `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` (`cmd/ai-grid-backend/main.go`). The console's deploy already provides these from a deploy-time Secret (recent commits `69059c0`, `7a7d947`).

Mapping: `Tenant{Id: org.id, Name: org.name, Domains: [d.name for d in org.domains]}`. The BFF (`ai-grid-provider-console/bff/internal/api/tenants.go`) sorts by name and normalises `domains` to `[]`; the frontend type `Tenant {id, name, domains}` matches.

Gaps versus Keycloak and versus `bff-api.md` / `work-breakdown.md` Step 6:

| Needed by the console | Proto today | Keycloak has |
|---|---|---|
| Create, update, delete tenant (POST/PUT/DELETE `/api/v1/tenants[/{name}]`) | only `ListTenants` | `POST`/`PUT`/`DELETE /organizations` |
| Paging and search for a long tenant list | none (hard-coded 100) | `first`, `max`, `search`, `exact`, `count` |
| Show/toggle enabled | missing | `enabled` |
| Stable short handle (used in URLs; the BFF route uses `{name}` today) | `name` only | `alias` (immutable, URL-safe) |
| Description, free-form metadata | missing | `description`, `attributes` |
| Domain verification state, per-domain IdP routing | `domains: []string` | `domains[]{name, verified, identityProviderAlias, autoRedirect}` |
| Members (list, add existing user, remove, invite) | missing | `/members`, `/members/invite-user` |
| Error semantics (409/404/400 to the UI) | all `Unknown` -> 502 | distinct HTTP codes and messages |
| Credential | master bootstrap admin, password grant | realm-local service account, client_credentials |

---

## 8. Proposal: `Tenants` service

### 8.1 Proto: `proto/ai_grid/draft/v1` is authoritative

The authoritative `Tenants` draft is `proto/ai_grid/draft/v1/tenant_type.proto` + `tenants_service.proto`; `docs/draft/research/backend-conventions.md` section 6.6 is the earlier research draft (OSAC shapes: `offset/limit/filter/order` request, `size/total` + `tenants` response, `object` envelope, `update_mask`) and lists how the two differ. Do not reuse its numbers: the draft reserves `Tenant` field 8 (`version`) and `UpdateTenantRequest` field 3 (`lock`), makes `enabled` (5) an `optional bool`, adds `repeated TenantDomain domain_details = 9` (`name`, `verified`, `identity_provider_alias`, `auto_redirect`), and narrows `ListTenantsRequest.filter` to `this.name.lowerAscii().contains("<term>") || this.domains.exists(d, d.lowerAscii().contains("<term>"))` (case-insensitive; `lowerAscii()` is cel-go's strings extension). `Tenant` adds `string resource_version = 10` (an opaque version the backend keeps in the organization attribute `ai-grid/resource-version`; `UpdateTenantRequest` compares `object.resource_version` instead of `lock`, Aborted on mismatch), `DeleteTenantRequest` adds `string resource_version = 2`, and `ListTenantsResponse.total` is `optional`. An earlier page-token sketch here was removed because it conflicted with that draft (different paging model, envelope field names and `Tenant` field numbers 7-9) and failed buf STANDARD lint (`ENUM_VALUE_PREFIX`, `RPC_RESPONSE_STANDARD_NAME`, `RPC_REQUEST_RESPONSE_UNIQUE`). Member RPCs are not drafted yet (open question 3).

Keycloak-specific notes for whoever implements 6.6 against Keycloak (real backend and fake alike):

- **No server-side version exists.** Keycloak Organizations carry no version, etag or `Last-Modified`, so a Keycloak-backed backend could not honour `Tenant.version` or `UpdateTenantRequest.lock`; the draft in `proto/ai_grid/draft/v1` has therefore reserved both. The draft instead carries `Tenant.resource_version` (field 10), which the backend stores itself in the organization attribute `ai-grid/resource-version` and bumps on every write it makes; an edit made in Keycloak directly does not bump it. (The hub-backed `ManagedCluster` keeps `resource_version` + `lock`.)
- **Shipped field numbers stay.** `Tenant.id=1`, `name=2`, `repeated string domains=3` are frozen so the current BFF and frontend keep compiling; everything else is additive.
- **Create** = `POST /organizations` (201, empty body) -> parse the id from `Location` -> follow-up `GET /organizations/{id}` -> return the object. Duplicate name/alias -> `AlreadyExists` with Keycloak's `errorMessage`.
- **Update** = `GET /organizations/{id}` (full) -> apply `update_mask` to the representation -> `PUT` the whole representation (204) -> return the merged object. Always send `name`, the stored `alias`, the stored `redirectUrl` (absent clears it) and each kept domain with its `verified`, `identityProviderAlias` and `autoRedirect` (a domain sent by name only loses them, `OrganizationAdapter.setDomains` L201-204); an omitted alias becomes the name and fails, an omitted name is a 409 (5.4, 5.6). Remember 5.6: `attributes`/`domains` must be sent explicitly to change them; omitting them leaves them untouched, `{}`/`[]` clears them.
- **List** paging is offset/limit mapped to `first`/`max`; `total` comes from a second call to `GET /organizations/count` with the same filters (two Keycloak round trips per list). Results are ordered by name; `filter` (CEL) is evaluated by the backend, with `this.name`/`this.domains` equality and `startsWith/contains` translatable to Keycloak's `search`/`exact` where possible.
- **Attributes** are `map<string, list<string>>` in Keycloak; `map<string, string>` in the proto keeps the first value (lossy). Decide in open question 4.
- The BFF route should key tenants by `id` (`/api/v1/tenants/{id}`) rather than `{name}` as `bff-api.md` currently sketches; names can change, ids and aliases cannot.

### 8.2 gRPC status codes the real backend should return

| Keycloak outcome | gRPC code | BFF HTTP (per `bff-api.md`) |
|---|---|---|
| `409 {"errorMessage": "A organization with the same name already exists."}` / alias / `"User is already a member of the organization."` | `AlreadyExists`, message = `errorMessage` | 409 (the BFF maps `Aborted` to 409 as well, `ai-grid-provider-console/bff/internal/api/errors.go`) |
| `404 {"errorMessage": "Organization not found."}`, member `404` | `NotFound` | 404 |
| `400 {"errorMessage": ...}` (domain, alias, redirect URL, name blank) | `InvalidArgument`, message = `errorMessage` | 422 |
| Request validation before calling Keycloak (empty name, unsupported filter) | `InvalidArgument` | 422 |
| `404 {"errorMessage": "Organizations not enabled for this realm."}`, `404 {"error": "Realm not found."}` | `FailedPrecondition` (deployment misconfiguration, not a caller error) | 412 |
| Token endpoint `400/401 invalid_grant` / `invalid_client`, admin API `401`/`403` (backend credential problem) | `Internal`, generic message, detail logged (never forward the Keycloak body: it can echo the credential context) | 502 (generic) |
| Connection refused, DNS, TLS failure, 502/503 from the route, 5xx from Keycloak | `Unavailable` | 503 |
| Context deadline hit while talking to Keycloak | `DeadlineExceeded` | 504 |
| Caller is not a provider admin (once the backend checks the caller) | `PermissionDenied` | 403 |

Message texts the Keycloak outcomes above do not fix, proposed here so the backend and the console's fake use one wording instead of each inventing its own (a client only displays them, so any text works, but tests and support will want one):

| Situation | Proposed gRPC message |
|---|---|
| Caller is not a provider admin | `User "<x-forwarded-user>" is not a provider admin, and only provider admins can change tenants (a provider admin may patch ManagedClusters on the hub)` |
| Realm missing / organizations disabled (`FailedPrecondition`) | `Keycloak realm "<realm>": Realm not found.` / `Keycloak realm "<realm>": Organizations not enabled for this realm.` (Keycloak's text after the realm) |
| Keycloak unreachable (`Unavailable`) | `Keycloak is unavailable: <transport error>` |
| Request-shape checks before Keycloak is called (`InvalidArgument`; console proposals, Keycloak never sees these requests; the fake's current texts in `internal/fakeinfra/draft/tenants.go` and `keycloak.go`; search for the texts, line numbers move) | `filter %s is not supported; this server only evaluates %s`, `order is not supported by this server yet; Keycloak returns organizations by name`, `offset must not be negative, got %d`, `limit must not be negative, got %d`, `update_mask path %q is not supported; supported paths: ...`, `object is required`, `object.id must be empty; Keycloak assigns it` (the draft rejects a client id although Keycloak would ignore it), `id is required`, `object.id is required`, `domains and domain_details name different domains` |

The backend must read **both** error body shapes: `{"errorMessage": ...}` (resource errors) and `{"error": ...}` (an explicit alias with whitespace or a reserved character, realm-level 404s), or the UI sees an empty message for exactly those cases.

### 8.3 What the fake backend must simulate

- In-memory store keyed by id; generate UUID v4 ids; seed with a handful of tenants including one disabled and one with two domains and attributes.
- Uniqueness of `name` and `alias` (409 `AlreadyExists` with the exact Keycloak messages), alias defaulting to name, alias immutability (400 `InvalidArgument` "Cannot change the alias"), domain format rules and cross-tenant domain uniqueness (400 messages from 5.7), `enabled` default true.
- Paging: `offset`/`limit` under the hood, `total` from the count, results sorted by name; `search`-style filters as case-insensitive contains on name or domain; exact match as equality.
- Keycloak's update semantics (5.6) when no mask is given: scalars replaced, `enabled` defaults to true, `attributes`/`domains` unchanged when omitted and cleared only by `{}`/`[]`; mask-based merge when a mask is given.
- Serialisation like Keycloak: unset `description`/`redirectUrl` and empty `domains` are **omitted**, not `null`/`[]` (4.2), so the BFF/frontend tolerate absent keys.
- Delete removes the tenant and its memberships; members are "unmanaged" realm users and remain available for re-adding (keep a small fake user list to pick from).
- Error-injection hooks (as `backendtest.Fake.Err` does today) to return `Unavailable`, `FailedPrecondition` and `Internal`, and an optional latency knob (Keycloak via the route typically answers in 50-300 ms; the real backend does two HTTP round trips per RPC today: token + list).
- A token-expiry scenario is not visible to the BFF (it is internal to the backend), so the fake does not need one.
- Frontend `MockConsoleService` mirrors the same rules (409 on duplicate name, 404 on unknown id, 422 on invalid domain) using the BFF's `{"error": {"code", "message"}}` envelope so the UI's error handling is exercised without a backend.

---

## 9. Bootstrap plan: `ai-grid` realm and service-account client

Suitable for a deploy script run once per hub with a one-time admin credential (bootstrap admin or a temporary `master` user with the `admin` role). Fictional values throughout. `kcadm.sh` lives in `/opt/keycloak/bin` in the Keycloak image; the REST equivalents are given for a `curl`-only Job like the backend's `deploy/kind/keycloak.yaml`.

1. Authenticate the script (once, short-lived):
   ```sh
   kcadm.sh config credentials --server https://sso.example.com --realm master --user "$KC_BOOTSTRAP_ADMIN_USERNAME" --password "$KC_BOOTSTRAP_ADMIN_PASSWORD"
   # REST: POST /realms/master/protocol/openid-connect/token  grant_type=password client_id=admin-cli username=... password=...
   ```
2. Create the realm with organizations enabled (idempotent: 201 or 409):
   ```sh
   kcadm.sh create realms -s realm=ai-grid -s enabled=true -s organizationsEnabled=true -s displayName="AI Grid"
   # REST: POST /admin/realms  {"realm":"ai-grid","enabled":true,"organizationsEnabled":true}
   # If it already exists: PUT /admin/realms/ai-grid {"organizationsEnabled":true}  -> 204
   ```
3. Create the confidential service client:
   ```sh
   kcadm.sh create clients -r ai-grid -s clientId=ai-grid-backend -s protocol=openid-connect \
     -s publicClient=false -s serviceAccountsEnabled=true -s standardFlowEnabled=false \
     -s directAccessGrantsEnabled=false -s implicitFlowEnabled=false -s 'description=AI Grid backend service account'
   # REST: POST /admin/realms/ai-grid/clients {...same fields...}  -> 201, Location: .../clients/<client-uuid>
   CID=$(kcadm.sh get clients -r ai-grid -q clientId=ai-grid-backend --fields id --format csv --noquotes)
   kcadm.sh get clients/$CID/client-secret -r ai-grid        # {"type":"secret","value":"<generated>"}
   # or set your own: kcadm.sh update clients/$CID -r ai-grid -s secret="$GENERATED_SECRET"
   ```
   Store the secret in the backend's Secret (`KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`, `KEYCLOAK_REALM=ai-grid`, `KEYCLOAK_URL=https://sso.example.com` or the in-cluster `http://keycloak.<keycloak-namespace>.svc:8080`).
4. Grant the service-account user the minimal `realm-management` roles:
   ```sh
   kcadm.sh add-roles -r ai-grid --uusername service-account-ai-grid-backend --cclientid realm-management \
     --rolename manage-organizations --rolename view-users --rolename query-users
   # add --rolename manage-users when member add/remove ships; use view-organizations instead of manage-organizations for a read-only deployment
   # REST: GET /admin/realms/ai-grid/clients?clientId=realm-management -> <rm-id>
   #       GET /admin/realms/ai-grid/clients/<rm-id>/roles/manage-organizations -> role rep
   #       GET /admin/realms/ai-grid/clients/<client-uuid>/service-account-user -> {"id": "<sa-user-id>", ...}
   #       POST /admin/realms/ai-grid/users/<sa-user-id>/role-mappings/clients/<rm-id>  [ {role rep}, ... ] -> 204
   ```
5. Verify without the admin credential:
   ```sh
   TOKEN=$(curl -fsS -X POST https://sso.example.com/realms/ai-grid/protocol/openid-connect/token \
     -d grant_type=client_credentials -d client_id=ai-grid-backend -d client_secret="$SECRET" | jq -r .access_token)
   curl -fsS -H "Authorization: Bearer $TOKEN" 'https://sso.example.com/admin/realms/ai-grid/organizations?max=10'   # 200 []
   curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" -X POST -H 'Content-Type: application/json' \
     https://sso.example.com/admin/realms/ai-grid/organizations -d '{"name":"acme-robotics","domains":[{"name":"acme.example.com","verified":false}]}'   # 201
   ```
6. Optional hardening: set `accessTokenLifespan` for the client (`attributes."access.token.lifespan"`) to 300-900 s; turn on admin events for the realm (`adminEventsEnabled=true`) so tenant mutations are auditable; do not give the service account `manage-realm`.
7. Backend change: replace the password grant in `internal/keycloak/client.go` with `client_credentials` against `/realms/ai-grid/...` (same realm as the admin API), cache the token by `exp`, and drop `KEYCLOAK_ADMIN_REALM/USERNAME/PASSWORD`.

---

## 10. Open questions

1. **Which Keycloak and realm is authoritative for tenants?** The hub's Keycloak already imports a SaaS-style realm with `organizationsEnabled: true` and two organizations linked to per-tenant IdP realms. Is `ai-grid` a new realm, or should the console read that existing realm? (`work-breakdown.md` open question for the identity owners and the tenant-model team.)
2. **Tenant identity in URLs**: `bff-api.md` plans `/api/v1/tenants/{name}`; Keycloak's stable, URL-safe handle is `alias` (immutable) and the primary key is `id`. Recommend `id` in the API and `alias` for display/links.
3. **Members**: are AI Grid tenant members realm users of `ai-grid` (unmanaged, added by id), or federated through each tenant's IdP (managed, auto-membership)? This decides whether the console needs `AddTenantMember` or only read access plus `invite-user` (which also needs SMTP on the realm).
4. **Attribute conventions**: Keycloak attributes are `map<string, list<string>>`. Which keys does AI Grid want (region, billing account, quota)? The proto above flattens to the first value.
5. **Error detail exposure**: should Keycloak's `errorMessage` text be shown to users verbatim (it is reasonably user-friendly: "A organization with the same name already exists.") or re-worded by the backend?
6. **Admin permissions v2**: if the realm later enables fine-grained admin permissions, `query-only` callers get per-organization filtered lists instead of `[]`; the mock does not need this, but the role table in section 6 would change.
7. **Direct-grant status code**: 26.8.0 returns 400 for `invalid_grant` on bad credentials; 26.3.0 (and the backend's Kind setup) returns 401. The backend only checks `/100 != 2`, so it does not matter today, but a mock's token endpoint should pick 400 for 26.8.
8. **Version drift**: the backend's Kind setup pins Keycloak 26.3; the hub runs 26.8.0. The Organizations API is **not** identical between them (see section 2, "Backend's own dev setup"): 26.3 accepts only `manage-realm`, requires at least one domain, uses different domain error strings and returns 401 for `invalid_grant`; the invitation API and the per-domain `identityProviderAlias`/`autoRedirect` fields exist only in 26.8. Everything in this document describes 26.8.0; the backend's Kind pin should be moved to 26.8 before its integration tests are trusted.

---

## 11. Verification (2026-10-15)

Independently re-checked on 2026-10-15 against the Keycloak `26.8.0` source tree (clone at the tag), the `26.3.0` sources of the same files (fetched for comparison), the Admin OpenAPI document, the backend checkout at `fb419b3`, and the hub with `oc get` (names, keys, image, args, env names only) plus two unauthenticated GETs through the route (`/realms/master/.well-known/openid-configuration` -> 200 with `organization` scope; `/realms/ai-grid/...` -> 404 `Realm does not exist`). Corrections applied in this pass: null fields are omitted (`NON_NULL`) and `domains` is absent when empty (4.1, 4.2, 5.6); `attributes`/`domains` are unchanged on `null` and cleared only by `{}`/`[]` (5.6); brief `MemberRepresentation` fields (4.3); 26.3 vs 26.8 differences (2, 10.8); realm 409 body (3.4); `unauthorized_client` for a wrong secret and "Account disabled" only with a valid password (3.5); explicit-alias rejection covers reserved URL chars and uses the `{"error":...}` body (3.1, 5.4); identity-provider permissions split into read/mutate (6); PUT 409 also on name-vs-domain match (3.1); the tenant proto now lives only in `backend-conventions.md` 6.6 (8.1).

Not verified in this pass: the unauthenticated admin call (2, L58) and the 401-before-404 ordering (5.14), because admin endpoints were off limits; the organization counts and key names inside the realm-import ConfigMap (values were not read); the exact `OrganizationsValidation.validateUrl` message; whether the OpenAPI "latest" document is generated from 26.8.x (its content is consistent with 26.8); "GA since 26.0".

Audit (2026-10-16). Re-checked against the Keycloak source at tag `26.8.0` and this branch's
`proto/ai_grid/draft/v1`. Corrected: omitting `alias` on PUT is not safe (`toModel` sets the name before the
alias, and a blank alias becomes the new name, so the PUT fails "Cannot change the alias" unless the name equals
the stored alias), and a PUT without `name` is a 409 whenever the realm has any organization (3.1, 5.4, 5.6,
8.1); an unknown or unlinked domain `identityProviderAlias` is a 400 with `OrganizationAdapter`'s messages (3.1);
a PUT must also carry `redirectUrl` and the domain metadata (8.1); `access` never appears in a member
representation (4.3); `/members/{id}/organizations` takes no paging (3.2); the disabled-organization login
behaviour (managed members cannot log in, unmanaged members log in without the organization's claim; 5.10); the
63-character label limit comes from `IDN.toASCII`, the 255 limit from `EmailValidationUtil` (5.7); the hub's real
Keycloak namespace and Service DNS name are replaced by placeholders (1, 2, 9, 10); 8.1 and 8.2 now point to
`proto/ai_grid/draft/v1` as the authoritative draft (version and lock reserved, optional `enabled`,
`domain_details`, the narrowed filter) and mark the fake's request-shape texts as console proposals. Verified now
that were marked not verified: the redirect URL message (`OrganizationsValidation.java` L22-25) and "GA since 26.0"
(release notes `26_0_0.adoc` L1-3). Still not verified: the unauthenticated admin call and the 401-before-404
ordering (admin endpoints stay off limits), the realm-import ConfigMap contents, and whether the OpenAPI "latest"
document is generated from 26.8.x.
