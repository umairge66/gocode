package toolrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// fakePrompter is a scripted PermissionPrompter: it records every request and
// returns a response produced by respond. maxConcurrent tracks the greatest
// number of Prompt calls that overlapped, so a test can assert prompts are
// serialized.
type fakePrompter struct {
	respond func(req PermissionRequest) (PermissionResponse, error)

	mu       sync.Mutex
	requests []PermissionRequest

	inFlight      int32
	maxConcurrent int32
	delay         time.Duration
}

func (f *fakePrompter) Prompt(ctx context.Context, req PermissionRequest) (PermissionResponse, error) {
	n := atomic.AddInt32(&f.inFlight, 1)
	for {
		old := atomic.LoadInt32(&f.maxConcurrent)
		if n <= old || atomic.CompareAndSwapInt32(&f.maxConcurrent, old, n) {
			break
		}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	atomic.AddInt32(&f.inFlight, -1)

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return f.respond(req)
}

func (f *fakePrompter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func bashCall(t *testing.T, cmd string) agent.ToolUse {
	t.Helper()
	return agent.ToolUse{
		ID:    "tu_bash",
		Name:  string(protocol.ToolBash),
		Input: mustJSON(t, map[string]any{"command": cmd}),
	}
}

func TestPromptAllowOnceRunsTool(t *testing.T) {
	d := New(toolreg.ModeDefault, nil)
	d.SetPermissionPrompter(&fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedOnce}, nil
	}})

	res, err := d.Dispatch(context.Background(), bashCall(t, "/bin/echo hi"))
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if res.IsError {
		t.Fatalf("allow-once still errored: %q", res.Content)
	}
	if !strings.Contains(res.Content, "hi") {
		t.Errorf("Bash output = %q, want it to contain echoed text", res.Content)
	}
}

func TestPromptDenyWithFeedback(t *testing.T) {
	d := New(toolreg.ModeDefault, nil)
	d.SetPermissionPrompter(&fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionDenied, Feedback: "use the Read tool instead"}, nil
	}})

	res, err := d.Dispatch(context.Background(), bashCall(t, "rm -rf /"))
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !res.IsError {
		t.Fatal("deny did not produce an is_error result")
	}
	if !strings.Contains(res.Content, "use the Read tool instead") {
		t.Errorf("deny result = %q, want the user feedback appended", res.Content)
	}
	if !strings.Contains(res.Content, "rejected") {
		t.Errorf("deny result = %q, want the rejection sentence", res.Content)
	}
}

func TestPromptAllowAlwaysPersistsRuleAndSkipsReprompt(t *testing.T) {
	rule := &toolreg.Rule{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "/bin/echo:*"}
	fp := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedAlways, Rule: rule}, nil
	}}
	d := New(toolreg.ModeDefault, nil)
	d.SetPermissionPrompter(fp)

	// Bash command content is the full command; the rule "/bin/echo:*" is the Bash
	// prefix grammar (word-boundary prefix "echo").
	res, err := d.Dispatch(context.Background(), bashCall(t, "/bin/echo one"))
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if res.IsError {
		t.Fatalf("allow-always errored: %q", res.Content)
	}

	// A second matching call must be allowed WITHOUT prompting again.
	res2, err := d.Dispatch(context.Background(), bashCall(t, "/bin/echo two"))
	if err != nil {
		t.Fatalf("second Dispatch error: %v", err)
	}
	if res2.IsError {
		t.Fatalf("second call errored: %q", res2.Content)
	}
	if fp.count() != 1 {
		t.Errorf("prompter called %d times, want 1 (second call allowed by the persisted rule)", fp.count())
	}
	// The rule is in the live set.
	if d.SnapshotRules().Match(toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "/bin/echo three"}, toolreg.RuleAllow) == nil {
		t.Error("allow rule not present in the live rule set")
	}
}

func TestPromptAllowEditsSessionSwitchesMode(t *testing.T) {
	dir := t.TempDir()
	var switched toolreg.Mode
	d := New(toolreg.ModeDefault, nil)
	d.SetModeSwitch(func(m toolreg.Mode) { switched = m })
	d.SetPermissionPrompter(&fakePrompter{respond: func(req PermissionRequest) (PermissionResponse, error) {
		if !req.IsEdit {
			t.Errorf("request IsEdit = false for %s, want true", req.ToolName)
		}
		return PermissionResponse{Kind: PermissionAllowedEditsSession}, nil
	}})

	file := filepath.Join(dir, "a.txt")
	res, err := d.Dispatch(context.Background(), agent.ToolUse{
		ID:    "tu_w",
		Name:  string(protocol.ToolWrite),
		Input: mustJSON(t, map[string]any{"file_path": file, "content": "x"}),
	})
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if res.IsError {
		t.Fatalf("allow-edits errored: %q", res.Content)
	}
	if d.CurrentMode() != toolreg.ModeAcceptEdits {
		t.Errorf("mode = %q, want acceptEdits", d.CurrentMode())
	}
	if switched != toolreg.ModeAcceptEdits {
		t.Errorf("OnModeSwitch got %q, want acceptEdits", switched)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("Write did not create the file: %v", err)
	}

	// A subsequent edit is now auto-allowed (acceptEdits), no second prompt.
	file2 := filepath.Join(dir, "b.txt")
	res2, err := d.Dispatch(context.Background(), agent.ToolUse{
		ID:    "tu_w2",
		Name:  string(protocol.ToolWrite),
		Input: mustJSON(t, map[string]any{"file_path": file2, "content": "y"}),
	})
	if err != nil || res2.IsError {
		t.Fatalf("second write under acceptEdits failed: err=%v content=%q", err, res2.Content)
	}
}

func TestNoPrompterDenialMessage(t *testing.T) {
	d := New(toolreg.ModeDefault, nil) // no prompter: headless / -p
	res, err := d.Dispatch(context.Background(), bashCall(t, "/bin/ls"))
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !res.IsError {
		t.Fatal("ask with no prompter did not deny")
	}
	// The message must tell the user how to allow it.
	for _, want := range []string{"Bash", "permission"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("no-prompter denial = %q, want it to mention %q", res.Content, want)
		}
	}
}

func TestPromptsAreSerialized(t *testing.T) {
	fp := &fakePrompter{
		delay: 2 * time.Millisecond,
		respond: func(PermissionRequest) (PermissionResponse, error) {
			return PermissionResponse{Kind: PermissionAllowedOnce}, nil
		},
	}
	d := New(toolreg.ModeDefault, nil)
	d.SetPermissionPrompter(fp)

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Bash still prompts in default mode (Read/Glob/Grep now auto-allow as
			// safe local reads, so they would not exercise the prompt path).
			_, _ = d.Dispatch(context.Background(), agent.ToolUse{
				ID:    "tu",
				Name:  string(protocol.ToolBash),
				Input: json.RawMessage(`{"command":"/bin/echo hi"}`),
			})
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&fp.maxConcurrent); got != 1 {
		t.Errorf("max concurrent prompts = %d, want 1 (prompts must be serialized)", got)
	}
	if fp.count() != n {
		t.Errorf("prompter called %d times, want %d", fp.count(), n)
	}
}

// TestPlanModeHoldsForApprovalInteractive: a non-read-only tool in plan mode is
// HELD for approval (behavior:"ask") rather than hard-denied, so the
// interactive prompt is shown; approving it runs the tool. The prompt's reason
// carries the plan-mode message.
func TestPlanModeHoldsForApprovalInteractive(t *testing.T) {
	d := New(toolreg.ModePlan, nil)
	var sawReason string
	d.SetPermissionPrompter(&fakePrompter{respond: func(req PermissionRequest) (PermissionResponse, error) {
		sawReason = req.Reason
		return PermissionResponse{Kind: PermissionAllowedOnce}, nil
	}})

	res, err := d.Dispatch(context.Background(), bashCall(t, "/bin/echo hi"))
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if res.IsError {
		t.Fatalf("plan-mode approve still errored: %q", res.Content)
	}
	if !strings.Contains(res.Content, "hi") {
		t.Errorf("approved tool did not run: %q", res.Content)
	}
	if !strings.Contains(sawReason, "plan mode") {
		t.Errorf("prompt reason = %q, want it to mention plan mode", sawReason)
	}
}

// TestPlanModeNoPrompterDenies: in -p / headless plan mode (no prompter) a held
// non-read-only tool resolves to a safe denial with a plan-specific message and
// is NOT executed.
func TestPlanModeNoPrompterDenies(t *testing.T) {
	d := New(toolreg.ModePlan, nil) // no prompter: -p / headless

	res, err := d.Dispatch(context.Background(), bashCall(t, "/bin/echo PLANMARKER"))
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !res.IsError {
		t.Fatal("plan mode with no prompter did not deny the non-read-only Bash")
	}
	for _, want := range []string{"plan mode", "Bash", "needs approval"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("plan deny = %q, want it to mention %q", res.Content, want)
		}
	}
	if strings.Contains(res.Content, "PLANMARKER") {
		t.Errorf("held tool must not run in -p: %q", res.Content)
	}
}
