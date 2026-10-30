//go:build !windows

package sessionstore

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// Red-team: transcripts hold tool output (which may include secrets); they
// must be 0600 in 0700 directories even under a permissive umask.
func TestTranscriptModesPrivate(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	base := filepath.Join(t.TempDir(), "projects")
	st := New(base)
	sess, err := st.Create("/work/p", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessages([]agent.Message{userMsg("hi")}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("transcript mode %o, want 0600", fi.Mode().Perm())
	}
	for d := filepath.Dir(sess.Path()); len(d) >= len(base); d = filepath.Dir(d) {
		di, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir %s mode %o, want 0700", d, di.Mode().Perm())
		}
	}
	if err := sess.ReplaceHistory([]agent.Message{userMsg("x")}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(sess.Path()); fi.Mode().Perm() != 0o600 {
		t.Errorf("rewritten transcript mode %o", fi.Mode().Perm())
	}
}
