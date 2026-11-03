package sessionstore

import (
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// TestDeferredToolUseLine: the deferred-call meta line leaves the loaded
// history alone and names the pending call only while the history ends with
// the assistant turn that holds it.
func TestDeferredToolUseLine(t *testing.T) {
	st := New(t.TempDir())
	const cwd, id = "/work/proj", "11111111-2222-4333-8444-555555555555"
	sess, err := st.Create(cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	call := agent.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}
	history := []agent.Message{
		agent.UserMessage(agent.TextBlock("list")),
		agent.AssistantMessage(agent.ToolUseBlock(call.ID, call.Name, call.Input)),
	}
	if err := sess.AppendMessages(history); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendDeferredToolUse(call); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Load(cwd, id)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("Load = %d messages, %v", len(msgs), err)
	}
	got, ok := st.PendingDeferred(cwd, id, msgs)
	if !ok || got.ID != call.ID || got.Name != "Bash" || string(got.Input) != `{"command":"ls"}` {
		t.Fatalf("PendingDeferred = %+v %v", got, ok)
	}

	// Once the call is answered it is no longer pending.
	if err := sess.AppendMessages([]agent.Message{agent.UserMessage(agent.ToolResultBlock(call.ID, "a b", false))}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = st.Load(cwd, id)
	if _, ok := st.PendingDeferred(cwd, id, msgs); ok {
		t.Fatal("an answered call is still pending")
	}
}
