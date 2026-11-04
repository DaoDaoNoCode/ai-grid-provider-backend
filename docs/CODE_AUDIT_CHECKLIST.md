# Code audit checklist — `ai-grid-provider-backend`

Commentable backlog from four parallel audits (Security, SOLID, Style, Coding Standards).

## How to use

For each item, set **Decision** to one of:

| Value | Meaning |
|-------|---------|
| `TODO` | Not decided yet |
| `DO` | Approve — implement / track |
| `SKIP` | Explicitly decline for now |

Add freeform notes under **Notes / user comments**. Agents and humans should treat `DO` items as approved work and `SKIP` as out of scope unless revisited.

**Suggested triage order:** Critical/High Security → High Standards mismatches that confuse agents → High SOLID extractions → Style cleanups.

---

## 1. Security

Source: [Security audit](4b55d055-730f-480a-8f6a-cc82d8e5cc7d)

### SEC-001: No API AuthN/AuthZ on gRPC/Connect/REST
- [ ] Decision: Skip for now will do eventually.
- Severity: Critical
- Location: `AGENTS.md` (Security and tenancy); `cmd/ai-grid-provider-backend/main.go`; `internal/servers/*`
- Finding: Documented MVP choice: every Clusters/Tenants/Models/Subscriptions method is open. No bearer/OIDC/kube-token check and no caller identity on the request path. Anyone who can reach the listeners acts with the ServiceAccount and Git token.
- Recommendation: Before any non-loopback or multi-tenant exposure, require authenticated identity (e.g. kube-auth-proxy / OIDC) and enforce AuthZ per RPC; reject unauthenticated calls in the binary itself, not only via an external proxy.
- Notes / user comments:
  -

### SEC-002: Unauthenticated ClusterIP Service (no NetworkPolicy)
- [ ] Decision: SKIP
- Severity: Critical
- Location: `deploy/openshift/backend-service.yaml`; `deploy/openshift/backend-deployment.yaml`; `hack/openshift-up.sh`
- Finding: OpenShift manifests expose ports 8080/8081 on a ClusterIP Service with no NetworkPolicy and no loopback bind. AGENTS.md describes a console-sidecar loopback model; these manifests do not. Any pod (or port-forward) in the cluster can call the open API.
- Recommendation: Bind to `127.0.0.1` when co-located with an authenticating proxy, or add NetworkPolicies that allow only the console/BFF; never expose via Route/Ingress/LoadBalancer until AuthN exists.
- Notes / user comments:
  -

### SEC-003: High privilege blast radius once reached
- [ ] Decision: SKIP
- Severity: Critical
- Location: `deploy/openshift/rbac.yaml`; `internal/servers/clusters_server.go`; `internal/servers/tenants_server.go`; `internal/gitops/`; `cmd/.../main.go`
- Finding: A successful caller can create/update/delete ManagedClusters (including accept), CRUD Keycloak Organizations via admin credentials, and commit Models/Subscriptions that Argo CD applies to spokes. Impact is hub-wide and spoke-wide.
- Recommendation: Treat AuthN as blocking for production; split credentials (read vs write, Git vs Keycloak vs clusters) and add human/approval gates for destructive cluster and catalog writes where feasible.
- Notes / user comments:
  -

### SEC-004: Unrestricted model images and fetch URIs → spokes
- [ ] Decision: SKIP
- Severity: High
- Location: `internal/servers/model_mapper.go`; `internal/servers/models_server.go`; `internal/servers/models_git.go`
- Finding: `spec.model.uri` allows `hf://`, `oci://`, `s3://`, `pvc://`, `http://`, `https://`; `spec.main.image` only rejects whitespace. Combined with SEC-001, a caller can commit arbitrary container images / remote sources that Argo CD deploys onto inference spokes.
- Recommendation: Allowlist registries and URI schemes (prefer `oci://`/`hf://` over cleartext `http://`); optionally require signed images / digest pins; document that catalog write ≡ code execution on spokes.
- Notes / user comments:
  -

### SEC-005: Keycloak Admin via password grant + powerful credentials
- [ ] Decision: SKIP
- Severity: High
- Location: `internal/keycloak/client.go`; `cmd/.../main.go`; `deploy/openshift/backend-deployment.yaml`; `hack/openshift-up.sh`
- Finding: Tenants API uses Resource Owner Password Credentials against the admin realm with username/password (often realm-admin for embedded Keycloak). Compromising the backend or its Secret yields full Organization admin on `KEYCLOAK_REALM`.
- Recommendation: Use a confidential client + client_credentials service account with least-privilege organization roles only; ban password-grant for non-local; refuse startup if credentials equal `admin`/`admin` outside Kind/dev.
- Notes / user comments:
  -

### SEC-006: `make run` binds all interfaces without AuthN
- [ ] Decision: TODO | DO | SKIP
- Severity: High
- Location: `Makefile` (`LISTEN_ADDRESS ?= :8080`); `cmd/.../main.go`; contrast `hack/run-hub.sh`
- Finding: Local `make run` listens on all interfaces by default while using the operator’s kubeconfig. `run-hub` enforces loopback; `make run` does not refuse non-loopback addresses.
- Recommendation: Default `make run` to `127.0.0.1` and refuse non-loopback binds in the binary when AuthN is absent.
- Notes / user comments: Lets make a plan from this and fix. 
  -

### SEC-007: Subscription org ID not verified against Keycloak
- [ ] Decision: SKIP
- Severity: Medium
- Location: `internal/servers/subscriptions_git.go`; `internal/servers/subscriptions_server.go`
- Finding: `spec.organization.id` is shape-checked (DNS label) and used to qualify owner groups for MaaSAuthPolicy, but is not verified to be an existing Keycloak Organization.
- Recommendation: On Create/Update, resolve `organization.id` via Keycloak (or a trusted tenant store) and reject unknown orgs; when AuthZ exists, ensure the caller may manage that tenant.
- Notes / user comments:
  -

### SEC-008: ACM Search `searches/allManagedData` over-privilege
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Location: `deploy/openshift/rbac.yaml`; `internal/acmsearch/client.go`
- Finding: SA may `get` Search’s all-managed-data virtual resource. Client code queries only GPU nodes and refuses redirects / caps body size, but RBAC is not narrowed.
- Recommendation: Prefer per-namespace/clusterview Search permissions when AuthZ is per-user; until then keep Search URL https-only and never follow redirects (already implemented).
- Notes / user comments:
  -

### SEC-009: Keycloak admin password in process environment
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Location: `deploy/openshift/backend-deployment.yaml`; contrast `AI_GRID_GIT_TOKEN_FILE`
- Finding: Git token is correctly mounted as a file. Keycloak password is injected as an env var. OpenShift deploy defaults embedded Keycloak to `admin`/`admin` unless overridden.
- Recommendation: Mount Keycloak credentials as files (same pattern as Git); require strong unique passwords for non-Kind; prefer short-lived client secrets over long-lived admin passwords.
- Notes / user comments:
  -

### SEC-010: Embedded Keycloak is `start-dev` with weak DB secret
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Location: `deploy/openshift/keycloak.yaml`; Kind parity manifests
- Finding: OpenShift “embedded” Keycloak is a development stack (dev mode, HTTP, bootstrap admin, static DB password in manifest `stringData`). Fine for smoke; unsafe if treated as production IdP.
- Recommendation: Document as non-production only; for real hubs require `EXTERNAL_KEYCLOAK=1` with TLS and a hardened realm; rotate/remove default DB passwords from checked-in manifests for shared clusters.
- Notes / user comments:
  -

### SEC-011: Swagger UI, OpenAPI, and gRPC reflection unauthenticated
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Location: `internal/restgateway/server.go`; `cmd/.../main.go`
- Finding: Full API surface discovery and interactive docs are served without auth on the HTTP listener.
- Recommendation: Disable reflection and Swagger in production builds/flags, or serve them only behind the same AuthN as the API.
- Notes / user comments:
  -

### SEC-012: Kind deployment lacks pod security hardening
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Location: `deploy/kind/ai-grid-provider-backend.yaml` vs `deploy/openshift/backend-deployment.yaml`
- Finding: OpenShift Deployment sets `runAsNonRoot`, `seccompProfile`, `readOnlyRootFilesystem`, dropped caps. Kind Deployment does not, and embeds `KEYCLOAK_ADMIN_PASSWORD=admin` in plain env.
- Recommendation: Align Kind securityContext with OpenShift where practical; keep Kind credentials local-only.
- Notes / user comments:
  -

### SEC-013: REST gateway missing `ReadHeaderTimeout`
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Location: `internal/restgateway/server.go`; contrast `cmd/.../main.go`
- Finding: Primary connect/gRPC server sets `ReadHeaderTimeout`/`IdleTimeout`; the REST/Swagger server does not.
- Recommendation: Apply the same header/idle timeouts to the gateway `http.Server`.
- Notes / user comments:
  -

### SEC-014: Keycloak URL allows cleartext HTTP by default
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Location: `cmd/.../main.go`; `hack/openshift-up.sh`
- Finding: Admin password grant traffic to Keycloak can run over HTTP. External plaintext is only warned, not blocked. Search correctly requires https.
- Recommendation: Require `https://` for non-loopback/non-cluster Keycloak URLs (fail closed), matching Search’s policy.
- Notes / user comments:
  -

### SEC-015: Supply-chain hygiene is solid but image tags still float
- [ ] Decision: TODO | DO | SKIP
- Severity: Info
- Location: `Containerfile`; `.github/dependabot.yml`; `.github/workflows/ci.yaml`; `deploy/openshift/keycloak.yaml`
- Finding: Go modules, CI actions, and backend base images are digest-pinned with Dependabot. Embedded Keycloak/Postgres/curl images use floating tags.
- Recommendation: Pin third-party deploy images by digest; keep Dependabot PRs reviewed.
- Notes / user comments:
  -

### SEC-016: Positive controls already in place (verify they stay)
- [ ] Decision: TODO | DO | SKIP
- Severity: Info
- Location: `hack/openshift-up.sh`; `internal/acmsearch/client.go`; `internal/gitops/`; `internal/celfilter/`; `internal/deploycheck/rbac_test.go`; OpenShift pod `securityContext`
- Finding: Maintainers already mitigate several risks: no public Routes in up script, Search no-redirects/size cap, Git credentials not in URL/logs, path traversal checks, CEL cost bounds, RBAC write-scope regression test, non-root read-only container on OpenShift.
- Recommendation: Keep these as release gates; add CI/policy checks that fail if a Route/Ingress reappears or if listen-address is non-loopback without AuthN.
- Notes / user comments:
  -

### Security posture (summary)
- Auth is the blocking gap; ClusterIP layout does not match the documented loopback+proxy model.
- Compromise of the listener ≈ hub + catalog + Keycloak org admin.
- Catalog writes are a spoke supply-chain path without image/URI allowlisting.
- Secrets handling is mixed (Git file good; Keycloak env + defaults weaker).
- Local/dev footguns: `make run` all-interface bind; Kind plaintext admin creds.

---

## 2. SOLID / separation of concerns

Source: [SOLID design audit](477777f1-261c-4916-827a-4a836bea6cd6)

### SOLID-001: `internal/servers` is a god package
- [ ] Decision:  SKIP
- Severity: High
- Principle: SRP | SoC | Modularity
- Location: `internal/servers/`
- Finding: One package owns Connect handlers, Git catalog persistence/rendering, K8s unstructured mapping, ACM Policy status parsing, GPU inventory caching, and static cloud-region geodata (~5k+ non-test LOC).
- Recommendation: Keep thin per-resource handlers in `servers/`; extract deep packages for catalog Git I/O+render, cluster mapping/location/state, and status/GPU.
- Notes / user comments:
  - SKIP confirmed; no god-package rewrite this pass.

### SOLID-002: No service/domain layer under Connect handlers
- [ ] Decision: TODO | DO | SKIP
- Severity: High
- Principle: SoC | SRP
- Location: `internal/servers/models_server.go`, `subscriptions_server.go`, `clusters_server.go`, `tenants_server.go`
- Finding: Handlers *are* the application: validation, Git commits, CR reads, status enrichment, and Connect error mapping live on `*Server` methods. There is no injectable service/use-case type between transport and infrastructure.
- Recommendation: Introduce small resource services (e.g. `ModelCatalog`, `SubscriptionCatalog`, `ManagedClusterStore`) with plain Go errors; leave handlers as request validation + status mapping + call-through.
- Notes / user comments:
  - Deferred: no resource service layer yet; this pass only DIP interfaces + helpers + policystatus split.

### SOLID-003: Asymmetric dependency inversion across servers
- [x] Decision: DO
- Severity: High
- Principle: DIP | Modularity
- Location: `tenants_server.go` (`organizationClient`); Models/Subscriptions (`*CatalogStatusReader`); Clusters (`*GPUInventory`)
- Finding: Tenants correctly depends on a narrow interface. Models/Subscriptions/Clusters take concrete types. `gitops.Writer` is already a good seam.
- Recommendation: Define consumer-owned interfaces where servers need only `Read(...)` / `Clusters(ctx)`; keep `gitops.Writer` as the model for seams.
- Notes / user comments:
  - Introduced catalogStatusReader and clusterGPUSource interfaces on Models/Subscriptions/Clusters builders (Tenants organizationClient pattern). Wire type still *CatalogStatusReader / *GPUInventory.

### SOLID-004: Connect/transport errors leaked into domain helpers
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Principle: SoC | DIP
- Location: `models_git.go`; `subscriptions_git.go`; mappers/validators returning `connect.Errorf`
- Finding: Placement rules, Git path errors, and cross-ref checks return `*connect.Error` from helpers that otherwise look like domain/catalog logic.
- Recommendation: Return typed/sentinel domain errors from catalog/mapper packages; map to Connect codes only at the handler edge.
- Notes / user comments:
  - Partial/deferred: mapGitopsError still returns Connect errors from git helpers; domain sentinels at helper edge left for a later slice.

### SOLID-005: Models ↔ Subscriptions cross-resource coupling inside one package
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Principle: SRP | SoC | OCP
- Location: `models_git.go`; `subscriptions_git.go`
- Finding: Model delete and subscription model-ref joining both know the other resource’s on-disk layout and parsers via package-private helpers.
- Recommendation: Extract a narrow `CatalogRefs` / `ModelReferenceIndex` (or shared `internal/catalog` read API) owned once; inject it into both servers.
- Notes / user comments:
  - Deferred: CatalogRefs extraction not started.

### SOLID-006: Hub CR / multi-doc YAML as the working domain model
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Principle: Modularity | SoC
- Location: `models_git.go`; `subscriptions_git.go`; `model_mapper.go`; `subscription_mapper.go`
- Finding: Business writes are expressed as `unstructured.Unstructured` map surgery and YAML document lists. Hub/spoke CR shapes leak through the server package’s mental model.
- Recommendation: Deepen an adapter module (e.g. `internal/catalogrender`) with proto → paths/CR docs and reverse; confine unstructured/YAML there.
- Notes / user comments:
  -

### SOLID-007: `CatalogStatusReader` mixes infra I/O and compliance domain parsing
- [x] Decision: DO (partial)
- Severity: Medium
- Principle: SRP | Modularity
- Location: `internal/servers/catalog_status.go`
- Finding: One type both talks to hub Policies via `dynamic.Interface` and owns fragile compliance-message parsing.
- Recommendation: Split pure `parseCompliance` / report aggregation into a small status domain type; keep a thin Policy client that returns raw messages or unstructured.
- Notes / user comments:
  - Partial: extracted pure ParseCompliance into internal/policystatus; CatalogStatusReader keeps Policy I/O. Further status aggregation still in servers (follow-up).

### SOLID-008: Cluster domain logic trapped in the handler package
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Principle: SRP | Modularity
- Location: `managed_cluster_state.go`, `managed_cluster_location.go`, `managed_cluster_mapper.go`, `cloud_regions.go`, `managed_cluster_gpus.go`
- Finding: Pure-ish rules already sit beside Connect/K8s handlers. `cloud_regions.go` is a large static dataset inside `servers`. `geo` is correctly extracted; the rest is not.
- Recommendation: Move state/location/region lookup (and optionally GPU label mapping) to `internal/clusters` (or extend `geo`).
- Notes / user comments:
  -

### SOLID-009: Shared list/pagination helpers mis-owned under cluster mapper
- [x] Decision: DO
- Severity: Low
- Principle: SRP | ISP
- Location: `managed_cluster_mapper.go` (`validateListLimit`, `applyListWindow`)
- Finding: Cross-cutting List paging lives in a ManagedCluster mapper file, blurring ownership.
- Recommendation: Move to a tiny `internal/listpage` (or `servers/list.go` with neutral name).
- Notes / user comments:
  - Moved validateListLimit/applyListWindow to servers/list.go (neutral ownership; kept in servers to avoid Connect dependency in a tiny package).

### SOLID-010: Duplicated identity validation (and similar constants)
- [x] Decision: DO
- Severity: Low
- Principle: SRP | OCP
- Location: `clusters_server.go` (`managedClusterNameRE`); `tenants_server.go` (`tenantNameRE`)
- Finding: Same RFC 1123 naming rule copied per server; ready-policy splitting also shared across Clusters/Models.
- Recommendation: One `internal/names` (or validation) helper for DNS-label IDs; keep ready-policy parsing next to the Policy/status module.
- Notes / user comments:
  - Shared DNS-label check in internal/names.ValidDNSLabel; Clusters/Tenants/Models/Subscriptions call sites updated.

### SOLID-011: Composition root in `main` is growing infra factories
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Principle: SoC | DIP
- Location: `cmd/ai-grid-provider-backend/main.go`
- Finding: Search TLS/credential assembly and Git background writer setup are substantial; `main` exports/uses concrete `*servers.GPUInventory`.
- Recommendation: Extract `internal/app` or `internal/wiring` that returns interfaces; keep `main` as flags + serve/shutdown only.
- Notes / user comments:
  -

### SOLID-012: `client` aggregates all RPCs (acceptable, slightly wide for ISP)
- [ ] Decision: TODO | DO | SKIP
- Severity: Info
- Principle: ISP | SoC
- Location: `client/client.go`
- Finding: Single fat `Client` forces every consumer to depend on the whole API surface. Fine for smoke/integration.
- Recommendation: Optional per-service constructors only if a caller needs a narrow fake; otherwise SKIP.
- Notes / user comments:
  -

### SOLID-013: Good deep modules exist—preserve and extend that pattern
- [ ] Decision: TODO | DO | SKIP
- Severity: Info
- Principle: Modularity | DIP | LSP
- Location: `internal/gitops`, `keycloak`, `acmsearch`, `geo`, `celfilter`, `restgateway`
- Finding: These packages expose small surfaces and hide implementation — local gold standard for deepening.
- Recommendation: Treat new extractions from `servers` as peers of these packages; do not fold more domain into `servers` or `cmd`.
- Notes / user comments:
  -

### SOLID-014: Draft fake duplicates real concerns (intentional, still a SoC risk)
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Principle: SoC | Modularity
- Location: `internal/fakeinfra/draft/` vs real `internal/servers`
- Finding: Draft is a temporary console fake. Duplicated CEL/filter and area logic can diverge until areas migrate.
- Recommendation: When moving an area to real `v1`, delete draft code. Prefer sharing pure helpers. SKIP large draft↔real unification.
- Notes / user comments:
  -

### SOLID-015: Tenants write-serialization policy lives inside the handler type
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Principle: SRP | SoC
- Location: `tenants_server.go` (`writeLocks`, `lockTenant`)
- Finding: Optimistic-concurrency workaround is correct for single-replica MVP but is concurrency policy embedded in the Connect server.
- Recommendation: If multi-replica is planned, extract a `TenantStore`; otherwise document as known shallow seam and SKIP until needed.
- Notes / user comments:
  -

### SOLID posture (summary)
- Composition and leaf packages (`gitops`, `keycloak`, etc.) are healthy; Tenants’ `organizationClient` is the best DIP example.
- Main weakness: shallow, overloaded `internal/servers`.
- Highest leverage: split catalog + status + cluster domain; Connect mapping at the edge; invert concrete status/GPU deps.
- Avoid fulfillment-sized package trees or draft↔real unification.

---

## 3. Style

Source: [Style consistency audit](57da2731-40ff-49b0-b819-0360cd59d17c)

All STYLE-001–014 items from that audit are done (or confirmed Info no-op for the
`gen/` boundary). House style now: Connect-v2 + builders for servers/`client`,
`New`+Config for infra clients, `Errorf`+`WithCause`, request-path
`slog.*Context`, stdlib `testing`, OSAC kept for API field/shape in `docs/API.md`
with Ginkgo marked fulfillment-reference only.

---

## 4. Coding standards

Source: [Coding standards audit](300deec2-5ac1-45dd-9f29-5343ee5f18dd)

### STD-001: Authoritative `docs/API.md` still documents OSAC, not this repo
- [ ] Decision: TODO | DO | SKIP
- Severity: High
- Standard source: AGENTS.md; docs/API.md
- Location: `docs/API.md`
- Finding: AGENTS.md treats `docs/API.md` as authoritative, but large sections still prescribe fulfillment-service layout (Ginkgo, `internal/logging`/`metrics`, OSAC linters). This repo disagrees. AGENTS.md says: if docs and executable config disagree, stop and report — this is that mismatch.
- Recommendation: Split “OSAC reference (non-normative)” from “ai-grid norms (normative)”, or rewrite normative sections to match current packages/tests/linters.
- Notes / user comments:
  -

### STD-004: Proto contract gates are strong and mostly aligned
- [ ] Decision: TODO | DO | SKIP
- Severity: Info
- Standard source: AGENTS.md; API_CONTRACT.md; `proto/buf.yaml`; Makefile; CI
- Location: `proto/buf.yaml`, `ci.yaml`, `make check-generated`
- Finding: `buf lint`, `breaking: FILE` (draft ignored), pinned buf/plugins, committed `gen/` + OpenAPI, and `check-generated` match written contract discipline.
- Recommendation: Keep as the model gate; optionally add a Makefile `proto-check` alias.
- Notes / user comments:
  -

### STD-005: `buf.validate` annotations exist but are not enforced on the request path
- [ ] Decision: TODO | DO | SKIP
- Severity: High
- Standard source: AGENTS.md; docs/API.md; de facto server code
- Location: `proto/ai_grid/v1/*_type.proto`; `internal/servers/*_server.go`
- Finding: Protos carry `buf.validate` rules and tests assert protovalidate agrees with hand checks, but production handlers reimplement validation and never call `protovalidate.Validate` / an interceptor.
- Recommendation: Add a shared validate helper (or Connect interceptor) for Create/Update inputs; keep only cross-resource checks in Go.
- Notes / user comments:
  -

### STD-007: No coverage threshold or coverage CI gate
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Standard source: AGENTS.md; de facto CI
- Location: `Makefile` `unit`; `ci.yaml`; `integration/`
- Finding: No `-cover` gate. Unit coverage under `internal/servers` is generally strong; Kind integration covers Ping/Clusters/Tenants only — not Models/Subscriptions.
- Recommendation: Document “no numeric coverage gate” or add a modest package cover gate; extend `kind-test`/smoke for Models/Subscriptions when catalog is in play.
- Notes / user comments:
  -

### STD-008: Kind integration and shellcheck are required in AGENTS.md but not CI-enforced
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Standard source: AGENTS.md; CI workflows
- Location: `integration/`, `hack/`, `.github/workflows/`
- Finding: CI never runs `kind-test` or shellcheck. Shell hygiene is human-process only.
- Recommendation: Add scheduled/labeled CI for `kind-test` if runners allow; add shellcheck or `bash -n` on PRs that touch `hack/**/*.sh`.
- Notes / user comments:
  -

### STD-009: Observability/logging docs vs implementation
- [ ] Decision: TODO | DO | SKIP
- Severity: Medium
- Standard source: AGENTS.md; docs/API.md §6/§9
- Location: `cmd/ai-grid-provider-backend/main.go`; absence of `internal/logging`, `internal/metrics`
- Finding: Production uses default `slog` (text). No Prometheus/metrics interceptor. Secret-redaction guidance is written but not mechanized.
- Recommendation: Normative docs should state current MVP (default slog, no metrics yet), or implement a small logging helper and demote API.md interceptor chain to “future/OSAC reference.”
- Notes / user comments:
  -

### STD-010: `.golangci.yml` vs `docs/API.md` linter inventory
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Standard source: `.golangci.yml`; docs/API.md; CI pin `v2.12.2`
- Location: `.golangci.yml`, `ci.yaml`
- Finding: Repo config is `default: standard` + a few extras. API.md still lists OSAC extras not enabled.
- Recommendation: Update API.md to mirror `.golangci.yml`.
- Notes / user comments:
  -

### STD-013: Table-driven / Connect-code test pattern is established
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Standard source: AGENTS.md; de facto `internal/servers/*_test.go`
- Location: `internal/servers/*_test.go`
- Finding: Strong pattern: colocated tests, `t.Parallel`, table `t.Run`, `assertConnectCode`. No `TODO`/`FIXME` debt found in `.go` sources.
- Recommendation: Codify “stdlib testing + table tests + Connect codes” in AGENTS.md / slim Testing section of API.md.
- Notes / user comments:
  -

### STD-015: Git hygiene (DCO / AI trailer) stated but not automated
- [ ] Decision: TODO | DO | SKIP
- Severity: Low
- Standard source: AGENTS.md §Git and review hygiene
- Location: `AGENTS.md` L276–282
- Finding: DCO sign-off and AI-assistance trailer required in prose; no workflow/hook in this repo snapshot.
- Recommendation: Add a PR check or document where org-level DCO is enforced; link the exact trailer format.
- Notes / user comments:
  -

### STD-016: Draft API isolation standard appears honored
- [ ] Decision: TODO | DO | SKIP
- Severity: Info
- Standard source: AGENTS.md §The console's draft API
- Location: `cmd/…`, `internal/servers`, `internal/restgateway`, `client`
- Finding: No `ai_grid/draft` imports in real backend paths; draft excluded from breaking.
- Recommendation: Keep a simple CI `rg` guard forbidding draft imports under those packages.
- Notes / user comments:
  -

### STD-017: `docs/draft/research/backend-conventions.md` is historical research, not current law
- [ ] Decision: TODO | DO | SKIP
- Severity: Info
- Standard source: docs/draft/research/backend-conventions.md; AGENTS.md draft docs map
- Location: `docs/draft/research/backend-conventions.md`
- Finding: Describes a two-commit early backend; dangerous if agents treat it as current standards.
- Recommendation: Banner at top: “Superseded by AGENTS.md / docs/API.md / API_CONTRACT.md; do not enforce.”
- Notes / user comments:
  -

### Standards posture (summary)
- Proto/codegen discipline is the strongest area; local `make verify` now mirrors CI Check (except PR-only `buf breaking`). AGENTS Validation points at `make verify` as the Go gate; full target catalog lives in `make help` / README.
- Normative docs are still split-brained on observability/linter inventory (`docs/API.md` still OSAC-shaped) — STD-001 / STD-009–010. Testing house style (stdlib) is clarified in API.md §10 while OSAC API shapes stay normative.
- Testing culture is good (Connect codes / typed errors for API tests; fakeinfra message cites-source is the documented exception, and so are the wire tests whose purpose is “the cause message survives to the caller” — they assert via `assertConnectMessage`, where a failed `errors.As` fails the test). Protovalidate-on-the-wire (STD-005) and Kind/shell/coverage gates remain aspirational.
- Image policy (beyond STD-018’s Buildx-vs-Podman docs note): intentional. Check runs on every branch push and every PR; image build/publish is main / `v*` / `workflow_dispatch` only. PR preview image workflows stay disabled.

---

## Cross-cutting themes (for prioritization)

1. **AuthN/AuthZ + network isolation** (SEC-001–003, SEC-006) block production trust.
2. **Docs vs reality** (STD-001, STD-009–010) — observability/linter sections of API.md still OSAC-shaped; API field/shape + test-framework honesty already addressed.
3. **`internal/servers` deepening** (SOLID-001–008) unlocks maintainability without changing the proto contract.
4. **Protovalidate on the wire** (STD-005) closes the remaining “written standard vs enforced” validation gap.
