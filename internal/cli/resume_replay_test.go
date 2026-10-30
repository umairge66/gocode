package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// snapshot returns a copy of the messages the fake program has received so far.
func (p *fakeProgram) snapshot() []tea.Msg {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]tea.Msg, len(p.sent))
	copy(out, p.sent)
	return out
}

// TestReplayResumeHistory_OrderAndKinds is the core unit test: a resumed history
// of [user "hi", assistant "hello" + tool_use, tool_result] replays, in order,
// as a resume separator → AppendUserMsg("hi") → AppendAssistantMsg("hello") →
// AppendToolCallMsg(id,verb,args) → UpdateToolCallMsg(id,result).
func TestReplayResumeHistory_OrderAndKinds(t *testing.T) {
	input := json.RawMessage(`{"command":"ls -la"}`)
	history := []agent.Message{
		agent.UserMessage(agent.TextBlock("hi")),
		agent.AssistantMessage(
			agent.TextBlock("hello"),
			agent.ToolUseBlock("tool-1", "Bash", input),
		),
		agent.UserMessage(agent.ToolResultBlock("tool-1", "file1\nfile2", false)),
	}

	var got []tea.Msg
	replayResumeHistory(func(m tea.Msg) { got = append(got, m) }, "My Title", history)

	if len(got) != 5 {
		t.Fatalf("got %d messages, want 5: %#v", len(got), got)
	}

	sep, ok := got[0].(ui.AppendSystemMsg)
	if !ok || !strings.Contains(sep.Text, "Resumed conversation: My Title") {
		t.Fatalf("got[0] = %#v, want resume separator with title", got[0])
	}
	if u, ok := got[1].(ui.AppendUserMsg); !ok || u.Text != "hi" {
		t.Fatalf("got[1] = %#v, want AppendUserMsg(hi)", got[1])
	}
	if a, ok := got[2].(ui.AppendAssistantMsg); !ok || a.Text != "hello" {
		t.Fatalf("got[2] = %#v, want AppendAssistantMsg(hello)", got[2])
	}
	tc, ok := got[3].(ui.AppendToolCallMsg)
	if !ok || tc.Tool.ID != "tool-1" || tc.Tool.Name != "Bash" || tc.Tool.Args != "ls -la" {
		t.Fatalf("got[3] = %#v, want AppendToolCallMsg(tool-1, Bash, ls -la)", got[3])
	}
	uc, ok := got[4].(ui.UpdateToolCallMsg)
	if !ok || uc.Tool.ID != "tool-1" || uc.Tool.ResultText != "file1\nfile2" || uc.Tool.IsError {
		t.Fatalf("got[4] = %#v, want UpdateToolCallMsg(tool-1, result)", got[4])
	}
}

// TestReplayResumeHistory_SkipsUnrenderableBlocks: thinking/redacted_thinking,
// empty text, and an orphan tool_result (no matching tool_use) are dropped.
func TestReplayResumeHistory_SkipsUnrenderableBlocks(t *testing.T) {
	history := []agent.Message{
		agent.AssistantMessage(
			agent.ContentBlock{Type: agent.BlockThinking, Thinking: "scratch work"},
			agent.ContentBlock{Type: "redacted_thinking", Data: "xxxx"},
			agent.TextBlock("   "), // whitespace-only
			agent.TextBlock("real answer"),
		),
		// tool_result for a tool_use that never appeared → dropped.
		agent.UserMessage(agent.ToolResultBlock("ghost", "orphaned", false)),
	}

	var got []tea.Msg
	replayResumeHistory(func(m tea.Msg) { got = append(got, m) }, "T", history)

	// separator + the single "real answer" assistant message only.
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 (separator + real answer): %#v", len(got), got)
	}
	if a, ok := got[1].(ui.AppendAssistantMsg); !ok || a.Text != "real answer" {
		t.Fatalf("got[1] = %#v, want AppendAssistantMsg(real answer)", got[1])
	}
	for _, m := range got {
		switch m.(type) {
		case ui.AppendToolCallMsg, ui.UpdateToolCallMsg:
			t.Fatalf("orphan tool_result produced a tool block: %#v", m)
		}
	}
}

// TestInteractiveResumePickerReplays: selecting a session in the /resume picker
// re-renders its history into the transcript (separator + each past message).
func TestInteractiveResumePickerReplays(t *testing.T) {
	home := t.TempDir()
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	cwd := home

	seedSession(t, home, cwd, "sess-new", []agent.Message{
		agent.UserMessage(agent.TextBlock("the recent conversation")),
		agent.AssistantMessage(agent.TextBlock("a prior reply")),
	}, time.Hour)

	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "m", progCh)
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/resume")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && d.Dialog.Kind == ui.DialogList && d.Dialog.Title == "Resume session"
	}, 2*time.Second) {
		t.Fatal("/resume did not open the picker")
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	// The replayed user + assistant messages must both land in the transcript.
	if !prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.AppendUserMsg)
		return ok && u.Text == "the recent conversation"
	}, 2*time.Second) {
		t.Fatal("resumed user message was not replayed into the transcript")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		a, ok := m.(ui.AppendAssistantMsg)
		return ok && a.Text == "a prior reply"
	}, 2*time.Second) {
		t.Fatal("resumed assistant message was not replayed into the transcript")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Resumed conversation:")
	}, 2*time.Second) {
		t.Fatal("resume separator/note missing")
	}
	prog.Quit()
}

// TestInteractiveStartupResumeReplays: launching with --resume <id> replays the
// conversation after the welcome banner (banner first, then replayed history).
func TestInteractiveStartupResumeReplays(t *testing.T) {
	home := t.TempDir()
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	cwd := home

	seedSession(t, home, cwd, "startup-sess", []agent.Message{
		agent.UserMessage(agent.TextBlock("launch-time question")),
		agent.AssistantMessage(agent.TextBlock("launch-time answer")),
	}, time.Hour)

	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "m", progCh)
	sess.print.resume = resumeIntent{resume: true, value: "startup-sess"}

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)

	if !prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.AppendUserMsg)
		return ok && u.Text == "launch-time question"
	}, 3*time.Second) {
		t.Fatal("--resume startup did not replay the conversation")
	}

	// Ordering: the welcome banner is pushed before any replayed history.
	snap := prog.snapshot()
	bannerIdx, userIdx := -1, -1
	for i, m := range snap {
		if _, ok := m.(ui.SetBannerMsg); ok && bannerIdx == -1 {
			bannerIdx = i
		}
		if u, ok := m.(ui.AppendUserMsg); ok && u.Text == "launch-time question" && userIdx == -1 {
			userIdx = i
		}
	}
	if bannerIdx == -1 || userIdx == -1 || bannerIdx > userIdx {
		t.Fatalf("banner must precede replayed history: bannerIdx=%d userIdx=%d", bannerIdx, userIdx)
	}
	prog.Quit()
}

// TestPrintResumeDoesNotReplay: the -p path loads history into context but never
// re-renders it into a TUI transcript (there is none) — no resume separator is
// printed to stdout.
func TestPrintResumeDoesNotReplay(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	env, out, errb := newTestEnv()
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
	s := &printSession{
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
		resume:    resumeIntent{resume: true, value: "prev-session"},
	}
	if code := runPrintSession(env, s, "follow-up"); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errb.String())
	}
	if strings.Contains(out.String(), "Resumed conversation:") || strings.Contains(out.String(), "— Resumed") {
		t.Fatalf("-p resume must not emit the TUI replay separator; stdout:\n%s", out.String())
	}
}

// TestInteractiveResumeSwitchReplaysEachOnce: a startup --resume of one session
// followed by a /resume of a DIFFERENT session replays each history once, each
// behind its own separator (no double-render of the same history).
func TestInteractiveResumeSwitchReplaysEachOnce(t *testing.T) {
	home := t.TempDir()
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	cwd := home

	seedSession(t, home, cwd, "sess-a", []agent.Message{
		agent.UserMessage(agent.TextBlock("alpha question")),
	}, 2*time.Hour)
	seedSession(t, home, cwd, "sess-b", []agent.Message{
		agent.UserMessage(agent.TextBlock("beta question")),
	}, time.Hour)

	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "m", progCh)
	sess.print.resume = resumeIntent{resume: true, value: "sess-a"}

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)

	// Startup replay of sess-a.
	if !prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.AppendUserMsg)
		return ok && u.Text == "alpha question"
	}, 3*time.Second) {
		t.Fatal("startup resume of sess-a not replayed")
	}

	// Now switch to sess-b via the picker (resolve directly by id).
	submitLine(prog, "/resume sess-b")
	if !prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.AppendUserMsg)
		return ok && u.Text == "beta question"
	}, 3*time.Second) {
		t.Fatal("/resume of sess-b not replayed")
	}

	// Each history replayed exactly once; two separators total.
	snap := prog.snapshot()
	var alpha, beta, seps int
	for _, m := range snap {
		if u, ok := m.(ui.AppendUserMsg); ok {
			if u.Text == "alpha question" {
				alpha++
			}
			if u.Text == "beta question" {
				beta++
			}
		}
		if s, ok := m.(ui.AppendSystemMsg); ok && strings.HasPrefix(s.Text, "— Resumed conversation") {
			seps++
		}
	}
	if alpha != 1 || beta != 1 {
		t.Fatalf("each history must replay once: alpha=%d beta=%d", alpha, beta)
	}
	if seps != 2 {
		t.Fatalf("want 2 resume separators (one per session), got %d", seps)
	}
	prog.Quit()
}
