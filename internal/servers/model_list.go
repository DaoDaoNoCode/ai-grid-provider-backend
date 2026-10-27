package servers

import (
	"sort"
	"strings"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

const modelOrderAllowlistHint = "id, -id, metadata.name, -metadata.name"

func applyModelOrder(items []*aigridv1.Model, order *string) error {
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
			a, b := modelSortID(items[i]), modelSortID(items[j])
			if desc {
				return a > b
			}
			return a < b
		})
		return nil
	default:
		shown := ""
		if order != nil {
			shown = *order
		}
		return connect.Errorf(connect.CodeInvalidArgument,
			"order %q is not supported; allowed values: %s", shown, modelOrderAllowlistHint)
	}
}

func modelSortID(m *aigridv1.Model) string {
	if m == nil {
		return ""
	}
	if id := m.GetId(); id != "" {
		return id
	}
	if m.GetMetadata() != nil {
		return m.GetMetadata().GetName()
	}
	return ""
}
