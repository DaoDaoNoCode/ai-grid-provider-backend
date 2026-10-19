# Draft fake fixtures

What the draft fake (`internal/fakeinfra/draft`, served by
`cmd/ai-grid-fake-infra --draft-address`, started by `make dev-fake` and
`make dev-fake-poc`) starts with: the console's fixtures for its draft API,
`ai_grid.draft.v1` (`proto/ai_grid/draft/v1`, `docs/draft/README.md`). Every
name, id, host and number in these files is fictional.

## Source

Copied verbatim from the console repository
[ai-grid-cloud/ai-grid-provider-console](https://github.com/ai-grid-cloud/ai-grid-provider-console)
at commit `ac80c25`. They are protojson of the draft proto (the console's
format), not converted like `fixtures/default` and `fixtures/poc`; only the
two READMEs moved to `docs/draft/fixtures/`.

| Directory here | Console directory | Preset | Described in |
|---|---|---|---|
| `default/` | `mock/fixtures/` | `make dev-fake` | [`docs/draft/fixtures/default.md`](../../docs/draft/fixtures/default.md) |
| `poc/` | `mock/fixtures-poc/` | `make dev-fake-poc` | [`docs/draft/fixtures/poc.md`](../../docs/draft/fixtures/poc.md) |

## Duplication with fixtures/default and fixtures/poc

`fixtures/default` and `fixtures/poc` are converted from the console's
fixtures too (at `537df38`; see `fixtures/README.md`): the clusters, the
organizations, the access tokens and the cluster scenarios are in both places.
The draft fake reads only these files and keeps its own state; it does not
read the fake hub's clusters or the fake Keycloak's organizations, so a change
made through the real backend's API is not seen through the draft API, and the
other way round. Wiring the draft fake to the fake hub and the fake Keycloak is
a later step.

The duplication shrinks area by area: when the real backend implements an
area, that area's draft proto, fake code and fixture file are deleted
(`docs/draft/README.md`, "When an area moves to the real backend").

The tests in `internal/fakeinfra/draft` load both directories
(`TestRepoFixturesLoad`, `preset_test.go`) and check them against the hub and
Keycloak shapes the research verified, so edit them with those tests running.
