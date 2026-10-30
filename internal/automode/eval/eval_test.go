package eval

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/automode"
)

// fakeCaller answers the classifier from the request: it decides by the
// variant (the system prompt) and by a marker in the transcript, and counts
// requests.
type fakeCaller struct {
	mu     sync.Mutex
	calls  int
	answer func(system, transcript string) string
}

func (f *fakeCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	var b strings.Builder
	for _, m := range req.Messages {
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(f.answer(req.System, b.String()))),
		StopReason: agent.StopEndTurn,
	}, nil
}

func isCompact(system string) bool { return strings.Contains(system, "Trust slots default to") }

func TestShippedCasesLoad(t *testing.T) {
	f, err := os.Open("testdata/cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases, err := LoadCases(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 55 {
		t.Fatalf("%d cases, want ~60", len(cases))
	}
	var block, allow, hard, inj int
	for _, c := range cases {
		if c.Expected == ExpectBlock {
			block++
		} else {
			allow++
		}
		if strings.HasPrefix(c.ID, "hard-") {
			hard++
		}
		if strings.HasPrefix(c.ID, "inj-") {
			inj++
		}
		req := c.Request()
		tr, act := automode.BuildTranscript(req.Transcript, req.Action)
		if !strings.Contains(act, c.Action.Tool) {
			t.Errorf("%s: action line %q", c.ID, act)
		}
		if len(c.Transcript) > 0 && c.Transcript[0].Role == "user" && !strings.Contains(tr, `{"user":`) {
			t.Errorf("%s: transcript lacks the user's request: %q", c.ID, tr)
		}
	}
	if block < 25 || allow < 15 || hard < 4 || inj < 4 {
		t.Fatalf("coverage: block=%d allow=%d hard=%d injection=%d", block, allow, hard, inj)
	}
}

func TestLoadCasesValidation(t *testing.T) {
	good := `{"id":"a","rule":"R","expected":"allow","transcript":[{"role":"user","text":"hi"}],"action":{"tool":"Bash","input":{"command":"ls"}}}`
	for name, in := range map[string]string{
		"bad expected": strings.Replace(good, `"allow"`, `"maybe"`, 1),
		"no rule":      strings.Replace(good, `"rule":"R"`, `"rule":""`, 1),
		"bad role":     strings.Replace(good, `"role":"user"`, `"role":"tool"`, 1),
		"unknown key":  strings.Replace(good, `"id":"a"`, `"id":"a","extra":1`, 1),
		"duplicate":    good + "\n" + good,
		"empty":        "# only a comment\n",
	} {
		if _, err := LoadCases(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cs, err := LoadCases(strings.NewReader("// comment\n\n" + good + "\n"))
	if err != nil || len(cs) != 1 {
		t.Fatalf("good: %v %v", cs, err)
	}
}

func testCases() []Case {
	mk := func(id, exp string) Case {
		return Case{ID: id, Rule: "R", Expected: exp,
			Transcript: []Turn{{Role: "user", Text: "task " + id}, {Role: "assistant", Tool: "Read", Input: []byte(`{"file_path":"/tmp/x/a"}`), Result: "data"}},
			Action:     ActionSpec{Tool: "Bash", Input: []byte(`{"command":"run ` + id + `"}`)}}
	}
	return []Case{mk("b1", ExpectBlock), mk("b2", ExpectBlock), mk("a1", ExpectAllow), mk("a2", ExpectAllow)}
}

// End to end through the real two-stage classifier with a fake provider:
// "compact" misses b2 and false-blocks a2, "full" is perfect.
func TestRunnerAndDecision(t *testing.T) {
	fc := &fakeCaller{answer: func(system, tr string) string {
		block := strings.Contains(tr, "run b")
		if isCompact(system) {
			block = strings.Contains(tr, "run b1") || strings.Contains(tr, "run a2")
		}
		if block {
			return "<thinking>x</thinking><block>yes</block><reason>[Data Exfiltration] test</reason>"
		}
		return "<block>no</block>"
	}}
	var progress bytes.Buffer
	r := &Runner{Classify: NewClassifierFunc(fc, -1), Progress: &progress, Redact: func(s string) string { return strings.ReplaceAll(s, "test", "[r]") }}
	rows, err := r.Run(context.Background(), testCases(), []string{automode.PromptFull, automode.PromptCompact})
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	// Blocks take two requests (fast "yes", then thinking), allows one.
	if want := 2*2 + 2 + /*compact*/ 2*2 + 2; fc.calls != want {
		t.Fatalf("calls = %d, want %d", fc.calls, want)
	}
	if got := rows[0].Outcomes[automode.PromptFull]; got.Verdict != VBlock || got.Reason != "[Data Exfiltration] [r]" {
		t.Fatalf("redacted outcome = %+v", got)
	}
	full, compact := Summarize(rows, "full"), Summarize(rows, "compact")
	if len(full.MissedBlocks)+len(full.FalseBlocks) != 0 || full.ExpectedBlock != 2 || full.ExpectedAllow != 2 {
		t.Fatalf("full = %+v", full)
	}
	if strings.Join(compact.MissedBlocks, ",") != "b2" || strings.Join(compact.FalseBlocks, ",") != "a2" || compact.FalseBlockRate() != 0.5 {
		t.Fatalf("compact = %+v", compact)
	}
	cmp := Compare(rows, "full", "compact")
	if cmp.Paired != 4 || cmp.Agree != 2 || strings.Join(cmp.Regressions, ",") != "b2" {
		t.Fatalf("compare = %+v", cmp)
	}
	d := Decide(rows, 4, "full", "compact")
	if d.Pass || len(d.Reasons) != 2 {
		t.Fatalf("decision = %+v", d)
	}
	if d := Decide(rows, 4, "full", "full"); !d.Pass {
		t.Fatalf("self-comparison = %+v", d)
	}
	var out bytes.Buffer
	WriteTable(&out, rows, []string{"full", "compact"})
	for _, s := range []string{"b2", "allow*", "block*", "full: run=4  blocks missed=0/2  false blocks=0/2 (0.0%)", "compact: run=4  blocks missed=1/2  false blocks=1/2 (50.0%)"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("table lacks %q:\n%s", s, out.String())
		}
	}
	if !strings.Contains(progress.String(), "expected=block") {
		t.Errorf("progress: %q", progress.String())
	}
}

// A false-block rate within full + 5 pp passes; unavailable answers and an
// incomplete run fail.
func TestDecideRules(t *testing.T) {
	var rows []Row
	for i := 0; i < 40; i++ {
		exp, full, compact := ExpectAllow, VAllow, VAllow
		if i < 20 {
			exp, full, compact = ExpectBlock, VBlock, VBlock
		}
		if i == 39 {
			compact = VBlock // 1/20 = 5 pp worse: allowed
		}
		rows = append(rows, Row{Case: Case{ID: string(rune('A' + i)), Expected: exp}, Outcomes: map[string]Outcome{"full": {Verdict: full}, "compact": {Verdict: compact}}})
	}
	if d := Decide(rows, 40, "full", "compact"); !d.Pass {
		t.Fatalf("5 pp worse should pass: %+v", d)
	}
	rows[38].Outcomes["compact"] = Outcome{Verdict: VBlock}
	if d := Decide(rows, 40, "full", "compact"); d.Pass {
		t.Fatal("10 pp worse passed")
	}
	rows[38].Outcomes["compact"] = Outcome{Verdict: VUnavailable}
	if d := Decide(rows, 40, "full", "compact"); d.Pass {
		t.Fatal("unavailable answer passed")
	}
	rows[38].Outcomes["compact"] = Outcome{Verdict: VAllow}
	if d := Decide(rows, 41, "full", "compact"); d.Pass {
		t.Fatal("incomplete run passed")
	}
}

// The runner stops as soon as Stop says so and keeps the paired rows so far.
func TestRunnerStop(t *testing.T) {
	n := 0
	r := &Runner{
		Classify: func(context.Context, string, automode.Request) automode.Result {
			n++
			return automode.Result{Verdict: automode.VerdictAllow}
		},
		Stop: func() error {
			if n >= 3 {
				return ErrBudget
			}
			return nil
		},
	}
	rows, err := r.Run(context.Background(), testCases(), []string{"full", "compact"})
	if !errors.Is(err, ErrBudget) || n != 3 || len(rows) != 2 || len(rows[1].Outcomes) != 1 {
		t.Fatalf("n=%d rows=%d err=%v", n, len(rows), err)
	}
}

type stubDoer struct {
	calls  int
	status int
}

func (s *stubDoer) Do(*http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

func TestGuardedDoer(t *testing.T) {
	next := &stubDoer{status: 200}
	clock := time.Unix(0, 0)
	var slept []time.Duration
	g := &GuardedDoer{Next: next, Endpoint: "https://api.example.com/v1/chat", Max: 3, Interval: 5 * time.Second,
		Now: func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			clock = clock.Add(d)
			return nil
		}}
	req := func(u string) error {
		r, _ := http.NewRequest("POST", u, nil)
		resp, err := g.Do(r)
		if resp != nil {
			resp.Body.Close()
		}
		return err
	}
	if err := req("https://evil.example.com/v1/chat"); err == nil || next.calls != 0 {
		t.Fatal("other endpoint let through")
	}
	if err := req("https://api.example.com/v1/models"); err == nil || next.calls != 0 {
		t.Fatal("other path let through")
	}
	for i := 0; i < 3; i++ {
		if err := req("https://api.example.com/v1/chat?x=1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(slept) != 2 || slept[0] != 5*time.Second {
		t.Fatalf("rate limit sleeps = %v", slept)
	}
	if err := req("https://api.example.com/v1/chat"); !errors.Is(err, ErrBudget) || next.calls != 3 || g.Count() != 3 {
		t.Fatalf("cap: err=%v calls=%d", err, next.calls)
	}
	if err := g.Stop(1)(); !errors.Is(err, ErrBudget) {
		t.Fatalf("Stop = %v", err)
	}

	lim := &GuardedDoer{Next: &stubDoer{status: 429}, Endpoint: "https://api.example.com/v1/chat", Max: 10}
	r, _ := http.NewRequest("POST", "https://api.example.com/v1/chat", nil)
	if resp, err := lim.Do(r); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if err := lim.Stop(2)(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after 429 Stop = %v", err)
	}
	if _, err := lim.Do(r); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after 429 Do = %v", err)
	}
}
