package toolrun

import (
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

type recordingTracker struct{ paths []string }

func (r *recordingTracker) TrackEdit(path string) error {
	r.paths = append(r.paths, path)
	return nil
}

// TestFileHistoryTracksBeforeWrite checks the checkpoint seam: an allowed
// Write is tracked (before it runs), a read-only tool is not.
func TestFileHistoryTracksBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	d := New(toolreg.ModeBypassPermissions, nil)
	rec := &recordingTracker{}
	d.SetFileHistory(rec)
	file := filepath.Join(dir, "a.txt")
	if res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": file, "content": "x"}); res.IsError {
		t.Fatalf("Write failed: %s", res.Content)
	}
	dispatch(t, d, string(protocol.ToolGlob), map[string]any{"pattern": "*", "path": dir})
	if len(rec.paths) != 1 || rec.paths[0] != file {
		t.Fatalf("tracked = %v, want [%s]", rec.paths, file)
	}
}

// TestFileHistoryNotTrackedWhenDenied checks a denied edit is never tracked.
func TestFileHistoryNotTrackedWhenDenied(t *testing.T) {
	dir := t.TempDir()
	d := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Write"}})
	rec := &recordingTracker{}
	d.SetFileHistory(rec)
	dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": filepath.Join(dir, "a.txt"), "content": "x"})
	if len(rec.paths) != 0 {
		t.Fatalf("denied write tracked: %v", rec.paths)
	}
}
