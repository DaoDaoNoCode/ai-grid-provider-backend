package servers

import (
	connect "connectrpc.com/connect/v2"
)

// validateListLimit rejects a negative List limit. A nil or non-negative
// limit is accepted (nil / omitted means no page size cap).
func validateListLimit(limit *int32) error {
	if limit != nil && *limit < 0 {
		return connect.Errorf(connect.CodeInvalidArgument, "limit must not be negative")
	}
	return nil
}

// applyListWindow returns the offset/limit window over items and the full
// collection size. A nil offset starts at 0; a nil limit returns the rest of
// the collection; a limit of 0 returns an empty page.
func applyListWindow[T any](items []T, offset, limit *int32) (page []T, total int32) {
	total = int32(len(items))
	start := 0
	if offset != nil && *offset > 0 {
		start = int(*offset)
		if start > len(items) {
			start = len(items)
		}
	}
	end := len(items)
	if limit != nil && *limit >= 0 {
		end = start + int(*limit)
		if end > len(items) {
			end = len(items)
		}
		if *limit == 0 {
			end = start
		}
	}
	return items[start:end], total
}
