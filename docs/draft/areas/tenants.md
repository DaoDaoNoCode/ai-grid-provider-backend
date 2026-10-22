# Tenants (Keycloak Organizations)

What the fake backend and the BFF implement for this area, where they deviate from the
research, which texts are invented, and what remains. The fixture data is described in
`docs/draft/fixtures/default.md`, the HTTP contract in `ai-grid-provider-console/docs/plan/bff-api.md`, the proto in
`docs/draft/proto/README.md`. The research reports cited here (`docs/draft/research/`) are background
reading. The code cites the Keycloak 26.8.0 source file and line beside every message and
check (`internal/fakeinfra/draft/keycloak.go`, `tenants.go`).

Implemented. Fake (`internal/fakeinfra/draft/{keycloak.go,tenants.go}`): an in-memory
organization store keyed by UUID v4 that answers like the backend's mapping of Keycloak 26.8
(`docs/draft/research/keycloak.md` 3, 5, 8). `CreateTenant` checks in Keycloak's order: an explicit
alias (`ReservedCharValidator.validateNoSpace`: ASCII whitespace anywhere in the alias first,
"Empty Space not allowed.", then the first RFC 3986 reserved character, "Character '/' not
allowed."; a no-break or other Unicode space passes), a blank name ("Name can not be null";
blank is Java's `String.isBlank`, so U+2003 is blank and U+00A0 is not), the alias defaulting
to the name ("Name cannot be used as alias: ..."), a taken name or alias (`AlreadyExists`, "A
organization with the same name already exists." / "...same alias already exists"), then the
domains: format with `Organizations.validateDomain`'s messages (blank is "Domain name cannot
be empty", from `OrganizationAdapter.validateDomain`), unique across the realm ("Domain X is
already linked to organization Y in realm ai-grid"), then the identity provider alias of each
domain. `enabled` is stored as sent (true when omitted). Domain names are trimmed (Java's
`trim()`: every character up to U+0020) and lower-cased before validation, as
`OrganizationDomainModel` does, so messages print the canonical form; the wildcard "parts"
messages print the base without `*.`, and a `*` not followed by a dot is an ordinary
e-mail-domain character (`*acme.example.com` is accepted).
Identity providers: the fake realm has none, so a domain with a non-empty
`identity_provider_alias` fails exactly as Keycloak fails for an unknown alias, InvalidArgument
"Identity provider with alias '<alias>' does not exist in realm ai-grid", on create, on update
and in `tenants.json` (the fake refuses to start). Keycloak's other answer, "Identity provider
'<alias>' is not associated with organization <name>", needs a real identity provider and
cannot occur. `autoRedirect` is stored without a check, as in Keycloak.
`UpdateTenant` is the backend's GET, merge, PUT (the console's proposal): mask paths `name`,
`domains`, `enabled`, `description`, `attributes`, `domain_details` replace the field as a
whole and everything else, `alias` included, goes back as stored. The path `alias` changes
nothing: the stored alias is kept, and a different non-blank one is "Cannot change the alias"
as below, so a client can send back the alias it loaded. A merge that leaves the organization
as stored is not PUT (console proposal: the backend compares its GET with the merged
representation, so Keycloak writes no admin event and the version is not bumped; Keycloak
itself would accept the PUT and write the event); the version precondition and Keycloak's
checks run first. Without a mask `name`,
`enabled` (true when unset, Keycloak's default) and `description` come from the request and
`attributes` and the domains stay, which is what a Keycloak PUT does with a body that lacks
those two keys. The `domains` path keeps the `verified`, identity provider and `autoRedirect`
of a domain that stays because the backend copies the stored values into the PUT; Keycloak
itself would reset them from a body with names only (`OrganizationAdapter.setDomains`
overwrites all three). The alias is the stored one unless the request names a non-blank
alias, and a different one is "Cannot change the alias"; a bare Keycloak PUT without `alias`
would instead default it to the new name and fail whenever that differs from the stored
alias (always for `globex-research`, alias `globex`). A new name that matches any
organization's name or domain, its own domains included, is `AlreadyExists` (Keycloak's exact
search matches both and does not exclude the organization itself), and so is a blank name,
an omitted one on the no-mask path included: Keycloak's search treats a blank term as no
filter, finds the organization itself and answers 409 "A organization with the same name
already exists.", not "Name can not be null".
`ListTenants` orders by name (byte order; Keycloak's `ORDER BY name` follows the realm
database's collation, not checked on the hub), includes disabled organizations and evaluates
`filter` as CEL over `this` (an `ai_grid.v1.Tenant`, proto field names) and `now`, with cel-go
and its strings extension (`ext.Strings()`, for `lowerAscii()`), in memory over the whole realm
list and before paging, as `docs/draft/research/backend-conventions.md` 5.3 says for List (a realm's
organizations are few), so `total` counts the matches (`internal/fakeinfra/draft/celfilter`).
Any expression works; the one the BFF used to write, `this.name.lowerAscii().contains("<term>")
|| this.domains.exists(d, d.lowerAscii().contains("<term>"))`, is the case-insensitive contains
on the name or any domain that Keycloak's `search` means, provided the caller lowers the term.
The evaluation is strict CEL, with no Keycloak folding: `lowerAscii()` lowers A-Z only, so a
term with a capital letter matches nothing, a non-ASCII letter is compared exactly, a
whitespace-only term is a substring like any other (Keycloak's search treats it as no filter),
and `%` and `_` are literal characters. A filter that does not compile or is not a bool is
`InvalidArgument` before Keycloak is asked; one that fails on an organization (an unguarded
`this.attributes["k"]` on one without `k`, CEL's `no such key`) is `InvalidArgument` after the
Keycloak gate, because it needs the organizations. The paging rules are the
contract's, not Keycloak's: Keycloak's `max` defaults to 10 and it ignores a negative `first`
or `max`, so the backend must send an explicit `max` for an omitted `limit` and reject
negatives itself. A backend that pushes a simple term down to Keycloak's `search` instead of
evaluating in memory must get the same answer, and must escape `%`, `_` and `\` first,
because that `search` is a SQL LIKE without escaping or an ESCAPE clause. `Get`, `Update` and `Delete`
of an unknown id answer `NotFound` with the conventions' message `object with identifier
'<id>' not found` (the tenant id, not a name) and log Keycloak's "Organization not found.".
Everything Keycloak would decide is answered only after the Keycloak gate, so with Keycloak
unreachable a bad domain is `Unavailable`, as it would be from the real backend. Mutations
are gated by the provider-admin rule of `access.json` (`PermissionDenied`); every RPC needs
`authorization`. `tenants.json` selects the three failure modes (`unavailable`,
`realmMissing`, `organizationsDisabled`); latency applies.
Version (the console's proposal to the backend). Keycloak Organizations carry no version,
etag or `Last-Modified` (`docs/draft/research/keycloak.md` 8.1), so the backend keeps one itself:
the organization attribute `ai-grid/resource-version`, a decimal counter it writes as `1` on
create and as the stored value plus one with every PUT it makes, and maps to
`Tenant.resource_version` (field 10; field 8 and the name `version` stay reserved), never to
`attributes`. `UpdateTenant` compares a non-empty `object.resource_version`, and
`DeleteTenant` a non-empty `resource_version`, with the stored value after its Keycloak GET
and before any other check, and answers `Aborted` on a mismatch; an empty one is not checked.
The fake keeps the counter in the stored tenant instead of an attribute, which a client
cannot tell apart: the key never appears in `attributes`, a create or an `attributes` update
that sets it is `InvalidArgument`, a fixture organization starts at `1` (or at the
`resourceVersion` it names) and a fixture that sets the attribute does not load. Two limits
of the attribute, for the backend team: an edit made in Keycloak directly (the admin console
or another client) does not bump it, so it guards only writes through the backend; and the
GET, compare and PUT are not atomic in Keycloak, so two backend replicas could both pass the
check for the same version (a single replica can serialise them with a per-tenant lock, as
the fake does with its store lock).
The BFF maps `If-Match` to these fields and sends the version as the `ETag`
(`ai-grid-provider-console/docs/plan/bff-api.md`, "Writes, versions and lists").
Known differences of the e-mail domain rule (`EmailValidationUtil`, Keycloak's copy of
hibernate-validator). ASCII domains are decided exactly as Keycloak decides them, the
bracketed IPv4 and `IPv6:` literals included; the differences are:
- `java.net.IDN.toASCII` runs nameprep (RFC 3491) on every non-ASCII label and rejects
  prohibited or unassigned code points (for example U+00A0 or U+0085), bidi violations and a
  non-ASCII label starting with `xn--`; the fake does not, so it accepts some non-ASCII
  domains Keycloak rejects.
- The 63-character label cap (IDN) and the 255-character domain cap apply to the punycode
  form; the fake counts input characters, so a long non-ASCII label or domain can pass the
  fake and fail Keycloak. IDN also splits labels at U+3002, U+FF0E and U+FF61; the fake does
  so for the label cap only.
- Lower-casing is Java's `toLowerCase()` in the server's default locale; the fake uses Go's
  `strings.ToLower`, which differs for U+0130, a word-final capital sigma and a Turkish
  default locale. Trimming is exact.
- With several bad domains Keycloak reports one in `HashSet` order (format and owner per
  domain, identity providers afterwards); the fake checks every format in request order,
  then every owner, then every identity provider alias. Of names equal after canonicalising,
  Keycloak keeps one in that order too; the fake keeps the first sent.
Invented here, because nothing in Keycloak or the conventions fixes a text, and proposed to
the backend so they become the contract (`docs/draft/research/keycloak.md` 8.2 has the
permission, realm, unavailable and request-shape texts; the `update_mask` and list texts are
proposed here). A client only displays them. Fake backend (gRPC status messages):
- `PermissionDenied`: `User "<user>" is not a provider admin, and only provider admins can
  change tenants (a provider admin may patch ManagedClusters on the hub)` (`models` for a model
  write; `authorizeProviderAdmin`, `server.go`). It names what the caller tried to change, then
  the rule, so a viewer creating a tenant is not told about ManagedClusters alone.
- `FailedPrecondition`: the wrapper `Keycloak realm "ai-grid": ` before Keycloak's "Realm not
  found." or "Organizations not enabled for this realm.".
- `Unavailable`: the prefix `Keycloak is unavailable: ` before the fixture's text (the text
  after it is whatever `tenants.json` says; Keycloak says nothing when it is unreachable).
- Request shape, all `InvalidArgument`: `object is required` (Keycloak: "Organization cannot
  be null."), `object.id must be empty; Keycloak assigns it` (Keycloak ignores a client id;
  the draft rejects it), `object.id is required` (update), `id is required` (get, delete;
  Keycloak: "Id cannot be null."), `update_mask path "<p>" is not supported; supported paths:
  name, alias, domains, enabled, description, attributes, domain_details`, `domains and
  domain_details name different domains`.
- List, all `InvalidArgument`: `filter <filter> is not a valid CEL expression: <cel-go's
  error>`, `filter <filter> must evaluate to a bool, not <type>`, `filter <filter> failed on
  '<id>': <cel-go's error>` (the wrapper is the console's, the error after the colon cel-go's,
  for example `no such key: k`), `order is not supported by this server yet; Keycloak returns
  organizations by name`, `offset must not be negative, got <n>`, `limit must not be
  negative, got <n>`.
- Version: `Aborted` `object with identifier '<id>' has resource version <v>, the request
  carries "<v>"; fetch it again and retry` (the clusters' wording); `InvalidArgument`
  `attribute "ai-grid/resource-version" is reserved for the tenant's version; it cannot be
  set`.
- `NotFound`: `object with identifier '<id>' not found` is the conventions' text, not
  Keycloak's.
- `Unauthenticated` (shared with the other areas, `server.go`): `missing authorization
  metadata`, `authorization metadata must be 'Bearer <token>'`, `the hub rejected the token:
  Unauthorized`.
BFF (`ai-grid-provider-console/bff/internal/api/tenants.go`): `query parameter "offset"` (or `"limit"`) `must be an
integer between 0 and 2147483647` (422); `query parameter "search" is not supported; send a CEL
filter in "filter"` (422); the 428 for a write without `If-Match`; the alias is sent to the
backend like any other field (the `alias` path), which answers a changed one; the
body-decoding (`invalid request body: ...`) and status texts (`errors.go`) are shared with the
other areas.
BFF: `GET /api/v1/tenants?offset=&limit=&filter=` with a `meta` page and the client's
`filter` forwarded unchanged (the BFF no longer builds an expression from a `search` term),
`GET/PATCH/DELETE /api/v1/tenants/{id}` (a PATCH body that names no field is 422),
`POST /api/v1/tenants` (201 + Location); Keycloak's messages reach the client as 422 and
409 (a blank name on PATCH is 409,
see above); an absent `enabled` from today's backend (fields 1-3 only) reads as enabled; the
JSON has no `redirectUrl` and carries `resourceVersion` (also the `ETag` of a single tenant);
PATCH and DELETE need `If-Match` (428 without it) and map it to the version fields above (409
when stale). Against the shipped backend, which has only `ListTenants`, `GET
/api/v1/tenants/{id}` has no list fallback (unlike `GET /api/v1/clusters/{name}`) and answers
501, as does every tenant write, and so does a list with `filter`, `limit` or an `offset` above 0: the shipped `ListTenantsRequest`
is empty and its response has no `total`, so the BFF cannot tell a filtered result from the
unfiltered list and refuses it (`ai-grid-provider-console/docs/plan/bff-api.md`).
UI: main already has a basic, read-only Tenants page (`ai-grid-provider-console/frontend/src/pages/TenantsPage.tsx`:
name, domains, id), which keeps working because the list's `data` is still an array of
tenants with those fields. Porting the fuller UI for this area (search, paging, create, edit,
enable and delete) is out of scope here; it builds on the HTTP contract in
`ai-grid-provider-console/docs/plan/bff-api.md`.
Remaining here: members (list, add, remove, invite; the draft has no member RPCs yet, open
question 3 in `docs/draft/research/keycloak.md`), identity providers (the fake realm has none, so no
domain can be routed to one), the realm and service-account bootstrap (section 9), and
for the real backend the choice between evaluating `filter` in memory (as the fake does) and
pushing simple terms down to Keycloak's `search` and `exact`.
Backend ask: the `Tenants` CRUD proto as now drafted in `proto/ai_grid/draft/v1/tenant*.proto`
(`resource_version` kept in an organization attribute as above, `domain_details`, `optional
bool enabled`, an optional `ListTenantsResponse.total`), keyed by `id`;
an update that merges onto a full GET so the PUT carries the stored `alias`, `redirectUrl`
and domain metadata; the gRPC code table and the proposed message texts in
`docs/draft/research/keycloak.md` 8.2 (today every failure is `Unknown`); reading both Keycloak
error body shapes (`{"errorMessage": ...}` from `ErrorResponse`, `{"error": ...}` from a
JAX-RS exception such as an explicit alias's "Empty Space not allowed." or "Realm not
found."); a scoped service-account client instead of the master admin.
