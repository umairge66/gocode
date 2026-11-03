package agent

import "context"

// ModelCaller sends one request to the model and returns the assistant turn.
// provider.ModelCaller (OpenRouter / Anthropic / OpenAI) plugs in here; tests
// inject a fake. The caller is injectable so the loop stays independent
// of transport, retries, fallback and compaction.
type ModelCaller interface {
	CallModel(ctx context.Context, req ModelRequest) (ModelResponse, error)
}

// StreamHandler receives incremental deltas as the model streams one turn. Both
// fields are optional; a nil func is simply not called. A handler is invoked
// synchronously on the goroutine running the model call, in stream order, as
// each content_block_delta arrives. It is the optional live-output seam: when no
// handler is set the turn behaves exactly as before (silent accumulation to the
// final ModelResponse).
type StreamHandler struct {
	// OnText receives each incremental assistant text delta.
	OnText func(delta string)
	// OnThinking receives each incremental extended-thinking delta.
	OnThinking func(delta string)
	// OnToolUseStart is told each tool_use block as it begins streaming (its
	// id and tool name), before any of its input. A provider that does not
	// stream tool calls never calls it.
	OnToolUseStart func(id, name string)
	// OnToolInputDelta receives each partial-JSON fragment of the input of
	// the tool_use block id, in order. The fragments concatenate to the
	// block's final input.
	OnToolInputDelta func(id, partialJSON string)
}

// StreamingModelCaller is the optional streaming extension of ModelCaller. A
// model caller that can forward incremental deltas implements it; the Loop uses
// CallModelStream only when it has a non-nil StreamHandler AND its Model
// implements this interface. Any other model caller (or a nil handler) uses the
// plain CallModel path, so default behavior is unchanged.
type StreamingModelCaller interface {
	ModelCaller
	CallModelStream(ctx context.Context, req ModelRequest, h *StreamHandler) (ModelResponse, error)
}

// Loop drives the agent turn loop over an injectable model caller and tool
// dispatcher. Deep concerns the real loop carries — streaming UI, token-cost
// accounting, auto-compaction, model/refusal fallback, thinking-only and
// truncated-response recovery — are deliberately out of scope.
type Loop struct {
	// Model is the model caller (required).
	Model ModelCaller
	// Tools dispatches tool_use requests (required once the model asks for a
	// tool; a model that never emits tool_use needs no dispatcher).
	Tools ToolDispatcher
	// Hooks runs the PreToolUse / PostToolUse checkpoints (optional).
	Hooks PermissionHook

	// System is the system prompt passed on every model request. It is
	// marked as a cacheable prefix (ModelRequest.CacheSystemPrompt), so it
	// must stay byte-identical across the rounds of a run; text that may
	// change goes in SystemContext.
	System string
	// SystemContext is trailing system text sent after System but outside
	// its cached prefix (ModelRequest.SystemContext).
	SystemContext string
	// ToolSchemas advertises the available tools to the model.
	ToolSchemas []ToolSchema
	// RefreshTools, when set, is called before each model request with the
	// current tool list and returns the list to use from then on (the
	// argument itself when nothing changed), so tools that arrive mid-run (an
	// MCP server that finished connecting) can be loaded in the same run.
	RefreshTools func([]ToolSchema) []ToolSchema
	// MaxTokens caps each response (0 → caller/model default).
	MaxTokens int

	// MaxTurns bounds the number of model rounds per run. 0 means unbounded.
	MaxTurns int

	// StreamHandler, when non-nil, receives the model's incremental text/thinking
	// deltas during every round — but only if Model implements
	// StreamingModelCaller. It is the optional live-output seam; nil (the default)
	// leaves the loop's behavior unchanged. Each round still ends with the same
	// fully accumulated assistant turn.
	StreamHandler *StreamHandler

	// MidTurn, when set, is called between two tool rounds of a running turn —
	// after a round's tool results (and the turn-end hook) and before the next
	// model call — and returns queued messages to deliver now. Its blocks ride
	// in the same user turn as the tool results, so the model sees them at its
	// next call instead of only at the start of the next turn. It is not
	// called when the turn budget is spent (no further model call follows).
	MidTurn func(ctx context.Context) []ContentBlock

	// Budget, when set, is asked after every model response, once its cost
	// is accounted; a non-nil error means the session's spend cap is reached
	// and the run ends with ReasonMaxBudget (see budget.go). The error's text
	// is the user-facing notice.
	Budget func() error

	// Done, when set, is asked after each tool round; true ends the turn as
	// completed right there, without another model request. A subagent uses it
	// so its SubagentHandback call ends its run, as the hand-back contract
	// says, instead of calling the model once more after its report.
	Done func() bool

	// OnMessage, when set, observes each assistant turn the loop keeps and each
	// user turn carrying a round's tool results, as it is appended, in order.
	// The assistant event carries the response's stop reason and usage. It is
	// called synchronously on the loop's goroutine.
	OnMessage func(MessageEvent)

	// LoopGuard sets how the loop reacts to the model repeating the exact
	// same tool call several times in a row, and to equivalent calls that
	// keep returning the same result with no progress in between (see
	// LoopGuardConfig and noprogress.go). nil uses DefaultLoopGuard; a config
	// with both stages 0 turns both detectors off. The exact streak spans
	// every model round of a run and is ended by any different call; both
	// detectors start over on a new run and on a new user message (mid-turn
	// delivery included).
	LoopGuard *LoopGuardConfig

	// guard holds the current streak of identical calls.
	guard *loopGuard
	// progress holds the no-progress detector's per-call-key state.
	progress *noProgressGuard
}

// MessageEvent is one message the loop appended, as reported to OnMessage.
type MessageEvent struct {
	Message Message
	// StopReason and Usage describe the model response an assistant message
	// came from (empty / zero for a tool-result turn).
	StopReason string
	Usage      ModelUsage
}

// observe reports m to OnMessage when one is set.
func (l *Loop) observe(m Message, resp *ModelResponse) {
	if l.OnMessage == nil {
		return
	}
	ev := MessageEvent{Message: m}
	if resp != nil {
		ev.StopReason, ev.Usage = resp.StopReason, resp.Usage
	}
	l.OnMessage(ev)
}

// Run executes the turn loop starting from the initial message list and returns
// when the model stops. It appends every assistant turn and every tool-result
// turn to the running message list, which Result.Messages reports in full.
//
// Control flow:
//
//  1. Call the model with the current messages, system prompt and tool schemas.
//     Deferred tools (ToolSchema.Deferred) are withheld until ToolSearch loads
//     them: a change in the deferred set is first announced in the trailing
//     user turn (AnnounceDeferredTools), then SelectTools picks the tools the
//     request carries. A call error terminates the run with ReasonAPIError
//     (ReasonAborted when the context was cancelled mid-stream).
//  2. Normalize the assistant turn's tool inputs (unparseable JSON → {} plus an
//     InputValidationError result; see sanitizeToolInputs).
//  3. Collect its tool_use blocks (a follow-up is needed when there is at
//     least one). With none, the stop-handling runs, in this order:
//     - max_tokens: keep the partial turn, append MaxOutputTokensNudge and call
//     again, up to maxOutputTokensRecoveryLimit (3) times; then stop with
//     ReasonAPIError + StopError{max_output_tokens}.
//     - model_context_window_exceeded: stop with ReasonContextWindowExceeded so
//     the caller compacts (see that constant).
//     - tool_use with no tool_use block: drop the turn, append
//     MalformedToolUseNudge and retry ONCE; then ReasonMalformedToolUse.
//     - pause_turn: keep the turn and re-send to let the server continue, up to
//     maxPauseTurnResumes times.
//     - refusal: stop with ReasonRefusal + StopError{refusal}.
//     - end_turn / stop_sequence with no visible text: drop the turn, append
//     NoVisibleOutputNudge and retry ONCE; then complete.
//     - otherwise: ReasonCompleted.
//  4. Otherwise dispatch every tool_use (a deferred tool the request did not
//     carry is answered with the load-it-first InputValidationError instead;
//     concurrency-safe calls in parallel,
//     mutating calls serialized), append a single user turn carrying the
//     tool_result blocks, run the turn-end hook, reset the per-round recovery
//     budgets, then loop.
//  5. The turn budget is checked on each follow-up; exceeding it yields
//     ReasonMaxTurns.
//  6. Before dispatch, every call is checked against the repeated-call guard
//     (LoopGuard): from the warn threshold on, a note is appended to the
//     call's result; the call that reaches the stop threshold is not run
//     (nor are the calls after it in the same turn) and the run ends with
//     ReasonRepeatedToolCalls.
//  7. After dispatch, every executed call and its result feed the no-progress
//     detector (noprogress.go): from the warn threshold on, a result that
//     equals the previous results of equivalent calls, with no file change in
//     between, gets a note (unless it already carries an exact-repeat note);
//     at twice the stop threshold the run ends with ReasonRepeatedToolCalls
//     once the turn's results are recorded.
//  8. After every model response the spend cap is checked (Budget): once it
//     is reached the response is kept, its tool calls are answered as not
//     run, and the run ends with ReasonMaxBudget. It is checked again after
//     a tool round, whose tools (a subagent) may have spent, so no further
//     request is made once the cap is reached.
//
// On abort every tool_use in the history is answered (synthetic interrupted
// results), so the returned Messages are always a valid next request. A tool
// call can end the turn itself through AbortTurn (the user rejected its
// permission prompt without feedback): the calls of that round not yet run
// are answered with the rejection result, the round's results are followed
// by the "[Request interrupted by user for tool use]" marker, and the run
// returns ReasonAborted without another model request.
//
// Not handled here (owned elsewhere or out of scope): reactive/auto compaction
// inside the loop (the conversation layer compacts before each turn), model and
// refusal fallback chains, truncated-stream recovery (the provider surfaces a
// broken stream as a call error), and thinking-block resumption.
func (l *Loop) Run(ctx context.Context, initial []Message) Result {
	// The turn runs under its own cancellable context so a tool call can end
	// it (AbortTurn: a permission prompt rejected without feedback).
	ctx, endTurn := withTurnAbort(ctx)
	defer endTurn(nil)
	l.resetRepeats()
	messages := append([]Message(nil), initial...)
	turnCount := 1
	// Per-round recovery budgets, reset when a new round starts.
	var (
		maxOutputRecoveries int
		malformedRetried    bool
		noOutputNudged      bool
		pauseResumes        int
	)

	aborted := func(err error) Result {
		return Result{Reason: ReasonAborted, Messages: pairDanglingToolUses(messages), TurnCount: turnCount, Err: err}
	}

	for {
		if err := ctx.Err(); err != nil {
			return aborted(err)
		}

		// Deferred tools: announce any change in the deferred set, then send
		// the always-loaded tools plus those loaded so far.
		if l.RefreshTools != nil {
			l.ToolSchemas = l.RefreshTools(l.ToolSchemas)
		}
		messages = AnnounceDeferredTools(messages, l.ToolSchemas)
		tools, deferred := SelectTools(l.ToolSchemas, messages)
		// Prompt caching: tools + System form a stable prefix, and the
		// conversation carries rolling breakpoints, so every round after the
		// first reads what the previous round wrote. Providers without
		// explicit caching ignore both flags.
		resp, err := l.callModel(ctx, ModelRequest{
			Messages:          messages,
			System:            l.System,
			SystemContext:     l.SystemContext,
			Tools:             tools,
			MaxTokens:         l.MaxTokens,
			DeferredTools:     deferred,
			CacheSystemPrompt: l.System != "",
			CacheMessages:     true,
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return aborted(ctxErr)
			}
			return Result{Reason: ReasonAPIError, Messages: messages, TurnCount: turnCount, Err: err}
		}

		assistant, badInputs := sanitizeToolInputs(resp.Message)
		if assistant.Role == "" {
			assistant.Role = RoleAssistant
		}
		// Spend cap: the response that crossed it completes, then the run
		// stops before any of its tools run or another request is made.
		if l.Budget != nil {
			if spent := l.Budget(); spent != nil {
				return l.budgetStop(messages, assistant, &resp, turnCount, spent)
			}
		}
		toolUses := assistant.ToolUseBlocks()

		// --- no follow-up: stop handling --------------------------------------
		if len(toolUses) == 0 {
			switch resp.StopReason {
			case StopMaxTokens:
				if len(assistant.Content) > 0 {
					messages = append(messages, assistant)
					l.observe(assistant, &resp)
				}
				if maxOutputRecoveries < maxOutputTokensRecoveryLimit {
					maxOutputRecoveries++
					messages = appendUser(messages, TextBlock(MaxOutputTokensNudge))
					continue
				}
				return Result{Reason: ReasonAPIError, Messages: messages, TurnCount: turnCount,
					Err: &StopError{Kind: StopKindMaxOutputTokens, Message: maxOutputTokensMessage(l.MaxTokens)}}

			case StopModelContextExceeded:
				if len(assistant.Content) > 0 {
					messages = append(messages, assistant)
					l.observe(assistant, &resp)
				}
				return Result{Reason: ReasonContextWindowExceeded, Messages: messages, TurnCount: turnCount,
					Err: &StopError{Kind: StopKindContextWindow, Message: contextWindowMessage}}

			case StopToolUse:
				if !malformedRetried {
					malformedRetried = true
					messages = appendUser(messages, TextBlock(MalformedToolUseNudge))
					continue
				}
				if len(assistant.Content) > 0 {
					messages = append(messages, assistant)
					l.observe(assistant, &resp)
				}
				return Result{Reason: ReasonMalformedToolUse, Messages: messages, TurnCount: turnCount,
					Err: &StopError{Kind: StopKindMalformedToolUse, Message: malformedToolUseExhausted}}

			case StopPauseTurn:
				// The server paused a long-running server-tool turn: re-send
				// the conversation (with the paused turn) so it continues.
				messages = append(messages, withContent(assistant))
				l.observe(withContent(assistant), &resp)
				if pauseResumes < maxPauseTurnResumes {
					pauseResumes++
					continue
				}
				// Budget spent: end as for an unhandled stop reason.
				return Result{Reason: ReasonCompleted, Messages: messages, TurnCount: turnCount}

			case StopRefusal:
				if len(assistant.Content) > 0 {
					messages = append(messages, assistant)
					l.observe(assistant, &resp)
				}
				return Result{Reason: ReasonRefusal, Messages: messages, TurnCount: turnCount,
					Err: &StopError{Kind: StopKindRefusal, Message: refusalMessage}}

			case StopEndTurn, StopStopSequence:
				if !assistant.HasText() && !noOutputNudged {
					noOutputNudged = true
					messages = appendUser(messages, TextBlock(NoVisibleOutputNudge))
					continue
				}
			}
			// end_turn / stop_sequence / anything else → completed.
			messages = append(messages, withContent(assistant))
			l.observe(withContent(assistant), &resp)
			return Result{Reason: ReasonCompleted, Messages: messages, TurnCount: turnCount}
		}

		messages = append(messages, assistant)
		l.observe(assistant, &resp)

		// --- dispatch tools, append their results -----------------------------
		// The transcript so far rides on the dispatch context (the auto mode
		// classifier reads it, see transcript.go).
		verdict := l.checkRepeats(toolUses)
		runnable := toolUses
		if verdict.stopAt >= 0 {
			runnable = toolUses[:verdict.stopAt]
		}
		resultBlocks := l.dispatchTurn(WithTranscript(ctx, messages), runnable, badInputs, deferred)
		if verdict.stopAt >= 0 {
			resultBlocks = append(resultBlocks, ToolResultBlock(toolUses[verdict.stopAt].ID,
				repeatedCallNotRun(verdict.stopTool, verdict.stopCount), true))
			for _, c := range toolUses[verdict.stopAt+1:] {
				resultBlocks = append(resultBlocks, ToolResultBlock(c.ID, skippedAfterRepeatStop, true))
			}
		}
		var progress progressVerdict
		if ctx.Err() == nil {
			progress = l.checkProgress(runnable, resultBlocks, verdict.notes)
		}
		for i, note := range verdict.notes {
			resultBlocks[i] = appendNote(resultBlocks[i], note)
		}
		for i, note := range progress.notes {
			resultBlocks[i] = appendNote(resultBlocks[i], note)
		}
		if err := ctx.Err(); err != nil {
			messages = append(messages, UserMessage(append(resultBlocks, TextBlock(interruptedForToolUse))...))
			l.observe(messages[len(messages)-1], nil)
			return Result{Reason: ReasonAborted, Messages: messages, TurnCount: turnCount, Err: err}
		}
		messages = append(messages, UserMessage(resultBlocks...))
		l.observe(messages[len(messages)-1], nil)

		if verdict.stopAt >= 0 {
			return Result{Reason: ReasonRepeatedToolCalls, Messages: messages, TurnCount: turnCount,
				Err: &StopError{Kind: StopKindRepeatedToolCalls, Message: RepeatedCallsStopMessage(verdict.stopTool, verdict.stopCount)}}
		}
		if progress.stop {
			return Result{Reason: ReasonRepeatedToolCalls, Messages: messages, TurnCount: turnCount,
				Err: &StopError{Kind: StopKindRepeatedToolCalls, Message: NoProgressStopMessage(progress.stopTool, progress.stopCount)}}
		}

		if l.Done != nil && l.Done() {
			return Result{Reason: ReasonCompleted, Messages: messages, TurnCount: turnCount}
		}

		// --- turn-end hook (Stop / SubagentStop) ------------------------------
		if th, ok := l.Hooks.(TurnEndHook); ok {
			cont, extra := th.OnTurnEnd(ctx, messages)
			messages = append(messages, extra...)
			if !cont {
				return Result{Reason: ReasonStopHookPrevented, Messages: messages, TurnCount: turnCount}
			}
		}

		maxOutputRecoveries, malformedRetried, noOutputNudged, pauseResumes = 0, false, false, 0

		// --- spend cap: the round's tools (a subagent, say) may have spent it -
		if l.Budget != nil {
			if spent := l.Budget(); spent != nil {
				return Result{Reason: ReasonMaxBudget, Messages: messages, TurnCount: turnCount,
					Err: &StopError{Kind: StopKindMaxBudget, Message: spent.Error()}}
			}
		}

		// --- max-turns guard, then loop ---------------------------------------
		turnCount++
		if l.MaxTurns > 0 && turnCount > l.MaxTurns {
			return Result{Reason: ReasonMaxTurns, Messages: messages, TurnCount: turnCount}
		}

		// --- mid-turn delivery of queued messages -----------------------------
		if l.MidTurn != nil {
			if extra := l.MidTurn(ctx); len(extra) > 0 {
				l.resetRepeats()
				messages = appendToUserTurn(messages, extra)
			}
		}
	}
}

// appendToUserTurn adds blocks to the trailing user turn (the round's tool
// results), or as a new user turn when the history does not end with one.
// The trailing turn's content is copied, never appended in place.
func appendToUserTurn(messages []Message, blocks []ContentBlock) []Message {
	if n := len(messages); n > 0 && messages[n-1].Role == RoleUser {
		last := messages[n-1]
		content := make([]ContentBlock, 0, len(last.Content)+len(blocks))
		content = append(content, last.Content...)
		content = append(content, blocks...)
		last.Content = content
		out := append([]Message(nil), messages[:n-1]...)
		return append(out, last)
	}
	return append(messages, UserMessage(blocks...))
}

// callModel dispatches one model round. When the loop carries a StreamHandler
// and the Model supports streaming (StreamingModelCaller), it takes the
// streaming path so deltas reach the handler; otherwise it uses the plain,
// unchanged CallModel path. Either way the returned ModelResponse is the fully
// accumulated assistant turn.
func (l *Loop) callModel(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	if l.StreamHandler != nil {
		if sc, ok := l.Model.(StreamingModelCaller); ok {
			return sc.CallModelStream(ctx, req, l.StreamHandler)
		}
	}
	return l.Model.CallModel(ctx, req)
}

// TurnEndHook is an optional capability on a Loop's Hooks value: it runs after
// each tool-result turn and can halt the loop (preventContinuation) or feed
// extra messages (blockingErrors) before the next round.
type TurnEndHook interface {
	OnTurnEnd(ctx context.Context, messages []Message) (cont bool, extra []Message)
}
