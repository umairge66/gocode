package plugineval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const yamlCase = `schema_version: "1.0"
name: greets
tags: [smoke]
execution:
  prompt: Say hello
  max_turns: 4
  allowed_tools: [Read, Write]
runs: 2
graders:
  - type: regex
    name: says-hello
    pattern: "hel+o"
    flags: i
  - type: tool_used
    name: used-skill
    tool: Skill
  - type: file_exists
    name: wrote-file
    path: "out/*.txt"
`

func TestParseCaseDefaultsAndValidation(t *testing.T) {
	root := realTempDir(t)
	write(t, root, map[string]string{"evals/greet/case.yaml": yamlCase})
	doc, err := loadCaseYAML(filepath.Join(root, "evals/greet/case.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCase(doc)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "greets" || c.MaxTurns != 4 || c.TimeoutSeconds != 300 || c.Runs != 2 || len(c.Graders) != 3 {
		t.Fatalf("case = %+v", c)
	}
	g := c.Graders[0]
	if g.Target.Kind != "last_message" || g.Match != "contains" || g.Weight != 1 || g.Flags != "i" {
		t.Fatalf("regex grader defaults = %+v", g)
	}
	if !c.Graders[2].Exists {
		t.Fatal("file_exists defaults to exists: true")
	}

	bad := []struct{ doc, want string }{
		{`name: x`, "missing required field schema_version"},
		{"schema_version: \"2.0\"\nname: x", "requires a newer gocode"},
		{"schema_version: \"1.0\"\nname: x\nexecution: {prompt: hi}\ngraders: []", "graders: Array must contain at least 1 element(s)"},
		{"schema_version: \"1.0\"\nname: x\nexecution: {prompt: hi}\ngraders: [{type: regex, name: a, pattern: x, bogus: 1}]", "Unrecognized key(s) in object: 'bogus'"},
		{"schema_version: \"1.0\"\nname: x\nexecution: {prompt: hi}\ngraders: [{type: regex, name: a, pattern: x, flags: z}]", "must be JS RegExp flags"},
		{"schema_version: \"1.0\"\nname: x\nexecution: {prompt: hi}\ngraders: [{type: regex, name: a, pattern: x, match: some}]", "must be contains | not_contains | count:N"},
		{"schema_version: \"1.0\"\nname: x\nexecution: {prompt: hi}\ngraders: [{type: llm, name: a, criteria: c}, {type: llm, name: a, criteria: d}]", `duplicate grader name "a"`},
		{"schema_version: \"1.0\"\nname: x\nexecution: {max_turns: 3}\ngraders: [{type: llm, name: a, criteria: c}]", "execution.prompt is required"},
		{"schema_version: \"1.0\"\nname: x\nexecution: {prompt: \"TODO: describe what the agent should do\"}\ngraders: [{type: llm, name: a, criteria: c}]", "still the blank `init` template"},
		{"schema_version: \"1.0\"\nname: x\nexecution: {prompt: hi, max_turns: 500}\ngraders: [{type: llm, name: a, criteria: c}]", "execution.max_turns: Number must be less than or equal to 200"},
	}
	for _, b := range bad {
		write(t, root, map[string]string{"bad/case.yaml": b.doc})
		doc, err := loadCaseYAML(filepath.Join(root, "bad/case.yaml"))
		if err == nil {
			_, err = ParseCase(doc)
		}
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%q: err = %v, want %q", b.doc, err, b.want)
		}
	}
}

func TestProseCaseMerge(t *testing.T) {
	root := realTempDir(t)
	write(t, root, map[string]string{
		"evals/p1/prompt.md":           "---\nmax_turns: 5\ntags: [a]\nallowed_tools: [Read]\n---\n\nSummarize the README.\n",
		"evals/p1/graders/quality.md":  "---\ntype: llm\nweight: 2\n---\nThe summary mentions installation.\n",
		"evals/p1/graders/mentions.md": "---\ntype: regex\n---\ninstall\n",
		"evals/p1/graders/notes.md":    "no frontmatter: ignored\n",
	})
	d := Discover(DiscoverOptions{Target: root, Cwd: root})
	if len(d.Errors) != 0 || len(d.Cases) != 1 {
		t.Fatalf("errors=%v cases=%d", d.Errors, len(d.Cases))
	}
	c := d.Cases[0]
	if c.Name != "p1" || c.CaseSource != "prose" || c.MaxTurns != 5 || c.Prompt != "Summarize the README." {
		t.Fatalf("case = %+v", c)
	}
	if len(c.Graders) != 2 || c.Graders[0].Name != "mentions" || c.Graders[0].Pattern != "install" || c.Graders[1].Criteria != "The summary mentions installation." || c.Graders[1].Weight != 2 {
		t.Fatalf("graders = %+v", c.Graders)
	}
	// An unknown prompt.md key is an error.
	write(t, root, map[string]string{"evals/p2/prompt.md": "---\nbogus: 1\n---\nhi\n"})
	d = Discover(DiscoverOptions{Target: root, Cwd: root})
	if len(d.Errors) != 1 || !strings.Contains(d.Errors[0].Error, `unknown frontmatter key "bogus"`) {
		t.Fatalf("errors = %v", d.Errors)
	}
}

func TestDiscoverEvalDirAndFilters(t *testing.T) {
	root := realTempDir(t)
	write(t, root, map[string]string{
		".gocode-plugin/plugin.json":       `{"name":"demo","version":"1.2.0"}`,
		"evals/a/case.yaml":                yamlCase,
		"evals/group/b/case.yaml":          strings.Replace(yamlCase, "name: greets", "name: other", 1),
		"evals/results/old/case.yaml":      yamlCase,
		"evals/mocks/srv/case.yaml":        yamlCase,
		"node_modules/x/evals/c/case.yaml": yamlCase,
		"docs/case.yaml":                   yamlCase,
		"quality/evals/d/case.yaml":        strings.Replace(yamlCase, "name: greets", "name: custom", 1),
	})
	d := Discover(DiscoverOptions{Target: root, Cwd: root, FrameRoot: root})
	var names []string
	for _, c := range d.Cases {
		names = append(names, c.Name)
	}
	// The default eval dir is route-independent: quality/evals is found too.
	if strings.Join(names, ",") != "greets,other,custom" {
		t.Fatalf("names = %v errors=%v", names, d.Errors)
	}
	if d.Cases[0].PluginDirs[0] != root {
		t.Fatalf("plugin dir = %v", d.Cases[0].PluginDirs)
	}
	d = Discover(DiscoverOptions{Target: root, Cwd: root, FrameRoot: root, EvalDir: []string{"quality", "evals"}})
	if len(d.Cases) != 1 || d.Cases[0].Name != "custom" {
		t.Fatalf("custom eval dir: %+v", d.Cases)
	}
	d = Discover(DiscoverOptions{Target: root, Cwd: root, FrameRoot: root, Filter: Filter{CaseGlob: "o*"}})
	if len(d.Cases) != 1 || d.Cases[0].Name != "other" {
		t.Fatalf("glob filter: %+v", d.Cases)
	}
	d = Discover(DiscoverOptions{Target: root, Cwd: root, Filter: Filter{Tags: []string{"nope"}}})
	if len(d.Cases) != 0 {
		t.Fatalf("tag filter: %+v", d.Cases)
	}
	// Targeting a case file runs just that case.
	d = Discover(DiscoverOptions{Target: filepath.Join(root, "evals/a/case.yaml"), Cwd: root, FrameRoot: root})
	if len(d.Cases) != 1 || d.Cases[0].Name != "greets" || d.Root != root {
		t.Fatalf("case file target: %+v root=%s", d.Cases, d.Root)
	}
}

func TestEvalDirFlagAndManifest(t *testing.T) {
	for _, bad := range []string{"/abs", "../up", "", "a/../../b"} {
		if _, err := EvalDirFromFlag(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	segs, err := EvalDirFromFlag("quality/evals/")
	if err != nil || strings.Join(segs, "/") != "quality/evals" {
		t.Fatalf("segs=%v err=%v", segs, err)
	}
	segs, _, ok := EvalDirFromManifest(map[string]any{"experimental": map[string]any{"evals": []any{"q/e", "x"}}})
	if !ok || strings.Join(segs, "/") != "q/e" {
		t.Fatalf("manifest segs=%v", segs)
	}
	if _, w, ok := EvalDirFromManifest(map[string]any{"evals": "x"}); ok || !strings.Contains(w, "top-level") {
		t.Fatalf("misplaced key: %q", w)
	}
}

func TestGraders(t *testing.T) {
	cwd := realTempDir(t)
	write(t, cwd, map[string]string{"report.md": "All tests PASSED\n", "img.png": "\x89PNG\r\n\x1a\nrest"})
	run := &RunTrace{
		LastAssistantText: "Hello there, hello again",
		ToolCalls: []ToolCall{
			{Name: "Read", InputText: `{"file_path":"/x/README.md"}`},
			{Name: "Skill", InputText: `{"skill":"demo:greet"}`},
			{Name: "Write", InputText: `{"file_path":"/x/out/a.txt"}`},
		},
		Records: []map[string]any{{"type": "assistant"}},
	}
	in := gradeInput{c: &Case{CaseDir: cwd}, run: run, cwdDiff: "out/a.txt\nreport.md", sandboxCwd: cwd}
	two := 2
	zero := 0
	cases := []struct {
		g    Grader
		pass bool
		why  string
	}{
		{Grader{Type: "regex", Name: "r1", Target: Target{Kind: "last_message"}, Pattern: "HELLO", Flags: "i", Match: "contains", Weight: 1}, true, "matched HELLO"},
		{Grader{Type: "regex", Name: "r2", Target: Target{Kind: "last_message"}, Pattern: "hello", Flags: "i", Match: "count:2", Weight: 1}, true, "found 2 matches (expected 2)"},
		{Grader{Type: "regex", Name: "r3", Target: Target{Kind: "last_message"}, Pattern: "bye", Match: "not_contains", Weight: 1}, true, "pattern absent as expected"},
		{Grader{Type: "regex", Name: "r4", Target: Target{Kind: "file", Path: "report.md"}, Pattern: `(?<=All )tests`, Match: "contains", Weight: 1}, true, "matched"},
		{Grader{Type: "regex", Name: "r5", Target: Target{Kind: "file", Path: "img.png"}, Pattern: "x", Match: "contains", Weight: 1}, false, "is an image (image/png)"},
		{Grader{Type: "regex", Name: "r6", Target: Target{Kind: "file", Path: "missing.md"}, Pattern: "x", Match: "contains", Weight: 1}, false, "grader threw: focus file missing.md does not exist"},
		{Grader{Type: "tool_used", Name: "t1", Tool: "Skill", InputMatch: "demo:", Weight: 1}, true, "Skill called 1x (expected 1..∞)"},
		{Grader{Type: "tool_used", Name: "t2", Tool: "Bash", Max: &zero, Min: &zero, Weight: 1}, true, "Bash called 0x (expected 0..0)"},
		{Grader{Type: "tool_used", Name: "t3", Tool: "Read", Min: &two, Weight: 1}, false, "Read called 1x (expected 2..∞)"},
		{Grader{Type: "tool_order", Name: "o1", Before: ToolRef{Tool: "Read"}, After: ToolRef{Tool: "Write"}, Weight: 1}, true, "Read@0 precedes Write@2"},
		{Grader{Type: "tool_order", Name: "o2", Before: ToolRef{Tool: "Write"}, After: ToolRef{Tool: "Read"}, Weight: 1}, false, "Write@2 does NOT precede Read@0"},
		{Grader{Type: "tool_order", Name: "o3", Before: ToolRef{Tool: "Edit"}, After: ToolRef{Tool: "Read"}, Weight: 1}, false, `"before" tool Edit never called`},
		{Grader{Type: "file_exists", Name: "f1", Path: "out/*.txt", Exists: true, Weight: 1}, true, "out/*.txt exists as expected"},
		{Grader{Type: "file_exists", Name: "f2", Path: "**/*.log", Exists: false, Weight: 1}, true, "**/*.log absent as expected"},
		{Grader{Type: "file_exists", Name: "f3", Path: "*.txt", Exists: true, Weight: 1}, false, "*.txt missing (expected present)"},
	}
	for _, c := range cases {
		r, _ := runGrader(context.Background(), c.g, in)
		if r.Passed != c.pass || !strings.Contains(r.Explanation, c.why) {
			t.Errorf("%s: passed=%v %q, want %v %q", c.g.Name, r.Passed, r.Explanation, c.pass, c.why)
		}
	}
}

func TestLLMGraderVotesAndCost(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	replies := []string{"PASS", "FAIL", "pass."}
	judge := func(_ context.Context, req JudgeRequest) (string, float64, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts = append(prompts, req.Prompt)
		r := replies[len(prompts)-1]
		return r, 0.01, nil
	}
	run := &RunTrace{LastAssistantText: "the answer"}
	in := gradeInput{c: &Case{}, run: run, judge: judge, judgeModel: "m"}
	g := Grader{Type: "llm", Name: "q", Criteria: "Is it right?", Focus: Target{Kind: "last_message"}, Weight: 1}
	r, cost := runGrader(context.Background(), g, in)
	if !r.Passed || r.Explanation != "judge votes: PASS FAIL PASS" || len(r.JudgeVotes) != 3 || cost < 0.0299 || cost > 0.0301 {
		t.Fatalf("r=%+v cost=%v", r, cost)
	}
	if !strings.Contains(prompts[0], "Criterion:\nIs it right?") || !strings.Contains(prompts[0], "Agent output (last_message):\nthe answer") || !strings.HasSuffix(prompts[0], "Respond with exactly one word: PASS or FAIL.") {
		t.Fatalf("prompt = %q", prompts[0])
	}
	// Cost ceiling: paid graders are skipped.
	in.skipPaid = true
	r, _ = runGrader(context.Background(), g, in)
	if r.Passed || r.Explanation != "skipped: cost ceiling" {
		t.Fatalf("r=%+v", r)
	}
}

func TestScoringAndWithOnly(t *testing.T) {
	c := &Case{Graders: []Grader{
		{Type: "tool_used", Name: "fired", Tool: "Skill", Weight: 1},
		{Type: "regex", Name: "ok", Target: Target{Kind: "last_message"}, Pattern: "ok", Match: "contains", Weight: 3},
	}}
	run := &RunTrace{LastAssistantText: "ok"}
	res, _, _ := gradeRun(context.Background(), gradeInput{c: c, run: run, arm: "with"})
	if len(res) != 2 || !res[0].WithOnly || res[0].Scored || res[1].WithOnly {
		t.Fatalf("with arm: %+v", res)
	}
	if s := scoreGraders(res); s != 1 {
		t.Fatalf("score = %v (with-only must not count)", s)
	}
	res, _, _ = gradeRun(context.Background(), gradeInput{c: c, run: run, arm: "without"})
	if len(res) != 1 || res[0].Name != "ok" {
		t.Fatalf("without arm drops with-only graders: %+v", res)
	}
	res, _, _ = gradeRun(context.Background(), gradeInput{c: c, run: run})
	if len(res) != 2 || res[0].WithOnly || scoreGraders(res) != 0.75 {
		t.Fatalf("single arm scores every grader: %+v %v", res, scoreGraders(res))
	}
	if s, p := aggregateRuns([]RunReport{{Score: 1}, {Score: 0.5}}); s != 0.75 || p != 0.5 {
		t.Fatalf("aggregate %v %v", s, p)
	}
}

func TestParseTrace(t *testing.T) {
	recs := []map[string]any{
		{"type": "assistant", "message": map[string]any{"id": "m1", "content": []any{
			map[string]any{"type": "text", "text": "Looking"},
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "/a"}},
		}}},
		{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "data", "is_error": false},
		}}},
		{"type": "assistant", "message": map[string]any{"id": "m2", "content": []any{map[string]any{"type": "text", "text": "Done"}}}},
		{"type": "result", "num_turns": 2.0, "total_cost_usd": 0.12, "is_error": false},
	}
	tr := ParseTrace(recs, false, ChildExit{Kind: "ok"}, "/t")
	if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Output != "data" || tr.ToolCalls[0].InputText != `{"file_path":"/a"}` {
		t.Fatalf("calls = %+v", tr.ToolCalls)
	}
	if tr.LastAssistantText != "Done" || tr.NumTurns != 2 || tr.CostUSD != 0.12 || tr.HasError {
		t.Fatalf("trace = %+v", tr)
	}
	tr = ParseTrace(recs[:1], false, ChildExit{Kind: "exit", Code: "1"}, "/t")
	if !tr.HasError || tr.Error != "exit 1: (no stderr)" || tr.NumTurns != 1 {
		t.Fatalf("exit trace = %+v", tr)
	}
}

func TestToolGrants(t *testing.T) {
	g := grantTools([]string{"Read", "Write", "Bash(npm test:*)", "WebFetch", "Edit(/etc/passwd)"}, []string{"Write", "Bash(npm test:*)", "Monitor"}, []string{"Agent"})
	want := map[string]bool{"Write": true, "Bash(npm test:*)": true, "Read": true}
	if len(g.Allowed) != 3 {
		t.Fatalf("allowed = %v", g.Allowed)
	}
	for _, a := range g.Allowed {
		if !want[a] {
			t.Fatalf("unexpected grant %q in %v", a, g.Allowed)
		}
	}
	denied := strings.Join(g.Denied, ",")
	for _, d := range []string{"Monitor", "WebFetch", "Edit(/etc/passwd)"} {
		if !strings.Contains(denied, d) {
			t.Errorf("%s should be denied: %v", d, g.Denied)
		}
	}
	allow := childAllowRules([]string{"Read", "Write", "Read(docs/*.md)"}, "/s/home/cwd", []string{"/s/home/cwd", "/p"}, []string{"/s/home/cwd", "/s/tmp"}, nil)
	joined := strings.Join(allow, ",")
	for _, w := range []string{"Read(/s/home/cwd/docs/*.md)", "Read(/p/**)", "Glob(/p/**)", "Edit(/s/home/cwd/**)", "Write(/s/tmp/**)"} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing %s in %s", w, joined)
		}
	}
	if strings.Contains(joined, "Read,") || strings.HasPrefix(joined, "Write,") {
		t.Errorf("unscoped read/write grants must be confined: %s", joined)
	}
	if !shellGranted([]string{"Bash(npm test:*)"}) || shellGranted([]string{"Read"}) {
		t.Fatal("shellGranted")
	}
	tools, rej := checkDisallowList([]string{"Agent", "Bash(rm:*)", "mcp__x__y"}, []string{"Agent"})
	if len(tools) != 1 || len(rej) != 2 {
		t.Fatalf("tools=%v rej=%v", tools, rej)
	}
}

func TestAdvisories(t *testing.T) {
	c := &Case{Graders: []Grader{
		{Type: "file_exists", Name: "wrote", Path: "x", Exists: true},
		{Type: "tool_used", Name: "fetched", Tool: "WebFetch"},
		{Type: "tool_used", Name: "never", Tool: "Monitor"},
	}}
	adv := advisories(c, []string{"Read"}, nil, false)
	if len(adv) != 3 {
		t.Fatalf("adv = %+v", adv)
	}
	if !strings.Contains(adv[0].Text, "add Write (or Edit / Bash)") || !strings.Contains(adv[1].Text, "--allow-tools WebFetch") || !strings.Contains(adv[2].Text, "never available inside an eval run") {
		t.Fatalf("adv = %+v", adv)
	}
}

func TestRunEnv(t *testing.T) {
	sb := &Sandbox{Root: "/s", Cwd: "/s/home/cwd", ConfigDir: "/s/config", Home: "/s/home", OutDir: "/s/out", TmpDir: "/s/tmp"}
	c := &Case{Name: "c", Env: map[string]string{"EVAL_MODE": "fast"}}
	env, err := runEnv([]string{"HOME=/real", "PATH=/bin", "GOCODE_CODE_MAX_TURNS=99", "OPENROUTER_API_KEY=k"}, c, sb, []string{"GOCODE_PROVIDER=openrouter"})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	if m["HOME"] != "/s/home" || m["GOCODE_CONFIG_DIR"] != "/s/config" || m["PATH"] != "/bin" || m["EVAL_MODE"] != "fast" || m["GOCODE_CODE_EVAL_CONFINED"] != "1" || m["GOCODE_PROVIDER"] != "openrouter" || m["OPENROUTER_API_KEY"] != "k" {
		t.Fatalf("env = %v", m)
	}
	if _, has := m["GOCODE_CODE_MAX_TURNS"]; has {
		t.Fatal("harness control variables must not leak into the child")
	}
	if m["GIT_CONFIG_KEY_0"] != "core.pager" || m["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatal("git hardening missing")
	}
	c.Env = map[string]string{"PATH": "/evil"}
	if _, err := runEnv(nil, c, sb, nil); err == nil || !strings.Contains(err.Error(), "only EVAL_* keys") {
		t.Fatalf("err = %v", err)
	}
}

// fakeExecutor plays a child: it records the spec and writes a file into the
// run directory, then returns a canned trace.
type fakeExecutor struct {
	mu    sync.Mutex
	specs []ChildSpec
	reply func(spec ChildSpec) ChildOutcome
}

func (f *fakeExecutor) Run(_ context.Context, spec ChildSpec) ChildOutcome {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	return f.reply(spec)
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

func TestRunEndToEndWithAblation(t *testing.T) {
	root := realTempDir(t)
	write(t, root, map[string]string{
		".gocode-plugin/plugin.json": `{"name":"demo","version":"0.1.0"}`,
		"evals/greet/case.yaml":      yamlCase,
	})
	fe := &fakeExecutor{reply: func(spec ChildSpec) ChildOutcome {
		withPlugin := hasArg(spec.Args, "--plugin-dir")
		text := "nothing"
		if withPlugin {
			text = "Hello!"
			_ = os.MkdirAll(filepath.Join(spec.Dir, "out"), 0o755)
			_ = os.WriteFile(filepath.Join(spec.Dir, "out", "a.txt"), []byte("x"), 0o644)
		}
		content := []any{map[string]any{"type": "text", "text": text}}
		if withPlugin {
			content = append(content, map[string]any{"type": "tool_use", "id": "t1", "name": "Skill", "input": map[string]any{"skill": "demo:greet"}})
		}
		return ChildOutcome{Exit: ChildExit{Kind: "ok"}, Records: []map[string]any{
			{"type": "assistant", "message": map[string]any{"id": "m", "content": content}},
			{"type": "result", "num_turns": 1.0, "total_cost_usd": 0.05},
		}}
	}}
	var lines []string
	var mu sync.Mutex
	collect := func(s string) { mu.Lock(); lines = append(lines, s); mu.Unlock() }
	out := Run(context.Background(), Options{
		Target: root, Cwd: root, FrameRoot: root, Threshold: 1, Ablation: "auto",
		AllowTools: []string{"Write"}, Executor: fe, Version: "test", OnLine: collect, OnNotice: collect,
	})
	if out.ExitCode != 0 || out.Ablation != "with-without" || len(out.Report.Cases) != 1 {
		t.Fatalf("exit=%d ablation=%s cases=%d errors=%v\n%s", out.ExitCode, out.Ablation, len(out.Report.Cases), out.Errors, strings.Join(lines, "\n"))
	}
	cr := out.Report.Cases[0]
	if len(cr.Runs) != 2 || len(cr.RunsWithout) != 2 || cr.Score != 1 || cr.ScoreWithout == nil || *cr.ScoreWithout != 0 || cr.Delta == nil || *cr.Delta != 1 {
		t.Fatalf("case report = %+v", cr)
	}
	if len(fe.specs) != 4 {
		t.Fatalf("runs = %d", len(fe.specs))
	}
	s := fe.specs[0]
	for _, want := range []string{"-p", "--output-format", "stream-json", "--verbose", "--max-turns", "4", "--permission-mode", "dontAsk", "--setting-sources", "user", "--plugin-dir"} {
		if !hasArg(s.Args, want) {
			t.Errorf("child argv lacks %q: %v", want, s.Args)
		}
	}
	if s.Stdin != "Say hello" {
		t.Errorf("prompt on stdin = %q", s.Stdin)
	}
	var dis string
	for _, a := range s.Args {
		if strings.HasPrefix(a, "--disallowed-tools=") {
			dis = a
		}
	}
	if !strings.Contains(dis, "Bash") || !strings.Contains(dis, "Monitor") || !strings.Contains(dis, filepath.ToSlash(filepath.Join(root, "evals"))+"/**") {
		t.Errorf("disallowed = %s", dis)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Plugin under test: \"demo\" version \"0.1.0\"", "Ablation: 2 arms × 1 case (4 runs)", "greets run 1/2 [with]: score 1.00", "✓ greets  with 1.00  without 0.00  Δ +1.00  (4 runs)  $0.20"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing line %q in:\n%s", want, joined)
		}
	}
	res := BuildResult(ResultInput{Report: out.Report, Cases: out.Cases, Root: out.Root, Ablation: out.Ablation, Threshold: 1})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	agg := doc["aggregates"].(map[string]any)
	if doc["schemaVersion"].(float64) != 1 || agg["casesPassed"].(float64) != 1 || agg["meanDelta"].(float64) != 1 {
		t.Fatalf("result doc = %s", b)
	}
	c0 := doc["cases"].([]any)[0].(map[string]any)
	if c0["promptMarkdown"] != "Say hello" || len(c0["arms"].(map[string]any)["without"].([]any)) != 2 {
		t.Fatalf("case doc = %v", c0)
	}
	table := SummaryTable(out.Report)
	if !strings.Contains(table, "CASE    WITH  W/OUT Δ      RUNS COST    NOTES") || !strings.Contains(table, "1 case(s) · mean Δ +1.00") {
		t.Fatalf("table:\n%s", table)
	}
}

func TestRunCostCeilingAndFailures(t *testing.T) {
	root := realTempDir(t)
	write(t, root, map[string]string{
		"evals/a/case.yaml": strings.Replace(yamlCase, "runs: 2", "runs: 3", 1),
	})
	fe := &fakeExecutor{reply: func(ChildSpec) ChildOutcome {
		return ChildOutcome{Exit: ChildExit{Kind: "exit", Code: "1", StderrTail: "boom"}, Records: []map[string]any{{"type": "result", "num_turns": 1.0, "total_cost_usd": 1.0, "is_error": true, "result": "model refused"}}}
	}}
	ceiling := 1.5
	var lines []string
	out := Run(context.Background(), Options{Target: root, Cwd: root, Threshold: 1, Ablation: "none", MaxCostUSD: &ceiling, Executor: fe, OnLine: func(s string) { lines = append(lines, s) }})
	if out.ExitCode != 2 || !out.Report.Partial || out.Report.PartialReason != "cost_ceiling" {
		t.Fatalf("exit=%d partial=%v %q", out.ExitCode, out.Report.Partial, out.Report.PartialReason)
	}
	if len(fe.specs) != 2 {
		t.Fatalf("runs launched = %d (the ceiling is checked before each launch)", len(fe.specs))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "⚠ cost ceiling $1.5 hit; skipping remaining cases") || !strings.Contains(joined, "error: exit 1: model refused · stderr: boom") {
		t.Fatalf("lines:\n%s", joined)
	}
}

func TestRunRefusesShellGrant(t *testing.T) {
	root := realTempDir(t)
	write(t, root, map[string]string{"evals/a/case.yaml": strings.Replace(yamlCase, "allowed_tools: [Read, Write]", "allowed_tools: [Bash]", 1)})
	fe := &fakeExecutor{reply: func(ChildSpec) ChildOutcome { t.Error("a shell-granting run must not start"); return ChildOutcome{} }}
	out := Run(context.Background(), Options{Target: root, Cwd: root, Threshold: 1, Ablation: "none", AllowTools: []string{"Bash"}, Executor: fe})
	if out.ExitCode != 1 || out.Report.Cases[0].Runs[0].Error == nil || *out.Report.Cases[0].Runs[0].Error != ShellRefusal {
		t.Fatalf("exit=%d report=%+v", out.ExitCode, out.Report.Cases)
	}
}

func TestWriteBareCase(t *testing.T) {
	dir := realTempDir(t)
	p, g, err := WriteBareCase(dir, nil, "first-case")
	if err != nil || p != filepath.Join("evals", "first-case", "prompt.md") || g != filepath.Join("evals", "first-case", "graders", "criteria.md") {
		t.Fatalf("p=%s g=%s err=%v", p, g, err)
	}
	if _, _, err := WriteBareCase(dir, nil, "first-case"); err == nil {
		t.Fatal("overwrite must be refused")
	}
	for _, bad := range []string{"../x", "a/b", ".hidden", "con", "x:y"} {
		if _, _, err := WriteBareCase(dir, nil, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The blank template is refused by the loader until filled in.
	d := Discover(DiscoverOptions{Target: dir, Cwd: dir})
	if len(d.Errors) != 1 || !strings.Contains(d.Errors[0].Error, "blank `init` template") {
		t.Fatalf("errors = %v", d.Errors)
	}
}

// realTempDir is t.TempDir() with symlinks resolved (macOS reaches the temp
// dir through /var -> /private/var; Windows may use an 8.3 short name), the
// spelling the evaluator's own path resolution produces.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
