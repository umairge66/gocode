package ui

import (
	"strings"
	"testing"
)

// TestRetryStatusReplacesLabel: a retry message replaces the active label
// (keeping the elapsed / interrupt meta), clears back to the label, and is
// ignored while no status is active.
func TestRetryStatusReplacesLabel(t *testing.T) {
	vs := ViewState{}
	vs = Reduce(vs, SetRetryStatus{Text: "ignored"})
	if vs.Status.Retry != "" {
		t.Fatalf("retry set while idle: %q", vs.Status.Retry)
	}
	vs = Reduce(vs, SetStatus{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	vs = Reduce(vs, SetSpinnerMeta{Elapsed: 5})
	vs = Reduce(vs, SetRetryStatus{Text: "API overloaded · Retrying in 4s · attempt 2/10"})
	line := vs.Status.StatusLine()
	if !strings.HasPrefix(line, "API overloaded · Retrying in 4s · attempt 2/10") || !strings.Contains(line, "5s") || !strings.Contains(line, "esc to interrupt") {
		t.Fatalf("line = %q", line)
	}
	vs = Reduce(vs, SetRetryStatus{Text: ""})
	if line := vs.Status.StatusLine(); !strings.HasPrefix(line, "Thinking") {
		t.Fatalf("cleared line = %q", line)
	}
}
