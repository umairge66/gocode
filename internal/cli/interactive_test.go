package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/statusline"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// fakeProgram is a teaProgram that faithfully mirrors tea.Program's threading:
// every message (host Send and test keystroke alike) is enqueued on a channel
// and applied to the ui.Model on the SINGLE Run goroutine, so there is exactly
// one goroutine ever touching the model — just like the real program, whose
// Update loop runs only inside Run(). This is what lets the wiring in
// runInteractiveSession (which sets the model callbacks before calling Run)
// race-freely precede any message processing.
type fakeProgram struct {
	model *ui.Model
	msgs  chan tea.Msg

	mu   sync.Mutex // guards sent
	sent []tea.Msg
	done chan struct{}
}

func newFakeProgram(m *ui.Model) *fakeProgram {
	return &fakeProgram{model: m, msgs: make(chan tea.Msg, 256), done: make(chan struct{})}
}

// mustProgram waits for the session to build its program and returns it.
func mustProgram(t *testing.T, ch <-chan *fakeProgram) *fakeProgram {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("program was never built")
		return nil
	}
}

// Run is the single goroutine that owns the model: it drains the message queue
// and applies each message, until Quit closes done.
func (p *fakeProgram) Run() (tea.Model, error) {
	for {
		select {
		case msg := <-p.msgs:
			p.mu.Lock()
			p.sent = append(p.sent, msg)
			p.mu.Unlock()
			p.model.Update(msg)
		case <-p.done:
			return p.model, nil
		}
	}
}

// Send enqueues a host-pushed message (safe from any goroutine).
func (p *fakeProgram) Send(msg tea.Msg) {
	select {
	case p.msgs <- msg:
	case <-p.done:
	}
}

// press enqueues a simulated keystroke on the same path as Send.
func (p *fakeProgram) press(msg tea.Msg) { p.Send(msg) }

func (p *fakeProgram) Quit() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// sentOfType reports whether any recorded message is of the given concrete type
// (matched by a predicate) and returns the first match.
func (p *fakeProgram) waitFor(pred func(tea.Msg) bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, m := range p.sent {
			if pred(m) {
				p.mu.Unlock()
				return true
			}
		}
		p.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// TestInteractiveSession_NoCredentials asserts a credential-less start still
// LAUNCHES the program (the user finishes setup with /login), shows what is
// missing, and answers a message sent before setup with an actionable error
// instead of crashing or making an unauthenticated request.
func TestInteractiveSession_NoCredentials(t *testing.T) {
	env, _, _ := newTestEnv()
	progCh := make(chan *fakeProgram, 1)
	sess := &interactiveSession{
		print: &printSession{Mode: toolreg.ModeDefault, newCaller: defaultNewCaller},
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

	if !sawSystemMsg(prog, "run /login") {
		t.Fatal("startup did not say how to add a key")
	}
	for _, r := range "hello" {
		prog.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		e, ok := m.(ui.AppendErrorMsg)
		return ok && strings.Contains(e.Text, "/login")
	}, 2*time.Second) {
		t.Fatal("a message sent before setup did not get an actionable /login error")
	}
	prog.Quit()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit after quit")
	}
}

// TestInteractiveSession_SubmitRoundTrip drives the full interactive wiring with
// a fake ModelCaller and a fake program: a submitted input runs through
// conversation.Submit and the assistant text is pushed back to the UI.
func TestInteractiveSession_SubmitRoundTrip(t *testing.T) {
	env, _, _ := newTestEnv()

	progCh := make(chan *fakeProgram, 1)
	sess := &interactiveSession{
		print: &printSession{
			Mode: toolreg.ModeDefault,
			newCaller: func(*printSession) (agent.ModelCaller, error) {
				return fakeCaller{reply: "interactive answer"}, nil
			},
		},
		newProgram: func(m *ui.Model, _ *Env) teaProgram {
			p := newFakeProgram(m)
			progCh <- p
			return p
		},
		newConversation: defaultNewConversation,
	}

	// Run the session on a goroutine; it blocks in prog.Run() until Quit.
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)

	// Simulate the user typing "ping" and pressing Enter.
	for _, r := range "ping" {
		prog.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	// The driver should push the assistant result back. A non-streaming caller
	// leaves OnDelta uncalled, so the final text arrives as a one-shot
	// StreamAssistantMsg finalize (Done:true) that renders as Markdown.
	ok := prog.waitFor(func(m tea.Msg) bool {
		a, isA := m.(ui.StreamAssistantMsg)
		return isA && a.Text == "interactive answer" && a.Done
	}, 2*time.Second)
	if !ok {
		t.Fatal("assistant result was not pushed back to the UI")
	}
	prog.Quit()
}

// TestInteractiveSession_InitialPrompt confirms a bare positional prompt is
// submitted as the first turn (echoed as a user message and answered).
func TestInteractiveSession_InitialPrompt(t *testing.T) {
	env, _, _ := newTestEnv()

	progCh := make(chan *fakeProgram, 1)
	sess := &interactiveSession{
		print: &printSession{
			Mode: toolreg.ModeDefault,
			newCaller: func(*printSession) (agent.ModelCaller, error) {
				return fakeCaller{reply: "first answer"}, nil
			},
		},
		newProgram: func(m *ui.Model, _ *Env) teaProgram {
			p := newFakeProgram(m)
			progCh <- p
			return p
		},
		newConversation: defaultNewConversation,
	}

	go func() { runInteractiveSession(env, sess, "do the thing") }()
	prog := mustProgram(t, progCh)

	echoed := prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.AppendUserMsg)
		return ok && u.Text == "do the thing"
	}, 2*time.Second)
	if !echoed {
		t.Fatal("initial prompt was not echoed as a user message")
	}
	answered := prog.waitFor(func(m tea.Msg) bool {
		a, ok := m.(ui.StreamAssistantMsg)
		return ok && a.Text == "first answer" && a.Done
	}, 2*time.Second)
	if !answered {
		t.Fatal("initial prompt was not answered")
	}
	prog.Quit()
}

// streamingCliCaller is a fake agent.StreamingModelCaller that emits scripted
// text deltas to the loop's StreamHandler before returning the assembled final
// message. It lets the interactive test assert deltas were pushed to the program
// live, during the turn.
type streamingCliCaller struct{ deltas []string }

func (c streamingCliCaller) full() string {
	var s string
	for _, d := range c.deltas {
		s += d
	}
	return s
}

func (c streamingCliCaller) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(c.full())),
		StopReason: agent.StopEndTurn,
	}, nil
}

func (c streamingCliCaller) CallModelStream(_ context.Context, _ agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	if h != nil && h.OnText != nil {
		for _, d := range c.deltas {
			h.OnText(d)
		}
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(c.full())),
		StopReason: agent.StopEndTurn,
	}, nil
}

// TestInteractiveSession_StreamsDeltas drives the interactive wiring with a
// streaming fake caller and asserts that incremental text was pushed to the
// program DURING the turn (as growing StreamAssistantMsg deltas, Done:false)
// and that the turn is finalized with a Done:true StreamAssistantMsg carrying
// the full assistant text.
func TestInteractiveSession_StreamsDeltas(t *testing.T) {
	env, _, _ := newTestEnv()

	caller := streamingCliCaller{deltas: []string{"streamed ", "live ", "answer"}}
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

	for _, r := range "go" {
		prog.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	// A streaming delta carrying the FIRST increment must have been sent during
	// the turn (proving deltas are forwarded incrementally, not only at the end),
	// as an in-progress StreamAssistantMsg (Done:false).
	firstDelta := prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.StreamAssistantMsg)
		return ok && s.Text == "streamed " && !s.Done
	}, 2*time.Second)
	if !firstDelta {
		t.Fatal("first streamed delta was not pushed to the program as a StreamAssistantMsg")
	}

	// A streaming update carrying the FULL accumulated text (still in progress)
	// must also appear.
	fullStream := prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.StreamAssistantMsg)
		return ok && s.Text == "streamed live answer" && !s.Done
	}, 2*time.Second)
	if !fullStream {
		t.Fatal("accumulated streamed text was not pushed to the program")
	}

	// The turn is finalized with a Done:true StreamAssistantMsg carrying the
	// final text (which flips the renderer from raw text to Markdown).
	finalized := prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.StreamAssistantMsg)
		return ok && s.Text == "streamed live answer" && s.Done
	}, 2*time.Second)
	if !finalized {
		t.Fatal("turn was not finalized with the full assistant text")
	}
	prog.Quit()
}

// blockingCaller blocks until its context is cancelled, then reports the
// aborted reason — used to exercise interrupt handling.
type blockingCaller struct{}

func (blockingCaller) CallModel(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	<-ctx.Done()
	return agent.ModelResponse{}, ctx.Err()
}

// TestInteractiveSession_Interrupt confirms esc cancels the in-flight turn.
func TestInteractiveSession_Interrupt(t *testing.T) {
	env, _, _ := newTestEnv()

	progCh := make(chan *fakeProgram, 1)
	sess := &interactiveSession{
		print: &printSession{
			Mode:      toolreg.ModeDefault,
			newCaller: func(*printSession) (agent.ModelCaller, error) { return blockingCaller{}, nil },
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

	for _, r := range "hang" {
		prog.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Wait until the turn is marked thinking/interruptible, then press esc.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && s.Status.Kind == ui.StatusThinking
	}, 2*time.Second) {
		t.Fatal("turn never entered the thinking state")
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEsc})

	interrupted := prog.waitFor(func(m tea.Msg) bool {
		sys, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(sys.Text, "Interrupted")
	}, 2*time.Second)
	if !interrupted {
		t.Fatal("esc did not interrupt the in-flight turn")
	}
	prog.Quit()
}

// newThemeSession builds a fake-backed interactive session with the given theme
// setting value and (optional) status-line config/runner.
func newThemeSession(theme string, sl *statusline.Config, runner statusline.Runner, progCh chan *fakeProgram) *interactiveSession {
	return &interactiveSession{
		print: &printSession{
			Mode:      toolreg.ModeDefault,
			Model:     "gocode-opus",
			newCaller: func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
		},
		newProgram: func(m *ui.Model, _ *Env) teaProgram {
			p := newFakeProgram(m)
			progCh <- p
			return p
		},
		newConversation: defaultNewConversation,
		theme:           theme,
		statusLine:      sl,
		statusRunner:    runner,
		sessionID:       "test-session",
	}
}

// TestInteractiveSession_ThemeApplied asserts the resolved theme is pushed to
// the program as a SetThemeMsg: a valid name is kept, and an invalid/unset name
// falls back to dark.
func TestInteractiveSession_ThemeApplied(t *testing.T) {
	cases := []struct {
		name    string
		setting string
		want    string
	}{
		{"valid", "light", "light"},
		{"invalid falls back to dark", "totally-bogus", "dark"},
		{"unset falls back to dark", "", "dark"},
		{"auto falls back to dark", "auto", "dark"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _, _ := newTestEnv()
			progCh := make(chan *fakeProgram, 1)
			sess := newThemeSession(tc.setting, nil, nil, progCh)

			go func() { runInteractiveSession(env, sess, "") }()
			prog := mustProgram(t, progCh)

			ok := prog.waitFor(func(m tea.Msg) bool {
				st, isTheme := m.(ui.SetThemeMsg)
				return isTheme && st.Name == tc.want
			}, 2*time.Second)
			if !ok {
				t.Fatalf("expected SetThemeMsg{Name:%q} to be pushed", tc.want)
			}
			prog.Quit()
		})
	}
}

// fakeStatusRunner is a statusline.Runner that returns canned output without
// running a shell, recording the stdin it was handed.
type fakeStatusRunner struct {
	stdout string

	mu       sync.Mutex
	gotStdin string
	gotCmd   string
}

func (f *fakeStatusRunner) Run(_ context.Context, command, stdin string) (statusline.RunOutput, error) {
	f.mu.Lock()
	f.gotStdin = stdin
	f.gotCmd = command
	f.mu.Unlock()
	return statusline.RunOutput{Stdout: f.stdout}, nil
}

// TestInteractiveSession_StatusLinePushed asserts a configured status line is
// rendered through the injected fake runner and pushed to the program as a
// SetStatusLineMsg carrying the rendered content.
func TestInteractiveSession_StatusLinePushed(t *testing.T) {
	env, _, _ := newTestEnv()
	runner := &fakeStatusRunner{stdout: "my-branch | gocode-opus\n"}
	cfg := &statusline.Config{Type: "command", Command: "my-statusline.sh"}

	progCh := make(chan *fakeProgram, 1)
	sess := newThemeSession("dark", cfg, runner, progCh)

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)

	ok := prog.waitFor(func(m tea.Msg) bool {
		sl, isSL := m.(ui.SetStatusLineMsg)
		return isSL && sl.Line == "my-branch | gocode-opus"
	}, 2*time.Second)
	if !ok {
		t.Fatal("rendered status line was not pushed to the program")
	}

	// The context payload must have reached the runner on stdin (proving the
	// Context was built and serialized through the renderer seam).
	runner.mu.Lock()
	stdin, cmd := runner.gotStdin, runner.gotCmd
	runner.mu.Unlock()
	if cmd != "my-statusline.sh" {
		t.Fatalf("runner command = %q, want %q", cmd, "my-statusline.sh")
	}
	if !strings.Contains(stdin, `"session_id":"test-session"`) {
		t.Fatalf("status-line stdin did not carry the session id: %s", stdin)
	}
	prog.Quit()
}

// TestInteractiveSession_StatusLineAbsent asserts that with no status line
// configured, nothing is ever pushed as a SetStatusLineMsg.
func TestInteractiveSession_StatusLineAbsent(t *testing.T) {
	env, _, _ := newTestEnv()
	progCh := make(chan *fakeProgram, 1)
	sess := newThemeSession("dark", nil, nil, progCh)

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)

	// The theme is pushed at start; wait for it so the startup goroutine has run,
	// then confirm no status line was pushed.
	if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetThemeMsg); return ok }, 2*time.Second) {
		t.Fatal("startup did not run")
	}
	pushed := prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetStatusLineMsg); return ok }, 200*time.Millisecond)
	if pushed {
		t.Fatal("a status line was pushed although none was configured")
	}
	prog.Quit()
}
