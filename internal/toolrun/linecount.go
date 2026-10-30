package toolrun

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/protocol"
)

// Session code-change counters.
//
// After a successful Edit, MultiEdit or Write the dispatcher reports how many
// lines the call added and removed (OnLinesChanged), so /usage and the status
// line can show the session's total code changes:
//
//   - a Write that creates a file adds every line of its content (the number
//     of newlines plus one; an empty file adds none);
//   - an Edit, a MultiEdit or a Write over an existing file counts the "+"
//     and "-" lines of a line diff between the content before and after.
//
// NotebookEdit changes cells, not lines, and is not counted.

// lineCountTools are the tools whose successful calls are counted.
var lineCountTools = map[protocol.Tool]bool{
	protocol.ToolWrite:     true,
	protocol.ToolEdit:      true,
	protocol.ToolMultiEdit: true,
}

// maxDiffCells bounds the line-diff table (changed old lines x changed new
// lines). A larger change counts every changed line as removed and added.
const maxDiffCells = 4_000_000

// WithLinesChanged installs the code-change counter callback.
func WithLinesChanged(fn func(added, removed int)) Option {
	return func(d *Dispatcher) { d.SetLinesChanged(fn) }
}

// SetLinesChanged installs (or, with nil, removes) the code-change counter.
func (d *Dispatcher) SetLinesChanged(fn func(added, removed int)) {
	d.stateMu.Lock()
	d.onLinesChanged = fn
	d.stateMu.Unlock()
}

// LinesChanged returns the installed code-change counter (nil when none), so
// a subagent's dispatcher can report into the same session totals.
func (d *Dispatcher) LinesChanged() func(added, removed int) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.onLinesChanged
}

// lineCount is the pre-call snapshot of a counted file change.
type lineCount struct {
	fn      func(added, removed int)
	path    string
	before  string
	existed bool
}

// beginLineCount snapshots the target of a counted tool call before it runs.
// It returns nil when the tool is not counted, no counter is installed, or the
// target cannot be determined or read.
func (d *Dispatcher) beginLineCount(tool protocol.Tool, ad adapter, input json.RawMessage, path string, rewritten bool) *lineCount {
	if !lineCountTools[tool] {
		return nil
	}
	fn := d.LinesChanged()
	if fn == nil {
		return nil
	}
	path = d.editTarget(ad, input, path, rewritten)
	if path == "" {
		return nil
	}
	lc := &lineCount{fn: fn, path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		lc.before, lc.existed = string(b), true
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil
	}
	return lc
}

// finish reads the file after a successful call and reports the change.
func (lc *lineCount) finish() {
	if lc == nil {
		return
	}
	b, err := os.ReadFile(lc.path)
	if err != nil {
		return
	}
	added, removed := countChangedLines(lc.before, string(b), lc.existed)
	if added > 0 || removed > 0 {
		lc.fn(added, removed)
	}
}

// editTarget resolves the absolute path a file-modifying call writes: the one
// derived for the permission check, re-derived when a PreToolUse hook rewrote
// the input, and joined to the working directory when relative. "" when none.
func (d *Dispatcher) editTarget(ad adapter, input json.RawMessage, path string, rewritten bool) string {
	if rewritten {
		if _, p, _, err := ad.permission(input); err == nil {
			path = p
		}
	}
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		base := d.WorkDir
		if base == "" {
			base, _ = os.Getwd()
		}
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

// countChangedLines counts the lines a change added and removed. A new file
// adds all of its lines; otherwise the counts are those of a line diff.
func countChangedLines(before, after string, existed bool) (added, removed int) {
	if !existed {
		if after == "" {
			return 0, 0
		}
		return strings.Count(after, "\n") + 1, 0
	}
	if before == after {
		return 0, 0
	}
	a := strings.Split(strings.ReplaceAll(before, "\r\n", "\n"), "\n")
	b := strings.Split(strings.ReplaceAll(after, "\r\n", "\n"), "\n")
	// Trim the common prefix and suffix; only the middle can differ.
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	if len(a) == 0 || len(b) == 0 || len(a)*len(b) > maxDiffCells {
		return len(b), len(a)
	}
	common := lcsLength(a, b)
	return len(b) - common, len(a) - common
}

// lcsLength is the length of the longest common subsequence of a and b, in
// O(len(b)) memory.
func lcsLength(a, b []string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
