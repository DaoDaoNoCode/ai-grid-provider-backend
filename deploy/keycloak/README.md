# Local Keycloak (tenant API playground)

Podman-first Keycloak for developing against the Tenants API without Kind or
OpenShift. Mirrors the Kind `keycloak-init` Job: realm `ai-grid` with
Organizations enabled and sample orgs `tenant-a` / `tenant-b`.

## Quick start

```sh
make keycloak-up          # start on :8180 and seed
make run                  # API uses KEYCLOAK_URL=http://127.0.0.1:8180
# curl http://localhost:8081/api/v1/tenants
make keycloak-seed        # re-seed (idempotent) if you wiped orgs
make keycloak-down        # stop and remove the container
```

Admin console: http://127.0.0.1:8180 (admin / admin).

Port **8180** avoids clashing with local API gRPC (`:8080`) and REST (`:8081`).

## Overrides

| Variable | Default | Purpose |
| --- | --- | --- |
| `KEYCLOAK_PORT` | `8180` | Host port |
| `KEYCLOAK_URL` | `http://127.0.0.1:8180` | Admin API base (seed / `make run`) |
| `KEYCLOAK_IMAGE` | `quay.io/keycloak/keycloak:26.3` | Image tag (Kind parity) |
| `CONTAINER_NAME` | `ai-grid-keycloak` | Podman/docker name |
| `SKIP_SEED` | `0` | Set `1` on up to skip seeding |
| `TENANTS` | `tenant-a tenant-b` | Org names for seed |

```sh
SKIP_SEED=1 make keycloak-up
TENANTS="acme globex" make keycloak-seed
KEYCLOAK_PORT=8280 KEYCLOAK_URL=http://127.0.0.1:8280 make keycloak-up
```
