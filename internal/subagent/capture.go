package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
)

// Live activity / transcript capture.
//
// A spawned subagent runs its own agent.Loop on its own goroutine. To surface
// what it is doing — the current tool call (Task.Activity) and the full running
// log of assistant text + tool calls (Task.Transcript) — the Spawner wraps two
// seams around the child loop when Capture is enabled:
//
//   - the tool dispatcher, so every tool call records a tool TranscriptEntry and
//     refreshes Task.Activity (mirrors conversation.toolEventDispatcher); and
//   - the model caller, so each round's assistant text (streamed deltas,
//     coalesced, plus the authoritative final text) records an assistant entry.
//
// All mutation goes through the registry's mutex-guarded Update, so capture is
// safe even though concurrency-safe tools dispatch on parallel goroutines. This
// stays entirely UI-agnostic: it only writes the neutral TranscriptEntry
// structure on the task.

// ActivitySummarizer turns a tool call into a short activity string (for
// Task.Activity, e.g. `Grep("ctrl-c exit")`) and a one-line argument summary
// (for the tool entry's ToolArgs). It is injected by the wiring layer so this
// package keeps no tool-name knowledge; a nil summarizer falls back to the raw
// tool name and no args.
type ActivitySummarizer func(name string, input json.RawMessage) (activity, args string)

// captureSink records a child agent's activity and transcript onto one task.
// It is safe for concurrent use.
type captureSink struct {
	reg       *TaskRegistry
	taskID    string
	summarize ActivitySummarizer

	mu       sync.Mutex
	openAsst bool   // an un-finalized assistant entry is the last transcript entry
	asstText string // accumulated text of that open assistant entry
}

// assistantDelta coalesces one streamed assistant text delta into the latest
// assistant entry (starting one if none is open), so the live view shows the
// text growing.
func (c *captureSink) assistantDelta(delta string) {
	if delta == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.openAsst {
		c.openAsst = true
		c.asstText = delta
		c.reg.Update(c.taskID, func(t *Task) {
			t.appendTranscript(TranscriptEntry{Kind: KindAssistant, Text: c.asstText})
		})
		return
	}
	c.asstText += delta
	text := c.asstText
	c.reg.Update(c.taskID, func(t *Task) {
		if n := len(t.Transcript); n > 0 && t.Transcript[n-1].Kind == KindAssistant {
			t.Transcript[n-1].Text = text
		}
	})
}

// assistantFinal records a round's authoritative final assistant text: it
// replaces the open streamed entry's text when one is open (the final text is
// canonical), or appends a fresh assistant entry when the round did not stream
// (plain, non-streaming model caller). It then closes the open entry.
func (c *captureSink) assistantFinal(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openAsst {
		if strings.TrimSpace(text) != "" {
			c.reg.Update(c.taskID, func(t *Task) {
				if n := len(t.Transcript); n > 0 && t.Transcript[n-1].Kind == KindAssistant {
					t.Transcript[n-1].Text = text
				}
			})
		}
		c.openAsst = false
		c.asstText = ""
		return
	}
	if strings.TrimSpace(text) != "" {
		c.reg.Update(c.taskID, func(t *Task) {
			t.appendTranscript(TranscriptEntry{Kind: KindAssistant, Text: text})
		})
	}
}

// toolStart closes any open assistant entry and sets Task.Activity to the
// current tool's summary. The tool entry itself is appended on toolEnd (so it
// carries the result) — a single append per call keeps the capture trivially
// concurrency-safe for parallel tools.
func (c *captureSink) toolStart(name string, input json.RawMessage) {
	c.mu.Lock()
	c.openAsst = false
	c.asstText = ""
	c.mu.Unlock()
	activity := name
	if c.summarize != nil {
		if a, _ := c.summarize(name, input); a != "" {
			activity = a
		}
	}
	c.reg.Update(c.taskID, func(t *Task) { t.Activity = activity })
}

// toolEnd appends the completed tool call as a transcript entry.
func (c *captureSink) toolEnd(name string, input json.RawMessage, result string, isErr bool) {
	var args string
	if c.summarize != nil {
		_, args = c.summarize(name, input)
	}
	c.reg.Update(c.taskID, func(t *Task) {
		t.appendTranscript(TranscriptEntry{
			Kind:       KindTool,
			ToolName:   name,
			ToolArgs:   args,
			ToolResult: result,
			IsError:    isErr,
		})
	})
}

// capturingDispatcher wraps the child's tool dispatcher to record a tool entry
// (and refresh Task.Activity) around every dispatch. It forwards the optional
// ConcurrencyClassifier so the loop's parallel-tool scheduling is preserved.
type capturingDispatcher struct {
	inner agent.ToolDispatcher
	sink  *captureSink
}

var (
	_ agent.ToolDispatcher        = (*capturingDispatcher)(nil)
	_ agent.ConcurrencyClassifier = (*capturingDispatcher)(nil)
)

func (d *capturingDispatcher) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	d.sink.toolStart(call.Name, call.Input)
	res, err := d.inner.Dispatch(ctx, call)
	result, isErr := res.Content, res.IsError
	if err != nil {
		isErr = true
		if result == "" {
			result = err.Error()
		}
	}
	d.sink.toolEnd(call.Name, call.Input, result, isErr)
	return res, err
}

func (d *capturingDispatcher) IsConcurrencySafe(call agent.ToolUse) bool {
	if cc, ok := d.inner.(agent.ConcurrencyClassifier); ok {
		return cc.IsConcurrencySafe(call)
	}
	return false
}

// capturingCaller wraps the child's model caller to record each round's final
// assistant text (one assistant TranscriptEntry per round). It presents ONLY
// the plain ModelCaller interface so the child loop keeps its original,
// non-streaming path — subagents never streamed, so this adds no behavior change
// (notably it keeps the -p request/response shape identical). assistantFinal is
// still able to coalesce streamed deltas (captureSink.assistantDelta) should a
// future streaming seam be wired.
type capturingCaller struct {
	inner agent.ModelCaller
	sink  *captureSink
}

var _ agent.ModelCaller = (*capturingCaller)(nil)

func (c *capturingCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	resp, err := c.inner.CallModel(ctx, req)
	if err == nil {
		c.sink.assistantFinal(assistantText(resp.Message))
	}
	return resp, err
}

// assistantText joins the text blocks of an assistant message.
func assistantText(m agent.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == agent.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
