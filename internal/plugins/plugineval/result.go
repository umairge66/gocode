package plugineval

import (
	"fmt"
	"strings"
	"time"

	"github.com/rivo/uniseg"
)

// RunReport is one run of one arm (only the camelCase Result document is
// serialized).
type RunReport struct {
	Score              float64
	Turns              int
	CostUSD            float64
	JudgeCostUSD       float64
	Graders            []GraderResult
	TracePath          string
	Error              *string
	StartedAt          string
	DurationSeconds    int
	SkippedPaidGraders bool
	EndedOnAPIError    bool
}

// Advisory is a pre-run warning that a grader cannot pass.
type Advisory struct {
	Grader string
	Text   string
}

// CaseReport is one case's aggregate.
type CaseReport struct {
	Name            string
	Dir             string
	Source          string
	Score           float64
	PassRate        float64
	Runs            []RunReport
	RunsWithout     []RunReport // nil when the case ran single-arm
	ScoreWithout    *float64
	PassRateWithout *float64
	Delta           *float64
	Advisories      []Advisory
}

// PluginInfo is a plugin under test.
type PluginInfo struct {
	Name          string `json:"name"`
	Version       string `json:"version,omitempty"`
	Path          string `json:"path"`
	Problem       string `json:"problem,omitempty"`
	ProblemDetail string `json:"-"`
}

// Report is the aggregate of a suite run.
type Report struct {
	GocodeVersion   string
	StartedAt       time.Time
	DurationSeconds int
	CostUSD         float64
	Partial         bool
	PartialReason   string
	Concurrency     int
	Plugins         []PluginInfo
	Cases           []CaseReport
}

var partialReasonText = map[string]string{
	"interrupted":  "interrupted",
	"cost_ceiling": "cost ceiling hit",
	"auth_failed":  "stopped: authentication failed",
}

// scoreGraders: the weighted share of scored graders that passed.
func scoreGraders(gs []GraderResult) float64 {
	var got, total float64
	for _, g := range gs {
		if g.WithOnly {
			continue
		}
		total += g.Weight
		if g.Passed {
			got += g.Weight
		}
	}
	if total == 0 {
		return 0
	}
	return got / total
}

// aggregateRuns: the mean score and the share of perfect runs.
func aggregateRuns(runs []RunReport) (score, passRate float64) {
	if len(runs) == 0 {
		return 0, 0
	}
	perfect := 0
	for _, r := range runs {
		score += r.Score
		if r.Score >= 1 {
			perfect++
		}
	}
	return score / float64(len(runs)), float64(perfect) / float64(len(runs))
}

func newReport(cases []CaseReport, started time.Time, partial, version string, plugins []PluginInfo, concurrency int) Report {
	total := 0.0
	for _, c := range cases {
		for _, r := range c.Runs {
			total += r.CostUSD
		}
		for _, r := range c.RunsWithout {
			total += r.CostUSD
		}
	}
	return Report{
		GocodeVersion:   version,
		StartedAt:       started,
		DurationSeconds: int(time.Since(started).Seconds() + 0.5),
		CostUSD:         total,
		Partial:         partial != "",
		PartialReason:   partial,
		Concurrency:     concurrency,
		Plugins:         plugins,
		Cases:           cases,
	}
}

// --- the camelCase result document ------------------------------------------

// ResultGrader is a grader verdict in the result document.
type ResultGrader struct {
	Name        string  `json:"name"`
	Passed      bool    `json:"passed"`
	Weight      float64 `json:"weight"`
	Explanation string  `json:"explanation"`
	WithOnly    bool    `json:"withOnly"`
	Scored      bool    `json:"scored"`
	JudgeVotes  []bool  `json:"judgeVotes,omitempty"`
	Evidence    string  `json:"evidence,omitempty"`
}

// ResultRun is a run in the result document.
type ResultRun struct {
	Score              float64        `json:"score"`
	Passed             bool           `json:"passed"`
	Turns              int            `json:"turns"`
	CostUSD            float64        `json:"costUsd"`
	JudgeCostUSD       float64        `json:"judgeCostUsd"`
	DurationSeconds    *int           `json:"durationSeconds,omitempty"`
	StartedAt          string         `json:"startedAt,omitempty"`
	Error              *string        `json:"error"`
	TracePath          string         `json:"tracePath"`
	SkippedPaidGraders bool           `json:"skippedPaidGraders"`
	Graders            []ResultGrader `json:"graders"`
}

// ResultCaseGrader describes a grader of a case.
type ResultCaseGrader struct {
	Name           string         `json:"name"`
	Type           string         `json:"type"`
	Weight         float64        `json:"weight"`
	GraderMarkdown string         `json:"graderMarkdown,omitempty"`
	Config         map[string]any `json:"config"`
}

// ResultArms are the with / without runs of a case.
type ResultArms struct {
	With    []ResultRun `json:"with"`
	Without []ResultRun `json:"without,omitempty"`
}

// ResultCaseAggregates are a case's scores.
type ResultCaseAggregates struct {
	Score           float64  `json:"score"`
	PassRate        float64  `json:"passRate"`
	ScoreWithout    *float64 `json:"scoreWithout,omitempty"`
	PassRateWithout *float64 `json:"passRateWithout,omitempty"`
	Delta           *float64 `json:"delta,omitempty"`
}

// ResultCase is a case in the result document.
type ResultCase struct {
	Name           string               `json:"name"`
	Dir            string               `json:"dir"`
	Source         string               `json:"source"`
	PromptMarkdown string               `json:"promptMarkdown"`
	Model          string               `json:"model,omitempty"`
	RunsPerCase    int                  `json:"runsPerCase"`
	TimeoutSeconds int                  `json:"timeoutSeconds"`
	MaxTurns       int                  `json:"maxTurns"`
	Graders        []ResultCaseGrader   `json:"graders"`
	Arms           ResultArms           `json:"arms"`
	Advisories     []string             `json:"advisories,omitempty"`
	Aggregates     ResultCaseAggregates `json:"aggregates"`
}

// ResultSuite describes the run.
type ResultSuite struct {
	Root          string       `json:"root"`
	Ablation      string       `json:"ablation"`
	ModelOverride string       `json:"modelOverride,omitempty"`
	JudgeModel    string       `json:"judgeModel,omitempty"`
	CaseFilter    string       `json:"caseFilter,omitempty"`
	TagFilters    []string     `json:"tagFilters,omitempty"`
	Threshold     float64      `json:"threshold"`
	Concurrency   int          `json:"concurrency,omitempty"`
	PluginID      string       `json:"pluginId,omitempty"`
	Plugins       []PluginInfo `json:"plugins"`
}

// ResultAggregates are the suite's scores.
type ResultAggregates struct {
	CasesTotal      int      `json:"casesTotal"`
	CasesPassed     int      `json:"casesPassed"`
	OverallScore    float64  `json:"overallScore"`
	OverallPassRate float64  `json:"overallPassRate"`
	MeanDelta       *float64 `json:"meanDelta,omitempty"`
}

// Result is the document written to aggregate-result.json and --json
// .
type Result struct {
	SchemaVersion   int              `json:"schemaVersion"`
	GocodeVersion   string           `json:"gocodeVersion"`
	StartedAt       string           `json:"startedAt"`
	DurationSeconds int              `json:"durationSeconds"`
	CostUSD         float64          `json:"costUsd"`
	Partial         bool             `json:"partial"`
	PartialReason   string           `json:"partialReason,omitempty"`
	Suite           ResultSuite      `json:"suite"`
	Cases           []ResultCase     `json:"cases"`
	Aggregates      ResultAggregates `json:"aggregates"`
}

// ResultInput is BuildResult's argument.
type ResultInput struct {
	Report        Report
	Cases         []*Case // the resolved cases, parallel to Report.Cases
	Root          string
	Ablation      string
	PluginID      string
	ModelOverride string
	JudgeModel    string
	CaseFilter    string
	TagFilters    []string
	Threshold     float64
}

// BuildResult builds the camelCase result document.
func BuildResult(in ResultInput) Result {
	r := in.Report
	res := Result{
		SchemaVersion:   1,
		GocodeVersion:   r.GocodeVersion,
		StartedAt:       r.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		DurationSeconds: r.DurationSeconds,
		CostUSD:         r.CostUSD,
		Partial:         r.Partial,
		PartialReason:   r.PartialReason,
		Suite: ResultSuite{
			Root: in.Root, Ablation: in.Ablation, PluginID: in.PluginID,
			ModelOverride: in.ModelOverride, JudgeModel: in.JudgeModel,
			CaseFilter: in.CaseFilter, TagFilters: in.TagFilters,
			Threshold: in.Threshold, Concurrency: r.Concurrency,
			Plugins: r.Plugins,
		},
		Cases: []ResultCase{},
	}
	if res.Suite.Plugins == nil {
		res.Suite.Plugins = []PluginInfo{}
	}
	var scores, rates, deltas []float64
	for i, cr := range r.Cases {
		var c *Case
		if i < len(in.Cases) {
			c = in.Cases[i]
		}
		rc := resultCase(c, cr)
		res.Cases = append(res.Cases, rc)
		scores = append(scores, rc.Aggregates.Score)
		rates = append(rates, rc.Aggregates.PassRate)
		if rc.Aggregates.Delta != nil {
			deltas = append(deltas, *rc.Aggregates.Delta)
		}
		if rc.Aggregates.Score >= in.Threshold {
			res.Aggregates.CasesPassed++
		}
	}
	res.Aggregates.CasesTotal = len(res.Cases)
	res.Aggregates.OverallScore = mean(scores)
	res.Aggregates.OverallPassRate = mean(rates)
	if len(deltas) > 0 {
		m := mean(deltas)
		res.Aggregates.MeanDelta = &m
	}
	return res
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func resultCase(c *Case, cr CaseReport) ResultCase {
	rc := ResultCase{Name: cr.Name, Dir: cr.Dir, Source: cr.Source, Graders: []ResultCaseGrader{}}
	if c != nil {
		rc.PromptMarkdown = c.Prompt
		rc.Model = c.Model
		rc.RunsPerCase = c.Runs
		rc.TimeoutSeconds = c.TimeoutSeconds
		rc.MaxTurns = c.MaxTurns
		for _, g := range c.Graders {
			cg := ResultCaseGrader{Name: g.Name, Type: g.Type, Weight: g.Weight, Config: g.config}
			if cg.Config == nil {
				cg.Config = map[string]any{}
			}
			if g.Type == "llm" || g.Type == "baseline" {
				cg.GraderMarkdown = g.Criteria
			}
			rc.Graders = append(rc.Graders, cg)
		}
	}
	rc.Arms.With = resultRuns(cr.Runs)
	if cr.RunsWithout != nil {
		rc.Arms.Without = resultRuns(cr.RunsWithout)
	}
	for _, a := range cr.Advisories {
		rc.Advisories = append(rc.Advisories, a.Text)
	}
	rc.Aggregates = ResultCaseAggregates{Score: cr.Score, PassRate: cr.PassRate, ScoreWithout: cr.ScoreWithout, PassRateWithout: cr.PassRateWithout, Delta: cr.Delta}
	return rc
}

func resultRuns(runs []RunReport) []ResultRun {
	out := make([]ResultRun, 0, len(runs))
	for _, r := range runs {
		rr := ResultRun{
			Score: r.Score, Passed: r.Score >= 1, Turns: r.Turns, CostUSD: r.CostUSD,
			JudgeCostUSD: r.JudgeCostUSD, StartedAt: r.StartedAt, Error: r.Error,
			TracePath: r.TracePath, SkippedPaidGraders: r.SkippedPaidGraders,
			Graders: []ResultGrader{},
		}
		if r.StartedAt != "" {
			d := r.DurationSeconds
			rr.DurationSeconds = &d
		}
		for _, g := range r.Graders {
			rr.Graders = append(rr.Graders, ResultGrader{Name: g.Name, Passed: g.Passed, Weight: g.Weight, Explanation: g.Explanation, WithOnly: g.WithOnly, Scored: g.Scored, JudgeVotes: g.JudgeVotes, Evidence: g.Evidence})
		}
		out = append(out, rr)
	}
	return out
}

// --- the summary table -------------------------------------------------------

// SummaryTable renders the per-case summary table.
func SummaryTable(r Report) string {
	var lines []string
	width := 4
	for _, c := range r.Cases {
		if w := displayWidth(c.Name); w > width {
			width = w
		}
	}
	ablation := false
	for _, c := range r.Cases {
		if c.RunsWithout != nil {
			ablation = true
		}
	}
	if ablation {
		lines = append(lines, pad("CASE", width)+"  "+pad("WITH", 6)+pad("W/OUT", 6)+pad("Δ", 7)+pad("RUNS", 5)+pad("COST", 8)+"NOTES")
	} else {
		lines = append(lines, pad("CASE", width)+"  "+pad("SCORE", 6)+pad("PASS%", 6)+pad("RUNS", 5)+pad("COST", 8)+"NOTES")
	}
	for _, c := range r.Cases {
		runs := len(c.Runs) + len(c.RunsWithout)
		cost := 0.0
		for _, x := range c.Runs {
			cost += x.CostUSD
		}
		for _, x := range c.RunsWithout {
			cost += x.CostUSD
		}
		note := caseNote(c)
		if ablation {
			wo, d := "—", "—"
			if c.ScoreWithout != nil {
				wo = fmt.Sprintf("%.2f", *c.ScoreWithout)
			}
			if c.Delta != nil {
				d = signed(*c.Delta)
			}
			lines = append(lines, pad(c.Name, width)+"  "+pad(fmt.Sprintf("%.2f", c.Score), 6)+pad(wo, 6)+pad(d, 7)+pad(fmt.Sprint(runs), 5)+pad(fmt.Sprintf("$%.2f", cost), 8)+note)
		} else {
			lines = append(lines, pad(c.Name, width)+"  "+pad(fmt.Sprintf("%.2f", c.Score), 6)+pad(fmt.Sprintf("%d%%", int(c.PassRate*100+0.5)), 6)+pad(fmt.Sprint(len(c.Runs)), 5)+pad(fmt.Sprintf("$%.2f", cost), 8)+note)
		}
	}
	lines = append(lines, "")
	partial := ""
	if r.Partial {
		reason := r.PartialReason
		if reason == "" {
			reason = "interrupted"
		}
		partial = fmt.Sprintf(" · ⚠ partial (%s)", partialReasonText[reason])
	}
	if ablation {
		var ds []float64
		for _, c := range r.Cases {
			if c.Delta != nil {
				ds = append(ds, *c.Delta)
			}
		}
		if len(ds) > 0 {
			lines = append(lines, fmt.Sprintf("%d case(s) · mean Δ %s · %ds · $%.2f%s", len(r.Cases), signed(mean(ds)), r.DurationSeconds, r.CostUSD, partial))
			return strings.Join(lines, "\n")
		}
	}
	lines = append(lines, fmt.Sprintf("%d case(s) · %ds · $%.2f%s", len(r.Cases), r.DurationSeconds, r.CostUSD, partial))
	return strings.Join(lines, "\n")
}

func signed(d float64) string {
	if d > 0 {
		return fmt.Sprintf("+%.2f", d)
	}
	return fmt.Sprintf("%.2f", d)
}

// caseNote: the first run error, else the heaviest failed grader (or
// its advisory, when no run passed that grader).
func caseNote(c CaseReport) string {
	var worst *GraderResult
	for _, r := range c.Runs {
		for i := range r.Graders {
			g := r.Graders[i]
			if !g.Passed && (worst == nil || g.Weight > worst.Weight) {
				worst = &g
			}
		}
		if r.Error != nil {
			return *r.Error
		}
	}
	if worst == nil {
		return ""
	}
	for _, a := range c.Advisories {
		if a.Grader != worst.Name {
			continue
		}
		passedSomewhere := false
		for _, r := range c.Runs {
			for _, g := range r.Graders {
				if g.Name == worst.Name && g.Passed {
					passedSomewhere = true
				}
			}
		}
		if !passedSomewhere {
			return a.Text
		}
	}
	return worst.Name + ": " + worst.Explanation
}

func displayWidth(s string) int { return uniseg.StringWidth(s) }

// pad: left-align in width columns (a too-long cell gets one space).
func pad(s string, width int) string {
	w := displayWidth(s)
	if w > width {
		return s + " "
	}
	return s + strings.Repeat(" ", width-w)
}
