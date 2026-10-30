package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// denyHook denies a named tool and allows the rest, recording PostToolUse.
type denyHook struct {
	deny     string
	postSeen []ToolUse
}

func (h *denyHook) PreToolUse(_ context.Context, call ToolUse) (PermissionDecision, error) {
	if call.Name == h.deny {
		return PermissionDecision{Behavior: BehaviorDeny, Message: "nope"}, nil
	}
	return PermissionDecision{Behavior: BehaviorAllow}, nil
}

func (h *denyHook) PostToolUse(_ context.Context, call ToolUse, _ ToolResult) {
	h.postSeen = append(h.postSeen, call)
}

// TestPreToolUseDenyProducesErrorResult checks that a PreToolUse deny skips the
// dispatcher and yields an is_error tool_result, while an allowed tool runs and
// fires PostToolUse.
func TestPreToolUseDenyProducesErrorResult(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		{
			Message: AssistantMessage(
				ToolUseBlock("toolu_deny", "Bash", json.RawMessage(`{"command":"rm -rf /"}`)),
				ToolUseBlock("toolu_ok", "Read", json.RawMessage(`{"path":"/x"}`)),
			),
			StopReason: StopToolUse,
		},
		endTurnResp("done"),
	}}
	tools := &recordingDispatcher{def: ToolResult{Content: "ran"}}
	hook := &denyHook{deny: "Bash"}

	loop := &Loop{Model: model, Tools: tools, Hooks: hook}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})

	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonCompleted)
	}
	// Dispatcher should have run only the allowed tool.
	if len(tools.calls) != 1 || tools.calls[0].Name != "Read" {
		t.Fatalf("dispatcher calls = %+v, want only Read", tools.calls)
	}
	// Two tool_result blocks in the user turn, in order.
	blocks := res.Messages[2].Content
	if len(blocks) != 2 {
		t.Fatalf("tool_result blocks = %d, want 2", len(blocks))
	}
	if blocks[0].ToolUseID != "toolu_deny" || !blocks[0].IsError {
		t.Fatalf("block[0] = %+v, want denied is_error for toolu_deny", blocks[0])
	}
	if blocks[0].Content != toolUseErrorOpen+"nope"+toolUseErrorClose {
		t.Fatalf("deny message = %q", blocks[0].Content)
	}
	if blocks[1].ToolUseID != "toolu_ok" || blocks[1].IsError || blocks[1].Content != "ran" {
		t.Fatalf("block[1] = %+v, want success for toolu_ok", blocks[1])
	}
	// PostToolUse fired only for the allowed, successful tool.
	if len(hook.postSeen) != 1 || hook.postSeen[0].Name != "Read" {
		t.Fatalf("PostToolUse seen = %+v, want one Read", hook.postSeen)
	}
}

// concDispatcher classifies tools as concurrency-safe by name and tracks the
// peak number of simultaneously in-flight calls.
type concDispatcher struct {
	safe    map[string]bool
	inFlew  int32
	peak    int32
	mu      sync.Mutex
	order   []string
	barrier time.Duration
}

func (d *concDispatcher) IsConcurrencySafe(call ToolUse) bool { return d.safe[call.Name] }

func (d *concDispatcher) Dispatch(_ context.Context, call ToolUse) (ToolResult, error) {
	n := atomic.AddInt32(&d.inFlew, 1)
	for {
		p := atomic.LoadInt32(&d.peak)
		if n <= p || atomic.CompareAndSwapInt32(&d.peak, p, n) {
			break
		}
	}
	time.Sleep(d.barrier)
	atomic.AddInt32(&d.inFlew, -1)
	d.mu.Lock()
	d.order = append(d.order, call.ID)
	d.mu.Unlock()
	return ToolResult{Content: call.Name}, nil
}

// TestConcurrencySafeToolsRunInParallel checks that consecutive
// concurrency-safe tool_use blocks execute together, while a mutating tool
// serializes, and that tool_result order follows tool_use order regardless.
func TestConcurrencySafeToolsRunInParallel(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		{
			Message: AssistantMessage(
				ToolUseBlock("r1", "Read", json.RawMessage(`{}`)),
				ToolUseBlock("r2", "Read", json.RawMessage(`{}`)),
				ToolUseBlock("r3", "Grep", json.RawMessage(`{}`)),
				ToolUseBlock("w1", "Write", json.RawMessage(`{}`)),
			),
			StopReason: StopToolUse,
		},
		endTurnResp("done"),
	}}
	tools := &concDispatcher{
		safe:    map[string]bool{"Read": true, "Grep": true}, // Write is not safe
		barrier: 20 * time.Millisecond,
	}

	loop := &Loop{Model: model, Tools: tools}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})

	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonCompleted)
	}
	// The three read-only tools should have overlapped (peak >= 2).
	if tools.peak < 2 {
		t.Fatalf("peak concurrency = %d, want >= 2 (read-only tools should run in parallel)", tools.peak)
	}
	// tool_result blocks must be in tool_use order.
	ids := []string{"r1", "r2", "r3", "w1"}
	blocks := res.Messages[2].Content
	if len(blocks) != 4 {
		t.Fatalf("tool_result blocks = %d, want 4", len(blocks))
	}
	for i, b := range blocks {
		if b.ToolUseID != ids[i] {
			t.Fatalf("block[%d].tool_use_id = %q, want %q", i, b.ToolUseID, ids[i])
		}
	}
}

// TestSerialDefaultWithoutClassifier checks that a dispatcher that does not
// implement ConcurrencyClassifier runs every tool serially (peak == 1).
func TestSerialDefaultWithoutClassifier(t *testing.T) {
	// recordingDispatcher does not implement ConcurrencyClassifier.
	loop := &Loop{Tools: &recordingDispatcher{def: ToolResult{Content: "ok"}}}
	calls := []ContentBlock{
		ToolUseBlock("a", "Read", nil),
		ToolUseBlock("b", "Read", nil),
	}
	got := loop.dispatchTurn(context.Background(), calls, nil, nil)
	if len(got) != 2 || got[0].ToolUseID != "a" || got[1].ToolUseID != "b" {
		t.Fatalf("results = %+v, want ordered a,b", got)
	}
	if loop.concurrencySafe(ToolUse{Name: "Read"}) {
		t.Fatalf("expected default concurrencySafe=false without a classifier")
	}
}
