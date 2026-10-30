package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/mods"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// modsHost is a hook host over one real hooks module (its source as
// hooks/mod.ts), with the classic UserPromptSubmit hooks of cfg.
func modsHost(t *testing.T, src string, cfg map[string]any, run func(cmd string) (int, string, string)) *hookhost.Host {
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
	s, errs := mods.LoadSession(mods.SessionConfig{Plugins: []mods.PluginRef{{Name: "guard", ID: "guard@m", Root: dir}}, Host: &mods.Host{}, Logf: t.Logf})
	if len(errs) > 0 || s == nil {
		t.Fatalf("load: %v", errs)
	}
	t.Cleanup(s.Close)
	return hookhost.New(hookhost.Options{
		Settings: []hookhost.SettingsLayer{{Source: settingspolicy.SourceUser, Settings: map[string]any{"hooks": cfg}}},
		Runner: hookhost.CommandRunnerFunc(func(_ context.Context, req hookhost.RunRequest) (hooks.Decision, hooks.RunResult, error) {
			code, so, se := run(req.Hook.Command)
			return hooks.Classify(code, so, se, req.Options), hooks.RunResult{ExitCode: code, Stdout: so, Stderr: se}, nil
		}),
		Mods: s,
	})
}

const commandGuard = `
export function register(on) {
  on('command.run', { command: 'foo' }, ($, e, next) =>
    e.args === 'stop' ? { text: 'foo is off today' } : next({ ...e, args: e.args.toUpperCase() }))
  on('command.run', { command: 'compact' }, ($, e, next) => ({ text: 'no compacting' }))
}`

func TestCommandRunHooksCustomAndBuiltinCommands(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})
	cmds := commands.NewRegistry()
	cmds.Add(&commands.Command{Type: "prompt", Name: "foo", Body: "Do: $ARGUMENTS", Source: commands.SourceProject})
	c.Commands = cmds
	c.HookHost = modsHost(t, commandGuard, nil, func(string) (int, string, string) { return 0, "", "" })

	// A rewrite of the args: the command expands with what the hook passed.
	if _, err := c.Submit(context.Background(), "/foo bar"); err != nil {
		t.Fatal(err)
	}
	if got := messageText(c.History[0]); got != "Do: BAR" {
		t.Fatalf("user turn = %q", got)
	}
	// An answer in the command's place: no model turn, the plugin's text.
	calls := model.callCount
	res, err := c.Submit(context.Background(), "/foo stop")
	if err != nil || res.Kind != ResultLocal || res.Effect == nil || res.Effect.Text != "guard: foo is off today" || model.callCount != calls {
		t.Fatalf("answered: %+v %v", res, err)
	}
	// A builtin is hooked too: /compact does not run.
	res, err = c.Submit(context.Background(), "/compact")
	if err != nil || res.Compacted || res.Effect == nil || res.Effect.Text != "guard: no compacting" {
		t.Fatalf("builtin: %+v %v", res, err)
	}
}

func TestPromptSubmitRewriteBecomesTheUserTurn(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})
	var seen []string
	c.HookHost = modsHost(t, `
export function register(on) {
  on('prompt.submit', ($, e, next) => next({ ...e, text: e.text.replace('passw0rd', '[redacted]') }))
}`, map[string]any{"UserPromptSubmit": group("u")}, func(cmd string) (int, string, string) {
		seen = append(seen, cmd)
		return 0, "", ""
	})
	if _, err := c.Submit(context.Background(), "my passw0rd is long"); err != nil {
		t.Fatal(err)
	}
	if got := messageText(c.History[0]); !strings.Contains(got, "my [redacted] is long") || strings.Contains(got, "passw0rd") {
		t.Fatalf("user turn = %q", got)
	}
	if len(seen) != 1 {
		t.Errorf("the classic UserPromptSubmit hooks ran %d times", len(seen))
	}
}
