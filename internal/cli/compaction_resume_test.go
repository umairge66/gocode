package cli

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

func summaryTurn(text string) agent.Message {
	m := compaction.BuildSummaryMessage(text, false)
	return agent.Message{Role: m.Role, Content: m.Content}
}

// A compacted history replays as: resume separator → "Conversation compacted"
// divider → the post-compaction turns; the summary turn itself is not shown.
func TestReplayResumeHistory_CompactedDivider(t *testing.T) {
	history := []agent.Message{
		summaryTurn("Summary:\nearlier"),
		agent.UserMessage(agent.TextBlock("after")),
		agent.AssistantMessage(agent.TextBlock("reply")),
	}
	var got []tea.Msg
	replayResumeHistory(func(m tea.Msg) { got = append(got, m) }, "T", history)
	if len(got) != 4 {
		t.Fatalf("got %d messages: %#v", len(got), got)
	}
	if d, ok := got[1].(ui.AppendSystemMsg); !ok || !strings.Contains(d.Text, "Conversation compacted") {
		t.Fatalf("got[1] = %#v, want the compacted divider", got[1])
	}
	if u, ok := got[2].(ui.AppendUserMsg); !ok || u.Text != "after" {
		t.Fatalf("got[2] = %#v, want AppendUserMsg(after)", got[2])
	}
	for _, m := range got {
		if u, ok := m.(ui.AppendUserMsg); ok && strings.Contains(u.Text, "earlier") {
			t.Fatal("summary turn rendered in the main transcript")
		}
	}

	// An uncompacted history shows no divider.
	got = nil
	replayResumeHistory(func(m tea.Msg) { got = append(got, m) }, "T", history[1:])
	for _, m := range got {
		if d, ok := m.(ui.AppendSystemMsg); ok && strings.Contains(d.Text, "Conversation compacted") {
			t.Fatal("divider shown for an uncompacted history")
		}
	}
}

// Resuming a compacted session loads only the post-boundary history, and the
// continuation appends to the same file without rewriting it.
func TestResumeCompactedSessionLoadsFromLastBoundary(t *testing.T) {
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

	st := sessionstore.New(filepath.Join(home, ".config", "gocode"))
	sess, err := st.Create(cwd, "cmp")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessages([]agent.Message{
		agent.UserMessage(agent.TextBlock("pre-compaction question")),
		agent.AssistantMessage(agent.TextBlock("pre-compaction answer")),
	})
	_ = sess.AppendCompaction(compaction.BoundaryMarker{Trigger: compaction.TriggerAuto, PreTokens: 99}, []agent.Message{summaryTurn("Summary:\nthe gist")})
	_ = sess.AppendMessages([]agent.Message{agent.UserMessage(agent.TextBlock("post question")), agent.AssistantMessage(agent.TextBlock("post answer"))})

	caller := &resumeRecCaller{}
	s := &printSession{
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
		resume:    resumeIntent{resume: true, value: "cmp"},
	}
	if code := runPrintSession(env, s, "follow-up"); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errb.String())
	}
	joined := ""
	for _, m := range caller.lastSeen() {
		for _, b := range m.Content {
			joined += b.Text + "\n"
		}
	}
	if strings.Contains(joined, "pre-compaction") {
		t.Fatalf("resumed request carried pre-compaction turns:\n%s", joined)
	}
	for _, want := range []string{"the gist", "post question", "post answer", "follow-up"} {
		if !strings.Contains(joined, want) {
			t.Errorf("resumed request missing %q:\n%s", want, joined)
		}
	}
	reloaded, _ := st.Load(cwd, "cmp")
	if len(reloaded) != 5 { // summary + 2 post + user + assistant
		t.Fatalf("reloaded %d messages, want 5", len(reloaded))
	}
	m, ok := st.Find(cwd, "cmp")
	if !ok || m.Title != "pre-compaction question" {
		t.Fatalf("title = %+v", m)
	}
}
