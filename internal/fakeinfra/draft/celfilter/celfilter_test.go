package celfilter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

func TestEmptyFilterMatchesEverything(t *testing.T) {
	for _, src := range []string{"", "  \n"} {
		f, err := Compile(src, &aigriddraftv1.Tenant{})
		if err != nil || f != nil {
			t.Fatalf("%q = %v, %v; want no filter", src, f, err)
		}
		if ok, err := f.Match(context.Background(), &aigriddraftv1.Tenant{}, "x", time.Now()); !ok || err != nil {
			t.Errorf("nil filter = %v, %v", ok, err)
		}
	}
}

func TestNowIsTheCallersTime(t *testing.T) {
	f, err := Compile(`now == timestamp("2026-10-15T12:00:00Z")`, &aigriddraftv1.Model{})
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[time.Time]bool{time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC): true, time.Date(2026, 10, 15, 12, 0, 1, 0, time.UTC): false} {
		if ok, err := f.Match(context.Background(), &aigriddraftv1.Model{}, "ns/m", at); ok != want || err != nil {
			t.Errorf("at %v = %v, %v", at, ok, err)
		}
	}
}

// A dyn result the checker cannot type is checked at run time.
func TestDynResultMustBeABool(t *testing.T) {
	f, err := Compile(`dyn(this.name)`, &aigriddraftv1.Tenant{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Match(context.Background(), &aigriddraftv1.Tenant{Name: "a"}, "t-1", time.Now())
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "filter dyn(this.name) must evaluate to a bool, not string" {
		t.Errorf("dyn string = %v", err)
	}
}

func TestCostLimitStopsARunawayFilter(t *testing.T) {
	tn := &aigriddraftv1.Tenant{}
	for i := range 200 {
		tn.Domains = append(tn.Domains, fmt.Sprintf("d%d.example.com", i))
	}
	src := `this.domains.all(a, this.domains.all(b, this.domains.all(c, a != "")))`
	f, err := Compile(src, &aigriddraftv1.Tenant{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Match(context.Background(), tn, "t-1", time.Now())
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "cost limit exceeded") {
		t.Errorf("runaway = %v", err)
	}
}

// A List whose caller went away stops evaluating with the context's code,
// not InvalidArgument: the filter is not at fault.
func TestMatchStopsWhenTheContextEnds(t *testing.T) {
	f, err := Compile(`this.name.contains("a")`, &aigriddraftv1.Tenant{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.Match(ctx, &aigriddraftv1.Tenant{Name: "a"}, "t-1", time.Now())
	if status.Code(err) != codes.Canceled {
		t.Fatalf("Match after cancel = %v, want Canceled", err)
	}
}
