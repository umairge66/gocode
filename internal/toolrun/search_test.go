package toolrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/filetools"
)

func TestBashDescriptionSteersToDedicatedTools(t *testing.T) {
	d := toolDescriptions[protocol.ToolBash]
	for _, want := range []string{
		"Avoid using this tool to run `find`, `grep`, `cat`, `head`, `tail`, `sed`, `awk`, or `echo` commands",
		"File search: Use Glob (NOT find or ls)",
		"Content search: Use Grep (NOT grep or rg)",
		"Read files: Use Read (NOT cat/head/tail)",
		"Edit files: Use Edit (NOT sed/awk)",
		"Write files: Use Write (NOT echo >/cat <<EOF)",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("Bash description missing %q", want)
		}
	}
}

// TestBashDescriptionGrepViaBash: with `grep` running ugrep, grep leaves the
// commands to avoid and the Grep line goes; find/cat/head guidance stays.
func TestBashDescriptionGrepViaBash(t *testing.T) {
	d := bashDescriptionGrepViaBash
	for _, want := range []string{
		"Avoid using this tool to run `find`, `cat`, `head`, `tail`, `sed`, `awk`, or `echo` commands",
		"File search: Use Glob (NOT find or ls)",
		"Read files: Use Read (NOT cat/head/tail)",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("description missing %q", want)
		}
	}
	for _, not := range []string{"`grep`, `cat`", "Content search", "Grep"} {
		if strings.Contains(d, not) {
			t.Errorf("description still has %q", not)
		}
	}
}

func TestSearchNoticeRendered(t *testing.T) {
	out := renderGrep(filetools.GrepResult{Content: "a.go:1:x", Notice: "ugrep could not start"})
	if out != "a.go:1:x\n(ugrep could not start)" {
		t.Fatalf("renderGrep = %q", out)
	}
	if renderGlob(filetools.GlobResult{Filenames: []string{"a"}}) != "a" {
		t.Fatal("no notice must leave Glob output unchanged")
	}
}

func TestWithSearchEngineReachesTools(t *testing.T) {
	f := false
	e := filetools.NewSearchEngine(filetools.SearchSettings{UseUgrep: &f})
	d := New(toolreg.ModeBypassPermissions, nil, WithSearchEngine(e))
	if d.SearchEngine != e {
		t.Fatal("WithSearchEngine not applied")
	}
}

func schemaNames(d *Dispatcher) map[string]agent.ToolSchema {
	out := map[string]agent.ToolSchema{}
	for _, s := range d.Schemas() {
		out[s.Name] = s
	}
	return out
}

// TestUgrepShadowReplacesGrepTool: with the shadow the Grep tool is neither
// offered nor callable, Glob stays, and the Bash, EnterPlanMode descriptions
// point at `grep`; without it nothing changes.
func TestUgrepShadowReplacesGrepTool(t *testing.T) {
	plain := schemaNames(New(toolreg.ModeBypassPermissions, nil))
	if _, ok := plain["Grep"]; !ok {
		t.Fatal("Grep must be offered without the shadow")
	}
	d := New(toolreg.ModeBypassPermissions, nil, WithUgrepShadow("/opt/ugrep"))
	shadowed := schemaNames(d)
	if _, ok := shadowed["Grep"]; ok {
		t.Fatal("Grep must not be offered with the shadow")
	}
	if _, ok := shadowed["Glob"]; !ok {
		t.Fatal("Glob stays offered")
	}
	if !d.GrepViaBash() || d.UgrepShadow() != "/opt/ugrep" {
		t.Fatal("accessors")
	}
	if shadowed["Bash"].Description != bashDescriptionGrepViaBash || plain["Bash"].Description == bashDescriptionGrepViaBash {
		t.Fatal("Bash description variant not applied")
	}
	if ep, ok := shadowed["EnterPlanMode"]; ok && !strings.Contains(ep.Description, "`grep` (via Bash)") {
		t.Fatalf("EnterPlanMode description: %q", ep.Description)
	}
	in, _ := json.Marshal(map[string]any{"pattern": "x"})
	res, _ := d.Dispatch(context.Background(), agent.ToolUse{ID: "1", Name: "Grep", Input: in})
	if !res.IsError {
		t.Fatalf("a Grep call must be refused with the shadow: %+v", res)
	}
}

// TestUgrepShadowBashRunsUgrep: a read-only `grep` command is still allowed
// without a prompt (its classification does not change) and runs the
// shadowing ugrep.
func TestUgrepShadowBashRunsUgrep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "ugrep")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ugrep-ran \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(toolreg.ModeDefault, nil, WithUgrepShadow(bin), WithWorkingDir(dir))
	in, _ := json.Marshal(map[string]any{"command": "grep -rn needle ."})
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "1", Name: "Bash", Input: in})
	if err != nil || res.IsError {
		t.Fatalf("grep must run without a prompt: %v %+v", err, res)
	}
	if !strings.Contains(res.Content, "ugrep-ran -G --ignore-files --hidden -I") || !strings.Contains(res.Content, "-rn needle .") {
		t.Fatalf("grep did not run ugrep: %q", res.Content)
	}
}
