// Package validation holds the PoC's request rules, checked against a body in
// the BFF's JSON shape. Only the draft fake uses it (the BFF forwards bodies
// unchecked), and TestSharedValidationCases runs it against cases.json, the
// cases the console frontend's zod schemas also pass, so the two can't drift.
//
// It is a copy of the console's bff/internal/validation (ac80c25), and
// cases.json a copy of the console's mock/validation/cases.json: the console
// keeps both for its frontend tests, so the copies must stay in sync until the
// console no longer needs them.
//
// Models and Subscriptions validation lived here while those areas were draft;
// ai_grid.v1 now owns them, so createSubscription/updateSubscription (and
// organization/model) are FrontendOnly until the console drops its copy. The
// Go zod-mirror helpers that backed those validators were removed with them.
package validation

import "encoding/json"

// Schemas maps each schema name in internal/fakeinfra/draft/validation/cases.json that the Go
// side checks to its rule; FrontendOnly lists the rest.
// Every rule returns field errors keyed by JSON path, the last message per
// path (as the PoC keeps them), and nil when the body is valid.
var Schemas = map[string]func(json.RawMessage) map[string]string{}

// FrontendOnly are the schemas in cases.json that only the frontend's zod
// rules run. Organization/model were always frontend-only here; subscription
// schemas moved here when draft Subscriptions were retired to ai_grid.v1.
var FrontendOnly = []string{"organization", "model", "createSubscription", "updateSubscription"}
