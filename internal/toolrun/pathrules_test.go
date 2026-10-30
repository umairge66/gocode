package toolrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

func searchFixture(t *testing.T) (proj, outside string) {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	proj = filepath.Join(base, "proj")
	outside = filepath.Join(base, "outside")
	files := map[string]string{
		filepath.Join(proj, "src", "a.txt"):       "needle public\n",
		filepath.Join(proj, "secrets", "k.txt"):   "needle secret\n",
		filepath.Join(proj, "deep", "x", "c.pem"): "needle pem\n",
		filepath.Join(outside, "o.txt"):           "needle outside\n",
	}
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return proj, outside
}

func sessionFilesFor(proj, home string, block bool) *pathguard.SessionFiles {
	return &pathguard.SessionFiles{OriginalCwd: proj, Cwd: proj, HomeDir: home, BlockReadsOutsideWorkingDirectories: block}
}

// A Read deny rule keeps Glob and Grep from revealing the denied files
// .
func TestReadDenyRulesFilterGlobAndGrep(t *testing.T) {
	proj, _ := searchFixture(t)
	rules := toolreg.RuleSet{
		{Behavior: toolreg.RuleDeny, ToolName: "Read", Content: "./secrets/**", Source: "projectSettings"},
		{Behavior: toolreg.RuleDeny, ToolName: "Read", Content: "*.pem", Source: "projectSettings"},
	}
	d := New(toolreg.ModeDefault, rules, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	glob := dispatch(t, d, string(protocol.ToolGlob), map[string]any{"pattern": "**/*", "path": proj})
	if glob.IsError || !strings.Contains(glob.Content, "a.txt") {
		t.Fatalf("glob = %+v", glob)
	}
	if strings.Contains(glob.Content, "k.txt") || strings.Contains(glob.Content, "c.pem") {
		t.Errorf("Glob revealed denied files:\n%s", glob.Content)
	}
	grep := dispatch(t, d, string(protocol.ToolGrep), map[string]any{"pattern": "needle", "path": proj, "output_mode": "content"})
	if grep.IsError || !strings.Contains(grep.Content, "public") {
		t.Fatalf("grep = %+v", grep)
	}
	if strings.Contains(grep.Content, "secret") || strings.Contains(grep.Content, "pem") {
		t.Errorf("Grep revealed denied content:\n%s", grep.Content)
	}
	// Searching the denied directory itself is refused outright.
	if res := dispatch(t, d, string(protocol.ToolGrep), map[string]any{"pattern": "needle", "path": filepath.Join(proj, "secrets")}); !res.IsError {
		t.Errorf("Grep of a denied dir allowed: %+v", res)
	}
	// An absolute Glob pattern is judged on the directory it searches.
	if res := dispatch(t, d, string(protocol.ToolGlob), map[string]any{"pattern": filepath.Join(proj, "secrets") + "/*.txt", "path": proj}); !res.IsError {
		t.Errorf("absolute Glob pattern into a denied dir allowed: %+v", res)
	}
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": filepath.Join(proj, "secrets", "k.txt")}); !res.IsError {
		t.Errorf("Read of denied file allowed: %+v", res)
	}
}

// permissions.blockReadsOutsideWorkingDirectories refuses Read / Glob / Grep
// outside the working directories in every mode, bypass included, also when a
// link inside leads outside.
func TestBlockReadsOutsideWorkingDirectories(t *testing.T) {
	proj, outside := searchFixture(t)
	link := filepath.Join(proj, "src", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, mode := range []toolreg.Mode{toolreg.ModeDefault, toolreg.ModeAuto, toolreg.ModeBypassPermissions} {
		d := New(mode, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), true)))
		for _, c := range []struct {
			tool  protocol.Tool
			input map[string]any
		}{
			{protocol.ToolRead, map[string]any{"file_path": filepath.Join(outside, "o.txt")}},
			{protocol.ToolRead, map[string]any{"file_path": filepath.Join(link, "o.txt")}},
			{protocol.ToolGlob, map[string]any{"pattern": "*", "path": outside}},
			{protocol.ToolGlob, map[string]any{"pattern": outside + "/*.txt", "path": proj}},
			{protocol.ToolGrep, map[string]any{"pattern": "needle", "path": outside}},
			{protocol.ToolGrep, map[string]any{"pattern": "needle", "path": link}},
		} {
			res := dispatch(t, d, string(c.tool), c.input)
			if !res.IsError || !strings.Contains(res.Content, "blockReadsOutsideWorkingDirectories") {
				t.Errorf("%s %s %v: %+v", mode, c.tool, c.input, res)
			}
		}
		// Inside reads still work; a search inside never lists files reached
		// through the escaping link.
		if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": filepath.Join(proj, "src", "a.txt")}); res.IsError {
			t.Errorf("%s: inside Read refused: %+v", mode, res)
		}
		res := dispatch(t, d, string(protocol.ToolGrep), map[string]any{"pattern": "needle", "path": proj, "output_mode": "content"})
		if res.IsError || strings.Contains(res.Content, "outside") {
			t.Errorf("%s: inside Grep: %+v", mode, res)
		}
	}
	// Off: an outside read is not refused by this check — it asks as an
	// outside read (here, with no prompter, a denial naming the read), and
	// auto mode allows it (safe allowlist).
	d := New(toolreg.ModeDefault, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": filepath.Join(outside, "o.txt")}); !res.IsError || strings.Contains(res.Content, "blockReadsOutsideWorkingDirectories") || !strings.Contains(res.Content, "read from") {
		t.Errorf("setting off: outside Read should ask, got %+v", res)
	}
	d = New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": filepath.Join(outside, "o.txt")}); res.IsError {
		t.Errorf("setting off, auto mode: outside Read refused: %+v", res)
	}
}

// Plan mode holds edits and non-read-only Bash (no prompter: denied with the
// plan message), while reads still run.
func TestPlanModeBlocksEditsAndBash(t *testing.T) {
	proj, _ := searchFixture(t)
	target := filepath.Join(proj, "src", "a.txt")
	d := New(toolreg.ModePlan, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": target}); res.IsError {
		t.Fatalf("plan Read refused: %+v", res)
	}
	for name, in := range map[protocol.Tool]map[string]any{
		protocol.ToolEdit:  {"file_path": target, "old_string": "needle", "new_string": "x"},
		protocol.ToolWrite: {"file_path": target, "content": "x"},
		protocol.ToolBash:  {"command": "rm -f " + target},
	} {
		if res := dispatch(t, d, string(name), in); !res.IsError {
			t.Errorf("plan mode ran %s: %+v", name, res)
		}
	}
	// Read-only Bash runs in plan mode (the per-command
	// isReadOnly lets plan mode explore); anything that could write is held.
	for _, cmd := range []string{"ls " + proj, "git status", "grep -r needle " + proj, "cat " + target} {
		res := dispatch(t, d, string(protocol.ToolBash), map[string]any{"command": cmd})
		if strings.Contains(res.Content, "plan mode") || strings.Contains(res.Content, "permission") {
			t.Errorf("plan mode held read-only %q: %+v", cmd, res)
		}
		if cmd != "git status" && res.IsError { // git status may fail outside a repository
			t.Errorf("read-only %q failed in plan mode: %+v", cmd, res)
		}
	}
	for _, cmd := range []string{
		"sed -i s/needle/x/ " + target,
		"find " + proj + " -delete",
		"echo x > " + target,
		"git commit -m x",
		"ls; rm -f " + target,
		"ls $(rm -f " + target + ")",
		"cat `rm -f " + target + "`",
	} {
		res := dispatch(t, d, string(protocol.ToolBash), map[string]any{"command": cmd})
		if !res.IsError || !strings.Contains(res.Content, "plan mode") {
			t.Errorf("plan mode did not hold %q: %+v", cmd, res)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "needle public\n" {
		t.Errorf("plan mode modified the file: %q", b)
	}
}
