package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/toolreg"
)

// TestRunPrintSession_LocalBuiltinClear confirms the print path surfaces a
// locally-handled builtin (/clear) without a model round-trip: it prints the
// rendered outcome and exits 0.
func TestRunPrintSession_LocalBuiltinClear(t *testing.T) {
	env, out, errb := newTestEnv()
	called := false
	s := &printSession{
		Mode: toolreg.ModeDefault,
		newCaller: func(*printSession) (agent.ModelCaller, error) {
			return recordingCaller{onCall: func() { called = true }}, nil
		},
	}
	code := runPrintSession(env, s, "/clear")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errb.String())
	}
	if called {
		t.Fatal("model was called for a local builtin /clear")
	}
	if got := strings.TrimSpace(out.String()); got != "Conversation cleared." {
		t.Fatalf("stdout = %q, want %q", got, "Conversation cleared.")
	}
}

// TestRunPrintSession_LocalBuiltinModel confirms /model prints the rendered
// outcome locally.
func TestRunPrintSession_LocalBuiltinModel(t *testing.T) {
	env, out, _ := newTestEnv()
	s := &printSession{
		Mode:      toolreg.ModeDefault,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return recordingCaller{}, nil },
	}
	if code := runPrintSession(env, s, "/model sonnet"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != "Model set to sonnet." {
		t.Fatalf("stdout = %q, want %q", got, "Model set to sonnet.")
	}
}

// TestRunPrintSession_Exit confirms /exit exits 0 with no output.
func TestRunPrintSession_Exit(t *testing.T) {
	env, out, _ := newTestEnv()
	s := &printSession{
		Mode:      toolreg.ModeDefault,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return recordingCaller{}, nil },
	}
	if code := runPrintSession(env, s, "/exit"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty on /exit", out.String())
	}
}

// TestRenderLocalEffect covers the one-line rendering of each local effect.
func TestRenderLocalEffect(t *testing.T) {
	cases := []struct {
		eff  *builtincmds.Effect
		want string
	}{
		{nil, ""},
		{&builtincmds.Effect{Kind: builtincmds.EffectPrintText, Text: "hi"}, "hi"},
		{&builtincmds.Effect{Kind: builtincmds.EffectSetModel, Model: "opus"}, "Model set to opus."},
		{&builtincmds.Effect{Kind: builtincmds.EffectClearTranscript}, "Conversation cleared."},
		{&builtincmds.Effect{Kind: builtincmds.EffectTriggerCompaction}, "Conversation compacted."},
		{&builtincmds.Effect{Kind: builtincmds.EffectOpenDialog, Dialog: "help"}, ""},
	}
	for _, c := range cases {
		if got := renderLocalEffect(c.eff); got != c.want {
			t.Errorf("renderLocalEffect(%+v) = %q, want %q", c.eff, got, c.want)
		}
	}
}

// recordingCaller is a model caller that records whether it was called and ends
// the turn with fixed text.
type recordingCaller struct{ onCall func() }

func (c recordingCaller) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	if c.onCall != nil {
		c.onCall()
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("model reply")),
		StopReason: agent.StopEndTurn,
	}, nil
}
