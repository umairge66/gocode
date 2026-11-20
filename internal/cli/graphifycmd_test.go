package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/graphify"
)

func graphifyEnv(t *testing.T) (*Env, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cwd := t.TempDir()
	files := map[string]string{
		"go.mod":       "module example.com/app\n",
		"main.go":      "package main\n\nimport \"example.com/app/auth\"\n\nfunc main() { auth.Login(\"u\") }\n",
		"auth/auth.go": "package auth\n\nfunc Login(user string) bool { return check(user) }\n\nfunc check(user string) bool { return user != \"\" }\n",
	}
	for name, content := range files {
		p := filepath.Join(cwd, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env, out, errb := newTestEnv()
	env.Cwd = func() (string, error) { return cwd, nil }
	return env, cwd, out, errb
}

// `gocode graphify` builds graphify-out/ in the current directory, and the
// query verbs answer from it with the graphify exit codes.
func TestGraphifyCommand(t *testing.T) {
	env, cwd, out, errb := graphifyEnv(t)

	// Queries before a build say how to build.
	if code := Run(env, []string{"gocode", "graphify", "explain", "Login"}); code != 1 || !strings.Contains(errb.String(), "No graph yet") {
		t.Fatalf("explain without a graph: code %d, stderr %q", code, errb.String())
	}
	errb.Reset()

	if code := Run(env, []string{"gocode", "graphify"}); code != 0 {
		t.Fatalf("build: code %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{"Read 2 code files and 0 documents (2 parsed, 0 unchanged from cache)", "graphify-out/graph.json", "graphify-out/GRAPH_REPORT.md", "graphify-out/graph.html"} {
		if !strings.Contains(filepath.ToSlash(out.String()), want) {
			t.Errorf("build output lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, "graphify-out", "graph.json")); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if code := Run(env, []string{"gocode", "graphify", "path", "main", "check"}); code != 0 || !strings.Contains(out.String(), "2 hops from main() to check()") {
		t.Errorf("path: code %d\n%s", code, out.String())
	}
	out.Reset()
	if code := Run(env, []string{"gocode", "graphify", "query", "who calls login", "--depth=1", "--budget", "500"}); code != 0 || !strings.Contains(out.String(), "main() --calls--> Login()") {
		t.Errorf("query: code %d\n%s", code, out.String())
	}
	out.Reset()
	if code := Run(env, []string{"gocode", "graphify", "explain", "nope"}); code != graphify.ExitNoMatch || !strings.Contains(out.String(), "No node matches") {
		t.Errorf("explain unknown: code %d\n%s", code, out.String())
	}

	// The graph is found from a subdirectory, and a second build is cached.
	env.Cwd = func() (string, error) { return filepath.Join(cwd, "auth"), nil }
	out.Reset()
	if code := Run(env, []string{"gocode", "graphify", "explain", "check"}); code != 0 || !strings.Contains(out.String(), "← calls (1)") {
		t.Errorf("explain from a subdirectory: code %d\n%s", code, out.String())
	}
	env.Cwd = func() (string, error) { return cwd, nil }
	out.Reset()
	if code := Run(env, []string{"gocode", "graphify", ".", "--no-viz"}); code != 0 || !strings.Contains(out.String(), "0 parsed, 2 unchanged from cache") || !strings.Contains(out.String(), "No change since the last run") {
		t.Errorf("second build: code %d\n%s", code, out.String())
	}

	// Usage errors.
	errb.Reset()
	if code := Run(env, []string{"gocode", "graphify", "--bogus"}); code != 2 || !strings.Contains(errb.String(), "unknown option --bogus") {
		t.Errorf("bad flag: code %d, stderr %q", code, errb.String())
	}
	if code := Run(env, []string{"gocode", "graphify", "path", "main"}); code != 2 {
		t.Errorf("path with one name: code %d", code)
	}
}

// /graphify runs the same commands and returns their text.
func TestGraphifySlash(t *testing.T) {
	env, _, _, _ := graphifyEnv(t)
	if got := graphifySlash(env, ""); !strings.Contains(got, "Graph: 7 nodes, 8 edges") {
		t.Errorf("/graphify:\n%s", got)
	}
	if got := graphifySlash(env, `query "who calls login"`); !strings.Contains(got, "main() --calls--> Login()") {
		t.Errorf("/graphify query:\n%s", got)
	}
	if got := graphifySlash(env, "help"); !strings.Contains(got, "gocode graphify explain") {
		t.Errorf("/graphify help:\n%s", got)
	}
}

func TestSplitQuotedArgs(t *testing.T) {
	got := splitQuotedArgs(`query "what connects a  to b" --depth 3  'x y'z ""`)
	want := []string{"query", "what connects a  to b", "--depth", "3", "x yz", ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
