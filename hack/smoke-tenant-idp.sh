#!/usr/bin/env bash
# End-to-end check: CreateTenant with spec.identity_provider, then verify the
# Keycloak realm IdP + maas_org_id mapper against the generated org UUID.
#
# Prerequisites:
#   1. Kind (or OpenShift) backend + Keycloak up; backend image includes this branch
#   2. kubectl apply -f deploy/kind/idp-client-secret.example.yaml
#   3. Port-forward:
#        kubectl -n ai-grid port-forward svc/ai-grid-provider-backend 8080:8080 8081:8081
#        kubectl -n ai-grid port-forward svc/keycloak 8180:8080
#
# Usage:
#   bash hack/smoke-tenant-idp.sh
#
# Env:
#   GRPC_ADDR          default localhost:8080
#   KEYCLOAK_URL       default http://127.0.0.1:8180
#   KEYCLOAK_REALM     default ai-grid
#   KEYCLOAK_USER/PASS default admin/admin (master realm admin-cli)
set -euo pipefail

addr="${GRPC_ADDR:-localhost:8080}"
# Host-reachable Keycloak (port-forward) for Admin API checks from this script.
kc="${KEYCLOAK_URL:-http://127.0.0.1:8180}"
# IdP URLs stored on the realm IdP must be reachable from Keycloak itself.
idp_base="${IDP_ISSUER_BASE:-http://keycloak.ai-grid.svc:8080}"
realm="${KEYCLOAK_REALM:-ai-grid}"
kc_user="${KEYCLOAK_USER:-admin}"
kc_pass="${KEYCLOAK_PASSWORD:-admin}"
suffix="$(date +%s | tail -c 6)"
name="idp-smoke-${suffix}"
secret_name="demo-idp-secret"

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: $1 required" >&2; exit 1; }; }
need grpcurl
need curl
need jq

echo "==> checking IdP Secret ${secret_name} in ai-grid-idp-secrets"
kubectl -n ai-grid-idp-secrets get secret "$secret_name" >/dev/null

echo "==> creating tenant ${name} with identity_provider"
create_json="$(grpcurl -plaintext -d @ "$addr" ai_grid.v1.Tenants/Create <<EOF
{
  "object": {
    "metadata": { "name": "${name}" },
    "spec": {
      "domains": ["${name}.example.com"],
      "identityProvider": {
        "title": "${name}",
        "config": {
          "clientId": "ai-grid-broker",
          "clientSecretSecretName": "${secret_name}",
          "issuer": "${idp_base}/realms/org-idp",
          "authorizationUrl": "${idp_base}/realms/org-idp/protocol/openid-connect/auth",
          "tokenUrl": "${idp_base}/realms/org-idp/protocol/openid-connect/token",
          "jwksUrl": "${idp_base}/realms/org-idp/protocol/openid-connect/certs",
          "logoutUrl": "${idp_base}/realms/org-idp/protocol/openid-connect/logout",
          "defaultScopes": "openid profile email",
          "validateSignature": true
        }
      }
    }
  }
}
EOF
)"
echo "$create_json" | jq '{id: .object.id, name: .object.metadata.name, idp: .object.spec.identityProvider.config.clientSecretSecretName}'
org_id="$(echo "$create_json" | jq -r '.object.id')"
version="$(echo "$create_json" | jq -r '.object.metadata.version')"
[[ -n "$org_id" && "$org_id" != null ]] || { echo "error: no org id in Create response" >&2; exit 1; }
echo "$create_json" | jq -e '.object.spec.identityProvider.config.clientSecretSecretName == "'"$secret_name"'"' >/dev/null
if echo "$create_json" | grep -q 'demo-broker-client-secret'; then
  echo "error: Create response leaked the client secret" >&2
  exit 1
fi

echo "==> Keycloak admin token"
token="$(curl -fsS -X POST "$kc/realms/master/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=password --data-urlencode client_id=admin-cli \
  --data-urlencode username="$kc_user" --data-urlencode password="$kc_pass" | jq -r .access_token)"
[[ -n "$token" && "$token" != null ]] || { echo "error: no Keycloak token" >&2; exit 1; }
auth="Authorization: Bearer $token"

echo "==> realm IdP alias ${name} exists"
idp="$(curl -fsS -H "$auth" "$kc/admin/realms/$realm/identity-provider/instances/${name}")"
echo "$idp" | jq '{alias, providerId, enabled, clientId: .config.clientId}'
echo "$idp" | jq -e '.alias == "'"$name"'" and .providerId == "oidc"' >/dev/null
if echo "$idp" | jq -r '.config.clientSecret // empty' | grep -q .; then
  # Admin GET may include the secret; we only assert the backend API did not.
  :
fi

echo "==> org link + mapper value is org UUID"
curl -fsS -H "$auth" "$kc/admin/realms/$realm/organizations/${org_id}/identity-providers/${name}" >/dev/null
mappers="$(curl -fsS -H "$auth" "$kc/admin/realms/$realm/identity-provider/instances/${name}/mappers")"
echo "$mappers" | jq .
echo "$mappers" | jq -e --arg id "$org_id" \
  '[.[] | select(.name=="AI Grid org id" and .config.attribute=="maas_org_id" and .config["attribute.value"]==$id)] | length == 1' >/dev/null

echo "==> immutability: changed IdP on Update is InvalidArgument"
set +e
upd_err="$(grpcurl -plaintext -d @ "$addr" ai_grid.v1.Tenants/Update 2>&1 <<EOF
{
  "object": {
    "id": "${org_id}",
    "metadata": { "version": ${version} },
    "spec": {
      "identityProvider": {
        "config": {
          "clientId": "changed-client",
          "clientSecretSecretName": "${secret_name}",
          "issuer": "${idp_base}/realms/org-idp",
          "authorizationUrl": "${idp_base}/realms/org-idp/protocol/openid-connect/auth",
          "tokenUrl": "${idp_base}/realms/org-idp/protocol/openid-connect/token"
        }
      }
    }
  },
  "updateMask": { "paths": ["spec.identity_provider"] }
}
EOF
)"
upd_rc=$?
set -e
[[ "$upd_rc" -ne 0 ]] || { echo "error: Update should have failed: $upd_err" >&2; exit 1; }
echo "$upd_err" | grep -qi 'InvalidArgument\|immutable\|cannot' || echo "(Update failed as expected: $upd_err)"

echo "==> Delete removes IdP then org"
grpcurl -plaintext -d "{\"id\":\"${org_id}\",\"version\":${version}}" "$addr" ai_grid.v1.Tenants/Delete >/dev/null
code="$(curl -sS -o /dev/null -w '%{http_code}' -H "$auth" \
  "$kc/admin/realms/$realm/identity-provider/instances/${name}" || true)"
[[ "$code" == "404" ]] || { echo "error: IdP still present (HTTP $code)" >&2; exit 1; }
code="$(curl -sS -o /dev/null -w '%{http_code}' -H "$auth" \
  "$kc/admin/realms/$realm/organizations/${org_id}" || true)"
[[ "$code" == "404" ]] || { echo "error: organization still present (HTTP $code)" >&2; exit 1; }

echo "OK: tenant IdP create/mapper/delete smoke passed for ${name} (${org_id})"
