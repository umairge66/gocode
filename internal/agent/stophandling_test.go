package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func malformedResp() ModelResponse {
	return ModelResponse{Message: AssistantMessage(TextBlock("calling a tool")), StopReason: StopToolUse}
}

func stopResp(stop string, blocks ...ContentBlock) ModelResponse {
	return ModelResponse{Message: AssistantMessage(blocks...), StopReason: stop}
}

// lastUserText returns the text of the final text block of the last user turn.
func lastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != RoleUser {
			continue
		}
		for j := len(msgs[i].Content) - 1; j >= 0; j-- {
			if msgs[i].Content[j].Type == BlockText {
				return msgs[i].Content[j].Text
			}
		}
		return ""
	}
	return ""
}

// assertPaired fails unless every tool_use in msgs is answered by a
// tool_result in the immediately following user turn, and the history
// marshals as JSON (i.e. is a sendable request).
func assertPaired(t *testing.T, msgs []Message) {
	t.Helper()
	for i, m := range msgs {
		uses := m.ToolUseBlocks()
		if len(uses) == 0 {
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1].Role != RoleUser {
			t.Fatalf("tool_use turn %d has no following user turn", i)
		}
		got := map[string]bool{}
		for _, b := range msgs[i+1].Content {
			if b.Type == BlockToolResult {
				got[b.ToolUseID] = true
			}
		}
		for _, u := range uses {
			if !got[u.ID] {
				t.Fatalf("tool_use %s has no tool_result", u.ID)
			}
		}
	}
	if _, err := json.Marshal(msgs); err != nil {
		t.Fatalf("history does not marshal: %v", err)
	}
}

func stopErrKind(t *testing.T, err error) string {
	t.Helper()
	var se *StopError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *StopError", err, err)
	}
	return se.Kind
}

func TestMalformedToolUseRetriesOnceThenSucceeds(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		malformedResp(),
		toolUseResp("toolu_1", "Read", `{"path":"/x"}`),
		endTurnResp("done"),
	}}
	tools := &recordingDispatcher{def: ToolResult{Content: "ok"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})

	if res.Reason != ReasonCompleted || res.Err != nil {
		t.Fatalf("reason=%q err=%v, want completed", res.Reason, res.Err)
	}
	if model.calls != 3 || len(tools.calls) != 1 {
		t.Fatalf("model calls=%d tool calls=%d, want 3/1", model.calls, len(tools.calls))
	}
	// The retry request: the malformed turn was dropped and the nudge rides in
	// the (merged) trailing user turn.
	retry := model.seen[1]
	if len(retry) != 1 || retry[0].Role != RoleUser {
		t.Fatalf("retry request = %+v, want the single merged user turn", retry)
	}
	if got := lastUserText(retry); got != MalformedToolUseNudge {
		t.Fatalf("retry nudge = %q, want %q", got, MalformedToolUseNudge)
	}
	if retry[0].Content[0].Text != "go" {
		t.Fatalf("original prompt lost: %+v", retry[0])
	}
	assertPaired(t, res.Messages)
}

func TestMalformedToolUseTwiceFails(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{malformedResp(), malformedResp(), endTurnResp("never")}}
	res := (&Loop{Model: model, Tools: &recordingDispatcher{}}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonMalformedToolUse {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonMalformedToolUse)
	}
	if model.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (one retry only)", model.calls)
	}
	if k := stopErrKind(t, res.Err); k != StopKindMalformedToolUse {
		t.Fatalf("kind = %q", k)
	}
	if res.Err.Error() != "The model's tool call could not be parsed (retry also failed)." {
		t.Fatalf("message = %q", res.Err.Error())
	}
}

func TestMalformedRetryBudgetResetsAfterToolRound(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		malformedResp(),
		toolUseResp("toolu_1", "Read", `{}`),
		malformedResp(),
		endTurnResp("done"),
	}}
	res := (&Loop{Model: model, Tools: &recordingDispatcher{def: ToolResult{Content: "ok"}}}).
		Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted || model.calls != 4 {
		t.Fatalf("reason=%q calls=%d, want completed/4", res.Reason, model.calls)
	}
}

func TestInvalidToolInputJSONBecomesValidationError(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		toolUseResp("toolu_bad", "Write", `{"path":"C:\x","content":"trunc`),
		endTurnResp("fixed"),
	}}
	tools := &recordingDispatcher{def: ToolResult{Content: "ok"}}
	res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("w"))})

	if res.Reason != ReasonCompleted {
		t.Fatalf("reason = %q (err=%v)", res.Reason, res.Err)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("dispatcher must not run an unparseable call: %+v", tools.calls)
	}
	tu := res.Messages[1].Content[0]
	if string(tu.Input) != `{}` {
		t.Fatalf("stored input = %s, want {}", tu.Input)
	}
	tr := res.Messages[2].Content[0]
	if !tr.IsError || !strings.HasPrefix(tr.Content, "<tool_use_error>InputValidationError: Write was called with input that could not be parsed as JSON. You sent (first 31 of 31 bytes): ") {
		t.Fatalf("tool_result = %+v", tr)
	}
	if !strings.Contains(tr.Content, `(use / or \\)`) || !strings.HasSuffix(tr.Content, "Retry with valid JSON.</tool_use_error>") {
		t.Fatalf("tool_result tail = %q", tr.Content)
	}
	assertPaired(t, res.Messages)
}

// unknownToolDispatcher mimics toolrun: an unregistered name is an is_error
// result, not a Go error.
type unknownToolDispatcher struct{}

func (unknownToolDispatcher) Dispatch(_ context.Context, c ToolUse) (ToolResult, error) {
	return ToolResult{Content: "Error: No such tool available: " + c.Name, IsError: true}, nil
}

func TestUnknownToolNameFedBackAsError(t *testing.T) {
	for name, tools := range map[string]ToolDispatcher{"dispatcher": unknownToolDispatcher{}, "nil": nil} {
		t.Run(name, func(t *testing.T) {
			model := &scriptedModel{responses: []ModelResponse{
				toolUseResp("toolu_u", "NoSuchTool", `{}`),
				endTurnResp("sorry"),
			}}
			res := (&Loop{Model: model, Tools: tools}).Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
			if res.Reason != ReasonCompleted {
				t.Fatalf("reason = %q", res.Reason)
			}
			tr := res.Messages[2].Content[0]
			if !tr.IsError || tr.Content != "<tool_use_error>Error: No such tool available: NoSuchTool</tool_use_error>" {
				t.Fatalf("tool_result = %+v", tr)
			}
			if lastSeen := model.seen[1]; lastSeen[len(lastSeen)-1].Content[0].ToolUseID != "toolu_u" {
				t.Fatalf("error result not fed back to the model")
			}
		})
	}
}

func TestMaxTokensNudgesContinuation(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		stopResp(StopMaxTokens, TextBlock("part one")),
		endTurnResp("part two"),
	}}
	res := (&Loop{Model: model, MaxTokens: 100}).Run(context.Background(), []Message{UserMessage(TextBlock("write"))})
	if res.Reason != ReasonCompleted || model.calls != 2 {
		t.Fatalf("reason=%q calls=%d", res.Reason, model.calls)
	}
	second := model.seen[1]
	if len(second) != 3 || messageTextForTest(second[1]) != "part one" || lastUserText(second) != MaxOutputTokensNudge {
		t.Fatalf("continuation request = %+v", second)
	}
}

func TestMaxTokensRecoveryExhausted(t *testing.T) {
	var rs []ModelResponse
	for i := 0; i < 6; i++ {
		rs = append(rs, stopResp(StopMaxTokens, TextBlock("more")))
	}
	model := &scriptedModel{responses: rs}
	res := (&Loop{Model: model, MaxTokens: 512}).Run(context.Background(), []Message{UserMessage(TextBlock("write"))})
	if res.Reason != ReasonAPIError {
		t.Fatalf("reason = %q", res.Reason)
	}
	if model.calls != maxOutputTokensRecoveryLimit+1 {
		t.Fatalf("calls = %d, want %d", model.calls, maxOutputTokensRecoveryLimit+1)
	}
	if k := stopErrKind(t, res.Err); k != StopKindMaxOutputTokens {
		t.Fatalf("kind = %q", k)
	}
	if res.Err.Error() != "API Error: gocode's response exceeded the 512 output token maximum." {
		t.Fatalf("message = %q", res.Err.Error())
	}
	if last := res.Messages[len(res.Messages)-1]; last.Role != RoleAssistant {
		t.Fatalf("history should end with the last partial turn, got %+v", last)
	}
}

func TestPauseTurnIsResent(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		stopResp(StopPauseTurn, TextBlock("searching…")),
		endTurnResp("found it"),
	}}
	res := (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("search"))})
	if res.Reason != ReasonCompleted || model.calls != 2 {
		t.Fatalf("reason=%q calls=%d", res.Reason, model.calls)
	}
	second := model.seen[1]
	if len(second) != 2 || second[1].Role != RoleAssistant || messageTextForTest(second[1]) != "searching…" {
		t.Fatalf("resume request = %+v, want the paused turn re-sent", second)
	}
}

func TestPauseTurnBounded(t *testing.T) {
	var rs []ModelResponse
	for i := 0; i < 50; i++ {
		rs = append(rs, stopResp(StopPauseTurn, TextBlock("…")))
	}
	model := &scriptedModel{responses: rs}
	res := (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("s"))})
	if res.Reason != ReasonCompleted || model.calls != maxPauseTurnResumes+1 {
		t.Fatalf("reason=%q calls=%d", res.Reason, model.calls)
	}
}

func TestRefusalIsCleanStop(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{stopResp(StopRefusal, TextBlock("I can't"))}}
	res := (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
	if res.Reason != ReasonRefusal || model.calls != 1 {
		t.Fatalf("reason=%q calls=%d", res.Reason, model.calls)
	}
	if k := stopErrKind(t, res.Err); k != StopKindRefusal {
		t.Fatalf("kind = %q", k)
	}
	if res.Err.Error() != "API Error: gocode can't help with this. Start a new session to continue." {
		t.Fatalf("message = %q", res.Err.Error())
	}
}

func TestRefusalWithNoContentKeepsHistoryValid(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{stopResp(StopRefusal)}}
	res := (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
	if res.Reason != ReasonRefusal || len(res.Messages) != 1 {
		t.Fatalf("reason=%q messages=%+v", res.Reason, res.Messages)
	}
}

func TestContextWindowExceededSignalsCaller(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{stopResp(StopModelContextExceeded, TextBlock("par"))}}
	res := (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
	if res.Reason != ReasonContextWindowExceeded || model.calls != 1 {
		t.Fatalf("reason=%q calls=%d", res.Reason, model.calls)
	}
	if k := stopErrKind(t, res.Err); k != StopKindContextWindow {
		t.Fatalf("kind = %q", k)
	}
}

func TestEmptyResponseNudgedOnce(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		stopResp(StopEndTurn), // no content at all
		endTurnResp("visible answer"),
	}}
	res := (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
	if res.Reason != ReasonCompleted || model.calls != 2 {
		t.Fatalf("reason=%q calls=%d", res.Reason, model.calls)
	}
	second := model.seen[1]
	if len(second) != 1 || lastUserText(second) != NoVisibleOutputNudge {
		t.Fatalf("nudge request = %+v", second)
	}
	if got := messageTextForTest(res.Messages[len(res.Messages)-1]); got != "visible answer" {
		t.Fatalf("final text = %q", got)
	}
}

func TestEmptyResponseTwiceCompletesWithPlaceholder(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		stopResp(StopEndTurn),
		stopResp(StopEndTurn, ContentBlock{Type: BlockThinking, Thinking: "hmm"}),
		endTurnResp("never"),
	}}
	res := (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
	if res.Reason != ReasonCompleted || model.calls != 2 {
		t.Fatalf("reason=%q calls=%d", res.Reason, model.calls)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != RoleAssistant || len(last.Content) == 0 {
		t.Fatalf("last = %+v", last)
	}

	// A fully empty second response gets the "(no content)" placeholder.
	model = &scriptedModel{responses: []ModelResponse{stopResp(StopEndTurn), stopResp(StopEndTurn)}}
	res = (&Loop{Model: model}).Run(context.Background(), []Message{UserMessage(TextBlock("x"))})
	if got := messageTextForTest(res.Messages[len(res.Messages)-1]); got != "(no content)" {
		t.Fatalf("placeholder = %q", got)
	}
}

// cancellingDispatcher cancels the run while executing its first call.
type cancellingDispatcher struct {
	cancel context.CancelFunc
	calls  int
}

func (d *cancellingDispatcher) Dispatch(_ context.Context, c ToolUse) (ToolResult, error) {
	d.calls++
	d.cancel()
	return ToolResult{Content: "first finished"}, nil
}

func TestAbortMidToolsPairsEveryToolUse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &scriptedModel{responses: []ModelResponse{{
		Message: AssistantMessage(
			ToolUseBlock("t1", "Bash", json.RawMessage(`{"command":"a"}`)),
			ToolUseBlock("t2", "Bash", json.RawMessage(`{"command":"b"}`)),
			ToolUseBlock("t3", "Bash", json.RawMessage(`{"command":"c"}`)),
		),
		StopReason: StopToolUse,
	}}}
	tools := &cancellingDispatcher{cancel: cancel}
	res := (&Loop{Model: model, Tools: tools}).Run(ctx, []Message{UserMessage(TextBlock("go"))})

	if res.Reason != ReasonAborted {
		t.Fatalf("reason = %q", res.Reason)
	}
	if tools.calls != 1 {
		t.Fatalf("dispatcher ran %d calls after abort, want 1", tools.calls)
	}
	assertPaired(t, res.Messages)
	results := res.Messages[2].Content
	if results[0].Content != "first finished" || results[0].IsError {
		t.Fatalf("completed result lost: %+v", results[0])
	}
	for _, r := range results[1:3] {
		if !r.IsError || r.Content != toolUseInterruptedResult {
			t.Fatalf("synthetic result = %+v", r)
		}
	}
	if lastUserText(res.Messages) != "[Request interrupted by user for tool use]" {
		t.Fatalf("missing interrupt marker: %+v", res.Messages[2])
	}
}

// abortingModel cancels the context and fails the call, like a stream torn
// down by the user.
type abortingModel struct{ cancel context.CancelFunc }

func (m abortingModel) CallModel(ctx context.Context, _ ModelRequest) (ModelResponse, error) {
	m.cancel()
	return ModelResponse{}, ctx.Err()
}

func TestAbortDuringModelCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := (&Loop{Model: abortingModel{cancel}}).Run(ctx, []Message{UserMessage(TextBlock("x"))})
	if res.Reason != ReasonAborted {
		t.Fatalf("reason = %q, want aborted", res.Reason)
	}
}

func TestAbortPairsDanglingInitialToolUse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	initial := []Message{
		UserMessage(TextBlock("x")),
		AssistantMessage(ToolUseBlock("t9", "Read", json.RawMessage(`{}`))),
	}
	res := (&Loop{Model: &scriptedModel{}}).Run(ctx, initial)
	if res.Reason != ReasonAborted {
		t.Fatalf("reason = %q", res.Reason)
	}
	assertPaired(t, res.Messages)
}

func TestSanitizeToolInputsDoesNotMutateResponse(t *testing.T) {
	orig := AssistantMessage(ToolUseBlock("a", "X", json.RawMessage(`{bad`)), ToolUseBlock("b", "Y", nil))
	got, bad := sanitizeToolInputs(orig)
	if string(orig.Content[0].Input) != `{bad` {
		t.Fatalf("original mutated")
	}
	if string(got.Content[0].Input) != `{}` || string(got.Content[1].Input) != `{}` || bad["a"] != `{bad` || len(bad) != 1 {
		t.Fatalf("got=%+v bad=%v", got, bad)
	}
}
