package servers

import (
	"context"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

func TestSubscriptionsServerBuildAndPlaceholder(t *testing.T) {
	t.Parallel()

	srv, err := NewSubscriptionsServer().Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	ctx := context.Background()

	list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.GetSize() != 0 || list.GetTotal() != 0 {
		t.Fatalf("List size/total = %d/%d, want 0/0", list.GetSize(), list.GetTotal())
	}
	if len(list.GetItems()) != 0 {
		t.Fatalf("List items = %d, want 0", len(list.GetItems()))
	}

	_, err = srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "free-subscription"})
	assertConnectCode(t, err, connect.CodeUnimplemented)
	_, err = srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{
		Object: &aigridv1.Subscription{Id: "free-subscription"},
	})
	assertConnectCode(t, err, connect.CodeUnimplemented)
	_, err = srv.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object: &aigridv1.Subscription{Id: "free-subscription"},
	})
	assertConnectCode(t, err, connect.CodeUnimplemented)
	_, err = srv.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "free-subscription"})
	assertConnectCode(t, err, connect.CodeUnimplemented)
}
