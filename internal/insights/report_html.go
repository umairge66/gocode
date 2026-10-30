package insights

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// escapeHTML escapes &, <, >, " and '.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return strings.ReplaceAll(s, "'", "&apos;")
}

// textOf is a model-supplied field as text. A missing/null field renders as
// "" rather than aborting the command.
func textOf(o *jsObject, k string) string {
	v, ok := o.get(k)
	if !ok || v == nil {
		return ""
	}
	return jsString(v, true)
}

var boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

// escapeBold escapes text and turns **x** into <strong>x</strong>.
func escapeBold(s string) string {
	return boldRe.ReplaceAllString(escapeHTML(s), "<strong>$1</strong>")
}

// labels maps category keys to display names.
var labels = map[string]string{
	"debug_investigate": "Debug/Investigate", "implement_feature": "Implement Feature", "fix_bug": "Fix Bug",
	"write_script_tool": "Write Script/Tool", "refactor_code": "Refactor Code", "configure_system": "Configure System",
	"create_pr_commit": "Create PR/Commit", "analyze_data": "Analyze Data", "understand_codebase": "Understand Codebase",
	"write_tests": "Write Tests", "write_docs": "Write Docs", "deploy_infra": "Deploy/Infra",
	"warmup_minimal": "Cache Warmup", "fast_accurate_search": "Fast/Accurate Search",
	"correct_code_edits": "Correct Code Edits", "good_explanations": "Good Explanations",
	"proactive_help": "Proactive Help", "multi_file_changes": "Multi-file Changes",
	"handled_complexity": "Multi-file Changes", "good_debugging": "Good Debugging",
	"misunderstood_request": "Misunderstood Request", "wrong_approach": "Wrong Approach",
	"buggy_code": "Buggy Code", "user_rejected_action": "User Rejected Action",
	"gocode_got_blocked": "gocode Got Blocked", "user_stopped_early": "User Stopped Early",
	"wrong_file_or_location": "Wrong File/Location", "excessive_changes": "Excessive Changes",
	"slow_or_verbose": "Slow/Verbose", "tool_failed": "Tool Failed", "user_unclear": "User Unclear",
	"external_issue": "External Issue", "frustrated": "Frustrated", "dissatisfied": "Dissatisfied",
	"likely_satisfied": "Likely Satisfied", "satisfied": "Satisfied", "happy": "Happy",
	"unsure": "Unsure", "neutral": "Neutral", "delighted": "Delighted", "single_task": "Single Task",
	"multi_task": "Multi Task", "iterative_refinement": "Iterative Refinement",
	"exploration": "Exploration", "quick_question": "Quick Question", "fully_achieved": "Fully Achieved",
	"mostly_achieved": "Mostly Achieved", "partially_achieved": "Partially Achieved",
	"not_achieved": "Not Achieved", "unclear_from_transcript": "Unclear", "unhelpful": "Unhelpful",
	"slightly_helpful": "Slightly Helpful", "moderately_helpful": "Moderately Helpful",
	"very_helpful": "Very Helpful", "essential": "Essential",
}

// Fixed orderings for the satisfaction and outcome charts.
var (
	satisfactionOrder = []string{"frustrated", "dissatisfied", "likely_satisfied", "satisfied", "happy", "unsure"}
	outcomeOrder      = []string{"not_achieved", "partially_achieved", "mostly_achieved", "fully_achieved", "unclear_from_transcript"}
)

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// titleCase is g.replaceAll("_"," ").replace(/\b\w/g, c=>c.toUpperCase()).
func titleCase(s string) string {
	b := []byte(strings.ReplaceAll(s, "_", " "))
	for i := range b {
		if isWordByte(b[i]) && (i == 0 || !isWordByte(b[i-1])) && b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// barChart renders a horizontal bar chart: the top-n entries by value, or,
// with order, the listed keys present with a positive value.
func barChart(c *counts, color string, n int, order []string) string {
	var d []countEntry
	if order != nil {
		for _, k := range order {
			if c.has(k) && c.get(k) > 0 {
				d = append(d, countEntry{k, c.get(k)})
			}
		}
	} else {
		d = c.sortedDesc()
		if len(d) > n {
			d = d[:n]
		}
	}
	if len(d) == 0 {
		return `<p class="empty">No data</p>`
	}
	m := math.Inf(-1)
	for _, e := range d {
		m = math.Max(m, e.val)
	}
	rows := make([]string, len(d))
	for i, e := range d {
		label, ok := labels[e.key]
		if !ok {
			label = titleCase(e.key)
		}
		rows[i] = fill(barRowTpl, escapeHTML(label), jsNum(e.val/m*100), color, jsNum(e.val))
	}
	return strings.Join(rows, "\n")
}

// responseTimeChart buckets user response times.
func responseTimeChart(times []float64) string {
	if len(times) == 0 {
		return `<p class="empty">No response time data</p>`
	}
	names := []string{"2-10s", "10-30s", "30s-1m", "1-2m", "2-5m", "5-15m", ">15m"}
	buckets := make([]int, len(names))
	for _, a := range times {
		switch {
		case a < 10:
			buckets[0]++
		case a < 30:
			buckets[1]++
		case a < 60:
			buckets[2]++
		case a < 120:
			buckets[3]++
		case a < 300:
			buckets[4]++
		case a < 900:
			buckets[5]++
		default:
			buckets[6]++
		}
	}
	mx := 0
	for _, b := range buckets {
		if b > mx {
			mx = b
		}
	}
	if mx == 0 {
		return `<p class="empty">No response time data</p>`
	}
	rows := make([]string, len(names))
	for i, nm := range names {
		rows[i] = fill(responseBarRowTpl, nm, jsNum(float64(buckets[i])/float64(mx)*100), jsNum(float64(buckets[i])))
	}
	return strings.Join(rows, "\n")
}

// dayPeriods are the time-of-day buckets.
var dayPeriods = []struct {
	label string
	from  int
}{
	{"Morning (6-12)", 6}, {"Afternoon (12-18)", 12}, {"Evening (18-24)", 18}, {"Night (0-6)", 0},
}

// hourChart renders user messages by time-of-day period.
func hourChart(hours []int) string {
	if len(hours) == 0 {
		return `<p class="empty">No time data</p>`
	}
	byHour := map[int]int{}
	for _, h := range hours {
		byHour[h]++
	}
	cnt := make([]int, len(dayPeriods))
	mx := 0
	for i, p := range dayPeriods {
		for h := p.from; h < p.from+6; h++ {
			cnt[i] += byHour[h]
		}
		if cnt[i] > mx {
			mx = cnt[i]
		}
	}
	if mx == 0 {
		mx = 1
	}
	rows := make([]string, len(dayPeriods))
	for i, p := range dayPeriods {
		rows[i] = fill(hourBarRowTpl, p.label, jsNum(float64(cnt[i])/float64(mx)*100), jsNum(float64(cnt[i])))
	}
	return fill(hourHistogramTpl, strings.Join(rows, "\n"))
}

// hourCountsJSON is the raw hour→count object embedded in the page script.
func hourCountsJSON(hours []int) string {
	c := newCounts()
	for _, h := range hours {
		c.add(jsNum(float64(h)), 1)
	}
	return stringifyJS(c.toObject(), "")
}

var bulletLineRe = regexp.MustCompile(`(?m)^- `)

// paragraphs renders model prose: blank-line separated paragraphs, escaped,
// **bold**, leading "- " bullets and line breaks.
func paragraphs(b string) string {
	if b == "" {
		return ""
	}
	parts := strings.Split(b, "\n\n")
	for i, s := range parts {
		e := escapeBold(s)
		e = bulletLineRe.ReplaceAllString(e, "• ")
		e = strings.ReplaceAll(e, "\n", "<br>")
		parts[i] = fill(paragraphTpl, e)
	}
	return strings.Join(parts, "\n")
}

func objects(arr []any) []*jsObject {
	out := make([]*jsObject, 0, len(arr))
	for _, it := range arr {
		o, _ := it.(*jsObject)
		out = append(out, o)
	}
	return out
}

func cond(ok bool, s func() string) string {
	if ok {
		return s()
	}
	return ""
}

// renderReport builds the self-contained HTML report.
//
// The "Closing the Loop: Feedback for Other Teams" section is always empty
// and is not rendered. The page links no external resources.
func renderReport(e *AggregatedData, s *jsObject, n *AutoModeRecommendation) string {
	m := ""
	if d := s.obj("at_a_glance"); d != nil {
		m = fill(glanceCardTpl,
			cond(d.truthy("whats_working"), func() string { return fill(glanceCardWorkingTpl, escapeBold(textOf(d, "whats_working"))) }),
			cond(d.truthy("whats_hindering"), func() string { return fill(glanceCardHinderingTpl, escapeBold(textOf(d, "whats_hindering"))) }),
			cond(d.truthy("quick_wins"), func() string { return fill(glanceCardQuickWinsTpl, escapeBold(textOf(d, "quick_wins"))) }),
			cond(d.truthy("ambitious_workflows"), func() string {
				return fill(glanceCardAmbitiousTpl, escapeBold(textOf(d, "ambitious_workflows")))
			}),
		)
	}
	g := ""
	if n != nil {
		if n.Kind == "switch_to_auto" {
			g = fill(autoSwitchCardTpl, toLocale(roundEstimate(n.EstimatedPrompts)), toLocaleInt(n.NonAutoSessions),
				cond(n.DestructiveCommands > 0, func() string { return fill(autoSwitchNoteTpl, toLocaleInt(n.DestructiveCommands)) }))
		} else {
			g = autoSetupCardHTML
		}
	}

	w := ""
	if y := s.obj("project_areas").arr("areas"); len(y) > 0 {
		var items []string
		for _, b := range objects(y) {
			items = append(items, fill(projectAreaTpl, escapeHTML(textOf(b, "name")), b.field("session_count"), escapeHTML(textOf(b, "description"))))
		}
		w = fill(projectAreasTpl, strings.Join(items, ""))
	}

	f := ""
	if r := s.obj("interaction_style"); r.truthy("narrative") {
		f = fill(usageTpl, paragraphs(textOf(r, "narrative")),
			cond(r.truthy("key_pattern"), func() string { return fill(keyInsightTpl, escapeHTML(textOf(r, "key_pattern"))) }))
	}

	a := ""
	if h := s.obj("what_works"); len(h.arr("impressive_workflows")) > 0 {
		var items []string
		for _, b := range objects(h.arr("impressive_workflows")) {
			items = append(items, fill(winTpl, escapeHTML(textOf(b, "title")), escapeHTML(textOf(b, "description"))))
		}
		a = fill(winsTpl, cond(h.truthy("intro"), func() string { return fill(sectionIntroTpl, escapeHTML(textOf(h, "intro"))) }), strings.Join(items, ""))
	}

	i := ""
	if r := s.obj("friction_analysis"); len(r.arr("categories")) > 0 {
		var items []string
		for _, b := range objects(r.arr("categories")) {
			ex := ""
			if b.truthy("examples") {
				var lis []string
				for _, x := range b.arr("examples") {
					lis = append(lis, fill(listItemTpl, escapeHTML(jsString(x, true))))
				}
				ex = fill(frictionExamplesTpl, strings.Join(lis, ""))
			}
			items = append(items, fill(frictionCategoryTpl, escapeHTML(textOf(b, "category")), escapeHTML(textOf(b, "description")), ex))
		}
		i = fill(frictionTpl, cond(r.truthy("intro"), func() string { return fill(sectionIntroTpl, escapeHTML(textOf(r, "intro"))) }), strings.Join(items, ""))
	}

	k := ""
	if p := s.obj("suggestions"); p != nil {
		md := ""
		if adds := p.arr("gocode_md_additions"); len(adds) > 0 {
			var items []string
			for idx, b := range objects(adds) {
				scaffold := textOf(b, "prompt_scaffold")
				if scaffold == "" {
					scaffold = textOf(b, "where")
				}
				if scaffold == "" {
					scaffold = "Add to gocode.md"
				}
				si := jsNum(float64(idx))
				items = append(items, fill(mdItemTpl, si, escapeHTML(scaffold), escapeHTML(textOf(b, "addition")), si, escapeHTML(textOf(b, "addition")), si, escapeHTML(textOf(b, "why"))))
			}
			md = fill(mdAdditionsTpl, strings.Join(items, ""))
		}
		feats := ""
		if ft := p.arr("features_to_try"); len(ft) > 0 {
			var items []string
			for _, b := range objects(ft) {
				items = append(items, fill(featureCardTpl, escapeHTML(textOf(b, "feature")), escapeHTML(textOf(b, "one_liner")), escapeHTML(textOf(b, "why_for_you")),
					cond(b.truthy("example_code"), func() string { return fill(featureExampleTpl, escapeHTML(textOf(b, "example_code"))) })))
			}
			feats = fill(featuresTpl, strings.Join(items, ""))
		}
		pats := ""
		if up := p.arr("usage_patterns"); len(up) > 0 {
			var items []string
			for _, b := range objects(up) {
				items = append(items, fill(patternCardTpl, escapeHTML(textOf(b, "title")), escapeHTML(textOf(b, "suggestion")),
					cond(b.truthy("detail"), func() string { return fill(patternDetailTpl, escapeHTML(textOf(b, "detail"))) }),
					cond(b.truthy("copyable_prompt"), func() string { return fill(copyablePromptTpl, escapeHTML(textOf(b, "copyable_prompt"))) })))
			}
			pats = fill(patternsTpl, strings.Join(items, ""))
		}
		k = fill(suggestionsTpl, md, feats, pats)
	}

	bh := ""
	if j := s.obj("on_the_horizon"); len(j.arr("opportunities")) > 0 {
		var items []string
		for _, b := range objects(j.arr("opportunities")) {
			items = append(items, fill(horizonCardTpl, escapeHTML(textOf(b, "title")), escapeHTML(textOf(b, "whats_possible")),
				cond(b.truthy("how_to_try"), func() string { return fill(horizonTipTpl, escapeHTML(textOf(b, "how_to_try"))) }),
				cond(b.truthy("copyable_prompt"), func() string { return fill(horizonPromptTpl, escapeHTML(textOf(b, "copyable_prompt"))) })))
		}
		bh = fill(horizonTpl, cond(j.truthy("intro"), func() string { return fill(sectionIntroTpl, escapeHTML(textOf(j, "intro"))) }), strings.Join(items, ""))
	}

	ne := ""
	if z := s.obj("fun_ending"); z.truthy("headline") {
		ne = fill(funEndingTpl, escapeHTML(textOf(z, "headline")),
			cond(z.truthy("detail"), func() string { return fill(funDetailTpl, escapeHTML(textOf(z, "detail"))) }))
	}

	oe := "" // feedback section: always empty (see doc comment)
	script := fill(reportScriptTpl, hourCountsJSON(e.MessageHours))

	multi := noMultiClaudingHTML
	if e.MultiClauding.OverlapEvents != 0 {
		pct := 0.0
		if e.TotalMessages > 0 {
			pct = jsRound(100 * float64(e.MultiClauding.UserMessagesDuring) / float64(e.TotalMessages))
		}
		multi = fill(multiClaudingTpl, jsNum(float64(e.MultiClauding.OverlapEvents)), jsNum(float64(e.MultiClauding.SessionsInvolved)), jsNum(pct))
	}
	toolErrors := `<p class="empty">No tool errors</p>`
	if e.ToolErrorCategories.len() > 0 {
		toolErrors = barChart(e.ToolErrorCategories, "#dc2626", 6, nil)
	}
	sessionTypes := e.SessionTypes
	if sessionTypes == nil {
		sessionTypes = newCounts()
	}
	return fill(reportPageTpl,
		toLocaleInt(e.TotalMessages),
		jsNum(float64(e.TotalSessions)),
		cond(e.TotalSessionsScanned > e.TotalSessions, func() string { return fill(scannedTotalTpl, toLocaleInt(e.TotalSessionsScanned)) }),
		e.DateRange.Start,
		e.DateRange.End,
		m,
		g,
		toLocaleInt(e.TotalMessages),
		toLocaleInt(e.TotalLinesAdded),
		toLocaleInt(e.TotalLinesRemoved),
		jsNum(float64(e.TotalFilesModified)),
		jsNum(float64(e.DaysActive)),
		jsNum(e.MessagesPerDay),
		w,
		barChart(e.GoalCategories, "#2563eb", 6, nil),
		barChart(e.ToolCounts, "#0891b2", 6, nil),
		barChart(e.Languages, "#10b981", 6, nil),
		barChart(sessionTypes, "#8b5cf6", 6, nil),
		f,
		responseTimeChart(e.UserResponseTimes),
		toFixed1(e.MedianResponseTime),
		toFixed1(e.AvgResponseTime),
		multi,
		hourChart(e.MessageHours),
		toolErrors,
		a,
		barChart(e.Success, "#16a34a", 6, nil),
		barChart(e.Outcomes, "#8b5cf6", 6, outcomeOrder),
		i,
		barChart(e.Friction, "#dc2626", 6, nil),
		barChart(e.Satisfaction, "#eab308", 6, satisfactionOrder),
		k,
		bh,
		ne,
		oe,
		script,
	)
}

// writeReport writes the report to usage-data/report-<stamp>.html and
// usage-data/report.html (mode 0600) and returns the stamped path.
func writeReport(configHome, html, stamp string) (string, error) {
	dir := usageDataDir(configHome)
	path := filepath.Join(dir, "report-"+stamp+".html")
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(path, []byte(html), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.html"), []byte(html), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
