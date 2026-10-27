# PR #27 Review Decision Tracker

| Field | Value |
| --- | --- |
| **PR** | https://github.com/ai-grid-cloud/ai-grid-provider-backend/pull/27 |
| **Branch** | `feat/models-and-subscriptions` |
| **Reviewed head** | `e852ba8` (round 1); re-check `9ff1f1b` (round 2) |
| **Date** | 2026-10-26 |
| **Purpose** | Decide yes / no / later per review finding; implement accepted items. |

## Legend

| Status | Meaning |
| --- | --- |
| `pending` | Awaiting decision |
| `accepted` | Accepted; not yet implemented |
| `skipped` | Declined for this PR |
| `later` | Deferred / follow-up |
| `done` | Implemented (or already landed) |

## Summary

| # | Priority | Title | Status | Notes |
| --- | --- | --- | --- | --- |
| 0 | — | cfchase golangci-lint fixes | done | Landed in `e852ba8` |
| 1 | — | Kind RBAC incomplete for Models | done | Kind ClusterRole aligned with OpenShift Models RBAC |
| 2 | — | Subscription Update: merge mask before validate | done | Merge stored+mask paths before validate; reject unsupported paths |
| 3 | — | List maps Forbidden→Internal | done | Typed K8s→Connect mapper on Models/Subscriptions List + coveringAuthPolicies |
| 4 | — | Models Update writes unmasked spec.main fields | done | Exact main FieldMask paths; env-only no longer clears image; empty main.image rejected |
| 5 | — | Metadata maps merge instead of replace | done | Replace semantics via null removals; Models preserve owned labels |
| 6 | — | Create compensation deletes preexisting Placement/MWRS | done | Compensate only UID-tracked creates from this call |
| 7 | — | replicas↔scaling explicit null | done | Update patch emits null for cleared mode; subscription optionals too |
| 8 | — | Subscription Delete swallows policy cleanup failure | done | Policy deleted first; cleanup errors fail Delete; orphan retry cleans managed policy |
| 9 | — | Reject negative Models List limit | done | Models + Subscriptions validate limit < 0 → InvalidArgument |
| 10 | — | Models Delete K8s ResourceVersion precondition | done | Preconditioned LLM delete before Placement/MWRS cleanup |
| 11 | — | Model phase NotAsExpected→FAILED too aggressive | done | NotAsExpected → PENDING; FAILED when a work is Degraded |
| 12 | — | Fill rollout.works (+ P13 fields) | done | works[] from ManifestWorks + RBAC; fuller P13 fields still optional follow-up |
| 13 | — | Locations (P7/P8) | done | ListLocations RPC + placement.locations enrichment |
| 14 | — | Subscription Delete resource_version / If-Match | done | Delete resource_version → K8s precondition; Conflict→Aborted |
| 15 | — | Agree models namespace | done | Default `ai-grid-models`; `AI_GRID_MODELS_NAMESPACE` override kept. Changed to `ai-grid-internal-models` by #38 |
| 16 | — | resourceVersionToVersion int32 overflow→0 unlocks | later | Deferred: fail-closed or wider type; shared helper + console string-version ADR |
| 17 | — | Fakeinfra duplicated resource-handler (P3) | done | Shared writeNamespacedUpdate + decodeTypedObject |
| 18 | — | Retire draft Models/Subscriptions surfaces | done | Draft protos, fake, fixtures, docs retired per workflow |
| 19 | high | Stale subscription delete removes policy first | done | Get+RV compare before policy delete; K8s precondition kept |
| 20 | high | Model delete can leave spoke MWRS | done | MWRS/Placement/ModelRef first (errors returned); orphan cleanup on NotFound |
| 21 | medium-high | Model update replaces containers list | done | Path-selective LLM patch; merge main into stored containers |
| 22 | medium | ManifestWorks wrong MWRS label value | done | Select `namespace.name`; filter placementname fallback by owner |
| 23 | medium | Applied=False kept Pending | done | `mwrsHasFailedOrDegradedWork` treats Applied=False as failure |
| 24 | medium | Nested subscription masks replace parent | done | Reject nested owner/model_refs/organization/token_metadata/guardrails |
| 25 | smaller | Clear replicas/scaling; tokenMetadata keys; poc fixtures | done | Null cleared modes; replace tokenMetadata keys; seed poc MaaS/LLM fixtures |
| 26 | follow-up | P13 per-work phase/reason/message/feedback + PARTIALLY_AVAILABLE | later | Console PR #66; offered as follow-up |

## How we use this

1. Parent walks items one-by-one and records a decision (`accepted` / `skipped` / `later`).
2. On **accept**, a subagent implements the change and marks the item `done` with implementation notes.
3. On **skip** or **later**, update Status and Notes only; no code change required in this PR.

---

## Detailed items

### 0. cfchase golangci-lint fixes — Status: `done`

- **Source:** cfchase (golangci-lint)
- **Description:** Fix lint issues: `errors.Is` in celfilter, dead client in `models_server_test`, unused `annotationCreatedAt`.
- **Pros:** Clean CI; already reviewed and merged.
- **Cons:** N/A
- **Decision:** Done — already landed in `e852ba8`.
- **Implementation notes:** Present on reviewed head `e852ba8`.

### 1. Kind RBAC incomplete for Models — Status: `done`

- **Source:** TheiaSurette [discussion_r4380863144](https://github.com/ai-grid-cloud/ai-grid-provider-backend/pull/27#discussion_r4380863144); DaoDaoNoCode review #7
- **Description:** Align Kind ClusterRole with OpenShift: grant access for `LLMInferenceService`, `Placement`, `PlacementDecision`, `MWRS`, and fuller `maasmodelrefs` so Kind can exercise Models CRUD.
- **Pros:** Kind exercises Models CRUD; matches AGENTS.md; avoids Forbidden→Internal.
- **Cons:** Needs CRDs in Kind; larger Kind YAML.
- **Decision:** Accepted — Kind SA needs Models CRUD parity with OpenShift.
- **Implementation notes:** Updated `deploy/kind/ai-grid-provider-backend.yaml` ClusterRole: `maasmodelrefs` now get/list/watch/create/update/patch/delete (was read-only); added `llminferenceservices` (serving.kserve.io), `placements`/`placementdecisions` (cluster.open-cluster-management.io), and `manifestworkreplicasets` (work.open-cluster-management.io) with full CRUD verbs matching `deploy/openshift/rbac.yaml`. Left MaaS subscription/authpolicy and managedcluster rules intact. Intentionally did not copy OpenShift-only `policies` (resourceNames: ai-grid-ready) rule — not Models-related.

### 2. Subscription Update: merge mask before validate — Status: `done`

- **Source:** Theia #2; DaoDao #4
- **Description:** Build `candidate = stored + exact mask paths`, validate that candidate, and reject unsupported paths.
- **Pros:** Matches docs/API.md; partial updates work; no overwrite of unmasked fields.
- **Cons:** Update rewrite + tests; must enumerate supported paths.
- **Decision:** Accepted — validate the merged object per docs/API.md.
- **Implementation notes:** `subscriptionUpdatePaths` now rejects unsupported paths and tracks exact spec fields (`spec.priority`, `spec.owner`, …). Update clones the stored subscription, applies only selected mask paths, validates that candidate (plus org immutability), and merge-patches only those fields. AuthPolicy sync uses the candidate. Tests cover priority-only success, unmasked owner/model_refs preserved, and unsupported path → InvalidArgument.

### 3. List maps Forbidden→Internal — Status: `done`

- **Source:** Theia #3
- **Description:** Use typed K8s→Connect error mappers on Models/Subscriptions List (and coveringAuthPolicies) so Forbidden is not collapsed to Internal.
- **Pros:** Matches AGENTS.md; clients can distinguish auth failures vs bugs.
- **Cons:** Small change but easy to miss a call path.
- **Decision:** Accepted — List/auth-policy join must not collapse Forbidden to Internal.
- **Implementation notes:** Models List uses `mapModelAPIError`; Subscriptions List and `coveringAuthPolicies` use `mapSubscriptionAPIError` (NotFound still returns empty). Tests assert Forbidden → PermissionDenied for Models List, Subscriptions List, and Get covering policies.

### 4. Models Update writes unmasked spec.main fields — Status: `done`

- **Source:** Theia #4; DaoDao #3
- **Description:** Apply only exact FieldMask paths on Models Update; possibly bring PR #31.
- **Pros:** Fixes console BFF empty-container bug; honors FieldMask contract.
- **Cons:** Careful mask/patch logic; regression tests; cherry-pick conflicts possible.
- **Decision:** Accepted — apply only FieldMask-selected main fields.
- **Implementation notes:** Expanded `modelUpdatePaths` with exact LLM/main subfields (`spec.main.image`, `spec.main.env`, …). `mergeModelUpdate` applies only selected fields so an env-only patch keeps the stored image. `validateModelSpec` rejects empty/whitespace-only `spec.main.image` when `spec.main` is set. Regression tests: `TestModelsUpdateEnvOnlyPreservesImage`, empty-image Create → InvalidArgument.

### 5. Metadata maps merge instead of replace — Status: `done`

- **Source:** Theia #5
- **Description:** Use whole-map replace (null removals / JSON Patch) for labels and annotations on Models and Subscriptions, instead of merge-only behavior.
- **Pros:** Empty `{}` clears labels; matches contract.
- **Cons:** Must preserve backend-owned labels; Merge Patch alone is insufficient.
- **Decision:** Accepted — whole-map replace with owned-label preservation.
- **Implementation notes:** Added `replaceStringMapPatch` (nulls for removed keys). Models Update preserves `ai-grid/model` + `app.kubernetes.io/managed-by`. Subscriptions Update uses the same helper for labels/annotations. Tests cover `{}` clearing `remove-me`.

### 6. Create compensation deletes preexisting Placement/MWRS — Status: `done`

- **Source:** Theia #6; DaoDao #2
- **Description:** Track UIDs of objects created during Create; compensate (delete) only those UIDs on failure—not preexisting GitOps objects.
- **Pros:** Avoids deleting GitOps-managed objects; correct AlreadyExists handling.
- **Cons:** Create bookkeeping; UID-preconditioned deletes.
- **Decision:** Accepted — compensate only objects this Create call made.
- **Implementation notes:** Create appends `createdObjectRef{gvr,name,uid}` after each successful create; `compensateCreate` deletes those refs in reverse with UID preconditions. Preexisting Placement on AlreadyExists is left alone (test `TestModelsCreatePlacementAlreadyExistsKeepsPreexisting`).

### 7. replicas↔scaling explicit null — Status: `done`

- **Source:** Theia #7; DaoDao #5
- **Description:** Emit explicit null for removed mutually exclusive fields (replicas vs scaling), including optional Subscription clears.
- **Pros:** CR will not keep both; matches ModelSpec contract.
- **Cons:** Update-specific patch logic.
- **Decision:** Accepted — explicit nulls on Update merge patch.
- **Implementation notes:** `modelSpecUpdatePatch` nulls `replicas` when switching to scaling and `scaling` when switching to replicas. Subscriptions whole-spec updates use `subscriptionSpecUpdateMap` to null cleared `tokenMetadata` / `inferencePriority` / `guardrails`. Test covers both mode transitions.

### 8. Subscription Delete swallows policy cleanup failure — Status: `done`

- **Source:** Theia #8; DaoDao #1
- **Description:** Fail Delete (or make cleanup retryable) when AuthPolicy cleanup fails; do not succeed with a leaked AuthPolicy.
- **Pros:** Security/correctness; UI contract.
- **Cons:** Retry/ordering change; must not delete unmanaged policies.
- **Decision:** Accepted — fail Delete when managed policy get/delete fails.
- **Implementation notes:** Managed AuthPolicy is deleted **before** the subscription so a cleanup failure leaves the subscription for retry. Get/delete errors map via `mapSubscriptionAPIError`. Unmanaged same-named policies remain untouched. A missing subscription still attempts managed-policy cleanup (orphaned-policy retry), then returns NotFound. Tests cover Internal-on-policy-failure (subscription remains), delete order, and orphaned managed policy cleanup.

### 9. Reject negative Models List limit — Status: `done`

- **Source:** Theia #9
- **Description:** Return InvalidArgument when `limit < 0`, with a test; also check Subscriptions List.
- **Pros:** Matches proto; cheap fix.
- **Cons:** Should also cover Subscriptions List for consistency.
- **Decision:** Accepted — honor proto contract on Models and Subscriptions.
- **Implementation notes:** `validateListLimit` rejects `limit < 0` before paging on Models and Subscriptions List. Tests cover `limit=-1`.

### 10. Models Delete K8s ResourceVersion precondition — Status: `done`

- **Source:** Theia #10
- **Description:** Pass `Preconditions{ResourceVersion}` on Models Delete; map conflict→Aborted; do not cleanup Placement/MWRS before successful delete.
- **Pros:** Real optimistic concurrency; closes TOCTOU.
- **Cons:** Must reorder Delete carefully.
- **Decision:** Accepted — atomic preconditioned LLM delete first.
- **Implementation notes:** Nonzero `resource_version` sets `DeleteOptions.Preconditions.ResourceVersion` on LLMInferenceService delete; Conflict maps to Aborted via `mapModelAPIError`. Placement/MWRS/ModelRef cleanup runs only after successful LLM delete (best-effort warn on cleanup errors). Test asserts options include RV and conflict → Aborted without removing the model.

### 11. Model phase NotAsExpected→FAILED too aggressive — Status: `done`

- **Source:** DaoDao #6
- **Description:** Treat NotAsExpected as PENDING unless Degraded/failed apply (draft rule), instead of mapping straight to FAILED.
- **Pros:** Avoids false Failed flash; better console expectations.
- **Cons:** Mapping tests needed.
- **Decision:** Accepted — align with draft ModelPhase rule.
- **Implementation notes:** `enrichModelFromMWRS` maps NotAsExpected to PENDING. FAILED only when `mwrsHasFailedOrDegradedWork` sees a work with Degraded=True (populated once works[] is filled). Tests cover NotAsExpected→PENDING and degraded helper.

### 12. Fill rollout.works (+ P13 fields) — Status: `done`

- **Source:** DaoDao #9
- **Description:** Populate per-cluster work status in `rollout.works` (and related P13 fields); requires ManifestWorks RBAC.
- **Pros:** Needed for console Models page.
- **Cons:** Larger scope; OK as follow-up.
- **Decision:** Accepted — usable works[] for console; remaining P13 field expansions optional.
- **Implementation notes:** `enrichManifestWorks` lists ManifestWorks by MWRS/placement labels and fills `ModelWorkStatus` (cluster, applied/available/progressing/degraded). Kind+OpenShift RBAC grants get/list/watch on `manifestworks`. Phase re-derived after works so Degraded→FAILED. Remaining draft P13 extras (per-work conditions/feedback/PARTIALLY_AVAILABLE) not added to v1 proto in this pass.

### 13. Locations (P7/P8) — Status: `done`

- **Source:** DaoDao #10
- **Description:** Support placement location labels and `ListLocations`.
- **Pros:** Console Locations / wizard.
- **Cons:** New RPC; easy follow-up.
- **Decision:** Accepted — ship P7/P8 on v1.
- **Implementation notes:** Added `Models.ListLocations` + `ModelPlacementStatus.locations`. ListLocations returns unique sorted `location` labels of clusters with ai-grid/enabled+provisioned and Available. Get/List enrich placement.locations from decided clusters. Client helper `ListModelLocations`; UI docs updated.

### 14. Subscription Delete resource_version / If-Match — Status: `done`

- **Source:** DaoDao #11
- **Description:** Add resource_version / If-Match on Subscription Delete, consistent with Models/Tenants.
- **Pros:** Console If-Match; API consistency.
- **Cons:** Proto + server + OpenAPI; potential client break.
- **Decision:** Accepted — match Models Delete version pattern.
- **Implementation notes:** `SubscriptionsDeleteRequest.resource_version`; nonzero sets K8s Delete Preconditions; Conflict→Aborted via mapper. Generated OpenAPI/connect stubs included. Test asserts RV in DeleteOptions.

### 15. Agree models namespace — Status: `done`

- **Source:** DaoDao #12
- **Description:** Pick one default / env alignment between `ai-grid-internal-models` and `ai-grid-models` with console.
- **Pros:** Delete guard and refs work correctly.
- **Cons:** Cross-repo; renaming default can surprise deploys.
- **Decision:** Accepted — default `ai-grid-models` to match console; keep env override.
- **Implementation notes:** Changed `defaultModelsNamespace`, main.go getEnv default, OpenShift deploy env, docs, fixtures, and tests to `ai-grid-models`. `AI_GRID_MODELS_NAMESPACE` still overrides.
- **Superseded by #38:** the default is now `ai-grid-internal-models`. In git mode this variable is the spokes' namespace for each model's `LLMInferenceService`, not the console's: the console lists models in the subscription namespace (`models-as-a-service`). The AI Grid reference environment (`kahowell/ai-grid-tenant`) creates `ai-grid-internal-models` on its spokes and deploys its models there, and our clusters use it too.

### 16. resourceVersionToVersion int32 overflow→0 unlocks — Status: `later`

- **Source:** DaoDao #8
- **Description:** Fail closed (or use a wider type) when resourceVersion→int32 overflows instead of returning 0 (which unlocks).
- **Pros:** Long-lived hubs stay locked correctly.
- **Cons:** Shared helper change; broader API version discussion.
- **Decision:** `later`
- **Notes / TODO:**
  - Deferred: fail-closed or wider type for `resourceVersionToVersion` when value does not fit int32 (currently returns 0 and unlocks).
  - Touches shared helper (Clusters too); may need broader API version discussion with console string-version ADR.
  - Revisit after merge-blocking P1/P2 items land.
- **Implementation notes:**

### 17. Fakeinfra duplicated resource-handler (P3) — Status: `done`

- **Source:** Theia P3
- **Description:** Share decode→lock→validate→commit path in hub fake instead of duplicating resource-handler logic.
- **Pros:** Less drift between fakes.
- **Cons:** Refactor-only; defer OK.
- **Decision:** Accepted — dedupe Models NS and MaaS update paths.
- **Implementation notes:** Extracted `writeNamespacedUpdate` + `decodeTypedObject` in `hub/resource_update.go`; `writeNSUpdate`/`writeMAASUpdate` and `decodeNSObject`/`decodeMAASObject` are thin wrappers. Behavior unchanged; `go test ./internal/fakeinfra/...` green.

### 18. Retire draft Models/Subscriptions surfaces — Status: `done`

- **Source:** Theia workflow note
- **Description:** Delete draft Models/Subscriptions area now, or explicitly link a follow-up.
- **Pros:** Advances draft retirement workflow.
- **Cons:** Bigger cleanup; next PR OK.
- **Decision:** Accepted — complete retirement now (not a follow-up link).
- **Implementation notes:** Removed draft `model_*.proto` / `subscription_*.proto` (+ gen), fake handlers (`models.go`, `subscriptions.go`, `kserve.go`, `placement.go`, `work.go`, `subscription/`), fixtures (`models.json`, `subscriptions.json`, `presets.json` in default+poc), `areas/models.md`, and ListLocations from the draft fake (P7 lives on `ai_grid.v1.Models`). Kept Grid, CatalogItems stubs, Clusters, Tenants, GetHubCapabilities. Updated registration, intentionallyUnimplemented expectations, validation FrontendOnly, draft docs, and tests. Shared helpers (`cTrue`/`ClusterSetDefault`, `newRepoServer`, GetHubCapabilities) moved out of deleted files.

## Round 2 (re-check at `9ff1f1b`)

### 19. Stale subscription delete removes policy first — Status: `done`

- **Source:** DaoDao re-review (high)
- **Description:** Compare subscription resourceVersion before deleting the managed AuthPolicy so a stale delete changes nothing.
- **Decision:** Accepted.
- **Implementation notes:** Get subscription first; when request RV is nonzero and live RV is non-empty and mismatches → Aborted before policy delete. K8s Delete precondition retained. Orphan policy cleanup on NotFound unchanged. Test seeds RV and asserts policy remains on stale delete.

### 20. Model delete can leave spoke MWRS — Status: `done`

- **Source:** DaoDao re-review (high)
- **Description:** Delete MWRS/Placement before LLM (return errors); allow retry cleanup when the service is already gone.
- **Decision:** Accepted.
- **Implementation notes:** `deleteModelSideEffects` removes MWRS → Placement → ModelRef (errors returned). Stale RV checked before side-effect deletes. Missing LLM still cleans side effects then NotFound. Tests cover stale RV leaving MWRS.

### 21. Model update replaces containers list — Status: `done`

- **Source:** DaoDao re-review (medium-high)
- **Description:** Do not rewrite `spec.template.containers` on unrelated masks; when main paths are selected, merge into the stored containers list.
- **Decision:** Accepted.
- **Implementation notes:** `modelSpecUpdatePatch` is path-selective; `mergeMainContainerIntoStored` updates the `main` container in place and keeps sidecars / unmapped fields. Test `TestModelsUpdateReplicasOnlyPreservesSidecar`.

### 22. ManifestWorks wrong MWRS label value — Status: `done`

- **Source:** DaoDao re-review (medium)
- **Description:** Select works with `<namespace>.<name>` owner label; do not use bare placement name alone.
- **Decision:** Accepted.
- **Implementation notes:** Primary selector `manifestworkreplicaset=<ns>.<pname>`; placementname fallback filters out works with a different owner label. (Slash owner values are not valid K8s label values, so not selected.)

### 23. Applied=False kept Pending — Status: `done`

- **Source:** DaoDao re-review (medium)
- **Description:** Treat Applied=False (without Degraded) as failure.
- **Decision:** Accepted.
- **Implementation notes:** `mwrsHasFailedOrDegradedWork` returns true for Degraded=True or Applied=False.

### 24. Nested subscription masks replace parent — Status: `done`

- **Source:** DaoDao re-review (medium)
- **Description:** Reject nested `spec.owner.*` / `model_refs.*` / `organization.*` (and token_metadata/guardrails) with InvalidArgument.
- **Decision:** Accepted (reject nested rather than deep-merge).
- **Implementation notes:** Exact paths only (`spec.owner`, `spec.model_refs`, …). Test covers `spec.owner.groups` → InvalidArgument.

### 25. Clear replicas/scaling; tokenMetadata keys; poc fixtures — Status: `done`

- **Source:** DaoDao re-review (smaller)
- **Description:** Emit nulls when clearing a mode; replace tokenMetadata keys (incl. labels); restore Models/Subscriptions seeds for `dev-fake-poc`.
- **Decision:** Accepted.
- **Implementation notes:** Path-selective patch nulls cleared replicas/scaling; `replaceTokenMetadataPatch` nulls removed keys/labels; copied default MaaS/LLM fixtures into `fixtures/poc/` and updated README.

### 26. P13 per-work phase/reason/message/feedback + PARTIALLY_AVAILABLE — Status: `later`

- **Source:** DaoDao re-review / console PR #66
- **Description:** Fuller `ModelWorkStatus` fields and `MODEL_PHASE_PARTIALLY_AVAILABLE`.
- **Decision:** `later` (follow-up as offered).
