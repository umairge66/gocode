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
// When the round held other calls, or more than one was deferred, the line
// also carries every deferred call, in tool_use order, and the tool_result
// blocks of the calls that ran (deferredToolUse stays the first deferred
// call, so a reader that knows only it still finds one):
//
//	{"type":"deferred-tool-use","deferredToolUse":{…first…},
//	 "deferredToolUses":[{…},{…}],"toolResults":[{"type":"tool_result",…}],"sessionId":"<id>"}
//
// The assistant turn holding the calls is an ordinary transcript line, left
// unanswered. The round is still pending while the loaded history ends with
// that assistant turn; once it is settled, its tool_results follow in the
// next user line and the marker no longer applies. Readers that do not know
// the line skip it.

// typeDeferredToolUse is the deferred-call meta line's type.
const typeDeferredToolUse = "deferred-tool-use"

// deferredToolUse is a deferred call as the meta line records it.
type deferredToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type deferredLine struct {
	Type             string               `json:"type"`
	DeferredToolUse  deferredToolUse      `json:"deferredToolUse"`
	DeferredToolUses []deferredToolUse    `json:"deferredToolUses,omitempty"`
	ToolResults      []agent.ContentBlock `json:"toolResults,omitempty"`
	SessionID        string               `json:"sessionId"`
	GocodeSchema     int                  `json:"gocodeSchema"`
}

// AppendDeferredToolUse records call, a round's only call, as the session's
// deferred tool call.
func (s *Session) AppendDeferredToolUse(call agent.ToolUse) error {
	return s.AppendDeferredRound(agent.DeferredRound{Calls: []agent.ToolUse{call}})
}

// AppendDeferredRound records r as the session's deferred tool round
// (append-only; the latest line wins). Implements
// conversation.DeferredRecorder.
func (s *Session) AppendDeferredRound(r agent.DeferredRound) error {
	if s == nil {
		return ErrNoConfigHome
	}
	if len(r.Calls) == 0 {
		return nil
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	first := r.Calls[0]
	line := deferredLine{
		Type:            typeDeferredToolUse,
		DeferredToolUse: deferredToolUse{ID: first.ID, Name: first.Name, Input: first.Input},
		SessionID:       s.id,
		GocodeSchema:    SchemaVersion,
	}
	if len(r.Calls) > 1 || len(r.Results) > 0 {
		for _, c := range r.Calls {
			line.DeferredToolUses = append(line.DeferredToolUses, deferredToolUse{ID: c.ID, Name: c.Name, Input: c.Input})
		}
		line.ToolResults = r.Results
	}
	return s.store.appendLine(s.path, line)
}

// PendingDeferred returns the deferred tool round still waiting in session
// id, whose loaded history is history: the round the latest
// deferred-tool-use line records, when history ends with the assistant turn
// that holds it.
func (s *Store) PendingDeferred(cwd, id string, history []agent.Message) (agent.DeferredRound, bool) {
	if s == nil || s.base == "" {
		return agent.DeferredRound{}, false
	}
	// Only a history that ends with an unanswered tool call can hold one;
	// skip reading the transcript otherwise.
	if n := len(history); n == 0 || history[n-1].Role != agent.RoleAssistant || len(history[n-1].ToolUseBlocks()) == 0 {
		return agent.DeferredRound{}, false
	}
	data, err := s.fs.ReadFile(s.sessionPath(cwd, id))
	if err != nil {
		return agent.DeferredRound{}, false
	}
	var last *agent.DeferredRound
	forEachEntry(data, func(e entry) {
		if e.Type != typeDeferredToolUse || e.DeferredToolUse == nil {
			return
		}
		r := agent.DeferredRound{Results: e.ToolResults}
		calls := e.DeferredToolUses
		if len(calls) == 0 {
			calls = []deferredToolUse{*e.DeferredToolUse}
		}
		for _, c := range calls {
			r.Calls = append(r.Calls, agent.ToolUse{ID: c.ID, Name: c.Name, Input: c.Input})
		}
		last = &r
	})
	if last == nil {
		return agent.DeferredRound{}, false
	}
	return agent.PendingRound(history, *last)
}
