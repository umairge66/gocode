//go:build !windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/errutil"
)

func TestVerifyOwnedDirsTightensMode(t *testing.T) {
	d := filepath.Join(t.TempDir(), "rt")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyOwnedDirs(d); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(d)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestVerifyOwnedDirsRefusesForeignOwner(t *testing.T) {
	d := t.TempDir()
	old := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	defer func() { currentUID = old }()
	err := verifyOwnedDirs(d)
	var ne *errutil.NotOwnedError
	if !errors.As(err, &ne) || ne.Code() != errutil.CodeNotOwned {
		t.Fatalf("want NotOwnedError, got %v", err)
	}
}
