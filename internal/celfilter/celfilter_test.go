package celfilter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

func TestEmptyFilterMatchesEverything(t *testing.T) {
	for _, src := range []string{"", "  \n"} {
		f, err := Compile(src, &aigridv1.Subscription{})
		if err != nil || f != nil {
			t.Fatalf("%q = %v, %v; want no filter", src, f, err)
		}
		if ok, err := (*Filter)(nil).Match(context.Background(), &aigridv1.Subscription{}, "x", time.Now()); !ok || err != nil {
			t.Errorf("nil filter = %v, %v", ok, err)
		}
	}
}

func TestCompileAndMatchSubscription(t *testing.T) {
	f, err := Compile(`this.id == "engineering"`, &aigridv1.Subscription{})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := f.Match(context.Background(), &aigridv1.Subscription{Id: "engineering"}, "engineering", time.Now())
	if !ok || err != nil {
		t.Fatalf("match engineering = %v, %v", ok, err)
	}
	ok, err = f.Match(context.Background(), &aigridv1.Subscription{Id: "other"}, "other", time.Now())
	if ok || err != nil {
		t.Fatalf("match other = %v, %v", ok, err)
	}
}

func TestInvalidExpression(t *testing.T) {
	_, err := Compile(`this.id ==`, &aigridv1.Subscription{})
	assertConnectCodeMsg(t, err, connect.CodeInvalidArgument, "not a valid CEL expression")
}

func TestNonBoolExpression(t *testing.T) {
	_, err := Compile(`this.id`, &aigridv1.Subscription{})
	assertConnectCodeMsg(t, err, connect.CodeInvalidArgument, "must evaluate to a bool")
}

func TestMissingMapKeyFails(t *testing.T) {
	f, err := Compile(`this.metadata.labels["missing"] == "x"`, &aigridv1.Subscription{})
	if err != nil {
		t.Fatal(err)
	}
	sub := &aigridv1.Subscription{
		Id:       "s1",
		Metadata: &aigridv1.Metadata{Name: "s1", Labels: map[string]string{}},
	}
	_, err = f.Match(context.Background(), sub, "s1", time.Now())
	assertConnectCodeMsg(t, err, connect.CodeInvalidArgument, "failed on 's1'")
}

func TestNowIsTheCallersTime(t *testing.T) {
	f, err := Compile(`now == timestamp("2026-10-15T12:00:00Z")`, &aigridv1.Subscription{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ok, err := f.Match(context.Background(), &aigridv1.Subscription{Id: "s"}, "s", at)
	if !ok || err != nil {
		t.Fatalf("at exact = %v, %v", ok, err)
	}
	ok, err = f.Match(context.Background(), &aigridv1.Subscription{Id: "s"}, "s", at.Add(time.Second))
	if ok || err != nil {
		t.Fatalf("at +1s = %v, %v", ok, err)
	}
}

func TestMatchStopsWhenTheContextEnds(t *testing.T) {
	f, err := Compile(`this.id.contains("a")`, &aigridv1.Subscription{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.Match(ctx, &aigridv1.Subscription{Id: "a"}, "a", time.Now())
	assertConnectCodeMsg(t, err, connect.CodeCanceled, "")
}

func assertConnectCodeMsg(t *testing.T, err error, want connect.Code, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %v", want)
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v (%T), want *connect.Error %v", err, err, want)
	}
	if ce.Code() != want {
		t.Fatalf("code = %v, want %v (%v)", ce.Code(), want, err)
	}
	if substr != "" && !strings.Contains(ce.Message(), substr) {
		t.Fatalf("message %q missing %q", ce.Message(), substr)
	}
}
