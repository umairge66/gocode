package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/sessionstore"
)

// fakeTurn is one scripted model response of the fake OpenAI-compatible
// provider: either text, or a single tool call.
type fakeTurn struct {
	text     string
	toolName string
	toolArgs string
	in, out  int
	// cached is prompt_tokens_details.cached_tokens (part of in).
	cached int
	// model overrides the reported model id ("" = gocode-sonnet-4-5).
	model string
}

// fakeProvider is a 127.0.0.1 OpenRouter-compatible endpoint that answers
// with the scripted turns in order (the last one repeats).
type fakeProvider struct {
	srv   *httptest.Server
	mu    sync.Mutex
	turns []fakeTurn
	calls int
	tools [][]string // tool names advertised per request
	// bodies are the request bodies, in order.
	bodies []string
}

func newFakeProvider(t *testing.T, turns ...fakeTurn) *fakeProvider {
	t.Helper()
	p := &fakeProvider{turns: turns}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &req)
	p.mu.Lock()
	var names []string
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}
	p.tools = append(p.tools, names)
	p.bodies = append(p.bodies, string(body))
	i := p.calls
	p.calls++
	if i >= len(p.turns) {
		i = len(p.turns) - 1
	}
	turn := p.turns[i]
	p.mu.Unlock()

	usage := map[string]any{"prompt_tokens": turn.in, "completion_tokens": turn.out, "total_tokens": turn.in + turn.out}
	if turn.cached > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": turn.cached}
	}
	model := "gocode-sonnet-4-5"
	if turn.model != "" {
		model = turn.model
	}
	id := fmt.Sprintf("gen-%d", i)
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if turn.toolName != "" {
			send(map[string]any{"id": id, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "call_" + id, "type": "function", "function": map[string]any{"name": turn.toolName, "arguments": turn.toolArgs}}}}}}})
			send(map[string]any{"id": id, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": usage})
		} else {
			for _, part := range strings.SplitAfter(turn.text, " ") {
				send(map[string]any{"id": id, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": part}}}})
			}
			send(map[string]any{"id": id, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	msg := map[string]any{"role": "assistant", "content": turn.text}
	finish := "stop"
	if turn.toolName != "" {
		msg["content"] = nil
		msg["tool_calls"] = []any{map[string]any{"id": "call_" + id, "type": "function", "function": map[string]any{"name": turn.toolName, "arguments": turn.toolArgs}}}
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   usage,
	})
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// headlessFixture is an isolated HOME + project with the fake provider
// configured through the environment.
func headlessFixture(t *testing.T, p *fakeProvider) *subcmdFixture {
	t.Helper()
	f := newSubcmdFixture(t)
	f.vars["OPENROUTER_API_KEY"] = "sk-test-not-a-real-key"
	f.vars["OPENROUTER_BASE_URL"] = p.srv.URL
	f.vars["GOCODE_PROVIDER"] = "openrouter"
	f.vars["GOCODE_MAX_RETRIES"] = "0"
	f.stdinTTY = true
	subcmdWriteFile(t, f.userSettingsPath(), `{"model":"gocode-sonnet-4-5"}`)
	return f
}

func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON line %q: %v", l, err)
		}
		recs = append(recs, m)
	}
	return recs
}

func sessionFiles(t *testing.T, f *subcmdFixture) []string {
	t.Helper()
	dir := sessionstore.New(filepath.Join(f.home, ".config", "gocode")).ProjectDir(f.cwd)
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	return matches
}

func TestPrintJSONResultShape(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Read", toolArgs: "", in: 1000, out: 100},
		fakeTurn{text: "The file says hello.", in: 2000, out: 200},
	)
	f := headlessFixture(t, p)
	file := filepath.Join(f.cwd, "hello.txt")
	if err := os.WriteFile(file, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.turns[0].toolArgs = fmt.Sprintf(`{"file_path":%q}`, file)

	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--model", "gocode-sonnet-4-5", "read hello.txt")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, out %q", code, errOut, out)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("json output must be one line: %q", out)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	for _, k := range []string{"type", "subtype", "is_error", "duration_ms", "duration_api_ms", "num_turns", "result",
		"stop_reason", "session_id", "total_cost_usd", "usage", "modelUsage", "permission_denials", "uuid"} {
		if _, ok := r[k]; !ok {
			t.Errorf("result lacks %q: %v", k, r)
		}
	}
	if r["type"] != "result" || r["subtype"] != "success" || r["is_error"] != false {
		t.Fatalf("result = %v", r)
	}
	if r["result"] != "The file says hello." || r["stop_reason"] != "end_turn" {
		t.Fatalf("result text/stop = %v / %v", r["result"], r["stop_reason"])
	}
	if r["num_turns"].(float64) != 2 {
		t.Fatalf("num_turns = %v", r["num_turns"])
	}
	u := r["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 3000 || u["output_tokens"].(float64) != 300 ||
		u["cache_creation_input_tokens"].(float64) != 0 || u["cache_read_input_tokens"].(float64) != 0 {
		t.Fatalf("usage = %v", u)
	}
	// gocode-sonnet-4-5: $3 / $15 per million tokens.
	want := 3000*3/1e6 + 300*15/1e6
	if got := r["total_cost_usd"].(float64); math.Abs(got-want) > 1e-9 {
		t.Fatalf("total_cost_usd = %v, want %v", got, want)
	}
	mu := r["modelUsage"].(map[string]any)["gocode-sonnet-4-5"].(map[string]any)
	if mu["inputTokens"].(float64) != 3000 || math.Abs(mu["costUSD"].(float64)-want) > 1e-9 {
		t.Fatalf("modelUsage = %v", mu)
	}
	if d := r["permission_denials"].([]any); len(d) != 0 {
		t.Fatalf("denials = %v", d)
	}
	// The run was persisted under the reported session id.
	sid := r["session_id"].(string)
	files := sessionFiles(t, f)
	if len(files) != 1 || filepath.Base(files[0]) != sid+".jsonl" {
		t.Fatalf("transcripts = %v, session_id %s", files, sid)
	}
	if !strings.Contains(errOut, "") || strings.Contains(out, "sk-test") || strings.Contains(errOut, "sk-test") {
		t.Fatal("API key leaked into output")
	}
}

func TestPrintStreamJSONEvents(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Read", in: 10, out: 5},
		fakeTurn{text: "done reading", in: 20, out: 7},
	)
	f := headlessFixture(t, p)
	file := filepath.Join(f.cwd, "a.txt")
	_ = os.WriteFile(file, []byte("alpha\n"), 0o644)
	p.turns[0].toolArgs = fmt.Sprintf(`{"file_path":%q}`, file)

	// stream-json without --verbose is refused.
	code, _, errOut := f.run(t, "-p", "--output-format=stream-json", "hi")
	if code != 1 || !strings.Contains(errOut, "Error: When using --print, --output-format=stream-json requires --verbose") {
		t.Fatalf("exit %d err %q", code, errOut)
	}

	code, out, errOut := f.run(t, "-p", "--output-format", "stream-json", "--verbose", "--model", "gocode-sonnet-4-5", "read a.txt")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	recs := decodeLines(t, out)
	var kinds []string
	for _, r := range recs {
		k := r["type"].(string)
		if st, ok := r["subtype"].(string); ok && k == "system" {
			k += "/" + st
		}
		kinds = append(kinds, k)
	}
	if got := strings.Join(kinds, ","); got != "system/init,assistant,user,assistant,result" {
		t.Fatalf("event order = %s\n%s", got, out)
	}
	sid := recs[0]["session_id"].(string)
	init := recs[0]
	if init["model"] != "gocode-sonnet-4-5" || init["permissionMode"] != "default" || init["cwd"] != f.cwd {
		t.Fatalf("init = %v", init)
	}
	var hasRead bool
	for _, n := range init["tools"].([]any) {
		if n == "Read" {
			hasRead = true
		}
	}
	if !hasRead {
		t.Fatalf("init tools = %v", init["tools"])
	}
	for _, r := range recs {
		if r["session_id"] != sid {
			t.Fatalf("record with another session id: %v", r)
		}
	}
	a := recs[1]["message"].(map[string]any)
	blk := a["content"].([]any)[0].(map[string]any)
	if blk["type"] != "tool_use" || blk["name"] != "Read" || a["role"] != "assistant" || a["stop_reason"] != "tool_use" {
		t.Fatalf("assistant tool_use = %v", a)
	}
	if recs[1]["parent_tool_use_id"] != nil {
		t.Fatalf("parent_tool_use_id = %v", recs[1]["parent_tool_use_id"])
	}
	um := recs[2]["message"].(map[string]any)
	res := um["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != blk["id"] || !strings.Contains(fmt.Sprint(res["content"]), "alpha") {
		t.Fatalf("tool_result = %v", res)
	}
	last := recs[4]
	if last["subtype"] != "success" || last["result"] != "done reading" || last["num_turns"].(float64) != 2 {
		t.Fatalf("result = %v", last)
	}
	if u := last["usage"].(map[string]any); u["input_tokens"].(float64) != 30 || u["output_tokens"].(float64) != 12 {
		t.Fatalf("usage = %v", u)
	}
}

func TestPrintStreamJSONPartialMessages(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "one two three", in: 1, out: 3})
	f := headlessFixture(t, p)
	code, _, errOut := f.run(t, "-p", "--include-partial-messages", "hi")
	if code != 1 || !strings.Contains(errOut, "Error: --include-partial-messages requires --print and --output-format=stream-json.") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	code, out, errOut := f.run(t, "-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "hi")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	var text strings.Builder
	var events []string
	for _, r := range decodeLines(t, out) {
		if r["type"] != "stream_event" {
			continue
		}
		ev := r["event"].(map[string]any)
		events = append(events, ev["type"].(string))
		if d, ok := ev["delta"].(map[string]any); ok && d["type"] == "text_delta" {
			text.WriteString(d["text"].(string))
		}
	}
	if text.String() != "one two three" {
		t.Fatalf("deltas = %q (events %v)\n%s", text.String(), events, out)
	}
	if events[0] != "message_start" || events[len(events)-1] != "message_stop" {
		t.Fatalf("events = %v", events)
	}
}

func TestPrintJSONMaxTurns(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{toolName: "Glob", toolArgs: `{"pattern":"*.none"}`, in: 5, out: 5})
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--max-turns", "1", "loop forever")
	if code != 1 {
		t.Fatalf("exit %d, err %q out %q", code, errOut, out)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q", out)
	}
	if r["subtype"] != "error_max_turns" || r["is_error"] != true || r["num_turns"].(float64) != 2 {
		t.Fatalf("result = %v", r)
	}
	if _, ok := r["result"]; ok {
		t.Fatalf("error result carries result text: %v", r)
	}
	errs := r["errors"].([]any)
	if len(errs) != 1 || errs[0] != "Reached maximum number of turns (1)" {
		t.Fatalf("errors = %v", errs)
	}
	if p.callCount() != 1 {
		t.Fatalf("model called %d times", p.callCount())
	}
}

func TestPrintJSONPermissionDenials(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Write", in: 1, out: 1},
		fakeTurn{text: "could not write", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	target := filepath.Join(f.cwd, "out.txt")
	p.turns[0].toolArgs = fmt.Sprintf(`{"file_path":%q,"content":"x"}`, target)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "write it")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	d := r["permission_denials"].([]any)
	if len(d) != 1 {
		t.Fatalf("denials = %v", r)
	}
	den := d[0].(map[string]any)
	if den["tool_name"] != "Write" || den["tool_use_id"] != "call_gen-0" || den["tool_input"].(map[string]any)["file_path"] != target {
		t.Fatalf("denial = %v", den)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("denied write happened")
	}
}

func TestPrintJSONVerboseArray(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "hi there", in: 1, out: 1})
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--verbose", "hello")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("not an array: %q", out)
	}
	if len(arr) != 3 || arr[0]["type"] != "system" || arr[1]["type"] != "assistant" || arr[2]["type"] != "result" {
		t.Fatalf("records = %v", arr)
	}
}

func TestPrintTextUnchanged(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "plain answer", in: 1, out: 1})
	f := headlessFixture(t, p)
	for _, args := range [][]string{{"-p", "q"}, {"-p", "--output-format", "text", "q"}} {
		code, out, errOut := f.run(t, args...)
		if code != 0 || out != "plain answer\n" {
			t.Fatalf("%v: exit %d out %q err %q", args, code, out, errOut)
		}
	}
}

func TestPrintInvalidFormats(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "x"})
	f := headlessFixture(t, p)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-p", "--output-format", "xml", "q"}, "error: option '--output-format <format>' argument 'xml' is invalid. Allowed choices are text, json, stream-json."},
		{[]string{"-p", "--input-format=yaml", "q"}, "error: option '--input-format <format>' argument 'yaml' is invalid. Allowed choices are text, stream-json."},
		{[]string{"-p", "--input-format", "stream-json", "q"}, "Error: --input-format=stream-json requires output-format=stream-json."},
		{[]string{"-p", "--output-format", "json", "--json-schema", "{nope", "q"}, "Error: --json-schema is not valid JSON:"},
		{[]string{"-p", "--output-format", "json", "--json-schema", "[1]", "q"}, "Error: --json-schema must be a JSON object"},
		{[]string{"-p", "--session-id", "not-a-uuid", "q"}, "Error: Invalid session ID. Must be a valid UUID."},
		{[]string{"-p", "--continue", "--session-id", "6f1c2a52-8a39-4c47-9c39-1f0c5b0e8a11", "q"}, "Error: --session-id can only be used with --continue or --resume if --fork-session is also specified."},
	}
	for _, c := range cases {
		code, out, errOut := f.run(t, c.args...)
		if code != 1 || !strings.Contains(errOut, c.want) || out != "" {
			t.Errorf("%v: exit %d out %q err %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
	if p.callCount() != 0 {
		t.Fatalf("model called %d times on invalid flags", p.callCount())
	}
}

func TestPrintSessionPersistenceAndResume(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "first answer", in: 1, out: 1})
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "remember the word kiwi")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	sid := r["session_id"].(string)
	files := sessionFiles(t, f)
	if len(files) != 1 || filepath.Base(files[0]) != sid+".jsonl" {
		t.Fatalf("transcripts %v (session %s)", files, sid)
	}
	msgs, err := sessionstore.New(filepath.Join(f.home, ".config", "gocode")).Load(f.cwd, sid)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("persisted %d messages: %v", len(msgs), err)
	}

	// --resume <id> continues the same session and reports its id.
	p.turns = []fakeTurn{{text: "kiwi", in: 1, out: 1}}
	code, out, errOut = f.run(t, "-p", "--output-format", "json", "--resume", sid, "what was the word?")
	if code != 0 {
		t.Fatalf("resume exit %d err %q", code, errOut)
	}
	_ = json.Unmarshal([]byte(out), &r)
	if r["session_id"] != sid || r["result"] != "kiwi" {
		t.Fatalf("resumed result = %v", r)
	}
	msgs, _ = sessionstore.New(filepath.Join(f.home, ".config", "gocode")).Load(f.cwd, sid)
	if len(msgs) != 4 {
		t.Fatalf("resumed transcript has %d messages, want 4", len(msgs))
	}
	if n := len(sessionFiles(t, f)); n != 1 {
		t.Fatalf("resume created extra transcripts: %d", n)
	}

	// --continue picks the same (latest) session.
	code, out, _ = f.run(t, "-p", "--output-format", "json", "--continue", "again")
	_ = json.Unmarshal([]byte(out), &r)
	if code != 0 || r["session_id"] != sid {
		t.Fatalf("continue: exit %d result %v", code, r)
	}

	// --session-id names a fresh session; reusing it is refused.
	const fresh = "0b8f0c4e-3d1a-4f5e-9a7b-2c6d8e9f1a2b"
	code, out, errOut = f.run(t, "-p", "--output-format", "json", "--session-id", fresh, "new one")
	_ = json.Unmarshal([]byte(out), &r)
	if code != 0 || r["session_id"] != fresh {
		t.Fatalf("--session-id: exit %d err %q result %v", code, errOut, r)
	}
	if _, err := os.Stat(filepath.Join(sessionstore.New(filepath.Join(f.home, ".config", "gocode")).ProjectDir(f.cwd), fresh+".jsonl")); err != nil {
		t.Fatalf("--session-id transcript missing: %v", err)
	}
	code, _, errOut = f.run(t, "-p", "--session-id", fresh, "again")
	if code != 1 || !strings.Contains(errOut, "Error: Session ID "+fresh+" is already in use.") {
		t.Fatalf("reused --session-id: exit %d err %q", code, errOut)
	}

	// --fork-session copies into a new session.
	code, out, _ = f.run(t, "-p", "--output-format", "json", "--resume", sid, "--fork-session", "forked")
	_ = json.Unmarshal([]byte(out), &r)
	if code != 0 || r["session_id"] == sid || r["session_id"] == "" {
		t.Fatalf("fork: exit %d result %v", code, r)
	}
}

func TestPrintNoSessionPersistence(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "ephemeral", in: 1, out: 1})
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--no-session-persistence", "q")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	if r["session_id"] == "" || r["result"] != "ephemeral" {
		t.Fatalf("result = %v", r)
	}
	if _, err := os.Stat(filepath.Join(f.home, ".config", "gocode", "projects")); err == nil {
		files := sessionFiles(t, f)
		if len(files) != 0 {
			t.Fatalf("transcripts written: %v", files)
		}
	}
	// Text mode as well.
	code, out, _ = f.run(t, "-p", "--no-session-persistence", "q")
	if code != 0 || out != "ephemeral\n" || len(sessionFiles(t, f)) != 0 {
		t.Fatalf("text: exit %d out %q files %v", code, out, sessionFiles(t, f))
	}
	// Outside print mode the flag is refused.
	f.stdinTTY = true
	env, _, errb := f.env()
	env.StdoutTTY = true
	if code := Run(env, []string{"gocode", "--no-session-persistence"}); code != 1 || !strings.Contains(errb.String(), "Error: --no-session-persistence can only be used with --print mode.") {
		t.Fatalf("interactive: exit %d err %q", code, errb.String())
	}
}

func TestPrintStreamJSONInput(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "answer one", in: 1, out: 1}, fakeTurn{text: "answer two", in: 1, out: 1})
	f := headlessFixture(t, p)
	f.stdinTTY = false
	f.stdin = `{"type":"user","message":{"role":"user","content":"first"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"second"}]}}` + "\n"
	code, out, errOut := f.run(t, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	var results []string
	for _, r := range decodeLines(t, out) {
		if r["type"] == "result" {
			results = append(results, r["result"].(string))
		}
	}
	if strings.Join(results, "|") != "answer one|answer two" {
		t.Fatalf("results = %v\n%s", results, out)
	}
	f.stdin = "not json\n"
	code, _, errOut = f.run(t, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	if code != 1 || !strings.Contains(errOut, "Error parsing streaming input line (type=unknown, 8 chars)") {
		t.Fatalf("bad line: exit %d err %q", code, errOut)
	}
}

func TestPrintJSONSchemaStructuredOutput(t *testing.T) {
	schema := `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`
	p := newFakeProvider(t,
		fakeTurn{toolName: "StructuredOutput", toolArgs: `{"nom":"x"}`, in: 1, out: 1},
		fakeTurn{toolName: "StructuredOutput", toolArgs: `{"name":"Ada"}`, in: 1, out: 1},
		fakeTurn{text: "should not be asked", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--json-schema", schema, "who?")
	if code != 0 {
		t.Fatalf("exit %d err %q out %q", code, errOut, out)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	so, ok := r["structured_output"].(map[string]any)
	if !ok || so["name"] != "Ada" || r["result"] != `{"name":"Ada"}` {
		t.Fatalf("result = %v", r)
	}
	if p.callCount() != 2 {
		t.Fatalf("model called %d times, want 2 (the valid call ends the turn)", p.callCount())
	}
	var advertised bool
	for _, n := range p.tools[0] {
		if n == "StructuredOutput" {
			advertised = true
		}
	}
	if !advertised {
		t.Fatalf("StructuredOutput not offered: %v", p.tools[0])
	}
}
