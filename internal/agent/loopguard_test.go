package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCallSignatureCanonical(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{`{"command":"ls","timeout":5000}`, `{"timeout":5000.0,"command":"ls"}`, true},
		{`{"timeout":5e3}`, `{"timeout":5000}`, true},
		{`{"a":{"y":1,"x":[1.50,2]}}`, `{ "a" : { "x" : [1.5, 2.0], "y" : 1.0 } }`, true},
		{`{"command":"ls"}`, `{"command":"ls "}`, false},
		{`{"timeout":5000}`, `{"timeout":5001}`, false},
		{`{"n":0.1}`, `{"n":0.10}`, true},
		{``, `{}`, true},
		{`not json`, `not json`, true},
		{`not json`, `not  json`, false},
	}
	for _, c := range cases {
		got := CallSignature("Bash", json.RawMessage(c.a)) == CallSignature("Bash", json.RawMessage(c.b))
		if got != c.same {
			t.Errorf("CallSignature(%s) == CallSignature(%s): %v, want %v", c.a, c.b, got, c.same)
		}
	}
	if CallSignature("Bash", json.RawMessage(`{}`)) == CallSignature("Read", json.RawMessage(`{}`)) {
		t.Error("different tools must not share a signature")
	}
}

func TestLoopGuardConfigValidate(t *testing.T) {
	for _, c := range []struct {
		cfg LoopGuardConfig
		ok  bool
	}{
		{LoopGuardConfig{3, 8}, true},
		{LoopGuardConfig{0, 8}, true},
		{LoopGuardConfig{3, 0}, true},
		{LoopGuardConfig{0, 0}, true},
		{LoopGuardConfig{8, 8}, false},
		{LoopGuardConfig{9, 8}, false},
		{LoopGuardConfig{-1, 8}, false},
	} {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("Validate(%+v) = %v, want ok=%v", c.cfg, err, c.ok)
		}
	}
}

// callSeq is a fake model that asks for one scripted call per round (each
// entry may hold several parallel calls), then ends the turn.
type callSeq struct {
	rounds [][]ToolUse
	n      int
}

func (m *callSeq) CallModel(context.Context, ModelRequest) (ModelResponse, error) {
	if m.n >= len(m.rounds) {
		m.n++
		return endTurnResp("finished"), nil
	}
	var blocks []ContentBlock
	for i, c := range m.rounds[m.n] {
		blocks = append(blocks, ToolUseBlock(fmt.Sprintf("toolu_%d_%d", m.n, i), c.Name, c.Input))
	}
	m.n++
	return ModelResponse{Message: AssistantMessage(blocks...), StopReason: StopToolUse}, nil
}

func repeat(n int, name, input string) [][]ToolUse {
	out := make([][]ToolUse, n)
	for i := range out {
		out[i] = []ToolUse{{Name: name, Input: json.RawMessage(input)}}
	}
	return out
}

// toolResults returns every tool_result block of the run, in order.
func toolResults(msgs []Message) []ContentBlock {
	var out []ContentBlock
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == BlockToolResult {
				out = append(out, b)
			}
		}
	}
	return out
}

func TestLoopGuardWarnsFromThirdRepeat(t *testing.T) {
	model := &callSeq{rounds: repeat(5, "Bash", `{"command":"ls","timeout":5000}`)}
	tools := &recordingDispatcher{def: ToolResult{Content: "out"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want completed", res.Reason)
	}
	rs := toolResults(res.Messages)
	if len(rs) != 5 {
		t.Fatalf("results = %d, want 5", len(rs))
	}
	for i, r := range rs {
		count := i + 1
		hasNote := strings.Contains(r.Content, "Note: you have now run")
		if (count >= 3) != hasNote {
			t.Errorf("call %d: note present = %v (content %q)", count, hasNote, r.Content)
		}
		if count >= 3 {
			want := "out\n\n" + RepeatedCallNote("Bash", count)
			if r.Content != want {
				t.Errorf("call %d content = %q, want %q", count, r.Content, want)
			}
		}
	}
	if !strings.Contains(rs[2].Content, "this exact Bash call 3 times in a row") {
		t.Errorf("third note wording: %q", rs[2].Content)
	}
}

func TestLoopGuardStopsOnEighth(t *testing.T) {
	model := &callSeq{rounds: repeat(20, "Bash", `{"command":"ls"}`)}
	tools := &recordingDispatcher{def: ToolResult{Content: "out"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonRepeatedToolCalls {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonRepeatedToolCalls)
	}
	if len(tools.calls) != 7 {
		t.Fatalf("executed %d calls, want 7 (the 8th is not run)", len(tools.calls))
	}
	var se *StopError
	if e, ok := res.Err.(*StopError); ok {
		se = e
	}
	if se == nil || se.Kind != StopKindRepeatedToolCalls || se.Message != "Stopped: the same Bash call was repeated 8 times in a row" {
		t.Fatalf("err = %#v", res.Err)
	}
	rs := toolResults(res.Messages)
	if len(rs) != 8 {
		t.Fatalf("results = %d, want 8 (every tool_use answered)", len(rs))
	}
	if !rs[7].IsError || !strings.Contains(rs[7].Content, "Not run") {
		t.Errorf("8th result = %+v", rs[7])
	}
	if model.n != 8 {
		t.Errorf("model calls = %d, want 8", model.n)
	}
}

func TestLoopGuardStreakResetByDifferentCall(t *testing.T) {
	rounds := append(repeat(7, "Bash", `{"command":"ls"}`), []ToolUse{{Name: "Bash", Input: json.RawMessage(`{"command":"pwd"}`)}})
	rounds = append(rounds, repeat(7, "Bash", `{"command":"ls"}`)...)
	model := &callSeq{rounds: rounds}
	tools := &recordingDispatcher{def: ToolResult{Content: "out"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want completed (streak reset by the pwd call)", res.Reason)
	}
	if len(tools.calls) != 15 {
		t.Fatalf("executed %d, want 15", len(tools.calls))
	}
	if rs := toolResults(res.Messages); strings.Contains(rs[7].Content, "this exact") || strings.Contains(rs[9].Content, "this exact") {
		t.Errorf("no note expected right after the reset: %q / %q", rs[7].Content, rs[9].Content)
	}
}

func TestLoopGuardStreakResetByMidTurnUserMessage(t *testing.T) {
	model := &callSeq{rounds: repeat(12, "Bash", `{"command":"ls"}`)}
	tools := &recordingDispatcher{def: ToolResult{Content: "out"}}
	round := 0
	loop := &Loop{Model: model, Tools: tools, MidTurn: func(context.Context) []ContentBlock {
		round++
		if round == 6 {
			return []ContentBlock{TextBlock("keep checking please")}
		}
		return nil
	}}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want completed", res.Reason)
	}
	rs := toolResults(res.Messages)
	if strings.Contains(rs[6].Content, "Note:") || strings.Contains(rs[7].Content, "Note:") {
		t.Errorf("streak should restart after the user message: %q / %q", rs[6].Content, rs[7].Content)
	}
	if !strings.Contains(rs[8].Content, RepeatedCallNote("Bash", 3)) {
		t.Errorf("third call after the reset should warn: %q", rs[8].Content)
	}
}

func TestLoopGuardNewRunStartsFresh(t *testing.T) {
	tools := &recordingDispatcher{def: ToolResult{Content: "out"}}
	for i := 0; i < 2; i++ {
		model := &callSeq{rounds: repeat(6, "Bash", `{"command":"ls"}`)}
		res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
		if res.Reason != ReasonCompleted {
			t.Fatalf("run %d reason = %q", i, res.Reason)
		}
	}
}

func TestLoopGuardParallelCalls(t *testing.T) {
	same := ToolUse{Name: "Read", Input: json.RawMessage(`{"file_path":"/a"}`)}
	other := ToolUse{Name: "Read", Input: json.RawMessage(`{"file_path":"/b"}`)}
	// Round 1: three identical calls in one turn → the third warns.
	// Round 2: five more identical → counts 4..8; the 8th stops, the call
	// after it is skipped.
	model := &callSeq{rounds: [][]ToolUse{
		{same, same, same},
		{same, same, same, same, same, other},
	}}
	tools := &recordingDispatcher{def: ToolResult{Content: "x"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonRepeatedToolCalls {
		t.Fatalf("reason = %q", res.Reason)
	}
	rs := toolResults(res.Messages)
	if len(rs) != 9 {
		t.Fatalf("results = %d, want 9", len(rs))
	}
	if strings.Contains(rs[1].Content, "Note:") || !strings.Contains(rs[2].Content, RepeatedCallNote("Read", 3)) {
		t.Errorf("first turn notes wrong: %q / %q", rs[1].Content, rs[2].Content)
	}
	if !strings.Contains(rs[6].Content, RepeatedCallNote("Read", 7)) {
		t.Errorf("7th: %q", rs[6].Content)
	}
	if !rs[7].IsError || !strings.Contains(rs[7].Content, "repeated 8 times") {
		t.Errorf("8th: %+v", rs[7])
	}
	if !rs[8].IsError || !strings.Contains(rs[8].Content, skippedAfterRepeatStop) {
		t.Errorf("trailing call: %+v", rs[8])
	}
	if len(tools.calls) != 7 {
		t.Errorf("executed %d, want 7", len(tools.calls))
	}
}

func TestLoopGuardDisabled(t *testing.T) {
	for name, cfg := range map[string]LoopGuardConfig{
		"off":       {},
		"warn only": {WarnAfter: 3},
	} {
		model := &callSeq{rounds: repeat(12, "Bash", `{"command":"ls"}`)}
		tools := &recordingDispatcher{def: ToolResult{Content: "out"}}
		c := cfg
		res := (&Loop{Model: model, Tools: tools, LoopGuard: &c}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
		if res.Reason != ReasonCompleted || len(tools.calls) != 12 {
			t.Fatalf("%s: reason %q, calls %d", name, res.Reason, len(tools.calls))
		}
		notes := strings.Contains(toolResults(res.Messages)[11].Content, "Note:")
		if notes != (cfg.WarnAfter > 0) {
			t.Errorf("%s: note present = %v", name, notes)
		}
	}
	// Stop only: no notes, still stops.
	model := &callSeq{rounds: repeat(12, "Bash", `{"command":"ls"}`)}
	tools := &recordingDispatcher{def: ToolResult{Content: "out"}}
	res := (&Loop{Model: model, Tools: tools, LoopGuard: &LoopGuardConfig{StopAfter: 5}}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonRepeatedToolCalls || len(tools.calls) != 4 {
		t.Fatalf("stop only: reason %q, calls %d", res.Reason, len(tools.calls))
	}
	for _, r := range toolResults(res.Messages)[:4] {
		if strings.Contains(r.Content, "Note:") {
			t.Errorf("stop-only config added a note: %q", r.Content)
		}
	}
}

func TestLoopGuardPollingToolsUseHigherThresholds(t *testing.T) {
	if !pollingTools["ListAgents"] || pollingTools["Bash"] {
		t.Fatal("polling set")
	}
	model := &callSeq{rounds: repeat(40, "ListAgents", `{}`)}
	tools := &recordingDispatcher{def: ToolResult{Content: "none running"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonRepeatedToolCalls {
		t.Fatalf("reason = %q", res.Reason)
	}
	warn, stop := DefaultLoopGuard.WarnAfter*pollingFactor, DefaultLoopGuard.StopAfter*pollingFactor
	if len(tools.calls) != stop-1 {
		t.Fatalf("executed %d, want %d", len(tools.calls), stop-1)
	}
	rs := toolResults(res.Messages)
	if strings.Contains(rs[warn-2].Content, "Note:") || !strings.Contains(rs[warn-1].Content, RepeatedCallNote("ListAgents", warn)) {
		t.Errorf("polling note threshold: %q / %q", rs[warn-2].Content, rs[warn-1].Content)
	}
}

// TestLoopGuardReplaysBenchmarkLoop replays the tool calls of a real run in
// which the model, done after a dozen calls, re-ran one identical Bash command
// 31 times in a row. With every call answered by the same text, the
// exact-repeat guard warns from the third repeat and ends the run at the
// eighth, instead of letting it continue to call 101.
func TestLoopGuardReplaysBenchmarkLoop(t *testing.T) {
	f, err := os.Open("testdata/repeated_tool_calls.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rounds [][]ToolUse
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		rounds = append(rounds, []ToolUse{{Name: c.Name, Input: c.Input}})
	}
	if len(rounds) != 101 {
		t.Fatalf("fixture has %d calls, want 101", len(rounds))
	}
	// Locate the first streak of 8+ identical calls in the recording.
	streakStart := -1
	for i := 0; i+8 <= len(rounds) && streakStart < 0; i++ {
		sig := CallSignature(rounds[i][0].Name, rounds[i][0].Input)
		j := i
		for j < len(rounds) && CallSignature(rounds[j][0].Name, rounds[j][0].Input) == sig {
			j++
		}
		if j-i >= 8 {
			streakStart = i
		}
	}
	if streakStart != 53 {
		t.Fatalf("streak starts at call %d, want 54 (index 53)", streakStart+1)
	}

	model := &callSeq{rounds: rounds}
	tools := &recordingDispatcher{def: ToolResult{Content: "-rw-r--r-- 1 root root 1234 plus_comm.vo\nplus_comm.vo verified"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("Fix the proof in plus_comm.v"))})

	if res.Reason != ReasonRepeatedToolCalls {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonRepeatedToolCalls)
	}
	stopCall := streakStart + 7 // 0-based index of the 8th identical call
	if model.n != stopCall+1 {
		t.Fatalf("model rounds = %d, want %d", model.n, stopCall+1)
	}
	if len(tools.calls) != stopCall {
		t.Fatalf("executed %d calls, want %d (stop call not run)", len(tools.calls), stopCall)
	}
	rs := toolResults(res.Messages)
	for i := streakStart; i < stopCall; i++ {
		count := i - streakStart + 1
		has := strings.Contains(rs[i].Content, RepeatedCallNote("Bash", count))
		if has != (count >= 3) {
			t.Errorf("call %d (repeat %d): note = %v", i+1, count, has)
		}
	}
	for i := 0; i < streakStart; i++ {
		if strings.Contains(rs[i].Content, "this exact") {
			t.Errorf("call %d warned before the streak: %q", i+1, rs[i].Content)
		}
	}
	if got := res.Err.Error(); got != "Stopped: the same Bash call was repeated 8 times in a row" {
		t.Errorf("stop message = %q", got)
	}
	t.Logf("recorded run: 101 calls; with the guard: stopped at call %d (%d executed), warned from call %d",
		stopCall+1, len(tools.calls), streakStart+3)
}
