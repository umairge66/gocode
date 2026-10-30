package cli

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ui"
)

// editInput is the tool_use input for the scripted Edit call the fake caller
// emits — enough for toolArgs (file_path) and editDiff (old/new).
var editInput = json.RawMessage(`{"file_path":"/x.go","old_string":"a","new_string":"b"}`)

// scriptedCaller is a fake agent.ModelCaller (and TurnUsageReporter) that emits
// an Edit tool_use on its first round and a final "done" text turn afterwards.
// An optional gate lets a chosen round block until released, so a test can
// observe the live spinner-meta ticker fire mid-turn.
type scriptedCaller struct {
	mu     sync.Mutex
	calls  int
	gate   chan struct{} // nil disables gating
	gateOn int           // which call number (1-based) blocks on the gate
}

func (c *scriptedCaller) CallModel(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	gate := c.gate
	c.mu.Unlock()

	if gate != nil && n == c.gateOn {
		select {
		case <-gate:
		case <-ctx.Done():
			return agent.ModelResponse{}, ctx.Err()
		}
	}

	if n == 1 {
		return agent.ModelResponse{
			Message:    agent.AssistantMessage(agent.ToolUseBlock("tid-1", "Edit", editInput)),
			StopReason: agent.StopToolUse,
		}, nil
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("done")),
		StopReason: agent.StopEndTurn,
	}, nil
}

// LastTurnUsage reports a small fixed per-round usage so the cost ledger — and
// thus conv.OutputTokens() — grows as the turn runs.
func (c *scriptedCaller) LastTurnUsage() (string, apiclient.Usage, bool) {
	return "fake-model", apiclient.Usage{OutputTokens: 5}, true
}

// fakeToolDispatcher returns a scripted result for every tool call.
type fakeToolDispatcher struct{ result agent.ToolResult }

func (d fakeToolDispatcher) Dispatch(_ context.Context, _ agent.ToolUse) (agent.ToolResult, error) {
	return d.result, nil
}

// runDriverTurn builds a turnDriver over a fake program and the given caller +
// dispatcher, starts a turn, and returns the running program for assertions.
func runDriverTurn(t *testing.T, caller agent.ModelCaller, disp agent.ToolDispatcher) *fakeProgram {
	t.Helper()
	conv := conversation.New(caller, disp)
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	drv := &turnDriver{conv: conv, prog: prog}
	drv.submit("go")
	return prog
}

// TestInteractiveTurn_ToolCallBlock asserts a turn that runs a tool surfaces a
// start block (AppendToolCallMsg) and an end update (UpdateToolCallMsg) with the
// right ID, user-facing verb, arg summary, result text and error flag.
func TestInteractiveTurn_ToolCallBlock(t *testing.T) {
	caller := &scriptedCaller{}
	disp := fakeToolDispatcher{result: agent.ToolResult{Content: "edited ok", IsError: false}}
	prog := runDriverTurn(t, caller, disp)
	defer prog.Quit()

	start := prog.waitFor(func(m tea.Msg) bool {
		a, ok := m.(ui.AppendToolCallMsg)
		return ok && a.Tool.ID == "tid-1" && a.Tool.Name == "Update" && a.Tool.Args == "/x.go"
	}, 2*time.Second)
	if !start {
		t.Fatal("tool START block (AppendToolCallMsg) with verb/args was not sent")
	}

	end := prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.UpdateToolCallMsg)
		return ok && u.Tool.ID == "tid-1" && u.Tool.ResultText == "edited ok" && !u.Tool.IsError
	}, 2*time.Second)
	if !end {
		t.Fatal("tool END update (UpdateToolCallMsg) with the result was not sent")
	}
}

// TestInteractiveTurn_ToolCallError asserts a failing tool result propagates the
// IsError flag onto the end update.
func TestInteractiveTurn_ToolCallError(t *testing.T) {
	caller := &scriptedCaller{}
	disp := fakeToolDispatcher{result: agent.ToolResult{Content: "boom", IsError: true}}
	prog := runDriverTurn(t, caller, disp)
	defer prog.Quit()

	end := prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.UpdateToolCallMsg)
		return ok && u.Tool.ID == "tid-1" && u.Tool.IsError && u.Tool.ResultText == "boom"
	}, 2*time.Second)
	if !end {
		t.Fatal("failed tool call did not report IsError on the end update")
	}

	// A failed edit must NOT produce a diff block.
	if prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.AppendDiffMsg); return ok }, 300*time.Millisecond) {
		t.Fatal("a diff block was sent for a failed edit")
	}
}

// TestInteractiveTurn_EditDiff asserts a successful edit tool produces an
// AppendDiffMsg for the changed file, with the old→new lines.
func TestInteractiveTurn_EditDiff(t *testing.T) {
	caller := &scriptedCaller{}
	disp := fakeToolDispatcher{result: agent.ToolResult{Content: "edited ok"}}
	prog := runDriverTurn(t, caller, disp)
	defer prog.Quit()

	got := prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.AppendDiffMsg)
		if !ok || d.Diff.Path != "/x.go" {
			return false
		}
		added, removed := d.Diff.Stat()
		return added == 1 && removed == 1
	}, 2*time.Second)
	if !got {
		t.Fatal("successful edit did not produce an AppendDiffMsg with +1/-1 lines")
	}
}

// TestInteractiveTurn_SpinnerMeta asserts the spinner-meta ticker pushes the
// elapsed seconds and the cumulative output-token count during the turn. The
// second model round is gated so the turn stays in flight long enough for a
// ~1s tick to fire, by which point round one's tokens are already recorded.
func TestInteractiveTurn_SpinnerMeta(t *testing.T) {
	gate := make(chan struct{})
	caller := &scriptedCaller{gate: gate, gateOn: 2}
	disp := fakeToolDispatcher{result: agent.ToolResult{Content: "edited ok"}}
	prog := runDriverTurn(t, caller, disp)
	defer prog.Quit()

	meta := prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetSpinnerMetaMsg)
		return ok && s.Elapsed >= 1 && s.Tokens > 0
	}, 3*time.Second)
	close(gate) // let the turn finish regardless of the assertion outcome
	if !meta {
		t.Fatal("no SetSpinnerMetaMsg with elapsed>=1 and tokens>0 was sent during the turn")
	}
}

// streamingCaller is a fake StreamingModelCaller + TurnUsageReporter: a single
// model round that streams several text deltas via the StreamHandler (so the
// driver's live per-turn token estimate grows mid-stream) and then returns the
// finalized text. Its real output usage is reported only AFTER the round
// completes (LastTurnUsage), exactly like a provider that emits usage in the
// final chunk — so during streaming the ledger stays at 0 and only the estimate
// moves the counter.
type streamingCaller struct {
	deltas    []string
	finalText string
	realOut   int
}

func (c *streamingCaller) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(c.finalText)),
		StopReason: agent.StopEndTurn,
	}, nil
}

func (c *streamingCaller) CallModelStream(ctx context.Context, _ agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	for _, d := range c.deltas {
		if err := ctx.Err(); err != nil {
			return agent.ModelResponse{}, err
		}
		if h != nil && h.OnText != nil {
			h.OnText(d)
		}
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(c.finalText)),
		StopReason: agent.StopEndTurn,
	}, nil
}

// LastTurnUsage reports the authoritative per-round usage, available only after
// the round returns (as the real provider does).
func (c *streamingCaller) LastTurnUsage() (string, apiclient.Usage, bool) {
	return "fake-model", apiclient.Usage{OutputTokens: c.realOut}, true
}

// spinnerTokenSeq returns the Tokens field of every SetSpinnerMetaMsg recorded
// so far, in send order.
func spinnerTokenSeq(prog *fakeProgram) []int {
	prog.mu.Lock()
	defer prog.mu.Unlock()
	var out []int
	for _, m := range prog.sent {
		if s, ok := m.(ui.SetSpinnerMetaMsg); ok {
			out = append(out, s.Tokens)
		}
	}
	return out
}

// TestInteractiveTurn_LiveTokenEstimateGrows asserts the spinner token figure
// climbs in real time as assistant text streams, driven by the live per-turn
// estimate (the ledger is still 0 during streaming — usage lands only at round
// end). Each delta is 8 bytes, so ceil(bytes/4) yields a strictly increasing
// 2,4,6,8,10 as the five deltas arrive.
func TestInteractiveTurn_LiveTokenEstimateGrows(t *testing.T) {
	deltas := []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd", "eeeeeeee"}
	caller := &streamingCaller{deltas: deltas, finalText: "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee", realOut: 3}
	disp := fakeToolDispatcher{result: agent.ToolResult{}}
	prog := runDriverTurn(t, caller, disp)
	defer prog.Quit()

	// Wait until the stream has pushed its growing estimate (the last delta → 10).
	grew := prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetSpinnerMetaMsg)
		return ok && s.Tokens >= 10
	}, 2*time.Second)
	if !grew {
		t.Fatalf("live token estimate never climbed during streaming; seq=%v", spinnerTokenSeq(prog))
	}

	// The sequence must contain a strictly increasing pair (a later meta with
	// more tokens than an earlier one) produced while streaming.
	seq := spinnerTokenSeq(prog)
	increased := false
	for i := 1; i < len(seq); i++ {
		if seq[i] > seq[0] {
			increased = true
			break
		}
	}
	if !increased {
		t.Fatalf("no SetSpinnerMetaMsg had Tokens greater than an earlier one; seq=%v", seq)
	}

	// At turn end the footer reflects the REAL ledger usage (3), not the larger
	// live estimate — the display-only estimate never corrupts the ledger/footer.
	footerOK := prog.waitFor(func(m tea.Msg) bool {
		f, ok := m.(ui.SetFooterTokensMsg)
		return ok && f.Output == 3
	}, 2*time.Second)
	if !footerOK {
		t.Fatal("footer token total did not settle to the real ledger usage (3)")
	}
}

// TestInteractiveTurn_SpinnerTokensStartAtZeroEachTurn asserts the spinner's
// token figure counts only the current turn: after a turn that used 500 output
// tokens, the next turn starts again from 0 instead of continuing from 500.
func TestInteractiveTurn_SpinnerTokensStartAtZeroEachTurn(t *testing.T) {
	caller := &streamingCaller{deltas: []string{"aaaaaaaa"}, finalText: "aaaaaaaa", realOut: 500}
	conv := conversation.New(caller, fakeToolDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	drv.submit("first")
	if !prog.waitFor(func(m tea.Msg) bool {
		f, ok := m.(ui.SetFooterTokensMsg)
		return ok && f.Output == 500
	}, 2*time.Second) {
		t.Fatal("first turn's usage never reached the ledger")
	}
	drv.waitIdle(2 * time.Second)
	firstTurn := len(spinnerTokenSeq(prog))

	drv.submit("second")
	if !prog.waitFor(func(m tea.Msg) bool {
		f, ok := m.(ui.SetFooterTokensMsg)
		return ok && f.Output == 1000
	}, 2*time.Second) {
		t.Fatal("second turn's usage never reached the ledger")
	}
	seq := spinnerTokenSeq(prog)[firstTurn:]
	if len(seq) == 0 {
		t.Fatal("no spinner updates during the second turn")
	}
	if seq[0] != 0 {
		t.Fatalf("second turn's counter started at %d, want 0; seq=%v", seq[0], seq)
	}
	for _, n := range seq {
		if n > 500 {
			t.Fatalf("second turn's counter carried over the first turn's tokens: seq=%v", seq)
		}
	}
}

// thinkingCaller streams extended thinking (and then text) each round: round 1
// thinks and calls a tool, round 2 thinks again and answers. Usage lands per
// round, after the round returns.
type thinkingCaller struct {
	mu      sync.Mutex
	calls   int
	think   [2]string
	realOut int
}

func (c *thinkingCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	return c.CallModelStream(ctx, req, nil)
}

func (c *thinkingCaller) CallModelStream(_ context.Context, _ agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if h != nil && h.OnThinking != nil {
		h.OnThinking(c.think[min(n, 2)-1])
	}
	if n == 1 {
		return agent.ModelResponse{
			Message:    agent.AssistantMessage(agent.ToolUseBlock("tid-1", "Edit", editInput)),
			StopReason: agent.StopToolUse,
		}, nil
	}
	if h != nil && h.OnText != nil {
		h.OnText("answer!!")
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("answer!!")), StopReason: agent.StopEndTurn}, nil
}

func (c *thinkingCaller) LastTurnUsage() (string, apiclient.Usage, bool) {
	return "fake-model", apiclient.Usage{OutputTokens: c.realOut}, true
}

// TestInteractiveTurn_SpinnerThinkingTokensSplit asserts the spinner meta
// carries a separate ThinkingTokens figure: ceil(thinking bytes/4), cumulative
// across the turn's rounds (NOT reset when the ledger absorbs round 1), never
// above the total, while Tokens still starts at 0 for the turn.
func TestInteractiveTurn_SpinnerThinkingTokensSplit(t *testing.T) {
	caller := &thinkingCaller{think: [2]string{string(make([]byte, 40)), string(make([]byte, 18))}, realOut: 100}
	disp := fakeToolDispatcher{result: agent.ToolResult{Content: "ok"}}
	prog := runDriverTurn(t, caller, disp)
	defer prog.Quit()

	// 40 bytes → 10, then +18 bytes → ceil(58/4) = 15 across both rounds.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetSpinnerMetaMsg)
		return ok && s.ThinkingTokens == 15
	}, 2*time.Second) {
		t.Fatalf("thinking estimate never reached the cumulative 15")
	}
	prog.mu.Lock()
	defer prog.mu.Unlock()
	first := true
	sawTen := false
	for _, m := range prog.sent {
		s, ok := m.(ui.SetSpinnerMetaMsg)
		if !ok {
			continue
		}
		if first {
			if s.Tokens != 0 || s.ThinkingTokens != 0 {
				t.Fatalf("turn's first meta = %+v, want zeros", s)
			}
			first = false
		}
		if s.ThinkingTokens > s.Tokens {
			t.Fatalf("thinking split exceeds the total: %+v", s)
		}
		if s.ThinkingTokens == 10 {
			sawTen = true
		}
		if sawTen && s.ThinkingTokens > 0 && s.ThinkingTokens < 10 {
			t.Fatalf("thinking estimate was reset mid-turn: %+v", s)
		}
	}
	if !sawTen {
		t.Fatal("round 1's thinking estimate (10) was never shown")
	}
}

func TestSpinnerThinkingTokens(t *testing.T) {
	for _, c := range []struct{ chars, total, want int }{
		{0, 0, 0}, {1, 5, 1}, {4, 5, 1}, {5, 5, 2}, {400, 7, 7},
	} {
		if got := spinnerThinkingTokens(c.chars, c.total); got != c.want {
			t.Errorf("spinnerThinkingTokens(%d,%d)=%d want %d", c.chars, c.total, got, c.want)
		}
	}
}
