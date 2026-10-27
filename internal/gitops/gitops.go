// Package gitops writes the backend's desired state to a Git repository, one
// atomic commit per API write, for Argo CD to deliver to the clusters.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

var (
	// ErrNotFound reports a Read of a file that is not in the snapshot.
	ErrNotFound = errors.New("gitops: file not found")
	// ErrConflict reports a Commit whose base is stale: a file the change
	// touches differs between the base revision and the branch head.
	ErrConflict = errors.New("gitops: a file in the change was modified since the base revision")
)

// Change is one atomic commit: files to write and files to delete, by
// repository path.
type Change struct {
	Put     map[string][]byte
	Delete  []string
	Message string
}

// Snapshot is the repository tree at one revision.
type Snapshot interface {
	Revision() string
	// Read returns the file at p, or ErrNotFound.
	Read(p string) ([]byte, error)
	// List returns the names of the files directly in dir, sorted. A missing
	// directory has no files.
	List(dir string) ([]string, error)
}

// Writer reads and commits to one branch.
type Writer interface {
	// Head returns the snapshot at the branch head.
	Head(ctx context.Context) (Snapshot, error)
	// Commit applies c on top of the branch head and returns the new revision.
	// It fails with ErrConflict when a file c touches changed since base, and
	// with ErrNotFound when c deletes a file the head doesn't have.
	Commit(ctx context.Context, base string, c Change) (string, error)
}

// validate checks that c is non-empty and that every path is a clean
// relative file path that appears once.
func (c Change) validate() error {
	if len(c.Put) == 0 && len(c.Delete) == 0 {
		return errors.New("gitops: empty change")
	}
	if strings.TrimSpace(c.Message) == "" {
		return errors.New("gitops: commit message is required")
	}
	seen := make(map[string]bool, len(c.Put)+len(c.Delete))
	for p := range c.Put {
		if err := validPath(p); err != nil {
			return err
		}
		seen[p] = true
	}
	for _, p := range c.Delete {
		if err := validPath(p); err != nil {
			return err
		}
		if seen[p] {
			return fmt.Errorf("gitops: path %q appears twice in one change", p)
		}
		seen[p] = true
	}
	return nil
}

// paths returns every path c touches.
func (c Change) paths() []string {
	out := make([]string, 0, len(c.Put)+len(c.Delete))
	for p := range c.Put {
		out = append(out, p)
	}
	return append(out, c.Delete...)
}

func validPath(p string) error {
	if p == "" || p != path.Clean(p) || path.IsAbs(p) || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("gitops: invalid path %q", p)
	}
	return nil
}
