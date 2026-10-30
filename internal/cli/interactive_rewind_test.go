package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/filehistory"
	"github.com/umairge66/gocode/internal/ui"
)

// TestRewindCommandNothingYet drives /rewind through a fake-backed interactive
// session: with no prompts yet the picker reports there is nothing to rewind.
func TestRewindCommandNothingYet(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()

	submitLine(prog, "/rewind")
	if !sawSystemMsg(prog, "Nothing to rewind to yet.") {
		t.Fatal("/rewind did not open the (empty) rewind picker")
	}
}

// TestRewindNonInteractiveGuidance checks the -p rendering of /rewind.
func TestRewindNonInteractiveGuidance(t *testing.T) {
	got := renderLocalEffect(&builtincmds.Effect{Kind: builtincmds.EffectOpenDialog, Dialog: "rewind"})
	if !strings.Contains(got, "needs an interactive terminal") {
		t.Fatalf("renderLocalEffect(rewind) = %q", got)
	}
}

// TestRewindDialogRestoresCodeAndConversation runs two simulated turns that
// edit a file, then picks the first prompt and "Restore code and
// conversation": the file is back to its original content, the history is
// truncated, and the prompt text is handed back.
func TestRewindDialogRestoresCodeAndConversation(t *testing.T) {
	cwd := t.TempDir()
	file := filepath.Join(cwd, "a.txt")
	if err := os.WriteFile(file, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	fh := filehistory.New(filepath.Join(t.TempDir(), "file-history"), "sess", cwd)
	fileHistories.Store(conv, fh)
	defer fileHistories.Delete(conv)

	userTurn := func(prompt, content string) {
		conv.History = append(conv.History, agent.UserMessage(agent.TextBlock(prompt)))
		snapshotPrompt(conv, fh, conv.History)
		if err := fh.TrackEdit(file); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		conv.History = append(conv.History, agent.AssistantMessage(agent.TextBlock("done")))
	}
	userTurn("first edit", "one\ntwo\n")
	userTurn("second edit", "three\n")

	var dialogs []*ui.Dialog
	answers := []string{"0", "both"}
	msgs := make(chan tea.Msg, 64)
	var d *interactiveDialogs
	d = &interactiveDialogs{conv: conv, send: func(m tea.Msg) {
		if od, ok := m.(ui.OpenDialogMsg); ok {
			dialogs = append(dialogs, od.Dialog)
			if len(answers) == 0 {
				d.cancel()
				return
			}
			v := answers[0]
			answers = answers[1:]
			d.submit([]string{v})
			return
		}
		msgs <- m
	}}
	d.rewind()

	if len(dialogs) != 2 {
		t.Fatalf("opened %d dialogs, want 2", len(dialogs))
	}
	picker := dialogs[0]
	if picker.Title != "Rewind" || len(picker.Items) != 2 {
		t.Fatalf("picker = %+v", picker)
	}
	// Newest first, each with what its turn changed.
	if picker.Items[0].Label != "second edit" || picker.Items[1].Label != "first edit" {
		t.Fatalf("picker order = %q, %q", picker.Items[0].Label, picker.Items[1].Label)
	}
	if got := picker.Items[1].Detail; got != "a.txt +1 -0" {
		t.Fatalf("first turn detail = %q", got)
	}
	var labels []string
	for _, it := range dialogs[1].Items {
		labels = append(labels, it.Label)
	}
	if strings.Join(labels, "|") != "Restore code and conversation|Restore conversation|Restore code|Never mind" {
		t.Fatalf("options = %v", labels)
	}

	data, _ := os.ReadFile(file)
	if string(data) != "one\n" {
		t.Fatalf("file after rewind = %q", data)
	}
	if len(conv.History) != 0 {
		t.Fatalf("history after rewind has %d messages", len(conv.History))
	}
	sawPrompt, sawReset := false, false
	for len(msgs) > 0 {
		switch m := (<-msgs).(type) {
		case ui.SetComposerTextMsg:
			sawPrompt = strings.Contains(m.Text, "first edit")
		case ui.ResetTranscriptMsg:
			sawReset = true
		}
	}
	if !sawPrompt {
		t.Fatal("the rewound prompt was not put back in the composer")
	}
	if !sawReset {
		t.Fatal("the screen was not reset for the rewound conversation")
	}
}

// TestRewindDialogCodeOnly restores the code but keeps the conversation.
func TestRewindDialogCodeOnly(t *testing.T) {
	cwd := t.TempDir()
	created := filepath.Join(cwd, "new.txt")
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	fh := filehistory.New(filepath.Join(t.TempDir(), "file-history"), "sess", cwd)
	fileHistories.Store(conv, fh)
	defer fileHistories.Delete(conv)

	conv.History = append(conv.History, agent.UserMessage(agent.TextBlock("create a file")))
	snapshotPrompt(conv, fh, conv.History)
	_ = fh.TrackEdit(created)
	if err := os.WriteFile(created, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conv.History = append(conv.History, agent.AssistantMessage(agent.TextBlock("done")))

	answers := []string{"0", "code"}
	var d *interactiveDialogs
	d = &interactiveDialogs{conv: conv, send: func(m tea.Msg) {
		if _, ok := m.(ui.OpenDialogMsg); ok {
			v := answers[0]
			answers = answers[1:]
			d.submit([]string{v})
		}
	}}
	d.rewind()

	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("created file should be deleted, stat err = %v", err)
	}
	if len(conv.History) != 2 {
		t.Fatalf("code-only rewind changed the conversation (%d messages)", len(conv.History))
	}
}
