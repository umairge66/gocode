package plugineval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChildSpec is one headless gocode run.
type ChildSpec struct {
	Args    []string
	Dir     string
	Env     []string
	Stdin   string
	Timeout time.Duration
}

// ChildOutcome is what a child produced.
type ChildOutcome struct {
	Records  []map[string]any
	Exit     ChildExit
	TimedOut bool
	Killed   bool
}

// Executor runs a child. The default spawns Self with os/exec.
type Executor interface {
	Run(ctx context.Context, spec ChildSpec) ChildOutcome
}

// maxChildStdout: a child whose stdout passes 64 MB is killed.
const maxChildStdout = 64 << 20

// ProcessExecutor spawns the gocode binary at Self.
type ProcessExecutor struct {
	Self string
}

// Run implements Executor (stdin carries the prompt,
// stdout is parsed line by line as stream-json, stderr's tail is kept).
func (p ProcessExecutor) Run(ctx context.Context, spec ChildSpec) ChildOutcome {
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, p.Self, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = strings.NewReader(spec.Stdin)
	setProcessGroup(cmd)
	var stderr capped
	stderr.max = 64 << 10
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return ChildOutcome{Exit: ChildExit{Kind: "error", Message: err.Error()}}
	}
	if err := cmd.Start(); err != nil {
		return ChildOutcome{Exit: ChildExit{Kind: "error", Message: err.Error()}}
	}
	var records []map[string]any
	overflow := false
	br := bufio.NewReaderSize(io.LimitReader(out, maxChildStdout+1), 1<<20)
	total := 0
	for {
		line, err := br.ReadBytes('\n')
		total += len(line)
		if total > maxChildStdout {
			overflow = true
			killProcessGroup(cmd)
			break
		}
		if s := strings.TrimSpace(string(line)); s != "" {
			if rec, derr := decodeRecord([]byte(s)); derr == nil {
				records = append(records, rec)
			}
		}
		if err != nil {
			break
		}
	}
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	o := ChildOutcome{Records: records}
	switch {
	case ctx.Err() != nil:
		o.Killed = true
		o.Exit = ChildExit{Kind: "error", Message: "interrupted"}
	case overflow:
		o.Killed = true
		o.Exit = ChildExit{Kind: "error", Message: fmt.Sprintf("subprocess stdout exceeded %d bytes — killed", maxChildStdout)}
	case runCtx.Err() == context.DeadlineExceeded:
		o.TimedOut = true
		o.Exit = ChildExit{Kind: "error", Message: fmt.Sprintf("timed out after %ds", int(spec.Timeout.Seconds()))}
	case werr == nil:
		o.Exit = ChildExit{Kind: "ok"}
	default:
		var ee *exec.ExitError
		tail := stderr.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		if errors.As(werr, &ee) {
			code := strconv.Itoa(ee.ExitCode())
			if ee.ExitCode() < 0 {
				code = "signal " + ee.String()
			}
			o.Exit = ChildExit{Kind: "exit", Code: code, StderrTail: tail}
		} else {
			o.Exit = ChildExit{Kind: "error", Message: werr.Error()}
		}
	}
	return o
}

// decodeRecord decodes one stream-json line.
func decodeRecord(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not an object")
	}
	return m, nil
}

// Options configures Run.
type Options struct {
	Target    string
	Cwd       string
	EvalDir   []string
	FrameRoot string
	// PluginID is the installed plugin id when the target named one.
	PluginID string
	Filter   Filter
	// Runs overrides each case's runs (0 = the case's own).
	Runs        int
	Concurrency int
	// Model overrides every case's model; JudgeModel the graders' model.
	Model      string
	JudgeModel string
	// MaxCostUSD is the optional cost ceiling (nil = none).
	MaxCostUSD *float64
	Threshold  float64
	// AllowTools / DisallowTools are the operator's grants and removals.
	AllowTools    []string
	DisallowTools []string
	// Scaffold runs each case's scaffold_script (--scaffold).
	Scaffold       bool
	KeepTemp       bool
	KeepFailedRuns bool
	// Ablation is "auto", "none" or "with-without".
	Ablation string
	// Catalog is the child's built-in tool names (for hf); may be nil.
	Catalog []string
	// ChildArgsPrefix are extra argv entries before the eval flags (e.g. the
	// provider selection), ExtraEnv extra environment for every child.
	ChildArgsPrefix []string
	ExtraEnv        []string
	// WithholdServers keeps the plugins' own MCP servers from starting
	// (--mocks record without --allow-real-servers): the child loads each
	// plugin with --plugin-dir-no-mcp.
	WithholdServers bool
	// AuthPreflight checks the credential once before any case runs.
	AuthPreflight func() (ok bool, message string)
	Version       string
	OnLine        func(string)
	OnNotice      func(string)
	Executor      Executor
	Judge         Judge
}

// Outcome is the result of the package-level Run.
type Outcome struct {
	Report          Report
	ExitCode        int
	Errors          []LoadError
	Root            string
	Cases           []*Case // resolved, parallel to Report.Cases
	Ablation        string
	HarnessFailures int
	AuthFailed      bool
	GitTooOld       bool
}

// runState: the shared accounting across runs.
type runState struct {
	mu               sync.Mutex
	spent            float64
	partialReason    string
	ceilingAnnounced bool
	harnessFailures  int
}

// casePlan is the per-case plan _e builds.
type casePlan struct {
	c          *Case
	runs       int
	arms       []string
	allowed    map[string][]string // per arm
	advisories []Advisory
	results    map[string][]*RunReport
	landed     int
	finalized  bool
}

// Run: discovery, grants, ablation, the scheduled runs, the
// per-case aggregates and the report.
func Run(ctx context.Context, o Options) Outcome {
	line := func(s string) {
		if o.OnLine != nil {
			o.OnLine(s)
		}
	}
	notice := func(s string) {
		if o.OnNotice != nil {
			o.OnNotice(s)
		}
	}
	conc := o.Concurrency
	if conc < 1 {
		conc = 1
	}
	started := time.Now()
	disc := Discover(DiscoverOptions{Target: o.Target, Cwd: o.Cwd, EvalDir: o.EvalDir, FrameRoot: o.FrameRoot, Filter: o.Filter})
	errs := disc.Errors
	opGrants := grantTools(nil, o.AllowTools, nil)
	if len(opGrants.Denied) > 0 {
		errs = append(errs, LoadError{File: "--allow-tools", Error: fmt.Sprintf("refused %d %s: malformed, a wildcard tool name the child does not support, or a tool never available in an evaluation (Monitor, EnterWorktree, ExitWorktree): %s", len(opGrants.Denied), plural(len(opGrants.Denied), "entry", "entries"), strings.Join(opGrants.Denied, ", "))})
	}
	allowTools := opGrants.Allowed
	disTools, disRejected := checkDisallowList(o.DisallowTools, o.Catalog)
	var cases []*Case
	if len(disRejected) > 0 {
		errs = append(errs, LoadError{File: "--disallow-tools", Error: disallowMessage(disRejected) + " No case was run."})
	} else {
		if len(disTools) > 0 {
			notice("Removed from gocode's tools in every case (--disallow-tools): " + strings.Join(disTools, ", "))
		}
		for _, c := range disc.Cases {
			if _, rej := checkDisallowList(c.DisallowedTools, o.Catalog); len(rej) > 0 {
				f := c.CaseFile
				if c.CaseSource == "prose" {
					f = c.CaseDir
				}
				errs = append(errs, LoadError{File: f, Error: "disallowed_tools: " + disallowMessage(rej)})
				continue
			}
			cases = append(cases, c)
		}
	}
	for _, e := range errs {
		line(fmt.Sprintf("✗ %s: %s", e.File, e.Error))
	}
	ablation := o.Ablation
	if ablation == "" || ablation == "auto" {
		ablation = "none"
		for _, c := range cases {
			if len(c.PluginDirs) > 0 {
				ablation = "with-without"
			}
		}
		if ablation == "with-without" {
			notice("Ablation: defaulting to with-without — a plugin resolved from this path, so each case also runs a no-plugin baseline arm (2× runs) and reports Δ; graders marked with-only (including `tool_used: Skill`) become a plugin-fired indicator rather than part of the score. Pass --ablation none for the previous single-arm run and scoring.")
			var single []string
			for _, c := range cases {
				if len(armsFor(c, ablation, true)) == 1 {
					single = append(single, c.Name)
				}
			}
			if len(single) > 0 {
				notice(fmt.Sprintf("%d %s %s single-arm (no Δ) — no plugin to strip, or a replay case whose history carries the plugin into both arms: %s", len(single), plural(len(single), "case", "cases"), plural(len(single), "runs", "run"), strings.Join(single, ", ")))
			}
		}
	}
	auto := o.Ablation == "" || o.Ablation == "auto"
	if !auto && ablation == "with-without" {
		var keep []*Case
		for _, c := range cases {
			if len(c.PluginDirs) > 0 {
				keep = append(keep, c)
				continue
			}
			msg := "ablation requested but no plugin resolved for this case: auto-detection found no plugin.json, .gocode-plugin/plugin.json, or SKILL.md it may load between the case directory and the discovery root. The with and without arms would run identical configs, so Δ would measure nothing. Fix: target the plugin directory itself (or run from within it), declare `plugins:` in the case pointing at a plugin or skill directory beneath the discovery root, or pass --ablation none for a single-arm eval."
			f := c.CaseFile
			if c.CaseSource == "prose" {
				f = c.CaseDir
			}
			errs = append(errs, LoadError{File: f, Error: msg})
			line(fmt.Sprintf("✗ %s: %s", c.Name, msg))
		}
		cases = keep
	}
	out := Outcome{Root: disc.Root, Ablation: ablation, Errors: errs}
	if len(cases) == 0 {
		partial := ""
		code := 0
		if ctx.Err() != nil {
			partial, code = "interrupted", 2
		} else if len(errs) > 0 {
			code = 1
		}
		out.Report = newReport(nil, started, partial, o.Version, nil, conc)
		out.ExitCode = code
		return out
	}
	warnDuplicateNames(cases, line)
	plugins := describePlugins(cases)
	if len(plugins) == 0 {
		line("Plugin under test: none resolved — cases run against baseline gocode")
	}
	if o.WithholdServers {
		for _, p := range plugins {
			if declaresMCPServers(p.Path) {
				notice(fmt.Sprintf("  mocks: the MCP servers %q declares have no mock and are NOT started — their tools are unavailable this run; pass --allow-real-servers to run the plugin's real servers (as you, outside the sandbox), or --mocks off (mock stand-ins from mocks/ are not supported by this build)", p.Name))
			}
		}
	}
	armWord := "this eval"
	if ablation == "with-without" {
		armWord = "the with-arm"
	}
	for _, p := range plugins {
		ver := "(no version)"
		if p.Version != "" {
			ver = "version " + strconv.Quote(p.Version)
		}
		msg := fmt.Sprintf("Plugin under test: %q %s at %q", p.Name, ver, p.Path)
		if p.Problem == "" {
			line(msg)
		} else {
			notice(msg)
		}
		switch p.Problem {
		case "manifest_invalid":
			notice(fmt.Sprintf("  ⚠ its manifest is invalid (%s), so %s will run WITHOUT this plugin", p.ProblemDetail, armWord))
		case "disabled_by_default":
			notice(fmt.Sprintf("  ⚠ its manifest sets defaultEnabled: false and nothing in the eval sandbox enables it, so %s will run WITHOUT this plugin", armWord))
		case "will_not_load":
			notice(fmt.Sprintf("  ⚠ %s, so %s will run WITHOUT this plugin", p.ProblemDetail, armWord))
		}
	}
	if o.AuthPreflight != nil {
		if ok, msg := o.AuthPreflight(); !ok {
			e := "authentication check failed before running any case — every run would use this same credential: " + msg
			out.Errors = append(out.Errors, LoadError{File: disc.Root, Error: e})
			line("✗ " + e)
			out.Report = newReport(nil, started, "auth_failed", o.Version, plugins, conc)
			out.ExitCode, out.AuthFailed = 2, true
			return out
		}
	}
	if err := checkGitVersion(); err != nil {
		out.Errors = append(out.Errors, LoadError{File: disc.Root, Error: err.Error()})
		line("✗ " + err.Error())
		out.Report = newReport(nil, started, "", o.Version, plugins, conc)
		out.ExitCode, out.GitTooOld = 1, true
		return out
	}
	for _, c := range cases {
		if c.ScaffoldScript != "" && !o.Scaffold {
			notice(fmt.Sprintf("⚠ case %q: its scaffold_script is not run without --scaffold (author-supplied bash that runs as you - pass it only for suites you trust); the case runs against an unstaged workspace", c.Name))
		}
		if c.ArtifactPublish {
			line(fmt.Sprintf("  %s: requests artifact publishing; not granted (operator opt-in required) — running without it", c.Name))
		}
		if len(c.GrowthbookOverrides) > 0 {
			line(fmt.Sprintf("  %s: growthbook_overrides not applied — they seed only a run granted artifact publishing", c.Name))
		}
	}
	if ablation == "with-without" {
		total := 0
		for _, c := range cases {
			runs := c.Runs
			if o.Runs > 0 {
				runs = o.Runs
			}
			total += runs * len(armsFor(c, ablation, auto))
		}
		line(fmt.Sprintf("Ablation: 2 arms × %d %s (%d %s)", len(cases), plural(len(cases), "case", "cases"), total, plural(total, "run", "runs")))
	}
	if conc > 1 {
		line(fmt.Sprintf("Concurrency: up to %d runs in flight (all children share your rate limit; progress lines interleave, results keep case order)", conc))
	}

	st := &runState{}
	reports := make([]*CaseReport, len(cases))
	var plans []*casePlan
	type job struct {
		plan  *casePlan
		arm   string
		index int
	}
	var jobs []job
	for i, c := range cases {
		p := planCase(c, o, allowTools, disTools, ablation, auto, line, notice)
		plans = append(plans, p)
		if p == nil {
			continue
		}
		_ = i
		for _, arm := range p.arms {
			for r := 0; r < p.runs; r++ {
				jobs = append(jobs, job{plan: p, arm: arm, index: r})
			}
		}
	}
	var finalizeMu sync.Mutex
	finalize := func(idx int, p *casePlan) {
		finalizeMu.Lock()
		defer finalizeMu.Unlock()
		if p.finalized {
			return
		}
		p.finalized = true
		reports[idx] = finalizeCase(p, disc.Root, o.Threshold, line, st)
	}
	planIndex := map[*casePlan]int{}
	for i, p := range plans {
		if p != nil {
			planIndex[p] = i
		}
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for _, j := range jobs {
		st.mu.Lock()
		if ctx.Err() != nil && st.partialReason == "" {
			st.partialReason = "interrupted"
		} else if o.MaxCostUSD != nil && st.spent >= *o.MaxCostUSD && st.partialReason == "" {
			st.partialReason = "cost_ceiling"
		}
		if st.partialReason == "cost_ceiling" && !st.ceilingAnnounced {
			st.ceilingAnnounced = true
			line(fmt.Sprintf("⚠ cost ceiling $%s hit; skipping remaining cases", formatUSD(*o.MaxCostUSD)))
		}
		stop := st.partialReason != ""
		st.mu.Unlock()
		if stop {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			armLabel := ""
			if len(j.plan.arms) > 1 {
				armLabel = j.arm
			}
			rep := runOnce(ctx, j.plan, j.arm, armLabel, j.index, o, st, line, notice)
			finalizeMu.Lock()
			j.plan.results[j.arm][j.index] = &rep
			j.plan.landed++
			done := j.plan.landed == len(j.plan.arms)*j.plan.runs
			finalizeMu.Unlock()
			if done {
				finalize(planIndex[j.plan], j.plan)
			}
		}(j)
		// Re-check the ceiling only after a run lands when running serially,
		// so the next launch sees what this one spent (the next run is
		// scheduled after the previous settles).
		if conc == 1 {
			wg.Wait()
		}
	}
	wg.Wait()
	for i, p := range plans {
		if p != nil {
			finalize(i, p)
		}
	}
	if o.MaxCostUSD != nil && st.spent > *o.MaxCostUSD && !st.ceilingAnnounced {
		notice(fmt.Sprintf("⚠ cost ceiling $%s exceeded: $%.2f spent by runs already in flight when it was crossed; nothing was skipped", formatUSD(*o.MaxCostUSD), st.spent))
	}
	if st.partialReason == "" && ctx.Err() != nil {
		st.partialReason = "interrupted"
	}
	var final []CaseReport
	for i, c := range cases {
		if reports[i] != nil {
			final = append(final, *reports[i])
			out.Cases = append(out.Cases, c)
		}
	}
	out.Report = newReport(final, started, st.partialReason, o.Version, plugins, conc)
	out.HarnessFailures = st.harnessFailures
	if st.harnessFailures > 0 {
		notice(fmt.Sprintf("⚠ %d %s could not be started by the harness (see the errors above); the suite exits non-zero regardless of scores", st.harnessFailures, plural(st.harnessFailures, "run", "runs")))
	}
	apiErrors := 0
	for _, c := range final {
		for _, r := range append(append([]RunReport{}, c.Runs...), c.RunsWithout...) {
			if r.EndedOnAPIError {
				apiErrors++
			}
		}
	}
	if apiErrors > 0 {
		notice(fmt.Sprintf("⚠ %d %s ended on an API error and scored 0 without grading (each run's error is on its progress line above, or in its `error` field under --json); fix the cause and re-run — the suite exits non-zero regardless of scores", apiErrors, plural(apiErrors, "run", "runs")))
	}
	allPass := true
	for _, c := range final {
		if c.Score < o.Threshold {
			allPass = false
		}
	}
	switch {
	case st.partialReason != "":
		out.ExitCode = 2
	case allPass && len(out.Errors) == 0 && st.harnessFailures == 0 && apiErrors == 0:
		out.ExitCode = 0
	default:
		out.ExitCode = 1
	}
	return out
}

func formatUSD(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func armsFor(c *Case, ablation string, auto bool) []string {
	if ablation != "with-without" || len(c.PluginDirs) == 0 {
		return []string{"with"}
	}
	if auto && c.HistoryFile != "" {
		return []string{"with"}
	}
	return []string{"with", "without"}
}

func planCase(c *Case, o Options, allowTools, disTools []string, ablation string, auto bool, line, notice func(string)) *casePlan {
	runs := c.Runs
	if o.Runs > 0 {
		runs = o.Runs
	}
	caseDis, _ := checkDisallowList(c.DisallowedTools, o.Catalog)
	disallowed := dedupe(append(append([]string{}, caseDis...), disTools...))
	g := grantTools(c.AllowedTools, allowTools, disallowed)
	if len(caseDis) > 0 {
		notice(fmt.Sprintf("  %s: removed from gocode's tools by the case's disallowed_tools: %s", c.Name, strings.Join(caseDis, ", ")))
	}
	if len(g.Denied) > 0 {
		line(fmt.Sprintf("  %s: not granted (missing --allow-tools grant, or a malformed entry): %s", c.Name, strings.Join(g.Denied, ", ")))
	}
	for _, d := range append(append([]string{}, g.Denied...), allowTools...) {
		if parseRule(d).tool == "Artifact" {
			line(fmt.Sprintf("  %s: the Artifact tool is not available inside eval runs; --allow-tools cannot enable it", c.Name))
			break
		}
	}
	p := &casePlan{c: c, runs: runs, arms: armsFor(c, ablation, auto), allowed: map[string][]string{}, results: map[string][]*RunReport{}}
	for _, a := range p.arms {
		p.allowed[a] = g.Allowed
		p.results[a] = make([]*RunReport, runs)
	}
	p.advisories = advisories(c, g.Allowed, disallowed, c.ScaffoldScript != "" && o.Scaffold)
	for _, a := range p.advisories {
		notice(fmt.Sprintf("⚠ case %q: %s", c.Name, a.Text))
	}
	return p
}

func finalizeCase(p *casePlan, root string, threshold float64, line func(string), st *runState) *CaseReport {
	collect := func(arm string) []RunReport {
		var out []RunReport
		for _, r := range p.results[arm] {
			if r != nil {
				out = append(out, *r)
			}
		}
		return out
	}
	with := collect("with")
	st.mu.Lock()
	partial := st.partialReason
	st.mu.Unlock()
	if len(with) == 0 && partial != "" {
		return nil
	}
	var without []RunReport
	hasWithout := len(p.arms) > 1
	if hasWithout {
		without = collect("without")
	}
	score, pass := aggregateRuns(with)
	cr := &CaseReport{Name: p.c.Name, Dir: relDir(root, p.c.CaseDir), Source: p.c.CaseSource, Score: score, PassRate: pass, Runs: with, Advisories: p.advisories}
	skipped := func(rs []RunReport) bool {
		for _, r := range rs {
			if r.SkippedPaidGraders {
				return true
			}
		}
		return false
	}
	apiErr := false
	for _, r := range append(append([]RunReport{}, with...), without...) {
		if r.EndedOnAPIError {
			apiErr = true
		}
	}
	comparable := !skipped(with) && !skipped(without) && !apiErr
	n := len(with) + len(without)
	cost := 0.0
	for _, r := range append(append([]RunReport{}, with...), without...) {
		cost += r.CostUSD
	}
	mark := "✗"
	if score >= threshold {
		mark = "✓"
	}
	if hasWithout && len(without) > 0 {
		ws, wp := aggregateRuns(without)
		cr.RunsWithout = without
		cr.PassRateWithout = &wp
		if comparable {
			d := score - ws
			cr.ScoreWithout, cr.Delta = &ws, &d
			line(fmt.Sprintf("%s %s  with %.2f  without %.2f  Δ %s  (%d %s)  $%.2f", mark, p.c.Name, score, ws, signed(d), n, plural(n, "run", "runs"), cost))
		} else {
			why := "cost ceiling: arms graded under different rules"
			if apiErr {
				why = "a run ended on an API error and was not graded"
			}
			line(fmt.Sprintf("%s %s  with %.2f  Δ — (%s)  (%d %s)  $%.2f", mark, p.c.Name, score, why, n, plural(n, "run", "runs"), cost))
		}
	} else {
		line(fmt.Sprintf("%s %s  score %.2f  (%d %s)  $%.2f", mark, p.c.Name, score, len(with), plural(len(with), "run", "runs"), cost))
	}
	return cr
}

func relDir(root, dir string) string {
	r, err := filepath.Rel(root, dir)
	if err != nil || r == "" {
		return "."
	}
	return filepath.ToSlash(r)
}

func warnDuplicateNames(cases []*Case, line func(string)) {
	byName := map[string][]string{}
	var order []string
	for _, c := range cases {
		if _, ok := byName[c.Name]; !ok {
			order = append(order, c.Name)
		}
		byName[c.Name] = append(byName[c.Name], c.CaseDir)
	}
	for _, n := range order {
		if dirs := byName[n]; len(dirs) > 1 {
			line(fmt.Sprintf("⚠ %d cases share the name %q (%s); the report and --case filter cannot distinguish them", len(dirs), n, strings.Join(dirs, ", ")))
		}
	}
}

// describePlugins: name/version/problem of each plugin under test.
func describePlugins(cases []*Case) []PluginInfo {
	seen := map[string]bool{}
	var out []PluginInfo
	for _, c := range cases {
		for _, d := range c.PluginDirs {
			if seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, describePlugin(d))
		}
	}
	return out
}

func describePlugin(dir string) PluginInfo {
	info := PluginInfo{Name: filepath.Base(dir), Path: dir}
	st, err := os.Stat(dir)
	if strings.HasSuffix(strings.ToLower(dir), ".zip") {
		if err == nil && st.IsDir() {
			info.Problem, info.ProblemDetail = "will_not_load", "a directory whose name ends in .zip — the child treats that name as an archive and fails to load it; rename the directory"
		} else {
			info.Problem = "archive_not_probed"
		}
		return info
	}
	if err != nil || !st.IsDir() {
		info.Problem, info.ProblemDetail = "will_not_load", "not a directory or .zip archive — the child loads it as an empty plugin"
		return info
	}
	for _, rel := range []string{filepath.Join(".gocode-plugin", "plugin.json"), filepath.Join(".claude-plugin", "plugin.json"), "plugin.json"} {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			info.Problem, info.ProblemDetail = "manifest_invalid", err.Error()
			return info
		}
		if n, ok := m["name"].(string); ok && n != "" {
			info.Name = n
		} else {
			info.Problem, info.ProblemDetail = "manifest_invalid", `plugin.json has no "name"`
			return info
		}
		if v, ok := m["version"].(string); ok {
			info.Version = v
		}
		if de, ok := m["defaultEnabled"].(bool); ok && !de {
			info.Problem = "disabled_by_default"
		}
		return info
	}
	return info
}

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// checkGitVersion: git, when present, must be 2.31+ (it must honour
// GIT_CONFIG_COUNT, which switches off hooks and helpers for the run).
func checkGitVersion() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if ctx.Err() != nil {
		return errors.New("git on this host did not report its version within 10 seconds, so gocode plugin eval cannot tell whether it is new enough (git 2.31 or newer) to switch off the repository's git hooks and helper programs for the run. Try again, or install git 2.31 or newer.")
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return errors.New("git on this host did not print a version gocode plugin eval can read, so it cannot tell whether git is new enough (git 2.31 or newer) to switch off the repository's git hooks and helper programs for the run. Run git --version to see what it prints, and install git 2.31 or newer.")
		}
		return nil // git absent
	}
	m := gitVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return errors.New("git on this host did not print a version gocode plugin eval can read, so it cannot tell whether git is new enough (git 2.31 or newer) to switch off the repository's git hooks and helper programs for the run. Run git --version to see what it prints, and install git 2.31 or newer.")
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > 2 || major == 2 && minor >= 31 {
		return nil
	}
	return fmt.Errorf("git %d.%d is too old for gocode plugin eval: it ignores the environment configuration (GIT_CONFIG_COUNT, added in git 2.31) that switches off the repository's git hooks and helper programs for the run. Install git 2.31 or newer.", major, minor)
}

func oneLine200(s string) string {
	s = regexp.MustCompile(`\s*\n\s*`).ReplaceAllString(s, " ")
	return truncateRunes(s, 200)
}

// runOnce: one run of one arm, graded.
func runOnce(ctx context.Context, p *casePlan, arm, armLabel string, index int, o Options, st *runState, line, notice func(string)) RunReport {
	start := time.Now()
	rep := executeRun(ctx, p, arm, armLabel, o, st, line, notice)
	rep.StartedAt = start.UTC().Format("2006-01-02T15:04:05.000Z")
	rep.DurationSeconds = int(time.Since(start).Seconds() + 0.5)
	st.mu.Lock()
	st.spent += rep.CostUSD
	st.mu.Unlock()
	label := ""
	if armLabel != "" {
		label = " [" + armLabel + "]"
	}
	tail := ""
	if rep.Error != nil {
		tail = "  error: " + oneLine200(*rep.Error)
	}
	line(fmt.Sprintf("  %s run %d/%d%s: score %.2f  $%.2f%s", p.c.Name, index+1, p.runs, label, rep.Score, rep.CostUSD, tail))
	for _, g := range rep.Graders {
		mark := "✗"
		if g.Passed {
			mark = "✓"
		}
		w := fmt.Sprintf(" (weight %s)", strconv.FormatFloat(g.Weight, 'f', -1, 64))
		if g.WithOnly {
			w = " [with-only, not scored]"
		}
		line(fmt.Sprintf("    %s %s%s: %s", mark, g.Name, w, oneLine200(g.Explanation)))
	}
	return rep
}

func errPtr(s string) *string { return &s }

func executeRun(ctx context.Context, p *casePlan, arm, armLabel string, o Options, st *runState, line, notice func(string)) (rep RunReport) {
	c := p.c
	failedRun := false
	sb, err := newSandbox()
	if err != nil {
		st.mu.Lock()
		st.harnessFailures++
		st.mu.Unlock()
		return RunReport{Error: errPtr(fmt.Sprintf("run could not start (%v)", err))}
	}
	defer func() {
		keepFailed := failedRun && o.KeepFailedRuns && ctx.Err() == nil
		if o.KeepTemp || keepFailed {
			suffix := ""
			if keepFailed && !o.KeepTemp {
				suffix = " (run failed)"
			}
			line(fmt.Sprintf("  kept temp%s: %s", suffix, sb.Root))
			if sealed, err := sb.seal(); err == nil {
				notice(fmt.Sprintf("⚠ kept %s: %s", sb.Root, sb.sealedNote(sealed)))
			} else {
				notice(fmt.Sprintf("⚠ kept %s: could NOT seal what the plugin under test wrote (%v) — do not run git or anything that loads configuration from a working directory anywhere inside, and remove it (`%s`)", sb.Root, err, sb.removeHint()))
			}
			return
		}
		if err := sb.Cleanup(); err != nil {
			notice(fmt.Sprintf("⚠ could not remove %s (%v); it holds what the plugin under test wrote — do not run git or anything that loads configuration from a working directory inside it, and remove it (`%s`)", sb.Root, err, sb.removeHint()))
		}
	}()
	runCase := *c
	if arm == "without" {
		runCase.PluginDirs = nil
	}
	if c.ScaffoldScript != "" && o.Scaffold {
		script, err := resolveCasePath(c, c.ScaffoldScript, "scaffold_script")
		if err != nil {
			failedRun = true
			return RunReport{Error: errPtr(err.Error())}
		}
		line("  scaffold: " + script)
		if code, stderr := runScaffold(ctx, script, sb); code != 0 {
			failedRun = true
			return RunReport{Error: errPtr(fmt.Sprintf("scaffold failed (exit %d): %s", code, truncateRunes(stderr, 500)))}
		}
	}
	allowed := p.allowed[arm]
	if shellGranted(allowed) {
		failedRun = true
		return RunReport{Error: errPtr(ShellRefusal)}
	}
	before, err := listFiles(sb.Cwd)
	if err != nil {
		failedRun = true
		return RunReport{Error: errPtr(err.Error())}
	}
	args, err := childArgs(&runCase, c, sb, allowed, o)
	if err != nil {
		failedRun = true
		return RunReport{Error: errPtr(err.Error())}
	}
	env, err := runEnv(os.Environ(), c, sb, o.ExtraEnv)
	if err != nil {
		failedRun = true
		return RunReport{Error: errPtr(err.Error())}
	}
	exe := o.Executor
	if exe == nil {
		self, _ := os.Executable()
		exe = ProcessExecutor{Self: self}
	}
	outcome := exe.Run(ctx, ChildSpec{Args: args, Dir: sb.Cwd, Env: env, Stdin: c.Prompt, Timeout: time.Duration(c.TimeoutSeconds) * time.Second})
	tracePath := filepath.Join(sb.OutDir, "trace.jsonl")
	writeTrace(tracePath, outcome.Records)
	trace := ParseTrace(outcome.Records, outcome.TimedOut, outcome.Exit, tracePath)
	if trace.EndedOnAPIError {
		failedRun = true
		r := RunReport{Turns: trace.NumTurns, CostUSD: trace.CostUSD, TracePath: tracePath, EndedOnAPIError: true}
		if trace.HasError {
			r.Error = errPtr(trace.Error)
		}
		return r
	}
	after, err := listFiles(sb.Cwd)
	if err != nil {
		failedRun = true
		return RunReport{Turns: trace.NumTurns, CostUSD: trace.CostUSD, TracePath: tracePath, Error: errPtr(err.Error())}
	}
	st.mu.Lock()
	skipPaid := o.MaxCostUSD != nil && trace.CostUSD >= *o.MaxCostUSD-st.spent
	st.mu.Unlock()
	armName := ""
	if armLabel != "" {
		armName = arm
	}
	results, judgeCost, paidSkipped := gradeRun(ctx, gradeInput{
		c: c, run: &trace, cwdDiff: newFiles(before, after), sandboxCwd: sb.Cwd,
		arm: armName, judgeModel: o.JudgeModel, skipPaid: skipPaid, judge: o.Judge,
	})
	if trace.HasError {
		failedRun = true
	}
	r := RunReport{
		Score: scoreGraders(results), Turns: trace.NumTurns, CostUSD: trace.CostUSD + judgeCost,
		JudgeCostUSD: judgeCost, Graders: results, TracePath: tracePath, SkippedPaidGraders: paidSkipped,
	}
	if trace.HasError {
		r.Error = errPtr(trace.Error)
	}
	if r.Score < o.Threshold {
		failedRun = true
	}
	return r
}

// writeTrace writes trace.jsonl.
func writeTrace(path string, records []map[string]any) {
	var b strings.Builder
	for _, r := range records {
		b.WriteString(stringify(r))
		b.WriteByte('\n')
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
}

// childArgs: the headless child's argv.
func childArgs(run, c *Case, sb *Sandbox, allowed []string, o Options) ([]string, error) {
	args := append([]string{}, o.ChildArgsPrefix...)
	args = append(args, "-p", "--output-format", "stream-json", "--verbose",
		"--max-turns", strconv.Itoa(c.MaxTurns), "--permission-mode", "dontAsk", "--setting-sources", "user")
	model := o.Model
	if model == "" {
		model = c.Model
	}
	if model != "" {
		args = append(args, "--model="+model)
	}
	pluginFlag := "--plugin-dir"
	if o.WithholdServers {
		pluginFlag = "--plugin-dir-no-mcp"
	}
	for _, d := range run.PluginDirs {
		args = append(args, pluginFlag, d)
	}
	var addDirs []string
	for _, d := range c.AddDirs {
		p, err := resolveCasePath(c, d, "add_dirs entry")
		if err != nil {
			return nil, err
		}
		addDirs = append(addDirs, p)
	}
	evalDirs := evalDirsOf(c, o.EvalDir)
	readRoots := append([]string{sb.Cwd, sb.Home, sb.TmpDir}, run.PluginDirs...)
	allow := childAllowRules(allowed, sb.Cwd, readRoots, []string{sb.Cwd, sb.TmpDir}, addDirs)
	if len(allow) > 0 {
		args = append(args, "--allowed-tools="+strings.Join(allow, ","))
	}
	caseDis, _ := checkDisallowList(c.DisallowedTools, o.Catalog)
	opDis, _ := checkDisallowList(o.DisallowTools, o.Catalog)
	dis := childDisallowRules(sb, c, evalDirs, allowed, dedupe(append(caseDis, opDis...)), o.Catalog)
	args = append(args, "--disallowed-tools="+strings.Join(dis, ","))
	if c.HistoryFile != "" {
		// Replaying a saved transcript file is not possible: resume takes a
		// session id, not a transcript path.
		return nil, fmt.Errorf("case %q: context.history_file (resuming a saved transcript) is not supported", c.Name)
	}
	if c.AppendSystem != "" {
		args = append(args, "--append-system-prompt="+c.AppendSystem)
	}
	return args, nil
}

// evalDirsOf are the eval directories of the plugins under test (kept from
// the child's reads so it never sees the graders).
func evalDirsOf(c *Case, segs []string) []string {
	if len(segs) == 0 {
		segs = []string{DefaultEvalDir}
	}
	var out []string
	for _, d := range c.PluginDirs {
		out = append(out, filepath.Join(append([]string{d}, segs...)...))
	}
	return out
}

// declaresMCPServers reports whether a plugin declares MCP servers (.mcp.json
// or the manifest's mcpServers).
func declaresMCPServers(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err == nil {
		return true
	}
	for _, rel := range []string{filepath.Join(".gocode-plugin", "plugin.json"), filepath.Join(".claude-plugin", "plugin.json"), "plugin.json"} {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil && m["mcpServers"] != nil {
			return true
		}
	}
	return false
}
