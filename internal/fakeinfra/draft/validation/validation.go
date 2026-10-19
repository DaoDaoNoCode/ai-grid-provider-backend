// Package validation holds the PoC's request rules, checked against a body in
// the BFF's JSON shape. Only the draft fake uses it (the BFF forwards bodies
// unchecked), and TestSharedValidationCases runs it against cases.json, the
// cases the console frontend's zod schemas also pass, so the two can't drift.
//
// It is a copy of the console's bff/internal/validation (ac80c25), and
// cases.json a copy of the console's mock/validation/cases.json: the console
// keeps both for its frontend tests, so the copies must stay in sync until the
// console no longer needs them.
package validation

import "encoding/json"

// Schemas maps each schema name in internal/fakeinfra/draft/validation/cases.json that the Go
// side checks to its rule; FrontendOnly lists the rest.
// Every rule returns field errors keyed by JSON path, the last message per
// path (as the PoC keeps them), and nil when the body is valid.
var Schemas = map[string]func(json.RawMessage) map[string]string{
	"createSubscription": CreateSubscription,
	"updateSubscription": UpdateSubscription,
}

// FrontendOnly are the schemas in internal/fakeinfra/draft/validation/cases.json that only the
// frontend's zod rules run: organizations and models are validated by the
// backend that owns them (PR 12's fake), not by this package.
var FrontendOnly = []string{"organization", "model"}
