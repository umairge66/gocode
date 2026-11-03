package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/sessionstore"
)

// deferHookFixture is a print-mode fixture whose project PreToolUse hook on
// Bash defers every call until the file approve exists, then allows it.
func deferHookFixture(t *testing.T, p *fakeProvider) (f *subcmdFixture, approve string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell command")
	}
	f = headlessFixture(t, p)
	approve = filepath.Join(f.cwd, "approve")
	decision := func(d string) string {
		return `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"` + d + `"}}`
	}
	cmd := "if [ -f '" + approve + "' ]; then echo '" + decision("allow") + "'; else echo '" + decision("defer") + "'; fi"
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{
		map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": cmd}}},
	}}})
	subcmdWriteFile(t, f.localSettingsPath(), string(settings))
	return f, approve
}

func printResult(t *testing.T, out string) map[string]any {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	return r
}

// TestPrintDeferredToolCallRoundTrip: a hook's defer ends the -p turn with
// the call unrun and reported; a resume with no prompt asks the hook again
// and, once it allows the call, runs it and continues the conversation.
func TestPrintDeferredToolCallRoundTrip(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Bash", in: 1, out: 1},
		fakeTurn{text: "all done", in: 1, out: 1},
	)
	f, approve := deferHookFixture(t, p)
	marker := filepath.Join(f.cwd, "ran.txt")
	p.turns[0].toolArgs = `{"command":"echo ran > '` + marker + `'"}`

	code, out, errOut := f.run(t, "-p", "--output-format", "json", "write the marker")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	r := printResult(t, out)
	if r["subtype"] != "success" || r["is_error"] != false || r["stop_reason"] != "tool_deferred" || r["terminal_reason"] != "tool_deferred" {
		t.Fatalf("deferred result = %v", r)
	}
	dtu, _ := r["deferred_tool_use"].(map[string]any)
	if dtu["id"] != "call_gen-0" || dtu["name"] != "Bash" {
		t.Fatalf("deferred_tool_use = %v", r["deferred_tool_use"])
	}
	if in, _ := dtu["input"].(map[string]any); !strings.Contains(in["command"].(string), "ran.txt") {
		t.Fatalf("deferred_tool_use input = %v", dtu["input"])
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the deferred call ran")
	}
	if p.callCount() != 1 {
		t.Fatalf("model called %d times, want 1", p.callCount())
	}
	sid := r["session_id"].(string)
	store := sessionstore.New(filepath.Join(f.home, ".config", "gocode"))
	msgs, _ := store.Load(f.cwd, sid)
	if call, ok := store.PendingDeferred(f.cwd, sid, msgs); !ok || call.ID != "call_gen-0" {
		t.Fatalf("transcript does not hold the deferred call: %v %v", call, ok)
	}

	// Still not approved: a prompt-less resume defers it again, with no
	// model request.
	code, out, errOut = f.run(t, "-p", "--output-format", "json", "--resume", sid)
	if r = printResult(t, out); code != 0 || r["stop_reason"] != "tool_deferred" || p.callCount() != 1 {
		t.Fatalf("re-deferred resume: exit %d err %q result %v calls %d", code, errOut, r, p.callCount())
	}

	// Approved: the resume runs the call, and the model answers its result.
	subcmdWriteFile(t, approve, "")
	code, out, errOut = f.run(t, "-p", "--output-format", "json", "--resume", sid)
	if code != 0 {
		t.Fatalf("resume exit %d err %q", code, errOut)
	}
	r = printResult(t, out)
	if r["result"] != "all done" || r["terminal_reason"] != "completed" || r["deferred_tool_use"] != nil {
		t.Fatalf("resumed result = %v", r)
	}
	if b, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(b)) != "ran" {
		t.Fatalf("the resumed call did not run: %q %v", b, err)
	}
	p.mu.Lock()
	body := p.bodies[len(p.bodies)-1]
	p.mu.Unlock()
	if !strings.Contains(body, `"tool_call_id":"call_gen-0"`) {
		t.Fatalf("the resumed request does not answer the deferred call:\n%s", body)
	}
	msgs, _ = store.Load(f.cwd, sid)
	if _, ok := store.PendingDeferred(f.cwd, sid, msgs); ok {
		t.Fatal("the call is still pending after it ran")
	}

	// Nothing is pending now: a resume without a prompt is refused.
	code, _, errOut = f.run(t, "-p", "--resume", sid)
	if code != 1 || !strings.Contains(errOut, "no deferred tool call") {
		t.Fatalf("prompt-less resume with nothing deferred: exit %d err %q", code, errOut)
	}
}

// TestPrintDeferredCallSettledByPrompt: a resume with a prompt settles the
// deferred call first — a hook that still defers it now gets an ask, which
// print mode refuses — and the prompt follows its result in the same turn.
func TestPrintDeferredCallSettledByPrompt(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Bash", toolArgs: `{"command":"echo hi"}`, in: 1, out: 1},
		fakeTurn{text: "ok then", in: 1, out: 1},
	)
	f, _ := deferHookFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "say hi")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	sid := printResult(t, out)["session_id"].(string)

	code, out, errOut = f.run(t, "-p", "--output-format", "json", "--resume", sid, "never mind")
	if code != 0 {
		t.Fatalf("resume exit %d err %q", code, errOut)
	}
	r := printResult(t, out)
	if r["result"] != "ok then" || r["stop_reason"] == "tool_deferred" {
		t.Fatalf("resumed result = %v", r)
	}
	denials, _ := r["permission_denials"].([]any)
	if len(denials) != 1 {
		t.Fatalf("permission_denials = %v, want the deferred Bash call", r["permission_denials"])
	}
	p.mu.Lock()
	body := p.bodies[len(p.bodies)-1]
	p.mu.Unlock()
	if !strings.Contains(body, `"tool_call_id":"call_gen-0"`) || !strings.Contains(body, "never mind") {
		t.Fatalf("the resumed request lacks the deferred call's result or the prompt:\n%s", body)
	}
}
