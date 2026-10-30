package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// effortRecorder is an offline provider that records each request's effort.
type effortRecorder struct{ efforts []string }

func (r *effortRecorder) Name() string { return "openrouter" }
func (r *effortRecorder) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	r.efforts = append(r.efforts, req.Effort)
	return provider.ChatResponse{Content: "ok", FinishReason: provider.FinishStop}, nil
}
func (r *effortRecorder) StreamChat(ctx context.Context, req provider.ChatRequest, _ func(string)) (provider.ChatResponse, error) {
	return r.Chat(ctx, req)
}

// localSettings reads <cwd>/.gocode/settings.local.json as an object.
func localSettings(t *testing.T, cwd string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cwd, ".gocode", "settings.local.json"))
	if err != nil {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("settings.local.json: %v", err)
	}
	return obj
}

// TestResolveEffort_Precedence: --effort > persisted effortLevel > unset, the
// flag's "auto" overrides a persisted level, and an invalid flag is an error
// that lists the valid levels and is surfaced by buildCaller.
func TestResolveEffort_Precedence(t *testing.T) {
	none := providerTestEnv(t, nil, "")
	if s := newPrintSession(none, nil); s.Effort != "" {
		t.Errorf("unset: Effort = %q", s.Effort)
	}
	env := providerTestEnv(t, nil, `{"effortLevel":"low"}`)
	if s := newPrintSession(env, nil); s.Effort != "low" {
		t.Errorf("setting: Effort = %q, want low", s.Effort)
	}
	if s := newPrintSession(env, &DispatchDefaults{Effort: "HIGH"}); s.Effort != "high" {
		t.Errorf("flag beats setting: Effort = %q, want high", s.Effort)
	}
	if s := newPrintSession(env, &DispatchDefaults{Effort: "auto"}); s.Effort != "" {
		t.Errorf("flag auto: Effort = %q, want unset", s.Effort)
	}
	bad := providerTestEnv(t, nil, `{"effortLevel":"bogus"}`)
	if s := newPrintSession(bad, nil); s.Effort != "" {
		t.Errorf("invalid persisted value should be ignored, got %q", s.Effort)
	}

	s := newPrintSession(env, &DispatchDefaults{Effort: "extreme"})
	_, err := s.buildCaller()
	if err == nil {
		t.Fatal("invalid --effort: want error")
	}
	for _, want := range []string{`"extreme"`, "low, medium, high, xhigh, max"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestDefaultNewCaller_AppliesEffort: the resolved level reaches the provider
// request through the real caller builder.
func TestDefaultNewCaller_AppliesEffort(t *testing.T) {
	s := &printSession{Provider: providerAnthropic, Model: "m", APIKey: "k", Effort: "xhigh"}
	c, err := defaultNewCaller(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.(*provider.ModelCaller).CurrentEffort(); got != "xhigh" {
		t.Fatalf("caller effort = %q", got)
	}
}

// TestEffortCommand_SetPersistReload: /effort high sets the live caller,
// persists effortLevel to local settings and the next session loads it; max is
// session-only; auto clears and removes the saved key; -p never persists.
func TestEffortCommand_SetPersistReload(t *testing.T) {
	env, _, cwd := settingsTestEnv(t)
	rec := &effortRecorder{}
	pc := provider.NewModelCaller(rec, "m")
	ps := &printSession{}
	conv := &conversation.Conversation{Model: pc}
	st := effortState{conv: conv, print: ps, env: env}

	msg := st.command("high", true)
	if !strings.Contains(msg, "Set effort level to high (saved as your default for new sessions)") {
		t.Errorf("msg = %q", msg)
	}
	if pc.CurrentEffort() != "high" || ps.Effort != "high" {
		t.Errorf("live effort = %q / %q", pc.CurrentEffort(), ps.Effort)
	}
	_, _ = pc.CallModel(context.Background(), agent.ModelRequest{})
	if len(rec.efforts) != 1 || rec.efforts[0] != "high" {
		t.Errorf("request efforts = %v", rec.efforts)
	}
	if got := localSettings(t, cwd)["effortLevel"]; got != "high" {
		t.Fatalf("persisted effortLevel = %v", got)
	}
	if s := newPrintSession(env, nil); s.Effort != "high" {
		t.Fatalf("next session Effort = %q, want high", s.Effort)
	}

	// max: applied live, but session-only (the saved default stays high).
	if msg := st.command("max", true); !strings.Contains(msg, "Set effort level to max (this session only)") {
		t.Errorf("max msg = %q", msg)
	}
	if pc.CurrentEffort() != "max" || localSettings(t, cwd)["effortLevel"] != "high" {
		t.Errorf("max: live %q saved %v", pc.CurrentEffort(), localSettings(t, cwd)["effortLevel"])
	}

	// -p (persist=false): live only.
	if msg := st.command("low", false); !strings.Contains(msg, "(this session only)") {
		t.Errorf("-p msg = %q", msg)
	}
	if localSettings(t, cwd)["effortLevel"] != "high" {
		t.Error("-p /effort persisted")
	}

	// current / help / invalid.
	if msg := st.command("current", true); !strings.HasPrefix(msg, "Current effort level: low (") {
		t.Errorf("current = %q", msg)
	}
	if msg := st.command("--help", true); !strings.Contains(msg, "- auto: Use the default effort level for your model") {
		t.Errorf("help = %q", msg)
	}
	if msg := st.command("turbo", true); msg != "Invalid argument: turbo. Valid options are: low, medium, high, xhigh, max, auto" {
		t.Errorf("invalid = %q", msg)
	}
	if pc.CurrentEffort() != "low" {
		t.Error("invalid argument changed the level")
	}

	// auto clears live and removes the saved key.
	if msg := st.command("auto", true); msg != "Effort level set to auto" {
		t.Errorf("auto msg = %q", msg)
	}
	if pc.CurrentEffort() != "" || ps.Effort != "" {
		t.Error("auto did not clear the live level")
	}
	if _, ok := localSettings(t, cwd)["effortLevel"]; ok {
		t.Error("auto did not remove effortLevel")
	}
	if s := newPrintSession(env, nil); s.Effort != "" {
		t.Errorf("after auto next session Effort = %q", s.Effort)
	}
	if msg := st.command("current", true); msg != "Effort level: auto" {
		t.Errorf("current after auto = %q", msg)
	}
}

// TestEffortIsBuiltinCommand: /effort is a recognized built-in (so it never
// falls through to the model or the unknown-command path).
func TestEffortIsBuiltinCommand(t *testing.T) {
	tbl := builtincmds.NewTable()
	b, ok := tbl.Lookup("effort")
	if !ok || b.Description != "Set effort level for model usage" {
		t.Fatalf("lookup = %+v, %v", b, ok)
	}
	eff, ok := tbl.Dispatch("/effort high")
	if !ok || eff.Kind != builtincmds.EffectOpenDialog || eff.Dialog != "effort" || strings.TrimSpace(eff.Args) != "high" {
		t.Fatalf("dispatch = %+v", eff)
	}
	conv := &conversation.Conversation{Builtins: tbl}
	if !isRecognizedCommand(conv, "/effort") || !isRecognizedCommand(conv, "/effort max") {
		t.Fatal("/effort not recognized as a command")
	}
}

// TestPrintSession_EffortAndStatus: in -p, `/effort high` reports a
// session-only change, and /status shows the effort line.
func TestPrintSession_EffortAndStatus(t *testing.T) {
	env, _, _ := settingsTestEnv(t)
	for _, tc := range []struct{ prompt, want string }{
		{"/effort high", "Set effort level to high (this session only)"},
		{"/effort", "Usage: /effort <low|medium|high|xhigh|max|auto>"},
		{"/status", "Effort:        medium"},
	} {
		var out strings.Builder
		env.Stdout = &out
		s := &printSession{
			Mode:   toolreg.ModeDefault,
			Effort: "medium",
			newCaller: func(ps *printSession) (agent.ModelCaller, error) {
				mc := provider.NewModelCaller(&effortRecorder{}, "m")
				mc.SetEffort(ps.Effort)
				return mc, nil
			},
		}
		if code := runPrintSession(env, s, tc.prompt); code != 0 {
			t.Fatalf("%s: exit %d", tc.prompt, code)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: stdout = %q, want %q", tc.prompt, out.String(), tc.want)
		}
	}
}

// TestInteractiveEffortCommand: `/effort high` in the REPL applies live,
// persists, shows the confirmation (not the "isn't available" fallback) and
// re-renders the banner with the effort; a bare /effort opens the picker.
func TestInteractiveEffortCommand(t *testing.T) {
	home := t.TempDir()
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	pc := provider.NewModelCaller(&effortRecorder{}, "my-model")
	sess.print.newCaller = func(*printSession) (agent.ModelCaller, error) { return pc, nil }
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	defer prog.Quit()
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 10*time.Second)

	submitLine(prog, "/effort high")
	if !sawSystemMsg(prog, "Set effort level to high (saved as your default for new sessions)") {
		t.Fatal("/effort high did not confirm")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		b, ok := m.(ui.SetBannerMsg)
		return ok && b.Banner != nil && b.Banner.Model == "my-model with high effort"
	}, 10*time.Second) {
		t.Fatal("banner did not show the effort")
	}
	if pc.CurrentEffort() != "high" {
		t.Fatalf("live effort = %q", pc.CurrentEffort())
	}
	if got := localSettings(t, home)["effortLevel"]; got != "high" {
		t.Fatalf("persisted effortLevel = %v", got)
	}
	if prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "isn't available")
	}, 200*time.Millisecond) {
		t.Fatal("/effort fell into the unavailable-dialog fallback")
	}

	submitLine(prog, "/status")
	if !sawSystemMsg(prog, "Effort:        high") {
		t.Fatal("/status did not show the effort")
	}

	submitLine(prog, "/effort")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		if !ok || d.Dialog == nil || d.Dialog.Kind != ui.DialogList || d.Dialog.Title != "Effort" {
			return false
		}
		var vals []string
		for _, it := range d.Dialog.Items {
			vals = append(vals, it.Value)
		}
		return strings.Join(vals, ",") == "low,medium,high,xhigh,max,auto"
	}, 10*time.Second) {
		t.Fatal("bare /effort did not open the effort picker")
	}
	submitLine(prog, "low")
	if !sawSystemMsg(prog, "Set effort level to low") {
		t.Fatal("picker choice was not applied")
	}
	if pc.CurrentEffort() != "low" {
		t.Fatalf("live effort after picker = %q", pc.CurrentEffort())
	}
}

// TestSubagentFactoryInheritsEffort: a subagent caller starts with the
// parent's live effort; overriding it does not touch the parent.
func TestSubagentFactoryInheritsEffort(t *testing.T) {
	parent := provider.NewModelCaller(&effortRecorder{}, "anthropic/claude-opus-5-5")
	parent.SetEffort("high")
	child, ok := subagentModelFactory(providerOpenRouter, parent, nil)("sonnet").(*provider.ModelCaller)
	if !ok || child == parent {
		t.Fatal("factory did not build a fresh provider caller")
	}
	if child.CurrentEffort() != "high" {
		t.Fatalf("child effort = %q, want inherited high", child.CurrentEffort())
	}
	child.SetEffort("low")
	if parent.CurrentEffort() != "high" {
		t.Fatal("child override leaked into the parent")
	}
}
