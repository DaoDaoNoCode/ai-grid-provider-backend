package validation

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

type validationCase struct {
	Schema      string            `json:"schema"`
	Name        string            `json:"name"`
	Input       json.RawMessage   `json:"input"`
	FieldErrors map[string]string `json:"fieldErrors"`
}

// TestSharedValidationCases runs cases.json, the cases the console frontend's
// zod schemas also run (the console's frontend/src/validation/cases.test.ts).
func TestSharedValidationCases(t *testing.T) {
	raw, err := os.ReadFile("cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Cases []validationCase }
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range doc.Cases {
		if slices.Contains(FrontendOnly, c.Schema) {
			continue
		}
		v, ok := Schemas[c.Schema]
		if !ok {
			t.Errorf("%s: no Go validator for schema %q", c.Name, c.Schema)
			continue
		}
		got := v(c.Input)
		if len(got) == 0 && len(c.FieldErrors) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.FieldErrors) {
			t.Errorf("%s/%s: got %v, want %v", c.Schema, c.Name, got, c.FieldErrors)
		}
	}
}
