package toolrun

import (
	"encoding/json"

	"github.com/umairge66/gocode/internal/protocol"
)

// FileTracker is the file-checkpointing seam (internal/filehistory.Store): it
// backs up a file's current content, or records its absence, before the agent's
// first modification of it under the current user prompt.
type FileTracker interface {
	TrackEdit(path string) error
}

// SetFileHistory installs (or, with nil, removes) the file checkpoint tracker.
func (d *Dispatcher) SetFileHistory(t FileTracker) {
	d.stateMu.Lock()
	d.FileHistory = t
	d.stateMu.Unlock()
}

// fileHistory reads the tracker under the state lock (concurrency-safe tools
// run in parallel with a host that may swap it).
func (d *Dispatcher) fileHistory() FileTracker {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.FileHistory
}

// fileEditTools are the tools whose calls are checkpointed before they run.
var fileEditTools = map[protocol.Tool]bool{
	protocol.ToolWrite:        true,
	protocol.ToolEdit:         true,
	protocol.ToolMultiEdit:    true,
	protocol.ToolNotebookEdit: true,
}

// trackFileEdit backs up the target of an allowed file-modifying call just
// before it runs. path is the one derived for the permission check; when a
// PreToolUse hook rewrote the input, it is re-derived from the new input. A
// tracking failure never blocks the edit (the file is just not rewindable).
func (d *Dispatcher) trackFileEdit(tool protocol.Tool, ad adapter, input json.RawMessage, path string, rewritten bool) {
	if !fileEditTools[tool] {
		return
	}
	fh := d.fileHistory()
	if fh == nil {
		return
	}
	if path = d.editTarget(ad, input, path, rewritten); path == "" {
		return
	}
	_ = fh.TrackEdit(path)
}
