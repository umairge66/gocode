package hooks

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestFailureBlocks(t *testing.T) {
	failed := Decision{Behavior: BehaviorNone, Continue: true, ErrKind: ErrTimeout, Failed: true}
	if !(CommandHook{Type: HookTypeCommand}).FailureBlocks(failed) {
		t.Error("a failed command hook blocks by default")
	}
	if (CommandHook{Type: HookTypeCommand, OnFailure: OnFailureContinue}).FailureBlocks(failed) {
		t.Error("onFailure continue must not block")
	}
	if !(CommandHook{Type: HookTypeHTTP}).FailureBlocks(failed) {
		t.Error("a failed http hook blocks by default")
	}
	if (CommandHook{Type: HookTypePrompt}).FailureBlocks(failed) {
		t.Error("a prompt hook has no onFailure")
	}
	if (CommandHook{}).FailureBlocks(Decision{ErrKind: ErrExit}) {
		t.Error("an ordinary non-zero exit is the hook's answer, not a failure")
	}
	if !BlocksOnFailure(EventPreToolUse) || BlocksOnFailure(EventStop) || BlocksOnFailure(EventPostToolUse) {
		t.Error("BlocksOnFailure: only the events that gate an action block")
	}
}

// Timeouts, spawn failures and a hook killed by a signal are failures to
// answer (Decision.Failed), which the hook's onFailure governs; an ordinary
// non-zero exit is not.
func TestRunnerMarksFailuresToAnswer(t *testing.T) {
	skipOnWindows(t)
	r := &Runner{Dir: t.TempDir()}
	ctx := context.Background()
	p := Payload{HookEventName: EventPreToolUse}
	if d, _, _ := r.Run(ctx, CommandHook{Command: "sleep 5", TimeoutSeconds: 1}, p, DecisionOptions{}); !d.Failed {
		t.Errorf("timeout: %+v, want Failed", d)
	}
	if d, _, _ := r.Run(ctx, CommandHook{Command: "kill -9 $$"}, p, DecisionOptions{}); !d.Failed || d.ErrKind != ErrExit {
		t.Errorf("killed: %+v, want Failed", d)
	}
	if d, _, _ := r.Run(ctx, CommandHook{Command: "exit 1"}, p, DecisionOptions{}); d.Failed {
		t.Errorf("exit 1: %+v, want an answer, not a failure", d)
	}
	bad := &Runner{Shell: "/nonexistent/shell-xyz", Dir: t.TempDir()}
	if d, _, _ := bad.Run(ctx, CommandHook{Command: "echo"}, p, DecisionOptions{}); !d.Failed {
		t.Errorf("spawn failure: %+v, want Failed", d)
	}
}

type failingDoer struct{}

func (failingDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestHTTPHookUnreachableIsFailure(t *testing.T) {
	r := &HTTPRunner{Doer: failingDoer{}}
	d, _, _ := r.Run(context.Background(), CommandHook{Type: HookTypeHTTP, URL: "https://x/y"}, Payload{}, DecisionOptions{})
	if !d.Failed || d.Behavior != BehaviorNone {
		t.Errorf("unreachable http hook = %+v, want a failure to answer", d)
	}
	// A configured-out seam is not the hook's failure.
	d, _, _ = (&HTTPRunner{}).Run(context.Background(), CommandHook{Type: HookTypeHTTP, URL: "https://x/y"}, Payload{}, DecisionOptions{})
	if d.Failed {
		t.Errorf("no http client = %+v, want no Failed", d)
	}
}
