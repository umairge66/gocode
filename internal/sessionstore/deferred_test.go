package sessionstore

import (
	"encoding/json"
	"os"
	"strings"
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
	round, ok := st.PendingDeferred(cwd, id, msgs)
	got := round.First()
	if !ok || len(round.Calls) != 1 || len(round.Results) != 0 || got.ID != call.ID || got.Name != "Bash" || string(got.Input) != `{"command":"ls"}` {
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

// TestDeferredRoundLine: a round with several calls records every deferred
// call and the other calls' results, keeping deferredToolUse as the first
// deferred call for a reader that knows only it.
func TestDeferredRoundLine(t *testing.T) {
	st := New(t.TempDir())
	const cwd, id = "/work/proj", "11111111-2222-4333-8444-555555555556"
	sess, err := st.Create(cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	history := []agent.Message{
		agent.UserMessage(agent.TextBlock("go")),
		agent.AssistantMessage(
			agent.ToolUseBlock("a", "Read", json.RawMessage(`{"file_path":"x"}`)),
			agent.ToolUseBlock("b", "Bash", json.RawMessage(`{"command":"ls"}`)),
			agent.ToolUseBlock("c", "Write", json.RawMessage(`{}`)),
		),
	}
	if err := sess.AppendMessages(history); err != nil {
		t.Fatal(err)
	}
	round := agent.DeferredRound{
		Calls:   []agent.ToolUse{{ID: "b", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}, {ID: "c", Name: "Write", Input: json.RawMessage(`{}`)}},
		Results: []agent.ContentBlock{agent.ToolResultBlock("a", "contents", false)},
	}
	if err := sess.AppendDeferredRound(round); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &line); err != nil {
		t.Fatal(err)
	}
	if first, _ := line["deferredToolUse"].(map[string]any); first["id"] != "b" {
		t.Fatalf("deferredToolUse = %v", line["deferredToolUse"])
	}
	if all, _ := line["deferredToolUses"].([]any); len(all) != 2 {
		t.Fatalf("deferredToolUses = %v", line["deferredToolUses"])
	}
	msgs, _ := st.Load(cwd, id)
	got, ok := st.PendingDeferred(cwd, id, msgs)
	if !ok || len(got.Calls) != 2 || got.Calls[1].ID != "c" || len(got.Results) != 1 || got.Results[0].ToolUseID != "a" || got.Results[0].Content != "contents" {
		t.Fatalf("PendingDeferred = %+v %v", got, ok)
	}
}
