package servers

import (
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

func TestApplySubscriptionOrder(t *testing.T) {
	t.Parallel()

	mk := func(id string, priority int32) *aigridv1.Subscription {
		return &aigridv1.Subscription{
			Id:       id,
			Metadata: &aigridv1.Metadata{Name: id},
			Spec:     &aigridv1.SubscriptionSpec{Priority: priority},
		}
	}

	ids := func(items []*aigridv1.Subscription) []string {
		out := make([]string, len(items))
		for i, s := range items {
			out[i] = s.GetId()
		}
		return out
	}

	items := []*aigridv1.Subscription{mk("c", 5), mk("a", 20), mk("b", 5)}
	if err := applySubscriptionOrder(items, nil); err != nil {
		t.Fatal(err)
	}
	if got := ids(items); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("default order = %v", got)
	}

	items = []*aigridv1.Subscription{mk("c", 5), mk("a", 20), mk("b", 5)}
	order := "priority"
	if err := applySubscriptionOrder(items, &order); err != nil {
		t.Fatal(err)
	}
	if got := ids(items); got[0] != "b" || got[1] != "c" || got[2] != "a" {
		// priority 5 first (b before c by id tie-break), then a at 20
		t.Fatalf("priority asc = %v", got)
	}

	items = []*aigridv1.Subscription{mk("c", 5), mk("a", 20), mk("b", 5)}
	order = "-priority"
	if err := applySubscriptionOrder(items, &order); err != nil {
		t.Fatal(err)
	}
	if got := ids(items); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("priority desc = %v", got)
	}

	items = []*aigridv1.Subscription{mk("c", 1), mk("a", 1)}
	order = "-id"
	if err := applySubscriptionOrder(items, &order); err != nil {
		t.Fatal(err)
	}
	if got := ids(items); got[0] != "c" || got[1] != "a" {
		t.Fatalf("-id = %v", got)
	}

	bad := "created_at"
	err := applySubscriptionOrder(items, &bad)
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}
