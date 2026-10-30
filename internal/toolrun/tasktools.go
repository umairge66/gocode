package toolrun

import (
	"encoding/json"
	"path/filepath"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/tools/tasks"
)

// subagentTaskOwner labels the background tasks a subagent starts: they are
// not announced to the main conversation, and a subagent's TaskStop reaches
// only them.
const subagentTaskOwner = "subagent"

// taskOwner is the owner label of the background tasks this dispatcher's
// agent starts ("" for the main session).
func (d *Dispatcher) taskOwner() string {
	if d.Subagent {
		return subagentTaskOwner
	}
	return ""
}

// registerTaskTools registers TaskStop when a background task manager is
// present (and removes it otherwise), and lets the read-only file tools read
// the manager's output files without a prompt: the Bash tool hands their
// paths to the model.
func (d *Dispatcher) registerTaskTools() {
	m := d.BackgroundTasks
	if m == nil {
		delete(d.adapters, protocol.ToolTaskStop)
		return
	}
	d.Register(protocol.ToolTaskStop, taskStopAdapter(tasks.TaskStopTool{Manager: m, Owner: d.taskOwner()}))
	if dir := m.OutputDir(); dir != "" && d.Decider != nil {
		dir = filepath.Clean(dir)
		for _, have := range d.Decider.ReadableDirs {
			if have == dir {
				return
			}
		}
		d.Decider.ReadableDirs = append(d.Decider.ReadableDirs, dir)
	}
}

// taskStopAdapter adapts the TaskStop tool. Its rules name the whole tool (no
// rule content).
func taskStopAdapter(t tasks.TaskStopTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in tasks.Input
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		run:         t.Execute,
		description: t.Description(),
		inputSchema: t.InputSchema(),
	}
}
