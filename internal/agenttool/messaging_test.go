package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/inbox"
	"github.com/umairge66/gocode/internal/subagent"
)

// scriptModel routes each request to the script of the agent whose prompt
// opens the conversation; every script sees its own call index and request.
type scriptModel struct {
	mu      sync.Mutex
	scripts map[string]func(n int, req agent.ModelRequest) agent.ModelResponse
	calls   map[string]int
	reqs    map[string][]agent.ModelRequest
}

func newScriptModel() *scriptModel {
	return &scriptModel{scripts: map[string]func(int, agent.ModelRequest) agent.ModelResponse{}, calls: map[string]int{}, reqs: map[string][]agent.ModelRequest{}}
}

func (m *scriptModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	prompt := req.Messages[0].Content[0].Text
	m.mu.Lock()
	m.calls[prompt]++
	n := m.calls[prompt]
	m.reqs[prompt] = append(m.reqs[prompt], req)
	script := m.scripts[prompt]
	m.mu.Unlock()
	return script(n, req), nil
}

func (m *scriptModel) requests(prompt string) []agent.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]agent.ModelRequest(nil), m.reqs[prompt]...)
}

func toolCall(id, name string, in any) agent.ModelResponse {
	b, _ := json.Marshal(in)
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock(id, name, b)), StopReason: "tool_use"}
}

func handback(report string) agent.ModelResponse {
	return toolCall("hb", "SubagentHandback", map[string]string{"message": report})
}

func endTurn() agent.ModelResponse {
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: "end_turn"}
}

// lastUserText is the text blocks of the request's last (user) turn.
func lastUserText(req agent.ModelRequest) string {
	last := req.Messages[len(req.Messages)-1]
	var b strings.Builder
	for _, blk := range last.Content {
		if blk.Type == agent.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// msgDispatcher answers a subagent's SendMessage with the real tool and
// blocks its "Wait" until the test releases that agent.
type msgDispatcher struct {
	send  *inbox.SendMessageTool
	waits map[string]chan struct{} // by agent name
	in    map[string]chan struct{} // closed when the agent enters Wait
}

func (d *msgDispatcher) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	switch call.Name {
	case "SendMessage":
		out, err := d.send.Execute(ctx, call.Input)
		if err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		return agent.ToolResult{Content: out}, nil
	case "Wait":
		who, _ := agent.IdentityFrom(ctx)
		close(d.in[who.Name])
		<-d.waits[who.Name]
		return agent.ToolResult{Content: "waited"}, nil
	}
	return agent.ToolResult{Content: "ok"}, nil
}

type msgHarness struct {
	tool   *AgentTool
	model  *scriptModel
	disp   *msgDispatcher
	main   *inbox.SendMessageTool
	mainQ  *inbox.AgentQueue
	doneMu sync.Mutex
	done   map[string]chan string // by task name: background / resumed completions
}

func newMsgHarness(t *testing.T, waiters ...string) *msgHarness {
	t.Helper()
	h := &msgHarness{model: newScriptModel(), mainQ: &inbox.AgentQueue{}, done: map[string]chan string{}}
	h.tool = NewAgentTool(subagent.NewTaskRegistry(), nil, func(string) agent.ModelCaller { return h.model }, nil)
	pm := &inbox.PeerMessaging{Agents: h.tool.MessageDirectory(), QueueMain: h.mainQ.Push}
	h.disp = &msgDispatcher{send: &inbox.SendMessageTool{P: pm, Subagent: true}, waits: map[string]chan struct{}{}, in: map[string]chan struct{}{}}
	for _, w := range waiters {
		h.disp.waits[w], h.disp.in[w] = make(chan struct{}), make(chan struct{})
	}
	h.tool.NewDispatcher = func([]string) agent.ToolDispatcher { return h.disp }
	h.tool.ToolNames = []string{"Read", "SendMessage", "Wait"}
	h.tool.ToolSchemas = func(allowed []string) []agent.ToolSchema {
		var out []agent.ToolSchema
		for _, n := range allowed {
			out = append(out, agent.ToolSchema{Name: n})
		}
		return out
	}
	notify := func(task *subagent.Task, result string) {
		h.doneCh(task.Name) <- result
	}
	h.tool.OnBackgroundDone = notify
	h.tool.OnSubagentUpdate = notify
	h.main = &inbox.SendMessageTool{P: pm}
	return h
}

func (h *msgHarness) doneCh(name string) chan string {
	h.doneMu.Lock()
	defer h.doneMu.Unlock()
	if h.done[name] == nil {
		h.done[name] = make(chan string, 4)
	}
	return h.done[name]
}

func (h *msgHarness) waitDone(t *testing.T, name string) string {
	t.Helper()
	select {
	case r := <-h.doneCh(name):
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("agent %s did not report", name)
	}
	return ""
}

func (h *msgHarness) spawn(t *testing.T, name, prompt string, background bool) string {
	t.Helper()
	in := map[string]any{"description": "d", "prompt": prompt, "subagent_type": "general-purpose", "name": name, "run_in_background": background}
	out, err := call(t, h.tool, in)
	if err != nil {
		t.Fatalf("spawn %s: %v", name, err)
	}
	return out
}

func (h *msgHarness) sendFromMain(t *testing.T, to, msg string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"to": to, "message": msg})
	out, err := h.main.Execute(context.Background(), b)
	if err != nil {
		t.Fatalf("send to %s: %v", to, err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result %q: %v", out, err)
	}
	return res
}

func waitClosed(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("agent never reached its tool round")
	}
}

// The main conversation messages a running subagent by name: the message is
// queued and reaches the subagent's model at its next tool round, framed as
// coming from the coordinator.
func TestSendMessageMainToRunningSubagentByName(t *testing.T) {
	h := newMsgHarness(t, "worker")
	h.model.scripts["work"] = func(n int, req agent.ModelRequest) agent.ModelResponse {
		switch n {
		case 1:
			return toolCall("w1", "Wait", map[string]any{})
		case 2:
			return handback("worker report")
		}
		return endTurn()
	}
	h.spawn(t, "worker", "work", true)
	waitClosed(t, h.disp.in["worker"])

	// A normalized name matches; the result names the agent as registered.
	res := h.sendFromMain(t, "Worker", "focus on the parser")
	if res["success"] != true || res["message"] != "Message queued for delivery to worker at its next tool round. If it finishes first, it is resumed with the message and notifies again, so wait for that notification before resending." {
		t.Fatalf("send: %v", res)
	}
	close(h.disp.waits["worker"])
	h.waitDone(t, "worker")

	reqs := h.model.requests("work")
	got := lastUserText(reqs[1])
	want := "The coordinator sent a message while you were working:\nfocus on the parser\n\nAddress this before completing your current task."
	if got != want {
		t.Fatalf("second round saw %q, want %q", got, want)
	}
	// The message rides with the tool result of the same round.
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if last.Content[0].Type != agent.BlockToolResult {
		t.Fatalf("mid-turn message not in the tool-result turn: %+v", last.Content)
	}
}

// One subagent messages another by name (and "main"): the sender's name
// frames the <agent-message>, and the receiver gets it at its next round.
func TestSendMessageSubagentToSubagent(t *testing.T) {
	h := newMsgHarness(t, "beta")
	h.model.scripts["beta task"] = func(n int, req agent.ModelRequest) agent.ModelResponse {
		switch n {
		case 1:
			return toolCall("b1", "Wait", map[string]any{})
		case 2:
			return handback("beta report")
		}
		return endTurn()
	}
	h.model.scripts["alpha task"] = func(n int, req agent.ModelRequest) agent.ModelResponse {
		switch n {
		case 1:
			return toolCall("a1", "SendMessage", map[string]string{"to": "beta", "message": "hi beta"})
		case 2:
			return toolCall("a2", "SendMessage", map[string]string{"to": "main", "message": "beta has been told"})
		case 3:
			return handback("alpha report")
		}
		return endTurn()
	}
	h.spawn(t, "beta", "beta task", true)
	waitClosed(t, h.disp.in["beta"])
	h.spawn(t, "alpha", "alpha task", false)

	areqs := h.model.requests("alpha task")
	if r := areqs[1].Messages[len(areqs[1].Messages)-1].Content[0].Content; !strings.Contains(r, `"success":true`) || !strings.Contains(r, "Message queued for delivery to beta") {
		t.Fatalf("alpha's send result: %q", r)
	}
	close(h.disp.waits["beta"])
	h.waitDone(t, "beta")

	got := lastUserText(h.model.requests("beta task")[1])
	want := "Another gocode session sent a message while you were working:\n<agent-message from=\"alpha\">\nhi beta\n</agent-message>\n\n" +
		`That "other gocode session" is an agent working inside this same session`
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "(reply via SendMessage to the `from=` address).") {
		t.Fatalf("beta saw %q", got)
	}
	// "main" got alpha's message under alpha's name, not "subagent".
	msgs := h.mainQ.Take()
	if len(msgs) != 1 || msgs[0].From != "alpha" || !strings.HasPrefix(msgs[0].Text, "<agent-message from=\"alpha\">\nbeta has been told") {
		t.Fatalf("main queue: %+v", msgs)
	}
}

// A finished agent is resumed by a message, which becomes its next prompt;
// it reports again when the resumed turn ends.
func TestSendMessageResumesFinishedAgent(t *testing.T) {
	h := newMsgHarness(t)
	h.model.scripts["scout task"] = func(n int, req agent.ModelRequest) agent.ModelResponse {
		// The hand-back ends a run, so the resumed turn's first call (n == 2)
		// is the next model request.
		switch n {
		case 1:
			return handback("first report")
		case 2:
			return handback("second report")
		}
		return endTurn()
	}
	out := h.spawn(t, "scout", "scout task", false)
	if !strings.Contains(out, "first report") {
		t.Fatalf("spawn: %q", out)
	}
	tk, _ := h.tool.Tasks.ByName("scout")
	if tk.Status != subagent.StatusCompleted {
		t.Fatalf("status %s", tk.Status)
	}

	res := h.sendFromMain(t, "scout", "one more thing")
	if res["success"] != true || res["message"] != "Resuming agent scout" || res["resumedAgentId"] != tk.ID || res["revivedAgentId"] != tk.ID {
		t.Fatalf("send: %v", res)
	}
	if got := h.waitDone(t, "scout"); !strings.Contains(got, "second report") {
		t.Fatalf("resumed report: %q", got)
	}
	reqs := h.model.requests("scout task")
	if len(reqs) < 2 || lastUserText(reqs[1]) != "one more thing" {
		t.Fatalf("resumed prompt: %q", lastUserText(reqs[len(reqs)-1]))
	}
	// The resumed turn carries the whole earlier conversation.
	if len(reqs[1].Messages) < 3 {
		t.Fatalf("history lost: %d messages", len(reqs[1].Messages))
	}
}

// A message that arrives after the agent's last tool round is stranded: it
// resumes the agent once its turn ends.
func TestSendMessageStrandedResumesAgent(t *testing.T) {
	h := newMsgHarness(t)
	gate, entered := make(chan struct{}), make(chan struct{})
	h.model.scripts["late task"] = func(n int, req agent.ModelRequest) agent.ModelResponse {
		switch n {
		case 1:
			close(entered)
			<-gate
			return endTurn() // ends without another tool round
		case 2:
			return handback("got the late message")
		}
		return endTurn()
	}
	h.spawn(t, "late", "late task", true)
	<-entered
	if res := h.sendFromMain(t, "late", "late news"); res["success"] != true {
		t.Fatalf("send: %v", res)
	}
	close(gate)
	// Two reports: the first run's and the resumed run's (in either order).
	if a, b := h.waitDone(t, "late"), h.waitDone(t, "late"); !strings.Contains(a+b, "got the late message") {
		t.Fatalf("stranded resume: %q / %q", a, b)
	}
	if got := lastUserText(h.model.requests("late task")[1]); got != "late news" {
		t.Fatalf("stranded prompt: %q", got)
	}
}

// An agent the user stopped is not resumed by a message.
func TestSendMessageStoppedByUserNotResumed(t *testing.T) {
	h := newMsgHarness(t)
	tk := h.tool.Tasks.Create(subagent.Task{AgentType: "general-purpose", Name: "gone"})
	h.tool.Tasks.RegisterName("gone", tk.ID)
	h.tool.Tasks.Update(tk.ID, func(t *subagent.Task) { t.Status = subagent.StatusKilled; t.StoppedByUser = true })
	res := h.sendFromMain(t, "gone", "are you there")
	if res["success"] != false || res["message"] != `Agent "gone" was stopped by the user and was not resumed. Treat its work as cancelled; only start a new agent for it if the user explicitly asks.` {
		t.Fatalf("send: %v", res)
	}
	if got, _ := h.tool.Tasks.Get(tk.ID); len(got.PendingMessages) != 0 {
		t.Fatal("message queued for a user-stopped agent")
	}
}

func TestAgentNameDescriptionMentionsSendMessage(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	h := newHarness(nil)
	if err := json.Unmarshal(h.tool.InputSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["name"].Description; got != "Name for the spawned agent. Makes it addressable via SendMessage({to: name}) while running." {
		t.Fatalf("name description: %q", got)
	}
}
