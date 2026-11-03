package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	if round, ok := store.PendingDeferred(f.cwd, sid, msgs); !ok || round.First().ID != "call_gen-0" {
		t.Fatalf("transcript does not hold the deferred call: %v %v", round, ok)
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

// roundCall is one tool call of a scripted round.
type roundCall struct{ id, name, args string }

// newRoundProvider is a fake provider whose first response is one round of
// several tool calls (*calls, read under p.mu) and every later one is the text
// "all done".
func newRoundProvider(t *testing.T, calls *[]roundCall) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		p.mu.Lock()
		p.bodies = append(p.bodies, string(body))
		first := p.calls == 0
		p.calls++
		round := *calls
		p.mu.Unlock()
		usage := map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		var toolCalls []any
		if first {
			for i, c := range round {
				toolCalls = append(toolCalls, map[string]any{"index": i, "id": c.id, "type": "function",
					"function": map[string]any{"name": c.name, "arguments": c.args}})
			}
		}
		msg := map[string]any{"role": "assistant", "content": "all done"}
		finish := "stop"
		if first {
			msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
			finish = "tool_calls"
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			send := func(v any) {
				b, _ := json.Marshal(v)
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			delta := map[string]any{"content": "all done"}
			if first {
				delta = map[string]any{"tool_calls": toolCalls}
			}
			send(map[string]any{"id": "gen", "model": "gocode-sonnet-4-5", "choices": []any{map[string]any{"index": 0, "delta": delta}}})
			send(map[string]any{"id": "gen", "model": "gocode-sonnet-4-5", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage})
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gen", "model": "gocode-sonnet-4-5",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   usage,
		})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// lastBody is the provider's last request body.
func lastBody(p *fakeProvider) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bodies[len(p.bodies)-1]
}

// toolResultOrder reports whether body answers every id, in that order.
func toolResultOrder(body string, ids ...string) bool {
	at := -1
	for _, id := range ids {
		i := strings.Index(body, `"tool_call_id":"`+id+`"`)
		if i <= at {
			return false
		}
		at = i
	}
	return true
}

// TestPrintDeferredAmongSeveralCalls: a hook defers a Bash call that shares
// its round with a Read. The Read runs, the turn ends tool_deferred with the
// Bash call reported, and the resume runs it and sends both results, in
// tool-call order, in one request.
func TestPrintDeferredAmongSeveralCalls(t *testing.T) {
	var calls []roundCall
	p := newRoundProvider(t, &calls)
	f, approve := deferHookFixture(t, p)
	marker, notes := filepath.Join(f.cwd, "ran.txt"), filepath.Join(f.cwd, "notes.txt")
	subcmdWriteFile(t, notes, "the notes\n")
	p.mu.Lock()
	calls = []roundCall{
		{"call_a", "Bash", `{"command":"echo ran > '` + marker + `'"}`},
		{"call_b", "Read", `{"file_path":"` + notes + `"}`},
	}
	p.mu.Unlock()

	code, out, errOut := f.run(t, "-p", "--output-format", "json", "write the marker")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	r := printResult(t, out)
	if r["stop_reason"] != "tool_deferred" || r["is_error"] != false {
		t.Fatalf("deferred result = %v", r)
	}
	if dtu, _ := r["deferred_tool_use"].(map[string]any); dtu["id"] != "call_a" {
		t.Fatalf("deferred_tool_use = %v", r["deferred_tool_use"])
	}
	if r["deferred_tool_uses"] != nil {
		t.Fatalf("deferred_tool_uses with one deferred call = %v", r["deferred_tool_uses"])
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the deferred call ran")
	}
	sid := r["session_id"].(string)
	store := sessionstore.New(filepath.Join(f.home, ".config", "gocode"))
	msgs, _ := store.Load(f.cwd, sid)
	round, ok := store.PendingDeferred(f.cwd, sid, msgs)
	if !ok || len(round.Calls) != 1 || len(round.Results) != 1 || !strings.Contains(round.Results[0].Content, "the notes") {
		t.Fatalf("transcript round = %+v %v", round, ok)
	}

	subcmdWriteFile(t, approve, "")
	code, out, errOut = f.run(t, "-p", "--output-format", "json", "--resume", sid)
	if code != 0 {
		t.Fatalf("resume exit %d err %q", code, errOut)
	}
	if r = printResult(t, out); r["result"] != "all done" || r["deferred_tool_use"] != nil {
		t.Fatalf("resumed result = %v", r)
	}
	if b, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(b)) != "ran" {
		t.Fatalf("the resumed call did not run: %q %v", b, err)
	}
	if p.callCount() != 2 {
		t.Fatalf("model called %d times, want 2", p.callCount())
	}
	body := lastBody(p)
	if !toolResultOrder(body, "call_a", "call_b") || !strings.Contains(body, "the notes") {
		t.Fatalf("the resumed request does not answer the round in order:\n%s", body)
	}
	msgs, _ = store.Load(f.cwd, sid)
	if _, ok := store.PendingDeferred(f.cwd, sid, msgs); ok {
		t.Fatal("the round is still pending after it ran")
	}
}

// TestPrintSeveralDeferredCalls: two deferred calls of one round are both
// reported (deferred_tool_uses), stay deferred together on a resume the hook
// still defers, and both run on the resume it allows.
func TestPrintSeveralDeferredCalls(t *testing.T) {
	var calls []roundCall
	p := newRoundProvider(t, &calls)
	f, approve := deferHookFixture(t, p)
	p.mu.Lock()
	calls = []roundCall{
		{"call_a", "Bash", `{"command":"echo a > '` + filepath.Join(f.cwd, "a.txt") + `'"}`},
		{"call_b", "Bash", `{"command":"echo b > '` + filepath.Join(f.cwd, "b.txt") + `'"}`},
	}
	p.mu.Unlock()

	code, out, errOut := f.run(t, "-p", "--output-format", "json", "write both")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	r := printResult(t, out)
	all, _ := r["deferred_tool_uses"].([]any)
	if r["stop_reason"] != "tool_deferred" || len(all) != 2 {
		t.Fatalf("deferred result = %v", r)
	}
	if a, _ := all[0].(map[string]any); a["id"] != "call_a" {
		t.Fatalf("deferred_tool_uses = %v", all)
	}
	if b, _ := all[1].(map[string]any); b["id"] != "call_b" || b["name"] != "Bash" {
		t.Fatalf("deferred_tool_uses = %v", all)
	}
	if dtu, _ := r["deferred_tool_use"].(map[string]any); dtu["id"] != "call_a" {
		t.Fatalf("deferred_tool_use = %v", r["deferred_tool_use"])
	}
	sid := r["session_id"].(string)

	code, out, _ = f.run(t, "-p", "--output-format", "json", "--resume", sid)
	if r = printResult(t, out); code != 0 || r["stop_reason"] != "tool_deferred" || p.callCount() != 1 {
		t.Fatalf("re-deferred resume: exit %d result %v calls %d", code, r, p.callCount())
	}
	if all, _ := r["deferred_tool_uses"].([]any); len(all) != 2 {
		t.Fatalf("re-deferred deferred_tool_uses = %v", r["deferred_tool_uses"])
	}

	subcmdWriteFile(t, approve, "")
	code, out, errOut = f.run(t, "-p", "--output-format", "json", "--resume", sid)
	if code != 0 {
		t.Fatalf("resume exit %d err %q", code, errOut)
	}
	if r = printResult(t, out); r["result"] != "all done" {
		t.Fatalf("resumed result = %v", r)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(f.cwd, name)); err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
	}
	if !toolResultOrder(lastBody(p), "call_a", "call_b") {
		t.Fatalf("the resumed request does not answer both calls in order:\n%s", lastBody(p))
	}
}

// TestPrintCompactRefusedWhileDeferred: /compact on a session with a pending
// deferred call is refused with a message, and the call stays pending.
func TestPrintCompactRefusedWhileDeferred(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Bash", toolArgs: `{"command":"echo hi"}`, in: 1, out: 1},
		fakeTurn{text: "ok", in: 1, out: 1},
	)
	f, _ := deferHookFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "say hi")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	sid := printResult(t, out)["session_id"].(string)

	code, out, errOut = f.run(t, "-p", "--resume", sid, "/compact")
	if !strings.Contains(out+errOut, "Cannot compact yet") {
		t.Fatalf("/compact: exit %d out %q err %q", code, out, errOut)
	}
	store := sessionstore.New(filepath.Join(f.home, ".config", "gocode"))
	msgs, _ := store.Load(f.cwd, sid)
	if _, ok := store.PendingDeferred(f.cwd, sid, msgs); !ok {
		t.Fatal("the deferred call is no longer pending")
	}
	if p.callCount() != 1 {
		t.Fatalf("model called %d times, want 1", p.callCount())
	}
}
