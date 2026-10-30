package subagent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// fakeSummarize is a trivial ActivitySummarizer for tests: activity = "Name(x)",
// args = "x".
func fakeSummarize(name string, _ json.RawMessage) (string, string) {
	return name + "(x)", "x"
}

// TestSpawnCapturesActivityAndTranscript: a spawned child that emits assistant
// text and a tool call populates Task.Activity and Task.Transcript with a
// neutral assistant entry and a tool entry (with its result).
func TestSpawnCapturesActivityAndTranscript(t *testing.T) {
	readIn, _ := json.Marshal(map[string]string{"file_path": "/etc/hosts"})
	model := &scriptedModel{responses: []agent.ModelResponse{
		{
			Message: agent.AssistantMessage(
				agent.TextBlock("Let me read the file."),
				agent.ToolUseBlock("t1", "Read", readIn),
			),
			StopReason: agent.StopToolUse,
		},
		handbackResp("t2", "done"),
	}}
	sp := newSpawner()
	sp.Capture = true
	sp.Summarize = fakeSummarize

	_, task, err := sp.Spawn(context.Background(),
		SpawnRequest{SubagentType: "general-purpose", Prompt: "read it"},
		model, nopDispatcher{result: agent.ToolResult{Content: "127.0.0.1 localhost"}})
	if err != nil {
		t.Fatalf("Spawn error: %v", err)
	}

	got, ok := sp.Registry.Get(task.ID)
	if !ok {
		t.Fatal("task missing from registry")
	}
	if got.Activity != "Read(x)" {
		t.Fatalf("Activity = %q, want %q", got.Activity, "Read(x)")
	}

	var sawAssistant, sawTool bool
	for _, e := range got.Transcript {
		switch e.Kind {
		case KindAssistant:
			if e.Text == "Let me read the file." {
				sawAssistant = true
			}
		case KindTool:
			if e.ToolName == "Read" && e.ToolArgs == "x" && e.ToolResult == "127.0.0.1 localhost" && !e.IsError {
				sawTool = true
			}
		}
	}
	if !sawAssistant {
		t.Fatalf("no assistant transcript entry with the streamed text: %+v", got.Transcript)
	}
	if !sawTool {
		t.Fatalf("no tool transcript entry with result: %+v", got.Transcript)
	}
}

// TestCaptureSinkConcurrentAndCapped drives the sink from many goroutines (as
// concurrency-safe tools do) and asserts the transcript is bounded to
// MaxTranscriptEntries without a data race (run under -race).
func TestCaptureSinkConcurrentAndCapped(t *testing.T) {
	reg := NewTaskRegistry()
	task := reg.Create(Task{AgentType: "general-purpose", Prompt: "p"})
	sink := &captureSink{reg: reg, taskID: task.ID, summarize: fakeSummarize}

	const n = MaxTranscriptEntries * 3
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink.toolStart("Bash", nil)
			sink.toolEnd("Bash", nil, "ok", false)
		}()
	}
	wg.Wait()

	got, _ := reg.Get(task.ID)
	if len(got.Transcript) != MaxTranscriptEntries {
		t.Fatalf("transcript length = %d, want capped at %d", len(got.Transcript), MaxTranscriptEntries)
	}
	if got.Activity != "Bash(x)" {
		t.Fatalf("Activity = %q, want %q", got.Activity, "Bash(x)")
	}
}

// TestCaptureCoalescesStreamingAssistant: streamed deltas coalesce into one
// assistant entry, and the authoritative final text replaces it.
func TestCaptureCoalescesStreamingAssistant(t *testing.T) {
	reg := NewTaskRegistry()
	task := reg.Create(Task{AgentType: "general-purpose", Prompt: "p"})
	sink := &captureSink{reg: reg, taskID: task.ID}

	sink.assistantDelta("Hel")
	sink.assistantDelta("lo wor")
	sink.assistantDelta("ld")
	// Mid-stream: one coalesced entry showing the growing text.
	got, _ := reg.Get(task.ID)
	if len(got.Transcript) != 1 || got.Transcript[0].Text != "Hello world" {
		t.Fatalf("mid-stream transcript = %+v, want one entry %q", got.Transcript, "Hello world")
	}
	// Final text closes/replaces the open entry (no duplicate entry).
	sink.assistantFinal("Hello world.")
	got, _ = reg.Get(task.ID)
	if len(got.Transcript) != 1 || got.Transcript[0].Text != "Hello world." {
		t.Fatalf("final transcript = %+v, want one entry %q", got.Transcript, "Hello world.")
	}
	// A subsequent non-streamed final appends a fresh entry.
	sink.assistantFinal("Second message.")
	got, _ = reg.Get(task.ID)
	if len(got.Transcript) != 2 || got.Transcript[1].Text != "Second message." {
		t.Fatalf("second message transcript = %+v", got.Transcript)
	}
}
