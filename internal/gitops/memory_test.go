package gitops

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestMemoryWriterCommitAndRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := NewMemoryWriter(map[string][]byte{"catalog/subscriptions/a.yaml": []byte("a")})

	head, err := w.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	rev, err := w.Commit(ctx, head.Revision(), Change{
		Put:     map[string][]byte{"catalog/subscriptions/b.yaml": []byte("b")},
		Delete:  []string{"catalog/subscriptions/a.yaml"},
		Message: "subscriptions: create b, delete a",
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	head, err = w.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	if head.Revision() != rev {
		t.Fatalf("head revision = %q, want %q", head.Revision(), rev)
	}
	if data, err := head.Read("catalog/subscriptions/b.yaml"); err != nil || string(data) != "b" {
		t.Fatalf("Read(b) = %q, %v", data, err)
	}
	if _, err := head.Read("catalog/subscriptions/a.yaml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(a) error = %v, want ErrNotFound", err)
	}
	names, err := head.List("catalog/subscriptions")
	if err != nil || !slices.Equal(names, []string{"b.yaml"}) {
		t.Fatalf("List() = %v, %v", names, err)
	}
	if got := w.Messages(); !slices.Equal(got, []string{"subscriptions: create b, delete a"}) {
		t.Fatalf("Messages() = %v", got)
	}
}

func TestMemoryWriterListOnlyDirectChildren(t *testing.T) {
	t.Parallel()
	w := NewMemoryWriter(map[string][]byte{
		"catalog/subscriptions/b.yaml":   nil,
		"catalog/subscriptions/a.yaml":   nil,
		"catalog/subscriptions/x/c.yaml": nil,
		"catalog/models/m.yaml":          nil,
	})
	head, _ := w.Head(context.Background())
	names, err := head.List("catalog/subscriptions")
	if err != nil || !slices.Equal(names, []string{"a.yaml", "b.yaml"}) {
		t.Fatalf("List() = %v, %v", names, err)
	}
	if names, _ := head.List("missing"); len(names) != 0 {
		t.Fatalf("List(missing) = %v", names)
	}
}

func TestMemoryWriterSnapshotIsStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := NewMemoryWriter(map[string][]byte{"f": []byte("1")})
	old, _ := w.Head(ctx)
	if _, err := w.Commit(ctx, old.Revision(), Change{Put: map[string][]byte{"f": []byte("2")}, Message: "m"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if data, _ := old.Read("f"); string(data) != "1" {
		t.Fatalf("old snapshot Read(f) = %q, want 1", data)
	}
}

func TestMemoryWriterConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := NewMemoryWriter(map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	base, _ := w.Head(ctx)
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"a": []byte("a2")}, Message: "m"}); err != nil {
		t.Fatalf("first Commit() error = %v", err)
	}

	tests := []struct {
		name   string
		change Change
		want   error
	}{
		{"put a file changed since base", Change{Put: map[string][]byte{"a": []byte("a3")}, Message: "m"}, ErrConflict},
		{"delete a file changed since base", Change{Delete: []string{"a"}, Message: "m"}, ErrConflict},
		{"put a file created since base", Change{Put: map[string][]byte{"c": []byte("c")}, Message: "m"}, nil},
		{"put an untouched file from a stale base", Change{Put: map[string][]byte{"b": []byte("b2")}, Message: "m"}, nil},
	}
	for _, tt := range tests {
		_, err := w.Commit(ctx, base.Revision(), tt.change)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: Commit() error = %v, want %v", tt.name, err, tt.want)
		}
	}
	// A concurrent create of the same new file: the second one conflicts.
	base, _ = w.Head(ctx)
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"d": []byte("1")}, Message: "m"}); err != nil {
		t.Fatalf("Commit(d) error = %v", err)
	}
	if _, err := w.Commit(ctx, base.Revision(), Change{Put: map[string][]byte{"d": []byte("2")}, Message: "m"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second Commit(d) error = %v, want ErrConflict", err)
	}
}

func TestMemoryWriterRejectsInvalidChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := NewMemoryWriter(nil)
	head, _ := w.Head(ctx)
	for name, c := range map[string]Change{
		"empty":          {Message: "m"},
		"no message":     {Put: map[string][]byte{"a": nil}},
		"absolute path":  {Put: map[string][]byte{"/a": nil}, Message: "m"},
		"parent path":    {Put: map[string][]byte{"../a": nil}, Message: "m"},
		"unclean path":   {Put: map[string][]byte{"a//b": nil}, Message: "m"},
		"put and delete": {Put: map[string][]byte{"a": nil}, Delete: []string{"a"}, Message: "m"},
		"empty path":     {Delete: []string{""}, Message: "m"},
	} {
		if _, err := w.Commit(ctx, head.Revision(), c); err == nil {
			t.Errorf("%s: Commit() succeeded", name)
		}
	}
	if _, err := w.Commit(ctx, head.Revision(), Change{Delete: []string{"missing"}, Message: "m"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Commit(delete missing) error = %v, want ErrNotFound", err)
	}
	if _, err := w.Commit(ctx, "9", Change{Put: map[string][]byte{"a": nil}, Message: "m"}); err == nil {
		t.Error("Commit() with an unknown base succeeded")
	}
}
