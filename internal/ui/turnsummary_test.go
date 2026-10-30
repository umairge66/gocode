package ui

import (
	"strings"
	"testing"
	"time"
)

func TestFormatElapsed(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{400 * time.Millisecond, "0s"},
		{45 * time.Second, "45s"},
		{59*time.Second + 600*time.Millisecond, "1m 0s"},
		{2*time.Minute + 14*time.Second, "2m 14s"},
		{time.Hour + 5*time.Second, "1h 0m 5s"},
		{3*time.Hour + 2*time.Minute + 1*time.Second, "3h 2m 1s"},
	} {
		if got := FormatElapsed(tc.d); got != tc.want {
			t.Errorf("FormatElapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestTurnSummary(t *testing.T) {
	at := time.Date(2026, 10, 24, 14, 32, 9, 0, time.Local)
	if got := TurnSummary(2*time.Minute+14*time.Second, "done", at); got != "2m 14s · done · 14:32" {
		t.Fatalf("TurnSummary = %q", got)
	}
}

// The summary is drawn in the status slot above the input only while idle,
// and a new active status (next turn) replaces it.
func TestTurnSummaryRendersAboveInput(t *testing.T) {
	vs := Reduce(New(80), SetStatus{Status: Status{Kind: StatusIdle, Done: "2m 14s · done · 14:32"}})
	for _, r := range []Renderer{PlainRenderer{}, NewLipglossRenderer()} {
		out := strings.Join(r.Render(vs), "\n")
		if !strings.Contains(out, "✻ 2m 14s · done · 14:32") {
			t.Errorf("%T: summary line missing:\n%s", r, out)
		}
	}
	vs = Reduce(vs, SetStatus{Status: Status{Kind: StatusThinking, Label: "Thinking"}})
	if strings.Contains(strings.Join(PlainRenderer{}.Render(vs), "\n"), "· done ·") {
		t.Error("summary still shown after the next turn started")
	}
}
