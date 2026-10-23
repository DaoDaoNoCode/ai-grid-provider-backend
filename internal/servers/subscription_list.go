package servers

import (
	"sort"
	"strings"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

// Allowed Subscriptions List order values. Empty/unset means id ascending.
const subscriptionOrderAllowlistHint = "id, -id, priority, -priority, metadata.name, -metadata.name"

// applySubscriptionOrder sorts items by the allowlisted order field.
// Nil or empty order sorts by id ascending. Unknown values are InvalidArgument.
// When sorting by priority, id is the tie-breaker for a stable order.
func applySubscriptionOrder(items []*aigridv1.Subscription, order *string) error {
	key := ""
	if order != nil {
		key = strings.TrimSpace(*order)
	}
	desc := false
	if strings.HasPrefix(key, "-") {
		desc = true
		key = strings.TrimPrefix(key, "-")
	}
	switch key {
	case "", "id", "metadata.name":
		sort.SliceStable(items, func(i, j int) bool {
			a, b := subscriptionSortID(items[i]), subscriptionSortID(items[j])
			if desc {
				return a > b
			}
			return a < b
		})
		return nil
	case "priority":
		sort.SliceStable(items, func(i, j int) bool {
			pa, pb := subscriptionPriority(items[i]), subscriptionPriority(items[j])
			if pa != pb {
				if desc {
					return pa > pb
				}
				return pa < pb
			}
			return subscriptionSortID(items[i]) < subscriptionSortID(items[j])
		})
		return nil
	default:
		shown := ""
		if order != nil {
			shown = *order
		}
		return connect.Errorf(connect.CodeInvalidArgument,
			"order %q is not supported; allowed values: %s", shown, subscriptionOrderAllowlistHint)
	}
}

func subscriptionSortID(sub *aigridv1.Subscription) string {
	if sub == nil {
		return ""
	}
	if id := sub.GetId(); id != "" {
		return id
	}
	if sub.GetMetadata() != nil {
		return sub.GetMetadata().GetName()
	}
	return ""
}

func subscriptionPriority(sub *aigridv1.Subscription) int32 {
	if sub == nil || sub.GetSpec() == nil {
		return 0
	}
	return sub.GetSpec().GetPriority()
}
