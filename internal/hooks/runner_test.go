package hooks

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook tests")
	}
}

// A trivial "cat" hook echoes its stdin straight to stdout. Because the payload
// is valid JSON starting with "{", the round-tripped stdout parses back as a
// HookOutput — here we assert the payload reached the hook intact and that a
// hand-built decision document round-trips to the expected Decision.
func TestRunnerStdinRoundTrip(t *testing.T) {
	skipOnWindows(t)
	r := &Runner{Dir: t.TempDir(), DefaultTimeout: 10 * time.Second}
	hook := CommandHook{Type: HookTypeCommand, Command: "cat"}
	payload := Payload{
		HookEventName:  EventPreToolUse,
		SessionID:      "sess-123",
		TranscriptPath: "/tmp/t.jsonl",
		Cwd:            "/work",
		ToolName:       "Bash",
		ToolInput:      json.RawMessage(`{"command":"ls"}`),
	}
	_, res, err := r.Run(context.Background(), hook, payload, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if res.SpawnFailed || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	var got Payload
	if err := json.Unmarshal([]byte(res.Stdout), &got); err != nil {
		t.Fatalf("stdin did not round-trip as JSON: %v\nstdout=%q", err, res.Stdout)
	}
	if got.HookEventName != EventPreToolUse || got.SessionID != "sess-123" || got.ToolName != "Bash" {
		t.Errorf("payload corrupted on round-trip: %+v", got)
	}
}

// A hook that emits a decision JSON on stdout and exits 0 yields that decision.
func TestRunnerEchoDecision(t *testing.T) {
	skipOnWindows(t)
	r := &Runner{Dir: t.TempDir()}
	hook := CommandHook{
		Type:    HookTypeCommand,
		Command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"forbidden"}}'`,
	}
	d, res, err := r.Run(context.Background(), hook, Payload{HookEventName: EventPreToolUse}, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %+v", res)
	}
	if d.Behavior != BehaviorDeny || d.Reason != "forbidden" {
		t.Errorf("decision = %+v, want deny/forbidden", d)
	}
}

// Exit 2 blocks with stderr as the feedback.
func TestRunnerExit2Blocks(t *testing.T) {
	skipOnWindows(t)
	r := &Runner{Dir: t.TempDir()}
	hook := CommandHook{Type: HookTypeCommand, Command: `echo "denied by policy" >&2; exit 2`}
	d, res, err := r.Run(context.Background(), hook, Payload{HookEventName: EventPreToolUse}, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if res.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %+v", res)
	}
	if d.Behavior != BehaviorDeny || d.Reason != "denied by policy" {
		t.Errorf("decision = %+v, want deny with stderr reason", d)
	}
}

// Other non-zero exit is a non-blocking error; the action proceeds.
func TestRunnerExit1NonBlocking(t *testing.T) {
	skipOnWindows(t)
	r := &Runner{Dir: t.TempDir()}
	hook := CommandHook{Type: HookTypeCommand, Command: `echo "oops" >&2; exit 1`}
	d, _, err := r.Run(context.Background(), hook, Payload{HookEventName: EventPreToolUse}, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if d.Behavior != BehaviorNone || !d.Continue || d.ErrKind != ErrExit {
		t.Errorf("decision = %+v, want non-blocking ErrExit", d)
	}
}

// A hook that overruns its timeout is reported as a timeout, non-blocking,
// and promptly: the shell's own child (which holds the output pipes open) is
// terminated with it rather than left running until it exits by itself.
func TestRunnerTimeout(t *testing.T) {
	skipOnWindows(t)
	r := &Runner{Dir: t.TempDir()}
	hook := CommandHook{Type: HookTypeCommand, Command: "sleep 5; echo after", TimeoutSeconds: 1}
	start := time.Now()
	d, res, err := r.Run(context.Background(), hook, Payload{HookEventName: EventStop}, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !res.TimedOut || d.ErrKind != ErrTimeout {
		t.Errorf("decision = %+v res=%+v, want timeout", d, res)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("timeout was not enforced promptly (%v)", time.Since(start))
	}
}

// A command that cannot be started surfaces as a spawn failure (non-blocking).
func TestRunnerSpawnFailure(t *testing.T) {
	skipOnWindows(t)
	// Point the runner at a shell that does not exist.
	r := &Runner{Shell: "/nonexistent/shell-xyz", Dir: t.TempDir()}
	hook := CommandHook{Type: HookTypeCommand, Command: "echo hi"}
	d, res, err := r.Run(context.Background(), hook, Payload{HookEventName: EventPreToolUse}, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !res.SpawnFailed || d.ErrKind != ErrSpawnFailed {
		t.Errorf("decision = %+v res=%+v, want spawn failure", d, res)
	}
}
