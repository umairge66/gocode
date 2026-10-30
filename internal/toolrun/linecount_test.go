package toolrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

type lineTotals struct{ added, removed, calls int }

func (l *lineTotals) add(a, r int) { l.added += a; l.removed += r; l.calls++ }

// TestLinesChanged: a created file adds all its lines, an Edit counts its
// diff, an overwriting Write counts its diff, and a failed call counts nothing.
func TestLinesChanged(t *testing.T) {
	dir := t.TempDir()
	d := New(toolreg.ModeBypassPermissions, nil)
	var tot lineTotals
	d.SetLinesChanged(tot.add)
	file := filepath.Join(dir, "calc.py")

	if res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": file, "content": "a\nb\nc"}); res.IsError {
		t.Fatalf("Write: %s", res.Content)
	}
	if tot.added != 3 || tot.removed != 0 {
		t.Fatalf("after create: %+v", tot)
	}
	dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file})
	if res := dispatch(t, d, string(protocol.ToolEdit), map[string]any{"file_path": file, "old_string": "b", "new_string": "B1\nB2"}); res.IsError {
		t.Fatalf("Edit: %s", res.Content)
	}
	if tot.added != 5 || tot.removed != 1 {
		t.Fatalf("after edit: %+v", tot)
	}
	if res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": file, "content": "a\nB1\nB2\nc\nd"}); res.IsError {
		t.Fatalf("overwrite: %s", res.Content)
	}
	if tot.added != 6 || tot.removed != 1 {
		t.Fatalf("after overwrite: %+v", tot)
	}
	calls := tot.calls
	if res := dispatch(t, d, string(protocol.ToolEdit), map[string]any{"file_path": file, "old_string": "zzz", "new_string": "y"}); !res.IsError {
		t.Fatal("edit of a missing string succeeded")
	}
	if tot.calls != calls {
		t.Fatalf("failed edit counted: %+v", tot)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "B2") {
		t.Fatalf("file = %q", b)
	}
}

func TestCountChangedLines(t *testing.T) {
	cases := []struct {
		before, after string
		existed       bool
		add, rem      int
	}{
		{"", "", false, 0, 0},
		{"", "x\n", false, 2, 0},
		{"a\nb\n", "a\nb\n", true, 0, 0},
		{"a\nb\nc\n", "a\nx\nc\n", true, 1, 1},
		{"a\r\nb\r\n", "a\nb\nc\n", true, 1, 0},
		{"a\nb\nc\nd\n", "a\nd\n", true, 0, 2},
		{"x\ny\n", "y\nx\n", true, 1, 1},
	}
	for _, c := range cases {
		add, rem := countChangedLines(c.before, c.after, c.existed)
		if add != c.add || rem != c.rem {
			t.Errorf("%q -> %q: +%d -%d, want +%d -%d", c.before, c.after, add, rem, c.add, c.rem)
		}
	}
}
