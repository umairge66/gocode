//go:build linux || darwin

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

func TestShutdownCodes(t *testing.T) {
	owns := func() bool { return true }
	noTTY := func() bool { return false }
	for _, c := range []struct {
		sig  os.Signal
		bg   bool
		ctty func() bool
		code int
		ok   bool
	}{
		{syscall.SIGINT, false, noTTY, 0, true},
		{syscall.SIGTERM, false, noTTY, 143, true},
		{syscall.SIGHUP, false, noTTY, 129, true},
		{syscall.SIGHUP, true, noTTY, 0, false}, // a detached worker ignores a stray hang-up
		{syscall.SIGHUP, true, owns, 129, true}, // …but not its own terminal's
	} {
		code, ok := shutdownCode(c.sig, c.bg, c.ctty)
		if code != c.code || ok != c.ok {
			t.Errorf("shutdownCode(%v, bg=%v) = %d %v", c.sig, c.bg, code, ok)
		}
	}
}

// sigBlockingCaller blocks every model call until its context is cancelled.
type sigBlockingCaller struct{ started chan struct{} }

func (b sigBlockingCaller) CallModel(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return agent.ModelResponse{}, ctx.Err()
}

func TestPrintSessionSIGTERM(t *testing.T) {
	env, _, _ := newTestEnv()
	started := make(chan struct{}, 1)
	s := &printSession{Mode: toolreg.ModeDefault, Provider: providerOpenRouter, Model: "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return sigBlockingCaller{started}, nil }}
	done := make(chan int, 1)
	go func() { done <- runPrintSession(env, s, "hello") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never started")
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 143 {
			t.Fatalf("exit code = %d, want 143", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("-p did not stop on SIGTERM")
	}
}

// The interactive session stops its turn on SIGTERM, ends the session and
// exits 143; as a background worker its state showed the running turn.
func TestInteractiveSessionSIGTERM(t *testing.T) {
	env, _, _ := newTestEnv()
	dir := filepath.Join(t.TempDir(), "abcd5678")
	_ = os.MkdirAll(dir, 0o700)
	// An idle session (dispatched without a prompt) waiting for a message.
	idle := daemon.DetailIdleNoMsg
	st := daemon.NewJobState(daemon.SeedParams{Template: "bg", SessionID: "abcd5678-s", Cwd: dir, Intent: "seed",
		Detail: &idle, Tempo: "blocked", Needs: daemon.NeedsPrompt})
	if err := daemon.WriteJobState(dir, st); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"GOCODE_CODE_SESSION_KIND": "bg", "GOCODE_JOB_DIR": dir, "GOCODE_BG_BACKEND": "daemon"}
	env.Getenv = func(k string) string { return vars[k] }
	started := make(chan struct{}, 1)
	progCh := make(chan *fakeProgram, 1)
	sess := &interactiveSession{
		print: &printSession{Mode: toolreg.ModeDefault, newCaller: func(*printSession) (agent.ModelCaller, error) {
			return sigBlockingCaller{started}, nil
		}},
		newProgram: func(m *ui.Model, _ *Env) teaProgram {
			p := newFakeProgram(m)
			progCh <- p
			return p
		},
		newConversation: defaultNewConversation,
	}
	done := make(chan int, 1)
	go func() { done <- runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	for _, r := range "work on it" {
		prog.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never started")
	}
	if got, _ := daemon.ReadJobState(dir); got == nil || got.Tempo != "active" || !strings.Contains(got.Detail, "work on it") {
		t.Fatalf("state during the turn: %+v", got)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 143 {
			t.Fatalf("exit code = %d, want 143", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not stop on SIGTERM")
	}
	if got, _ := daemon.ReadJobState(dir); got == nil || got.Tempo == "active" {
		t.Fatalf("state after stop: %+v", got)
	}
}
