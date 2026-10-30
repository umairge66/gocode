package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/ui"
)

// repeatingGlob is a provider script whose single turn (a Glob call) repeats
// on every request.
func repeatingGlob(t *testing.T) *fakeProvider {
	return newFakeProvider(t, fakeTurn{toolName: "Glob", toolArgs: `{"pattern":"*.none"}`, in: 1, out: 1})
}

func TestPrintJSONRepeatedToolCallsStop(t *testing.T) {
	p := repeatingGlob(t)
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "find it")
	if code != 1 {
		t.Fatalf("exit %d, err %q out %q", code, errOut, out)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q", out)
	}
	if r["subtype"] != "error_during_execution" || r["is_error"] != true || r["terminal_reason"] != "repeated_tool_calls" {
		t.Fatalf("result = %v", r)
	}
	errs, _ := r["errors"].([]any)
	if len(errs) != 1 || errs[0] != "Stopped: the same Glob call was repeated 8 times in a row" {
		t.Fatalf("errors = %v", r["errors"])
	}
	if _, ok := r["result"]; ok {
		t.Fatalf("error result carries result text: %v", r)
	}
	if p.callCount() != 8 {
		t.Fatalf("model called %d times, want 8", p.callCount())
	}
}

func TestPrintStreamJSONRepeatedToolCallsStop(t *testing.T) {
	p := repeatingGlob(t)
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "stream-json", "--verbose", "find it")
	if code != 1 {
		t.Fatalf("exit %d, err %q", code, errOut)
	}
	recs := decodeLines(t, out)
	last := recs[len(recs)-1]
	if last["type"] != "result" || last["subtype"] != "error_during_execution" || last["is_error"] != true ||
		last["terminal_reason"] != "repeated_tool_calls" {
		t.Fatalf("result = %v", last)
	}
	// The tool results carry the note from the third repeat on; the eighth
	// call is answered without running.
	var results []string
	for _, r := range recs {
		if r["type"] != "user" {
			continue
		}
		for _, b := range r["message"].(map[string]any)["content"].([]any) {
			blk := b.(map[string]any)
			if blk["type"] == "tool_result" {
				results = append(results, toolResultText(blk["content"]))
			}
		}
	}
	if len(results) != 8 {
		t.Fatalf("tool results = %d, want 8", len(results))
	}
	if strings.Contains(results[1], "Note:") || !strings.Contains(results[2], agent.RepeatedCallNote("Glob", 3)) ||
		!strings.Contains(results[6], agent.RepeatedCallNote("Glob", 7)) {
		t.Fatalf("notes: %q", results[:7])
	}
	if !strings.Contains(results[7], "Not run") {
		t.Fatalf("8th result = %q", results[7])
	}
}

// toolResultText flattens a stream-json tool_result content value.
func toolResultText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPrintTextRepeatedToolCallsStop(t *testing.T) {
	p := repeatingGlob(t)
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "find it")
	if code == 0 {
		t.Fatalf("exit 0, out %q", out)
	}
	if !strings.Contains(errOut, "Stopped: the same Glob call was repeated 8 times in a row") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestPrintRepeatedToolCallsSettings(t *testing.T) {
	p := repeatingGlob(t)
	f := headlessFixture(t, p)
	subcmdWriteFile(t, f.userSettingsPath(), `{"model":"gocode-sonnet-4-5","loopGuard":{"warnAfter":2,"stopAfter":4}}`)
	code, out, _ := f.run(t, "-p", "--output-format", "json", "find it")
	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	if code != 1 || r["terminal_reason"] != "repeated_tool_calls" || p.callCount() != 4 {
		t.Fatalf("exit %d, calls %d, result %v", code, p.callCount(), r)
	}
	if errs := r["errors"].([]any); errs[0] != "Stopped: the same Glob call was repeated 4 times in a row" {
		t.Fatalf("errors = %v", errs)
	}
}

func TestPrintRepeatedToolCallsDisabledByEnv(t *testing.T) {
	p := repeatingGlob(t)
	f := headlessFixture(t, p)
	f.vars[agent.LoopGuardEnv] = "off"
	code, out, _ := f.run(t, "-p", "--output-format", "json", "--max-turns", "10", "find it")
	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	if code != 1 || r["subtype"] != "error_max_turns" || p.callCount() != 10 {
		t.Fatalf("exit %d, calls %d, result %v", code, p.callCount(), r)
	}
}

func TestPrintRepeatedToolCallsDisabledBySettings(t *testing.T) {
	p := repeatingGlob(t)
	f := headlessFixture(t, p)
	subcmdWriteFile(t, f.userSettingsPath(), `{"model":"gocode-sonnet-4-5","loopGuard":{"warnAfter":0,"stopAfter":0}}`)
	code, _, _ := f.run(t, "-p", "--output-format", "json", "--max-turns", "10", "find it")
	if code != 1 || p.callCount() != 10 {
		t.Fatalf("exit %d, calls %d", code, p.callCount())
	}
}

func TestLoadLoopGuard(t *testing.T) {
	cases := []struct {
		name     string
		settings string
		env      string
		want     agent.LoopGuardConfig
		warn     string
	}{
		{"default", `{}`, "", agent.DefaultLoopGuard, ""},
		{"custom", `{"loopGuard":{"warnAfter":5,"stopAfter":12}}`, "", agent.LoopGuardConfig{WarnAfter: 5, StopAfter: 12}, ""},
		{"stop only", `{"loopGuard":{"warnAfter":0}}`, "", agent.LoopGuardConfig{WarnAfter: 0, StopAfter: 8}, ""},
		{"invalid order", `{"loopGuard":{"warnAfter":9,"stopAfter":4}}`, "", agent.DefaultLoopGuard, "must be greater than warnAfter"},
		{"bad value", `{"loopGuard":{"stopAfter":-2}}`, "", agent.DefaultLoopGuard, "whole number"},
		{"not object", `{"loopGuard":true}`, "", agent.DefaultLoopGuard, "must be an object"},
		{"env off", `{"loopGuard":{"warnAfter":5,"stopAfter":12}}`, "off", agent.LoopGuardConfig{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSubcmdFixture(t)
			subcmdWriteFile(t, f.userSettingsPath(), c.settings)
			if c.env != "" {
				f.vars[agent.LoopGuardEnv] = c.env
			}
			env, _, _ := f.env()
			got, warns := loadLoopGuard(env)
			if got == nil || *got != c.want {
				t.Fatalf("config = %+v, want %+v", got, c.want)
			}
			joined := strings.Join(warns, "\n")
			if (c.warn == "") != (joined == "") || !strings.Contains(joined, c.warn) {
				t.Fatalf("warnings = %q, want %q", joined, c.warn)
			}
		})
	}
}

// repeatCaller asks for the same Bash call on every round.
type repeatCaller struct {
	mu sync.Mutex
	n  int
}

func (c *repeatCaller) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.ToolUseBlock("rep", "Bash", json.RawMessage(`{"command":"ls -l out.vo"}`))),
		StopReason: agent.StopToolUse,
	}, nil
}

// TestInteractiveRepeatedToolCallsNotice: in the TUI the guard ends the turn
// with a system notice (not an error) so the user can decide how to go on.
func TestInteractiveRepeatedToolCallsNotice(t *testing.T) {
	caller := &repeatCaller{}
	prog := runDriverTurn(t, caller, fakeToolDispatcher{result: agent.ToolResult{Content: "out.vo"}})
	defer prog.Quit()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Stopped: the same Bash call was repeated 8 times in a row") &&
			strings.Contains(s.Text, "Type a message to continue")
	}, 3*time.Second) {
		t.Fatal("repeated-call notice was not shown")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && s.Status.Kind == ui.StatusIdle
	}, 2*time.Second) {
		t.Fatal("spinner was not reset to idle")
	}
	if prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.AppendErrorMsg); return ok }, 100*time.Millisecond) {
		t.Fatal("the stop was shown as an error")
	}
	caller.mu.Lock()
	n := caller.n
	caller.mu.Unlock()
	if n != 8 {
		t.Fatalf("model rounds = %d, want 8", n)
	}
}
