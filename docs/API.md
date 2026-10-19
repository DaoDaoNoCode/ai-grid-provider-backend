# API building standards

Authoritative API and service-architecture standards for
`ai-grid-provider-backend`. These conventions are based on the OSAC fulfillment
service architecture. Agents must read and follow this document for proto, REST,
resource-shape, server-layer, and related API work. Entry-point instructions for
agents live in [`../AGENTS.md`](../AGENTS.md).

Patterns, naming conventions, and examples below were verified against the OSAC
fulfillment service. Adapt package paths and URL prefixes to this repository
(`ai_grid.v1`, `/api/v1/`) when implementing here. Current MVP endpoints
are intentionally simpler; new resource APIs must converge on these standards.

---

## Table of Contents

1. [Project Layout & Module Structure](#1-project-layout--module-structure)
2. [API Design (Proto Conventions)](#2-api-design-proto-conventions)
3. [Adding a New Resource Type — The 7-Layer Checklist](#3-adding-a-new-resource-type--the-7-layer-checklist)
4. [Core Abstractions Reference](#4-core-abstractions-reference)
5. [Data Model Philosophy](#5-data-model-philosophy)
6. [Interceptor Chain (Request Pipeline)](#6-interceptor-chain-request-pipeline)
7. [Configuration & Dependency Wiring](#7-configuration--dependency-wiring)
8. [Error Handling](#8-error-handling)
9. [Logging](#9-logging)
10. [Testing Standards](#10-testing-standards)
11. [Build & Deployment](#11-build--deployment)
12. [Event System](#12-event-system)
13. [Authentication & Authorization](#13-authentication--authorization)

---

## 1. Project Layout & Module Structure

### This repository today

```
ai-grid-provider-backend/
├── cmd/ai-grid-provider-backend/   # Service entrypoint
├── client/                         # Reusable gRPC client
├── proto/ai_grid/v1/               # Per-resource *_type.proto / *_service.proto + metadata
├── gen/ai_grid/v1/                 # Generated Go + gateway stubs (committed)
├── docs/openapi/                   # Generated OpenAPI (Swagger 2.0)
├── internal/servers/               # Per-resource gRPC servers (OSAC-style builders)
├── internal/keycloak/              # Keycloak Admin API client
├── internal/restgateway/           # HTTP gateway + Swagger UI
├── deploy/kind/                    # Kind manifests
├── hack/                           # Local Kind/OCM helpers
├── integration/                    # Kind integration tests
├── Containerfile
├── Makefile
├── AGENTS.md                       # Agent entrypoint
└── docs/API.md                     # This file
```

Module: `github.com/osac-project/ai-grid-provider-backend`.

REST prefix for this service: `/api/v1/`. When the examples below use
`/api/fulfillment/v1/` or `/api/private/v1/`, map them to `/api/v1/`
unless a public/private split is intentionally introduced.

### Reference: OSAC Go Workspace

The OSAC platform (reference architecture) is a Go workspace mono-repo. The workspace file `go.work` at the repo root lists modules such as:

```
go 1.26.3

use (
    ./fulfillment-service
    ./proto
    ./bare-metal-fulfillment-operator
    ./osac-operator
    // ... others
)
```

Each service module declares `replace` directives in its `go.mod` for sibling dependencies:

```go
// fulfillment-service/go.mod
module github.com/osac-project/osac/fulfillment-service

replace (
    github.com/osac-project/osac/bare-metal-fulfillment-operator => ../bare-metal-fulfillment-operator
    github.com/osac-project/osac/osac-operator/api => ../osac-operator/api
    github.com/osac-project/osac/proto => ../proto
)
```

### Directory Layout

```
fulfillment-service/
├── cmd/                          # Entry points (one subdir per binary)
│   ├── fulfillment-service/      # Server binary → internal/cmd/service.Root()
│   ├── osac/                     # CLI binary → internal/cmd/cli.Root()
│   ├── osac-dev/                 # Dev CLI → internal/cmd/osac-dev.Root()
│   ├── test-server/              # Mock gRPC server for testing
│   └── buf-plugin-osac-lint/     # Custom buf lint plugin
├── internal/                     # All Go business logic (~45 packages)
│   ├── cmd/                      # Cobra command trees (service/, cli/, osac-dev/)
│   ├── servers/                  # gRPC server implementations (~180 files)
│   ├── database/                 # PostgreSQL: connection, migrations, DAO, tx
│   │   ├── dao/                  # GenericDAO[O] and typed errors
│   │   └── migrations/           # Numbered SQL migrations (118+ files)
│   ├── controllers/              # Reconcilers (one subdir per resource)
│   ├── auth/                     # JWT, JWKS, OPA/Rego authz, tenancy
│   │   ├── jwe/                  # JWE token sealing
│   │   └── policies/             # Rego policy files
│   ├── events/                   # Notifier/Listener interfaces
│   ├── network/                  # gRPC client/server builders, TLS, CORS
│   ├── logging/                  # slog logger, interceptor, context
│   ├── metrics/                  # Prometheus metrics, interceptor
│   ├── validation/               # Protovalidate interceptor
│   ├── recovery/                 # Panic recovery interceptor
│   ├── shutdown/                 # Graceful shutdown orchestration
│   ├── health/                   # Health aggregator
│   ├── services/                 # Service enablement flags (CaaS/VMaaS/BMaaS/MaaS)
│   ├── config/                   # CLI settings (JSON + OS keyring)
│   ├── vault/                    # HashiCorp Vault integration
│   ├── oauth/                    # OAuth token flows
│   ├── idp/                      # IdP client
│   ├── testing/                  # Test helpers, fixtures, mock builders
│   └── version/                  # Version injection via ldflags
├── it/                           # Integration test suite (60+ files)
├── charts/service/               # Helm chart
├── docs/                         # Design documents (API.md, CODEWALK.md, etc.)
├── examples/                     # Example data
├── scripts/                      # Shell scripts
├── .golangci.yml                 # Linter configuration
├── .goreleaser.yaml              # Release automation
├── Containerfile                 # Multi-stage container build
├── Makefile                      # Container image targets
├── dev.py                        # Python-based dev CLI
└── AGENTS.md                     # AI coding agent instructions / invariants
```

### Proto Module (Sibling)

Proto definitions live in a **separate sibling module** at `../proto/`, NOT inside the service directory.

```
proto/
├── public/osac/public/v1/       # Generated from private (NEVER edit manually)
├── private/osac/private/v1/     # Editable source of truth (~60 files)
├── tests/osac/tests/v1/         # Test-only protos (editable)
├── gen/                          # Generated Go code (committed to repo)
│   ├── osac/public/v1/
│   ├── osac/private/v1/
│   └── osac/tests/v1/
├── buf.yaml                      # Three modules: public, private, tests
└── buf.gen.yaml                  # Code generation config
```

**Invariant**: Only edit files in `proto/private/`. `proto/public/` and `proto/gen/` are generated by `make -C ../proto generate`.

### Entry Points

The server binary (`cmd/fulfillment-service`) provides these subcommands via Cobra:

| Subcommand | Purpose |
|-----------|---------|
| `start grpc-server` | Main gRPC API server |
| `start rest-gateway` | gRPC-Gateway REST proxy |
| `start controller` | Kubernetes reconciliation controllers |
| `start console-proxy` | VM console WebSocket proxy |
| `migrate` | Database schema migrations |
| `probe grpc-server` | Health probe |
| `version` | Print version |

---

## 2. API Design (Proto Conventions)

> Full reference: `docs/API.md` in the fulfillment service codebase.

### Object Shape

All resource types follow a standard structure, enforced by the custom `OSAC_OBJECT_SHAPE` buf lint rule (implemented in `cmd/buf-plugin-osac-lint/`):

```protobuf
message Thing {
  string id = 1;
  Metadata metadata = 2;
  ThingSpec spec = 3;
  ThingStatus status = 4;
}
```

- `id` — system-assigned unique identifier
- `metadata` — common fields (name, labels, annotations, creator, tenant, version, etc.)
- `spec` — **user-controlled** desired state (system must never modify)
- `status` — **system-controlled** observed state (users cannot modify)

Exception: static configuration objects (e.g., `HostType`, `ClusterTemplate`) may omit `spec`/`status` and use flat fields. This requires a lint suppression comment: `// buf:lint:ignore OSAC_OBJECT_SHAPE`.

### File Organization

Each resource type spans two proto files:

| File | Contents |
|------|----------|
| `<type>_type.proto` | Message definition, Spec, Status, conditions, enums |
| `<type>s_service.proto` | gRPC service definition, request/response messages |

Example: `cluster_type.proto` + `clusters_service.proto`

### Public/Private API Split

The public API is **auto-generated** from the private API using `protoc-gen-cleanapi`. Elements are filtered using annotations:

```protobuf
import "cleanapi/annotations.proto";

// Field-level: remove from public API
string internal_node_id = 3 [(cleanapi.field).private = true];

// Message-level: entire message removed
message InternalDebugInfo {
  option (cleanapi.message).private = true;
}

// Method-level: RPC removed (e.g., Signal)
rpc Signal(ThingsSignalRequest) returns (ThingsSignalResponse) {
  option (cleanapi.method).private = true;
}

// File-level: entire file removed
option (cleanapi.file).private = true;
```

**Rule**: Private-only fields must always be placed at the **end** of the message to preserve field numbering compatibility.

### Standard Service RPCs

| Method | Purpose | Public | Private |
|--------|---------|--------|---------|
| `Create` | Create a new object | Yes | Yes |
| `List` | Filtered list with offset/limit/CEL filter | Yes | Yes |
| `Get` | Single object by `id` | Yes | Yes |
| `Update` | Partial update with field mask | Yes (most) | Yes |
| `Delete` | Delete by `id` | Yes | Yes |
| `Signal` | Notify controller of needed reconciliation | **No** | Yes |

### Request/Response Naming

Pattern: `{ServiceName}{Method}Request` / `{ServiceName}{Method}Response`

```protobuf
// Service: Things
message ThingsCreateRequest { Thing object = 1; }
message ThingsCreateResponse { Thing object = 1; }
message ThingsListRequest {
  optional int32 offset = 1;
  optional int32 limit = 2;
  optional string filter = 3;   // CEL expression
  optional string order = 4;
}
message ThingsListResponse {
  int32 size = 1;
  int32 total = 2;
  repeated Thing items = 3;
}
message ThingsGetRequest { string id = 1; }
message ThingsGetResponse { Thing object = 1; }
message ThingsUpdateRequest {
  Thing object = 1;
  google.protobuf.FieldMask update_mask = 2;
  bool lock = 3;   // Optimistic locking
}
message ThingsUpdateResponse { Thing object = 1; }
message ThingsDeleteRequest { string id = 1; }
message ThingsDeleteResponse {}

// Private only:
message ThingsSignalRequest { string id = 1; }
message ThingsSignalResponse {}
```

### REST Transcoding

| Method | HTTP | URL | Body | Response |
|--------|------|-----|------|----------|
| List | `GET` | `/api/fulfillment/v1/things` | — | full response |
| Get | `GET` | `/api/fulfillment/v1/things/{id}` | — | `object` |
| Create | `POST` | `/api/fulfillment/v1/things` | `object` | `object` |
| Update | `PATCH` | `/api/fulfillment/v1/things/{object.id}` | `object` | `object` |
| Delete | `DELETE` | `/api/fulfillment/v1/things/{id}` | — | — |

**URL prefix (this service)**: `/api/v1/`.
**URL prefix (OSAC fulfillment reference)**: `/api/fulfillment/v1/` (public), `/api/private/v1/` (private).
**URL space**: Always flat — no nested collection URLs. Use CEL filters to find related objects.

When adopting a public/private split later, the `cleanapi.file` annotation
`http_route_prefix_map` controls public URL generation (fulfillment example:
`option (cleanapi.file).http_route_prefix_map = "private:fulfillment";`).

### Naming Conventions

| Element | Convention | Example |
|---------|-----------|---------|
| Types | `CamelCase` | `ComputeInstance`, `ExternalIPAttachment` |
| Spec/Status | `<Type>Spec`, `<Type>Status` | `ClusterSpec`, `ClusterStatus` |
| Fields | `snake_case` | `virtual_network`, `creation_timestamp` |
| Enum types | `CamelCase` | `ClusterState` |
| Enum values | `UPPER_SNAKE_CASE` with type prefix | `CLUSTER_STATE_READY` |
| Service names | Plural of type, no `Service` suffix | `Clusters`, `BareMetalInstances` |

**Enum rule**: Every enum must start with `_UNSPECIFIED = 0`.

### Metadata Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Required, immutable, RFC 1123 DNS label (1–63 chars, `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`). Unique per (tenant, project, resource type). |
| `creation_timestamp` | `Timestamp` | System-set at creation |
| `deletion_timestamp` | `Timestamp` | System-set when marked for deletion |
| `creator` | `string` | System-determined identity |
| `tenant` | `string` | Owning tenant |
| `labels` | `map<string, string>` | Indexed key-value pairs |
| `annotations` | `map<string, string>` | Arbitrary user metadata |
| `version` | `int32` | Auto-incremented on every change |
| `display_name` | `string` | Optional friendly label (max 63) |
| `description` | `string` | Optional description (max 256) |
| `finalizers` | `repeated string` | **Private only** — deletion protection |

### Validation

- Use `buf.validate` proto annotations for field/cross-field validation with CEL expressions
- Cross-resource validation (existence checks, uniqueness, quotas) must be in server logic, not proto annotations
- Update validation operates on the **merged** object (stored + update mask), not the partial request

### Object References

Use typed reference messages, not plain strings:

```protobuf
// Full reference (cross-project capable)
message ClusterTemplateReference {
  string id = 1;
  string name = 2;
  string project = 3;
  bool shared = 4;
}

// Local reference (same tenant/project)
message VirtualNetworkLocalReference {
  string id = 1;
  string name = 2;
}
```

Server auto-populates whichever of `id`/`name` the caller omits after lookup.

---

## 3. Adding a New Resource Type — The 7-Layer Checklist

This is the repeatable recipe for adding a new API resource. Every layer is mandatory unless noted.

### Layer 1: Proto Definitions

**Location**: `../proto/private/osac/private/v1/`

Create two files:

**`<resource>_type.proto`** — the type definition:
```protobuf
syntax = "proto3";
package osac.private.v1;

import "osac/private/v1/metadata_type.proto";
import "buf/validate/validate.proto";
import "cleanapi/annotations.proto";
import "google/protobuf/timestamp.proto";

message Thing {
  string id = 1;
  Metadata metadata = 2;
  ThingSpec spec = 3;
  ThingStatus status = 4;
}

message ThingSpec {
  // User-controlled desired state fields
}

message ThingStatus {
  ThingState state = 1;
  repeated ThingCondition conditions = 2;
  // System-controlled observed state fields
}

enum ThingState {
  THING_STATE_UNSPECIFIED = 0;
  THING_STATE_PROGRESSING = 1;
  THING_STATE_READY = 2;
  THING_STATE_FAILED = 3;
}

message ThingCondition {
  ThingConditionType type = 1;
  ConditionStatus status = 2;
  google.protobuf.Timestamp last_transition_time = 3;
  optional string reason = 4;
  optional string message = 5;
}

enum ThingConditionType {
  THING_CONDITION_TYPE_UNSPECIFIED = 0;
  THING_CONDITION_TYPE_PROGRESSING = 1;
  THING_CONDITION_TYPE_READY = 2;
  THING_CONDITION_TYPE_FAILED = 3;
}
```

**`<resource>s_service.proto`** — the service definition:
```protobuf
syntax = "proto3";
package osac.private.v1;

import "osac/private/v1/<resource>_type.proto";
import "google/api/annotations.proto";
import "google/protobuf/field_mask.proto";
import "cleanapi/annotations.proto";

option (cleanapi.file).http_route_prefix_map = "private:fulfillment";

service Things {
  rpc List(ThingsListRequest) returns (ThingsListResponse) {
    option (google.api.http) = {
      get: "/api/private/v1/things"
    };
  }
  rpc Get(ThingsGetRequest) returns (ThingsGetResponse) {
    option (google.api.http) = {
      get: "/api/private/v1/things/{id}"
      response_body: "object"
    };
  }
  rpc Create(ThingsCreateRequest) returns (ThingsCreateResponse) {
    option (google.api.http) = {
      post: "/api/private/v1/things"
      body: "object"
      response_body: "object"
    };
  }
  rpc Update(ThingsUpdateRequest) returns (ThingsUpdateResponse) {
    option (google.api.http) = {
      patch: "/api/private/v1/things/{object.id}"
      body: "object"
      response_body: "object"
    };
  }
  rpc Delete(ThingsDeleteRequest) returns (ThingsDeleteResponse) {
    option (google.api.http) = {
      delete: "/api/private/v1/things/{id}"
    };
  }
  rpc Signal(ThingsSignalRequest) returns (ThingsSignalResponse) {
    option (cleanapi.method).private = true;
  }
}

// Standard request/response messages (see Section 2 for full set)
```

**Add to the Event oneof** in `proto/private/osac/private/v1/event_type.proto`:
```protobuf
message Event {
  // ... existing fields ...
  oneof payload {
    // ... existing entries ...
    Thing thing = <next_number>;
  }
}
```

**Generate**: `make -C ../proto generate`

This produces:
- `proto/public/osac/public/v1/` — auto-generated public protos (cleanapi-filtered)
- `proto/gen/osac/public/v1/` — generated Go code for public API
- `proto/gen/osac/private/v1/` — generated Go code for private API

### Layer 2: Database Migration

**Location**: `internal/database/migrations/`
**File**: `<next_number>_create_<resources>_tables.up.sql`

Standard table template (verified from migration 95 — `create_volumes_tables`):

```sql
create table things (
  id text not null primary key,
  name text not null default '',
  creation_timestamp timestamp with time zone not null default now(),
  deletion_timestamp timestamp with time zone not null default 'epoch',
  finalizers text[] not null default '{}',
  creator text not null default '',
  tenant text not null default '',
  project ltree not null default ''::ltree,
  labels jsonb not null default '{}'::jsonb,
  annotations jsonb not null default '{}'::jsonb,
  data jsonb not null,
  version integer not null default 0
);

create table archived_things (
  id text not null,
  name text not null default '',
  creation_timestamp timestamp with time zone not null,
  deletion_timestamp timestamp with time zone not null,
  archival_timestamp timestamp with time zone not null default now(),
  creator text not null default '',
  tenant text not null default '',
  project ltree not null default ''::ltree,
  labels jsonb not null default '{}'::jsonb,
  annotations jsonb not null default '{}'::jsonb,
  data jsonb not null,
  version integer not null default 0
);

-- Standard indexes
create index things_by_name on things (name);
create index things_by_creator on things (creator);
create index things_by_tenant on things (tenant);
create index things_by_label on things using gin (labels);

-- Unique name per tenant on active records only
create unique index things_unique_name_per_tenant
  on things (name, tenant)
  where deletion_timestamp = 'epoch'
    and name != '';

-- Tenant foreign key
alter table things
  add constraint things_tenant_fk
  foreign key (tenant) references tenants (name);

-- Project foreign key
alter table things
  add constraint things_project_fk
  foreign key (tenant, project) references projects (tenant, name);

-- Immutable columns trigger
create trigger check_immutable_columns
  before update on things
  for each row
  execute function check_immutable_columns('id', 'name', 'tenant', 'project');
```

**Rules**:
- Migrations are **immutable** once applied. Never modify an existing migration — always add a new numbered one.
- The `check_immutable_columns` function already exists (created in an earlier migration) — just reference it.
- Every table must have the full standard column set. Do not omit columns.
- The `archived_` table is for soft-delete archival — objects move here after finalizers are cleared and deletion completes.

### Layer 3: Private Server

**Location**: `internal/servers/private_<resources>_server.go`

```go
package servers

import (
    "context"
    "fmt"
    "log/slog"

    "github.com/prometheus/client_golang/prometheus"
    privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"

    "github.com/osac-project/osac/fulfillment-service/internal/auth"
    "github.com/osac-project/osac/fulfillment-service/internal/events"
)

// Compile-time interface check
var _ privatev1.ThingsServer = (*PrivateThingsServer)(nil)

type PrivateThingsServer struct {
    privatev1.UnimplementedThingsServer
    generic *GenericServer[*privatev1.Thing]
}

type PrivateThingsServerBuilder struct {
    logger            *slog.Logger
    notifier          events.Notifier
    attributionLogic  auth.AttributionLogic
    tenancyLogic      auth.TenancyLogic
    metricsRegisterer prometheus.Registerer
}

func NewPrivateThingsServer() *PrivateThingsServerBuilder {
    return &PrivateThingsServerBuilder{}
}

func (b *PrivateThingsServerBuilder) SetLogger(v *slog.Logger) *PrivateThingsServerBuilder {
    b.logger = v
    return b
}

func (b *PrivateThingsServerBuilder) SetNotifier(v events.Notifier) *PrivateThingsServerBuilder {
    b.notifier = v
    return b
}

func (b *PrivateThingsServerBuilder) SetAttributionLogic(v auth.AttributionLogic) *PrivateThingsServerBuilder {
    b.attributionLogic = v
    return b
}

func (b *PrivateThingsServerBuilder) SetTenancyLogic(v auth.TenancyLogic) *PrivateThingsServerBuilder {
    b.tenancyLogic = v
    return b
}

func (b *PrivateThingsServerBuilder) SetMetricsRegisterer(v prometheus.Registerer) *PrivateThingsServerBuilder {
    b.metricsRegisterer = v
    return b
}

func (b *PrivateThingsServerBuilder) Build() (*PrivateThingsServer, error) {
    if b.logger == nil {
        return nil, fmt.Errorf("logger is mandatory")
    }
    generic, err := NewGenericServer[*privatev1.Thing]().
        SetLogger(b.logger).
        SetService(privatev1.Things_ServiceDesc.ServiceName).
        SetNotifier(b.notifier).
        SetAttributionLogic(b.attributionLogic).
        SetTenancyLogic(b.tenancyLogic).
        SetMetricsRegisterer(b.metricsRegisterer).
        Build()
    if err != nil {
        return nil, err
    }
    return &PrivateThingsServer{generic: generic}, nil
}

// Standard CRUD delegation

func (s *PrivateThingsServer) List(ctx context.Context,
    request *privatev1.ThingsListRequest) (response *privatev1.ThingsListResponse, err error) {
    err = s.generic.List(ctx, request, &response)
    return
}

func (s *PrivateThingsServer) Get(ctx context.Context,
    request *privatev1.ThingsGetRequest) (response *privatev1.ThingsGetResponse, err error) {
    err = s.generic.Get(ctx, request, &response)
    return
}

func (s *PrivateThingsServer) Create(ctx context.Context,
    request *privatev1.ThingsCreateRequest) (response *privatev1.ThingsCreateResponse, err error) {
    err = s.generic.CreateWithCandidatePreparation(ctx, request, &response,
        func(ctx context.Context, _ *privatev1.Thing, candidate *privatev1.Thing) error {
            return s.prepareCreate(ctx, candidate)
        })
    return
}

func (s *PrivateThingsServer) Update(ctx context.Context,
    request *privatev1.ThingsUpdateRequest) (response *privatev1.ThingsUpdateResponse, err error) {
    err = s.generic.UpdateWithCandidatePreparation(ctx, request, &response,
        func(ctx context.Context, current *privatev1.Thing, candidate *privatev1.Thing) error {
            return s.prepareUpdate(ctx, current, candidate)
        })
    return
}

func (s *PrivateThingsServer) Delete(ctx context.Context,
    request *privatev1.ThingsDeleteRequest) (response *privatev1.ThingsDeleteResponse, err error) {
    err = s.generic.Delete(ctx, request, &response)
    return
}

func (s *PrivateThingsServer) Signal(ctx context.Context,
    request *privatev1.ThingsSignalRequest) (response *privatev1.ThingsSignalResponse, err error) {
    err = s.generic.Signal(ctx, request, &response)
    return
}

// Domain-specific validation

func (s *PrivateThingsServer) prepareCreate(ctx context.Context,
    candidate *privatev1.Thing) error {
    // Validate spec, resolve references, set defaults on status
    return nil
}

func (s *PrivateThingsServer) prepareUpdate(ctx context.Context,
    current *privatev1.Thing, candidate *privatev1.Thing) error {
    // Validate changes, check immutability constraints
    return nil
}
```

**Key points**:
- The compile-time interface check (`var _ privatev1.ThingsServer = ...`) catches missing method implementations at build time.
- `CreateWithCandidatePreparation` and `UpdateWithCandidatePreparation` accept a `PrepareCandidateFunc` that receives both the current object (nil for create) and the candidate object. Validation and side effects go here.
- For resources needing cross-resource validation, add additional DAO references to the server struct and builder.

### Layer 4: Public Server

**Location**: `internal/servers/<resources>_server.go`

The public server wraps the private server and adds public/private type mapping:

```go
package servers

import (
    "context"
    "fmt"
    "log/slog"

    "github.com/prometheus/client_golang/prometheus"

    publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
    privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"

    "github.com/osac-project/osac/fulfillment-service/internal/auth"
    "github.com/osac-project/osac/fulfillment-service/internal/events"
)

var _ publicv1.ThingsServer = (*ThingsServer)(nil)

var thingStatusField = (*privatev1.Thing)(nil).ProtoReflect().Descriptor().
    Fields().ByName("status")

type ThingsServer struct {
    publicv1.UnimplementedThingsServer
    logger    *slog.Logger
    private   privatev1.ThingsServer
    inMapper  *GenericMapper[*publicv1.Thing, *privatev1.Thing]
    outMapper *GenericMapper[*privatev1.Thing, *publicv1.Thing]
}

// Builder pattern: same SetLogger/SetNotifier/etc. chain as private server

func (b *ThingsServerBuilder) Build() (*ThingsServer, error) {
    // inMapper: strict mode, ignores status (users cannot set status)
    inMapper, err := NewGenericMapper[*publicv1.Thing, *privatev1.Thing]().
        SetLogger(b.logger).
        SetStrict(true).
        AddIgnoredFields(thingStatusField.FullName()).
        Build()
    if err != nil {
        return nil, err
    }

    // outMapper: non-strict (private-only fields silently dropped)
    outMapper, err := NewGenericMapper[*privatev1.Thing, *publicv1.Thing]().
        SetLogger(b.logger).
        SetStrict(false).
        Build()
    if err != nil {
        return nil, err
    }

    // Build private delegate
    private, err := NewPrivateThingsServer().
        SetLogger(b.logger).
        SetNotifier(b.notifier).
        SetAttributionLogic(b.attributionLogic).
        SetTenancyLogic(b.tenancyLogic).
        SetMetricsRegisterer(b.metricsRegisterer).
        Build()
    if err != nil {
        return nil, err
    }

    return &ThingsServer{
        logger:    b.logger,
        private:   private,
        inMapper:  inMapper,
        outMapper: outMapper,
    }, nil
}
```

**CRUD methods** delegate to the private server with mapper conversion. The pattern for each:

```go
func (s *ThingsServer) Create(ctx context.Context,
    request *publicv1.ThingsCreateRequest) (*publicv1.ThingsCreateResponse, error) {
    privateObject := &privatev1.Thing{}
    if err := s.inMapper.Copy(ctx, request.GetObject(), privateObject); err != nil {
        return nil, err
    }
    privateResponse, err := s.private.Create(ctx,
        privatev1.ThingsCreateRequest_builder{Object: privateObject}.Build())
    if err != nil {
        return nil, err
    }
    publicObject := &publicv1.Thing{}
    if err := s.outMapper.Copy(ctx, privateResponse.GetObject(), publicObject); err != nil {
        return nil, err
    }
    return publicv1.ThingsCreateResponse_builder{Object: publicObject}.Build(), nil
}
```

**Update** has special handling based on field mask presence:
- **With field mask**: starts from an empty private object (only the ID set), merges the request fields
- **Without field mask**: fetches the current object via `Get`, then merges the request onto it

### Layer 5: gRPC Server Registration

**Location**: `internal/cmd/service/start/grpcserver/register_servers.go`

Add to `RegisterResourceServers()`:

```go
// Create and register the public things server
deps.Logger.InfoContext(ctx, "Creating things server")
thingsServer, err := servers.NewThingsServer().
    SetLogger(deps.Logger).
    SetNotifier(deps.Notifier).
    SetAttributionLogic(deps.PublicAttributionLogic).
    SetTenancyLogic(deps.TenancyLogic).
    SetMetricsRegisterer(deps.MetricsRegisterer).
    Build()
if err != nil {
    return nil, fmt.Errorf("failed to create things server: %w", err)
}
publicv1.RegisterThingsServer(registrar, thingsServer)

// Create and register the private things server
deps.Logger.InfoContext(ctx, "Creating private things server")
privateThingsServer, err := servers.NewPrivateThingsServer().
    SetLogger(deps.Logger).
    SetNotifier(deps.Notifier).
    SetAttributionLogic(deps.PrivateAttributionLogic).
    SetTenancyLogic(deps.TenancyLogic).
    SetMetricsRegisterer(deps.MetricsRegisterer).
    Build()
if err != nil {
    return nil, fmt.Errorf("failed to create private things server: %w", err)
}
privatev1.RegisterThingsServer(registrar, privateThingsServer)
```

**Feature gating**: For resources that belong to a specific service tier, wrap registration in a conditional:

```go
if deps.Services.CaaS {
    // CaaS-specific resources
}
if deps.Services.VMaaS {
    // VMaaS-specific resources
}
```

The `ResourceServerDeps` struct provides all shared dependencies:

| Field | Type | Purpose |
|-------|------|---------|
| `Logger` | `*slog.Logger` | Structured logging |
| `Notifier` | `events.Notifier` | Event publication |
| `PublicAttributionLogic` | `auth.AttributionLogic` | Creator for public API |
| `PrivateAttributionLogic` | `auth.AttributionLogic` | Creator for private API |
| `TenancyLogic` | `auth.TenancyLogic` | Tenant visibility |
| `MetricsRegisterer` | `prometheus.Registerer` | Prometheus metrics |
| `Services` | `*services.Flags` | Feature enablement flags |

### Layer 6: REST Gateway Registration

**Location**: `internal/cmd/service/start/restgateway/start_rest_gateway_cmd.go`

Add **both** public and private handlers to `buildHandlerList()`:

```go
func buildHandlerList() []handlerRegistrar {
    return []handlerRegistrar{
        // ... existing handlers ...

        // Things:
        publicv1.RegisterThingsHandler,
        privatev1.RegisterThingsHandler,
    }
}
```

> **Pitfall**: Missing this registration causes REST 404 while gRPC works perfectly. This is a validated, common mistake. All handlers are always registered — disabled services are rejected by the gRPC server's tap handler, which grpc-gateway translates to HTTP 503.

### Layer 7: Reconciler (Optional)

Required when the resource needs to drive side effects (create K8s CRDs, trigger AAP provisioning, etc.). Not needed for static reference data (host types, templates, etc.).

**Location**: `internal/controllers/<resource>/`

**Reconciler function** (`<resource>_reconciler_function.go`):

```go
package thing

import (
    "context"
    "fmt"
    "log/slog"

    "google.golang.org/grpc"
    "google.golang.org/protobuf/proto"
    privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"

    "github.com/osac-project/osac/fulfillment-service/internal/controllers"
)

type Function struct {
    logger       *slog.Logger
    thingsClient privatev1.ThingsClient
    hubsClient   privatev1.HubsClient
}

type FunctionBuilder struct {
    logger     *slog.Logger
    connection *grpc.ClientConn
}

func NewFunction() *FunctionBuilder {
    return &FunctionBuilder{}
}

func (b *FunctionBuilder) SetLogger(v *slog.Logger) *FunctionBuilder {
    b.logger = v
    return b
}

func (b *FunctionBuilder) SetConnection(v *grpc.ClientConn) *FunctionBuilder {
    b.connection = v
    return b
}

func (b *FunctionBuilder) Build() (controllers.ReconcilerFunction[*privatev1.Thing], error) {
    if b.logger == nil {
        return nil, fmt.Errorf("logger is mandatory")
    }
    f := &Function{
        logger:       b.logger,
        thingsClient: privatev1.NewThingsClient(b.connection),
        hubsClient:   privatev1.NewHubsClient(b.connection),
    }
    return f.run, nil
}

func (f *Function) run(ctx context.Context, object *privatev1.Thing) error {
    task := &task{
        f:     f,
        thing: proto.Clone(object).(*privatev1.Thing),
    }
    if object.GetMetadata().GetDeletionTimestamp().IsValid() {
        return task.delete(ctx)
    }
    return task.update(ctx)
}
```

**Registration** in `internal/cmd/service/start/controller/start_controller_cmd.go`:

```go
// Create the reconciler function
thingReconcilerFunction, err := thing.NewFunction().
    SetLogger(r.logger).
    SetConnection(r.client).
    Build()
if err != nil {
    return fmt.Errorf("failed to create thing reconciler: %w", err)
}

// Create the reconciler
thingReconciler, err := controllers.NewReconciler[*privatev1.Thing]().
    SetLogger(r.logger).
    SetName("thing").
    SetClient(r.client).
    SetFunction(thingReconcilerFunction).
    SetEventFilter("has(event.thing)").
    SetHealthReporter(healthAggregator).
    Build()
if err != nil {
    return fmt.Errorf("failed to create thing reconciler: %w", err)
}

go func() {
    err := thingReconciler.Start(ctx)
    if err != nil {
        r.logger.ErrorContext(ctx, "Thing reconciler failed", slog.Any("error", err))
    }
}()
```

**Reconciler design rules**:
- Controller is a **separate process** — a gRPC client of the private API. It never touches PostgreSQL directly.
- Clone the object before modifying it (`proto.Clone`).
- Dispatch to `update()` or `delete()` based on `deletion_timestamp`.
- After calculating changes, compute a field mask and send an `Update` RPC with only the modified fields.
- **Equivalence check**: GenericServer's built-in equivalence check prevents no-op writes — if the merged object is unchanged, no DB write occurs and no event fires. This prevents event storms and infinite reconciliation loops.
- Hub selection: randomly chosen from available hubs, persisted to `status.hub` on first reconciliation.

### Layer 7 Summary Checklist

| # | Layer | File(s) | Validated |
|---|-------|---------|-----------|
| 1 | Proto type + service | `../proto/private/osac/private/v1/<resource>_type.proto` + `<resources>_service.proto` | `make -C ../proto generate` succeeds |
| 2 | Event oneof | `../proto/private/osac/private/v1/event_type.proto` | Included in generate |
| 3 | DB migration | `internal/database/migrations/<N>_create_<resources>_tables.up.sql` | `fulfillment-service migrate` succeeds |
| 4 | Private server | `internal/servers/private_<resources>_server.go` | `go build` + unit tests |
| 5 | Public server | `internal/servers/<resources>_server.go` | `go build` + unit tests |
| 6 | gRPC registration | `register_servers.go` — both public + private | gRPC calls work |
| 7 | REST registration | `start_rest_gateway_cmd.go` — both handlers | REST calls work |
| 8 | Reconciler (optional) | `internal/controllers/<resource>/` + `start_controller_cmd.go` | Controller reconciles |

---

## 4. Core Abstractions Reference

### GenericServer[O dao.Object]

**File**: `internal/servers/generic_server.go`

Type-parameterized CRUD implementation over any protobuf object type.

| Capability | Description |
|-----------|-------------|
| Service discovery | Uses protobuf reflection to find the gRPC `ServiceDesc`, request/response types, and Event payload field at build time |
| Metadata validation | Validates labels, annotations, name (DNS format), creator, tenant |
| Attribution | Determines creator from auth context via `AttributionLogic` |
| Tenancy | Determines visibility via `TenancyLogic` |
| Optimistic locking | Via `version` field comparison when `lock=true` |
| Field masks | Compiles and caches field mask paths; merges request onto stored object |
| Dry-run | `X-Dry-Run` HTTP header / `x-dry-run` gRPC metadata skips persistence |
| Equivalence checks | Prevents no-op writes — if the merged object is unchanged, no DB write and no event fires |
| Table name derivation | Proto message name → snake_case → pluralized (override with `SetTableName`) |

**Builder methods**:

| Method | Required | Description |
|--------|----------|-------------|
| `SetLogger(*slog.Logger)` | Yes | Structured logger |
| `SetService(string)` | Yes | From `privatev1.<Resources>_ServiceDesc.ServiceName` |
| `SetTableName(string)` | No | Override derived table name |
| `AddIgnoredFields(...any)` | No | Fields to skip in mapping/validation |
| `SetNotifier(events.Notifier)` | No | Event publisher |
| `SetRedactFunc(func(O) O)` | No | Redact sensitive fields before events |
| `SetAttributionLogic(auth.AttributionLogic)` | No | Creator determination |
| `SetTenancyLogic(auth.TenancyLogic)` | No | Tenant visibility |
| `SetMetricsRegisterer(prometheus.Registerer)` | No | Prometheus registration |

### GenericDAO[O Object]

**File**: `internal/database/dao/generic_dao.go`

Type-parameterized PostgreSQL data access.

- Table name derived by reflection: proto message name → snake_case → pluralized (`BareMetalInstance` → `bare_metal_instances`)
- Metadata columns extracted to dedicated DB columns; the rest serialized as JSON in `data`
- CEL filter translation for List queries
- Optimistic locking via `version` column
- Event callbacks for change notification

### GenericMapper[From, To]

**File**: `internal/servers/generic_mapper.go`

Field-by-field protobuf reflection copy/merge between types.

| Operation | Behavior |
|-----------|----------|
| `Copy(ctx, from, to)` | Overwrites target. Clears target fields not set in source. |
| `Merge(ctx, from, to)` | Only sets fields present in source. Skips absent fields. |

| Mode | Purpose |
|------|---------|
| Strict (`SetStrict(true)`) | Fails if source has a field with no matching target field. Used for `inMapper` (public→private). |
| Non-strict (default) | Silently skips unmatched fields. Used for `outMapper` (private→public). |

Fields matched by **protobuf name** — public and private types must have identically named fields for the mapper to work.

### Reconciler[O]

**File**: `internal/controllers/reconciler.go`

| Feature | Detail |
|---------|--------|
| Watch loop | Streams `Events.Watch` gRPC with a CEL event filter |
| Sync loop | Periodic full `List` RPC (default: 1 hour interval) |
| Fresh read | Re-reads object via `Get` before calling reconciler function (avoids stale data) |
| Separate process | Controller is a gRPC **client** — never touches the database directly |
| Default event filter | `has(event.<payloadFieldName>)` auto-derived from proto type reflection |

---

## 5. Data Model Philosophy

The codebase uses **protobuf as the single data model** — there is no DTO/domain/persistence split. This mirrors the Kubernetes pattern where a CRD Go type is simultaneously the API type, the etcd storage format, and the object controllers reconcile against.

```
privatev1.Thing  =  wire format (gRPC)
                 =  business logic type (server handlers operate on it directly)
                 =  persistence model (serialized as JSON into the `data` column)
```

`GenericDAO[O]` serializes the object as JSON in the `data` column. Metadata fields (`id`, `name`, `creation_timestamp`, `deletion_timestamp`, `finalizers`, `creator`, `tenant`, `project`, `labels`, `annotations`, `version`) get their own indexed columns for efficient querying.

**Two models, not one**: The public/private proto split means there are two proto types per resource. The `GenericMapper` converts between them at the public server boundary. Private fields never leak to the public API.

**Tradeoff**: No mapping boilerplate between three representation layers, but business logic is coupled to protobuf accessor API (getters, builders).

---

## 6. Interceptor Chain (Request Pipeline)

The gRPC server uses a layered interceptor chain. Order matters — earlier interceptors run first on the request and last on the response.

**Unary chain** (verified from `start_grpc_server_cmd.go`):

| # | Interceptor | Package | Purpose |
|---|------------|---------|---------|
| 1 | Panic recovery | `internal/recovery/` | Catches panics, returns `codes.Internal` |
| 2 | Metrics | `internal/metrics/` | Prometheus request/response metrics |
| 3 | Logging | `internal/logging/` | Structured request/response logging (configurable verbosity) |
| 4 | Validation | `internal/validation/` | `buf.build/go/protovalidate` on Create requests |
| 5 | Transaction | `internal/database/` | Begins lazy DB transaction, commits/rollbacks on return |
| 6 | Authentication | `internal/auth/` | JWT Bearer token validation, stores token in context |
| 7 | Authorization | `internal/auth/` | OPA/Rego policy evaluation, stores subject in context |
| 8 | JIT provisioning | `internal/auth/` | Just-in-time user record creation |
| 9 | Reference validation | `internal/servers/` | Auto-resolves and validates typed object references |

**Stream chain** is the same minus the transaction interceptor (transactions are unary-only).

**Transaction behavior**: Lazy — a real Postgres transaction (`pgx.Tx`) is only opened on the first `Query`/`Exec`. Requests that never touch the DB incur no transaction overhead. The `Notifier` writes events into the **same transaction** as the business-logic change (transactional outbox pattern).

**Service tap handler**: A `ServerInHandle` tap function rejects RPCs for disabled services before any interceptor runs. This is how feature gating works at the gRPC level — disabled services get `codes.Unavailable`, which grpc-gateway translates to HTTP 503.

---

## 7. Configuration & Dependency Wiring

### No DI Framework

The project uses **manual constructor injection via the builder pattern** everywhere. There is no Wire, dig, or fx.

```go
// Universal pattern — every component follows this:
thing, err := NewThing().
    SetLogger(logger).
    SetDep1(dep1).
    SetDep2(dep2).
    Build()
```

`Build()` validates all mandatory parameters and returns a descriptive error for any missing dependency (e.g., `"logger is mandatory"`).

### Server Configuration

Configuration is exclusively via **Cobra/pflag command-line flags**. The project does NOT use Viper, envconfig, or config files for the server binary.

Each subsystem registers its own flags:

| Subsystem | Registration | Key flags |
|-----------|-------------|-----------|
| Database | `database.AddFlags(flags)` | `--db-url`, `--db-url-file` |
| Network | `network.AddListenerFlags(flags, name, default)` | per-listener address |
| Auth | Custom flags | `--grpc-authn-trusted-token-issuers`, `--ca-file` |
| Vault | `vault.AddBaseFlags(flags)` | `--vault-endpoint`, `--vault-namespace` |
| Services | `services.RegisterFlags(flags)` | `--enable-caas`, `--enable-vmaas`, `--enable-bmaas`, `--enable-maas` |
| Logging | `logging.AddFlags(flags)` | log level and output control |

### Context Propagation

Request-scoped state is propagated via Go context:

| What | Into context | From context |
|------|-------------|--------------|
| Logger | `logging.LoggerIntoContext(ctx, logger)` | `logging.LoggerFromContext(ctx)` |
| Transaction | `database.TxIntoContext(ctx, tx)` | `database.TxFromContext(ctx)` |
| TxManager | `database.TxManagerIntoContext(ctx, mgr)` | `database.TxManagerFromContext(ctx)` |
| Auth token | `auth.ContextWithToken(ctx, token)` | `auth.TokenFromContext(ctx)` |
| Subject | (set by authz interceptor) | `auth.SubjectFromContext(ctx)` |

---

## 8. Error Handling

### Typed DAO Errors

**File**: `internal/database/dao/dao_errors.go`

| Error | Carries | Maps to gRPC |
|-------|---------|-------------|
| `ErrNotFound` | `IDs` | `codes.NotFound` |
| `ErrAlreadyExists` | `Kind`, `ID`, `Name`, `Reason`, `ConstraintName` | `codes.AlreadyExists` |
| `ErrConflict` | `RequestVersion`, `CurrentVersion` | `codes.Aborted` |
| `ErrDenied` | — | `codes.PermissionDenied` |
| `ErrImmutable` | `Fields` | `codes.InvalidArgument` |
| `ErrInvalidFilter` | — | `codes.InvalidArgument` |
| `ErrReference` | — | `codes.InvalidArgument` |
| `ErrInUse` | — | `codes.FailedPrecondition` |
| `ErrNotUnique` | — | `codes.FailedPrecondition` |
| `ErrValidation` | — | `codes.InvalidArgument` |
| `ErrDeadlock` | — | `codes.Aborted` |

### Custom PostgreSQL SQLSTATE Codes

The project defines custom error codes in the Z-class (user-defined) for database-level enforcement:

| Code | Meaning | Trigger |
|------|---------|---------|
| `Z0001` | Immutable column modified | `check_immutable_columns` trigger function |
| `Z0002` | Invalid reference | Reference validation trigger |
| `Z0003` | Object in use | Deletion protection trigger |
| `Z0004` | Custom uniqueness violation | Special uniqueness triggers |

### Error Mapping Rules

- **Ordinary name uniqueness** → `codes.AlreadyExists`
- **Singleton/single-default invariant** → `codes.FailedPrecondition` (slot is occupied; retry may succeed)
- **Constraint failures** → `codes.InvalidArgument`
- Match on gRPC status code, never on message text
- Wrap errors with `fmt.Errorf("context: %w", err)` to preserve error chain for `errors.As/Is`

---

## 9. Logging

### Library

Go standard `log/slog` with a JSON handler. NOT zap, zerolog, or logr (though bridged to logr for controller-runtime/klog compatibility).

### Setup

```go
logger, err := logging.NewLogger().
    SetWriter(os.Stdout).
    SetLevel(slog.LevelInfo).
    Build()
```

### Key Features

| Feature | Detail |
|---------|--------|
| JSON output | Custom handler with UTC RFC3339 timestamps |
| Context propagation | `LoggerIntoContext(ctx, logger)` / `LoggerFromContext(ctx)` |
| Sensitive data redaction | Fields prefixed with `!` are replaced with `***`: `slog.String("!secret", value)` |
| Protobuf serialization | Proto messages serialized via `protojson` in log output |
| Stack traces | Error values include stack traces |
| K8s bridge | `logr.FromSlogHandler(logger.Handler())` bridges to controller-runtime/klog |
| gRPC interceptor | `logging_interceptor.go` logs request/response with configurable headers/bodies |

### Usage Pattern

```go
logger.InfoContext(ctx, "Creating resource",
    slog.String("name", resource.GetMetadata().GetName()),
    slog.String("tenant", resource.GetMetadata().GetTenant()),
)
logger.ErrorContext(ctx, "Failed to create CRD",
    slog.Any("error", err),
    slog.String("hub", hubID),
)
```

---

## 10. Testing Standards

### Framework

**Ginkgo v2** + **Gomega** for all tests (both unit and integration).

```go
// Suite entry point (one per package)
func TestServers(t *testing.T) {
    RegisterFailHandler(Fail)
    RunSpecs(t, "Servers")
}

// Test structure
var _ = Describe("Things server", func() {
    It("should create a thing", func() {
        // ...
        Expect(err).ToNot(HaveOccurred())
        Expect(response.GetObject().GetId()).ToNot(BeEmpty())
    })
})
```

### Test Organization

| Type | Location | Runs against |
|------|----------|-------------|
| Unit tests | Colocated with source (`internal/servers/things_server_test.go`) | Containerized PostgreSQL |
| Integration tests | `it/it_things_test.go` | Live fulfillment-service + Keycloak |

### Unit Test Setup

Suite-wide setup in `BeforeSuite` creates:
- Logger
- Database container (podman/docker)
- Template database (all migrations applied once, then cloned per-test for speed)

Per-test setup in `BeforeEach`:
- Fresh database clone from template
- Transaction manager
- Test tenant inserted

### Mocking

- **Framework**: `go.uber.org/mock` (gomock) with `//go:generate mockgen` directives
- **Files**: Named `*_mock.go`, colocated with the code that uses them
- **Never hand-edit**: Generated mocks are regenerated by `go generate ./...`

### Test Helpers

**Location**: `internal/testing/`

| Helper | Purpose |
|--------|---------|
| `NewServer()` / `NewTLSServer()` | gRPC server on random local port |
| `MakeTokenObject()` / `MakeTokenString()` | Generate test JWT tokens |
| `MakeClaims()` / `MakeJwksObject()` | Test JWT claims and JWKS |
| `MatchLine()` | Custom Gomega matcher for regex on string slices |
| `*ServerFuncs` structs | Function-based server stubs with injectable `CreateFunc`, etc. |

### Database Testing

```go
// Template database pattern (in BeforeSuite):
container, err := database.NewContainer().
    SetLogger(logger).
    Build()
// Starts PostgreSQL in podman/docker and applies all migrations once.
// Each test gets a fast clone (CREATE DATABASE ... TEMPLATE), no migration replay.
```

### Validation Commands

```bash
# Linting (golangci-lint + ruff for Python)
uv run dev.py lint

# Build check
go build ./cmd/fulfillment-service ./cmd/osac

# Unit tests (all packages under internal/)
ginkgo run -r internal

# Focused package test
ginkgo run internal/servers

# Integration tests (requires running service + Keycloak)
make -C ../osac-installer test PLATFORM=kind PROFILE=dev NS=osac SUITE=fulfillment
```

### Linter Configuration

**File**: `.golangci.yml` — golangci-lint v2

Enabled linters above default: `depguard` (bans `math/rand` v1), `errorlint`, `ginkgolinter`, `gocyclo`, `gosec`, `misspell`, `nolintlint`.

Exemptions:
- `_test.go` files exempt from `gosec` and `errcheck`
- `internal/testing/`, `it/`, `database_container.go` exempt from `gosec`, `errcheck`, `gocyclo`
- `_mock.go` files exempt from `unused`
- Dot imports whitelisted for Ginkgo/Gomega

---

## 11. Build & Deployment

### Container Image

**File**: `Containerfile` (not Dockerfile)

| Property | Value |
|----------|-------|
| Build base | `registry.access.redhat.com/ubi10/go-toolset:1.26.7-*` |
| Runtime base | `registry.access.redhat.com/ubi10-minimal:10.2-*` |
| Build context | Mono-repo root (`..` from service dir) — copies sibling modules |
| Binaries | Both `fulfillment-service` and `osac` at `/usr/local/bin` |
| User | 1001 (non-root) |
| Debug target | `--target runtime-debug` adds Delve debugger |
| Version injection | ldflags into `internal/version.id` |

### GoReleaser

**File**: `.goreleaser.yaml`

- Two binaries: `osac` (from `./cmd/osac`) and `fulfillment-service` (from `./cmd/fulfillment-service`)
- Platforms: linux/darwin/windows, amd64/arm64
- `CGO_ENABLED=0`
- Keyless cosign signing via Fulcio/Rekor

### Helm Chart

**Location**: `charts/service/`

Separate Deployments per component:

| Component | Deployment | Binary command |
|-----------|-----------|---------------|
| gRPC server | `grpc-server/deployment.yaml` | `fulfillment-service start grpc-server` |
| REST gateway | `rest-gateway/deployment.yaml` | `fulfillment-service start rest-gateway` |
| Controller | `controller/deployment.yaml` | `fulfillment-service start controller` |
| Console proxy | `console-proxy/deployment.yaml` | `fulfillment-service start console-proxy` |
| Ingress proxy | `ingress-proxy/deployment.yaml` | Envoy sidecar |

Each component has its own `Service` and cert-manager `Certificate`.

### Health Checks

Standard gRPC health protocol (`google.golang.org/grpc/health`).

`Aggregator` in `internal/health/` tracks multiple component statuses — reports `SERVING` only when ALL components are healthy. Initially `NOT_SERVING` until all components report in.

**REST health endpoint**: `GET /healthz` on the REST gateway, backed by a gRPC health client call to the gRPC server.

---

## 12. Event System

### Transactional Outbox

Events are written to the `notifications` table **in the same database transaction** as the business-logic change. This guarantees atomicity: an event is never sent for a change that rolls back, and a committed change never fails to produce a notification row.

Flow:
1. `GenericServer.Create/Update/Delete` → `dao.GenericDAO` writes to the resource table
2. `GenericServer` calls `events.Notifier.Notify` → writes to `notifications` table + `pg_notify('events', id)`
3. Both writes are in the same transaction (from `TxInterceptor`)
4. Transaction commits on successful handler return

### PostgreSQL LISTEN/NOTIFY

`pg_notify` is a **low-latency hint**, not a delivery guarantee. The notification carries only the row `id` — subscribers re-read the full payload from the `notifications` table. Missed notifications (connection drop, subscriber not listening) are caught by the sync loop.

### Dual-Loop Correctness

Every reconciler runs two concurrent loops:

| Loop | Mechanism | Latency | Guarantee |
|------|-----------|---------|-----------|
| Watch | `Events.Watch` gRPC stream | Low (instant notification) | Best-effort (can miss events) |
| Sync | Periodic `List` RPC | High (default: 1 hour) | Correctness fallback (heals missed events) |

This mirrors Kubernetes: informer watches are an optimization; relists are the correctness guarantee.

### Event Payload Discovery

`GenericServer` uses protobuf reflection at build time to find the correct oneof field in `Event` for its object type. The payload is set via `event.ProtoReflect().Set(payloadField, ...)`. No manual type-switch needed per resource — but the Event oneof entry in `event_type.proto` must exist (Layer 1 of the checklist).

---

## 13. Authentication & Authorization

### Authentication

**Interceptor**: `internal/auth/grpc_authn_interceptor.go`

- Extracts Bearer token from gRPC `authorization` metadata
- Validates JWT using `JwtValidator` interface
- JWKS caching via `JwksCache` (auto-refresh from token issuers)
- Configurable anonymous methods via regex patterns (`--grpc-authn-anonymous-methods`)
- Validated token stored in context for downstream use

### Authorization

**Interceptor**: `internal/auth/grpc_authz_interceptor.go`

- **Open Policy Agent (OPA)** with an embedded Rego policy (`internal/auth/policies/authz.rego`)
- Constructs `Subject` from JWT claims and stores in context
- Special roles:

| Role | Source | Privilege |
|------|--------|-----------|
| Emergency SA | External data file | Full bypass |
| Admin SA | `service-account-osac-admin`, `service-account-osac-controller` | Full access |
| Admin group | `admins` group claim | Full access |
| Tenant admin | `tenant-admin` group claim | Tenant-scoped admin |
| Tenant IdP manager | `tenant-admin` or `tenant-idp-manager` group | IdP configuration |

### Multi-Tenancy

**Interfaces**: `TenancyLogic` and `AttributionLogic` in `internal/auth/`

- Tenant extracted from JWT `organization` claim (supports array format and Keycloak object format)
- `TenancyLogic` controls visibility — which objects a subject can see based on their tenant membership
- `AttributionLogic` determines the creator identity for new objects
- All public resource paths enforce tenant authorization and attribution — this is an invariant, not optional

### JIT User Provisioning

**Interceptor**: `internal/auth/grpc_jit_provisioning_interceptor.go`

On first authenticated request, automatically creates a user record. Eliminates explicit user registration before API access.

---

## Appendix: Key Dependencies

| Category | Library | Import |
|----------|---------|--------|
| gRPC | `google.golang.org/grpc` | Server + client |
| Protobuf | `google.golang.org/protobuf` | Types + reflection |
| REST gateway | `github.com/grpc-ecosystem/grpc-gateway/v2` | HTTP→gRPC proxy |
| Proto validation | `buf.build/go/protovalidate` | Field validation |
| PostgreSQL | `github.com/jackc/pgx/v5` | Database driver + pool |
| Migrations | `github.com/golang-migrate/migrate/v4` | Schema migrations |
| Kubernetes | `k8s.io/client-go`, `sigs.k8s.io/controller-runtime` | K8s API access |
| CLI | `github.com/spf13/cobra` + `pflag` | Command framework |
| Auth (JWT) | `github.com/golang-jwt/jwt/v5`, `github.com/lestrrat-go/jwx/v3` | Token validation |
| Auth (policy) | `github.com/open-policy-agent/opa` | Rego authorization |
| Secrets | `github.com/hashicorp/vault/api`, `github.com/zalando/go-keyring` | Secret management |
| Metrics | `github.com/prometheus/client_golang` | Prometheus |
| Filtering | `github.com/google/cel-go` | CEL expression evaluation |
| Testing | `github.com/onsi/ginkgo/v2`, `github.com/onsi/gomega` | Test framework |
| Mocking | `go.uber.org/mock` | Mock generation |

---

## Appendix: Naming Convention Summary

| What | Convention | Example |
|------|-----------|---------|
| Proto type file | `<type>_type.proto` | `cluster_type.proto` |
| Proto service file | `<type>s_service.proto` | `clusters_service.proto` |
| Migration file | `<N>_create_<resources>_tables.up.sql` | `95_create_volumes_tables.up.sql` |
| Public server file | `<resources>_server.go` | `clusters_server.go` |
| Private server file | `private_<resources>_server.go` | `private_clusters_server.go` |
| Public server struct | `<Resources>Server` | `ClustersServer` |
| Private server struct | `Private<Resources>Server` | `PrivateClustersServer` |
| Public builder | `New<Resources>Server()` | `NewClustersServer()` |
| Private builder | `NewPrivate<Resources>Server()` | `NewPrivateClustersServer()` |
| Reconciler function | `<resource>_reconciler_function.go` | `cluster_reconciler_function.go` |
| Reconciler directory | `internal/controllers/<resource>/` | `internal/controllers/cluster/` |
| Mock files | `*_mock.go` | `things_client_mock.go` |
| Test files | `*_test.go` (colocated) | `clusters_server_test.go` |
| Integration tests | `it/it_<resource>_<scenario>_test.go` | `it/it_clusters_create_test.go` |
| DB table | snake_case pluralized | `bare_metal_instances` |
| Archived table | `archived_<table>` | `archived_bare_metal_instances` |
| gRPC service name | Plural, no `Service` suffix | `Clusters`, `BareMetalInstances` |
| REST URL path (this service) | `/api/v1/<resources>` | `/api/v1/clusters` |
| REST URL path (OSAC public ref) | `/api/fulfillment/v1/<resources>` | `/api/fulfillment/v1/clusters` |
| REST URL path (OSAC private ref) | `/api/private/v1/<resources>` | `/api/private/v1/clusters` |
| Enum values | `TYPE_PREFIX_VALUE` | `CLUSTER_STATE_READY` |
| Proto package (this service) | `ai_grid.v1` | — |
| Proto package (OSAC public ref) | `osac.public.v1` | — |
| Proto package (OSAC private ref) | `osac.private.v1` | — |
| Go import alias (this service) | `aigridv1` | `aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"` |
| Go import alias (OSAC public) | `publicv1` | `publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"` |
| Go import alias (OSAC private) | `privatev1` | `privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"` |
