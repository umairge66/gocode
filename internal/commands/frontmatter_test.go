package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// realWorldCommand uses every recognised key, in the styles real files use:
// CRLF line endings, comments, quoted values, a block list, a folded
// description, a whitespace-split arguments string.
const realWorldCommand = "---\r\n" +
	"# Release helper\r\n" +
	"name: ship   # display name\r\n" +
	"description: >-\r\n" +
	"  Cut a release:\r\n" +
	"  tag, push and publish\r\n" +
	"model: Inherit\r\n" +
	"allowed-tools:\r\n" +
	"  - Bash(git tag:*)\r\n" +
	"  - \"Read\"\r\n" +
	"disallowed-tools: [Write, Edit]\r\n" +
	"argument-hint: '<version> [notes]'\r\n" +
	"arguments: version notes 2\r\n" +
	"disable-model-invocation: yes\r\n" +
	"user-invocable: \"false\"\r\n" +
	"effort: MED\r\n" +
	"shell: PWSH\r\n" +
	"when_to_use: \"When the user says \\\"ship it\\\"\"\r\n" +
	"version: 1.0\r\n" +
	"---\r\n" +
	"Release $version with $notes\r\n"

func TestRealWorldCommandEveryKey(t *testing.T) {
	root := t.TempDir()
	writeCmd(t, filepath.Join(root, "release.md"), realWorldCommand)
	cmds, err := LoadDir(root, SourceProject)
	if err != nil || len(cmds) != 1 {
		t.Fatalf("LoadDir: %v, %d cmds", err, len(cmds))
	}
	c := cmds[0]
	checks := []struct {
		key       string
		got, want any
	}{
		{"name", c.Name, "project:ship"},
		{"description", c.Description, "Cut a release: tag, push and publish"},
		{"model", c.Model, "inherit"},
		{"allowed-tools", c.AllowedTools, []string{"Bash(git tag:*)", "Read"}},
		{"disallowed-tools", c.DisallowedTools, []string{"Write", "Edit"}},
		{"argument-hint", c.ArgumentHint, "<version> [notes]"},
		{"arguments", c.ArgNames, []string{"version", "notes"}},
		{"disable-model-invocation", c.DisableModelInvocation, true},
		{"user-invocable", c.UserInvocable, false},
		{"hidden", c.Hidden, true},
		{"effort", c.Effort, "medium"},
		{"shell", c.ShellSetting(), "powershell"},
		{"when_to_use", c.WhenToUse, `When the user says "ship it"`},
		{"version", c.Version, "1"},
	}
	for _, ck := range checks {
		if !reflect.DeepEqual(ck.got, ck.want) {
			t.Errorf("%s = %#v, want %#v", ck.key, ck.got, ck.want)
		}
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings = %q", c.Warnings)
	}
	// arguments take effect in expansion.
	out, err := Expand(c.Body, "2.0 notes-here", ExpandOptions{ArgNames: c.ArgNames, Append: true})
	if err != nil || !strings.Contains(out, "Release 2.0 with notes-here") {
		t.Errorf("expand = %q, %v", out, err)
	}
}

func TestInvalidValuesFallBackWithWarning(t *testing.T) {
	fm, _ := ParseFrontmatter("---\nshell: fish\neffort: ludicrous\n---\nbody")
	c := BuildCommand("/x/cmd.md", "/x", SourceUser, fm, "body")
	if c.Shell != "bash" || c.ShellSetting() != "" || c.Effort != "" {
		t.Fatalf("shell=%q setting=%q effort=%q", c.Shell, c.ShellSetting(), c.Effort)
	}
	joined := strings.Join(c.Warnings, "\n")
	if !strings.Contains(joined, "'shell: fish' is not recognized") || !strings.Contains(joined, "invalid effort 'ludicrous'") {
		t.Fatalf("warnings = %q", c.Warnings)
	}
}

func TestMalformedFrontmatterStillLoads(t *testing.T) {
	root := t.TempDir()
	writeCmd(t, filepath.Join(root, "bad.md"), "---\ndescription: ok\n  bogus: indented\n---\n# Heading line\nbody\n")
	cmds, _ := LoadDir(root, SourceProject)
	if len(cmds) != 1 {
		t.Fatalf("want the command to load, got %d", len(cmds))
	}
	c := cmds[0]
	if c.Description != "Heading line" || c.HasUserSpecifiedDescription {
		t.Errorf("defaults not applied: %q", c.Description)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "failed to parse and was ignored: line 3:") {
		t.Errorf("warnings = %q", c.Warnings)
	}
}

func TestSymlinkedDuplicateLoadedOnce(t *testing.T) {
	root := t.TempDir()
	writeCmd(t, filepath.Join(root, "real", "deploy.md"), "---\ndescription: d\n---\nx")
	if err := os.Symlink(filepath.Join(root, "real", "deploy.md"), filepath.Join(root, "alias.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A symlinked directory pointing back at the root (a cycle) and a second
	// view of real/.
	if err := os.Symlink(root, filepath.Join(root, "real", "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "view")); err != nil {
		t.Fatal(err)
	}
	cmds, err := LoadDir(root, SourceProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		var names []string
		for _, c := range cmds {
			names = append(names, c.FilePath)
		}
		t.Fatalf("want 1 command, got %d: %v", len(cmds), names)
	}
	if !strings.Contains(strings.Join(cmds[0].Warnings, "\n"), "Skipping duplicate file") {
		t.Errorf("no duplicate note: %q", cmds[0].Warnings)
	}

	// Across roots: the same file reached via another root also dedups.
	other := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "real", "deploy.md"), filepath.Join(other, "deploy.md")); err != nil {
		t.Fatal(err)
	}
	more, _ := LoadDir(other, SourceUser)
	if all := DedupeByFile(append(cmds, more...)); len(all) != 1 || all[0].Source != SourceProject {
		t.Fatalf("cross-root dedup kept %d", len(all))
	}
}

func TestExpandShellBlocksCarriesShellOnContext(t *testing.T) {
	var seen string
	runner := func(ctx context.Context, cmd string) (string, error) {
		seen = CommandShellFrom(ctx)
		return "ok", nil
	}
	out, err := ExpandShellBlocks(context.Background(), "a !`x` b", ShellExecOptions{Runner: runner, Authorize: allowAllShell, Shell: "zsh"})
	if err != nil || out != "a ok b" || seen != "zsh" {
		t.Fatalf("out=%q err=%v shell=%q", out, err, seen)
	}
}

// TestShellFrontmatterHonored runs real `!`-blocks through DefaultShellRunner
// under `shell: zsh` / `shell: sh`, skipping shells that are not installed.
func TestShellFrontmatterHonored(t *testing.T) {
	for _, sh := range []string{"zsh", "sh", "bash"} {
		t.Run(sh, func(t *testing.T) {
			if _, err := exec.LookPath(sh); err != nil {
				t.Skipf("%s not installed", sh)
			}
			fm, body := ParseFrontmatter("---\nshell: " + sh + "\n---\nshell is !`echo $0`")
			c := BuildCommand("/x/c.md", "/x", SourceProject, fm, body)
			out, err := ExpandShellBlocks(context.Background(), c.Body, ShellExecOptions{Runner: DefaultShellRunner(0), Authorize: allowAllShell, Shell: c.ShellSetting()})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out, "shell is ") || !strings.Contains(out, sh) {
				t.Fatalf("out = %q, want the %s interpreter", out, sh)
			}
		})
	}
}
