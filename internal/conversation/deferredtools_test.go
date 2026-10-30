package conversation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func deferredSchemas() []agent.ToolSchema {
	raw := json.RawMessage(`{"type":"object"}`)
	return []agent.ToolSchema{
		{Name: "Read", Description: "read", InputSchema: raw},
		{Name: agent.ToolSearchName, Description: "search", InputSchema: raw},
		{Name: "WebFetch", Description: "fetch", InputSchema: raw, Deferred: true},
		{Name: "NotebookEdit", Description: "nb", InputSchema: raw, Deferred: true},
	}
}

func toolNames(ts []agent.ToolSchema) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

// TestDeferredToolsSurviveCompaction: a tool loaded with ToolSearch before a
// compaction is still sent in full after it (the compact summary carries its
// name), and the resumable transcript keeps it too.
func TestDeferredToolsSurviveCompaction(t *testing.T) {
	store := &recordingStore{}
	model := &fakeModel{reply: "ok"}
	c := New(model, fakeDispatcher{})
	c.ToolSchemas = deferredSchemas()
	c.Compactor = newCompactor(70000, &fakeSummarizer{})

	result := agent.ToolResultBlock("s1", "Loaded WebFetch.", false)
	result.LoadedTools = []string{"WebFetch"}
	seed := []agent.Message{
		agent.UserMessage(agent.TextBlock("fetch it")),
		agent.AssistantMessage(agent.ToolUseBlock("s1", agent.ToolSearchName, json.RawMessage(`{"query":"select:WebFetch"}`))),
		agent.UserMessage(result),
		agent.AssistantMessage(agent.TextBlock("loaded")),
	}
	_ = store.AppendMessages(seed)
	c.SeedHistory(append([]agent.Message(nil), seed...), store)

	if _, err := c.Submit(context.Background(), "continue after compaction"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(store.boundaryLog()) != 1 {
		t.Fatalf("compaction did not run")
	}
	got := toolNames(model.lastReq.Tools)
	want := []string{"Read", agent.ToolSearchName, "WebFetch"}
	if len(got) != len(want) {
		t.Fatalf("tools after compaction = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools after compaction = %v, want %v", got, want)
		}
	}
	if d := toolNames(model.lastReq.DeferredTools); len(d) != 1 || d[0] != "NotebookEdit" {
		t.Fatalf("deferred after compaction = %v, want [NotebookEdit]", d)
	}
	if first := store.snapshot()[0].Content[0]; len(first.LoadedTools) != 1 || first.LoadedTools[0] != "WebFetch" {
		t.Fatalf("compact summary does not carry the loaded tool: %+v", first)
	}
	if l := agent.LoadedTools(store.snapshot()); len(l) != 1 || l[0] != "WebFetch" {
		t.Fatalf("resumable transcript loaded tools = %v, want [WebFetch]", l)
	}
}

// TestDeferredToolsAnnouncedInPersistedTurn: the deferred tool names ride in
// the user turn written to the transcript before the model is called, once.
func TestDeferredToolsAnnouncedInPersistedTurn(t *testing.T) {
	store := &recordingStore{}
	model := &fakeModel{reply: "ok"}
	c := New(model, fakeDispatcher{})
	c.ToolSchemas = deferredSchemas()
	c.SeedHistory(nil, store)
	for _, p := range []string{"one", "two"} {
		if _, err := c.Submit(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	announcements := 0
	for _, m := range store.lineLog() {
		for _, b := range m.Content {
			if b.AnnouncedTools != nil {
				announcements++
				if len(b.AnnouncedTools) != 2 {
					t.Fatalf("announced %v, want both deferred tools", b.AnnouncedTools)
				}
			}
		}
	}
	if announcements != 1 {
		t.Fatalf("deferred tools announced %d times in the transcript, want 1", announcements)
	}
	for _, n := range toolNames(model.lastReq.Tools) {
		if n == "WebFetch" || n == "NotebookEdit" {
			t.Fatalf("unloaded deferred tool %s sent: %v", n, toolNames(model.lastReq.Tools))
		}
	}
}
