package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// startShellSession runs an interactive session over a recording fake caller
// in an isolated HOME and working directory. respond=false writes
// respondToBashCommands:false to the user settings.
func startShellSession(t *testing.T, respond bool) (*fakeProgram, *bgRecordingCaller, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-mode tests run POSIX commands")
	}
	home := t.TempDir()
	// `pwd` prints the physical directory; resolve the temp dir's symlinks
	// (macOS /var -> /private/var) so it compares equal.
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env, _, _ := newTestEnv()
	env.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env.Cwd = func() (string, error) { return cwd, nil }
	if !respond {
		p, ok := settingsContext(env).PathForSource(settingspolicy.SourceUser)
		if !ok {
			t.Fatal("no user settings path")
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"respondToBashCommands": false}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	caller := &bgRecordingCaller{}
	progCh := make(chan *fakeProgram, 1)
	sess := &interactiveSession{
		print: &printSession{
			Mode:      toolreg.ModeDefault,
			newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
		},
		newProgram: func(m *ui.Model, _ *Env) teaProgram {
			p := newFakeProgram(m)
			progCh <- p
			return p
		},
		newConversation: defaultNewConversation,
	}
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	t.Cleanup(prog.Quit)
	return prog, caller, cwd
}

// typeText presses each rune of s.
func typeText(p *fakeProgram, s string) {
	for _, r := range s {
		if r == ' ' {
			p.press(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
			continue
		}
		p.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func enter(p *fakeProgram) { p.press(tea.KeyPressMsg{Code: tea.KeyEnter}) }

// shellResult waits for the shell block's final update and then for the
// driver to go idle, so the next submission is accepted.
func shellResult(t *testing.T, p *fakeProgram, d time.Duration) ui.ToolCall {
	t.Helper()
	var got ui.ToolCall
	at := -1
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for i, m := range p.sent {
			if u, ok := m.(ui.UpdateToolCallMsg); ok && u.Tool.Shell && at < 0 {
				got, at = u.Tool, i
			}
			if s, ok := m.(ui.SetStatusMsg); ok && at >= 0 && i > at && s.Status.Kind == ui.StatusIdle {
				p.mu.Unlock()
				return got
			}
		}
		p.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("shell block was never completed")
	return got
}

// TestShellModeRunsAndModelResponds: "!" + command runs it in the session
// directory (not through the model), shows "! <command>" with its output, and
// (respondToBashCommands default) the model's next request carries the
// <bash-input> and <bash-stdout>/<bash-stderr> blocks.
func TestShellModeRunsAndModelResponds(t *testing.T) {
	prog, caller, cwd := startShellSession(t, true)
	typeText(prog, "!echo hello; echo '<b>&' 1>&2; pwd")
	enter(prog)

	tc := shellResult(t, prog, 5*time.Second)
	if tc.Header() != "! echo hello; echo '<b>&' 1>&2; pwd" {
		t.Fatalf("header = %q", tc.Header())
	}
	if !strings.Contains(tc.ResultText, "hello") || !strings.Contains(tc.ResultText, cwd) || tc.IsError {
		t.Fatalf("result = %q (error %v)", tc.ResultText, tc.IsError)
	}
	if prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.AppendUserMsg); return ok }, 50*time.Millisecond) {
		t.Fatal("shell command was echoed as a user message")
	}
	if !waitCount(caller, 1, 3*time.Second) {
		t.Fatal("model was not asked to respond")
	}
	req := caller.request(0)
	want := "<bash-input>echo hello; echo '<b>&' 1>&2; pwd</bash-input>\n<bash-stdout>hello\n" + cwd + "</bash-stdout><bash-stderr>&lt;b&gt;&amp;</bash-stderr>"
	if !strings.Contains(req, want) {
		t.Fatalf("request missing shell blocks:\n%s\nwant:\n%s", req, want)
	}
	if strings.Contains(req, "local-command-caveat") {
		t.Fatal("a responded command carries the no-response caveat")
	}
}

// TestShellModeContextOnly: with respondToBashCommands:false the command makes
// no model call; the exchange (behind the caveat) rides the next request. A
// failing command's exit code goes to stderr.
func TestShellModeContextOnly(t *testing.T) {
	prog, caller, _ := startShellSession(t, false)
	typeText(prog, "!echo out; exit 3")
	enter(prog)
	tc := shellResult(t, prog, 5*time.Second)
	if !tc.IsError || !strings.Contains(tc.ResultText, "Exit code 3") {
		t.Fatalf("result = %q (error %v)", tc.ResultText, tc.IsError)
	}
	time.Sleep(100 * time.Millisecond)
	if caller.count() != 0 {
		t.Fatalf("model called %d times for a context-only command", caller.count())
	}
	typeText(prog, "next")
	enter(prog)
	if !waitCount(caller, 1, 3*time.Second) {
		t.Fatal("next message did not reach the model")
	}
	req := caller.request(0)
	for _, want := range []string{
		"<local-command-caveat>The command below was run directly in gocode",
		"<bash-input>echo out; exit 3</bash-input>",
		"<bash-stdout>out</bash-stdout><bash-stderr>Exit code 3</bash-stderr>",
		"next",
	} {
		if !strings.Contains(req, want) {
			t.Fatalf("request missing %q:\n%s", want, req)
		}
	}
	if strings.Index(req, "<bash-input>") > strings.Index(req, "next") {
		t.Fatal("shell exchange should precede the next prompt")
	}
}

// TestShellModeOutputCapped: output beyond the Bash tool's inline limit
// reaches the model framed and cut, not in full.
func TestShellModeOutputCapped(t *testing.T) {
	prog, caller, _ := startShellSession(t, false)
	typeText(prog, "!yes aaaaaaaaa | head -c 40000")
	enter(prog)
	shellResult(t, prog, 5*time.Second)
	typeText(prog, "go")
	enter(prog)
	if !waitCount(caller, 1, 3*time.Second) {
		t.Fatal("next message did not reach the model")
	}
	req := caller.request(0)
	if !strings.Contains(req, "<persisted-output>") && !strings.Contains(req, "<truncated-output>") {
		t.Fatalf("oversized output not framed (len %d)", len(req))
	}
	if strings.Count(req, "aaaaaaaaa\\n") > 3500 {
		t.Fatal("oversized output sent in full")
	}
}

// TestShellModeInterrupt: Esc stops a running command; the block shows the
// interruption, no model call is made, and the exchange is recorded with the
// interruption marker.
func TestShellModeInterrupt(t *testing.T) {
	prog, caller, _ := startShellSession(t, true)
	typeText(prog, "!sleep 30")
	enter(prog)
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && s.Status.Label == "Running"
	}, 3*time.Second) {
		t.Fatal("command never started")
	}
	time.Sleep(50 * time.Millisecond)
	prog.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	start := time.Now()
	tc := shellResult(t, prog, 5*time.Second)
	if time.Since(start) > 4*time.Second || tc.ResultText != "Interrupted" || !tc.IsError {
		t.Fatalf("interrupt: %q after %v", tc.ResultText, time.Since(start))
	}
	if !sawSystemMsg(prog, "Interrupted") {
		t.Fatal("no interrupted notice")
	}
	time.Sleep(100 * time.Millisecond)
	if caller.count() != 0 {
		t.Fatal("model called for an interrupted command")
	}
	typeText(prog, "after")
	enter(prog)
	if !waitCount(caller, 1, 3*time.Second) {
		t.Fatal("next message did not reach the model")
	}
	req := caller.request(0)
	if !strings.Contains(req, "<bash-input>sleep 30</bash-input>\n[Request interrupted by user]") {
		t.Fatalf("request:\n%s", req)
	}
}
