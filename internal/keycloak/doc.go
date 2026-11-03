// Package keycloak is a small Keycloak Admin API client used by the Tenants
// service: Organizations CRUD in KEYCLOAK_REALM, plus the admin password-grant
// token endpoint.
//
// Construct with New(Config). This New+Config idiom is the convention for
// infra clients here (see also acmsearch); Connect servers and the public
// client package use builders instead.
package keycloak
