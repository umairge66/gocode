package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// streamArgs are the flags of a stream-json input session.
var streamArgs = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-prompt-tool", "stdio"}

// controlResponses indexes a run's control_response records by request id.
func controlResponses(recs []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range recs {
		if r["type"] != headless.TypeControlResponse {
			continue
		}
		body := r["response"].(map[string]any)
		out[body["request_id"].(string)] = body
	}
	return out
}

// Control requests are answered on stdout; system/init lists the
// supported subtypes.
func TestStreamInputControlRequests(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "ok", in: 1, out: 1})
	f := headlessFixture(t, p)
	f.stdinTTY = false
	subcmdWriteFile(t, filepath.Join(f.cwd, "gocode.md"), "Always run the tests before committing.\n")
	req := func(id, body string) string {
		return `{"type":"control_request","request_id":"` + id + `","request":` + body + "}\n"
	}
	f.stdin = req("i1", `{"subtype":"initialize"}`) +
		req("m1", `{"subtype":"mcp_status"}`) +
		req("c1", `{"subtype":"get_context_usage"}`) +
		req("p1", `{"subtype":"set_permission_mode","mode":"acceptEdits"}`) +
		req("p2", `{"subtype":"set_permission_mode","mode":"bypassPermissions"}`) +
		req("p3", `{"subtype":"set_permission_mode","mode":"sideways"}`) +
		req("s1", `{"subtype":"set_model","model":"gocode-haiku-4-5"}`) +
		req("x1", `{"subtype":"interrupt"}`) +
		req("b1", `{"subtype":"teleport"}`) +
		`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n"
	code, out, errOut := f.run(t, streamArgs...)
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	recs := decodeLines(t, out)
	caps := fmt.Sprint(recs[0]["capabilities"])
	for _, c := range headless.ControlCapabilities {
		if !strings.Contains(caps, c) {
			t.Fatalf("init capabilities = %v", caps)
		}
	}
	resp := controlResponses(recs)
	ok := func(id string) map[string]any {
		t.Helper()
		r := resp[id]
		if r == nil || r["subtype"] != headless.ControlSuccess {
			t.Fatalf("%s: %v", id, r)
		}
		return r["response"].(map[string]any)
	}
	fail := func(id, want string) {
		t.Helper()
		r := resp[id]
		if r == nil || r["subtype"] != headless.ControlError || !strings.Contains(fmt.Sprint(r["error"]), want) {
			t.Fatalf("%s: %v", id, r)
		}
	}
	init := ok("i1")
	if init["output_style"] != "default" || init["commands"] == nil {
		t.Fatalf("initialize = %v", init)
	}
	models := init["models"].([]any)
	var values []string
	for _, m := range models {
		e := m.(map[string]any)
		if e["displayName"] == "" || e["description"] == "" {
			t.Fatalf("model entry %v", e)
		}
		values = append(values, e["value"].(string))
	}
	if got := strings.Join(values, " "); got != "default anthropic/claude-sonnet-5-5 anthropic/claude-opus-5-5 anthropic/claude-haiku-5-5 anthropic/claude-fable-5-1" {
		t.Fatalf("models = %s", got)
	}
	if acct := init["account"].(map[string]any); acct["provider"] != "openrouter" || acct["apiKeySource"] != "OPENROUTER_API_KEY" {
		t.Fatalf("account = %v", acct)
	}
	if strings.Contains(out, "sk-test-not-a-real-key") {
		t.Fatal("the API key reached the output")
	}
	if st := ok("m1"); fmt.Sprint(st["mcpServers"]) != "[]" {
		t.Fatalf("mcp_status = %v", st)
	}
	cu := ok("c1")
	if cu["maxTokens"].(float64) <= 0 || cu["model"] != "gocode-sonnet-4-5" {
		t.Fatalf("get_context_usage = %v", cu)
	}
	cats := map[string]float64{}
	for _, c := range cu["categories"].([]any) {
		e := c.(map[string]any)
		cats[e["id"].(string)] = e["tokens"].(float64)
	}
	if cats["system_prompt"] <= 0 || cats["system_tools"] <= 0 || cats["memory_files"] <= 0 || cats["free_space"] <= 0 {
		t.Fatalf("categories = %v", cu["categories"])
	}
	if mf := fmt.Sprint(cu["memoryFiles"]); !strings.Contains(mf, "gocode.md") || !strings.Contains(mf, "Project") {
		t.Fatalf("memoryFiles = %s", mf)
	}
	if pm := ok("p1"); pm["mode"] != "acceptEdits" {
		t.Fatalf("set_permission_mode = %v", pm)
	}
	fail("p2", "bypassPermissions")
	fail("p3", "Invalid permission mode")
	if sm := ok("s1"); sm["model"] != "gocode-haiku-4-5" {
		t.Fatalf("set_model = %v", sm)
	}
	ok("x1")
	fail("b1", "Unsupported control request subtype: teleport")
	// The turn ran on the model set_model chose.
	if !strings.Contains(p.bodies[0], `"model":"gocode-haiku-4-5"`) {
		t.Fatalf("request = %s", p.bodies[0])
	}
}

// A user message's image blocks reach the model.
func TestStreamInputImages(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "a square", in: 1, out: 1})
	f := headlessFixture(t, p)
	f.stdinTTY = false
	f.stdin = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"what is it?"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}}` + "\n"
	code, _, errOut := f.run(t, streamArgs...)
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if !strings.Contains(p.bodies[0], "data:image/png;base64,QUJD") || !strings.Contains(p.bodies[0], "what is it?") {
		t.Fatalf("request = %s", p.bodies[0])
	}
}

// streamHost drives a running stream-json session the way an SDK host
// does: it writes records on the session's stdin and reads its stdout.
type streamHost struct {
	t     *testing.T
	stdin *io.PipeWriter
	lines chan map[string]any
	code  chan int
	seen  []map[string]any
	errb  *bytes.Buffer
}

func startStreamHost(t *testing.T, f *subcmdFixture) *streamHost {
	t.Helper()
	env, _, errb := f.env()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	env.Stdin, env.Stdout = inR, outW
	h := &streamHost{t: t, stdin: inW, lines: make(chan map[string]any, 256), code: make(chan int, 1), errb: errb}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				h.lines <- m
			}
		}
		close(h.lines)
	}()
	wd, _ := os.Getwd()
	go func() {
		code := Run(env, append([]string{"gocode"}, streamArgs...))
		_ = outW.Close()
		h.code <- code
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = os.Chdir(wd)
	})
	return h
}

func (h *streamHost) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stdin, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

// waitFor returns the next record matching pred.
func (h *streamHost) waitFor(what string, pred func(map[string]any) bool) map[string]any {
	h.t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case m, ok := <-h.lines:
			if !ok {
				h.t.Fatalf("output ended waiting for %s; seen %v", what, h.seen)
			}
			h.seen = append(h.seen, m)
			if pred(m) {
				return m
			}
		case <-timeout:
			h.t.Fatalf("timed out waiting for %s; seen %v", what, h.seen)
		}
	}
}

// finish closes stdin and returns the exit code once the session ended.
func (h *streamHost) finish() int {
	h.t.Helper()
	_ = h.stdin.Close()
	for m := range h.lines {
		h.seen = append(h.seen, m)
	}
	select {
	case c := <-h.code:
		return c
	case <-time.After(30 * time.Second):
		h.t.Fatal("session did not end")
	}
	return -1
}

func isType(typ string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["type"] == typ }
}

func canUseTool(m map[string]any) bool {
	if m["type"] != headless.TypeControlRequest {
		return false
	}
	return m["request"].(map[string]any)["subtype"] == headless.ControlCanUseTool
}

func userMsg(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	return string(b)
}

// A permission ask goes to the host as can_use_tool; a message sent while
// the turn waits rides the turn's next model call, and the turn reports one
// result.
func TestStreamInputCanUseToolAndMidTurnMessage(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Write", in: 1, out: 1},
		fakeTurn{text: "wrote it", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	f.stdinTTY = false
	target := filepath.Join(f.cwd, "out.txt")
	p.turns[0].toolArgs = fmt.Sprintf(`{"file_path":%q,"content":"x"}`, target)
	h := startStreamHost(t, f)
	h.send(userMsg("write it"))
	ask := h.waitFor("can_use_tool", canUseTool)
	body := ask["request"].(map[string]any)
	if body["tool_name"] != "Write" || body["tool_use_id"] != "call_gen-0" || body["input"].(map[string]any)["file_path"] != target {
		t.Fatalf("can_use_tool = %v", body)
	}
	if s := fmt.Sprint(body["permission_suggestions"]); !strings.Contains(s, "addRules") || !strings.Contains(s, "acceptEdits") {
		t.Fatalf("suggestions = %s", s)
	}
	h.send(userMsg("and also say hi"))
	h.send(`{"type":"control_response","response":{"subtype":"success","request_id":"` + ask["request_id"].(string) +
		`","response":{"behavior":"allow","updatedInput":` + mustJSON(t, body["input"]) + `}}}`)
	res := h.waitFor("result", isType("result"))
	if res["result"] != "wrote it" || res["is_error"] != false {
		t.Fatalf("result = %v", res)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "x" {
		t.Fatalf("write did not happen: %v", err)
	}
	if code := h.finish(); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errb)
	}
	results := 0
	for _, m := range h.seen {
		if m["type"] == "result" {
			results++
		}
	}
	if results != 1 || p.callCount() != 2 || !strings.Contains(p.bodies[1], "and also say hi") {
		t.Fatalf("results %d, calls %d, second request %s", results, p.callCount(), p.bodies[1])
	}
}

// interrupt ends the running turn (its pending can_use_tool is withdrawn)
// and the session takes the next message.
func TestStreamInputInterrupt(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Write", in: 1, out: 1},
		fakeTurn{text: "next answer", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	f.stdinTTY = false
	target := filepath.Join(f.cwd, "out.txt")
	p.turns[0].toolArgs = fmt.Sprintf(`{"file_path":%q,"content":"x"}`, target)
	h := startStreamHost(t, f)
	h.send(userMsg("write it"))
	ask := h.waitFor("can_use_tool", canUseTool)
	h.send(`{"type":"control_request","request_id":"int1","request":{"subtype":"interrupt"}}`)
	h.waitFor("cancel", func(m map[string]any) bool {
		return m["type"] == headless.TypeControlCancelRequest && m["request_id"] == ask["request_id"]
	})
	res := h.waitFor("result", isType("result"))
	if res["is_error"] != true || !strings.Contains(fmt.Sprint(res["errors"]), "Request interrupted by user") {
		t.Fatalf("interrupted result = %v", res)
	}
	h.send(userMsg("go on"))
	res = h.waitFor("second result", isType("result"))
	if res["result"] != "next answer" {
		t.Fatalf("second result = %v", res)
	}
	if code := h.finish(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("interrupted write happened")
	}
	acked := false
	for _, m := range h.seen {
		if m["type"] == headless.TypeControlResponse && m["response"].(map[string]any)["request_id"] == "int1" {
			acked = true
		}
	}
	if !acked {
		t.Fatalf("interrupt not answered: %v", h.seen)
	}
}

// /compact emits a manual compact_boundary; a retried request emits
// api_retry.
func TestStreamCompactBoundaryAndAPIRetry(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "summary of it all", in: 10, out: 1})
	var mu sync.Mutex
	failed := false
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := !failed
		failed = true
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		p.serve(w, r)
	}))
	t.Cleanup(flaky.Close)
	f := headlessFixture(t, p)
	f.vars["OPENROUTER_BASE_URL"] = flaky.URL
	f.vars["GOCODE_MAX_RETRIES"] = "1"
	f.stdinTTY = false
	f.stdin = userMsg("hi") + "\n" + userMsg("/compact") + "\n"
	code, out, errOut := f.run(t, streamArgs...)
	if code != 0 {
		t.Fatalf("exit %d err %q\n%s", code, errOut, out)
	}
	var retry, boundary map[string]any
	for _, r := range decodeLines(t, out) {
		if r["type"] == "system" && r["subtype"] == "api_retry" {
			retry = r
		}
		if r["type"] == "system" && r["subtype"] == "compact_boundary" {
			boundary = r
		}
		if r["type"] != headless.TypeControlResponse && (r["session_id"] == nil || r["uuid"] == nil) {
			t.Fatalf("record without session_id/uuid: %v", r)
		}
	}
	if retry == nil || retry["attempt"].(float64) != 1 || retry["max_retries"].(float64) != 1 || retry["error_status"].(float64) != 500 {
		t.Fatalf("api_retry = %v\n%s", retry, out)
	}
	if boundary == nil || boundary["compact_metadata"].(map[string]any)["trigger"] != "manual" ||
		boundary["compact_metadata"].(map[string]any)["pre_tokens"].(float64) <= 0 {
		t.Fatalf("compact_boundary = %v\n%s", boundary, out)
	}
}

// --- unit tests -------------------------------------------------------------

func TestUserContentBlocks(t *testing.T) {
	got := userContentBlocks(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},{"type":"text","text":"b"},{"type":"image"}]`))
	if len(got) != 2 || got[0].Text != "a\nb" || got[1].Type != agent.BlockImage || got[1].Source.Data != "QUJD" {
		t.Fatalf("blocks = %+v", got)
	}
	if got := userContentBlocks(json.RawMessage(`"  hi  "`)); len(got) != 1 || got[0].Text != "hi" {
		t.Fatalf("string = %+v", got)
	}
	if got := userContentBlocks(json.RawMessage(`[]`)); got != nil {
		t.Fatalf("empty = %+v", got)
	}
}

func TestPermissionFromResult(t *testing.T) {
	req := toolrun.PermissionRequest{ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`), Suggested: []string{"Bash(ls:*)"}}
	cases := []struct {
		name string
		res  headless.PermissionResult
		want toolrun.PermissionResponseKind
		fb   string
	}{
		{"allow", headless.PermissionResult{Behavior: "allow"}, toolrun.PermissionAllowedOnce, ""},
		{"allow same input", headless.PermissionResult{Behavior: "allow", UpdatedInput: json.RawMessage(`{ "command" : "ls" }`)}, toolrun.PermissionAllowedOnce, ""},
		{"allow changed input", headless.PermissionResult{Behavior: "allow", UpdatedInput: json.RawMessage(`{"command":"ls -la"}`)}, toolrun.PermissionAllowedOnce, ""},
		{"allow rule", headless.PermissionResult{Behavior: "allow", UpdatedPermissions: []headless.PermissionUpdate{{Type: "addRules", Behavior: "allow", Rules: []headless.PermissionRule{{ToolName: "Bash", RuleContent: "ls:*"}}}}}, toolrun.PermissionAllowedAlways, ""},
		{"deny", headless.PermissionResult{Behavior: "deny", Message: "not now"}, toolrun.PermissionDenied, "not now"},
		{"deny interrupt", headless.PermissionResult{Behavior: "deny", Message: "stop", Interrupt: true}, toolrun.PermissionDenied, ""},
	}
	for _, c := range cases {
		got, err := permissionFromResult(req, c.res)
		if err != nil || got.Kind != c.want || (c.fb == "" && got.Feedback != "") || !strings.Contains(got.Feedback, c.fb) {
			t.Errorf("%s: %+v %v", c.name, got, err)
		}
		if got.Kind == toolrun.PermissionAllowedAlways && (got.Rule == nil || got.Rule.ToolName != "Bash" || got.Rule.Content != "ls:*" || got.Rule.Behavior != toolreg.RuleAllow) {
			t.Errorf("%s: rule %+v", c.name, got.Rule)
		}
	}
	if _, err := permissionFromResult(req, headless.PermissionResult{Behavior: "maybe"}); err == nil {
		t.Error("unknown behavior accepted")
	}
	// A changed input rides on the answer; the same input does not.
	if got, _ := permissionFromResult(req, headless.PermissionResult{Behavior: "allow", UpdatedInput: json.RawMessage(`{"command":"ls -la"}`)}); string(got.UpdatedInput) != `{"command":"ls -la"}` {
		t.Errorf("changed input not carried: %+v", got)
	}
	if got, _ := permissionFromResult(req, headless.PermissionResult{Behavior: "allow", UpdatedInput: json.RawMessage(`{ "command" : "ls" }`)}); got.UpdatedInput != nil {
		t.Errorf("unchanged input carried: %+v", got)
	}
	if _, err := permissionFromResult(req, headless.PermissionResult{Behavior: "allow", UpdatedInput: json.RawMessage(`["ls"]`)}); err == nil {
		t.Error("a non-object updatedInput accepted")
	}
	edit := toolrun.PermissionRequest{ToolName: "Edit", IsEdit: true}
	if got, _ := permissionFromResult(edit, headless.PermissionResult{Behavior: "allow", UpdatedPermissions: []headless.PermissionUpdate{{Type: "setMode", Mode: "acceptEdits"}}}); got.Kind != toolrun.PermissionAllowedEditsSession {
		t.Errorf("setMode: %+v", got)
	}
}

// The MCP permission prompt tool is called with the call's name, input and
// id, and its JSON text is the decision.
func TestMCPPromptTool(t *testing.T) {
	var gotName string
	var gotInput map[string]any
	p := mcpPromptTool{tool: "mcp__auth__approve", call: func(_ context.Context, name string, input json.RawMessage) (string, bool, error) {
		gotName = name
		_ = json.Unmarshal(input, &gotInput)
		return `{"behavior":"deny","message":"policy says no"}`, false, nil
	}}
	resp, err := p.Prompt(context.Background(), toolrun.PermissionRequest{ToolName: "Bash", Input: json.RawMessage(`{"command":"make"}`)})
	if err != nil || resp.Kind != toolrun.PermissionDenied || resp.Feedback != "policy says no" {
		t.Fatalf("resp = %+v %v", resp, err)
	}
	if gotName != "mcp__auth__approve" || gotInput["tool_name"] != "Bash" || gotInput["input"].(map[string]any)["command"] != "make" {
		t.Fatalf("call = %s %v", gotName, gotInput)
	}
	bad := mcpPromptTool{tool: "mcp__a__b", call: func(context.Context, string, json.RawMessage) (string, bool, error) { return "yes", false, nil }}
	if _, err := bad.Prompt(context.Background(), toolrun.PermissionRequest{ToolName: "Bash"}); err == nil {
		t.Fatal("non-JSON answer accepted")
	}
}

func TestValidatePermissionPromptTool(t *testing.T) {
	for _, c := range []struct{ tool, input, want string }{
		{"", "", ""},
		{"stdio", headless.FormatStreamJSON, ""},
		{"stdio", "", "requires --input-format=stream-json"},
		{"mcp__auth__approve", "", ""},
		{"approve", "", "must name an MCP tool"},
	} {
		if got := validatePermissionPromptTool(c.tool, c.input); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%q/%q: %q", c.tool, c.input, got)
		}
	}
}

// Partial events frame thinking, text and tool_use blocks with their own
// indices.
func TestPartialStreamerBlocks(t *testing.T) {
	var buf bytes.Buffer
	h := &headlessRun{s: &printSession{Model: "m"}, rt: &sessionRuntime{conv: conversation.New(nil, nil)}, stream: headless.NewWriter(&buf)}
	p := &partialStreamer{h: h}
	p.thinking("hmm")
	p.text("Look")
	p.text("ing.")
	p.toolStart("tu_1", "Read")
	p.toolInput("tu_1", `{"file_`)
	p.toolInput("tu_1", `path":"a"}`)
	p.toolStart("tu_2", "Grep")
	p.finish(agent.MessageEvent{StopReason: "tool_use"})
	var got []string
	for _, r := range decodeLines(t, buf.String()) {
		ev := r["event"].(map[string]any)
		s := ev["type"].(string)
		if idx, ok := ev["index"]; ok {
			s += fmt.Sprintf("@%v", idx)
		}
		if d, ok := ev["delta"].(map[string]any); ok && d["type"] != nil {
			s += ":" + d["type"].(string)
		}
		if cb, ok := ev["content_block"].(map[string]any); ok {
			s += ":" + cb["type"].(string)
		}
		got = append(got, s)
	}
	want := []string{
		"message_start",
		"content_block_start@0:thinking", "content_block_delta@0:thinking_delta", "content_block_stop@0",
		"content_block_start@1:text", "content_block_delta@1:text_delta", "content_block_delta@1:text_delta", "content_block_stop@1",
		"content_block_start@2:tool_use", "content_block_delta@2:input_json_delta", "content_block_delta@2:input_json_delta", "content_block_stop@2",
		"content_block_start@3:tool_use", "content_block_stop@3",
		"message_delta", "message_stop",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, " "), strings.Join(want, " "))
	}
}

// A subagent's messages are forwarded with parent_tool_use_id set to the
// Agent tool call that spawned it; the session's own stay null.
func TestStreamSubagentMessagesCarryParentToolUseID(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Agent", toolArgs: `{"description":"look","prompt":"say hi","subagent_type":"general-purpose"}`, in: 1, out: 1},
		fakeTurn{toolName: "SubagentHandback", toolArgs: `{"message":"hi"}`, in: 1, out: 1},
		fakeTurn{text: "done", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "delegate", "--output-format", "stream-json", "--verbose")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	var agentCall string
	var childAssistant, childUser, mainAssistant int
	for _, m := range decodeLines(t, out) {
		if m["type"] != "assistant" && m["type"] != "user" {
			continue
		}
		parent, _ := m["parent_tool_use_id"].(string)
		if m["type"] == "assistant" && parent == "" {
			mainAssistant++
			for _, b := range m["message"].(map[string]any)["content"].([]any) {
				if blk := b.(map[string]any); blk["type"] == "tool_use" && blk["name"] == "Agent" {
					agentCall = blk["id"].(string)
				}
			}
			continue
		}
		if parent == "" {
			continue
		}
		if parent != agentCall {
			t.Fatalf("parent_tool_use_id = %q, want the Agent call %q", parent, agentCall)
		}
		if m["type"] == "assistant" {
			childAssistant++
		} else {
			childUser++
		}
	}
	if agentCall == "" || childAssistant != 1 || childUser != 1 || mainAssistant != 2 {
		t.Fatalf("agent call %q, child assistant %d user %d, main assistant %d:\n%s", agentCall, childAssistant, childUser, mainAssistant, out)
	}
}

// mcp_status reports each server as the Agent SDK's McpServerStatus: a
// connected server with its serverInfo, config (no env), scope / source and
// its tools with their annotations; one still connecting as "pending" with
// no tools; a disabled one as "disabled". system/init uses the same status
// vocabulary.
func TestMCPStatusFullShape(t *testing.T) {
	yes, no := true, false
	mem := echoServer()
	mem.tools = append(mem.tools, mcp.Tool{Name: "wipe", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &no, DestructiveHint: &yes, OpenWorldHint: &no}},
		mcp.Tool{Name: "peek", Description: "reads", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &yes}})
	slow := echoServer()
	slow.initGate = make(chan struct{})
	defer close(slow.initGate)
	servers := map[string]mcp.ServerConfig{
		"mem":  {Command: "mem-server", Args: []string{"--x"}, Env: map[string]string{"TOKEN": "secret"}},
		"slow": {Type: mcp.TransportHTTP, URL: "https://slow.example/mcp", Headers: map[string]string{"Authorization": "Bearer secret"}},
	}
	host := mcphost.Start(context.Background(), servers, mcphost.Options{
		Transport: func(name string, cfg mcp.ServerConfig) (mcp.Transport, error) {
			if name == "slow" {
				return slow.factory()(name, cfg)
			}
			return mem.factory()(name, cfg)
		},
		Background: func(name string, _ mcp.ServerConfig) bool { return name == "slow" },
	})
	defer host.Close()
	g := mcphost.Gathered{
		Servers: servers,
		Scopes:  map[string]mcphost.Scope{"mem": mcphost.ScopeUser, "slow": mcphost.ScopeProject},
		Skipped: []mcphost.Skipped{{Name: "off", Scope: mcphost.ScopeLocal, Reason: mcphost.ReasonDisabled},
			{Name: "unapproved", Scope: mcphost.ScopeProject, Reason: mcphost.ReasonPendingApproval}},
		SkippedConfigs: map[string]mcp.ServerConfig{"off": {Command: "off-server"}},
	}
	f := newSubcmdFixture(t)
	env, _, _ := f.env()
	h := &headlessRun{env: env, s: &printSession{Model: "m"}, rt: &sessionRuntime{conv: conversation.New(nil, nil), mcp: host, gathered: g}}

	raw, _ := json.Marshal(h.mcpStatus())
	var got []map[string]any
	_ = json.Unmarshal(raw, &got)
	if len(got) != 3 {
		t.Fatalf("mcp_status = %s", raw)
	}
	want := []string{
		`{"config":{"args":["--x"],"command":"mem-server","type":"stdio"},"name":"mem","scope":"user","serverInfo":{"name":"mem","version":"1"},"source":"user","status":"connected","tools":[` +
			`{"description":"echoes","name":"echo"},` +
			`{"annotations":{"destructive":true,"openWorld":false,"readOnly":false},"name":"wipe"},` +
			`{"annotations":{"readOnly":true},"description":"reads","name":"peek"}]}`,
		`{"config":{"type":"http","url":"https://slow.example/mcp"},"name":"slow","scope":"project","source":"project","status":"pending"}`,
		`{"config":{"command":"off-server","type":"stdio"},"name":"off","scope":"local","source":"local","status":"disabled"}`,
	}
	for i, w := range want {
		b, _ := json.Marshal(got[i])
		if string(b) != w {
			t.Errorf("server %d:\n got %s\nwant %s", i, b, w)
		}
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("mcp_status leaks env or headers: %s", raw)
	}

	init := h.systemInit()
	b, _ := json.Marshal(init.MCPServers)
	if string(b) != `[{"name":"mem","status":"connected","source":"user"},{"name":"slow","status":"pending","source":"project"}]` {
		t.Fatalf("system/init mcp_servers = %s", b)
	}
}
