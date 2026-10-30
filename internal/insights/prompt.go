package insights

import "strings"

// SkillPreloadText is returned instead of a report when /insights is loaded
// as a skill rather than invoked directly.
const SkillPreloadText = "The /insights report is generated only when the command is invoked directly."

// statsLine is the "N sessions · M messages · Xh · K commits" line.
func statsLine(y *AggregatedData) string {
	first := jsNum(float64(y.TotalSessions)) + " sessions"
	if y.TotalSessionsScanned > y.TotalSessions {
		first = toLocaleInt(y.TotalSessionsScanned) + " sessions total · " + jsNum(float64(y.TotalSessions)) + " analyzed"
	}
	return strings.Join([]string{
		first,
		toLocaleInt(y.TotalMessages) + " messages",
		jsNum(jsRound(y.TotalDurationHours)) + "h",
		jsNum(float64(y.GitCommits)) + " commits",
	}, " · ")
}

// glanceSummary is the markdown "At a Glance" block (or "_No insights
// generated_").
func glanceSummary(insights *jsObject) string {
	i := insights.obj("at_a_glance")
	if i == nil {
		return "_No insights generated_"
	}
	part := func(k string, tpl []string) string {
		if !i.truthy(k) {
			return ""
		}
		return fill(tpl, i.field(k))
	}
	return fill(glanceSummaryTpl,
		part("whats_working", glanceWorkingTpl),
		part("whats_hindering", glanceHinderingTpl),
		part("quick_wins", glanceQuickWinsTpl),
		part("ambitious_workflows", glanceAmbitiousTpl),
	)
}

// promptInput holds the inputs of the insights prompt.
type promptInput struct {
	insightsJSON, reportURL, htmlPath, facetsDir, header, summaryText, recommendationLine string
}

// buildPrompt is the text submitted to the model after /insights runs.
func buildPrompt(p promptInput) string {
	rec := ""
	if p.recommendationLine != "" {
		rec = fill(recommendationTpl, p.recommendationLine)
	}
	return fill(finalPromptTpl, p.insightsJSON, p.reportURL, p.htmlPath, p.facetsDir, p.header, p.summaryText, p.reportURL, rec)
}

// buildHeader is the markdown header (its trailing remote-stats line is
// always empty).
func buildHeader(y *AggregatedData) string {
	return fill(headerTpl, statsLine(y), y.DateRange.Start, y.DateRange.End, "")
}
