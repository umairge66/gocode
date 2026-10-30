package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// errCaller is an agent.ModelCaller that fails every model call with a fixed
// error, used to drive the -p friendly-error path.
type errCaller struct{ err error }

func (c errCaller) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, c.err
}

// creditsError is a typed provider APIError in the credits category, as the
// provider layer returns on a 402 / insufficient_quota.
func creditsError() *provider.APIError {
	return &provider.APIError{
		Provider: "openrouter",
		Status:   402,
		Category: provider.CategoryCredits,
		Message:  "Out of credits on openrouter. Add credits (or switch providers with /provider) and try again.",
	}
}

// networkError is a typed provider APIError in the network category, as the
// provider layer returns on a transport failure (disconnect mid-turn).
func networkError() *provider.APIError {
	return &provider.APIError{
		Provider:  "openrouter",
		Category:  provider.CategoryNetwork,
		Message:   "Network error — couldn't reach openrouter. Check your internet connection and try again.",
		Retryable: true,
	}
}

// TestTurnDriver_CreditsErrorRecovery drives the interactive turn driver through
// a mid-turn credits failure: the model streamed a partial assistant message and
// opened a tool-call block, then the next model call failed with an
// APIError(credits). The driver must FINALIZE the partial stream, CLOSE the
// in-flight tool block (marked "Interrupted (turn failed)"), show the FRIENDLY
// error plus the "/provider" suggestion, and RESET the spinner to idle.
func TestTurnDriver_CreditsErrorRecovery(t *testing.T) {
	conv := conversation.New(fakeCaller{reply: "x"}, fakeToolDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	drv.run(false, func(ctx context.Context) (conversation.Result, error) {
		// A partial assistant message streamed in...
		conv.OnDelta("partial thought before the failure")
		// ...and a tool call was opened but never completed...
		conv.OnToolEvent(conversation.ToolEvent{
			Phase: conversation.ToolStart,
			ID:    "stuck-1",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"echo hi"}`),
		})
		// ...then the next model call failed: out of credits.
		return conversation.Result{}, creditsError()
	})

	// Friendly error message (not a raw "http 402: {...}" blob).
	if !prog.waitFor(func(m tea.Msg) bool {
		e, ok := m.(ui.AppendErrorMsg)
		return ok && strings.Contains(e.Text, "Out of credits") && strings.Contains(e.Text, "openrouter")
	}, 2*time.Second) {
		t.Fatal("friendly credits error was not shown")
	}
	// Actionable suggestion line.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "/provider")
	}, 2*time.Second) {
		t.Fatal("/provider suggestion was not shown for a credits error")
	}
	// Partial stream finalized (Done:true with the accumulated text).
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.StreamAssistantMsg)
		return ok && s.Done && s.Text == "partial thought before the failure"
	}, 2*time.Second) {
		t.Fatal("partial streamed text was not finalized on failure")
	}
	// In-flight tool block closed, marked as a failed-turn interrupt.
	if !prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.UpdateToolCallMsg)
		return ok && u.Tool.ID == "stuck-1" && u.Tool.IsError && strings.Contains(u.Tool.ResultText, "turn failed")
	}, 2*time.Second) {
		t.Fatal("in-flight tool block was not closed with a failed-turn marker")
	}
	// Spinner reset to idle.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && s.Status.Kind == ui.StatusIdle
	}, 2*time.Second) {
		t.Fatal("spinner was not reset to idle after the failure")
	}
}

// TestTurnDriver_NetworkErrorRecovery drives the driver through a mid-turn
// network disconnect surfaced as res.Err (the agent loop captured it): the
// friendly network message and the "connection" suggestion must appear and the
// spinner must reset.
func TestTurnDriver_NetworkErrorRecovery(t *testing.T) {
	conv := conversation.New(fakeCaller{reply: "x"}, fakeToolDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	drv.run(false, func(ctx context.Context) (conversation.Result, error) {
		// A completed tool block earlier in the turn must survive the later drop:
		// a ToolStart followed by its ToolEnd leaves nothing in-flight.
		conv.OnToolEvent(conversation.ToolEvent{Phase: conversation.ToolStart, ID: "done-1", Name: "Read", Input: json.RawMessage(`{"file_path":"/x"}`)})
		conv.OnToolEvent(conversation.ToolEvent{Phase: conversation.ToolEnd, ID: "done-1", Name: "Read", Input: json.RawMessage(`{"file_path":"/x"}`), Result: "file contents"})
		// Then the next model response never arrives: the connection dropped.
		return conversation.Result{Err: networkError()}, nil
	})

	if !prog.waitFor(func(m tea.Msg) bool {
		e, ok := m.(ui.AppendErrorMsg)
		return ok && strings.Contains(e.Text, "Network error") && strings.Contains(e.Text, "openrouter")
	}, 2*time.Second) {
		t.Fatal("friendly network error was not shown")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "connection")
	}, 2*time.Second) {
		t.Fatal("connection suggestion was not shown for a network error")
	}
	// The COMPLETED tool block keeps its real result (not overwritten by a
	// terminal interrupt), since nothing was in-flight at teardown.
	if prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.UpdateToolCallMsg)
		return ok && u.Tool.ID == "done-1" && strings.Contains(u.Tool.ResultText, "turn failed")
	}, 200*time.Millisecond) {
		t.Fatal("a completed tool block was wrongly marked as a failed-turn interrupt")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && s.Status.Kind == ui.StatusIdle
	}, 2*time.Second) {
		t.Fatal("spinner was not reset to idle after the network failure")
	}
}

// TestErrorSuggestion maps the actionable categories to their follow-up lines
// and returns "" for a plain (non-provider) error.
func TestErrorSuggestion(t *testing.T) {
	cases := []struct {
		cat  provider.Category
		want string
	}{
		{provider.CategoryCredits, "/provider"},
		{provider.CategoryAuth, "/login"},
		{provider.CategoryNetwork, "connection"},
		{provider.CategoryRateLimit, "Wait a moment"},
		{provider.CategoryServer, ""},
		{provider.CategoryUnknown, ""},
	}
	for _, tc := range cases {
		got := errorSuggestion(&provider.APIError{Category: tc.cat, Message: "x"})
		if tc.want == "" {
			if got != "" {
				t.Errorf("category %q: suggestion = %q, want empty", tc.cat, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("category %q: suggestion = %q, want to contain %q", tc.cat, got, tc.want)
		}
	}
	if got := errorSuggestion(context.Canceled); got != "" {
		t.Errorf("non-provider error suggestion = %q, want empty", got)
	}
}

// TestRunPrintSession_FriendlyError confirms the -p path prints the FRIENDLY
// provider error and its suggestion to STDERR (never stdout) and exits
// non-zero, rather than a raw "http 402" blob.
func TestRunPrintSession_FriendlyError(t *testing.T) {
	env, out, errb := newTestEnv()
	s := &printSession{
		Mode: toolreg.ModeDefault,
		newCaller: func(*printSession) (agent.ModelCaller, error) {
			return errCaller{err: creditsError()}, nil
		},
	}
	code := runPrintSession(env, s, "question?")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero on a provider error")
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("stdout = %q, want empty (error belongs on stderr)", out.String())
	}
	e := errb.String()
	if !strings.Contains(e, "Out of credits") || !strings.Contains(e, "openrouter") {
		t.Errorf("stderr = %q, want the friendly credits message", e)
	}
	if !strings.Contains(e, "/provider") {
		t.Errorf("stderr = %q, want the /provider suggestion", e)
	}
	if strings.Contains(e, "http 402") || strings.Contains(e, "{") {
		t.Errorf("stderr = %q, should not contain the raw http/JSON blob", e)
	}
}
