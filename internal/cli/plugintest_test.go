package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeModTree(t *testing.T, root string, files map[string]string) {
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

func pluginTestEnv(t *testing.T, cwd string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	home := t.TempDir()
	return &Env{
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		Stdout: &out,
		Stderr: &errb,
		Cwd:    func() (string, error) { return cwd, nil },
	}, &out, &errb
}

func TestPluginTestHelp(t *testing.T) {
	env, out, _ := pluginTestEnv(t, t.TempDir())
	if code := runPluginTest(env, []string{"--help"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.HasPrefix(out.String(), "Usage: gocode plugin test [dir]\n\nRun a mod's tests\n") {
		t.Fatalf("usage = %q", out.String())
	}
}

func TestPluginTestRefusesNonMod(t *testing.T) {
	noSpawn(t, nil)
	dir := t.TempDir()
	env, _, errb := pluginTestEnv(t, dir)
	if code := runPluginTest(env, []string{"missing"}); code != 1 || !strings.Contains(errb.String(), "missing: no such plugin folder") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	errb.Reset()
	if code := runPluginTest(env, nil); code != 1 || !strings.Contains(errb.String(), `there is no hooks/hooks.json naming one in "modules"`) {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	writeModTree(t, dir, map[string]string{"hooks/hooks.json": `{"hooks":{}}`})
	errb.Reset()
	if code := runPluginTest(env, nil); code != 1 || !strings.Contains(errb.String(), `hooks/hooks.json names none in "modules"`) {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	writeModTree(t, dir, map[string]string{"hooks/hooks.json": `{"modules":["./register.js"]}`})
	errb.Reset()
	if code := runPluginTest(env, nil); code != 1 || !strings.Contains(errb.String(), "no *.test.ts or *.test.tsx under "+dir) {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestFindPluginTestFiles(t *testing.T) {
	root := t.TempDir()
	writeModTree(t, root, map[string]string{
		"a.test.ts":                 "",
		"sub/b.test.tsx":            "",
		"sub/c.test.js":             "",
		"node_modules/x/d.test.ts":  "",
		".hidden/e.test.ts":         "",
		"sub/deeper/f.test.ts":      "",
		"sub/deeper/not-a-test.ts":  "",
		"sub/deeper/g.spec.test.ts": "",
	})
	got, err := findPluginTestFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, f := range got {
		r, _ := filepath.Rel(root, f)
		rel = append(rel, filepath.ToSlash(r))
	}
	want := "a.test.ts sub/b.test.tsx sub/deeper/f.test.ts sub/deeper/g.spec.test.ts"
	if strings.Join(rel, " ") != want {
		t.Fatalf("got %v, want %s", rel, want)
	}
}

func TestPluginTestChildRunsTheFile(t *testing.T) {
	// A folder with no hooks module: the file did not load, and says why.
	dir := t.TempDir()
	file := filepath.Join(dir, "a.test.ts")
	env, out, errb := pluginTestEnv(t, dir)
	if code := runPluginTest(env, []string{"--file", file, dir}); code != 0 {
		t.Fatalf("code %d, err %q", code, errb.String())
	}
	r := parsePluginTestReport(file, strings.TrimSpace(out.String()), 0)
	if r.Error == nil || !strings.Contains(*r.Error, "no hooks module to load") {
		t.Fatalf("report = %+v", r)
	}

	// A mod with its tests: they run on the engine, one passing, one failing.
	mod := t.TempDir()
	writeFile(t, filepath.Join(mod, "hooks", "hooks.json"), `{"modules": ["./guard.ts"]}`)
	writeFile(t, filepath.Join(mod, "hooks", "guard.ts"), `
export function register(on: (event: string, hook: Function) => unknown) {
  on('tool.call', async ($: unknown, e: { command?: string }, next: (e: unknown) => Promise<unknown>) =>
    e.command?.startsWith('rm ') ? { deny: 'no rm here' } : next(e))
}`)
	tf := filepath.Join(mod, "tests", "guard.test.ts")
	writeFile(t, tf, `
import { test, expect } from 'gocode/testing'
test('refuses rm', async ($: any) => {
  expect(await $.tool.call({ tool: 'Bash', tool_use_id: 't', command: 'rm -rf /' })).toEqual({ deny: 'no rm here' })
})
test('passes ls on', async ($: any, on: any) => {
  on('tool.call', () => ({ result: 'listed' }))
  expect((await $.tool.call({ tool: 'Bash', tool_use_id: 't', command: 'ls' })).result).toBe('nope')
})`)
	out.Reset()
	if code := runPluginTest(env, []string{"--file", tf, mod}); code != 0 {
		t.Fatalf("code %d, err %q", code, errb.String())
	}
	r = parsePluginTestReport(tf, strings.TrimSpace(out.String()), 0)
	if r.Error != nil || len(r.Results) != 2 {
		t.Fatalf("report = %+v", r)
	}
	if r.Results[0].Failure != nil {
		t.Fatalf("first test failed: %s", *r.Results[0].Failure)
	}
	if r.Results[1].Failure == nil || !strings.Contains(*r.Results[1].Failure, `Expected: "nope"`) {
		t.Fatalf("second test = %+v", r.Results[1])
	}

	// A file outside the dir is refused.
	out.Reset()
	if code := runPluginTest(env, []string{"--file", "/elsewhere/x.test.ts", dir}); code != 1 || !strings.Contains(errb.String(), "is not in") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func writeFile(t *testing.T, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParsePluginTestReport(t *testing.T) {
	fail := "boom"
	good, _ := json.Marshal(pluginTestFileReport{File: "/m/a.test.ts", Results: []pluginTestResult{{Title: "t1", Ms: 1.5}, {Title: "t2", Failure: &fail}}})
	stdout := "noise\n" + pluginTestReportMark + string(good) + "\n"
	r := parsePluginTestReport("/m/a.test.ts", stdout, 0)
	if r.Error != nil || len(r.Results) != 2 {
		t.Fatalf("r = %+v", r)
	}
	r = parsePluginTestReport("/m/a.test.ts", stdout, 3)
	if len(r.Results) != 3 || !strings.Contains(*r.Results[2].Failure, "exited with code 3 after reporting") {
		t.Fatalf("r = %+v", r)
	}
	r = parsePluginTestReport("/m/other.test.ts", stdout, 0)
	if r.Error == nil || *r.Error != "its run ended without a report (exit code 0)" {
		t.Fatalf("foreign report accepted: %+v", r)
	}
	r = parsePluginTestReport("/m/a.test.ts", "nothing", 7)
	if r.Error == nil || *r.Error != "its run ended without a report (exit code 7)" {
		t.Fatalf("r = %+v", r)
	}
}

func TestRunPluginTestFilesSummaryAndOrder(t *testing.T) {
	root := "/mod"
	files := []string{"/mod/a.test.ts", "/mod/b.test.ts", "/mod/c.test.ts"}
	fail := "expected 1\n    at check (/mod/a.test.ts:3:5)\n    at run (/opt/kit/runner.js:9:1)"
	run := func(_ string, f string) pluginTestFileReport {
		switch f {
		case "/mod/a.test.ts":
			return pluginTestFileReport{File: f, Results: []pluginTestResult{{Title: "ok", Ms: 1}, {Title: "bad", Ms: 2, Failure: &fail}}}
		case "/mod/b.test.ts":
			return fileLoadFailure(f, "syntax error")
		default:
			return pluginTestFileReport{File: f, Results: []pluginTestResult{{Title: "fine\x07", Ms: 0.25}}}
		}
	}
	var out bytes.Buffer
	code := runPluginTestFiles(&out, root, files, run)
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	s := out.String()
	for _, want := range []string{
		"\na.test.ts:\n(pass) ok [1.00ms]\n(fail) bad [2.00ms]\n  expected 1\n      at check (a.test.ts:3:5)\n",
		"\nb.test.ts:\n(fail) the file did not load\n  syntax error\n",
		"(pass) fine\\u0007 [0.25ms]",
		" 2 pass\n 2 fail\nRan 4 tests across 3 files.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "/opt/kit/runner.js") {
		t.Error("frames outside the mod must be trimmed")
	}
	if strings.Index(s, "a.test.ts:") > strings.Index(s, "b.test.ts:") || strings.Index(s, "b.test.ts:") > strings.Index(s, "c.test.ts:") {
		t.Error("reports must keep file order")
	}
	out.Reset()
	if code := runPluginTestFiles(&out, root, files[2:], run); code != 0 || !strings.Contains(out.String(), "Ran 1 test across 1 file.") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

// noSpawn makes any child spawn a test failure (a test binary must never
// re-execute itself) and restores the real spawner afterwards.
func noSpawn(t *testing.T, fake func(env *Env, root, file string) pluginTestFileReport) {
	t.Helper()
	prev := pluginTestSpawn
	if fake == nil {
		fake = func(*Env, string, string) pluginTestFileReport {
			t.Error("unexpected child spawn")
			return pluginTestFileReport{}
		}
	}
	pluginTestSpawn = fake
	t.Cleanup(func() { pluginTestSpawn = prev })
}

func TestPluginTestPolicyRefusal(t *testing.T) {
	noSpawn(t, nil)
	dir := t.TempDir()
	cfg := t.TempDir()
	var out, errb bytes.Buffer
	home := t.TempDir()
	env := &Env{
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
		Cwd: func() (string, error) { return dir, nil },
	}
	writeModTree(t, cfg, map[string]string{"settings.json": `{"disableAllHooks": true}`})
	writeModTree(t, dir, map[string]string{"hooks/hooks.json": `{"modules":["./r.js"]}`, "a.test.ts": ""})
	if msg := pluginTestPolicyRefusal(env); msg == "" {
		t.Skip(fmt.Sprintf("user settings not read from %s in this layout", cfg))
	}
	if code := runPluginTest(env, nil); code != 1 || !strings.Contains(errb.String(), "hooks modules are turned off here") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestPluginTestRunsDiscoveredFiles(t *testing.T) {
	dir := t.TempDir()
	writeModTree(t, dir, map[string]string{"hooks/hooks.json": `{"modules":["./r.js"]}`, "t/a.test.ts": "", "b.test.tsx": ""})
	// The runner calls this from several goroutines at once (one per file).
	var mu sync.Mutex
	var seen []string
	noSpawn(t, func(_ *Env, root, file string) pluginTestFileReport {
		mu.Lock()
		seen = append(seen, file)
		mu.Unlock()
		return pluginTestFileReport{File: file, Results: []pluginTestResult{{Title: "ok"}}}
	})
	env, out, errb := pluginTestEnv(t, dir)
	if code := runPluginTest(env, nil); code != 0 {
		t.Fatalf("code=%d err=%q out=%q", code, errb.String(), out.String())
	}
	if len(seen) != 2 || !strings.Contains(out.String(), "Ran 2 tests across 2 files.") {
		t.Fatalf("seen=%v out=%q", seen, out.String())
	}
}
