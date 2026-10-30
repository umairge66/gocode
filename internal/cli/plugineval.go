package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/plugins/plugineval"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/updater"
)

// `gocode plugin eval [target]` and `gocode plugin eval init [name]`.
//
// Reports stay local (--publish-report is refused, --no-publish is the only
// behaviour); artifact-publish grants and growthbook_overrides in a case file
// are accepted but not applied. `eval init --bare <name>` writes a blank case.
// No telemetry is sent.

// evalOptions are the parsed `plugin eval` flags.
type evalOptions struct {
	target         string
	caseGlob       string
	tags           []string
	runs           string
	concurrency    string
	model          string
	judgeModel     string
	maxCostUSD     string
	outputDir      string
	evalDir        string
	json           string // "" none, "-" stdout, else a path
	jsonSet        bool
	threshold      string
	allowTools     []string
	disallowTools  []string
	scaffold       *bool
	trustPlugin    bool
	ablation       string
	mocks          string
	allowReal      bool
	keepTemp       bool
	verbose        bool
	report         string
	reportSet      bool
	publishReport  bool
	noPublish      bool
	unknownOptions []string
}

// variadicFlags take every following non-option token (`<x...>`).
var variadicFlags = map[string]bool{"--tag": true, "--allow-tools": true, "--disallow-tools": true}

// valueFlags take exactly one value.
var valueFlags = map[string]bool{
	"--case": true, "--runs": true, "-j": true, "--concurrency": true, "--model": true,
	"--judge-model": true, "--max-cost-usd": true, "--output-dir": true, "--eval-dir": true,
	"--threshold": true, "--ablation": true, "--mocks": true, "--report": true,
}

// parseEvalArgs parses the tokens after `plugin eval`.
func parseEvalArgs(tokens []string) (evalOptions, []string) {
	var o evalOptions
	var positionals []string
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		name, inline, hasInline := t, "", false
		if strings.HasPrefix(t, "--") {
			if eq := strings.IndexByte(t, '='); eq > 0 {
				name, inline, hasInline = t[:eq], t[eq+1:], true
			}
		}
		next := func() (string, bool) {
			if hasInline {
				return inline, true
			}
			if i+1 < len(tokens) {
				i++
				return tokens[i], true
			}
			return "", false
		}
		switch {
		case variadicFlags[name]:
			var vals []string
			if hasInline {
				vals = append(vals, inline)
			}
			for i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
				i++
				vals = append(vals, tokens[i])
			}
			switch name {
			case "--tag":
				o.tags = append(o.tags, vals...)
			case "--allow-tools":
				o.allowTools = append(o.allowTools, vals...)
			case "--disallow-tools":
				o.disallowTools = append(o.disallowTools, vals...)
			}
		case valueFlags[name]:
			v, _ := next()
			switch name {
			case "--case":
				o.caseGlob = v
			case "--runs":
				o.runs = v
			case "-j", "--concurrency":
				o.concurrency = v
			case "--model":
				o.model = v
			case "--judge-model":
				o.judgeModel = v
			case "--max-cost-usd":
				o.maxCostUSD = v
			case "--output-dir":
				o.outputDir = v
			case "--eval-dir":
				o.evalDir = v
			case "--threshold":
				o.threshold = v
			case "--ablation":
				o.ablation = v
			case "--mocks":
				o.mocks = v
			case "--report":
				o.report, o.reportSet = v, true
			}
		case name == "--json":
			o.jsonSet = true
			o.json = "-"
			if hasInline {
				o.json = inline
			} else if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
				i++
				o.json = tokens[i]
			}
		case t == "--scaffold":
			b := true
			o.scaffold = &b
		case t == "--no-scaffold":
			b := false
			o.scaffold = &b
		case t == "--trust-plugin":
			o.trustPlugin = true
		case t == "--allow-real-servers":
			o.allowReal = true
		case t == "--keep-temp":
			o.keepTemp = true
		case t == "--verbose":
			o.verbose = true
		case t == "--publish-report":
			o.publishReport = true
		case t == "--no-publish":
			o.noPublish = true
		case strings.HasPrefix(t, "-") && t != "-":
			o.unknownOptions = append(o.unknownOptions, t)
		default:
			positionals = append(positionals, t)
		}
	}
	return o, positionals
}

// evalCatalog is the built-in tool catalog the eval grants are checked
// against (the child withholds every catalog tool a case was not granted).
var evalCatalog = []string{
	string(protocol.ToolBash), string(protocol.ToolGlob), string(protocol.ToolGrep), string(protocol.ToolWrite),
	string(protocol.ToolNotebookEdit), string(protocol.ToolTodoWrite), string(protocol.ToolAskUserQuestion),
	string(protocol.ToolSkill), string(protocol.ToolAgent), string(protocol.ToolRead), string(protocol.ToolEdit),
	string(protocol.ToolWebFetch), string(protocol.ToolWebSearch), string(protocol.ToolWorkflow),
	string(protocol.ToolAmendAgent), string(protocol.ToolSearch), "PowerShell", "TaskCreate", "TaskGet", "TaskList",
	"TaskUpdate", "TaskStop", "NotebookRead", "LSP", "Monitor", "EnterWorktree", "ExitWorktree",
}

// runPluginEval is the `plugin eval` entry point.
func runPluginEval(env *Env, d *DispatchDefaults, rest []string) int {
	tokens := tokensAfterPluginEval(rest)
	if len(tokens) > 0 && tokens[0] == "init" {
		return runPluginEvalInit(env, tokens[1:])
	}
	o, pos := parseEvalArgs(tokens)
	if d != nil && d.Model != "" && o.model == "" {
		o.model = d.Model // --model was taken off the argv by the dispatch defaults
	}
	errf := func(format string, a ...any) int {
		fmt.Fprintf(env.Stderr, format+"\n", a...)
		return 1
	}
	if len(o.unknownOptions) > 0 {
		return errf("error: unknown option '%s'", o.unknownOptions[0])
	}
	if len(pos) > 1 {
		return errf("error: too many arguments for 'eval'. Expected 1 argument but got %d.", len(pos))
	}
	if len(pos) == 1 {
		o.target = pos[0]
	}
	if o.ablation != "" && o.ablation != "none" && o.ablation != "with-without" {
		return errf(`--ablation must be "none" or "with-without"`)
	}
	runs := 0
	if o.runs != "" {
		n, err := strconv.Atoi(o.runs)
		if err != nil || n < 1 {
			return errf("Error: --runs must be a positive integer")
		}
		runs = n
	}
	threshold := 1.0
	if o.threshold != "" {
		f, err := strconv.ParseFloat(o.threshold, 64)
		if err != nil || f < 0 || f > 1 {
			return errf("Error: --threshold must be between 0 and 1")
		}
		threshold = f
	}
	if o.mocks != "" && o.mocks != "record" && o.mocks != "off" {
		return errf("Error: --mocks must be record or off")
	}
	if o.jsonSet && o.json != "-" && !strings.HasSuffix(o.json, ".json") {
		if o.json == "" {
			return errf("Error: --json requires a non-empty file path")
		}
		return errf("Error: --json output path must end in .json (got '%s'). If that is your eval target, put it before --json.", o.json)
	}
	if o.reportSet && o.report == "" {
		return errf("Error: --report requires a non-empty file path")
	}
	if o.noPublish && o.publishReport {
		return errf("Error: --no-publish and --publish-report are contradictory — pass at most one.")
	}
	if o.publishReport {
		fmt.Fprintln(env.Stderr, "Publishing is unavailable: this build publishes nothing to gocode.ai. A local copy is written instead.")
	}
	var maxCost *float64
	if o.maxCostUSD != "" {
		f, err := strconv.ParseFloat(o.maxCostUSD, 64)
		if err != nil || f < 0 {
			return errf("Error: --max-cost-usd must be a non-negative number")
		}
		maxCost = &f
	}
	conc := 1
	if o.concurrency != "" {
		n, err := strconv.Atoi(o.concurrency)
		if err != nil || n < 1 || n > 8 || !regexp.MustCompile(`^\d+$`).MatchString(o.concurrency) {
			return errf("Error: --concurrency must be a whole number from 1 to 8")
		}
		conc = n
	}
	cwd, err := envCwd(env)
	if err != nil {
		return errf("Error: %v", err)
	}

	// Target: a path, an installed plugin name, or plugin@marketplace.
	targetDir, pluginID, rerr := resolveEvalTarget(env, o.target, cwd)
	if rerr != nil {
		return errf("%s", rerr.Error())
	}
	if !filepath.IsAbs(targetDir) {
		targetDir = filepath.Join(cwd, targetDir)
	}
	targetDir = filepath.Clean(targetDir)
	st, statErr := os.Stat(targetDir)
	// The plugin the target sits in: the target itself, or the nearest
	// plugin manifest above it inside the working directory.
	scope := targetDir
	if statErr == nil && !st.IsDir() {
		scope = filepath.Dir(targetDir)
	}
	frameRoot := ""
	if statErr == nil {
		frameRoot = framePlugin(scope, cwd, pluginID != "")
	}
	// Trust (ls): an installed plugin, --trust-plugin, or an interactive yes.
	trustDir := frameRoot
	if trustDir == "" {
		trustDir = scope
	}
	if statErr != nil && pluginID == "" {
		return errf("Error: %s could not be found or read - nothing to evaluate (nothing was loaded or run).", targetDir)
	}
	if pluginID == "" && !o.trustPlugin {
		if !(env.StdinTTY && env.StdoutTTY && !o.jsonSet) {
			return errf("Error: %s is not a trusted plugin directory, and this run cannot stop to ask you about it (no interactive terminal, or --json / CI). `gocode plugin eval` loads the plugin and runs its eval suite on this machine as you - only evaluate plugins you trust. Run it in a terminal to confirm, or pass --trust-plugin to assert that you trust this plugin's code and eval suite (e.g. in CI).", trustDir)
		}
		if !confirmEvalTrust(env, trustDir) {
			return errf("Not trusted - nothing from %s was loaded or run.", trustDir)
		}
	}
	// Eval directory: --eval-dir, else the manifest's
	// experimental.evals, else evals/.
	evalDir, source, warn, eerr := resolveEvalDir(o.evalDir, frameRoot)
	if eerr != nil {
		return errf("Error: %v", eerr)
	}
	if warn != "" {
		fmt.Fprintf(env.Stderr, "Warning: %s\n", warn)
	}
	if frameRoot != "" && frameRoot != scope && pluginID == "" {
		fmt.Fprintf(env.Stderr, "Evaluating plugin %s (the target is inside it; the plugin is loaded for the run)\n", frameRoot)
	}
	if source == "manifest" && !o.jsonSet {
		fmt.Fprintf(env.Stderr, "Using eval directory %s/ from %s\n", strings.Join(evalDir, "/"), frameRoot)
	}
	scaffold := o.scaffold != nil && *o.scaffold
	if scaffold {
		fmt.Fprintln(env.Stderr, "Note: --scaffold runs each case's scaffold_script as you. Only use it on case files you (or your org) authored.")
	}

	// The model side: the user's own provider and key, never a hosted route.
	session := newPrintSession(env, d)
	providerName := session.Provider
	if providerName == "" {
		providerName = providerOpenRouter
	}
	childPrefix := []string{"--provider", providerName}
	if session.Model != "" {
		childPrefix = append(childPrefix, "--model="+session.Model)
	}
	var extraEnv []string
	if session.APIKey != "" {
		for _, name := range auth.ProviderEnvVars(auth.Provider(providerName)) {
			extraEnv = append(extraEnv, name+"="+session.APIKey)
		}
	}
	// SIGINT / SIGTERM stop launching runs and finish up.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case sig := <-sigs:
			if sig == os.Interrupt {
				fmt.Fprintln(env.Stderr, "\nInterrupted — finishing up…")
			} else {
				fmt.Fprintln(env.Stderr, "\nTerminated — finishing up…")
			}
			cancel()
		case <-finished:
		}
	}()
	ablation := o.ablation
	if ablation == "" {
		ablation = "auto"
		if pluginID != "" {
			ablation = "with-without"
		}
	}
	target := targetDir
	outcome := plugineval.Run(ctx, plugineval.Options{
		Target: target, Cwd: cwd, EvalDir: evalDir, FrameRoot: frameRoot, PluginID: pluginID,
		Filter: plugineval.Filter{CaseGlob: o.caseGlob, Tags: o.tags},
		Runs:   runs, Concurrency: conc, Model: o.model, JudgeModel: o.judgeModel,
		MaxCostUSD: maxCost, Threshold: threshold, AllowTools: o.allowTools, DisallowTools: o.disallowTools,
		Scaffold: scaffold, KeepTemp: o.keepTemp, KeepFailedRuns: !o.jsonSet, Ablation: ablation,
		Catalog: evalCatalog, ChildArgsPrefix: childPrefix, ExtraEnv: extraEnv,
		WithholdServers: o.mocks != "off" && !o.allowReal,
		AuthPreflight: func() (bool, string) {
			if session.providerErr != nil {
				return false, session.providerErr.Error()
			}
			if session.APIKey == "" {
				return false, fmt.Sprintf("no API key for %s (set %s or add it to the settings \"apiKeys\")", providerName, strings.Join(auth.ProviderEnvVars(auth.Provider(providerName)), " / "))
			}
			return true, ""
		},
		Version:  updater.CurrentVersion,
		Executor: pluginEvalExecutor(),
		Judge:    evalJudge(session),
		OnLine: func(s string) {
			if !o.jsonSet {
				fmt.Fprintln(env.Stderr, escapeControls(s))
			}
		},
		OnNotice: func(s string) { fmt.Fprintln(env.Stderr, escapeControls(s)) },
	})
	result := plugineval.BuildResult(plugineval.ResultInput{
		Report: outcome.Report, Cases: outcome.Cases, Root: outcome.Root, Ablation: outcome.Ablation,
		PluginID: pluginID, ModelOverride: o.model, JudgeModel: o.judgeModel,
		CaseFilter: o.caseGlob, TagFilters: o.tags, Threshold: threshold,
	})
	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	interrupted := ctx.Err() != nil

	if len(outcome.Report.Cases) == 0 && len(outcome.Errors) == 0 && !outcome.Report.Partial && !interrupted {
		filters := []string{}
		if o.caseGlob != "" {
			filters = append(filters, fmt.Sprintf("--case %q", o.caseGlob))
		}
		for _, t := range o.tags {
			filters = append(filters, fmt.Sprintf("--tag %q", t))
		}
		match := ""
		if len(filters) > 0 {
			match = " matching " + strings.Join(filters, " ")
		}
		fmt.Fprintf(env.Stderr, "No eval cases found%s under %s.\n", match, targetDir)
		if !o.jsonSet {
			if len(filters) > 0 {
				fmt.Fprintln(env.Stderr, "Run without the filters to see all cases.")
			} else {
				where := frameRoot
				if where == "" {
					where = targetDir
				}
				fmt.Fprintf(env.Stderr, "Cases are expected in a %s/ directory under %s, each case a directory containing case.yaml or prompt.md.\n", strings.Join(evalDir, "/"), where)
				if pluginID == "" {
					flag := ""
					if source == "flag" {
						flag = " --eval-dir " + strings.Join(evalDir, "/")
					}
					fmt.Fprintf(env.Stderr, "Run `gocode plugin eval init --bare <name>%s` to scaffold a blank case.\n", flag)
				}
			}
		} else if err := writeEvalJSON(env, o.json, resultJSON); err != nil {
			fmt.Fprintf(env.Stderr, "warning: could not write --json result: %v\n", err)
		}
		return 1
	}

	// Results directory: aggregate-result.json (+ report.html).
	resultsBase := outcome.Root
	if pluginID != "" {
		resultsBase = cwd
	}
	segs := evalDir
	if pluginID != "" && source != "flag" {
		segs = []string{plugineval.DefaultEvalDir}
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(result.StartedAt)
	outDir := o.outputDir
	if outDir == "" {
		outDir = filepath.Join(append(append([]string{resultsBase}, segs...), "results", stamp)...)
	} else if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(cwd, outDir)
	}
	wroteResults := false
	if len(outcome.Report.Cases) > 0 || len(outcome.Errors) == 0 {
		if err := writeResultsDir(outDir, o.outputDir == "", resultJSON); err != nil {
			fmt.Fprintf(env.Stderr, "warning: could not write results: %v\n", err)
		} else {
			wroteResults = true
		}
	}
	if o.jsonSet {
		for _, e := range outcome.Errors {
			fmt.Fprintf(env.Stderr, "✗ %s: %s\n", escapeControls(e.File), escapeControls(e.Error))
		}
		if len(outcome.Errors) > 0 && !outcome.AuthFailed && !outcome.GitTooOld {
			fmt.Fprintf(env.Stderr, "%d case file(s) failed to load\n", len(outcome.Errors))
		}
		if err := writeEvalJSON(env, o.json, resultJSON); err != nil {
			fmt.Fprintf(env.Stderr, "warning: could not write --json result: %v\n", err)
			if outcome.ExitCode == 0 {
				return 1
			}
		} else if o.json != "-" {
			fmt.Fprintf(env.Stderr, "Wrote %s\n", o.json)
		}
	} else if !outcome.AuthFailed && !outcome.GitTooOld {
		fmt.Fprintf(env.Stdout, "\n%s\n", plugineval.SummaryTable(outcome.Report))
		if len(outcome.Errors) > 0 {
			fmt.Fprintf(env.Stdout, "\n%d case file(s) failed to load — see above.\n", len(outcome.Errors))
		}
		needsDebug := false
		for _, c := range outcome.Report.Cases {
			if c.Score < threshold {
				needsDebug = true
			}
			for _, r := range c.Runs {
				if r.Error != nil {
					needsDebug = true
				}
			}
		}
		if needsDebug && !o.keepTemp {
			fmt.Fprintln(env.Stdout, "\nRe-run with --keep-temp to preserve each run's sandbox (workspace + trace.jsonl) for debugging.")
		}
	}
	// The local HTML report: --report <path>, else report.html in the
	// results directory.
	reportPath := ""
	switch {
	case o.reportSet:
		reportPath = o.report
		if !filepath.IsAbs(reportPath) {
			reportPath = filepath.Join(cwd, reportPath)
		}
	case wroteResults && len(outcome.Report.Cases) > 0:
		reportPath = filepath.Join(outDir, "report.html")
	}
	if reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err == nil {
			err = os.WriteFile(reportPath, []byte(plugineval.RenderHTMLReport(result)), 0o644)
			if err != nil {
				fmt.Fprintf(env.Stderr, "Couldn't write the report to %s: %v\n", reportPath, err)
			} else {
				fmt.Fprintf(env.Stderr, "Report: %s\n", reportPath)
			}
		}
	}
	if interrupted {
		return 130
	}
	return outcome.ExitCode
}

// pluginEvalExecutor builds the child runner (this binary). Tests replace
// it: a test binary must never re-execute itself.
var pluginEvalExecutor = func() plugineval.Executor {
	self, _ := os.Executable()
	return plugineval.ProcessExecutor{Self: self}
}

// tokensAfterPluginEval returns the argv after `plugin eval`.
func tokensAfterPluginEval(rest []string) []string {
	for i := 0; i+1 < len(rest); i++ {
		if (rest[i] == "plugin" || rest[i] == "plugins") && rest[i+1] == "eval" {
			return rest[i+2:]
		}
	}
	return nil
}

func envCwd(env *Env) (string, error) {
	if env.Cwd != nil {
		return env.Cwd()
	}
	return os.Getwd()
}

// resolveEvalTarget resolves a path, or an installed plugin by name or
// plugin@marketplace (its install path, with the id for the report).
func resolveEvalTarget(env *Env, target, cwd string) (dir, pluginID string, err error) {
	if target == "" {
		return cwd, "", nil
	}
	if strings.ContainsAny(target, `/\`) || strings.HasPrefix(target, ".") {
		return target, "", nil
	}
	m := newPluginManager(env, resolveSessionPaths(env))
	if m == nil {
		return target, "", nil
	}
	installed, ierr := m.ListInstalled()
	if ierr != nil || len(installed) == 0 {
		return target, "", nil
	}
	var matches []string
	paths := map[string]string{}
	for _, p := range installed {
		if len(p.Record.InstallPath) == 0 {
			continue
		}
		name := p.ID
		if at := strings.LastIndexByte(p.ID, '@'); at > 0 {
			name = p.ID[:at]
		}
		if p.ID == target || name == target {
			matches = append(matches, p.ID)
			paths[p.ID] = p.Record.InstallPath
		}
	}
	switch len(matches) {
	case 0:
		return target, "", nil
	case 1:
		if _, serr := os.Stat(paths[matches[0]]); serr != nil {
			return "", "", fmt.Errorf("Error: %s cannot be evaluated where sessions load it: its install path %s is missing. Check the marketplace with `gocode plugin marketplace list`, or pass ./<dir> to evaluate a directory.", matches[0], paths[matches[0]])
		}
		return paths[matches[0]], matches[0], nil
	default:
		return "", "", fmt.Errorf("Error: plugin name %q is ambiguous — matches %s. Specify the full plugin@marketplace identifier.", target, strings.Join(matches, ", "))
	}
}

// framePlugin returns the plugin root: the target when it is a plugin, else
// the nearest plugin manifest above it while still inside the working
// directory (a plugin above the working directory is not adopted).
func framePlugin(dir, cwd string, installed bool) string {
	if installed {
		return dir
	}
	if plugineval.HasPluginManifest(dir) {
		return dir
	}
	stop := cwd
	if rel, err := filepath.Rel(cwd, dir); err != nil || strings.HasPrefix(rel, "..") {
		stop = dir
	}
	return plugineval.NearestPlugin(dir, stop)
}

// resolveEvalDir picks the eval directory: --eval-dir, else the manifest's
// experimental.evals, else evals/.
func resolveEvalDir(flag, pluginRoot string) (segs []string, source, warning string, err error) {
	if flag != "" {
		s, e := plugineval.EvalDirFromFlag(flag)
		return s, "flag", "", e
	}
	if pluginRoot != "" {
		for _, rel := range []string{filepath.Join(".gocode-plugin", "plugin.json"), filepath.Join(".claude-plugin", "plugin.json"), "plugin.json"} {
			b, rerr := os.ReadFile(filepath.Join(pluginRoot, rel))
			if rerr != nil {
				continue
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				return []string{plugineval.DefaultEvalDir}, "default", fmt.Sprintf("could not read %s (it is not a JSON object); using %s/", filepath.Join(pluginRoot, rel), plugineval.DefaultEvalDir), nil
			}
			s, w, ok := plugineval.EvalDirFromManifest(m)
			if ok {
				return s, "manifest", w, nil
			}
			return []string{plugineval.DefaultEvalDir}, "default", w, nil
		}
	}
	return []string{plugineval.DefaultEvalDir}, "default", "", nil
}

// confirmEvalTrust asks whether to trust the plugin directory (asked each run: the folder-trust
// store in trust.go covers workspaces, not plugin directories).
func confirmEvalTrust(env *Env, dir string) bool {
	fmt.Fprintf(env.Stdout, "\n`gocode plugin eval` loads this plugin (its skills, hooks and MCP servers) and runs its eval suite - prompts and graders; scaffold scripts only with --scaffold - on this machine, as you. The run's tool grants limit what a malicious plugin can reach but are not a guarantee against one. A plugin's own suite passing says nothing about whether the plugin is safe.\n  Plugin directory: %s\nOnly evaluate plugins you trust.\n", dir)
	fmt.Fprint(env.Stdout, "Trust this plugin directory? (y/N) ")
	if env.Stdin == nil {
		return false
	}
	line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// writeResultsDir creates the results directory (a default one must be new)
// and writes aggregate-result.json.
func writeResultsDir(dir string, mustBeNew bool, resultJSON []byte) error {
	if mustBeNew {
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("results directory %s already existed — a default results directory is always new; refusing to write into it", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "aggregate-result.json"), append(resultJSON, '\n'), 0o644)
}

// writeEvalJSON writes to stdout for a bare --json, else to the named file.
func writeEvalJSON(env *Env, dest string, resultJSON []byte) error {
	if dest == "-" || dest == "" {
		_, err := fmt.Fprintf(env.Stdout, "%s\n", resultJSON)
		return err
	}
	p := dest
	if cwd, err := envCwd(env); err == nil && !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(resultJSON, '\n'), 0o644)
}

// evalJudge is the LLM grader's judge: one call to the user's configured
// provider (the --judge-model, else the session's model). Images cannot be
// sent through the request path, so an image focus is reported as
// rejected.
func evalJudge(s *printSession) plugineval.Judge {
	return func(ctx context.Context, req plugineval.JudgeRequest) (string, float64, error) {
		if req.Image != nil {
			return "", 0, fmt.Errorf("%w: this build's judge route sends text only", plugineval.ErrJudgeImageRejected)
		}
		if s.providerErr != nil {
			return "", 0, fmt.Errorf("judge call failed: %v", s.providerErr)
		}
		backend, err := s.backend()
		if err != nil {
			return "", 0, fmt.Errorf("judge call failed: %v", err)
		}
		model := strings.TrimSpace(req.Model)
		if model == "" {
			model = strings.TrimSpace(s.Model)
		}
		if model == "" {
			return "", 0, errors.New("judge call failed: no model is configured (pass --judge-model or set a model)")
		}
		mc := provider.NewModelCaller(backend, model)
		resp, err := mc.CallModel(ctx, agent.ModelRequest{
			System:    req.System,
			Messages:  []agent.Message{{Role: "user", Content: []agent.ContentBlock{agent.TextBlock(req.Prompt)}}},
			MaxTokens: 1024,
		})
		if err != nil {
			return "", 0, fmt.Errorf("judge call failed: %v", err)
		}
		var text strings.Builder
		for _, b := range resp.Message.Content {
			if b.Type == "text" {
				text.WriteString(b.Text)
			}
		}
		usd := 0.0
		if m, u, ok := mc.LastTurnUsage(); ok {
			if m == "" {
				m = model
			}
			usd, _ = cost.CostForModel(m, cost.UsageFromAPI(u))
		}
		return text.String(), usd, nil
	}
}

// runPluginEvalInit runs `plugin eval init`. The authoring interview is not
// available (see the file comment); a name writes the blank template.
func runPluginEvalInit(env *Env, tokens []string) int {
	var name, evalDirFlag string
	bare, interactive := false, false
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		switch {
		case t == "--bare":
			bare = true
		case t == "-i" || t == "--interactive" || t == "--interview":
			interactive = true
		case t == "--eval-dir" && i+1 < len(tokens):
			i++
			evalDirFlag = tokens[i]
		case strings.HasPrefix(t, "--eval-dir="):
			evalDirFlag = strings.TrimPrefix(t, "--eval-dir=")
		case strings.HasPrefix(t, "-"):
			fmt.Fprintf(env.Stderr, "error: unknown option '%s'\n", t)
			return 1
		default:
			if name != "" {
				fmt.Fprintln(env.Stderr, "error: too many arguments for 'init'. Expected 1 argument.")
				return 1
			}
			name = t
		}
	}
	if interactive || (!bare && name == "" && env.StdinTTY && env.StdoutTTY) {
		fmt.Fprintln(env.Stderr, "The eval-authoring interview is not available in this build. Pass a case name to write a blank template instead (e.g. `gocode plugin eval init --bare my-case`).")
		return 1
	}
	if name == "" {
		if bare {
			fmt.Fprintln(env.Stderr, "A case name is required for --bare.")
		} else {
			fmt.Fprintln(env.Stderr, "A case name is required when no TTY is available (the interview needs an interactive terminal). Pass a name to write a blank template.")
		}
		return 1
	}
	if err := plugineval.ValidateCaseName(name); err != nil {
		fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}
	cwd, err := envCwd(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}
	pluginRoot := ""
	if plugineval.HasPluginManifest(cwd) {
		pluginRoot = cwd
	}
	segs, _, warn, err := resolveEvalDir(evalDirFlag, pluginRoot)
	if err != nil {
		fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}
	if warn != "" {
		fmt.Fprintf(env.Stderr, "Warning: %s\n", warn)
	}
	if evalDirFlag == "" && !plugineval.LooksLikePluginOrSkill(cwd, segs) {
		fmt.Fprintf(env.Stderr, "Error: %s is not a plugin or skill folder — run `gocode plugin eval init` from the plugin's root folder, or pass --eval-dir to scaffold here on purpose.\n", cwd)
		return 1
	}
	if !bare && !(env.StdinTTY && env.StdoutTTY) {
		fmt.Fprintln(env.Stderr, "No TTY available — writing a blank template. Re-run in a terminal for the authoring interview.")
	}
	p, g, err := plugineval.WriteBareCase(cwd, segs, name)
	if err != nil {
		fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Created %s and %s\n", p, g)
	return 0
}
