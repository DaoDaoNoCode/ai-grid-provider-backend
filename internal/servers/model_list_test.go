package servers

import (
	"testing"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

func TestApplyModelOrder(t *testing.T) {
	t.Parallel()
	items := []*aigridv1.Model{
		{Id: "b"},
		{Id: "a"},
		{Id: "c"},
	}
	if err := applyModelOrder(items, nil); err != nil {
		t.Fatalf("default order: %v", err)
	}
	if items[0].GetId() != "a" || items[2].GetId() != "c" {
		t.Fatalf("default = %v", idsOf(items))
	}

	desc := "-metadata.name"
	if err := applyModelOrder(items, &desc); err != nil {
		t.Fatalf("desc order: %v", err)
	}
	if items[0].GetId() != "c" || items[2].GetId() != "a" {
		t.Fatalf("desc = %v", idsOf(items))
	}

	bad := "status.url"
	if err := applyModelOrder(items, &bad); err == nil {
		t.Fatal("expected invalid order error")
	}
}
