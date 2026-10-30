package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/skills"
)

// importFixture lays out a temp HOME with a fake Codex config tree, a project
// dir, and pre-existing gocode settings that must survive the import.
type importFixture struct {
	home, proj, configDir string
}

func newImportFixture(t *testing.T) importFixture {
	t.Helper()
	base := t.TempDir()
	f := importFixture{
		home: filepath.Join(base, "home"),
		proj: filepath.Join(base, "proj"),
	}
	f.configDir = filepath.Join(f.home, ".config", "gocode") // settingsload default (no XDG / GOCODE_CONFIG_DIR)
	mk := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	codex := filepath.Join(f.home, ".codex")
	mk(filepath.Join(codex, "config.toml"), `approval_policy = "on-request"
sandbox_mode = "workspace-write"

[mcp_servers.docs]
command = "npx"
args = ["-y", "@acme/docs-mcp"]

[mcp_servers.search]
command = "search-mcp"
args = [
  "--port", "0",
]
env = { SEARCH_TOKEN = "${SEARCH_TOKEN}" }

[agents.reviewer]
description = "Reviews diffs"
instructions = "Review carefully."
`)
	mk(filepath.Join(codex, "AGENTS.md"), "Always run the tests.\n")
	mk(filepath.Join(codex, "prompts", "review.md"), "Review $ARGUMENTS\n")
	mk(filepath.Join(f.configDir, "settings.json"), `{"theme":"dark","mcpServers":{"docs":{"command":"mine"}}}`)
	if err := os.MkdirAll(f.proj, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f importFixture) env(t *testing.T, stdin string, tty bool, extra map[string]string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	prev := importAccountHome
	importAccountHome = func() (string, bool) { return f.home, true }
	t.Cleanup(func() { importAccountHome = prev })
	var out, errb bytes.Buffer
	vars := map[string]string{"HOME": f.home}
	for k, v := range extra {
		vars[k] = v
	}
	return &Env{
		Getenv:   func(k string) string { return vars[k] },
		Setenv:   func(string, string) {},
		Stdout:   &out,
		Stderr:   &errb,
		Stdin:    strings.NewReader(stdin),
		StdinTTY: tty,
		Cwd:      func() (string, error) { return f.proj, nil },
	}, &out, &errb
}

// gocodeFiles lists every file the import could have written.
func (f importFixture) gocodeFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range []string{filepath.Join(f.home, ".gocode"), f.configDir, f.proj} {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestImportNonTTYListsOnlyAndWritesNothing(t *testing.T) {
	f := newImportFixture(t)
	before := f.gocodeFiles(t)
	env, out, errb := f.env(t, "", false, nil)
	if code := Run(env, []string{"gocode", "import"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"Found ", "from OpenAI Codex (scan digest: ", "User-level config", `[mcp] MCP server "search"`,
		"Nothing was imported: stdin is not a terminal", "gocode import --yes=", "Also "} {
		if !strings.Contains(s, want) {
			t.Errorf("preview missing %q:\n%s", want, s)
		}
	}
	if got := f.gocodeFiles(t); len(got) != len(before) {
		t.Errorf("non-TTY without --yes wrote files: %v", got)
	}
}

func TestImportYesDryRunWritesNothing(t *testing.T) {
	f := newImportFixture(t)
	before := f.gocodeFiles(t)
	env, out, _ := f.env(t, "", false, nil)
	if code := Run(env, []string{"gocode", "import", "--yes", "--dry-run"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "Dry run — would import") || !strings.Contains(s, "would add MCP server search (user)") {
		t.Errorf("dry-run output:\n%s", s)
	}
	if got := f.gocodeFiles(t); len(got) != len(before) {
		t.Errorf("dry run wrote files: %v", got)
	}
}

func TestImportYesAppliesSafeItemsIdempotently(t *testing.T) {
	f := newImportFixture(t)
	env, out, errb := f.env(t, "", false, nil)
	if code := Run(env, []string{"gocode", "import", "codex", "--yes"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	// MCP commands are shown before anything is written.
	review := strings.Index(s, "MCP servers to be added")
	head := strings.Index(s, "Imported ")
	if review == -1 || head == -1 || review > head || !strings.Contains(s, "stdio: search-mcp --port 0") || !strings.Contains(s, "env: SEARCH_TOKEN") {
		t.Errorf("MCP review missing or after apply:\n%s", s)
	}
	if !strings.Contains(s, "warning-flagged item") || !strings.Contains(s, "held back — re-run without --yes to review.") {
		t.Errorf("held-back footer missing:\n%s", s)
	}

	// Memory appended once to the user gocode.md the session loads.
	mem, err := os.ReadFile(filepath.Join(f.configDir, "gocode.md"))
	if err != nil || strings.Count(string(mem), "<!-- imported-from: codex:user:instructions -->") != 1 {
		t.Fatalf("gocode.md = %q %v", mem, err)
	}
	// MCP: existing "docs" untouched, "search" added, other keys kept.
	st := readSettings(t, filepath.Join(f.configDir, "settings.json"))
	servers := st["mcpServers"].(map[string]any)
	if servers["docs"].(map[string]any)["command"] != "mine" || servers["search"] == nil || st["theme"] != "dark" {
		t.Errorf("settings = %v", st)
	}
	if _, ok := st["permissions"]; ok {
		t.Errorf("warning-flagged permission mode applied by --yes: %v", st["permissions"])
	}
	// Command + subagent + fallback skill landed where the loaders look.
	for _, p := range []string{
		filepath.Join(f.configDir, "commands", "review.md"),
		filepath.Join(f.configDir, "agents", "reviewer.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	reg := skills.Load(fsfacade.OSFS{}, skills.LoadConfig{UserDir: f.configDir, Home: f.home})
	if _, ok := reg.Lookup("import-to-gocode"); !ok {
		t.Errorf("fallback skill not loadable: %v", reg.List())
	}

	// Re-run: nothing is duplicated.
	env2, out2, _ := f.env(t, "", false, nil)
	if code := Run(env2, []string{"gocode", "import", "codex", "--yes"}); code != 0 {
		t.Fatalf("re-run exit %d", code)
	}
	if !strings.Contains(out2.String(), "already imported (marker present; not re-synced)") ||
		!strings.Contains(out2.String(), "MCP server already exists in user config") {
		t.Errorf("re-run output:\n%s", out2.String())
	}
	mem2, _ := os.ReadFile(filepath.Join(f.configDir, "gocode.md"))
	if string(mem2) != string(mem) {
		t.Errorf("gocode.md changed on re-run")
	}
}

var reDigest = regexp.MustCompile(`--yes=([0-9a-f]{32})`)

func TestImportYesDigestBinding(t *testing.T) {
	f := newImportFixture(t)
	env, out, _ := f.env(t, "", false, nil)
	Run(env, []string{"gocode", "import"})
	m := reDigest.FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no digest in preview:\n%s", out.String())
	}

	before := f.gocodeFiles(t)
	bad, _, errb := f.env(t, "", false, nil)
	if code := Run(bad, []string{"gocode", "import", "--yes=" + strings.Repeat("0", 32)}); code != 1 || !strings.Contains(errb.String(), "Refusing: the config on disk no longer matches") {
		t.Errorf("mismatched digest: code %d %s", code, errb.String())
	}
	short, _, errb2 := f.env(t, "", false, nil)
	if code := Run(short, []string{"gocode", "import", "--yes=abc"}); code != 1 || !strings.Contains(errb2.String(), "needs the scan digest") {
		t.Errorf("malformed digest: code %d %s", code, errb2.String())
	}
	if got := f.gocodeFiles(t); len(got) != len(before) {
		t.Errorf("refused confirm wrote files")
	}

	good, out3, errb3 := f.env(t, "", false, nil)
	if code := Run(good, []string{"gocode", "import", "--yes=" + m[1]}); code != 0 {
		t.Fatalf("good digest exit %d: %s", code, errb3.String())
	}
	if !strings.Contains(out3.String(), "Imported ") || !strings.Contains(out3.String(), "run `gocode import` from a terminal to review.") {
		t.Errorf("digest apply output:\n%s", out3.String())
	}
}

func TestImportInteractivePickerAppliesSelection(t *testing.T) {
	f := newImportFixture(t)
	// "all" → Enter (continue) → Y (helper skill) → y (confirm).
	env, out, errb := f.env(t, "all\n\ny\ny\n", true, nil)
	if code := Run(env, []string{"gocode", "import"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"Import to gocode", "Select what to import:", "[OpenAI Codex]", "(global)", "could not be mapped automatically", "Imported "} {
		if !strings.Contains(s, want) {
			t.Errorf("picker output missing %q:\n%s", want, s)
		}
	}
	st := readSettings(t, filepath.Join(f.configDir, "settings.json"))
	perms, _ := st["permissions"].(map[string]any)
	if perms["defaultMode"] != "default" || st["theme"] != "dark" {
		t.Errorf("settings merge = %v", st)
	}
}

func TestImportInteractiveCancelWritesNothing(t *testing.T) {
	f := newImportFixture(t)
	before := f.gocodeFiles(t)
	env, out, _ := f.env(t, "q\n", true, nil)
	if code := Run(env, []string{"gocode", "import"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "No items imported.") {
		t.Errorf("cancel output:\n%s", out.String())
	}
	if got := f.gocodeFiles(t); len(got) != len(before) {
		t.Errorf("cancel wrote files")
	}
}

func TestImportRedirectedConfigDirInsideProject(t *testing.T) {
	f := newImportFixture(t)
	cfg := filepath.Join(f.proj, ".cfg")
	env, out, _ := f.env(t, "", false, map[string]string{"GOCODE_CONFIG_DIR": cfg})
	if code := Run(env, []string{"gocode", "import", "--yes"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "user-scope config: The user-scope read or write root has been redirected") {
		t.Errorf("redirect note missing:\n%s", out.String())
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Errorf("wrote into the redirected config dir")
	}
}

func TestImportUnknownSourceAndBadFlag(t *testing.T) {
	f := newImportFixture(t)
	env, _, errb := f.env(t, "", false, nil)
	if code := Run(env, []string{"gocode", "import", "vim"}); code != 1 || !strings.Contains(errb.String(), `Unknown source "vim". Known: codex, gemini, cursor`) {
		t.Errorf("unknown source: %d %s", code, errb.String())
	}
	env2, _, errb2 := f.env(t, "", false, nil)
	if code := Run(env2, []string{"gocode", "import", "--force"}); code != 1 || !strings.Contains(errb2.String(), "unknown option '--force'") {
		t.Errorf("bad flag: %d %s", code, errb2.String())
	}
}

func TestImportHelpListsFlags(t *testing.T) {
	env, out, _ := newTestEnv()
	if code := Run(env, []string{"gocode", "import", "--help"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"[source]", "--dry-run", "--yes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing %q:\n%s", want, out.String())
		}
	}
}
