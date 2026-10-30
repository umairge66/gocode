package toolrun

import (
	"encoding/json"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// This file registers the local cross-session messaging tools
// (internal/inbox: SendMessage, ListAgents). Both go through the normal
// permission pipeline: their toolreg descriptors self-allow (SendMessage
// defers in auto/plan like Agent), and a SendMessage / ListAgents deny or ask
// settings rule is consulted first. Neither carries rule content — the
// tools define no per-input permission matcher, so a rule names the
// whole tool.

// WithPeerTools registers the SendMessage and ListAgents tools (either may be
// nil: a subagent gets SendMessage only).
func WithPeerTools(send, list ModelTool) Option {
	return func(d *Dispatcher) {
		d.SendMessageTool, d.ListAgentsTool = send, list
	}
}

// SetPeerTools registers (or, with nil, removes) the messaging tools after
// construction.
func (d *Dispatcher) SetPeerTools(send, list ModelTool) {
	d.SendMessageTool, d.ListAgentsTool = send, list
	d.registerPeerTools()
}

func (d *Dispatcher) registerPeerTools() {
	if d.SendMessageTool != nil {
		d.Register(protocol.ToolSendMessage, peerToolAdapter(d.SendMessageTool))
	} else {
		delete(d.adapters, protocol.ToolSendMessage)
	}
	if d.ListAgentsTool != nil {
		d.Register(protocol.ToolListAgents, peerToolAdapter(d.ListAgentsTool))
	} else {
		delete(d.adapters, protocol.ToolListAgents)
	}
}

// peerToolAdapter adapts a messaging tool: the input must be a JSON object;
// the rule content is empty (whole-tool rules).
func peerToolAdapter(t ModelTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in map[string]any
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
