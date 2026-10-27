package gitops

import (
	"context"
	"errors"
	"testing"
)

func TestUnconfiguredRefusesEveryCall(t *testing.T) {
	ctx := context.Background()
	var w Writer = Unconfigured{}
	if _, err := w.Head(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Head() error = %v, want ErrNotConfigured", err)
	}
	if _, err := w.Commit(ctx, "", Change{Put: map[string][]byte{"a": []byte("a")}, Message: "m"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Commit() error = %v, want ErrNotConfigured", err)
	}
}
