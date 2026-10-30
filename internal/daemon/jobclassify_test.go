package daemon

import (
	"strings"
	"testing"
)

func TestPreClassify(t *testing.T) {
	cases := []struct {
		name, text           string
		branch, state, tempo string
		needs, detail        string
	}{
		{"result marker", "Did the thing.\nResult: shipped the parser fix", "result-marker", "done", "idle", "", "shipped the parser fix"},
		{"result then next", "Result: phase one done\nNext: wire the UI", "result-then-next", "working", "idle", "", "phase one done"},
		{"failed marker", "Tried everything.\nFailed: the build needs a toolchain I can't install", "failed-marker", "failed", "idle", "", "the build needs a toolchain I can't install"},
		{"blocked marker", "Halfway there.\nBlocked: need the staging password", "blocked-marker", "blocked", "blocked", "need the staging password", "need the staging password"},
		{"needs input", "Needs input: which region?", "blocked-marker", "blocked", "blocked", "which region?", "which region?"},
		{"trailing question", "I found two candidates. Which one should I keep?", "trailing-q", "blocked", "blocked", "Which one should I keep?", "Which one should I keep?"},
		{"wait external", "Pushed the branch and now waiting for CI to finish", "wait-external", "working", "idle", "", "waiting for CI"},
		{"awaiting approval", "The migration is staged. Awaiting your approval before I run it", "awaiting-user", "blocked", "blocked", "Awaiting your approval before I run it", ""},
		{"ask verb", "Two schemas fit. Please confirm the target schema", "ask-verb", "blocked", "blocked", "Please confirm the target schema", ""},
		{"auth prose", "The push failed: 401 unauthorized from the registry", "auth-prose", "blocked", "blocked", "", "authentication required"},
		{"working verb", "Let me run the test suite now", "working-verb", "working", "active", "", "Let me run the test suite now"},
		{"giving up", "Giving up: the repository is archived", "giving-up", "failed", "idle", "", ""},
		{"verdict", "VERDICT: PASS — all checks green", "verdict-marker", "done", "idle", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PreClassify(c.text)
			if got == nil {
				t.Fatalf("PreClassify(%q) = nil", c.text)
			}
			if got.Branch != c.branch || got.State != c.state || got.Tempo != c.tempo {
				t.Fatalf("got %+v", got)
			}
			if c.needs != "" && got.Needs != c.needs {
				t.Fatalf("needs = %q want %q", got.Needs, c.needs)
			}
			if c.detail != "" && got.Detail != c.detail {
				t.Fatalf("detail = %q want %q", got.Detail, c.detail)
			}
		})
	}
}

func TestPreClassifyNoMatch(t *testing.T) {
	for _, text := range []string{
		"",
		"Want me to also update the docs?",       // an offer is not a block
		"Let me know if anything else comes up.", // "Let me know" is not work
		"```\nResult: inside a fence\n```\nDone it.", // fenced markers are ignored
		"I am not waiting for your approval here.",   // negated
	} {
		if got := PreClassify(text); got != nil && got.State != "working" {
			t.Errorf("PreClassify(%q) = %+v", text, got)
		}
	}
}

func TestClassifyFallsBackToLastLine(t *testing.T) {
	c := Classify("Updated three files.\n\nThe tests pass locally.\n")
	if c.Branch != "heuristic" || c.State != "working" || c.Tempo != "idle" || c.Detail != "The tests pass locally." {
		t.Fatalf("got %+v", c)
	}
	long := strings.Repeat("x", 900)
	if d := Classify(long).Detail; len([]rune(d)) != maxDetailChars || !strings.HasSuffix(d, "…") {
		t.Fatalf("detail not capped: %d", len([]rune(d)))
	}
}

func TestAPIFailureState(t *testing.T) {
	for _, c := range []struct{ kind, msg, state, needs string }{
		{"authentication_failed", "", "blocked", "login required — run /login"},
		{"rate_limit", "", "blocked", "rate limited — wait and retry"},
		{"invalid_request", "prompt is too long", "blocked", "request too large — /compact or trim"},
		{"", "", "blocked", "API error — see detail"},
		{"unknown", "", "failed", "API error"},
		{"max_output_tokens", "", "", ""},
	} {
		s, n := APIFailureState(c.kind, c.msg)
		if s != c.state || n != c.needs {
			t.Errorf("APIFailureState(%q) = %q %q", c.kind, s, n)
		}
	}
}

func TestOpenFence(t *testing.T) {
	s := "text\n```go\ncode\n```\nafter"
	if inFence(s, 0) || !inFence(s, strings.Index(s, "code")) || inFence(s, strings.Index(s, "after")) {
		t.Fatal("fence tracking wrong")
	}
	if inFence("a ```not a fence``` b", 15) {
		t.Fatal("an inline run is not a fence")
	}
}
