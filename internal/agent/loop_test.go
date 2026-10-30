package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// scriptedModel returns a pre-programmed sequence of responses, one per call,
// recording the message list it was handed each time.
type scriptedModel struct {
	responses []ModelResponse
	calls     int
	seen      [][]Message
	err       error
}

func (m *scriptedModel) CallModel(_ context.Context, req ModelRequest) (ModelResponse, error) {
	m.seen = append(m.seen, append([]Message(nil), req.Messages...))
	if m.err != nil {
		return ModelResponse{}, m.err
	}
	if m.calls >= len(m.responses) {
		// Default to a clean end-turn if the script runs out.
		m.calls++
		return ModelResponse{Message: AssistantMessage(TextBlock("done")), StopReason: StopEndTurn}, nil
	}
	r := m.responses[m.calls]
	m.calls++
	return r, nil
}

// streamingModel is a fake StreamingModelCaller: it emits a scripted sequence of
// text deltas to the handler before returning the assembled final message. It
// also records whether the streaming path (vs. plain CallModel) was taken.
type streamingModel struct {
	deltas       []string
	streamedCall bool
	plainCall    bool
}

func (m *streamingModel) assembled() ModelResponse {
	var full string
	for _, d := range m.deltas {
		full += d
	}
	return ModelResponse{Message: AssistantMessage(TextBlock(full)), StopReason: StopEndTurn}
}

func (m *streamingModel) CallModel(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	m.plainCall = true
	return m.assembled(), nil
}

func (m *streamingModel) CallModelStream(_ context.Context, _ ModelRequest, h *StreamHandler) (ModelResponse, error) {
	m.streamedCall = true
	if h != nil && h.OnText != nil {
		for _, d := range m.deltas {
			h.OnText(d)
		}
	}
	return m.assembled(), nil
}

// TestLoopForwardsStreamHandler confirms the loop uses the streaming path when a
// StreamHandler is set and the Model implements StreamingModelCaller: the
// handler sees every delta, their concatenation equals the final assistant text,
// and the loop still terminates normally.
func TestLoopForwardsStreamHandler(t *testing.T) {
	model := &streamingModel{deltas: []string{"Hel", "lo, ", "world"}}

	var got []string
	loop := &Loop{
		Model:         model,
		StreamHandler: &StreamHandler{OnText: func(d string) { got = append(got, d) }},
	}

	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})

	if !model.streamedCall || model.plainCall {
		t.Fatalf("streaming path not taken: streamed=%v plain=%v", model.streamedCall, model.plainCall)
	}
	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want completed", res.Reason)
	}
	want := "Hello, world"
	if j := joinStr(got); j != want {
		t.Errorf("concatenated deltas = %q, want %q", j, want)
	}
	last := res.Messages[len(res.Messages)-1]
	if messageTextForTest(last) != want {
		t.Errorf("final assistant text = %q, want %q", messageTextForTest(last), want)
	}
}

// TestLoopNilStreamHandlerUsesPlainPath confirms that without a StreamHandler the
// loop uses the plain CallModel path even when the Model can stream.
func TestLoopNilStreamHandlerUsesPlainPath(t *testing.T) {
	model := &streamingModel{deltas: []string{"a", "b"}}
	loop := &Loop{Model: model} // no StreamHandler
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})
	if model.streamedCall || !model.plainCall {
		t.Fatalf("plain path not taken: streamed=%v plain=%v", model.streamedCall, model.plainCall)
	}
	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want completed", res.Reason)
	}
}

func joinStr(parts []string) string {
	var s string
	for _, p := range parts {
		s += p
	}
	return s
}

// recordingDispatcher records each call and returns a fixed result (optionally
// keyed by tool name).
type recordingDispatcher struct {
	mu      sync.Mutex
	calls   []ToolUse
	results map[string]ToolResult
	def     ToolResult
	err     error
}

func (d *recordingDispatcher) Dispatch(_ context.Context, call ToolUse) (ToolResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
	if d.err != nil {
		return ToolResult{}, d.err
	}
	if r, ok := d.results[call.Name]; ok {
		return r, nil
	}
	return d.def, nil
}

func toolUseResp(id, name, input string) ModelResponse {
	return ModelResponse{
		Message:    AssistantMessage(ToolUseBlock(id, name, json.RawMessage(input))),
		StopReason: StopToolUse,
	}
}

func endTurnResp(text string) ModelResponse {
	return ModelResponse{Message: AssistantMessage(TextBlock(text)), StopReason: StopEndTurn}
}

// TestLoopDispatchesToolThenStops is the core happy path: the model asks for a
// tool, the loop dispatches it, appends a tool_result user turn, calls the
// model again, and terminates on end_turn.
func TestLoopDispatchesToolThenStops(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		toolUseResp("toolu_1", "Read", `{"path":"/x"}`),
		endTurnResp("all set"),
	}}
	tools := &recordingDispatcher{def: ToolResult{Content: "file contents"}}

	loop := &Loop{Model: model, Tools: tools}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("read /x"))})

	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want %q (err=%v)", res.Reason, ReasonCompleted, res.Err)
	}
	if model.calls != 2 {
		t.Fatalf("model called %d times, want 2", model.calls)
	}
	if len(tools.calls) != 1 || tools.calls[0].Name != "Read" || tools.calls[0].ID != "toolu_1" {
		t.Fatalf("dispatcher calls = %+v, want one Read/toolu_1", tools.calls)
	}

	// Messages: initial user, assistant tool_use, user tool_result, assistant text.
	if len(res.Messages) != 4 {
		t.Fatalf("messages len = %d, want 4: %+v", len(res.Messages), res.Messages)
	}
	trMsg := res.Messages[2]
	if trMsg.Role != RoleUser || len(trMsg.Content) != 1 {
		t.Fatalf("message[2] = %+v, want a user turn with one block", trMsg)
	}
	tr := trMsg.Content[0]
	if tr.Type != BlockToolResult || tr.ToolUseID != "toolu_1" || tr.IsError {
		t.Fatalf("tool_result block = %+v, want success tool_result for toolu_1", tr)
	}
	if tr.Content != "file contents" {
		t.Fatalf("tool_result content = %q, want %q", tr.Content, "file contents")
	}

	// The second model call must have seen the tool_result appended.
	if len(model.seen) != 2 {
		t.Fatalf("recorded %d model calls, want 2", len(model.seen))
	}
	second := model.seen[1]
	if len(second) != 3 || second[2].Content[0].Type != BlockToolResult {
		t.Fatalf("second model call did not include the tool_result turn: %+v", second)
	}
}

// TestLoopToolErrorPropagates checks that a tool reporting IsError produces an
// is_error tool_result (wrapped in <tool_use_error>) that is sent back to the
// model, and the loop still proceeds to terminate.
func TestLoopToolErrorPropagates(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		toolUseResp("toolu_err", "Bash", `{"command":"nope"}`),
		endTurnResp("acknowledged"),
	}}
	tools := &recordingDispatcher{def: ToolResult{Content: "command failed: exit 1", IsError: true}}

	loop := &Loop{Model: model, Tools: tools}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("run it"))})

	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonCompleted)
	}
	tr := res.Messages[2].Content[0]
	if tr.Type != BlockToolResult || !tr.IsError {
		t.Fatalf("expected is_error tool_result, got %+v", tr)
	}
	want := toolUseErrorOpen + "command failed: exit 1" + toolUseErrorClose
	if tr.Content != want {
		t.Fatalf("error tool_result content = %q, want %q", tr.Content, want)
	}
}

// TestLoopDispatchErrorBecomesToolResult checks that a loop-fatal dispatch
// error is still surfaced to the model as an is_error tool_result rather than
// aborting the whole run.
func TestLoopDispatchErrorBecomesToolResult(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		toolUseResp("toolu_x", "Write", `{"path":"/x"}`),
		endTurnResp("ok"),
	}}
	tools := &recordingDispatcher{err: errors.New("disk offline")}

	loop := &Loop{Model: model, Tools: tools}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("write"))})

	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonCompleted)
	}
	tr := res.Messages[2].Content[0]
	if !tr.IsError || tr.Content != toolUseErrorOpen+"Error calling tool (Write): disk offline"+toolUseErrorClose {
		t.Fatalf("dispatch error not surfaced correctly: %+v", tr)
	}
}

// TestLoopMaxTurnsGuard checks that a model that keeps asking for tools is
// stopped once the turn budget is exhausted.
func TestLoopMaxTurnsGuard(t *testing.T) {
	// A model that always asks for another tool.
	model := &loopingToolModel{}
	tools := &recordingDispatcher{def: ToolResult{Content: "ok"}}

	loop := &Loop{Model: model, Tools: tools, MaxTurns: 3}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})

	if res.Reason != ReasonMaxTurns {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonMaxTurns)
	}
	if res.TurnCount != 4 { // turnCount starts at 1, incremented each follow-up; 4 > MaxTurns(3)
		t.Fatalf("turnCount = %d, want 4", res.TurnCount)
	}
	// 3 rounds of dispatch happened before the budget tripped.
	if len(tools.calls) != 3 {
		t.Fatalf("dispatcher calls = %d, want 3", len(tools.calls))
	}
}

type loopingToolModel struct{ n int }

func (m *loopingToolModel) CallModel(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	m.n++
	return toolUseResp("toolu_loop", "Read", `{}`), nil
}

// TestLoopAPIError checks that a model-call failure terminates with
// ReasonAPIError and carries the underlying error.
func TestLoopAPIError(t *testing.T) {
	boom := errors.New("502 bad gateway")
	model := &scriptedModel{err: boom}
	loop := &Loop{Model: model, Tools: &recordingDispatcher{}}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})

	if res.Reason != ReasonAPIError {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonAPIError)
	}
	if !errors.Is(res.Err, boom) {
		t.Fatalf("err = %v, want %v", res.Err, boom)
	}
}

// TestLoopMalformedToolUse checks the stop_reason=="tool_use" with no
// tool_use block case once the single retry is also spent.
func TestLoopMalformedToolUse(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		{Message: AssistantMessage(TextBlock("I will call a tool")), StopReason: StopToolUse},
		{Message: AssistantMessage(TextBlock("I will call a tool")), StopReason: StopToolUse},
	}}
	loop := &Loop{Model: model, Tools: &recordingDispatcher{}}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("x"))})

	if res.Reason != ReasonMalformedToolUse {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonMalformedToolUse)
	}
}

// TestLoopImmediateEndTurn checks the simplest terminal path: the model answers
// without any tool call.
func TestLoopImmediateEndTurn(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{endTurnResp("hello")}}
	loop := &Loop{Model: model, Tools: &recordingDispatcher{}}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})

	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonCompleted)
	}
	if model.calls != 1 {
		t.Fatalf("model called %d times, want 1", model.calls)
	}
}

// messageTextForTest concatenates the text blocks of m.
func messageTextForTest(m Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// TestLoopDoneEndsAfterToolRound checks Done ends the turn as completed right
// after the tool round that satisfied it, without another model request.
func TestLoopDoneEndsAfterToolRound(t *testing.T) {
	model := &loopingToolModel{}
	tools := &recordingDispatcher{}
	loop := &Loop{Model: model, Tools: tools, Done: func() bool { return len(tools.calls) >= 2 }}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})
	if res.Reason != ReasonCompleted || model.n != 2 || len(tools.calls) != 2 {
		t.Fatalf("reason=%q model calls=%d tool calls=%d, want completed after 2 rounds", res.Reason, model.n, len(tools.calls))
	}
	if last := res.Messages[len(res.Messages)-1]; last.Role != RoleUser {
		t.Fatalf("the turn should end on the tool results, got %q", last.Role)
	}
}
