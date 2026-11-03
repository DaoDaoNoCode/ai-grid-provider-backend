// Package servers implements the Connect handlers for ai_grid.v1 resource
// services (Clusters, Tenants, Models, Subscriptions, and AiGrid Ping).
//
// Construction uses builders (NewXServer → Set… → Build), matching the public
// client package. Infra clients under internal/keycloak and internal/acmsearch
// use New + Config instead; both idioms are intentional.
//
// Handlers log with log/slog *Context methods on request paths when a context
// is available. SetLogger is optional; slog.Default() when unset.
package servers
