package toolrun

import (
	"github.com/umairge66/gocode/internal/protocol"
)

// CapShellOutput applies the Bash tool's inline size limit to the output of a
// command the user ran from the input box, so it reaches the model the same
// way a Bash tool result would. Output within the limit is returned unchanged
// (framed=false). Longer output is saved under the session's tool-results
// directory as <id>.txt and replaced by a <persisted-output> preview, or, when
// it cannot be saved, cut to fit inside a <truncated-output> block
// (framed=true either way).
func (d *Dispatcher) CapShellOutput(id, content string) (out string, framed bool) {
	dir := ""
	if d != nil {
		dir = d.ToolResultsDir
	}
	return capShellText(dir, id, content, resultLimit(string(protocol.ToolBash)))
}
