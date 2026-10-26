# PR #27 Review Decision Tracker

| Field | Value |
| --- | --- |
| **PR** | https://github.com/ai-grid-cloud/ai-grid-provider-backend/pull/27 |
| **Branch** | `feat/models-and-subscriptions` |
| **Reviewed head** | `e852ba8` |
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
| 2 | — | Subscription Update: merge mask before validate | pending | |
| 3 | — | List maps Forbidden→Internal | pending | |
| 4 | — | Models Update writes unmasked spec.main fields | pending | |
| 5 | — | Metadata maps merge instead of replace | pending | |
| 6 | — | Create compensation deletes preexisting Placement/MWRS | pending | |
| 7 | — | replicas↔scaling explicit null | pending | |
| 8 | — | Subscription Delete swallows policy cleanup failure | pending | |
| 9 | — | Reject negative Models List limit | pending | |
| 10 | — | Models Delete K8s ResourceVersion precondition | pending | |
| 11 | — | Model phase NotAsExpected→FAILED too aggressive | pending | |
| 12 | — | Fill rollout.works (+ P13 fields) | pending | |
| 13 | — | Locations (P7/P8) | pending | |
| 14 | — | Subscription Delete resource_version / If-Match | pending | |
| 15 | — | Agree models namespace | pending | |
| 16 | — | resourceVersionToVersion int32 overflow→0 unlocks | pending | |
| 17 | — | Fakeinfra duplicated resource-handler (P3) | pending | |
| 18 | — | Retire draft Models/Subscriptions surfaces | pending | |

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

### 2. Subscription Update: merge mask before validate — Status: `pending`

- **Source:** Theia #2; DaoDao #4
- **Description:** Build `candidate = stored + exact mask paths`, validate that candidate, and reject unsupported paths.
- **Pros:** Matches docs/API.md; partial updates work; no overwrite of unmasked fields.
- **Cons:** Update rewrite + tests; must enumerate supported paths.
- **Decision:**
- **Implementation notes:**

### 3. List maps Forbidden→Internal — Status: `pending`

- **Source:** Theia #3
- **Description:** Use typed K8s→Connect error mappers on Models/Subscriptions List (and coveringAuthPolicies) so Forbidden is not collapsed to Internal.
- **Pros:** Matches AGENTS.md; clients can distinguish auth failures vs bugs.
- **Cons:** Small change but easy to miss a call path.
- **Decision:**
- **Implementation notes:**

### 4. Models Update writes unmasked spec.main fields — Status: `pending`

- **Source:** Theia #4; DaoDao #3
- **Description:** Apply only exact FieldMask paths on Models Update; possibly bring PR #31.
- **Pros:** Fixes console BFF empty-container bug; honors FieldMask contract.
- **Cons:** Careful mask/patch logic; regression tests; cherry-pick conflicts possible.
- **Decision:**
- **Implementation notes:**

### 5. Metadata maps merge instead of replace — Status: `pending`

- **Source:** Theia #5
- **Description:** Use whole-map replace (null removals / JSON Patch) for labels and annotations on Models and Subscriptions, instead of merge-only behavior.
- **Pros:** Empty `{}` clears labels; matches contract.
- **Cons:** Must preserve backend-owned labels; Merge Patch alone is insufficient.
- **Decision:**
- **Implementation notes:**

### 6. Create compensation deletes preexisting Placement/MWRS — Status: `pending`

- **Source:** Theia #6; DaoDao #2
- **Description:** Track UIDs of objects created during Create; compensate (delete) only those UIDs on failure—not preexisting GitOps objects.
- **Pros:** Avoids deleting GitOps-managed objects; correct AlreadyExists handling.
- **Cons:** Create bookkeeping; UID-preconditioned deletes.
- **Decision:**
- **Implementation notes:**

### 7. replicas↔scaling explicit null — Status: `pending`

- **Source:** Theia #7; DaoDao #5
- **Description:** Emit explicit null for removed mutually exclusive fields (replicas vs scaling), including optional Subscription clears.
- **Pros:** CR will not keep both; matches ModelSpec contract.
- **Cons:** Update-specific patch logic.
- **Decision:**
- **Implementation notes:**

### 8. Subscription Delete swallows policy cleanup failure — Status: `pending`

- **Source:** Theia #8; DaoDao #1
- **Description:** Fail Delete (or make cleanup retryable) when AuthPolicy cleanup fails; do not succeed with a leaked AuthPolicy.
- **Pros:** Security/correctness; UI contract.
- **Cons:** Retry/ordering change; must not delete unmanaged policies.
- **Decision:**
- **Implementation notes:**

### 9. Reject negative Models List limit — Status: `pending`

- **Source:** Theia #9
- **Description:** Return InvalidArgument when `limit < 0`, with a test; also check Subscriptions List.
- **Pros:** Matches proto; cheap fix.
- **Cons:** Should also cover Subscriptions List for consistency.
- **Decision:**
- **Implementation notes:**

### 10. Models Delete K8s ResourceVersion precondition — Status: `pending`

- **Source:** Theia #10
- **Description:** Pass `Preconditions{ResourceVersion}` on Models Delete; map conflict→Aborted; do not cleanup Placement/MWRS before successful delete.
- **Pros:** Real optimistic concurrency; closes TOCTOU.
- **Cons:** Must reorder Delete carefully.
- **Decision:**
- **Implementation notes:**

### 11. Model phase NotAsExpected→FAILED too aggressive — Status: `pending`

- **Source:** DaoDao #6
- **Description:** Treat NotAsExpected as PENDING unless Degraded/failed apply (draft rule), instead of mapping straight to FAILED.
- **Pros:** Avoids false Failed flash; better console expectations.
- **Cons:** Mapping tests needed.
- **Decision:**
- **Implementation notes:**

### 12. Fill rollout.works (+ P13 fields) — Status: `pending`

- **Source:** DaoDao #9
- **Description:** Populate per-cluster work status in `rollout.works` (and related P13 fields); requires ManifestWorks RBAC.
- **Pros:** Needed for console Models page.
- **Cons:** Larger scope; OK as follow-up.
- **Decision:**
- **Implementation notes:**

### 13. Locations (P7/P8) — Status: `pending`

- **Source:** DaoDao #10
- **Description:** Support placement location labels and `ListLocations`.
- **Pros:** Console Locations / wizard.
- **Cons:** New RPC; easy follow-up.
- **Decision:**
- **Implementation notes:**

### 14. Subscription Delete resource_version / If-Match — Status: `pending`

- **Source:** DaoDao #11
- **Description:** Add resource_version / If-Match on Subscription Delete, consistent with Models/Tenants.
- **Pros:** Console If-Match; API consistency.
- **Cons:** Proto + server + OpenAPI; potential client break.
- **Decision:**
- **Implementation notes:**

### 15. Agree models namespace — Status: `pending`

- **Source:** DaoDao #12
- **Description:** Pick one default / env alignment between `ai-grid-internal-models` and `ai-grid-models` with console.
- **Pros:** Delete guard and refs work correctly.
- **Cons:** Cross-repo; renaming default can surprise deploys.
- **Decision:**
- **Implementation notes:**

### 16. resourceVersionToVersion int32 overflow→0 unlocks — Status: `pending`

- **Source:** DaoDao #8
- **Description:** Fail closed (or use a wider type) when resourceVersion→int32 overflows instead of returning 0 (which unlocks).
- **Pros:** Long-lived hubs stay locked correctly.
- **Cons:** Shared helper change; broader API version discussion.
- **Decision:**
- **Implementation notes:**

### 17. Fakeinfra duplicated resource-handler (P3) — Status: `pending`

- **Source:** Theia P3
- **Description:** Share decode→lock→validate→commit path in hub fake instead of duplicating resource-handler logic.
- **Pros:** Less drift between fakes.
- **Cons:** Refactor-only; defer OK.
- **Decision:**
- **Implementation notes:**

### 18. Retire draft Models/Subscriptions surfaces — Status: `pending`

- **Source:** Theia workflow note
- **Description:** Delete draft Models/Subscriptions area now, or explicitly link a follow-up.
- **Pros:** Advances draft retirement workflow.
- **Cons:** Bigger cleanup; next PR OK.
- **Decision:**
- **Implementation notes:**
