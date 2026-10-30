package cli

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/tools/interact"
	"github.com/umairge66/gocode/internal/ui"
)

// waitDialogOpen blocks until the program has processed an OpenDialogMsg. Since
// the fake program records then applies each message on its single goroutine (in
// FIFO order), once the open message is recorded any keystroke enqueued after
// this call is guaranteed to reach the open dialog. The test never touches the
// model off-thread (that would race the program goroutine).
func waitDialogOpen(t *testing.T, prog *fakeProgram) {
	t.Helper()
	if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.OpenDialogMsg); return ok }, 2*time.Second) {
		t.Fatal("ask dialog never opened")
	}
}

// newBoundAsker builds a dialogAsker wired to a fake program the same way
// runInteractiveSession wires the real one, and starts the program goroutine.
func newBoundAsker(t *testing.T) (*dialogAsker, *fakeProgram) {
	t.Helper()
	m := ui.NewModel(80)
	m.SetRenderer(ui.PlainRenderer{})
	prog := newFakeProgram(m)
	asker := newDialogAsker()
	asker.bind(prog.Send)
	m.OnDialogSubmit = func(values []string) { asker.submit(values) }
	m.OnDialogCancel = func() { asker.cancel() }
	go func() { _, _ = prog.Run() }()
	return asker, prog
}

// TestDialogAskerMapsChoiceToLabel drives a single-select ask through the fake
// program: a simulated choice maps back to the canonical option label and is
// returned keyed by the question text.
func TestDialogAskerMapsChoiceToLabel(t *testing.T) {
	asker, prog := newBoundAsker(t)
	defer prog.Quit()

	req := interact.AskRequest{
		Tool: protocol.ToolAskUserQuestion,
		Questions: []interact.AskRequestQuestion{{
			Question: "Which database?",
			Header:   "Database",
			Options:  []interact.QuestionOption{{Label: "Postgres"}, {Label: "SQLite"}},
		}},
	}

	type result struct {
		res interact.AskUserQuestionResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := asker.Ask(context.Background(), req)
		done <- result{res, err}
	}()

	// Wait for the dialog, highlight the second option (SQLite) and confirm.
	waitDialogOpen(t, prog)
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Ask returned error: %v", r.err)
		}
		got, ok := r.res.Answers["Which database?"]
		if !ok {
			t.Fatalf("no answer for the question: %+v", r.res.Answers)
		}
		if len(got.Values) != 1 || got.Values[0] != "SQLite" {
			t.Fatalf("answer = %+v, want [SQLite]", got.Values)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after the choice")
	}
}

// TestDialogAskerMultiSelect drives a multi-select ask: two toggled options come
// back as both labels for the question.
func TestDialogAskerMultiSelect(t *testing.T) {
	asker, prog := newBoundAsker(t)
	defer prog.Quit()

	req := interact.AskRequest{
		Questions: []interact.AskRequestQuestion{{
			Question:    "Pick features?",
			Header:      "Features",
			Options:     []interact.QuestionOption{{Label: "Auth"}, {Label: "Cache"}, {Label: "Search"}},
			MultiSelect: true,
		}},
	}

	type result struct {
		res interact.AskUserQuestionResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := asker.Ask(context.Background(), req)
		done <- result{res, err}
	}()

	waitDialogOpen(t, prog)
	// Mark Auth (idx 0), move to Search (idx 2), mark it, confirm.
	prog.press(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Ask returned error: %v", r.err)
		}
		got := r.res.Answers["Pick features?"].Values
		if len(got) != 2 || got[0] != "Auth" || got[1] != "Search" {
			t.Fatalf("answer = %v, want [Auth Search]", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after the multi-select")
	}
}

// TestDialogAskerCancelled confirms that dismissing the dialog (esc) returns the
// cancellation error.
func TestDialogAskerCancelled(t *testing.T) {
	asker, prog := newBoundAsker(t)
	defer prog.Quit()

	req := interact.AskRequest{
		Questions: []interact.AskRequestQuestion{{
			Question: "Proceed?",
			Header:   "Proceed",
			Options:  []interact.QuestionOption{{Label: "Yes"}, {Label: "No"}},
		}},
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := asker.Ask(context.Background(), req)
		errCh <- err
	}()

	waitDialogOpen(t, prog)
	prog.press(tea.KeyPressMsg{Code: tea.KeyEsc})

	select {
	case err := <-errCh:
		if err != errAskCancelled {
			t.Fatalf("Ask error = %v, want errAskCancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after dismissal")
	}
}

// TestDialogAskerOtherOption confirms the automatic "Other" option is appended
// to a choice question and maps to the "(notes only)" sentinel.
func TestDialogAskerOtherOption(t *testing.T) {
	q := interact.AskRequestQuestion{
		Question: "Which?",
		Header:   "Which",
		Options:  []interact.QuestionOption{{Label: "A"}, {Label: "B"}},
	}
	dlg, labelFor := buildAskDialog(q)
	if len(dlg.Options) != 3 {
		t.Fatalf("expected 2 options + Other, got %d", len(dlg.Options))
	}
	if dlg.Options[2].Label != askOtherLabel {
		t.Fatalf("last option = %q, want the Other option", dlg.Options[2].Label)
	}
	if labelFor(askOtherLabel) != interact.AskNotesOnly {
		t.Fatalf("Other maps to %q, want %q", labelFor(askOtherLabel), interact.AskNotesOnly)
	}
	// A normal option maps to itself; an unknown value passes through verbatim.
	if labelFor("A") != "A" || labelFor("zzz") != "zzz" {
		t.Fatalf("label mapping wrong: A=%q zzz=%q", labelFor("A"), labelFor("zzz"))
	}
}
