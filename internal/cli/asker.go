package cli

import (
	"context"
	"errors"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/tools/interact"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the interactive AskUserQuestion round-trip: the concrete
// toolrun.Asker that drives the ui Bubble Tea dialog. When the model calls
// AskUserQuestion, the toolrun adapter invokes dialogAsker.Ask, which presents
// one dialog per question, blocks until the user confirms (Enter) or dismisses
// (esc/ctrl+c), maps each chosen option value back to its canonical label, and
// returns the answers keyed by question text.
//
// The seam keeps the UI thread free: Ask runs on the conversation turn's
// goroutine (where the tool call executes), posts OpenDialogMsg to the program,
// and waits on a channel that the ui callbacks (OnDialogSubmit / OnDialogCancel)
// fulfill from the single program goroutine. No real TTY is required — a test
// can drive the same path through the fake program.

// errAskCancelled reports that the user dismissed the AskUserQuestion dialog
// without answering (esc / ctrl+c).
var errAskCancelled = errors.New("AskUserQuestion: no answer (the dialog was dismissed)")

// askOtherLabel is the automatic free-text / "Other" option added to every
// choice question ("There should be no 'Other' option, that will be provided
// automatically"). The option-only dialog cannot collect free text, so
// selecting it records the "(notes only)" sentinel.
const askOtherLabel = "Other"

// dialogReply carries the outcome of one dialog: the confirmed option values, or
// cancelled when the dialog was dismissed.
type dialogReply struct {
	values    []string
	cancelled bool
}

// dialogAsker is the ui-driven toolrun.Asker. send posts messages to the program
// (bound once the program exists); pending is the channel the current question's
// reply is delivered on.
type dialogAsker struct {
	mu      sync.Mutex
	send    func(tea.Msg)
	pending chan dialogReply
	// status reports the open question into a background job's state (nil
	// outside a background worker).
	status *jobStatus
}

// newDialogAsker returns an unbound asker. The send func is a no-op until bind
// is called with the running program's Send, so injecting the asker into the
// dispatcher before the program is built is safe.
func newDialogAsker() *dialogAsker {
	return &dialogAsker{send: func(tea.Msg) {}}
}

// bind attaches the program's Send func. Called once the program is built.
func (a *dialogAsker) bind(send func(tea.Msg)) {
	a.mu.Lock()
	a.send = send
	a.mu.Unlock()
}

// submit is the ui.Model.OnDialogSubmit seam: deliver the confirmed values.
func (a *dialogAsker) submit(values []string) { a.deliver(dialogReply{values: values}) }

// cancel is the ui.Model.OnDialogCancel seam: deliver a dismissal.
func (a *dialogAsker) cancel() { a.deliver(dialogReply{cancelled: true}) }

// deliver hands a reply to the question currently awaiting one (if any). It
// clears the pending channel so a stray callback cannot deliver twice.
func (a *dialogAsker) deliver(r dialogReply) {
	a.mu.Lock()
	ch := a.pending
	a.pending = nil
	a.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// Ask presents each question as a modal dialog, blocking for the user's answer,
// and returns the answers keyed by question text (the
// AskUserQuestionResult shape). A dismissed dialog returns errAskCancelled; a
// cancelled context (e.g. the turn was interrupted) returns ctx.Err().
func (a *dialogAsker) Ask(ctx context.Context, req interact.AskRequest) (interact.AskUserQuestionResult, error) {
	answers := make(map[string]interact.StringOrList, len(req.Questions))
	if len(req.Questions) > 0 {
		q := req.Questions[0]
		labels := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			labels = append(labels, o.Label)
		}
		a.status.setBlock(questionBlockText(q.Question, labels))
		defer a.status.clearBlock()
	}
	for _, q := range req.Questions {
		dlg, labelFor := buildAskDialog(q)

		ch := make(chan dialogReply, 1)
		a.mu.Lock()
		a.pending = ch
		send := a.send
		a.mu.Unlock()

		send(ui.OpenDialogMsg{Dialog: dlg})

		select {
		case <-ctx.Done():
			// Turn cancelled: dismiss the dialog and abandon the ask.
			a.mu.Lock()
			a.pending = nil
			a.mu.Unlock()
			send(ui.CloseDialogMsg{})
			return interact.AskUserQuestionResult{}, ctx.Err()
		case r := <-ch:
			if r.cancelled {
				return interact.AskUserQuestionResult{}, errAskCancelled
			}
			labels := make([]string, 0, len(r.values))
			for _, v := range r.values {
				labels = append(labels, labelFor(v))
			}
			answers[q.Question] = interact.StringOrList{Values: labels}
		}
	}
	return interact.AskUserQuestionResult{Answers: answers}, nil
}

// buildAskDialog turns one structured question into a ui.Dialog plus the
// answer→label mapping (a reported value is mapped to its canonical
// option label, falling back to the value verbatim). Each option's canonical
// label is used as both the display Label and the reported Value. A choice
// question gets the automatic "Other" option appended; a question with no
// options (text/number kind — the option-only dialog cannot host a free-text or
// numeric input) is presented as a single "(notes only)" acknowledgement so the
// round-trip still completes.
func buildAskDialog(q interact.AskRequestQuestion) (*ui.Dialog, func(value string) string) {
	labelByValue := make(map[string]string)
	var opts []ui.DialogOption

	for _, o := range q.Options {
		opts = append(opts, ui.DialogOption{Label: o.Label, Value: o.Label})
		labelByValue[o.Label] = o.Label
	}

	if len(opts) == 0 {
		// text / number question: no selectable options. Offer a single
		// acknowledgement that records the "(notes only)" sentinel.
		opts = append(opts, ui.DialogOption{Label: interact.AskNotesOnly, Value: interact.AskNotesOnly})
		labelByValue[interact.AskNotesOnly] = interact.AskNotesOnly
	} else {
		// choice question: append the automatic "Other" option. The option dialog
		// cannot collect the free text, so a chosen "Other" maps to "(notes only)".
		opts = append(opts, ui.DialogOption{Label: askOtherLabel, Value: askOtherLabel})
		labelByValue[askOtherLabel] = interact.AskNotesOnly
	}

	var body []string
	if q.Header != "" {
		body = append(body, "["+q.Header+"]")
	}
	dlg := ui.AskDialog(q.Question, q.MultiSelect, opts, body...)

	labelFor := func(value string) string {
		if label, ok := labelByValue[value]; ok {
			return label
		}
		return value
	}
	return dlg, labelFor
}
