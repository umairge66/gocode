package fsfacade

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

// TestRealpathSymlinkChain resolves a path through a multi-hop symlink chain
// mixing absolute and relative link targets.
func TestRealpathSymlinkChain(t *testing.T) {
	m := NewMemFS()
	m.MkdirAll("/real/dir")
	m.WriteFile("/real/dir/target.txt", []byte("hello"))

	// Absolute chain: /a -> /b -> /real
	m.Symlink("/b", "/a")
	m.Symlink("/real", "/b")
	// Relative link inside the resolved dir: /real/link -> dir/target.txt
	m.Symlink("dir/target.txt", "/real/link")

	got, err := Realpath(m, "/a/link")
	if err != nil {
		t.Fatalf("Realpath(/a/link) error: %v", err)
	}
	if want := "/real/dir/target.txt"; got != want {
		t.Fatalf("Realpath(/a/link) = %q, want %q", got, want)
	}

	// Resolving the directory symlink itself.
	got, err = Realpath(m, "/a/dir")
	if err != nil {
		t.Fatalf("Realpath(/a/dir) error: %v", err)
	}
	if want := "/real/dir"; got != want {
		t.Fatalf("Realpath(/a/dir) = %q, want %q", got, want)
	}
}

// TestRealpathLongChainWithinLimit confirms a chain just under the depth limit
// resolves, while one past it fails with ELOOP.
func TestRealpathLongChainWithinLimit(t *testing.T) {
	// 10 hops: /l0 -> /l1 -> ... -> /l9 -> /realfile
	m := NewMemFS()
	m.WriteFile("/realfile", []byte("x"))
	for i := 0; i < 9; i++ {
		m.Symlink(fmt.Sprintf("/l%d", i+1), fmt.Sprintf("/l%d", i))
	}
	m.Symlink("/realfile", "/l9")

	got, err := Realpath(m, "/l0")
	if err != nil {
		t.Fatalf("Realpath(/l0) 10-hop chain error: %v", err)
	}
	if want := "/realfile"; got != want {
		t.Fatalf("Realpath(/l0) = %q, want %q", got, want)
	}
}

// TestRealpathDepthLimit builds a chain longer than MaxSymlinkHops and expects
// an ELOOP-like failure before it ever reaches the real file.
func TestRealpathDepthLimit(t *testing.T) {
	m := NewMemFS()
	n := MaxSymlinkHops + 5
	m.WriteFile("/realfile", []byte("x"))
	for i := 0; i < n-1; i++ {
		m.Symlink(fmt.Sprintf("/l%d", i+1), fmt.Sprintf("/l%d", i))
	}
	m.Symlink("/realfile", fmt.Sprintf("/l%d", n-1))

	_, err := Realpath(m, "/l0")
	if err == nil {
		t.Fatalf("Realpath over %d-hop chain: expected ELOOP error, got nil", n)
	}
	if !errors.Is(err, ErrSymlinkLoop) {
		t.Fatalf("Realpath depth-limit error = %v, want wrapping ErrSymlinkLoop", err)
	}
}

// TestRealpathSymlinkLoop detects a cyclic symlink chain and self-loop.
func TestRealpathSymlinkLoop(t *testing.T) {
	t.Run("two-node cycle", func(t *testing.T) {
		m := NewMemFS()
		m.Symlink("/loopB", "/loopA")
		m.Symlink("/loopA", "/loopB")

		_, err := Realpath(m, "/loopA")
		if err == nil {
			t.Fatal("expected ELOOP error for A<->B cycle, got nil")
		}
		if !errors.Is(err, ErrSymlinkLoop) {
			t.Fatalf("cycle error = %v, want wrapping ErrSymlinkLoop", err)
		}
	})

	t.Run("self loop", func(t *testing.T) {
		m := NewMemFS()
		m.Symlink("/self", "/self")

		_, err := Realpath(m, "/self")
		if !errors.Is(err, ErrSymlinkLoop) {
			t.Fatalf("self-loop error = %v, want wrapping ErrSymlinkLoop", err)
		}
	})
}

// TestRealpathENOENT confirms a missing component surfaces as fs.ErrNotExist.
func TestRealpathENOENT(t *testing.T) {
	m := NewMemFS()
	m.MkdirAll("/exists")

	_, err := Realpath(m, "/exists/nope/deeper")
	if err == nil {
		t.Fatal("expected ENOENT error, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing-path error = %v, want wrapping fs.ErrNotExist", err)
	}

	// A dangling symlink must also report ENOENT once followed.
	m.Symlink("/missing-target", "/dangling")
	if _, err := Realpath(m, "/dangling/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dangling-symlink error = %v, want wrapping fs.ErrNotExist", err)
	}
}

// TestRealpathRelativeToAbsolute confirms relative inputs are resolved against
// the working directory and lexically canonicalized (".." / ".").
func TestRealpathRelativeToAbsolute(t *testing.T) {
	m := NewMemFS()
	m.Cwd = "/home/user"
	m.WriteFile("/home/user/sub/file.txt", []byte("hi"))

	got, err := Realpath(m, "sub/file.txt")
	if err != nil {
		t.Fatalf("Realpath(relative) error: %v", err)
	}
	if want := "/home/user/sub/file.txt"; got != want {
		t.Fatalf("Realpath(sub/file.txt) = %q, want %q", got, want)
	}

	// "." and ".." canonicalization within an absolute path.
	got, err = Realpath(m, "/home/user/./sub/../sub/file.txt")
	if err != nil {
		t.Fatalf("Realpath(dotdot) error: %v", err)
	}
	if want := "/home/user/sub/file.txt"; got != want {
		t.Fatalf("Realpath(dotdot) = %q, want %q", got, want)
	}

	// The input is resolved *lexically* before any symlink walk: resolveAbs
	// collapses ".." via path.Clean, so a ".." following a symlinked directory is collapsed against the link's lexical
	// parent, not its real target. Here "/link-to-b/.." cleans to "/" before
	// the walk begins, so the link is never traversed.
	m.MkdirAll("/real/a/b")
	m.WriteFile("/real/a/sibling", []byte("s"))
	m.WriteFile("/top", []byte("t"))
	m.Symlink("/real/a/b", "/link-to-b")
	got, err = Realpath(m, "/link-to-b/../../top")
	if err != nil {
		t.Fatalf("Realpath(lexical dotdot) error: %v", err)
	}
	if want := "/top"; got != want {
		t.Fatalf("Realpath(/link-to-b/../../top) = %q, want %q (lexical ..)", got, want)
	}
}
