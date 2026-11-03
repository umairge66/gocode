package agent

// Spend cap (Loop.Budget).
//
// The loop asks the budget after every model response, once the response
// (and its cost) has been accounted. When the budget reports it is spent the
// run ends right there with ReasonMaxBudget: the response that crossed the
// cap is kept in the history, but none of its tool calls run (each is
// answered with budgetToolNotRun so the history stays a valid request) and no
// further model request is made. A subagent's spend counts toward the same
// budget when its caller records into the same ledger, so the check after
// the parent's next response sees it.
//
// The one exception is a call Loop.RunAtBudget accepts: a subagent's
// SubagentHandback still runs, so the report the crossing response hands
// back reaches its caller. When that ends the run (Loop.Done), the run
// completes as it would have without the cap.

import "context"

// StopKindMaxBudget: the session reached its spend cap (Loop.Budget).
const StopKindMaxBudget = "max_budget_usd"

// budgetToolNotRun answers the tool calls of the response that reached the
// spend cap.
const budgetToolNotRun = "Not run: the session reached its spend cap, so the turn stopped."

// budgetStop builds the ReasonMaxBudget result for the response assistant
// (already sanitized): the response is kept, its tool calls are answered as
// not run — except those RunAtBudget accepts, which run — and Result.Err is a
// *StopError carrying the budget's message.
func (l *Loop) budgetStop(ctx context.Context, messages []Message, assistant Message, resp *ModelResponse, turnCount int, spent error, badInputs map[string]string, deferred []ToolSchema) Result {
	kept := withContent(assistant)
	messages = append(messages, kept)
	l.observe(kept, resp)
	calls := kept.ToolUseBlocks()
	results := make([]ContentBlock, len(calls))
	var run []ContentBlock
	var runIdx []int
	for i, b := range calls {
		if l.RunAtBudget != nil && l.RunAtBudget(toToolUse(b)) {
			run = append(run, b)
			runIdx = append(runIdx, i)
			continue
		}
		results[i] = ToolResultBlock(b.ID, budgetToolNotRun, true)
	}
	if len(run) > 0 {
		ran := l.dispatchTurn(WithTranscript(ctx, messages), run, badInputs, deferred)
		for j, i := range runIdx {
			results[i] = ran[j]
		}
	}
	if len(results) > 0 {
		messages = append(messages, UserMessage(results...))
		l.observe(messages[len(messages)-1], nil)
	}
	if len(run) > 0 && l.Done != nil && l.Done() {
		return Result{Reason: ReasonCompleted, Messages: messages, TurnCount: turnCount}
	}
	return Result{Reason: ReasonMaxBudget, Messages: messages, TurnCount: turnCount,
		Err: &StopError{Kind: StopKindMaxBudget, Message: spent.Error()}}
}
