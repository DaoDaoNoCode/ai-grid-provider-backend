# Draft fixtures: fixtures/draft/poc (PoC preset)

The data the PoC (`krisv/ai-grid-cloud-provider-poc` at `62319b8`) shows for clusters and
tenants, in the draft fake's fixture format. This page was the console's
`mock/fixtures-poc/README.md` at `ac80c25`. `make dev-fake-poc` loads `fixtures/draft/poc/`
(`--draft-fixtures`); `make dev-fake` loads PR 12's fixtures in `fixtures/draft/default/`.
The file shapes are the ones `docs/draft/fixtures/default.md` describes.

Models and Subscriptions are no longer draft fixtures: they ship in `ai_grid.v1` against the
fake hub (`fixtures/poc/` / `fixtures/default/`).

| File | What it seeds |
|---|---|
| `managedclusters.json` | The PoC's `seedClusters()` (`shared/grid-clusters.mjs`): `site1` San Francisco (`us-west`), `site2` New York (`us-east`), and `site3` Brussels (`europe-west`), AI Grid members and provisioned; `site4` Sydney (`asia-pacific`), not enrolled (no `ai-grid/enabled` label) and `ai-grid/provisioned: "false"`, the PoC's literal value. The `location` label is the cluster name. Everything else is copied from `fixtures/draft/default` `site-paris-1`, so all four read ready. Enrolling `site4` makes it a member that is still provisioning. |
| `tenants.json` | The three organizations the UAT seeds into the PoC: `acme-robotics`, `globex-research`, and `initech-labs`, with their domains and identity providers, a creation time, and version 1. |
| `settings.json` | No latency, speed 1, ManifestWorkReplicaSet gate on. |
| `access.json` | Copy of `fixtures/draft/default`. |

There's no `scenarios.json`, so nothing changes over time.

`internal/fakeinfra/draft/helpers_test.go` checks this directory against the PoC's clusters and
tenants and runs PR 12's fixture checks on it, and `TestDemoMetricsMatchThePoC` checks that
its members are the input of `internal/fakeinfra/draft/testdata/demo-metrics.json`. All names,
ids, and hosts are fictional.
