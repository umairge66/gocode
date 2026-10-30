package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mods"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// `gocode plugin test [dir]` — run a mod's tests.
//
// A "mod" is a plugin whose hooks/hooks.json names hooks MODULES (JavaScript /
// TypeScript files exporting register(on)), and its tests are *.test.ts /
// *.test.tsx files importing their kit from 'gocode/testing'. The command
// finds the test files, runs each one in a child of this binary
// (`gocode plugin test --file <file> <dir>`), collects each child's report
// line and prints a bun-test-style summary.
//
// The child runs its file on the hooks-module engine (internal/mods): the
// file and the mod's module are TypeScript/JSX compiled by esbuild in place
// of a native TypeScript loader, the file runs in an environment like a hooks
// module's (no fs, network or process), its kit is gocode's
// 'gocode/testing', and each test loads the mod afresh with the test's own
// hooks beneath it (mods.RunTestFile).

// pluginTestUsage is the `plugin test` usage text.
var pluginTestUsage = strings.Join([]string{
	"Usage: gocode plugin test [dir]",
	"",
	"Run a mod's tests",
	"",
	"Arguments:",
	"  dir         The mod's folder (default: the current folder)",
	"",
	"Options:",
	"  -h, --help  Display help for command",
	"",
	"Runs every *.test.ts and *.test.tsx under dir, each file in a child of this",
	"binary, in an environment like the one the mod's hooks run in. A test file",
	"imports its kit from 'gocode/testing'. Exits 1 when a test fails.",
	"",
}, "\n")

const (
	// pluginTestReportMark prefixes the child's report line.
	pluginTestReportMark = "gocode-plugin-test-report "
	// pluginTestChildArgc is the child argv length after `plugin test`
	//.
	pluginTestChildArgc = 3
	// pluginTestChildTimeout bounds one test file's child.
	pluginTestChildTimeout = 600 * time.Second
	// maxPluginTestFiles caps discovery.
	maxPluginTestFiles = 1000
)

// pluginTestResult is one test of a file report.
type pluginTestResult struct {
	Title   string  `json:"title"`
	Ms      float64 `json:"ms"`
	Failure *string `json:"failure"`
}

// pluginTestFileReport is the per-file report a child prints.
type pluginTestFileReport struct {
	File    string             `json:"file"`
	Results []pluginTestResult `json:"results"`
	Error   *string            `json:"error,omitempty"`
}

// fileLoadFailure is the report for a test file that failed to load.
func fileLoadFailure(file, msg string) pluginTestFileReport {
	return pluginTestFileReport{File: file, Results: []pluginTestResult{}, Error: &msg}
}

// ranToEnd is a synthetic failed result.
func ranToEnd(failure string) pluginTestResult {
	return pluginTestResult{Title: "the file ran to its end", Ms: 0, Failure: &failure}
}

// pluginTestRunner is the injectable seam for the per-file child.
type pluginTestRunner func(root, file string) pluginTestFileReport

// runPluginTest runs `plugin test`. args are the tokens after `plugin test`.
func runPluginTest(env *Env, args []string) int {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Fprint(env.Stdout, pluginTestUsage)
			return 0
		}
	}
	if len(args) == pluginTestChildArgc && args[0] == "--file" {
		return runPluginTestChild(env, args[1], args[2])
	}
	if msg := pluginTestPolicyRefusal(env); msg != "" {
		fmt.Fprintf(env.Stderr, "gocode plugin test: %s\n", msg)
		return 1
	}
	cwd := "."
	if env.Cwd != nil {
		if wd, err := env.Cwd(); err == nil {
			cwd = wd
		}
	}
	target := "."
	if len(args) > 0 {
		target = args[0]
	}
	root := target
	if !filepath.IsAbs(root) {
		root = filepath.Join(cwd, root)
	}
	root = filepath.Clean(root)
	if err := checkModFolder(root); err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin test: %s\n", err)
		return 1
	}
	files, err := findPluginTestFiles(root)
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin test: %s\n", err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(env.Stderr, "gocode plugin test: no *.test.ts or *.test.tsx under %s\n", root)
		return 1
	}
	return runPluginTestFiles(env.Stdout, root, files, func(root, file string) pluginTestFileReport {
		return pluginTestSpawn(env, root, file)
	})
}

// pluginTestPolicyRefusal is the local policy check: hooks modules cannot load
// where hooks are disabled (disableAllHooks), limited to managed hooks
// (allowManagedHooksOnly) or switched off by a policy.
func pluginTestPolicyRefusal(env *Env) string {
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder)
	var policy map[string]any
	mergedDisable := false
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if or.Source == settingspolicy.SourcePolicy {
			policy = or.Result.Settings
		}
		if b, ok := or.Result.Settings["disableAllHooks"].(bool); ok {
			mergedDisable = b
		}
	}
	policyDisable, _ := policy["disableAllHooks"].(bool)
	managedOnly, _ := policy["allowManagedHooksOnly"].(bool)
	if policyDisable || managedOnly || (mergedDisable && !policyDisable) {
		return "hooks modules are turned off here (disableAllHooks, allowManagedHooksOnly or a policy)"
	}
	return ""
}

// checkModFolder checks the mod folder: the folder must exist and its
// hooks/hooks.json must name at least one module in "modules".
func checkModFolder(dir string) error {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("%s: no such plugin folder", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hooks", "hooks.json"))
	if err != nil {
		return fmt.Errorf("%s: no hooks module to load; there is no hooks/hooks.json naming one in \"modules\"", dir)
	}
	var hj struct {
		Modules []any `json:"modules"`
	}
	if json.Unmarshal(data, &hj) != nil || len(hj.Modules) == 0 {
		return fmt.Errorf("%s: no hooks module to load; hooks/hooks.json names none in \"modules\"", dir)
	}
	return nil
}

var pluginTestFileRe = regexp.MustCompile(`\.test\.tsx?$`)

// findPluginTestFiles finds every *.test.ts(x) below root, skipping
// node_modules and dot-directories, sorted, at most 1000.
func findPluginTestFiles(root string) ([]string, error) {
	var out []string
	stack := []string{root}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			skip := e.Name() == "node_modules" || strings.HasPrefix(e.Name(), ".")
			if e.IsDir() && !skip {
				stack = append(stack, p)
			}
			if e.Type().IsRegular() && pluginTestFileRe.MatchString(e.Name()) {
				out = append(out, p)
			}
			if len(out) > maxPluginTestFiles {
				return nil, fmt.Errorf("more than %d test files under %s; name a narrower folder", maxPluginTestFiles, root)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// runPluginTestFiles runs up to availableParallelism children at once,
// reports printed in file order, then the summary. Exit 1 when anything
// failed (a failed test, or a file that did not load).
func runPluginTestFiles(w io.Writer, root string, files []string, run pluginTestRunner) int {
	start := time.Now()
	par := runtime.NumCPU()
	if par > len(files) {
		par = len(files)
	}
	if par < 1 {
		par = 1
	}
	reports := make([]chan pluginTestFileReport, len(files))
	for i := range reports {
		reports[i] = make(chan pluginTestFileReport, 1)
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			reports[i] <- run(root, f)
		}(i, f)
	}
	pass, fail := 0, 0
	for i := range files {
		r := <-reports[i]
		fmt.Fprint(w, renderPluginTestReport(r, root))
		failed := 0
		for _, res := range r.Results {
			if res.Failure != nil {
				failed++
			}
		}
		pass += len(r.Results) - failed
		fail += failed
		if r.Error != nil {
			fail++
		}
	}
	wg.Wait()
	total := pass + fail
	secs := time.Since(start).Seconds()
	fmt.Fprintf(w, "\n %d pass\n %d fail\nRan %d %s across %d %s. [%.2fs]\n",
		pass, fail, total, pluralWord(total, "test", "tests"), len(files), pluralWord(len(files), "file", "files"), secs)
	if fail == 0 {
		return 0
	}
	return 1
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// renderPluginTestReport renders one file report.
func renderPluginTestReport(r pluginTestFileReport, root string) string {
	rel, err := filepath.Rel(root, r.File)
	if err != nil {
		rel = r.File
	}
	lines := []string{"", escapeControls(rel) + ":"}
	if r.Error != nil {
		lines = append(lines, "(fail) the file did not load", trimTestStack(*r.Error, root))
	}
	for _, res := range r.Results {
		ms := fmt.Sprintf("[%.2fms]", res.Ms)
		title := escapeControls(res.Title)
		if res.Failure == nil {
			lines = append(lines, fmt.Sprintf("(pass) %s %s", title, ms))
		} else {
			lines = append(lines, fmt.Sprintf("(fail) %s %s", title, ms), trimTestStack(*res.Failure, root))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// stackFrameRe matches a "    at fn (file:line:col)" frame.
var stackFrameRe = regexp.MustCompile(`^\s+at (?:.+ \()?(?:\S+:\d+:\d+|native)\)?$`)

// trimTestStack keeps the message, then only the stack frames inside
// root (with root made relative), indented by two spaces.
func trimTestStack(text, root string) string {
	lines := strings.Split(text, "\n")
	cut := len(lines)
	for cut > 0 && stackFrameRe.MatchString(lines[cut-1]) {
		cut--
	}
	kept := append([]string{}, lines[:cut]...)
	for _, l := range lines[cut:] {
		if strings.Contains(l, root) {
			l = strings.ReplaceAll(l, root+string(filepath.Separator), "")
			l = strings.ReplaceAll(l, root+"/", "")
			kept = append(kept, l)
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	for i, l := range kept {
		kept[i] = "  " + escapeControls(l)
	}
	return strings.Join(kept, "\n")
}

// escapeControls escapes control characters: C0 (except tab), DEL and C1
// characters become \uXXXX.
func escapeControls(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r < 0x20 && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pluginTestSpawn runs one test file's child; tests replace it (a test binary
// must never re-execute itself).
var pluginTestSpawn = spawnPluginTestChild

// spawnPluginTestChild runs `<self> plugin test --file <file> <root>`
// in root with stdin closed, stdout captured, stderr passed through.
func spawnPluginTestChild(env *Env, root, file string) pluginTestFileReport {
	self, err := os.Executable()
	if err != nil {
		return fileLoadFailure(file, "its child did not run: "+err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginTestChildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "plugin", "test", "--file", file, root)
	cmd.Dir = root
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = env.Stderr
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fileLoadFailure(file, fmt.Sprintf("it did not finish: its run passed %d ms and was killed, its results with it; a test that never yields (a loop with no await) outlasts its own timeout", pluginTestChildTimeout.Milliseconds()))
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		return fileLoadFailure(file, "its child did not run: "+err.Error())
	}
	return parsePluginTestReport(file, out.String(), code)
}

// parsePluginTestReport parses a child report: the last REPORT_MARK line is
// the report; a missing/invalid/foreign report is a load failure, and a
// non-zero exit after a report adds a synthetic failure.
func parsePluginTestReport(file, stdout string, code int) pluginTestFileReport {
	var line string
	for _, l := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(l, pluginTestReportMark) {
			line = l
		}
	}
	var r pluginTestFileReport
	ok := line != "" && json.Unmarshal([]byte(strings.TrimPrefix(line, pluginTestReportMark)), &r) == nil && r.File == file && r.Results != nil
	if !ok {
		return fileLoadFailure(file, fmt.Sprintf("its run ended without a report (exit code %d)", code))
	}
	if code != 0 {
		r.Results = append(r.Results, ranToEnd(fmt.Sprintf("its run exited with code %d after reporting: a rejection nothing handled, or a throw after its test ended", code)))
	}
	return r
}

// runPluginTestChild runs one test file: the file must lie inside dir; the
// report line is printed on stdout and the child exits 0 (failures live in the
// report).
func runPluginTestChild(env *Env, file, dir string) int {
	rel, err := filepath.Rel(dir, file)
	if err != nil || rel == "" || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		fmt.Fprintf(env.Stderr, "gocode plugin test: %s is not in %s\n", file, dir)
		return 1
	}
	report := pluginTestFileReport{File: file, Results: []pluginTestResult{}}
	results, err := pluginTestRunFile(dir, file)
	if err != nil {
		report = fileLoadFailure(file, err.Error())
	}
	for _, r := range results {
		report.Results = append(report.Results, pluginTestResult{Title: r.Title, Ms: r.Ms, Failure: r.Failure})
	}
	b, _ := json.Marshal(report)
	fmt.Fprintf(env.Stdout, "%s%s\n", pluginTestReportMark, b)
	return 0
}

// pluginTestRunFile runs one test file in this process (the child's work);
// tests replace it.
var pluginTestRunFile = func(dir, file string) ([]mods.TestResult, error) {
	return mods.RunTestFile(context.Background(), dir, file, mods.TestOptions{Deadline: pluginTestChildTimeout - 10*time.Second})
}
