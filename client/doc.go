// Package client is a reusable gRPC client for ai_grid.v1, used by smoke and
// integration helpers.
//
// Construction uses a builder (NewBuilder → SetAddress / SetPlaintext → Build),
// matching the Connect server builders in internal/servers. Infra clients
// (keycloak, acmsearch) use New + Config instead.
package client
