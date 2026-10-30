package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/builtincmds"
)

func TestGeneratedPromptBuiltinRunsTurnWithScope(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	var gotScope []string
	var gotEffort string
	undone := false
	c.ScopeCommand = func(allowed []string, effort string) func() {
		gotScope, gotEffort = allowed, effort
		return func() { undone = true }
	}
	c.PromptGenerators = map[string]func(context.Context, string) (string, error){
		builtincmds.GeneratorTeamOnboarding: func(context.Context, string) (string, error) { return "GENERATED", nil },
	}
	res, err := c.Submit(context.Background(), "/team-onboarding")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ResultModel || c.History[0].Content[0].Text != "GENERATED" {
		t.Fatalf("kind=%v history=%+v", res.Kind, c.History)
	}
	if strings.Join(gotScope, "|") != "Edit(ONBOARDING.md)|Bash(ls *)" || gotEffort != "low" || !undone {
		t.Fatalf("scope=%v effort=%q undone=%v", gotScope, gotEffort, undone)
	}

	// No generator: answered locally.
	res, err = c.Submit(context.Background(), "/insights")
	if err != nil || res.Kind != ResultLocal || !strings.Contains(res.Effect.Text, "not available") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// A failing generator is an error, no model turn.
	c.PromptGenerators[builtincmds.GeneratorInsights] = func(context.Context, string) (string, error) { return "", errors.New("boom") }
	n := len(c.History)
	if _, err := c.Submit(context.Background(), "/insights"); err == nil || len(c.History) != n {
		t.Fatalf("err=%v", err)
	}
}
