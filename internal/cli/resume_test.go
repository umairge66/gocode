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
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// --- flag parsing ----------------------------------------------------------

func TestParseResumeFlags(t *testing.T) {
	t.Run("continue short", func(t *testing.T) {
		d, rest := parseDispatchDefaults([]string{"-c"})
		if d == nil || !d.Continue {
			t.Fatalf("-c did not set Continue: %+v", d)
		}
		if len(rest) != 0 {
			t.Errorf("rest = %v, want empty", rest)
		}
	})
	t.Run("resume with id and trailing prompt", func(t *testing.T) {
		// `-p --resume <id> "prompt"`: --resume consumes the id; the prompt stays
		// as a positional.
		d, rest := parseDispatchDefaults([]string{"--print", "--resume", "sess-123", "do the thing"})
		if d == nil || !d.Resume || d.ResumeValue != "sess-123" {
			t.Fatalf("--resume did not capture the id: %+v", d)
		}
		if firstPositional(rest) != "do the thing" {
			t.Errorf("prompt positional lost: rest=%v", rest)
		}
	})
	t.Run("bare resume opens picker (no value)", func(t *testing.T) {
		d, _ := parseDispatchDefaults([]string{"--resume"})
		if d == nil || !d.Resume || d.ResumeValue != "" {
			t.Fatalf("bare --resume should set Resume with empty value: %+v", d)
		}
	})
	t.Run("session-id / fork / no-persistence", func(t *testing.T) {
		d, _ := parseDispatchDefaults([]string{"--session-id", "abc", "--fork-session", "--no-session-persistence"})
		if d == nil || d.SessionID != "abc" || !d.ForkSession || !d.NoSessionPersistence {
			t.Fatalf("session flags not parsed: %+v", d)
		}
	})
	t.Run("resume=value inline", func(t *testing.T) {
		d, _ := parseDispatchDefaults([]string{"--resume=myterm"})
		if d == nil || !d.Resume || d.ResumeValue != "myterm" {
			t.Fatalf("--resume=value not parsed: %+v", d)
		}
	})
}

// --- helpers ---------------------------------------------------------------

// resumeRecCaller captures the messages the model last saw, so a test can
// assert the resumed history reached the request.
type resumeRecCaller struct {
	mu   sync.Mutex
	seen []agent.Message
}

func (c *resumeRecCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.seen = append([]agent.Message(nil), req.Messages...)
	c.mu.Unlock()
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("resumed-reply")),
		StopReason: agent.StopEndTurn,
	}, nil
}

func (c *resumeRecCaller) lastSeen() []agent.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen
}

// seedSession writes a transcript under the default gocode root for cwd and returns its id.
func seedSession(t *testing.T, home, cwd, id string, msgs []agent.Message, modAgo time.Duration) {
	t.Helper()
	st := sessionstore.New(filepath.Join(home, ".config", "gocode"))
	sess, err := st.Create(cwd, id)
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	if err := sess.AppendMessages(msgs); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	// Age the file so List ordering is deterministic.
	mod := time.Now().Add(-modAgo)
	if err := os.Chtimes(sess.Path(), mod, mod); err != nil {
		t.Fatalf("seed chtimes: %v", err)
	}
}

// --- -p resume -------------------------------------------------------------

// TestPrintResumeLoadsHistory: `-p --resume <id> "prompt"` loads the saved
// session's history and runs the single prompt in its context.
func TestPrintResumeLoadsHistory(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	env, _, errb := newTestEnv()
	env.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env.Cwd = func() (string, error) { return cwd, nil }

	seedSession(t, home, cwd, "prev-session", []agent.Message{
		agent.UserMessage(agent.TextBlock("earlier question")),
		agent.AssistantMessage(agent.TextBlock("earlier answer")),
	}, time.Hour)

	caller := &resumeRecCaller{}
	var rt *sessionRuntime
	s := &printSession{
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
		resume:    resumeIntent{resume: true, value: "prev-session"},
		onSession: func(r *sessionRuntime) { rt = r },
	}

	code := runPrintSession(env, s, "new follow-up")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errb.String())
	}

	// The model request must have carried the loaded history AND the new prompt.
	seen := caller.lastSeen()
	if len(seen) < 3 {
		t.Fatalf("model saw %d messages, want ≥3 (2 loaded + new): %+v", len(seen), seen)
	}
	joined := ""
	for _, m := range seen {
		for _, b := range m.Content {
			joined += b.Text + "\n"
		}
	}
	for _, want := range []string{"earlier question", "earlier answer", "new follow-up"} {
		if !strings.Contains(joined, want) {
			t.Errorf("resumed request missing %q; saw:\n%s", want, joined)
		}
	}

	// The new turns were appended to the SAME session file (resume chain).
	if rt == nil || rt.store == nil {
		t.Fatal("no runtime/store captured")
	}
	reloaded, err := rt.store.Load(cwd, "prev-session")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded) != 4 { // 2 seeded + user + assistant
		t.Fatalf("session file has %d messages after resume+turn, want 4", len(reloaded))
	}
}

// TestPrintContinueResumesLatest: `-p --continue` resumes the most recent
// session for the cwd.
func TestPrintContinueResumesLatest(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	env, _, errb := newTestEnv()
	env.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env.Cwd = func() (string, error) { return cwd, nil }

	seedSession(t, home, cwd, "older", []agent.Message{agent.UserMessage(agent.TextBlock("old"))}, 3*time.Hour)
	seedSession(t, home, cwd, "newer", []agent.Message{agent.UserMessage(agent.TextBlock("newest context"))}, 1*time.Hour)

	caller := &resumeRecCaller{}
	s := &printSession{
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
		resume:    resumeIntent{continueLatest: true},
	}
	if code := runPrintSession(env, s, "continue please"); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errb.String())
	}
	joined := ""
	for _, m := range caller.lastSeen() {
		for _, b := range m.Content {
			joined += b.Text + "\n"
		}
	}
	if !strings.Contains(joined, "newest context") {
		t.Errorf("--continue did not resume the most recent session; saw:\n%s", joined)
	}
	if strings.Contains(joined, "\nold\n") {
		t.Errorf("--continue resumed the older session by mistake:\n%s", joined)
	}
}

// --- interactive /resume picker --------------------------------------------

// TestInteractiveResumePicker drives /resume: the picker lists this project's
// saved sessions (most recent first), and selecting one seeds the live
// conversation's history.
func TestInteractiveResumePicker(t *testing.T) {
	home := t.TempDir()
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	// dialogEnv's Cwd returns home, so sessions live under home/.config/gocode for cwd=home.
	cwd := home

	seedSession(t, home, cwd, "sess-old", []agent.Message{
		agent.UserMessage(agent.TextBlock("the old conversation")),
	}, 3*time.Hour)
	seedSession(t, home, cwd, "sess-new", []agent.Message{
		agent.UserMessage(agent.TextBlock("the recent conversation")),
		agent.AssistantMessage(agent.TextBlock("a reply")),
	}, 1*time.Hour)

	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "m", progCh)
	var rt *sessionRuntime
	sess.print.onSession = func(r *sessionRuntime) { rt = r }

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/resume")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && d.Dialog.Kind == ui.DialogList &&
			d.Dialog.Title == "Resume session" && len(d.Dialog.Items) == 2 &&
			d.Dialog.Items[0].Value == "sess-new" // most-recent first
	}, 2*time.Second) {
		t.Fatal("/resume did not open the picker with sessions newest-first")
	}

	// Select the highlighted (first = most recent) session.
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Resumed conversation") && strings.Contains(s.Text, "recent conversation")
	}, 2*time.Second) {
		t.Fatal("selecting a session did not echo the resume confirmation")
	}

	// The live conversation's history was seeded from the chosen session.
	if rt == nil || rt.conv == nil {
		t.Fatal("no runtime captured")
	}
	// Give the resume goroutine a moment to seed History after the echo.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rt.conv.History) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(rt.conv.History) != 2 {
		t.Fatalf("conv.History seeded with %d messages, want 2 (from sess-new)", len(rt.conv.History))
	}
	prog.Quit()
}

// TestInteractiveResumeEmpty: /resume with no saved sessions reports the
// empty-state wording.
func TestInteractiveResumeEmpty(t *testing.T) {
	home := t.TempDir()
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "m", progCh)
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/resume")
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "No conversations found in this project.")
	}, 2*time.Second) {
		t.Fatal("/resume with no sessions did not report the empty state")
	}
	prog.Quit()
}
