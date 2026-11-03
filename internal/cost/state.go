package cost

import "time"

// State is the persistable part of a ledger: the spend and token totals a
// resumed session continues from. The session transcript stores it (see
// sessionstore.Session.AppendCostState), so `--resume` restores what /cost,
// the status line and the spend cap saw. Wall-clock time is not part of it:
// a resumed session's duration starts over.
type State struct {
	TotalCostUSD     float64 `json:"totalCostUsd"`
	SideQueryCostUSD float64 `json:"sideQueryCostUsd,omitempty"`
	APIDurationMS    int64   `json:"apiDurationMs,omitempty"`
	// APIDurationWithoutRetriesMS is the successful attempts' share of
	// APIDurationMS.
	APIDurationWithoutRetriesMS int64 `json:"apiDurationWithoutRetriesMs,omitempty"`
	ToolDurationMS              int64 `json:"toolDurationMs,omitempty"`
	LinesAdded                  int   `json:"linesAdded,omitempty"`
	LinesRemoved                int   `json:"linesRemoved,omitempty"`
	// ModelUsage is the per-model accounting, cost basis and unknown-price
	// flag included.
	ModelUsage map[string]StateModel `json:"modelUsage,omitempty"`
}

// StateModel is one model's entry in a State.
type StateModel struct {
	InputTokens              int     `json:"inputTokens,omitempty"`
	OutputTokens             int     `json:"outputTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	WebSearchRequests        int     `json:"webSearchRequests,omitempty"`
	CostUSD                  float64 `json:"costUsd,omitempty"`
	CostUnknown              bool    `json:"costUnknown,omitempty"`
	CostBasis                Basis   `json:"costBasis,omitempty"`
}

// State snapshots the ledger's persistable totals.
func (l *Ledger) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := State{
		TotalCostUSD:                l.totalCostUSD,
		SideQueryCostUSD:            l.sideQueryCostUSD,
		APIDurationMS:               l.totalAPIDuration.Milliseconds(),
		APIDurationWithoutRetriesMS: l.totalAPIDurationWithoutRetry.Milliseconds(),
		ToolDurationMS:              l.totalToolDuration.Milliseconds(),
		LinesAdded:                  l.linesAdded,
		LinesRemoved:                l.linesRemoved,
	}
	if len(l.modelUsage) > 0 {
		st.ModelUsage = make(map[string]StateModel, len(l.modelUsage))
		for k, m := range l.modelUsage {
			st.ModelUsage[k] = StateModel{
				InputTokens:              m.InputTokens,
				OutputTokens:             m.OutputTokens,
				CacheReadInputTokens:     m.CacheReadInputTokens,
				CacheCreationInputTokens: m.CacheCreationInputTokens,
				WebSearchRequests:        m.WebSearchRequests,
				CostUSD:                  m.CostUSD,
				CostUnknown:              m.CostUnknown,
				CostBasis:                m.CostBasis,
			}
		}
	}
	return st
}

// Restore replaces the ledger's persistable totals with st (a resumed
// session's saved state). The wall clock and the safety-stop count are
// kept; negative or NaN amounts are read as zero.
func (l *Ledger) Restore(st State) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.totalCostUSD = nonNegative(st.TotalCostUSD)
	l.sideQueryCostUSD = nonNegative(st.SideQueryCostUSD)
	l.totalAPIDuration = time.Duration(max(st.APIDurationMS, 0)) * time.Millisecond
	l.totalAPIDurationWithoutRetry = time.Duration(max(st.APIDurationWithoutRetriesMS, 0)) * time.Millisecond
	l.totalToolDuration = time.Duration(max(st.ToolDurationMS, 0)) * time.Millisecond
	l.linesAdded, l.linesRemoved = max(st.LinesAdded, 0), max(st.LinesRemoved, 0)
	l.modelUsage = make(map[string]*ModelUsage, len(st.ModelUsage))
	l.unknownCostModels = nil
	l.hasUnknownModelCost = false
	for k, m := range st.ModelUsage {
		l.modelUsage[k] = &ModelUsage{
			InputTokens:              m.InputTokens,
			OutputTokens:             m.OutputTokens,
			CacheReadInputTokens:     m.CacheReadInputTokens,
			CacheCreationInputTokens: m.CacheCreationInputTokens,
			WebSearchRequests:        m.WebSearchRequests,
			CostUSD:                  nonNegative(m.CostUSD),
			CostUnknown:              m.CostUnknown,
			CostBasis:                m.CostBasis,
		}
		if m.CostUnknown {
			if l.unknownCostModels == nil {
				l.unknownCostModels = map[string]bool{}
			}
			l.unknownCostModels[k] = true
			l.hasUnknownModelCost = true
		}
	}
}

func nonNegative(v float64) float64 {
	if v != v || v < 0 {
		return 0
	}
	return v
}
