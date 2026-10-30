package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/ui"
)

// usageCaller answers every round with a short text and reports a fixed
// input-token usage (settable between turns), which anchors the measured
// context size.
type usageCaller struct {
	mu    sync.Mutex
	input int
	model string
	calls int
}

func (c *usageCaller) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("ok")), StopReason: agent.StopEndTurn}, nil
}

func (c *usageCaller) LastTurnUsage() (string, apiclient.Usage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model, apiclient.Usage{InputTokens: c.input}, true
}

func (c *usageCaller) setInput(n int) {
	c.mu.Lock()
	c.input = n
	c.mu.Unlock()
}

func (c *usageCaller) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// sentSince returns the messages recorded after index from.
func (p *fakeProgram) sentSince(from int) []tea.Msg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tea.Msg(nil), p.sent[from:]...)
}

func (p *fakeProgram) sentLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func toastTexts(msgs []tea.Msg) []string {
	var out []string
	for _, m := range msgs {
		if t, ok := m.(ui.ToastMsg); ok {
			out = append(out, t.Text)
		}
	}
	return out
}

// runAndSettle submits input and waits for that turn to be fully torn down.
func runAndSettle(t *testing.T, drv *turnDriver, prog *fakeProgram, input string) []tea.Msg {
	t.Helper()
	from := prog.sentLen()
	drv.submit(input)
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && s.Status.Kind == ui.StatusIdle
	}, 3*time.Second) {
		t.Fatalf("turn %q never ended", input)
	}
	drv.waitIdle(2 * time.Second)
	// The context refresh follows the idle status in the teardown.
	time.Sleep(50 * time.Millisecond)
	return prog.sentSince(from)
}

func lastContextPercent(msgs []tea.Msg) (int, bool) {
	pct, ok := 0, false
	for _, m := range msgs {
		if f, is := m.(ui.SetFooterContextMsg); is {
			pct, ok = f.Percent, true
		}
	}
	return pct, ok
}

func lastContextUsage(msgs []tea.Msg) (ui.SetContextUsageMsg, bool) {
	var u ui.SetContextUsageMsg
	ok := false
	for _, m := range msgs {
		if v, is := m.(ui.SetContextUsageMsg); is {
			u, ok = v, true
		}
	}
	return u, ok
}

// After /clear the footer percent and the sidebar gauge are re-sent with the
// fresh (empty-history) figure instead of the stale last-response one, and the
// 80% warning — shown once on the crossing — re-arms so the next crossing
// warns again.
func TestContextRefreshAfterClearAndWarningRearm(t *testing.T) {
	caller := &usageCaller{}
	conv := conversation.New(caller, fakeToolDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}
	window := drv.cmdInfo().capabilities().ContextWindow
	if window <= 0 {
		t.Skip("no context window for the default model")
	}

	caller.setInput(window * 90 / 100)
	msgs := runAndSettle(t, drv, prog, "big")
	if pct, ok := lastContextPercent(msgs); !ok || pct != 90 {
		t.Fatalf("after the big turn: percent %d (sent %v)", pct, ok)
	}
	if got := toastTexts(msgs); len(got) != 1 || got[0] != "Context 80% full — /compact to free space" {
		t.Fatalf("80%% warning toasts: %q", got)
	}
	// Still above 80%: no repeat warning.
	msgs = runAndSettle(t, drv, prog, "again")
	if got := toastTexts(msgs); len(got) != 0 {
		t.Fatalf("warning repeated without a new crossing: %q", got)
	}

	msgs = runAndSettle(t, drv, prog, "/clear")
	if pct, ok := lastContextPercent(msgs); !ok || pct != 0 {
		t.Fatalf("after /clear: percent %d (sent %v), want a fresh 0", pct, ok)
	}
	if u, ok := lastContextUsage(msgs); !ok || u.Used != 0 || u.Window != window {
		t.Fatalf("after /clear: gauge %+v (sent %v)", u, ok)
	}
	if !hasMsg(msgs, func(m tea.Msg) bool { _, ok := m.(ui.SetFooterTokensMsg); return ok }) {
		t.Fatal("footer tokens not re-sent after /clear")
	}

	// Crossing 95% now warns (the 80% warning re-armed after /clear too:
	// the jump straight to 96% reports the higher level once).
	caller.setInput(window * 96 / 100)
	msgs = runAndSettle(t, drv, prog, "huge")
	if got := toastTexts(msgs); len(got) != 1 || got[0] != "Context 95% full — /compact to free space" {
		t.Fatalf("95%% warning toasts: %q", got)
	}
}

func hasMsg(msgs []tea.Msg, pred func(tea.Msg) bool) bool {
	for _, m := range msgs {
		if pred(m) {
			return true
		}
	}
	return false
}

// With no measured response (just compacted / resumed — the conversation
// drops the stale measurement), the gauge shows an estimate of the current
// history rather than disappearing or keeping the old number.
func TestContextPercentEstimatesUnmeasuredHistory(t *testing.T) {
	conv := conversation.New(&usageCaller{}, fakeToolDispatcher{})
	drv := &turnDriver{conv: conv}
	if got := drv.contextPercent(); got != -1 {
		t.Fatalf("fresh session: %d, want -1 (nothing to show)", got)
	}
	window := drv.cmdInfo().capabilities().ContextWindow
	if window <= 0 {
		t.Skip("no context window for the default model")
	}
	// A rebuilt (post-compaction) history of ~window/2 tokens at chars/4.
	conv.History = []agent.Message{agent.UserMessage(agent.TextBlock(strings.Repeat("x", window*2)))}
	if conv.ContextTokensMeasured() {
		t.Fatal("history should be unmeasured")
	}
	got := drv.contextPercent()
	if got < 45 || got > 55 {
		t.Fatalf("estimated percent %d, want ~50", got)
	}
	if u := drv.contextUsage(); u.Window != window || u.Used <= 0 {
		t.Fatalf("estimated gauge %+v", u)
	}
}

func TestObserveContextThresholds(t *testing.T) {
	var l limitState
	steps := []struct {
		pct  int
		want string
	}{
		{-1, ""}, {50, ""}, {80, "Context 80% full — /compact to free space"}, {85, ""},
		{95, "Context 95% full — /compact to free space"}, {99, ""}, {90, ""},
		{96, "Context 95% full — /compact to free space"}, {10, ""},
		{82, "Context 80% full — /compact to free space"},
	}
	for i, s := range steps {
		if got := l.observeContext(s.pct); got != s.want {
			t.Fatalf("step %d (pct %d): %q want %q", i, s.pct, got, s.want)
		}
	}
}

// writeJSON writes a settings file.
func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func capEnv(t *testing.T, vars map[string]string) (*Env, string, string) {
	t.Helper()
	root := t.TempDir()
	home, cwd, managed := filepath.Join(root, "home"), filepath.Join(root, "proj"), filepath.Join(root, "managed")
	for _, d := range []string{home, cwd, managed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := &Env{
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return vars[k]
		},
		Cwd:                func() (string, error) { return cwd, nil },
		ManagedSettingsDir: managed,
	}
	return env, cwd, managed
}

func TestLoadMaxSessionCost(t *testing.T) {
	env, _, _ := capEnv(t, nil)
	if got := loadMaxSessionCost(env); got != 0 {
		t.Fatalf("unset: %v", got)
	}

	env, cwd, managed := capEnv(t, nil)
	writeJSON(t, filepath.Join(cwd, ".gocode", "settings.json"), `{"maxSessionCostUsd": 5}`)
	writeJSON(t, filepath.Join(cwd, ".gocode", "settings.local.json"), `{"maxSessionCostUsd": 2.5}`)
	if got := loadMaxSessionCost(env); got != 2.5 {
		t.Fatalf("local over project: %v", got)
	}

	env, cwd, _ = capEnv(t, map[string]string{maxSessionCostEnv: "7"})
	writeJSON(t, filepath.Join(cwd, ".gocode", "settings.local.json"), `{"maxSessionCostUsd": 2.5}`)
	if got := loadMaxSessionCost(env); got != 7 {
		t.Fatalf("env over settings: %v", got)
	}

	env, cwd, managed = capEnv(t, map[string]string{maxSessionCostEnv: "7"})
	writeJSON(t, filepath.Join(managed, "managed-settings.json"), `{"maxSessionCostUsd": 1}`)
	if got := loadMaxSessionCost(env); got != 1 {
		t.Fatalf("policy wins: %v", got)
	}
	_ = cwd

	env, _, _ = capEnv(t, map[string]string{maxSessionCostEnv: "nonsense"})
	if got := loadMaxSessionCost(env); got != 0 {
		t.Fatalf("invalid env: %v", got)
	}
	for _, v := range []any{0.0, -3.0, "abc", true} {
		if _, ok := parseCostCap(v); ok {
			t.Fatalf("parseCostCap(%v) accepted", v)
		}
	}
	if v, ok := parseCostCap("$1.25"); !ok || v != 1.25 {
		t.Fatalf("parseCostCap($1.25) = %v %v", v, ok)
	}
}

// The spend cap: 80% of it warns once (toast), reaching it refuses the next
// model turn with an actionable error (the model is never called) while slash
// commands keep working; a free model costs $0 and never trips it.
func TestSpendCapWarnsThenRefuses(t *testing.T) {
	caller := &usageCaller{model: "gocode-opus-5", input: 1000}
	conv := conversation.New(caller, fakeToolDispatcher{})
	conv.Cost = cost.NewLedger()
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	perTurn, _ := cost.CostForModel("gocode-opus-5", cost.Usage{InputTokens: 1000})
	if perTurn <= 0 {
		t.Fatal("priced model has no cost")
	}
	// Cap = 1.2 turns: turn 1 crosses 80% (warn), turn 2 reaches the cap.
	drv.limits.maxCostUSD = perTurn * 1.2

	msgs := runAndSettle(t, drv, prog, "one")
	got := toastTexts(msgs)
	if len(got) != 1 || !strings.Contains(got[0], "of the session spend cap used") {
		t.Fatalf("80%% spend warning: %q", got)
	}
	msgs = runAndSettle(t, drv, prog, "two")
	if got := toastTexts(msgs); len(got) != 0 {
		t.Fatalf("spend warning repeated: %q", got)
	}
	calls := caller.callCount()

	from := prog.sentLen()
	drv.submit("three")
	if !prog.waitFor(func(m tea.Msg) bool {
		e, ok := m.(ui.AppendErrorMsg)
		return ok && strings.Contains(e.Text, "Session spend cap reached") &&
			strings.Contains(e.Text, maxSessionCostSetting) && strings.Contains(e.Text, maxSessionCostEnv)
	}, 2*time.Second) {
		t.Fatalf("no spend-cap refusal; sent %v", prog.sentSince(from))
	}
	drv.waitIdle(time.Second)
	if caller.callCount() != calls {
		t.Fatal("the model was called past the spend cap")
	}
	if msg := drv.spendCapError(); msg == "" {
		t.Fatal("spendCapError empty at the cap")
	}
	// A local command still runs.
	if !isLocalCommandInput("/usage") || isLocalCommandInput("hello /usage") {
		t.Fatal("isLocalCommandInput")
	}

	// A free model never trips the cap.
	free := &usageCaller{model: "vendor/model:free", input: 1_000_000}
	fconv := conversation.New(free, fakeToolDispatcher{})
	fconv.Cost = cost.NewLedger()
	fdrv := &turnDriver{conv: fconv, prog: prog}
	fdrv.limits.maxCostUSD = 0.01
	runAndSettle(t, fdrv, prog, "free one")
	runAndSettle(t, fdrv, prog, "free two")
	if free.callCount() != 2 || fdrv.spendCapError() != "" {
		t.Fatalf("free model blocked: calls=%d err=%q", free.callCount(), fdrv.spendCapError())
	}
}
