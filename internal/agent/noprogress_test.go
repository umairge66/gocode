package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestNoProgressGuardRecord(t *testing.T) {
	g := &noProgressGuard{}
	in := func(s string) json.RawMessage { return json.RawMessage(s) }
	a, b := in(`{"command":"make test"}`), in(`{"command":"git status"}`)
	steps := []struct {
		name  string
		input json.RawMessage
		out   string
		err   bool
		want  int
	}{
		{"Bash", a, "ok", false, 1},
		{"Bash", b, "clean", false, 1},
		{"Bash", a, "ok", false, 2},
		{"Bash", b, "clean", false, 2},
		{"Bash", a, "ok", false, 3},    // A,B,A,B,A counts each key
		{"Bash", a, "FAIL", false, 1},  // a changed result restarts the key
		{"Bash", b, "clean", false, 3}, // ... and leaves the others alone
		{"Edit", in(`{"file_path":"x"}`), "updated", false, 0},
		{"Bash", b, "clean", false, 1}, // a file change forgets every key
		{"Edit", in(`{"file_path":"x"}`), "old_string not found", true, 1},
		{"Bash", b, "clean", false, 2}, // a failed edit is no progress
		{"TodoWrite", in(`{"todos":[]}`), "ok", false, 0},
		{"Bash", b, "clean", false, 1},
	}
	for i, s := range steps {
		if got := g.record(s.name, s.input, s.out, s.err); got != s.want {
			t.Errorf("step %d (%s %s → %q): count %d, want %d", i+1, s.name, s.input, s.out, got, s.want)
		}
	}
}

// scriptDispatcher answers each call with out(call, n), n being the 0-based
// number of calls dispatched so far.
type scriptDispatcher struct {
	mu    sync.Mutex
	calls []ToolUse
	out   func(c ToolUse, n int) ToolResult
}

func (d *scriptDispatcher) Dispatch(_ context.Context, c ToolUse) (ToolResult, error) {
	d.mu.Lock()
	n := len(d.calls)
	d.calls = append(d.calls, c)
	d.mu.Unlock()
	return d.out(c, n), nil
}

func bash(cmd string) ToolUse {
	in, _ := json.Marshal(map[string]any{"command": cmd})
	return ToolUse{Name: "Bash", Input: in}
}

func single(calls ...ToolUse) [][]ToolUse {
	out := make([][]ToolUse, len(calls))
	for i, c := range calls {
		out[i] = []ToolUse{c}
	}
	return out
}

// reverifyCalls returns n equivalent but never byte-identical verification
// commands, each followed by the marker it echoes.
func reverifyCalls(n int) ([]ToolUse, []string) {
	words := []string{"done", "verified", "complete", "SUCCESS", "final check passed"}
	var calls []ToolUse
	var markers []string
	for i := 0; i < n; i++ {
		w := fmt.Sprintf("%s %d", words[i%len(words)], i)
		cmd := fmt.Sprintf(`coqc /w/a.v && ls -l /w/a.vo && echo "%s"`, w)
		if i%2 == 1 {
			cmd = fmt.Sprintf(`ls -l /w/a.vo && coqc /w/a.v && echo "%s"`, w)
		}
		in, _ := json.Marshal(map[string]any{"command": cmd, "timeout": 1000 * (i + 1)})
		calls = append(calls, ToolUse{Name: "Bash", Input: in})
		markers = append(markers, w)
	}
	return calls, markers
}

func reverifyDispatcher(markers []string) *scriptDispatcher {
	return &scriptDispatcher{out: func(_ ToolUse, n int) ToolResult {
		return ToolResult{Content: fmt.Sprintf("-rw-r--r--. 1 root root 14407 Oct 28 12:%02d /w/a.vo\n%s\n", 14+n/5, markers[n])}
	}}
}

func runCalls(l *Loop, rounds [][]ToolUse, d ToolDispatcher) Result {
	l.Model = &callSeq{rounds: rounds}
	l.Tools = d
	return l.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
}

func TestNoProgressWarnsFromThirdEquivalentCall(t *testing.T) {
	calls, markers := reverifyCalls(6)
	res := runCalls(&Loop{}, single(calls...), reverifyDispatcher(markers))
	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q", res.Reason)
	}
	rs := toolResults(res.Messages)
	for i, r := range rs {
		count := i + 1
		has := strings.Contains(r.Content, "Note:")
		if has != (count >= 3) {
			t.Errorf("call %d: note = %v (%q)", count, has, r.Content)
		}
		if count >= 3 && !strings.HasSuffix(r.Content, "\n\n"+NoProgressNote("Bash", count)) {
			t.Errorf("call %d content = %q", count, r.Content)
		}
	}
	if !strings.Contains(rs[2].Content, "run Bash calls equivalent to this one 3 times with the same result and no changes in between") {
		t.Errorf("note wording: %q", rs[2].Content)
	}
}

func TestNoProgressStopsAtTwiceStopAfter(t *testing.T) {
	calls, markers := reverifyCalls(30)
	d := reverifyDispatcher(markers)
	res := runCalls(&Loop{}, single(calls...), d)
	if res.Reason != ReasonRepeatedToolCalls {
		t.Fatalf("reason = %q", res.Reason)
	}
	stop := noProgressStopFactor * DefaultLoopGuard.StopAfter
	if len(d.calls) != stop {
		t.Fatalf("executed %d, want %d (the stopping call has already run)", len(d.calls), stop)
	}
	se, ok := res.Err.(*StopError)
	if !ok || se.Kind != StopKindRepeatedToolCalls || se.Message != "Stopped: equivalent Bash calls were repeated 16 times with no progress" {
		t.Fatalf("err = %#v", res.Err)
	}
	rs := toolResults(res.Messages)
	if len(rs) != stop {
		t.Fatalf("results = %d, want %d", len(rs), stop)
	}
	if !strings.Contains(rs[stop-1].Content, NoProgressNote("Bash", stop)) || rs[stop-1].IsError {
		t.Errorf("last result = %+v", rs[stop-1])
	}
	if last := res.Messages[len(res.Messages)-1]; last.Role != RoleUser {
		t.Errorf("history must end with the recorded results, got %q", last.Role)
	}
}

func TestNoProgressStopRecordsWholeTurn(t *testing.T) {
	calls, markers := reverifyCalls(16)
	rounds := single(calls[:15]...)
	other := bash("pwd")
	rounds = append(rounds, []ToolUse{calls[15], other})
	d := &scriptDispatcher{out: func(c ToolUse, n int) ToolResult {
		if n < len(markers) {
			return ToolResult{Content: "a.vo\n" + markers[n]}
		}
		return ToolResult{Content: "/w"}
	}}
	res := runCalls(&Loop{}, rounds, d)
	if res.Reason != ReasonRepeatedToolCalls || len(d.calls) != 17 {
		t.Fatalf("reason %q, executed %d", res.Reason, len(d.calls))
	}
	if rs := toolResults(res.Messages); len(rs) != 17 || rs[16].Content != "/w" {
		t.Fatalf("results: %d", len(rs))
	}
}

func TestNoProgressOneNotePerResult(t *testing.T) {
	model := repeat(5, "Bash", `{"command":"ls"}`)
	d := &scriptDispatcher{out: func(ToolUse, int) ToolResult { return ToolResult{Content: "a"} }}
	res := runCalls(&Loop{}, model, d)
	for i, r := range toolResults(res.Messages) {
		if n := strings.Count(r.Content, "Note:"); n > 1 {
			t.Errorf("call %d has %d notes: %q", i+1, n, r.Content)
		}
		if i >= 2 && !strings.Contains(r.Content, RepeatedCallNote("Bash", i+1)) {
			t.Errorf("call %d should carry the exact-repeat note: %q", i+1, r.Content)
		}
	}
}

func noNotes(t *testing.T, name string, res Result) {
	t.Helper()
	if res.Reason != ReasonCompleted {
		t.Errorf("%s: reason = %q", name, res.Reason)
	}
	for i, r := range toolResults(res.Messages) {
		if strings.Contains(r.Content, "Note:") {
			t.Errorf("%s: call %d got a note: %q", name, i+1, r.Content)
		}
	}
}

func TestNoProgressQuietOnNormalWork(t *testing.T) {
	t.Run("edit test cycles", func(t *testing.T) {
		var calls []ToolUse
		for i := 0; i < 20; i++ {
			in, _ := json.Marshal(map[string]any{"file_path": "/w/a.go", "old_string": fmt.Sprint(i), "new_string": fmt.Sprint(i + 1)})
			calls = append(calls, ToolUse{Name: "Edit", Input: in}, bash("go test ./..."))
		}
		d := &scriptDispatcher{out: func(c ToolUse, n int) ToolResult {
			if c.Name == "Edit" {
				return ToolResult{Content: "The file /w/a.go has been updated successfully."}
			}
			return ToolResult{Content: "--- FAIL: TestX (0.00s)\nFAIL\tpkg\t0.012s", IsError: true}
		}}
		noNotes(t, "edit/test", runCalls(&Loop{}, single(calls...), d))
	})
	t.Run("test output changes", func(t *testing.T) {
		var calls []ToolUse
		for i := 0; i < 20; i++ {
			calls = append(calls, bash("pytest -q"))
		}
		d := &scriptDispatcher{out: func(_ ToolUse, n int) ToolResult {
			return ToolResult{Content: fmt.Sprintf("%d failed, %d passed in 0.%02ds", 20-n, n, n)}
		}}
		// Identical calls still get the exact-repeat guard; only check the
		// no-progress detector here.
		g := LoopGuardConfig{WarnAfter: 3, StopAfter: 100}
		res := runCalls(&Loop{LoopGuard: &g}, single(calls...), d)
		for i, r := range toolResults(res.Messages) {
			if strings.Contains(r.Content, "equivalent to this one") {
				t.Errorf("call %d: %q", i+1, r.Content)
			}
		}
	})
	t.Run("reading different files", func(t *testing.T) {
		var calls []ToolUse
		for i := 0; i < 20; i++ {
			in, _ := json.Marshal(map[string]any{"file_path": fmt.Sprintf("/w/f%d.go", i)})
			calls = append(calls, ToolUse{Name: "Read", Input: in})
		}
		d := &scriptDispatcher{out: func(ToolUse, int) ToolResult { return ToolResult{Content: "package x"} }}
		noNotes(t, "reads", runCalls(&Loop{}, single(calls...), d))
	})
	t.Run("grep different patterns", func(t *testing.T) {
		var calls []ToolUse
		for i := 0; i < 20; i++ {
			in, _ := json.Marshal(map[string]any{"pattern": fmt.Sprintf("func%d", i), "path": "/w"})
			calls = append(calls, ToolUse{Name: "Grep", Input: in})
		}
		d := &scriptDispatcher{out: func(ToolUse, int) ToolResult { return ToolResult{Content: "No matches found"} }}
		noNotes(t, "grep", runCalls(&Loop{}, single(calls...), d))
	})
	t.Run("git status after commits", func(t *testing.T) {
		var calls []ToolUse
		for i := 0; i < 10; i++ {
			calls = append(calls,
				bash(fmt.Sprintf(`git add -A && git commit -m "step %d"`, i)),
				bash("git status --short && echo ---"))
		}
		d := &scriptDispatcher{out: func(c ToolUse, n int) ToolResult {
			if strings.Contains(string(c.Input), "commit") {
				return ToolResult{Content: fmt.Sprintf("[main %07x] step %d\n 1 file changed", 0xabc000+n, n/2)}
			}
			return ToolResult{Content: fmt.Sprintf("?? new%d.txt\n---", n/2)}
		}}
		noNotes(t, "git", runCalls(&Loop{}, single(calls...), d))
	})
}

func TestNoProgressDisabled(t *testing.T) {
	calls, markers := reverifyCalls(30)
	d := reverifyDispatcher(markers)
	res := runCalls(&Loop{LoopGuard: &LoopGuardConfig{}}, single(calls...), d)
	noNotes(t, "off", res)
	if len(d.calls) != 30 {
		t.Errorf("executed %d, want 30", len(d.calls))
	}
	// Warn only: notes, no stop.
	d = reverifyDispatcher(markers)
	res = runCalls(&Loop{LoopGuard: &LoopGuardConfig{WarnAfter: 3}}, single(calls...), d)
	if res.Reason != ReasonCompleted || !strings.Contains(toolResults(res.Messages)[29].Content, NoProgressNote("Bash", 30)) {
		t.Errorf("warn only: reason %q", res.Reason)
	}
	// Stop only: no notes, stops at 2 × StopAfter.
	d = reverifyDispatcher(markers)
	res = runCalls(&Loop{LoopGuard: &LoopGuardConfig{StopAfter: 5}}, single(calls...), d)
	if res.Reason != ReasonRepeatedToolCalls || len(d.calls) != 10 {
		t.Errorf("stop only: reason %q, executed %d", res.Reason, len(d.calls))
	}
	for i, r := range toolResults(res.Messages) {
		if strings.Contains(r.Content, "Note:") {
			t.Errorf("stop only: call %d got a note", i+1)
		}
	}
}

func TestNoProgressResetByUserMessageAndNewRun(t *testing.T) {
	calls, markers := reverifyCalls(6)
	round := 0
	l := &Loop{MidTurn: func(context.Context) []ContentBlock {
		round++
		if round == 3 {
			return []ContentBlock{TextBlock("check once more")}
		}
		return nil
	}}
	res := runCalls(l, single(calls...), reverifyDispatcher(markers))
	rs := toolResults(res.Messages)
	if !strings.Contains(rs[2].Content, "Note:") || strings.Contains(rs[3].Content, "Note:") || strings.Contains(rs[4].Content, "Note:") {
		t.Errorf("mid-turn message should restart the count: %q / %q / %q", rs[2].Content, rs[3].Content, rs[4].Content)
	}
	if !strings.Contains(rs[5].Content, NoProgressNote("Bash", 3)) {
		t.Errorf("third call after the reset: %q", rs[5].Content)
	}

	// The same Loop running again starts fresh.
	l = &Loop{}
	runCalls(l, single(calls[:2]...), reverifyDispatcher(markers))
	res = runCalls(l, single(calls[:2]...), reverifyDispatcher(markers))
	noNotes(t, "second run", res)
}

func TestNoProgressPollingToolsUseHigherThresholds(t *testing.T) {
	var calls []ToolUse
	for i := 0; i < 15; i++ {
		in, _ := json.Marshal(map[string]any{"task_id": "b1", "timeout": 1000 * (i + 1)})
		calls = append(calls, ToolUse{Name: "TaskOutput", Input: in})
	}
	d := &scriptDispatcher{out: func(ToolUse, int) ToolResult { return ToolResult{Content: "still running"} }}
	res := runCalls(&Loop{}, single(calls...), d)
	warn := DefaultLoopGuard.WarnAfter * pollingFactor
	rs := toolResults(res.Messages)
	if strings.Contains(rs[warn-2].Content, "Note:") || !strings.Contains(rs[warn-1].Content, NoProgressNote("TaskOutput", warn)) {
		t.Errorf("polling threshold: %q / %q", rs[warn-2].Content, rs[warn-1].Content)
	}
}

type recordedCall struct {
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Output  string          `json:"output"`
	IsError bool            `json:"is_error"`
}

func loadRecording(t *testing.T, path string) []recordedCall {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []recordedCall
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c recordedCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// replayRecording runs a recording through a Loop, one call per round, each
// call answered with its recorded result.
func replayRecording(rec []recordedCall) (Result, *scriptDispatcher) {
	rounds := make([][]ToolUse, len(rec))
	for i, c := range rec {
		rounds[i] = []ToolUse{{Name: c.Name, Input: c.Input}}
	}
	d := &scriptDispatcher{out: func(_ ToolUse, n int) ToolResult {
		return ToolResult{Content: rec[n].Output, IsError: rec[n].IsError}
	}}
	return runCalls(&Loop{}, rounds, d), d
}

// TestNoProgressReplaysReverificationRun replays a real run in which the
// model finished the task, then re-verified it about twenty times with
// slightly different commands (echo markers, segment order) that the
// exact-repeat guard never matched.
func TestNoProgressReplaysReverificationRun(t *testing.T) {
	rec := loadRecording(t, "testdata/equivalent_tool_calls.jsonl")
	if len(rec) != 35 {
		t.Fatalf("fixture has %d calls, want 35", len(rec))
	}
	// The re-verification starts with the first `coqc X && ls -l Y` (call 22).
	start := -1
	for i, c := range rec {
		var in struct{ Command string }
		_ = json.Unmarshal(c.Input, &in)
		if in.Command == "coqc /workspace/plus_comm.v && ls -l /workspace/plus_comm.vo" {
			start = i
			break
		}
	}
	if start != 21 {
		t.Fatalf("re-verification starts at call %d, want 22", start+1)
	}
	res, d := replayRecording(rec)
	rs := toolResults(res.Messages)
	first := -1
	for i, r := range rs {
		if strings.Contains(r.Content, "equivalent to this one") {
			first = i
			break
		}
		if strings.Contains(r.Content, "Note:") {
			t.Errorf("call %d: unexpected note %q", i+1, r.Content)
		}
	}
	if first < start || first > start+2 {
		t.Fatalf("first no-progress note at call %d, want within 3 calls of %d", first+1, start+1)
	}
	if res.Reason != ReasonCompleted || len(d.calls) != 35 {
		t.Fatalf("reason %q after %d calls", res.Reason, len(d.calls))
	}
	last := 0
	for n := 1; n <= 40; n++ {
		if strings.Contains(rs[34].Content, NoProgressNote("Bash", n)) {
			last = n
		}
	}
	stop := noProgressStopFactor * DefaultLoopGuard.StopAfter
	t.Logf("recorded run: 35 calls; re-verification from call %d; first note at call %d; "+
		"call 35 is equivalent repeat %d, so the stop (repeat %d) would come at call %d had the run continued",
		start+1, first+1, last, stop, 35+stop-last)
}

// TestNoProgressReplaysLoopWithRecordedOutputs replays the 101-call loop with
// its recorded results: the no-progress detector ends the run on the 16th
// equivalent `ls -l plus_comm.vo && echo ...` check, before the exact-repeat
// streak (which starts at call 54) reaches its stop at call 61.
func TestNoProgressReplaysLoopWithRecordedOutputs(t *testing.T) {
	rec := loadRecording(t, "testdata/repeated_tool_calls.jsonl")
	if len(rec) != 101 {
		t.Fatalf("fixture has %d calls, want 101", len(rec))
	}
	res, d := replayRecording(rec)
	if res.Reason != ReasonRepeatedToolCalls {
		t.Fatalf("reason = %q", res.Reason)
	}
	if !strings.Contains(res.Err.Error(), "equivalent Bash calls were repeated 16 times with no progress") {
		t.Fatalf("err = %v", res.Err)
	}
	if len(d.calls) != 55 {
		t.Fatalf("stopped after call %d, want 55", len(d.calls))
	}
	first := -1
	for i, r := range toolResults(res.Messages) {
		if strings.Contains(r.Content, "Note:") {
			first = i
			break
		}
	}
	t.Logf("recorded run: 101 calls; first no-progress note at call %d; stopped after call %d", first+1, len(d.calls))
}
