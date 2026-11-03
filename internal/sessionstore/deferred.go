package sessionstore

import (
	"encoding/json"

	"github.com/umairge66/gocode/internal/agent"
)

// A tool call a PreToolUse hook deferred is recorded with a meta line (no
// uuid, so it never joins the turn chain):
//
//	{"type":"deferred-tool-use","deferredToolUse":{"id":"…","name":"…","input":{…}},"sessionId":"<id>"}
//
// The assistant turn holding the call is an ordinary transcript line, left
// unanswered. A call is still pending while the loaded history ends with
// that assistant turn; once the resumed call runs, its tool_result follows
// in the next user line and the marker no longer applies. Readers that do
// not know the line skip it.

// typeDeferredToolUse is the deferred-call meta line's type.
const typeDeferredToolUse = "deferred-tool-use"

// deferredToolUse is the deferred call as the meta line records it.
type deferredToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type deferredLine struct {
	Type            string          `json:"type"`
	DeferredToolUse deferredToolUse `json:"deferredToolUse"`
	SessionID       string          `json:"sessionId"`
	GocodeSchema    int             `json:"gocodeSchema"`
}

// AppendDeferredToolUse records call as the session's deferred tool call
// (append-only; the latest line wins). Implements
// conversation.DeferredRecorder.
func (s *Session) AppendDeferredToolUse(call agent.ToolUse) error {
	if s == nil {
		return ErrNoConfigHome
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	return s.store.appendLine(s.path, deferredLine{
		Type:            typeDeferredToolUse,
		DeferredToolUse: deferredToolUse{ID: call.ID, Name: call.Name, Input: call.Input},
		SessionID:       s.id,
		GocodeSchema:    SchemaVersion,
	})
}

// PendingDeferred returns the deferred tool call still waiting in session
// id, whose loaded history is history: the call the latest deferred-tool-use
// line names, when history ends with the assistant turn that holds it.
func (s *Store) PendingDeferred(cwd, id string, history []agent.Message) (agent.ToolUse, bool) {
	if s == nil || s.base == "" {
		return agent.ToolUse{}, false
	}
	data, err := s.fs.ReadFile(s.sessionPath(cwd, id))
	if err != nil {
		return agent.ToolUse{}, false
	}
	var last string
	forEachEntry(data, func(e entry) {
		if e.Type == typeDeferredToolUse && e.DeferredToolUse != nil {
			last = e.DeferredToolUse.ID
		}
	})
	return agent.PendingToolUse(history, last)
}
