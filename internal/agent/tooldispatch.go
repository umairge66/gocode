package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ToolUse is one tool request extracted from an assistant turn.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is a dispatched tool's outcome before it is shaped into a
// tool_result content block.
type ToolResult struct {
	Content string
	IsError bool
	// Images, when non-empty, are image content blocks the tool produced
	// alongside its text (e.g. a Read of an image or PDF). They are carried onto
	// the tool_result content block and encoded per provider. Empty (the
	// default) means a text-only result, so every existing construction site is
	// unchanged.
	Images []ToolResultImage
	// LoadedTools, on a ToolSearch result, names the deferred tools whose
	// definitions the result loads (carried onto the tool_result block).
	LoadedTools []string
	// PermissionDenied marks an is_error result for a call the permission
	// check refused: the tool never ran, so it is not a tool failure.
	PermissionDenied bool
}

// ToolDispatcher runs one tool by name with the given JSON input and returns
// its result. internal/tools/* implementations plug in here later; tests
// inject a fake.
type ToolDispatcher interface {
	// Dispatch executes the tool. A dispatcher reports tool-level failure by
	// returning ToolResult{IsError:true}; it should reserve the error return
	// for the loop-fatal case where no result can be produced at all.
	Dispatch(ctx context.Context, call ToolUse) (ToolResult, error)
}

// ConcurrencyClassifier is an optional ToolDispatcher capability: it reports
// whether a given call is safe to run concurrently with other concurrency-safe
// calls (read-only tools are; mutating tools are not).
type ConcurrencyClassifier interface {
	IsConcurrencySafe(call ToolUse) bool
}

// PermissionDecision is the verdict of a PreToolUse / permission checkpoint.
type PermissionDecision struct {
	Behavior string
	// Message explains a deny/ask (becomes the <tool_use_error> body).
	Message string
}

// Permission behaviors.
const (
	BehaviorAllow = "allow"
	BehaviorDeny  = "deny"
	BehaviorAsk   = "ask"
)

// PermissionHook models the PreToolUse / PostToolUse checkpoints the runner
// calls around every tool execution. It is optional on a Loop.
type PermissionHook interface {
	// PreToolUse decides whether the call may proceed. Returning a non-nil
	// error aborts the run.
	PreToolUse(ctx context.Context, call ToolUse) (PermissionDecision, error)
	// PostToolUse observes a completed (allowed) call's result.
	PostToolUse(ctx context.Context, call ToolUse, result ToolResult)
}

// ToolFailureHook is an optional PermissionHook capability: it observes a call
// whose tool failed (an is_error result that is not a permission denial), in
// place of PostToolUse.
type ToolFailureHook interface {
	PostToolUseFailure(ctx context.Context, call ToolUse, result ToolResult)
}

// dispatchTurn runs every tool_use block of an assistant turn and returns the
// matching tool_result blocks, in the tool_use order.
//
// Parallelism: a maximal run of
// consecutive concurrency-safe calls executes together; a non-concurrency-safe
// call flushes any pending safe batch and then runs alone. Result order follows
// the tool_use order regardless of completion order (the executor keys results
// by tool_use id).
//
// badInputs maps a tool_use id to the raw input text that could not be parsed
// as JSON (see sanitizeToolInputs); such a call is answered with an
// InputValidationError without being dispatched. notLoaded are the deferred
// tools the request did not define (see SelectTools); a call to one is
// answered with an InputValidationError telling the model to load it with
// ToolSearch, without being dispatched.
func (l *Loop) dispatchTurn(ctx context.Context, calls []ContentBlock, badInputs map[string]string, notLoaded []ToolSchema) []ContentBlock {
	results := make([]ContentBlock, len(calls))
	var pending []ContentBlock
	var pendingIdx []int
	unloaded := make(map[string]bool, len(notLoaded))
	for _, t := range notLoaded {
		unloaded[t.Name] = true
	}
	for i, c := range calls {
		if raw, ok := badInputs[c.ID]; ok {
			results[i] = ToolResultBlock(c.ID, unparsedInputError(c.Name, raw), true)
			continue
		}
		if unloaded[c.Name] {
			// A deferred tool whose definition the request did not carry:
			// it is not run, and the model is told to load it first.
			results[i] = ToolResultBlock(c.ID, notLoadedError(c.Name), true)
			continue
		}
		pending = append(pending, c)
		pendingIdx = append(pendingIdx, i)
	}
	sub := l.dispatchCalls(ctx, pending)
	for j, i := range pendingIdx {
		results[i] = sub[j]
	}
	return results
}

// dispatchCalls runs well-formed tool_use blocks with the Ps batching rules.
func (l *Loop) dispatchCalls(ctx context.Context, calls []ContentBlock) []ContentBlock {
	results := make([]ContentBlock, len(calls))

	flush := func(batch []int) {
		if len(batch) == 0 {
			return
		}
		if len(batch) == 1 {
			i := batch[0]
			results[i] = l.runOne(ctx, toToolUse(calls[i]))
			return
		}
		var wg sync.WaitGroup
		for _, idx := range batch {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = l.runOne(ctx, toToolUse(calls[i]))
			}(idx)
		}
		wg.Wait()
	}

	var batch []int
	for i, c := range calls {
		if l.concurrencySafe(toToolUse(c)) {
			batch = append(batch, i)
			continue
		}
		// A mutating call: run everything queued so far, then this one alone.
		flush(batch)
		batch = nil
		flush([]int{i})
	}
	flush(batch)
	return results
}

// runOne executes a single tool_use: the PreToolUse / permission checkpoint,
// the dispatch, then the PostToolUse checkpoint. It always returns a
// tool_result block (an is_error block on any denial or failure) so the model
// receives an answer for every tool_use id.
func (l *Loop) runOne(ctx context.Context, call ToolUse) ContentBlock {
	// Honour cancellation before doing any work (zz checks the abort signal at
	// entry and yields the synthetic user_interrupted tool_result).
	if err := ctx.Err(); err != nil {
		return interruptedToolResult(call.ID)
	}

	// No dispatcher means no tool is available: answer like the runner's
	// failed lookup instead of crashing.
	if l.Tools == nil {
		return ToolResultBlock(call.ID, "Error: No such tool available: "+call.Name, true)
	}

	// PreToolUse / permission checkpoint.
	if l.Hooks != nil {
		decision, err := l.Hooks.PreToolUse(ctx, call)
		if err != nil {
			return ToolResultBlock(call.ID, fmt.Sprintf("PreToolUse hook failed: %v", err), true)
		}
		if decision.Behavior != BehaviorAllow {
			// deny or ask (no interactive dialog here) → denied tool_result.
			msg := decision.Message
			if msg == "" {
				msg = fmt.Sprintf("Permission to use %s has been denied.", call.Name)
			}
			return ToolResultBlock(call.ID, msg, true)
		}
	}

	result, err := l.Tools.Dispatch(ctx, call)
	if err != nil && ctx.Err() != nil {
		// The tool was cut off by the abort: synthetic interrupted result.
		return interruptedToolResult(call.ID)
	}
	if err != nil {
		// A loop-fatal dispatch error is still surfaced to the model as an
		// is_error tool_result, matching "Error calling
		// tool (<name>): <message>".
		msg := fmt.Sprintf("Error calling tool (%s): %v", call.Name, err)
		if fh, ok := l.Hooks.(ToolFailureHook); ok {
			fh.PostToolUseFailure(ctx, call, ToolResult{Content: msg, IsError: true})
		}
		return ToolResultBlock(call.ID, msg, true)
	}

	if l.Hooks != nil && !result.IsError {
		l.Hooks.PostToolUse(ctx, call, result)
	} else if fh, ok := l.Hooks.(ToolFailureHook); ok && result.IsError && !result.PermissionDenied {
		fh.PostToolUseFailure(ctx, call, result)
	}

	b := ToolResultBlockWithImages(call.ID, result.Content, result.IsError, result.Images)
	if !result.IsError && len(result.LoadedTools) > 0 {
		b.LoadedTools = append([]string(nil), result.LoadedTools...)
	}
	return b
}

// concurrencySafe reports whether a call may run in parallel. Default: false
// (fully serial), the conservative default.
func (l *Loop) concurrencySafe(call ToolUse) bool {
	if l.Tools == nil {
		return false
	}
	if c, ok := l.Tools.(ConcurrencyClassifier); ok {
		return c.IsConcurrencySafe(call)
	}
	return false
}

func toToolUse(b ContentBlock) ToolUse {
	return ToolUse{ID: b.ID, Name: b.Name, Input: b.Input}
}

// toolUseErrorOpen / Close bracket the error content the model sees.
const (
	toolUseErrorOpen  = "<tool_use_error>"
	toolUseErrorClose = "</tool_use_error>"
)

// wrapToolError wraps an error message for a tool_result, unless it is already
// wrapped (several call sites pre-wrap their message).
func wrapToolError(msg string) string {
	if strings.HasPrefix(msg, toolUseErrorOpen) {
		return msg
	}
	return toolUseErrorOpen + msg + toolUseErrorClose
}
