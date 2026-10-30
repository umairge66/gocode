package hookhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mods"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

type fakeMods struct {
	started  []string
	drop     string
	extra    []string
	turns    []string
	complete []string
}

func (f *fakeMods) SessionStart(_ context.Context, cwd string, _ bool) {
	f.started = append(f.started, cwd)
}
func (f *fakeMods) PromptSubmit(ctx context.Context, text string, classic func(context.Context, string) string) (string, string, []string) {
	if f.drop != "" {
		return text, f.drop, nil
	}
	if b := classic(ctx, text); b != "" {
		return text, b, nil
	}
	return text, "", f.extra
}
func (f *fakeMods) CommandRun(ctx context.Context, _, args string, _ map[string]any, run func(context.Context, string) (*string, error)) (mods.CommandOutcome, error) {
	t, err := run(ctx, args)
	out := mods.CommandOutcome{Ran: true, RanArgs: args}
	if t != nil {
		out.Text, out.HasText = *t, true
	}
	return out, err
}
func (f *fakeMods) TurnStart(_ context.Context, text, id string) {
	f.turns = append(f.turns, text+"#"+id)
}
func (f *fakeMods) TurnComplete(_ context.Context, answer, id string, _ time.Duration, _ bool) {
	f.complete = append(f.complete, answer+"#"+id)
}

func TestModsSeam(t *testing.T) {
	f := &fakeMods{extra: []string{"from a mod"}}
	h := New(Options{Session: SessionInfo{Cwd: "/work"}, Mods: f})
	h.SessionStart(context.Background(), "startup")
	if len(f.started) != 1 || f.started[0] != "/work" {
		t.Fatalf("session.start = %v", f.started)
	}
	r := h.UserPromptSubmit(context.Background(), "hi")
	if r.Blocked() || !strings.Contains(Context(r), "from a mod") {
		t.Fatalf("context: %+v", r)
	}
	f.drop = "not today"
	r = h.UserPromptSubmit(context.Background(), "hi")
	if !r.Blocked() || r.Reason != "not today" {
		t.Fatalf("drop: %+v", r)
	}
}

// modSession loads one hooks module (its source as hooks/mod.ts) into a
// real engine.
func modSession(t *testing.T, src string) *mods.Session {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{"hooks/hooks.json": `{"modules": ["./mod.ts"]}`, "hooks/mod.ts": src} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, errs := mods.LoadSession(mods.SessionConfig{Plugins: []mods.PluginRef{{Name: "rewriter", ID: "rewriter@m", Root: dir}}, Host: &mods.Host{}, Logf: t.Logf})
	if len(errs) > 0 || s == nil {
		t.Fatalf("load: %v", errs)
	}
	t.Cleanup(s.Close)
	return s
}

func TestPromptRewriteIsScreenedByTheClassicHooksAndBecomesThePrompt(t *testing.T) {
	s := modSession(t, `
export function register(on) {
  on('prompt.submit', ($, e, next) => next({ ...e, text: e.text + ' (rewritten)', context: ['note from a mod'] }))
}`)
	fr := &fakeRunner{script: func(r RunRequest) scripted {
		if strings.Contains(r.Payload.Prompt, "secret") {
			return scripted{exit: 2, stderr: "no secrets"}
		}
		return scripted{stdout: ctxOut("UserPromptSubmit", "saw: "+r.Payload.Prompt)}
	}}
	h := New(Options{
		Settings: []SettingsLayer{layer(settingspolicy.SourceUser, hooksMap("UserPromptSubmit", "", "check"))},
		Runner:   fr,
		Mods:     s,
	})
	r, final := h.UserPromptSubmitText(context.Background(), "hello")
	if r.Blocked() || final != "hello (rewritten)" {
		t.Fatalf("result %+v, final %q", r, final)
	}
	ctx := Context(r)
	if !strings.Contains(ctx, "saw: hello (rewritten)") || !strings.Contains(ctx, "note from a mod") {
		t.Errorf("context = %q (the classic hook must see the rewrite)", ctx)
	}
	r, _ = h.UserPromptSubmitText(context.Background(), "my secret")
	if !r.Blocked() || r.Reason != "no secrets" {
		t.Errorf("the classic block beneath the rewrite = %+v", r)
	}
}

func TestCommandRunSeam(t *testing.T) {
	s := modSession(t, `
export function register(on) {
  on('command.run', { command: 'deploy' }, ($, e, next) => e.args === 'prod' ? { text: 'not from here' } : next({ ...e, args: e.args + ' --dry-run' }))
}`)
	h := New(Options{Mods: s})
	var ran []string
	run := func(_ context.Context, args string) (*string, error) {
		ran = append(ran, args)
		t := "ran " + args
		return &t, nil
	}
	out, err := h.CommandRun(context.Background(), "deploy", "prod", nil, run)
	if err != nil || out.Ran || !out.Answered || out.Text != "rewriter: not from here" {
		t.Fatalf("answered = %+v %v", out, err)
	}
	out, err = h.CommandRun(context.Background(), "deploy", "staging", nil, run)
	if err != nil || !out.Ran || out.Answered || out.RanArgs != "staging --dry-run" || out.Text != "ran staging --dry-run" {
		t.Fatalf("rewritten = %+v %v", out, err)
	}
	out, _ = h.CommandRun(context.Background(), "other", "x", nil, run)
	if !out.Ran || out.RanArgs != "x" {
		t.Fatalf("unhooked = %+v", out)
	}
	if strings.Join(ran, ",") != "staging --dry-run,x" {
		t.Errorf("runs = %v", ran)
	}
}
