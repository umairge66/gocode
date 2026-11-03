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

// StopKindMaxBudget: the session reached its spend cap (Loop.Budget).
const StopKindMaxBudget = "max_budget_usd"

// budgetToolNotRun answers the tool calls of the response that reached the
// spend cap.
const budgetToolNotRun = "Not run: the session reached its spend cap, so the turn stopped."

// budgetStop builds the ReasonMaxBudget result for the response assistant
// (already sanitized): the response is kept, its tool calls are answered as
// not run, and Result.Err is a *StopError carrying the budget's message.
func (l *Loop) budgetStop(messages []Message, assistant Message, resp *ModelResponse, turnCount int, spent error) Result {
	kept := withContent(assistant)
	messages = append(messages, kept)
	l.observe(kept, resp)
	var results []ContentBlock
	for _, b := range kept.ToolUseBlocks() {
		results = append(results, ToolResultBlock(b.ID, budgetToolNotRun, true))
	}
	if len(results) > 0 {
		messages = append(messages, UserMessage(results...))
		l.observe(messages[len(messages)-1], nil)
	}
	return Result{Reason: ReasonMaxBudget, Messages: messages, TurnCount: turnCount,
		Err: &StopError{Kind: StopKindMaxBudget, Message: spent.Error()}}
}
