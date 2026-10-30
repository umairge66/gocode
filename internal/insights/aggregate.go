package insights

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
)

// jsonUnmarshalStrict decodes a cached SessionMeta, rejecting records that
// miss a required field.
func jsonUnmarshalStrict(data []byte, m *SessionMeta) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	for _, k := range []string{"session_id", "project_path", "start_time", "duration_minutes",
		"user_message_count", "assistant_message_count", "git_commits", "git_pushes",
		"input_tokens", "output_tokens", "first_prompt"} {
		if v, ok := probe[k]; !ok || string(v) == "null" {
			return errors.New("insights: session-meta missing " + k)
		}
	}
	return json.Unmarshal(data, m)
}

// DateRange is the analyzed period (YYYY-MM-DD).
type DateRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// MultiClauding summarizes overlapping (parallel) sessions.
type MultiClauding struct {
	OverlapEvents      int `json:"overlap_events"`
	SessionsInvolved   int `json:"sessions_involved"`
	UserMessagesDuring int `json:"user_messages_during"`
}

// AutoModeStats feeds the auto-mode recommendation.
type AutoModeStats struct {
	AutoSessions              int  `json:"auto_sessions"`
	NonAutoSessions           int  `json:"non_auto_sessions"`
	EstimatedPromptsInNonAuto int  `json:"estimated_prompts_in_non_auto"`
	DestructiveInNonAuto      int  `json:"destructive_in_non_auto"`
	RecentAllAuto             bool `json:"recent_all_auto"`
}

// SessionSummary is one row of the per-session summary list.
type SessionSummary struct {
	ID      string  `json:"id"`
	Date    string  `json:"date"`
	Summary string  `json:"summary"`
	Goal    *string `json:"goal,omitempty"`
}

// AggregatedData is the cross-session aggregate the report is built from.
type AggregatedData struct {
	TotalSessions          int              `json:"total_sessions"`
	SessionsWithFacets     int              `json:"sessions_with_facets"`
	DateRange              DateRange        `json:"date_range"`
	TotalMessages          int              `json:"total_messages"`
	TotalDurationHours     float64          `json:"total_duration_hours"`
	TotalInputTokens       float64          `json:"total_input_tokens"`
	TotalOutputTokens      float64          `json:"total_output_tokens"`
	ToolCounts             *counts          `json:"tool_counts"`
	Languages              *counts          `json:"languages"`
	GitCommits             int              `json:"git_commits"`
	GitPushes              int              `json:"git_pushes"`
	Projects               *counts          `json:"projects"`
	GoalCategories         *counts          `json:"goal_categories"`
	Outcomes               *counts          `json:"outcomes"`
	Satisfaction           *counts          `json:"satisfaction"`
	Helpfulness            *counts          `json:"helpfulness"`
	SessionTypes           *counts          `json:"session_types"`
	Friction               *counts          `json:"friction"`
	Success                *counts          `json:"success"`
	SessionSummaries       []SessionSummary `json:"session_summaries"`
	TotalInterruptions     int              `json:"total_interruptions"`
	TotalToolErrors        int              `json:"total_tool_errors"`
	ToolErrorCategories    *counts          `json:"tool_error_categories"`
	UserResponseTimes      []float64        `json:"user_response_times"`
	MedianResponseTime     float64          `json:"median_response_time"`
	AvgResponseTime        float64          `json:"avg_response_time"`
	SessionsUsingTaskAgent int              `json:"sessions_using_task_agent"`
	SessionsUsingMCP       int              `json:"sessions_using_mcp"`
	SessionsUsingWebSearch int              `json:"sessions_using_web_search"`
	SessionsUsingWebFetch  int              `json:"sessions_using_web_fetch"`
	TotalLinesAdded        int              `json:"total_lines_added"`
	TotalLinesRemoved      int              `json:"total_lines_removed"`
	TotalFilesModified     int              `json:"total_files_modified"`
	DaysActive             int              `json:"days_active"`
	MessagesPerDay         float64          `json:"messages_per_day"`
	MessageHours           []int            `json:"message_hours"`
	MultiClauding          MultiClauding    `json:"multi_clauding"`
	AutoModeStats          AutoModeStats    `json:"auto_mode_stats"`
	TotalSessionsScanned   int              `json:"total_sessions_scanned,omitempty"`
}

// Recommendation constants.
const (
	autoModeRecentWindow   = 20
	metaRecomputeThreshold = 3 * autoModeRecentWindow
	autoModeMinSessions    = 5
	autoModeShareThreshold = 0.25
	autoModeMinPrompts     = 3
)

// beforeT returns s up to the first "T".
func beforeT(s string) string {
	if i := strings.Index(s, "T"); i >= 0 {
		return s[:i]
	}
	return s
}

// numEntries yields the numeric entries of a facet sub-object in JS order.
func numEntries(o *jsObject) []countEntry {
	var out []countEntry
	for _, k := range objectEntries(o) {
		if f, ok := o.vals[k].(float64); ok {
			out = append(out, countEntry{k, f})
		}
	}
	return out
}

// aggregate folds per-session metadata and facets into AggregatedData.
func aggregate(metas []*SessionMeta, facets *facetMap) *AggregatedData {
	n := &AggregatedData{
		TotalSessions: len(metas), SessionsWithFacets: facets.len(),
		ToolCounts: newCounts(), Languages: newCounts(), Projects: newCounts(),
		GoalCategories: newCounts(), Outcomes: newCounts(), Satisfaction: newCounts(),
		Helpfulness: newCounts(), SessionTypes: newCounts(), Friction: newCounts(),
		Success: newCounts(), SessionSummaries: []SessionSummary{},
		ToolErrorCategories: newCounts(), UserResponseTimes: []float64{}, MessageHours: []int{},
	}
	type autoRow struct {
		start       string
		auto        bool
		prompts     int
		destructive int
	}
	var starts []string
	var rts []float64
	var hours []int
	var auto []autoRow
	for _, r := range metas {
		starts = append(starts, r.StartTime)
		n.TotalMessages += r.UserMessageCount
		n.TotalDurationHours += r.DurationMinutes / 60
		n.TotalInputTokens += r.InputTokens
		n.TotalOutputTokens += r.OutputTokens
		n.GitCommits += r.GitCommits
		n.GitPushes += r.GitPushes
		n.TotalInterruptions += r.UserInterruptions
		n.TotalToolErrors += r.ToolErrors
		for _, e := range r.ToolErrorCategories.entries() {
			n.ToolErrorCategories.add(e.key, e.val)
		}
		rts = append(rts, r.UserResponseTimes...)
		if r.UsesTaskAgent {
			n.SessionsUsingTaskAgent++
		}
		if r.UsesMCP {
			n.SessionsUsingMCP++
		}
		if r.UsesWebSearch {
			n.SessionsUsingWebSearch++
		}
		if r.UsesWebFetch {
			n.SessionsUsingWebFetch++
		}
		n.TotalLinesAdded += r.LinesAdded
		n.TotalLinesRemoved += r.LinesRemoved
		n.TotalFilesModified += r.FilesModified
		hours = append(hours, r.MessageHours...)
		if r.BashWouldPromptCount != nil {
			h := r.PermissionMode
			if h == "auto" || h == "default" || h == "acceptEdits" {
				row := autoRow{start: r.StartTime, auto: h == "auto"}
				if !row.auto {
					row.prompts = *r.BashWouldPromptCount
					if h == "default" && r.FileEditToolCount != nil {
						row.prompts += *r.FileEditToolCount
					}
					if r.DestructiveCommandCount != nil {
						row.destructive = *r.DestructiveCommandCount
					}
				}
				auto = append(auto, row)
			}
		}
		for _, e := range r.ToolCounts.entries() {
			n.ToolCounts.add(e.key, e.val)
		}
		for _, e := range r.Languages.entries() {
			n.Languages.add(e.key, e.val)
		}
		if r.ProjectPath != "" {
			n.Projects.add(r.ProjectPath, 1)
		}
		f := facets.get(r.SessionID)
		if f != nil {
			for _, e := range numEntries(f.obj.obj("goal_categories")) {
				if e.val > 0 {
					n.GoalCategories.add(e.key, e.val)
				}
			}
			n.Outcomes.add(f.obj.field("outcome"), 1)
			for _, e := range numEntries(f.obj.obj("user_satisfaction_counts")) {
				if e.val > 0 {
					n.Satisfaction.add(e.key, e.val)
				}
			}
			n.Helpfulness.add(f.obj.field("gocode_helpfulness"), 1)
			n.SessionTypes.add(f.obj.field("session_type"), 1)
			for _, e := range numEntries(f.obj.obj("friction_counts")) {
				if e.val > 0 {
					n.Friction.add(e.key, e.val)
				}
			}
			if ps := f.obj.field("primary_success"); ps != "none" {
				n.Success.add(ps, 1)
			}
		}
		if len(n.SessionSummaries) < 50 {
			sum := r.Summary
			if sum == "" {
				sum = truncateUTF16(r.FirstPrompt, 100)
			}
			row := SessionSummary{ID: truncateUTF16(r.SessionID, 8), Date: beforeT(r.StartTime), Summary: sum}
			if f != nil {
				if g, ok := f.obj.vals["underlying_goal"].(string); ok {
					row.Goal = &g
				}
			}
			n.SessionSummaries = append(n.SessionSummaries, row)
		}
	}
	sort.Strings(starts)
	if len(starts) > 0 {
		n.DateRange = DateRange{Start: beforeT(starts[0]), End: beforeT(starts[len(starts)-1])}
	}
	if rts == nil {
		rts = []float64{}
	}
	n.UserResponseTimes = rts
	if len(rts) > 0 {
		sorted := append([]float64(nil), rts...)
		sort.Float64s(sorted)
		n.MedianResponseTime = sorted[len(sorted)/2]
		sum := 0.0
		for _, v := range rts {
			sum += v
		}
		n.AvgResponseTime = sum / float64(len(rts))
	}
	days := map[string]bool{}
	for _, s := range starts {
		days[beforeT(s)] = true
	}
	n.DaysActive = len(days)
	if n.DaysActive > 0 {
		n.MessagesPerDay = jsRound(float64(n.TotalMessages)/float64(n.DaysActive)*10) / 10
	}
	if hours == nil {
		hours = []int{}
	}
	n.MessageHours = hours
	n.MultiClauding = detectMultiClauding(metas)
	sort.SliceStable(auto, func(i, j int) bool { return auto[i].start > auto[j].start })
	if len(auto) > autoModeRecentWindow {
		auto = auto[:autoModeRecentWindow]
	}
	for _, r := range auto {
		if r.auto {
			n.AutoModeStats.AutoSessions++
		} else {
			n.AutoModeStats.NonAutoSessions++
			n.AutoModeStats.EstimatedPromptsInNonAuto += r.prompts
			n.AutoModeStats.DestructiveInNonAuto += r.destructive
		}
	}
	n.AutoModeStats.RecentAllAuto = len(auto) >= 3 && auto[0].auto && auto[1].auto && auto[2].auto
	return n
}

// detectMultiClauding finds user messages in one session sent while another
// session was active within a 30-minute window.
func detectMultiClauding(metas []*SessionMeta) MultiClauding {
	type ev struct {
		ts  int64
		sid string
	}
	var evs []ev
	for _, m := range metas {
		for _, s := range m.UserMessageTimestamps {
			t := parseJSDate(s)
			if t.IsZero() {
				continue
			}
			evs = append(evs, ev{t.UnixMilli(), m.SessionID})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].ts < evs[j].ts })
	pairs := map[string]bool{}
	during := map[string]bool{}
	last := map[string]int{}
	lo := 0
	key := func(ts int64, sid string) string { return jsNum(float64(ts)) + ":" + sid }
	for w := range evs {
		r := evs[w]
		for lo < w && r.ts-evs[lo].ts > 1800000 {
			h := evs[lo]
			if idx, ok := last[h.sid]; ok && idx == lo {
				delete(last, h.sid)
			}
			lo++
		}
		if f, ok := last[r.sid]; ok {
			for h := f + 1; h < w; h++ {
				a := evs[h]
				if a.sid != r.sid {
					p := []string{r.sid, a.sid}
					sort.Strings(p)
					pairs[strings.Join(p, ":")] = true
					during[key(evs[f].ts, r.sid)] = true
					during[key(a.ts, a.sid)] = true
					during[key(r.ts, r.sid)] = true
					break
				}
			}
		}
		last[r.sid] = w
	}
	involved := map[string]bool{}
	for p := range pairs {
		a, b, _ := strings.Cut(p, ":")
		if a != "" {
			involved[a] = true
		}
		if b != "" {
			involved[b] = true
		}
	}
	return MultiClauding{OverlapEvents: len(pairs), SessionsInvolved: len(involved), UserMessagesDuring: len(during)}
}

// PermissionGates are the live permission facts the auto-mode recommendation
// consults. All default to false.
type PermissionGates struct {
	AutoModeAvailable           bool
	CurrentSessionIsAuto        bool
	SandboxAutoAllowActive      bool
	CurrentSessionSeesNoDialogs bool
	LiveArbiterIsAutoClassifier bool
	// SetupSkillAvailable: the /auto-mode-setup command is enabled.
	SetupSkillAvailable bool
	// HasAutoModeEnvironment: auto mode already has environment entries
	// configured.
	HasAutoModeEnvironment bool
}

// AutoModeRecommendation is the optional auto-mode tip.
type AutoModeRecommendation struct {
	Kind                string // "switch_to_auto" | "run_setup"
	EstimatedPrompts    int
	NonAutoSessions     int
	DestructiveCommands int
}

// recommendAutoMode decides whether to suggest auto mode or its setup.
func recommendAutoMode(d *AggregatedData, s PermissionGates) *AutoModeRecommendation {
	if !s.AutoModeAvailable {
		return nil
	}
	n := d.AutoModeStats
	a := n.AutoSessions + n.NonAutoSessions
	if a < autoModeMinSessions {
		return nil
	}
	if !(s.CurrentSessionIsAuto || n.RecentAllAuto || float64(n.AutoSessions)/float64(a) >= autoModeShareThreshold) {
		if s.SandboxAutoAllowActive || s.CurrentSessionSeesNoDialogs || s.LiveArbiterIsAutoClassifier {
			return nil
		}
		if n.EstimatedPromptsInNonAuto < autoModeMinPrompts {
			return nil
		}
		return &AutoModeRecommendation{
			Kind:                "switch_to_auto",
			EstimatedPrompts:    n.EstimatedPromptsInNonAuto,
			NonAutoSessions:     n.NonAutoSessions,
			DestructiveCommands: n.DestructiveInNonAuto,
		}
	}
	if !s.HasAutoModeEnvironment && s.SetupSkillAvailable {
		return &AutoModeRecommendation{Kind: "run_setup"}
	}
	return nil
}

// roundEstimate rounds counts ≥100 to two significant digits.
func roundEstimate(e int) float64 {
	if e < 100 {
		return float64(e)
	}
	s := math.Pow(10, math.Floor(math.Log10(float64(e)))-1)
	return jsRound(float64(e)/s) * s
}

// recommendationLine is the one-line tip appended to the final prompt.
func recommendationLine(r *AutoModeRecommendation) string {
	switch r.Kind {
	case "switch_to_auto":
		return fill(autoModeTipSwitchTpl, toLocale(roundEstimate(r.EstimatedPrompts)), toLocaleInt(r.NonAutoSessions))
	case "run_setup":
		return autoModeTipSetup
	}
	return ""
}

// autoModeTipSetup is the run-setup tip line.
const autoModeTipSetup = "Tip: you already use auto mode — run /auto-mode-setup once to teach it your environment and improve its decisions."
