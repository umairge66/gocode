package cost

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/apiclient"
)

// ModelUsage is the per-model cumulative accounting kept by the ledger.
type ModelUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	WebSearchRequests        int
	CostUSD                  float64
	// CostUnknown is set once any turn of the model could not be priced (no
	// reported cost, no configured, catalog or list price); those turns are
	// counted as $0 in CostUSD.
	CostUnknown bool
	// CostBasis is the table the model's most recent turn was priced at.
	CostBasis Basis
}

// TokenCounts is a bare token tuple (spent/restored token totals).
type TokenCounts struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

// Ledger is the concurrency-safe per-session cost/usage accumulator.
type Ledger struct {
	mu sync.Mutex

	totalCostUSD     float64
	sideQueryCostUSD float64

	totalAPIDuration             time.Duration
	totalAPIDurationWithoutRetry time.Duration
	totalToolDuration            time.Duration

	startTime time.Time

	linesAdded   int
	linesRemoved int

	hasUnknownModelCost bool
	unknownCostModels   map[string]bool
	safetyStops         int

	modelUsage  map[string]*ModelUsage
	spentTokens TokenCounts

	// now is injectable for deterministic tests; defaults to time.Now.
	now func() time.Time
}

// NewLedger returns a ledger with its wall clock started now.
func NewLedger() *Ledger {
	l := &Ledger{
		modelUsage: make(map[string]*ModelUsage),
		now:        time.Now,
	}
	l.startTime = l.now()
	return l
}

// AddTurn records one assistant turn for model: it prices the turn (see
// PriceTurn), accumulates tokens and cost into the per-model usage and the
// running total, and returns the cost. A turn whose price is unknown adds its
// tokens but no cost, and marks the model as unpriced.
func (l *Ledger) AddTurn(model string, u Usage) float64 {
	usd, basis := PriceTurn(model, u)

	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.modelUsage[model]
	if m == nil {
		m = &ModelUsage{}
		l.modelUsage[model] = m
	}
	if basis == BasisUnknown {
		l.hasUnknownModelCost = true
		if l.unknownCostModels == nil {
			l.unknownCostModels = map[string]bool{}
		}
		l.unknownCostModels[model] = true
		m.CostUnknown = true
	}
	m.CostBasis = basis
	m.InputTokens += u.InputTokens
	m.OutputTokens += u.OutputTokens
	m.CacheReadInputTokens += u.CacheReadInputTokens
	m.CacheCreationInputTokens += u.CacheCreationInputTokens
	m.WebSearchRequests += u.WebSearchRequests
	m.CostUSD += usd
	l.totalCostUSD += usd
	return usd
}

// AddLines records lines added/removed by an edit for the session code-change
// totals.
func (l *Ledger) AddLines(added, removed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.linesAdded += added
	l.linesRemoved += removed
}

// AddAPIDuration records an API request's wall time. total counts every attempt;
// withoutRetries counts only the attempt that succeeded.
func (l *Ledger) AddAPIDuration(total, withoutRetries time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.totalAPIDuration += total
	l.totalAPIDurationWithoutRetry += withoutRetries
}

// AddToolDuration records time spent in a tool call.
func (l *Ledger) AddToolDuration(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.totalToolDuration += d
}

// RecordSideQueryCost adds the cost of an auxiliary (non-main-loop) query.
func (l *Ledger) RecordSideQueryCost(usd float64) {
	if usd < 0 || usd != usd { // NaN or negative rejected
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sideQueryCostUSD += usd
}

// RecordSpentTokens accumulates raw spent-token counts (independent of model).
func (l *Ledger) RecordSpentTokens(t TokenCounts) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spentTokens.InputTokens += t.InputTokens
	l.spentTokens.OutputTokens += t.OutputTokens
	l.spentTokens.CacheReadInputTokens += t.CacheReadInputTokens
	l.spentTokens.CacheCreationInputTokens += t.CacheCreationInputTokens
}

// RecordSafetyStop increments the safety-stop counter.
func (l *Ledger) RecordSafetyStop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.safetyStops++
}

// --- totals / accessors ---

// TotalCostUSD is the running session cost.
func (l *Ledger) TotalCostUSD() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalCostUSD
}

// SideQueryCostUSD is the accumulated cost of auxiliary queries.
func (l *Ledger) SideQueryCostUSD() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sideQueryCostUSD
}

// TotalAPIDuration is the summed API wall time incl. retries.
func (l *Ledger) TotalAPIDuration() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalAPIDuration
}

// TotalAPIDurationWithoutRetries is the summed API wall time of successful
// attempts only.
func (l *Ledger) TotalAPIDurationWithoutRetries() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalAPIDurationWithoutRetry
}

// TotalToolDuration is the summed time spent in tool calls.
func (l *Ledger) TotalToolDuration() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalToolDuration
}

// TotalDuration is the elapsed wall time since the session started (never
// negative).
func (l *Ledger) TotalDuration() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.now().Sub(l.startTime)
	if d < 0 {
		return 0
	}
	return d
}

// TotalLinesAdded is the number of lines added this session.
func (l *Ledger) TotalLinesAdded() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.linesAdded
}

// TotalLinesRemoved is the number of lines removed this session.
func (l *Ledger) TotalLinesRemoved() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.linesRemoved
}

// HasUnknownModelCost reports whether any turn used a model whose price is
// unknown (its cost is not in the totals).
func (l *Ledger) HasUnknownModelCost() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hasUnknownModelCost
}

// UnknownCostModels returns, sorted, the models with at least one unpriced
// turn.
func (l *Ledger) UnknownCostModels() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return sortedKeys(l.unknownCostModels)
}

// SafetyStops is the safety-stop counter.
func (l *Ledger) SafetyStops() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.safetyStops
}

// ModelUsage returns a deep copy of the per-model usage map.
func (l *Ledger) ModelUsage() map[string]ModelUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]ModelUsage, len(l.modelUsage))
	for k, v := range l.modelUsage {
		out[k] = *v
	}
	return out
}

// UsageForModel returns the cumulative usage for one model (ok=false if none).
func (l *Ledger) UsageForModel(model string) (ModelUsage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.modelUsage[model]
	if !ok {
		return ModelUsage{}, false
	}
	return *m, true
}

// token roll-ups across all models.

func (l *Ledger) sumTokens(pick func(*ModelUsage) int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for _, m := range l.modelUsage {
		total += pick(m)
	}
	return total
}

// TotalInputTokens sums input tokens across all models.
func (l *Ledger) TotalInputTokens() int {
	return l.sumTokens(func(m *ModelUsage) int { return m.InputTokens })
}

// TotalOutputTokens sums output tokens across all models.
func (l *Ledger) TotalOutputTokens() int {
	return l.sumTokens(func(m *ModelUsage) int { return m.OutputTokens })
}

// TotalCacheReadInputTokens sums cache-read tokens across all models.
func (l *Ledger) TotalCacheReadInputTokens() int {
	return l.sumTokens(func(m *ModelUsage) int { return m.CacheReadInputTokens })
}

// TotalCacheCreationInputTokens sums cache-creation tokens across all models.
func (l *Ledger) TotalCacheCreationInputTokens() int {
	return l.sumTokens(func(m *ModelUsage) int { return m.CacheCreationInputTokens })
}

// TotalWebSearchRequests sums web-search requests across all models.
func (l *Ledger) TotalWebSearchRequests() int {
	return l.sumTokens(func(m *ModelUsage) int { return m.WebSearchRequests })
}

// setNow overrides the clock (tests only).
func (l *Ledger) setNow(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// UsageFromAPI maps an apiclient.Usage onto the cost model's Usage: the token
// counts (already in the Anthropic split: uncached input, cache read, cache
// write, output), the serving provider, any provider-reported cost, the
// 1-hour cache-write share and the web-search request count parsed from the
// server_tool_use raw field.
func UsageFromAPI(u apiclient.Usage) Usage {
	return Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		Ephemeral1hInputTokens:   u.CacheCreation1hInputTokens,
		WebSearchRequests:        webSearchRequests(u.ServerToolUse),
		Provider:                 u.Provider,
		ReportedCostUSD:          u.ReportedCostUSD,
		CostReported:             u.CostReported,
	}
}

func webSearchRequests(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	var v struct {
		WebSearchRequests int `json:"web_search_requests"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0
	}
	return v.WebSearchRequests
}
