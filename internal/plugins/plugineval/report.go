package plugineval

import (
	"fmt"
	"html"
	"strings"
)

// ReportTitle is evalReportTitle: the plugin under test, else the suite root.
func ReportTitle(r Result) string {
	if r.Suite.PluginID != "" {
		return r.Suite.PluginID
	}
	if len(r.Suite.Plugins) == 1 {
		return r.Suite.Plugins[0].Name
	}
	return r.Suite.Root
}

// RenderHTMLReport writes the self-contained local HTML report (a static
// page: scores, prompts and grader verdicts). Every value is escaped;
// the page loads nothing from the network.
func RenderHTMLReport(r Result) string {
	e := html.EscapeString
	var b strings.Builder
	title := "Eval report — " + ReportTitle(r)
	b.WriteString("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	fmt.Fprintf(&b, "<title>%s</title>", e(title))
	b.WriteString(`<style>
:root{--bg:#fff;--fg:#1b1b1f;--muted:#5f6068;--line:#e3e3e8;--pass:#1a7f37;--fail:#c42b1c;--card:#f6f6f8}
@media (prefers-color-scheme: dark){:root{--bg:#141417;--fg:#ececf1;--muted:#a0a1aa;--line:#2c2c33;--pass:#57c278;--fail:#ff7b6b;--card:#1c1c21}}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,sans-serif}
main{max-width:1100px;margin:0 auto;padding:24px 16px}
h1{font-size:20px;margin:0 0 4px}h2{font-size:16px;margin:28px 0 8px}
.muted{color:var(--muted)}table{border-collapse:collapse;width:100%;margin:8px 0}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);vertical-align:top}
.pass{color:var(--pass)}.fail{color:var(--fail)}
pre{white-space:pre-wrap;word-break:break-word;background:var(--card);padding:10px;border-radius:6px;margin:6px 0}
details{margin:6px 0}.scroll{overflow-x:auto}
</style></head><body><main>`)
	fmt.Fprintf(&b, "<h1>%s</h1>", e(title))
	fmt.Fprintf(&b, "<p class=\"muted\">gocode %s · started %s · %ds · $%.2f · threshold %.2f · ablation %s",
		e(r.GocodeVersion), e(r.StartedAt), r.DurationSeconds, r.CostUSD, r.Suite.Threshold, e(r.Suite.Ablation))
	if r.Partial {
		fmt.Fprintf(&b, " · <span class=\"fail\">partial (%s)</span>", e(r.PartialReason))
	}
	b.WriteString("</p>")
	fmt.Fprintf(&b, "<p>%d of %d cases passed · overall score %.2f · pass rate %.0f%%",
		r.Aggregates.CasesPassed, r.Aggregates.CasesTotal, r.Aggregates.OverallScore, r.Aggregates.OverallPassRate*100)
	if r.Aggregates.MeanDelta != nil {
		fmt.Fprintf(&b, " · mean Δ %s", e(signed(*r.Aggregates.MeanDelta)))
	}
	b.WriteString("</p>")
	b.WriteString("<div class=\"scroll\"><table><thead><tr><th>Case</th><th>Score</th><th>Pass rate</th><th>Without</th><th>Δ</th></tr></thead><tbody>")
	for _, c := range r.Cases {
		cls := "pass"
		if c.Aggregates.Score < r.Suite.Threshold {
			cls = "fail"
		}
		wo, d := "—", "—"
		if c.Aggregates.ScoreWithout != nil {
			wo = fmt.Sprintf("%.2f", *c.Aggregates.ScoreWithout)
		}
		if c.Aggregates.Delta != nil {
			d = signed(*c.Aggregates.Delta)
		}
		fmt.Fprintf(&b, "<tr><td><a href=\"#%s\">%s</a></td><td class=\"%s\">%.2f</td><td>%.0f%%</td><td>%s</td><td>%s</td></tr>",
			e(anchor(c.Name)), e(c.Name), cls, c.Aggregates.Score, c.Aggregates.PassRate*100, e(wo), e(d))
	}
	b.WriteString("</tbody></table></div>")
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "<h2 id=\"%s\">%s</h2><p class=\"muted\">%s · %s · %d runs/arm · max %d turns · timeout %ds</p>",
			e(anchor(c.Name)), e(c.Name), e(c.Dir), e(c.Source), c.RunsPerCase, c.MaxTurns, c.TimeoutSeconds)
		fmt.Fprintf(&b, "<details><summary>Prompt</summary><pre>%s</pre></details>", e(c.PromptMarkdown))
		for _, a := range c.Advisories {
			fmt.Fprintf(&b, "<p class=\"fail\">⚠ %s</p>", e(a))
		}
		arms := []struct {
			name string
			runs []ResultRun
		}{{"with", c.Arms.With}, {"without", c.Arms.Without}}
		for _, arm := range arms {
			if arm.runs == nil {
				continue
			}
			for i, run := range arm.runs {
				fmt.Fprintf(&b, "<details><summary>%s run %d — score %.2f · %d turns · $%.2f</summary>", e(arm.name), i+1, run.Score, run.Turns, run.CostUSD)
				if run.Error != nil {
					fmt.Fprintf(&b, "<p class=\"fail\">error: %s</p>", e(*run.Error))
				}
				b.WriteString("<div class=\"scroll\"><table><tbody>")
				for _, g := range run.Graders {
					mark, cls := "✗", "fail"
					if g.Passed {
						mark, cls = "✓", "pass"
					}
					note := fmt.Sprintf("weight %g", g.Weight)
					if g.WithOnly {
						note = "with-only, not scored"
					}
					fmt.Fprintf(&b, "<tr><td class=\"%s\">%s</td><td>%s</td><td class=\"muted\">%s</td><td>%s</td></tr>", cls, mark, e(g.Name), e(note), e(g.Explanation))
				}
				b.WriteString("</tbody></table></div></details>")
			}
		}
	}
	b.WriteString("</main></body></html>\n")
	return b.String()
}

func anchor(name string) string {
	var b strings.Builder
	b.WriteString("case-")
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
