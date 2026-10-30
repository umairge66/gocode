package builtincmds

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/toolreg"
)

// TestTableResolvesKnownBuiltinsToEffectKind checks that each known built-in
// resolves to the expected Effect kind.
func TestTableResolvesKnownBuiltinsToEffectKind(t *testing.T) {
	tbl := NewTable()
	cases := []struct {
		line string
		want EffectKind
	}{
		{"/clear", EffectClearTranscript},
		{"/reset", EffectClearTranscript}, // alias
		{"/new", EffectClearTranscript},   // alias
		{"/compact", EffectTriggerCompaction},
		{"/model sonnet", EffectSetModel},
		{"/model", EffectOpenDialog}, // empty arg -> picker
		{"/plan", EffectSetPermissionMode},
		{"/help", EffectOpenDialog},
		{"/status", EffectOpenDialog},
		{"/config", EffectOpenDialog},
		{"/settings", EffectOpenDialog}, // alias of config
		{"/mcp", EffectOpenDialog},
		{"/login", EffectOpenDialog},
		{"/login openrouter", EffectOpenDialog},
		{"/provider", EffectOpenDialog},
		{"/provider anthropic", EffectOpenDialog},
		{"/doctor", EffectOpenDialog},
		{"/resume", EffectOpenDialog},
		{"/exit", EffectExit},
		{"/quit", EffectExit}, // alias
		{"/usage", EffectPrintText},
		{"/cost", EffectPrintText}, // alias
		{"/version", EffectPrintText},
		{"/agents", EffectPrintText},
		{"/init", EffectSubmitPrompt},
		{"/ultrareview", EffectSubmitPrompt},
	}
	for _, c := range cases {
		eff, ok := tbl.Dispatch(c.line)
		if !ok {
			t.Errorf("%q: expected a builtin, got not-a-builtin", c.line)
			continue
		}
		if eff.Kind != c.want {
			t.Errorf("%q: got effect %s, want %s", c.line, eff.Kind, c.want)
		}
	}
}

func TestModelEffectCarriesModelName(t *testing.T) {
	tbl := NewTable()
	eff, ok := tbl.Dispatch("/model sonnet")
	if !ok || eff.Kind != EffectSetModel {
		t.Fatalf("/model sonnet: got ok=%v kind=%s", ok, eff.Kind)
	}
	if eff.Model != "sonnet" {
		t.Errorf("/model sonnet: Model = %q, want %q", eff.Model, "sonnet")
	}
}

func TestLoginAndProviderDialogsCarryName(t *testing.T) {
	tbl := NewTable()
	for _, c := range []struct{ line, dialog, args string }{
		{"/login", "login", ""},
		{"/login anthropic", "login", "anthropic"},
		{"/provider", "provider", ""},
		{"/provider openai", "provider", "openai"},
	} {
		eff, ok := tbl.Dispatch(c.line)
		if !ok || eff.Kind != EffectOpenDialog {
			t.Fatalf("%q: ok=%v kind=%s, want OpenDialog", c.line, ok, eff.Kind)
		}
		if eff.Dialog != c.dialog {
			t.Errorf("%q: Dialog = %q, want %q", c.line, eff.Dialog, c.dialog)
		}
		if strings.TrimSpace(eff.Args) != c.args {
			t.Errorf("%q: Args = %q, want %q", c.line, eff.Args, c.args)
		}
	}
}

func TestPlanEffectSetsPlanMode(t *testing.T) {
	tbl := NewTable()
	eff, _ := tbl.Dispatch("/plan")
	if eff.Kind != EffectSetPermissionMode || eff.Mode != toolreg.ModePlan {
		t.Errorf("/plan: got kind=%s mode=%q, want SetPermissionMode plan", eff.Kind, eff.Mode)
	}
}

func TestCompactCarriesCustomInstructions(t *testing.T) {
	tbl := NewTable()
	eff, _ := tbl.Dispatch("/compact focus on the API changes")
	if eff.Kind != EffectTriggerCompaction {
		t.Fatalf("got kind %s", eff.Kind)
	}
	if eff.Text != "focus on the API changes" {
		t.Errorf("custom instructions = %q", eff.Text)
	}
}

func TestUnknownCommandIsNotBuiltin(t *testing.T) {
	tbl := NewTable()
	// /logout (a gocode OAuth flow) does not exist; it falls through like any
	// non-builtin. /login IS a builtin (provider-key entry), tested separately.
	for _, line := range []string{"/logout"} {
		if _, ok := tbl.Dispatch(line); ok {
			t.Errorf("%s should not be a builtin", line)
		}
	}
	if _, ok := tbl.Dispatch("/foo bar"); ok {
		t.Error("/foo should not resolve to a builtin")
	}
	// A plain prompt is not a slash command at all.
	if _, ok := tbl.Dispatch("hello world"); ok {
		t.Error("plain text should not resolve to a builtin")
	}
	if _, ok := tbl.Lookup("definitely-not-a-command"); ok {
		t.Error("Lookup of unknown name should be false")
	}
}

func TestLookupIsCaseInsensitive(t *testing.T) {
	tbl := NewTable()
	if _, ok := tbl.Lookup("CLEAR"); !ok {
		t.Error("expected case-insensitive lookup of CLEAR")
	}
}

// TestRunShellCommandEcho runs a harmless echo and checks its output is captured.
func TestRunShellCommandEcho(t *testing.T) {
	res := RunShellCommand(context.Background(), "echo hello-shell", RunOptions{})
	if res.TimedOut {
		t.Fatal("echo timed out unexpectedly")
	}
	if res.ExitCode != 0 {
		t.Fatalf("echo exit code = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Output); got != "hello-shell" {
		t.Errorf("output = %q, want %q", got, "hello-shell")
	}
}

// TestRunShellCommandStderrCombined checks stderr is labelled and combined.
func TestRunShellCommandStderrCombined(t *testing.T) {
	res := RunShellCommand(context.Background(), "echo out; echo err 1>&2", RunOptions{})
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}
	if !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "[stderr]") || !strings.Contains(res.Output, "err") {
		t.Errorf("combined output missing parts: %q", res.Output)
	}
}

func TestResolveTimeoutHonoursEnvAndCap(t *testing.T) {
	// Default when nothing set.
	if got := resolveTimeout(0, map[string]string{}); got != time.Duration(defaultBashTimeoutMS)*time.Millisecond {
		t.Errorf("default timeout = %v", got)
	}
	// Explicit opt within cap is kept.
	if got := resolveTimeout(5*time.Second, map[string]string{}); got != 5*time.Second {
		t.Errorf("explicit timeout = %v", got)
	}
	// Opt above the ceiling is capped.
	if got := resolveTimeout(time.Hour, map[string]string{}); got != time.Duration(maxBashTimeoutMS)*time.Millisecond {
		t.Errorf("capped timeout = %v, want %v", got, time.Duration(maxBashTimeoutMS)*time.Millisecond)
	}
	// Env override raises the default.
	env := map[string]string{"BASH_DEFAULT_TIMEOUT_MS": "200000", "BASH_MAX_TIMEOUT_MS": "300000"}
	if got := resolveTimeout(0, env); got != 200000*time.Millisecond {
		t.Errorf("env default timeout = %v", got)
	}
}
