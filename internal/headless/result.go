package headless

import (
	"encoding/json"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/ids"
)

// Result subtypes.
const (
	SubtypeSuccess                  = "success"
	SubtypeErrorMaxTurns            = "error_max_turns"
	SubtypeErrorDuringExecution     = "error_during_execution"
	SubtypeErrorMaxStructuredOutput = "error_max_structured_output_retries"
	// SubtypeErrorMaxBudgetUSD: the session reached its spend cap
	// (--max-budget-usd / maxSessionCostUsd) and the turn stopped.
	SubtypeErrorMaxBudgetUSD = "error_max_budget_usd"
)

// ServerToolUse counts the server-side tool requests of a turn.
type ServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
}

// Usage is the token accounting of the main agent loop, in the Messages API
// shape.
type Usage struct {
	InputTokens              int           `json:"input_tokens"`
	CacheCreationInputTokens int           `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int           `json:"cache_read_input_tokens"`
	OutputTokens             int           `json:"output_tokens"`
	ServerToolUse            ServerToolUse `json:"server_tool_use"`
	ServiceTier              string        `json:"service_tier"`
}

// UsageFromModel converts one model response's usage.
func UsageFromModel(u agent.ModelUsage) Usage {
	return Usage{
		InputTokens:              u.InputTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
		CacheReadInputTokens:     u.CachedInputTokens,
		OutputTokens:             u.OutputTokens,
		ServiceTier:              "standard",
	}
}

// ModelUsage is one entry of the result's per-model usage map.
type ModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	WebSearchRequests        int     `json:"webSearchRequests"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow"`
	MaxOutputTokens          int     `json:"maxOutputTokens"`
	// CostUnknown marks a model with turns whose price is unknown; their
	// usage is not in CostUSD or the result's total_cost_usd.
	CostUnknown bool `json:"costUnknown,omitempty"`
	// CostBasis is the price table the model's latest turn was priced at
	// (reported, configured, catalog, list, free or unknown).
	CostBasis string `json:"costBasis,omitempty"`
}

// PermissionDenial is one tool call the permission decision refused.
type PermissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// Result is the final record of a print-mode run: the whole outcome of one
// turn. A success carries Result (the final text); an error subtype carries
// Errors instead.
type Result struct {
	Type          string  `json:"type"`
	Subtype       string  `json:"subtype"`
	IsError       bool    `json:"is_error"`
	DurationMS    int64   `json:"duration_ms"`
	DurationAPIMS int64   `json:"duration_api_ms"`
	NumTurns      int     `json:"num_turns"`
	Result        *string `json:"result,omitempty"`
	StopReason    *string `json:"stop_reason"`
	SessionID     string  `json:"session_id"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	// CostUnknown is set when some usage could not be priced, so
	// TotalCostUSD covers only the priced part (see modelUsage costUnknown).
	CostUnknown       bool                  `json:"cost_unknown,omitempty"`
	Usage             Usage                 `json:"usage"`
	ModelUsage        map[string]ModelUsage `json:"modelUsage"`
	PermissionDenials []PermissionDenial    `json:"permission_denials"`
	APIErrorStatus    *int                  `json:"api_error_status,omitempty"`
	StructuredOutput  any                   `json:"structured_output,omitempty"`
	TerminalReason    string                `json:"terminal_reason,omitempty"`
	Errors            []string              `json:"errors,omitempty"`
	UUID              string                `json:"uuid"`
}

// NewResult returns a result record with its identity, list and map fields
// initialised (they are always present, never null).
func NewResult(sessionID string) Result {
	return Result{
		Type:              "result",
		Subtype:           SubtypeSuccess,
		SessionID:         sessionID,
		Usage:             Usage{ServiceTier: "standard"},
		ModelUsage:        map[string]ModelUsage{},
		PermissionDenials: []PermissionDenial{},
		UUID:              newUUID(),
	}
}

// SetSuccess marks the result a success with the final text.
func (r *Result) SetSuccess(text string) {
	r.Subtype, r.IsError, r.Errors = SubtypeSuccess, false, nil
	r.Result = &text
}

// SetError marks the result an error of the given subtype.
func (r *Result) SetError(subtype string, errs ...string) {
	if errs == nil {
		errs = []string{}
	}
	r.Subtype, r.IsError, r.Result, r.Errors = subtype, true, nil, errs
}

// MarshalJSON keeps "errors" present (possibly empty) on error subtypes and
// absent on a success.
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	if r.Subtype == SubtypeSuccess {
		return Marshal(plain(r))
	}
	errs := r.Errors
	if errs == nil {
		errs = []string{}
	}
	return Marshal(struct {
		plain
		Errors []string `json:"errors"`
	}{plain: plain(r), Errors: errs})
}

// UsageDelta returns the main-loop token usage accumulated in l since before
// (a snapshot taken with UsageSnapshot).
func UsageDelta(l *cost.Ledger, before Usage) Usage {
	now := UsageSnapshot(l)
	return Usage{
		InputTokens:              now.InputTokens - before.InputTokens,
		CacheCreationInputTokens: now.CacheCreationInputTokens - before.CacheCreationInputTokens,
		CacheReadInputTokens:     now.CacheReadInputTokens - before.CacheReadInputTokens,
		OutputTokens:             now.OutputTokens - before.OutputTokens,
		ServerToolUse:            ServerToolUse{WebSearchRequests: now.ServerToolUse.WebSearchRequests - before.ServerToolUse.WebSearchRequests},
		ServiceTier:              "standard",
	}
}

// UsageSnapshot returns the ledger's cumulative usage.
func UsageSnapshot(l *cost.Ledger) Usage {
	if l == nil {
		return Usage{ServiceTier: "standard"}
	}
	return Usage{
		InputTokens:              l.TotalInputTokens(),
		CacheCreationInputTokens: l.TotalCacheCreationInputTokens(),
		CacheReadInputTokens:     l.TotalCacheReadInputTokens(),
		OutputTokens:             l.TotalOutputTokens(),
		ServerToolUse:            ServerToolUse{WebSearchRequests: l.TotalWebSearchRequests()},
		ServiceTier:              "standard",
	}
}

// ModelUsageFrom converts the ledger's per-model usage; limits supplies each
// model's context window and output-token cap (nil leaves them 0).
func ModelUsageFrom(l *cost.Ledger, limits func(model string) (contextWindow, maxOutput int)) map[string]ModelUsage {
	out := map[string]ModelUsage{}
	if l == nil {
		return out
	}
	for model, u := range l.ModelUsage() {
		mu := ModelUsage{
			InputTokens:              u.InputTokens,
			OutputTokens:             u.OutputTokens,
			CacheReadInputTokens:     u.CacheReadInputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
			WebSearchRequests:        u.WebSearchRequests,
			CostUSD:                  u.CostUSD,
			CostUnknown:              u.CostUnknown,
			CostBasis:                string(u.CostBasis),
		}
		if limits != nil {
			mu.ContextWindow, mu.MaxOutputTokens = limits(model)
		}
		out[model] = mu
	}
	return out
}

// TerminalReason maps the agent loop's outcome to the result's
// terminal_reason.
func TerminalReason(r agent.Reason) string {
	switch r {
	case agent.ReasonCompleted:
		return "completed"
	case agent.ReasonMaxTurns:
		return "max_turns"
	case agent.ReasonAPIError:
		return "api_error"
	case agent.ReasonMalformedToolUse:
		return "malformed_tool_use_exhausted"
	case agent.ReasonAborted:
		return "aborted_streaming"
	case agent.ReasonStopHookPrevented:
		return "stop_hook_prevented"
	case agent.ReasonRefusal:
		return "model_error"
	case agent.ReasonContextWindowExceeded:
		return "prompt_too_long"
	case agent.ReasonRepeatedToolCalls:
		return "repeated_tool_calls"
	case agent.ReasonMaxBudget:
		return "max_budget_usd"
	}
	return ""
}

func newUUID() string { return ids.RandomUUID() }
