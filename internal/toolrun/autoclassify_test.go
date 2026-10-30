package toolrun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/automode"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
)

// countingClassifier answers with a fixed result and records each request.
type countingClassifier struct {
	mu   sync.Mutex
	res  automode.Result
	reqs []automode.Request
}

func (c *countingClassifier) Classify(_ context.Context, req automode.Request) automode.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	return c.res
}

func (c *countingClassifier) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// autoFixture: a project dir (the working directory) and an outside dir. A
// Write outside the working directory is what reaches the classifier.
func autoFixture(t *testing.T) (proj, outside string) {
	t.Helper()
	root := t.TempDir()
	proj, outside = filepath.Join(root, "proj"), filepath.Join(root, "outside")
	for _, d := range []string{proj, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return proj, outside
}

func writeInput(path string) map[string]any {
	return map[string]any{"file_path": path, "content": "hello"}
}

func TestAutoModeClassifierAllowRuns(t *testing.T) {
	proj, outside := autoFixture(t)
	c := &countingClassifier{res: automode.Result{Verdict: automode.VerdictAllow}}
	d := New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c))
	target := filepath.Join(outside, "f.txt")
	if res := dispatch(t, d, string(protocol.ToolWrite), writeInput(target)); res.IsError {
		t.Fatalf("allowed write failed: %+v", res)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("file not written")
	}
	if c.calls() != 1 || c.reqs[0].Action.Tool != "Write" || c.reqs[0].Action.ID != "tu_Write" {
		t.Fatalf("classifier requests: %+v", c.reqs)
	}
}

func TestAutoModeClassifierBlockDenies(t *testing.T) {
	proj, outside := autoFixture(t)
	c := &countingClassifier{res: automode.Result{Verdict: automode.VerdictBlock, Reason: "[Data Exfiltration] writes outside"}}
	p := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedOnce}, nil
	}}
	d := New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c), WithPermissionPrompter(p))
	target := filepath.Join(outside, "f.txt")
	res := dispatch(t, d, string(protocol.ToolWrite), writeInput(target))
	if !res.IsError || !strings.Contains(res.Content, "denied by the gocode auto mode classifier. Reason: [Data Exfiltration] writes outside") {
		t.Fatalf("block result: %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("blocked write ran")
	}
	if len(p.requests) != 0 {
		t.Fatal("a block below the denial limit must not prompt")
	}
	// The third consecutive block goes to the user instead.
	dispatch(t, d, string(protocol.ToolWrite), writeInput(target))
	if len(p.requests) != 0 {
		t.Fatal("second block prompted")
	}
	if res := dispatch(t, d, string(protocol.ToolWrite), writeInput(target)); res.IsError {
		t.Fatalf("denial-limit fallback: user approved, got %+v", res)
	}
	if len(p.requests) != 1 || !strings.Contains(p.requests[0].Notice, "3 consecutive actions were blocked") {
		t.Fatalf("denial-limit prompt: %+v", p.requests)
	}
}

func TestAutoModeClassifierUnavailableAsksOrDenies(t *testing.T) {
	proj, outside := autoFixture(t)
	target := filepath.Join(outside, "f.txt")
	c := &countingClassifier{res: automode.Result{Verdict: automode.VerdictUnavailable, Reason: "Classifier unavailable - blocking for safety"}}

	// Non-interactive: denied, never run.
	d := New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c))
	res := dispatch(t, d, string(protocol.ToolWrite), writeInput(target))
	if !res.IsError || !strings.Contains(res.Content, "temporarily unavailable") {
		t.Fatalf("headless unavailable: %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("ran without a verdict")
	}

	// Interactive: the user is asked; their "No" stands.
	p := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionDenied}, nil
	}}
	d = New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c), WithPermissionPrompter(p))
	if res := dispatch(t, d, string(protocol.ToolWrite), writeInput(target)); !res.IsError {
		t.Fatalf("user said no, got %+v", res)
	}
	if len(p.requests) != 1 || !strings.Contains(p.requests[0].Notice, "could not review") {
		t.Fatalf("prompt: %+v", p.requests)
	}

	// No classifier wired at all: same as unavailable (never a silent allow).
	d = New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, string(protocol.ToolWrite), writeInput(target)); !res.IsError {
		t.Fatalf("no classifier: %+v", res)
	}
}

func TestAutoModeDenyRuleBeatsClassifier(t *testing.T) {
	proj, outside := autoFixture(t)
	c := &countingClassifier{res: automode.Result{Verdict: automode.VerdictAllow}}
	rules := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Write", Content: "//" + strings.TrimPrefix(outside, "/") + "/**"}}
	d := New(toolreg.ModeAuto, rules, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c))
	if res := dispatch(t, d, string(protocol.ToolWrite), writeInput(filepath.Join(outside, "f.txt"))); !res.IsError {
		t.Fatalf("deny rule ignored: %+v", res)
	}
	if c.calls() != 0 {
		t.Fatal("classifier consulted despite a deny rule")
	}
}

func TestAutoModeSkipsClassifierForSafeCalls(t *testing.T) {
	proj, outside := autoFixture(t)
	if err := os.WriteFile(filepath.Join(outside, "o.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &countingClassifier{res: automode.Result{Verdict: automode.VerdictBlock, Reason: "[X] no"}}
	d := New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c))
	// Read-only tools, inside and outside the working directories.
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": filepath.Join(outside, "o.txt")}); res.IsError {
		t.Fatalf("outside Read: %+v", res)
	}
	if res := dispatch(t, d, string(protocol.ToolGlob), map[string]any{"pattern": "*", "path": outside}); res.IsError {
		t.Fatalf("outside Glob: %+v", res)
	}
	// An edit inside the working directory (acceptEdits fast path).
	if res := dispatch(t, d, string(protocol.ToolWrite), writeInput(filepath.Join(proj, "in.txt"))); res.IsError {
		t.Fatalf("inside Write: %+v", res)
	}
	if c.calls() != 0 {
		t.Fatalf("classifier consulted %d times for safe calls", c.calls())
	}
}

func TestNonAutoModesNeverClassify(t *testing.T) {
	proj, outside := autoFixture(t)
	c := &countingClassifier{res: automode.Result{Verdict: automode.VerdictAllow}}
	for _, m := range []toolreg.Mode{toolreg.ModeDefault, toolreg.ModeAcceptEdits, toolreg.ModePlan, toolreg.ModeDontAsk, toolreg.ModeBypassPermissions} {
		d := New(m, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c))
		dispatch(t, d, string(protocol.ToolWrite), writeInput(filepath.Join(outside, "f.txt")))
	}
	if c.calls() != 0 {
		t.Fatalf("classifier called %d times outside auto mode", c.calls())
	}
}

// End to end through automode.Classifier and a 127.0.0.1 fake provider.
func TestAutoModeClassifierEndToEnd(t *testing.T) {
	proj, outside := autoFixture(t)
	verdict := "<block>no</block>"
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		v := verdict
		mu.Unlock()
		b, _ := json.Marshal(map[string]any{
			"model":   "m",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": v}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	mc := provider.NewModelCaller(&provider.OpenAI{APIKey: "k", BaseURL: srv.URL}, "m")
	mc.MaxRetries = 0
	c := &automode.Classifier{Caller: mc, ParseRetries: -1}
	d := New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c))

	ctx := agent.WithTranscript(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("write f.txt"))})
	run := func(name string) agent.ToolResult {
		in, _ := json.Marshal(writeInput(filepath.Join(outside, name)))
		res, err := d.Dispatch(ctx, agent.ToolUse{ID: "tu_1", Name: "Write", Input: in})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("a.txt"); res.IsError {
		t.Fatalf("allow: %+v", res)
	}
	mu.Lock()
	verdict = "<block>yes</block><reason>[Test Rule] blocked</reason>"
	mu.Unlock()
	if res := run("b.txt"); !res.IsError || !strings.Contains(res.Content, "[Test Rule] blocked") {
		t.Fatalf("block: %+v", res)
	}
	mu.Lock()
	verdict = "garbage"
	mu.Unlock()
	if res := run("c.txt"); !res.IsError || !strings.Contains(res.Content, "not a judgment") {
		t.Fatalf("garbage: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(outside, "b.txt")); err == nil {
		t.Fatal("blocked write ran")
	}
}
