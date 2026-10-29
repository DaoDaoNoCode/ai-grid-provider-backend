# The API contract: how it is consumed, checked and changed

`proto/ai_grid/v1` is the only description of this service's API. Every client is
generated from it or checked against it, so a change to it is a change to every client.
This page says who depends on it, what CI checks, which changes are safe, and the
procedure for the ones that are not. [`API.md`](API.md) covers how to design an API;
this page covers how to change one that already has clients.

## Who consumes the contract

| Client | How it gets the types | Protocol |
| --- | --- | --- |
| The provider console (ai-grid-cloud/ai-grid-provider-console) | Generates TypeScript (protobuf-es v2) from this repository's `proto/` at a pinned commit, the one in its `.backend-api-version`, with buf's git input | Connect (JSON over HTTP) from the browser, through the console's BFF, which relays the request without reading it |
| The Go client (`client/`), integration tests, `hack/smoke-apis.sh` | `gen/ai_grid/v1` in this repository | gRPC |
| curl, CLIs, GitOps tooling, Swagger UI | `docs/openapi`, generated from the same proto | REST, through vanguard |

The console's move to generated clients is in progress: today its BFF still calls this
service with grpc-go and maps the answers to its own JSON. The direction (the console keeps
no copy of the contract and no fake of its own) is recorded in the console's ADRs 0001–0004.

## One handler, two doors

`cmd/ai-grid-provider-backend` registers every service on one `connect.Server`.

- `connecthttp.Mount` serves it as **Connect, gRPC and gRPC-Web**:
  `POST /ai_grid.v1.Clusters/List`. The console and the Go client use this door.
- `restgateway.Mount` (vanguard) serves the same handlers as **REST**, from each RPC's
  `google.api.http` annotation: `GET /api/v1/clusters`. Tools that don't speak Connect use
  this door.

Both doors run the same handlers and interceptors, so validation, errors and behaviour
are identical. A REST caller and the console can never see two different implementations
of one RPC.

## What CI checks

| Check | Command | What it catches |
| --- | --- | --- |
| Lint | `buf lint proto` | Style rules (`STANDARD`, with the exceptions in `proto/buf.yaml`) |
| Breaking changes | `buf breaking proto --against <base branch>` (pull requests only) | Anything that would break an existing client, at the strictest level (`FILE`): a removed, renumbered, renamed or retyped field; a removed or renamed RPC, message or enum value; a moved message; a changed JSON name |
| Generated files | `make check-generated` | Generated code that doesn't match the proto: stale, missing or left over |

`ai_grid/draft` is excluded from the breaking check (`breaking.ignore` in
`proto/buf.yaml`): it's the console's proposal, not this service's API, and it changes or
disappears area by area.

Two things the breaking check can't see:

- **A changed meaning.** If a field keeps its name and type but starts to mean something
  else (a count of physical GPUs becomes a count of MIG slices), nothing fails. Treat it as
  a removal plus an addition: add a new field with a new name and retire the old one with
  the procedure below.
- **A changed REST path.** `buf breaking` doesn't compare the values of
  `google.api.http` annotations. A changed path shows only in the diff of
  `docs/openapi/ai_grid.swagger.json`; review that diff as the REST contract.

## Changes that are always safe

These pass the breaking check and need no coordination. Clients built against the old
contract keep working: connect-go and connect-es both skip JSON fields they don't know, and
binary clients skip unknown field numbers.

- A new field, with a number never used before in that message.
- A new message, enum or RPC.
- A new enum value. An older console reading it over JSON gets the field's zero value
  (`…_UNSPECIFIED`), which its pages show as unknown; say in the PR what the new value
  means, so the console can handle it when it bumps.
- A new `google.api.http` binding for a new RPC.
- Comments.

Use `optional` when "not set" and "zero" must be told apart, and give every new field a
`buf.validate` rule where one applies.

## Changes that break clients, and what to do instead

If `buf breaking` fails, don't work around it. Pick the path that fits.

### 1. Expand, migrate, contract (the normal path)

Use this for removing a field, changing a field's type, or renaming a field, RPC or message.

1. **Expand** (compatible). Add the replacement, if there is one, and mark the old one
   deprecated. The server keeps filling and accepting both.

   ```proto
   int32 version = 8 [deprecated = true]; // Use resource_version (ADR-0003).
   string resource_version = 9;
   ```

2. **Migrate.** Consumers move to the new field. In the console, protobuf-es marks the old
   field `@deprecated` in the generated TypeScript, so its uses are flagged after the pin
   bump. Wait until the console's pin is at or past the expand commit and it no longer reads
   or writes the old field.
3. **Contract** (breaking, approved). Remove the old field and reserve both its number and
   its name, so neither can be reused for something else:

   ```proto
   reserved 8;
   reserved "version";
   ```

   The breaking check fails on this PR, as it should. The PR description names the change,
   the consumers that have migrated (for the console: the pinned commit that no longer uses
   it), and why it's safe now. A maintainer then adds the `buf skip breaking` label, which
   skips the check for that PR only. Adding or removing the label reruns CI, so removing
   the exception restores the breaking check for the current head.

A field's type can never be changed in place. `int32` to `string` is always a new field.

**Never reuse a field number**, not even after a long time: an old binary client would read
the new value as the old field and no error would tell anyone.

### Recorded deviations from path 1

Each entry names the change, why the expand step was skipped, and the condition that keeps it
safe.

- **`ModelSource.name` (2026-10-29, backend #47).** Its meaning changed in one step, from "the
  name clients send" to "empty or the model's id": a model is served as its id, and any other
  value is `InvalidArgument`. The field is `[deprecated = true]` and still filled on output.
  Expand and migrate were skipped because the only consumer, the console, stopped sending the
  field in console #86 before this change merged. **Condition:** a deployment moves its
  backend and console pins together, and never runs a backend at or past #47 with a console
  older than #86. Another client that sends a served name gets `InvalidArgument`.

### 2. Unstable areas

The draft package is ignored by the breaking check. Anything else that's experimental
belongs in a package that says so (`ai_grid.v1alpha1`), and `ignore_unstable_packages` is
the switch for those. Don't put unstable fields into `ai_grid.v1`.

### 3. A redesign

When an API has to change shape completely, add `ai_grid.v2` next to `ai_grid.v1`, serve
both, move the clients, then retire v1. For the tech preview, path 1 is almost always
enough.

## Versions, validation and errors

- **Optimistic locking.** Update and Delete carry the version the caller last read; a
  mismatch answers `Aborted`. `Metadata.version` is `int32` today. The console's proposed ADR-0003
  recommends a string, because a Kubernetes `resourceVersion` is opaque. If agreed, that
  change follows path 1 above; clients continue to use the shipped integer contract.
- **Validation.** Field rules belong in `buf.validate` annotations. Cross-resource checks
  stay in server code and answer `InvalidArgument`, `FailedPrecondition` or
  `AlreadyExists`.
- **Errors.** Return Connect codes as [`API.md`](API.md) section 8 describes, and match them
  by code, never by message. A browser maps the code to what the page shows, so a wrong
  code is a contract bug.

## Pinning and releases

The console foundation pins a full commit SHA of this repository and generates its types
from that commit. Pin updates are manual today: each update regenerates the clients and
runs the console checks. A nightly job to propose updates to the newest `main` is planned
but not implemented yet. So:

- Everything merged to `main` must pass the breaking check, or carry an approved break
  from path 1.
- The Go module path is `github.com/osac-project/ai-grid-provider-backend`, which doesn't
  resolve, and there are no release tags yet. Neither blocks the console, which reads
  `proto/` from git. Go importers need both fixed.

## Checklist for a change to `proto/ai_grid/v1`

1. Edit the proto. Follow [`API.md`](API.md) for shapes and names.
2. `make generate`, then `make check-generated`.
3. `buf lint proto`.
4. `buf breaking proto --against '.git#branch=main,subdir=proto'`. If it fails,
   follow the section above; don't merge a surprise break.
5. Read the `docs/openapi` diff: does every REST path still say what you mean?
6. Update `docs/UI_API_REFERENCE.md` and the comments in the proto. They're what the
   console's developers read.
7. In the PR description, list new fields and enum values with their meaning, and any
   approved break with the consumers it affects.
