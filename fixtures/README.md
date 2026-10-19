# Fake infrastructure fixtures

What the fake hub and the fake Keycloak (`internal/fakeinfra`, started by
`make dev-fake` and `make dev-fake-poc`) start with. Every name, id, host and
number in these files is fictional.

## Source

Converted from the console repository
[ai-grid-cloud/ai-grid-provider-console](https://github.com/ai-grid-cloud/ai-grid-provider-console)
at commit `537df38`:

| Directory here | Console directory | Preset |
|---|---|---|
| `default/` | `mock/fixtures/` | Ten clusters in every state the ACM console distinguishes, six organizations, a timeline of cluster events. |
| `poc/` | `mock/fixtures-poc/` | The PoC's four sites (`site1`-`site4`) and three organizations; no timeline, so nothing changes over time. |

The console's fixtures are protojson of its draft `ai_grid.v1` proto, which
mirrors the hub's `ManagedCluster` and Keycloak's Organization field by
field. The files here are the real objects again, so the real backend reads
them through the real Kubernetes and Keycloak clients.

## Files

| File | Read by | Shape | From the console's |
|---|---|---|---|
| `managedclusters.json` | fake hub | A `ManagedClusterList` (`cluster.open-cluster-management.io/v1`) of full CRs: `metadata` (name, uid, resourceVersion, generation, creationTimestamp, deletionTimestamp, labels, annotations, finalizers), `spec` (managedClusterClientConfigs, hubAcceptsClient, leaseDurationSeconds, taints), `status` (conditions, capacity, allocatable, version, clusterClaims). | `managedclusters.json` |
| `organizations.json` | fake Keycloak | A JSON array of Keycloak 26.8 `OrganizationRepresentation`, as `GET /admin/realms/ai-grid/organizations?briefRepresentation=false` returns them: id, name, alias, enabled, description, attributes (Keycloak's map of string arrays), domains (`name`, `verified`, `autoRedirect`). | `tenants.json` |
| `access.json` | fake hub | Which bearer tokens the hub rejects (401), denies (SelfSubjectAccessReview `allowed: false`, 403 on managedclusters) or allows, plus the backend's ServiceAccount token that `bin/fake-kubeconfig` carries. | `access.json` |
| `scenarios.json` | fake hub | Timed events that the hub controllers apply to the CRs (joined, imported, add-ons available, lease lost or recovered, API server down or up, detaching, detached). | `scenarios.json` |
| `settings.json` | fake hub | `speed` of the scenario clock; optional `detachAfter`. | `settings.json` |

Every file is decoded strictly: a misspelled key stops the fake with an error.
A missing file leaves its part empty or at its default (no `access.json`
allows every token).

## How the conversion maps fields

- **ManagedCluster.** Field names stay; the draft's flat fields go back to
  `metadata`, `spec` and `status` (`kubernetesVersion` becomes
  `status.version.kubernetes`). `generation` becomes a number again. A
  condition's `CONDITION_STATUS_TRUE`/`FALSE` becomes `"True"`/`"False"`, and
  a condition without a status (the draft's UNSPECIFIED) becomes `"Unknown"`.
  A cluster with a `deletionTimestamp` also gets
  `deletionGracePeriodSeconds: 0`, which the API server sets when it marks an
  object with finalizers deleting and which the draft has no field for.
- **Organization.** `domainDetails` become `domains` (with `verified` and
  `autoRedirect`), each attribute value becomes a one-element array, an
  omitted `alias` is the name and an omitted `enabled` is `true` (Keycloak's
  defaults), an organization without domains has no `domains` key. Three
  fields of the console's tenants are its proposals for the backend, not
  Keycloak data, and are dropped: `identityProvider` (P1, an organization
  identity provider), `resourceVersion` (P2) and `createTime` (P3).
- **access.json** gains `serviceAccount`.
- **scenarios.json**: the `spoke` of a `joined` event is written in the CR's
  status shape.
- **settings.json** keeps `speed` only; `latency` and the hub capabilities are
  settings of the console's fake gRPC backend, which the real backend replaces.

The console's files not listed above (models, presets, subscriptions) are for
APIs the backend does not have yet.

## Regenerating

The converter is `hack/convert-console-fixtures`. From this repository's root,
with the console checked out next to it:

```sh
mkdir -p /tmp/console-537df38
git -C ../ai-grid-provider-console archive 537df38 mock/fixtures mock/fixtures-poc | tar -x -C /tmp/console-537df38
go run ./hack/convert-console-fixtures --src /tmp/console-537df38/mock/fixtures --out fixtures/default
go run ./hack/convert-console-fixtures --src /tmp/console-537df38/mock/fixtures-poc --out fixtures/poc
go test ./internal/fakeinfra/...
```

The tests load both directories into the fakes, and check that every cluster
is one the hub controllers have already settled.
