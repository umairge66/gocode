package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

var testSchema = json.RawMessage(`{"type":"object"}`)

func catalog() []ToolSchema {
	return []ToolSchema{
		{Name: "Bash", Description: "run", InputSchema: testSchema},
		{Name: "NotebookEdit", Description: "nb", InputSchema: testSchema, Deferred: true},
		{Name: "Read", Description: "read", InputSchema: testSchema},
		{Name: ToolSearchName, Description: "search", InputSchema: testSchema},
		{Name: "WebFetch", Description: "fetch", InputSchema: testSchema, Deferred: true},
		{Name: "mcp__srv__echo", Description: "echo", InputSchema: testSchema, Deferred: true},
	}
}

func names(ts []ToolSchema) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return strings.Join(out, ",")
}

func loadedResult(id string, loaded ...string) Message {
	b := ToolResultBlock(id, "Loaded", false)
	b.LoadedTools = loaded
	return UserMessage(b)
}

func TestSelectToolsDeferredUntilLoaded(t *testing.T) {
	send, deferred := SelectTools(catalog(), nil)
	if got := names(send); got != "Bash,Read,ToolSearch" {
		t.Fatalf("send = %s", got)
	}
	if got := names(deferred); got != "NotebookEdit,WebFetch,mcp__srv__echo" {
		t.Fatalf("deferred = %s", got)
	}

	// Loaded tools are appended after the core tools, in load order, and
	// stay there on later turns.
	history := []Message{
		UserMessage(TextBlock("hi")),
		loadedResult("a", "mcp__srv__echo"),
		UserMessage(TextBlock("more")),
		loadedResult("b", "WebFetch", "mcp__srv__echo", "Bash", "Unknown"),
	}
	send, deferred = SelectTools(catalog(), history)
	if got := names(send); got != "Bash,Read,ToolSearch,mcp__srv__echo,WebFetch" {
		t.Fatalf("send after loading = %s", got)
	}
	if got := names(deferred); got != "NotebookEdit" {
		t.Fatalf("deferred after loading = %s", got)
	}
	if !send[3].Deferred || send[0].Deferred {
		t.Fatalf("Deferred flags not kept: %+v", send)
	}
	// A later turn keeps the same prefix order.
	history = append(history, UserMessage(TextBlock("again")), loadedResult("c", "NotebookEdit"))
	send, _ = SelectTools(catalog(), history)
	if got := names(send); got != "Bash,Read,ToolSearch,mcp__srv__echo,WebFetch,NotebookEdit" {
		t.Fatalf("send = %s", got)
	}
}

func TestSelectToolsWithoutToolSearchSendsAll(t *testing.T) {
	var cat []ToolSchema
	for _, s := range catalog() {
		if s.Name != ToolSearchName {
			cat = append(cat, s)
		}
	}
	send, deferred := SelectTools(cat, nil)
	if len(deferred) != 0 || len(send) != len(cat) {
		t.Fatalf("send %s deferred %s", names(send), names(deferred))
	}
	for _, s := range send {
		if s.Deferred {
			t.Fatalf("%s still marked deferred", s.Name)
		}
	}
}

func TestSelectToolsNothingDeferredDropsToolSearch(t *testing.T) {
	cat := []ToolSchema{{Name: "Bash"}, {Name: ToolSearchName}, {Name: "Read"}}
	send, deferred := SelectTools(cat, nil)
	if names(send) != "Bash,Read" || deferred != nil {
		t.Fatalf("send %s deferred %v", names(send), deferred)
	}
}

func TestAnnounceDeferredToolsOnceAndOnChange(t *testing.T) {
	msgs := []Message{UserMessage(TextBlock("hi"))}
	msgs = AnnounceDeferredTools(msgs, catalog())
	if len(msgs) != 1 || len(msgs[0].Content) != 2 {
		t.Fatalf("reminder not appended to the user turn: %+v", msgs)
	}
	r := msgs[0].Content[1]
	for _, want := range []string{"<system-reminder>", deferredAddedLead, "NotebookEdit\nWebFetch\nmcp__srv__echo"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("reminder lacks %q:\n%s", want, r.Text)
		}
	}
	// Unchanged set: nothing more.
	again := AnnounceDeferredTools(msgs, catalog())
	if len(again[0].Content) != 2 {
		t.Fatalf("announced twice: %+v", again)
	}
	// Loading a tool does not withdraw it.
	msgs = append(msgs, AssistantMessage(ToolUseBlock("s", ToolSearchName, nil)), loadedResult("s", "WebFetch"))
	if out := AnnounceDeferredTools(msgs, catalog()); len(out[len(out)-1].Content) != 1 {
		t.Fatalf("loaded tool re-announced: %+v", out[len(out)-1])
	}
	// The MCP server goes away and a new tool appears.
	cat := catalog()[:5]
	cat = append(cat, ToolSchema{Name: "TodoWrite", Deferred: true})
	out := AnnounceDeferredTools(msgs, cat)
	text := out[len(out)-1].Content[1].Text
	if !strings.Contains(text, "TodoWrite") || strings.Contains(text, "NotebookEdit") {
		t.Fatalf("delta should list only the new tool:\n%s", text)
	}
	if !strings.Contains(text, "no longer available (their MCP server disconnected)") || !strings.Contains(text, "mcp__srv__echo") {
		t.Fatalf("delta should withdraw the MCP tool:\n%s", text)
	}
	// Without deferral nothing is announced.
	plain := []ToolSchema{{Name: "Bash"}, {Name: "Read"}}
	if out := AnnounceDeferredTools([]Message{UserMessage(TextBlock("x"))}, plain); len(out[0].Content) != 1 {
		t.Fatalf("announced without deferral: %+v", out)
	}
}

// recordingModel records each request and replays scripted responses.
type recordingModel struct {
	mu        sync.Mutex
	reqs      []ModelRequest
	responses []ModelResponse
}

func (m *recordingModel) CallModel(_ context.Context, req ModelRequest) (ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	if i := len(m.reqs) - 1; i < len(m.responses) {
		return m.responses[i], nil
	}
	return ModelResponse{Message: AssistantMessage(TextBlock("done")), StopReason: StopEndTurn}, nil
}

// searchDispatcher answers ToolSearch with the names after "select:" and
// every other tool with "ran <name>".
type searchDispatcher struct {
	mu  sync.Mutex
	ran []string
}

func (d *searchDispatcher) Dispatch(_ context.Context, call ToolUse) (ToolResult, error) {
	d.mu.Lock()
	d.ran = append(d.ran, call.Name)
	d.mu.Unlock()
	if call.Name == ToolSearchName {
		var in struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(call.Input, &in)
		list := strings.Split(strings.TrimPrefix(in.Query, "select:"), ",")
		return ToolResult{Content: "Loaded", LoadedTools: list}, nil
	}
	return ToolResult{Content: "ran " + call.Name}, nil
}

func toolCall(id, name, input string) ModelResponse {
	return ModelResponse{Message: AssistantMessage(ToolUseBlock(id, name, json.RawMessage(input))), StopReason: StopToolUse}
}

// TestLoopDeferredToolFlow: a deferred tool called before it is loaded is
// refused with the load-it-first error (not run); after ToolSearch loads it,
// the next request carries it after the core tools and the call runs.
func TestLoopDeferredToolFlow(t *testing.T) {
	model := &recordingModel{responses: []ModelResponse{
		toolCall("c1", "WebFetch", `{"url":"x"}`),
		toolCall("s1", ToolSearchName, `{"query":"select:WebFetch"}`),
		toolCall("c2", "WebFetch", `{"url":"x"}`),
	}}
	disp := &searchDispatcher{}
	l := Loop{Model: model, Tools: disp, ToolSchemas: catalog()}
	res := l.Run(context.Background(), []Message{UserMessage(TextBlock("fetch"))})
	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %s (%v)", res.Reason, res.Err)
	}
	if len(model.reqs) != 4 {
		t.Fatalf("requests = %d, want 4", len(model.reqs))
	}
	for i, want := range []string{
		"Bash,Read,ToolSearch",
		"Bash,Read,ToolSearch",
		"Bash,Read,ToolSearch,WebFetch",
		"Bash,Read,ToolSearch,WebFetch",
	} {
		if got := names(model.reqs[i].Tools); got != want {
			t.Errorf("request %d tools = %s, want %s", i, got, want)
		}
	}
	if got := names(model.reqs[0].DeferredTools); got != "NotebookEdit,WebFetch,mcp__srv__echo" {
		t.Errorf("request 0 deferred = %s", got)
	}
	// The unloaded call was answered without running.
	first := model.reqs[1].Messages[len(model.reqs[1].Messages)-1].Content[0]
	if !first.IsError || !strings.Contains(first.Content, "InputValidationError: WebFetch did not run: it was not loaded when it was called") ||
		!strings.Contains(first.Content, `query "select:WebFetch"`) {
		t.Fatalf("unloaded call result = %+v", first)
	}
	if strings.Join(disp.ran, ",") != "ToolSearch,WebFetch" {
		t.Fatalf("dispatched = %v, want only ToolSearch then WebFetch", disp.ran)
	}
	// The first request announced the deferred tools in the user turn.
	announce := model.reqs[0].Messages[0].Content
	if len(announce) != 2 || !strings.Contains(announce[1].Text, deferredAddedLead) {
		t.Fatalf("first request lacks the deferred-tools reminder: %+v", announce)
	}
	// Loaded state persists in the returned history for the next turn.
	if got := LoadedTools(res.Messages); len(got) != 1 || got[0] != "WebFetch" {
		t.Fatalf("loaded = %v", got)
	}
}

// TestLoopSameReplyLoadAndCall: ToolSearch and the deferred tool in the same
// reply — the call fails (the tool was not loaded when it was called).
func TestLoopSameReplyLoadAndCall(t *testing.T) {
	both := ModelResponse{Message: AssistantMessage(
		ToolUseBlock("s1", ToolSearchName, json.RawMessage(`{"query":"select:NotebookEdit"}`)),
		ToolUseBlock("c1", "NotebookEdit", json.RawMessage(`{}`)),
	), StopReason: StopToolUse}
	model := &recordingModel{responses: []ModelResponse{both}}
	disp := &searchDispatcher{}
	l := Loop{Model: model, Tools: disp, ToolSchemas: catalog()}
	l.Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
	results := model.reqs[1].Messages[len(model.reqs[1].Messages)-1].Content
	if results[0].IsError || !results[1].IsError {
		t.Fatalf("results = %+v", results)
	}
	if strings.Join(disp.ran, ",") != "ToolSearch" {
		t.Fatalf("dispatched = %v", disp.ran)
	}
}
