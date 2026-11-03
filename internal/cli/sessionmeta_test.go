package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/toolreg"
)

// TestRenameAndTagCommands drives /rename and /tag against a live session
// store, as both hosts do.
func TestRenameAndTagCommands(t *testing.T) {
	st := sessionstore.New(t.TempDir())
	sess, _ := st.Create("/w", "s1")
	_ = sess.AppendMessages([]agent.Message{agent.UserMessage(agent.TextBlock("hello"))})
	conv := &conversation.Conversation{Store: sess}

	if got := renameCommand(conv, ""); got != "Usage: /rename <title>" {
		t.Errorf("bare rename = %q", got)
	}
	if got := renameCommand(conv, "  my   session "); got != `Renamed the conversation to "my session".` {
		t.Errorf("rename = %q", got)
	}
	if got := renameCommand(conv, ""); !strings.Contains(got, `"my session"`) {
		t.Errorf("bare rename after = %q", got)
	}
	steps := []struct{ args, want string }{
		{"", "This conversation has no tags. Use /tag <name> to add one."},
		{"bug", `Tagged the conversation "bug".`},
		{"perf", `Tagged the conversation "perf".`},
		{"", "Tags: bug, perf"},
		{"bug", `Removed tag "bug".`}, // toggles
		{"--remove nope", `This conversation has no tag "nope".`},
		{"--remove perf", `Removed tag "perf".`},
		{"x", `Tagged the conversation "x".`},
		{"--clear", "Removed all tags."},
		{"", "This conversation has no tags. Use /tag <name> to add one."},
	}
	for _, s := range steps {
		if got := tagCommand(conv, s.args); got != s.want {
			t.Errorf("/tag %q = %q, want %q", s.args, got, s.want)
		}
	}
	if m, _ := st.Find("/w", "my session"); m.ID != "s1" {
		t.Errorf("Find by custom title = %+v", m)
	}
	// No store: a clear refusal.
	if got := tagCommand(&conversation.Conversation{}, "x"); got != sessionMetaUnavailable {
		t.Errorf("no store = %q", got)
	}
	// The built-ins route to the host.
	tbl := builtincmds.NewTable()
	for _, name := range []string{"rename", "tag"} {
		if eff, ok := tbl.Dispatch("/" + name + " a b"); !ok || eff.Kind != builtincmds.EffectOpenDialog || eff.Dialog != name || eff.Args != "a b" {
			t.Errorf("/%s effect = %+v %v", name, eff, ok)
		}
	}
}

func TestParseResumeSessionAt(t *testing.T) {
	d, rest := parseDispatchDefaults([]string{"--resume", "abc", "--resume-session-at", "u-1", "--fork-session"})
	if d == nil || d.ResumeSessionAt != "u-1" || !d.ForkSession || d.ResumeValue != "abc" || len(rest) != 0 {
		t.Fatalf("parsed %+v rest %v", d, rest)
	}
	if got := resumeIntentFrom(d); got.at != "u-1" {
		t.Errorf("intent = %+v", got)
	}
}

// TestPrintResumeSessionAt: -p --resume <id> --resume-session-at <uuid> runs the
// prompt on the history up to that message; without --fork-session the new
// turns branch the same transcript, with it they go to a new session.
func TestPrintResumeSessionAt(t *testing.T) {
	for _, fork := range []bool{false, true} {
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
		seedSession(t, home, cwd, "prev", []agent.Message{
			agent.UserMessage(agent.TextBlock("q1")),
			agent.AssistantMessage(agent.TextBlock("a1")),
			agent.UserMessage(agent.TextBlock("q2-dropped")),
			agent.AssistantMessage(agent.TextBlock("a2-dropped")),
		}, time.Hour)
		st := sessionstore.New(filepath.Join(home, ".config", "gocode"))
		m, _ := st.Find(cwd, "prev")
		recs, _, _ := st.Records(m, false)
		at := recs[1].UUID

		caller := &resumeRecCaller{}
		var rt *sessionRuntime
		s := &printSession{
			Mode: toolreg.ModeDefault, Provider: providerOpenRouter, Model: "m",
			newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
			resume:    resumeIntent{resume: true, value: "prev", at: at, fork: fork},
			onSession: func(r *sessionRuntime) { rt = r },
		}
		if code := runPrintSession(env, s, "q2-new"); code != 0 {
			t.Fatalf("fork=%v exit = %d; stderr=%s", fork, code, errb.String())
		}
		var joined []string
		for _, msg := range caller.lastSeen() {
			for _, b := range msg.Content {
				joined = append(joined, b.Text)
			}
		}
		if got := strings.Join(joined, ","); !strings.HasPrefix(got, "q1,a1,q2-new") || strings.Contains(got, "dropped") {
			t.Errorf("fork=%v model saw %q", fork, got)
		}
		cur := rt.currentSessionID()
		if (cur == "prev") == fork {
			t.Errorf("fork=%v recorded to %q", fork, cur)
		}
		reloaded, _ := st.Load(cwd, cur)
		if len(reloaded) != 4 || messageTextOf(reloaded[2]) != "q2-new" {
			t.Errorf("fork=%v reloaded %d msgs", fork, len(reloaded))
		}
		// The original's dropped turns are still on disk.
		if full, _, _ := st.Records(m, true); len(full) < 4 {
			t.Errorf("fork=%v original lost lines: %d", fork, len(full))
		}
	}
}

// messageTextOf is a message's first text block.
func messageTextOf(m agent.Message) string {
	for _, blk := range m.Content {
		if blk.Type == agent.BlockText {
			return blk.Text
		}
	}
	return ""
}
