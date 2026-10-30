package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/plugins/plugineval"
	"github.com/umairge66/gocode/internal/toolreg"
)

func TestParseEvalArgs(t *testing.T) {
	o, pos := parseEvalArgs([]string{"./plug", "--tag", "a", "b", "--allow-tools", "Write", "Bash(npm test:*)", "--json", "--runs=2", "-j", "3", "--no-scaffold", "--eval-dir", "q/e"})
	if len(pos) != 1 || pos[0] != "./plug" {
		t.Fatalf("pos = %v", pos)
	}
	if strings.Join(o.tags, ",") != "a,b" || strings.Join(o.allowTools, ",") != "Write,Bash(npm test:*)" {
		t.Fatalf("variadics: %+v", o)
	}
	if !o.jsonSet || o.json != "-" || o.runs != "2" || o.concurrency != "3" || o.scaffold == nil || *o.scaffold || o.evalDir != "q/e" {
		t.Fatalf("o = %+v", o)
	}
	o, pos = parseEvalArgs([]string{"--json", "out.json", "--bogus"})
	if o.json != "out.json" || len(pos) != 0 || len(o.unknownOptions) != 1 {
		t.Fatalf("o=%+v pos=%v", o, pos)
	}
}

// refuseExecutor fails the test if a child would be spawned.
type refuseExecutor struct{ t *testing.T }

func (r refuseExecutor) Run(context.Context, plugineval.ChildSpec) plugineval.ChildOutcome {
	r.t.Error("unexpected eval child spawn")
	return plugineval.ChildOutcome{Exit: plugineval.ChildExit{Kind: "error", Message: "refused in test"}}
}

func evalTestEnv(t *testing.T, cwd string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	prev := pluginEvalExecutor
	pluginEvalExecutor = func() plugineval.Executor { return refuseExecutor{t} }
	t.Cleanup(func() { pluginEvalExecutor = prev })
	var out, errb bytes.Buffer
	home := t.TempDir()
	cfg := t.TempDir()
	return &Env{
		Getenv: func(k string) string {
			switch k {
			case "HOME":
				return home
			case "GOCODE_CONFIG_DIR":
				return cfg
			}
			return ""
		},
		Stdout: &out, Stderr: &errb,
		Cwd: func() (string, error) { return cwd, nil },
	}, &out, &errb
}

func TestPluginEvalValidation(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--runs", "0"}, "--runs must be a positive integer"},
		{[]string{"--threshold", "2"}, "--threshold must be between 0 and 1"},
		{[]string{"--mocks", "maybe"}, "--mocks must be record or off"},
		{[]string{"--json", "./plugin"}, "--json output path must end in .json (got './plugin')"},
		{[]string{"--no-publish", "--publish-report"}, "contradictory"},
		{[]string{"--max-cost-usd", "-1"}, "--max-cost-usd must be a non-negative number"},
		{[]string{"-j", "9"}, "--concurrency must be a whole number from 1 to 8"},
		{[]string{"--ablation", "sometimes"}, `--ablation must be "none" or "with-without"`},
		{[]string{"a", "b"}, "too many arguments"},
		{[]string{"./missing"}, "could not be found or read"},
	}
	for _, c := range cases {
		env, _, errb := evalTestEnv(t, dir)
		code := runPluginEval(env, &DispatchDefaults{}, append([]string{"plugin", "eval"}, c.args...))
		if code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: code=%d err=%q, want %q", c.args, code, errb.String(), c.want)
		}
	}
}

func TestPluginEvalTrustGateWithoutTTY(t *testing.T) {
	dir := t.TempDir()
	writeModTree(t, dir, map[string]string{".gocode-plugin/plugin.json": `{"name":"p"}`, "evals/a/case.yaml": "x: 1"})
	env, _, errb := evalTestEnv(t, dir)
	code := runPluginEval(env, &DispatchDefaults{}, []string{"plugin", "eval"})
	if code != 1 || !strings.Contains(errb.String(), "is not a trusted plugin directory") || !strings.Contains(errb.String(), "--trust-plugin") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestPluginEvalNoCasesAndAuthPreflight(t *testing.T) {
	dir := t.TempDir()
	writeModTree(t, dir, map[string]string{".gocode-plugin/plugin.json": `{"name":"p","experimental":{"evals":"quality/evals"}}`})
	env, _, errb := evalTestEnv(t, dir)
	code := runPluginEval(env, &DispatchDefaults{}, []string{"plugin", "eval", "--trust-plugin"})
	if code != 1 || !strings.Contains(errb.String(), "No eval cases found under "+dir) || !strings.Contains(errb.String(), "Cases are expected in a quality/evals/ directory") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	// With a case but no API key, the auth preflight stops the suite (exit 2)
	// before any child starts.
	writeModTree(t, dir, map[string]string{"quality/evals/a/case.yaml": "schema_version: \"1.0\"\nname: a\nexecution: {prompt: hi}\ngraders: [{type: regex, name: r, pattern: hi}]\n"})
	env, _, errb = evalTestEnv(t, dir)
	code = runPluginEval(env, &DispatchDefaults{}, []string{"plugin", "eval", "--trust-plugin", "--ablation", "none"})
	if code != 2 || !strings.Contains(errb.String(), "authentication check failed before running any case") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	// No aggregate result was written for an auth failure with no cases run…
	// …but the results directory may exist only when something was written.
	matches, _ := filepath.Glob(filepath.Join(dir, "quality", "evals", "results", "*", "report.html"))
	if len(matches) != 0 {
		t.Fatalf("no report for a run that never started: %v", matches)
	}
}

func TestPluginEvalInitBare(t *testing.T) {
	dir := t.TempDir()
	writeModTree(t, dir, map[string]string{".gocode-plugin/plugin.json": `{"name":"p"}`})
	env, out, errb := evalTestEnv(t, dir)
	if code := runPluginEval(env, nil, []string{"plugin", "eval", "init", "--bare", "smoke"}); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "Created "+filepath.Join("evals", "smoke", "prompt.md")) {
		t.Fatalf("out=%q", out.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "evals", "smoke", "graders", "criteria.md"))
	if err != nil || !strings.Contains(string(b), "type: llm") {
		t.Fatalf("grader template: %v %q", err, b)
	}
	// Not a plugin folder → refused unless --eval-dir.
	other := t.TempDir()
	env, _, errb = evalTestEnv(t, other)
	if code := runPluginEval(env, nil, []string{"plugin", "eval", "init", "x"}); code != 1 || !strings.Contains(errb.String(), "is not a plugin or skill folder") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	env, _, _ = evalTestEnv(t, other)
	if code := runPluginEval(env, nil, []string{"plugin", "eval", "init", "--eval-dir", "e", "x"}); code != 0 {
		t.Fatalf("--eval-dir scaffold: code=%d", code)
	}
}

func TestEvalChildStreamJSON(t *testing.T) {
	dir := t.TempDir()
	env, out, _ := evalTestEnv(t, dir)
	o := parseEvalChildOptions([]string{"-p", "--output-format", "stream-json", "--max-turns", "3", "--disallowed-tools=Bash,Monitor,Read(/x/**)", "--allowed-tools=Read(/y/**)"})
	if o.maxTurns != "3" || len(o.disallowed) != 3 || len(o.allowed) != 1 {
		t.Fatalf("opts = %+v", o)
	}
	childEnv := evalChildEnv(env, o)
	if childEnv.Getenv("GOCODE_CODE_MAX_TURNS") != "3" {
		t.Fatal("max turns not overlaid")
	}
	s := &printSession{Mode: toolreg.ModeDontAsk, Model: "m", newCaller: func(*printSession) (agent.ModelCaller, error) {
		return fakeCaller{reply: "Hello from the child"}, nil
	}}
	fail := func(msg string, _ int) int { t.Fatalf("fail: %s", msg); return 1 }
	code := runEvalChildSession(childEnv, s, o, "hi", streamWriter{w: out}, time.Now(), fail)
	if code != 0 {
		t.Fatalf("code %d, out=%s", code, out.String())
	}
	var recs []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad line %q: %v", l, err)
		}
		recs = append(recs, m)
	}
	if recs[0]["type"] != "system" || recs[len(recs)-1]["type"] != "result" {
		t.Fatalf("records = %v", recs)
	}
	for _, tool := range recs[0]["tools"].([]any) {
		if tool == "Bash" || tool == "Monitor" {
			t.Fatalf("disallowed tool %v still offered", tool)
		}
	}
	res := recs[len(recs)-1]
	if res["is_error"] != false || res["num_turns"].(float64) != 1 || res["result"] != "Hello from the child" {
		t.Fatalf("result = %v", res)
	}
	var sawAssistant bool
	for _, r := range recs {
		if r["type"] == "assistant" {
			sawAssistant = true
		}
	}
	if !sawAssistant {
		t.Fatal("no assistant record")
	}
	_ = context.Background
}

func TestIsEvalChildInvocation(t *testing.T) {
	env := &Env{Getenv: func(k string) string {
		if k == evalChildMarker {
			return "1"
		}
		return ""
	}}
	if !isEvalChildInvocation(env, []string{"-p", "--output-format", "stream-json"}) {
		t.Fatal("eval child not detected")
	}
	if isEvalChildInvocation(&Env{Getenv: func(string) string { return "" }}, []string{"-p", "--output-format", "stream-json"}) {
		t.Fatal("ordinary -p must not take the eval child path")
	}
}
