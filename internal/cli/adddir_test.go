package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

func TestResolveAddDirs(t *testing.T) {
	cwd, home, extra := t.TempDir(), t.TempDir(), t.TempDir()
	file := filepath.Join(extra, "f.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "proj2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	dirs, warns := resolveAddDirs(cwd, home, []string{extra, extra + "/", "~/proj2", "sub", filepath.Join(extra, "missing"), file, ""})
	want := []string{extra, filepath.Join(home, "proj2")}
	if strings.Join(dirs, "|") != strings.Join(want, "|") {
		t.Errorf("dirs = %v, want %v", dirs, want)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "is not a directory") {
		t.Errorf("warnings = %v", warns)
	}
}

func TestAddDirFlagReachesSession(t *testing.T) {
	root := parseRootConfig([]string{"gocode", "--add-dir", "/a", "--add-dir", "/b", "-p", "hi"})
	if strings.Join(root.Config.AddDir, ",") != "/a,/b" {
		t.Fatalf("parsed --add-dir = %v", root.Config.AddDir)
	}
	env, _, _, _ := sessionEnv(t, "")
	s := newPrintSession(env, &DispatchDefaults{AddDirs: root.Config.AddDir})
	if strings.Join(s.AddDirs, ",") != "/a,/b" {
		t.Fatalf("print session add dirs = %v", s.AddDirs)
	}
}

// --add-dir directories and permissions.additionalDirectories are working
// directories for the permission checks and writable roots of the sandbox.
func TestAddDirIsWorkingDirectoryAndSandboxWritable(t *testing.T) {
	extra, fromSettings := t.TempDir(), t.TempDir()
	env, _, _, cwd := sessionEnv(t, `{"permissions":{"additionalDirectories":[`+jsonString(fromSettings)+`]},"sandbox":{"enabled":true}}`)

	files := newSessionFileChecks(env, extra)
	for _, d := range []string{cwd, extra, fromSettings} {
		if !files.WithinWorkingDirs([]string{filepath.Join(d, "x.txt")}) {
			t.Errorf("%s is not a working directory: %v", d, files.WorkingDirs())
		}
	}
	if files.WithinWorkingDirs([]string{filepath.Join(t.TempDir(), "x.txt")}) {
		t.Error("an unrelated directory is a working directory")
	}

	sb := newSessionSandbox(env, nil, false, extra)
	defer sb.Close()
	rc := sb.RuntimeConfig("")
	for _, d := range []string{cwd, extra, fromSettings} {
		found := false
		for _, w := range rc.AllowWrite {
			if w == d {
				found = true
			}
		}
		if !found {
			t.Errorf("%s not writable in the sandbox: %v", d, rc.AllowWrite)
		}
	}
	// The add-dir is protected like the project.
	for _, p := range []string{filepath.Join(extra, ".mcp.json"), filepath.Join(extra, ".git", "hooks"), filepath.Join(extra, ".bashrc")} {
		found := false
		for _, d := range rc.DenyWrite {
			if d == p {
				found = true
			}
		}
		if !found {
			t.Errorf("%s not write-protected", p)
		}
	}

	// acceptEdits writes into the add-dir without a prompt (no prompter here:
	// an ask would be a denial).
	d := newSessionDispatcher(toolreg.ModeAcceptEdits, env, nil, toolrun.WithSessionFiles(newSessionFileChecks(env, extra)))
	in, _ := json.Marshal(map[string]string{"file_path": filepath.Join(extra, "new.txt"), "content": "hi"})
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "w", Name: string(protocol.ToolWrite), Input: in})
	if err != nil || res.IsError {
		t.Fatalf("write into --add-dir: %v %q", err, res.Content)
	}
	outside := t.TempDir()
	in, _ = json.Marshal(map[string]string{"file_path": filepath.Join(outside, "new.txt"), "content": "hi"})
	if res, _ := d.Dispatch(context.Background(), agent.ToolUse{ID: "w2", Name: string(protocol.ToolWrite), Input: in}); !res.IsError {
		t.Fatalf("write outside the working directories allowed: %q", res.Content)
	}
}
