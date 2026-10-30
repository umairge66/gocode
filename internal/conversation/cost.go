package conversation

import (
	"context"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/cost"
)

// Cost accounting wiring.
//
// The ledger is LOCAL only: it accumulates per-turn token usage and the baked
// USD cost on-device so /cost and /usage can display it. Nothing in this file
// transmits usage anywhere.
//
// # Where per-turn usage is captured
//
// The agent turn loop (agent.Loop.Run) drives one or more model rounds and only
// returns a terminal agent.Result — which carries no usage — and the loop's
// agent.ModelResponse carries only {Message, StopReason}, not the apiclient
// usage. So per-turn usage is NOT reachable from the Result the conversation
// gets back. The one seam the conversation owns is the model caller it hands the
// loop, so usage is captured there: c.costModel() wraps c.Model in a
// costRecordingModel that times each round's CallModel / CallModelStream and,
// when the underlying caller can report the usage of the response it just
// produced (TurnUsageReporter), records that turn's usage + model id + wall
// duration into the ledger. Because the loop calls the caller serially within a
// Submit, the "last turn" the reporter returns is unambiguous.

// TurnUsageReporter is an optional capability a model caller may implement so the
// conversation can record per-turn token usage into its cost Ledger. A caller
// that returns the apiclient.Usage of the response it most recently produced
// (and the model id that produced it) enables local /cost accounting; a caller
// that does not implement it contributes no cost (the ledger stays empty).
//
// provider.ModelCaller implements it (stashing the usage of each provider
// response); fake callers in tests implement it directly.
type TurnUsageReporter interface {
	// LastTurnUsage reports the usage and model id of the response the caller most
	// recently produced. ok is false when no usage is available.
	LastTurnUsage() (model string, usage apiclient.Usage, ok bool)
}

// recordCost accumulates one model turn's usage, cost and wall duration into the
// session ledger. It is nil-safe: a Conversation with no ledger (Cost == nil)
// records nothing, so struct-literal Conversations and accounting-free hosts are
// unaffected. The model id falls back to the session's selected ModelID when the
// reporter does not name one.
func (c *Conversation) recordCost(model string, u apiclient.Usage, d time.Duration) {
	if c == nil || c.Cost == nil {
		return
	}
	if model == "" {
		model = c.ModelID
	}
	c.Cost.AddTurn(model, cost.UsageFromAPI(u))
	c.Cost.AddAPIDuration(d, d)
}

// costModel returns the model caller the loop should drive. When a ledger is
// wired, or the caller can report per-response usage (TurnUsageReporter), it
// wraps c.Model so each round's usage is recorded — into the ledger, and as the
// context-usage anchor ContextTokens builds on. Otherwise it returns c.Model
// unchanged (zero overhead, identical behavior).
func (c *Conversation) costModel() agent.ModelCaller {
	if _, reports := c.Model.(TurnUsageReporter); c.Cost == nil && !reports {
		return c.Model
	}
	return &costRecordingModel{inner: c.Model, conv: c}
}

// costRecordingModel decorates an agent.ModelCaller: it forwards every round to
// the inner caller, times it, and records the turn's usage into the ledger. It
// implements the optional streaming extension too, delegating to the inner
// caller's streaming path when available so live deltas are preserved; a
// non-streaming inner falls back to CallModel (the handler is simply unused,
// exactly as the loop would do without the wrapper).
type costRecordingModel struct {
	inner agent.ModelCaller
	conv  *Conversation
}

var (
	_ agent.ModelCaller          = (*costRecordingModel)(nil)
	_ agent.StreamingModelCaller = (*costRecordingModel)(nil)
)

func (m *costRecordingModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	start := time.Now()
	resp, err := m.inner.CallModel(ctx, req)
	m.record(start, len(req.Messages), err)
	return resp, err
}

func (m *costRecordingModel) CallModelStream(ctx context.Context, req agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	start := time.Now()
	var (
		resp agent.ModelResponse
		err  error
	)
	if sc, ok := m.inner.(agent.StreamingModelCaller); ok {
		resp, err = sc.CallModelStream(ctx, req, h)
	} else {
		resp, err = m.inner.CallModel(ctx, req)
	}
	m.record(start, len(req.Messages), err)
	return resp, err
}

// record queries the inner caller for the usage of the round it just produced
// and folds it into the ledger with the round's wall duration, and — for a
// successful round — anchors it as the live context usage (requestLen is the
// request's message count). A caller that cannot report usage (or has none) is
// a no-op.
func (m *costRecordingModel) record(start time.Time, requestLen int, callErr error) {
	d := time.Since(start)
	r, ok := m.inner.(TurnUsageReporter)
	if !ok {
		return
	}
	model, usage, ok := r.LastTurnUsage()
	if !ok {
		return
	}
	m.conv.recordCost(model, usage, d)
	if callErr == nil {
		m.conv.recordContextUsage(requestLen, usage)
	}
}

// CostSummary renders the /cost command body for this conversation's ledger, as
// the /cost builtin and the cli host display it. It returns "" when no ledger is
// wired. The cli layer and builtincmds reach the ledger through this accessor
// (and CostSession); neither is edited here — this is the display seam.
func (c *Conversation) CostSummary() string {
	if c.Cost == nil {
		return ""
	}
	return cost.CostSummary(c.Cost)
}

// OutputTokens returns the cumulative output-token count recorded in the
// session ledger so far, for the spinner's running "↓ N tokens" counter. It is
// nil-safe: a Conversation with no ledger reports 0. It reads the ledger's own
// concurrency-safe roll-up, so it is cheap to poll from a ticker while a turn
// runs.
func (c *Conversation) OutputTokens() int {
	if c == nil || c.Cost == nil {
		return 0
	}
	return c.Cost.TotalOutputTokens()
}

// CostSession snapshots the ledger into the status-line / /usage session block
// (total_cost_usd, durations, lines changed, per-model usage). ok is false when
// no ledger is wired.
func (c *Conversation) CostSession() (cost.StatusLineSession, bool) {
	if c.Cost == nil {
		return cost.StatusLineSession{}, false
	}
	return c.Cost.Session(), true
}
