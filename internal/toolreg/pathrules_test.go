package toolreg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

func pathRuleFixture(t *testing.T) (d *Decider, cwd, home, cfg string) {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	cwd = filepath.Join(base, "proj")
	home = filepath.Join(base, "home")
	cfg = filepath.Join(home, ".config", "gocode")
	for _, dir := range []string{filepath.Join(cwd, "secrets"), filepath.Join(cwd, "src", "deep"), filepath.Join(home, ".ssh"), cfg} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d = &Decider{Registry: DefaultRegistry(), PathRules: &pathguard.PathRuleRoots{Cwd: cwd, Home: home, UserConfigDir: cfg}}
	return d, cwd, home, cfg
}

func pathCall(tool protocol.Tool, p string) Call {
	op := pathguard.OpRead
	if pathRuleKind(tool) == protocol.ToolEdit {
		op = pathguard.OpWrite
	}
	return Call{Tool: tool, Rule: string(tool), RuleContent: p, Path: p, Op: op}
}

// File-path rules are gitignore patterns under a root, not raw strings,
// and Read deny rules reach Grep and Glob.
func TestPathRulesGitignoreSemantics(t *testing.T) {
	d, cwd, home, cfg := pathRuleFixture(t)
	rules := RuleSet{
		{Behavior: RuleDeny, ToolName: "Read", Content: "./secrets/**", Source: "projectSettings"},
		{Behavior: RuleDeny, ToolName: "Read", Content: "*.pem", Source: "localSettings"},
		{Behavior: RuleDeny, ToolName: "Read", Content: "~/.ssh/**", Source: "userSettings"},
		{Behavior: RuleDeny, ToolName: "Read", Content: "//etc/shadow", Source: "userSettings"},
		{Behavior: RuleDeny, ToolName: "Read", Content: "/private.txt", Source: "userSettings"},
	}
	cases := []struct {
		tool protocol.Tool
		path string
		deny bool
	}{
		{protocol.ToolRead, filepath.Join(cwd, "secrets", "key"), true},
		{protocol.ToolRead, filepath.Join(cwd, "secrets"), true},
		{protocol.ToolGrep, filepath.Join(cwd, "secrets"), true},
		{protocol.ToolGlob, filepath.Join(cwd, "secrets"), true},
		{protocol.ToolRead, filepath.Join(cwd, "src", "deep", "server.pem"), true},
		{protocol.ToolRead, filepath.Join(cwd, "src", "deep", "server.pem.txt"), false},
		{protocol.ToolRead, filepath.Join(home, ".ssh", "id_ed25519"), true},
		{protocol.ToolGrep, filepath.Join(home, ".ssh"), true},
		{protocol.ToolRead, "/etc/shadow", true},
		{protocol.ToolRead, filepath.Join(cfg, "private.txt"), true}, // "/x" from userSettings: config root
		{protocol.ToolRead, "/private.txt", true},                    // tightened: also absolute
		{protocol.ToolRead, filepath.Join(cwd, "src", "main.go"), false},
		{protocol.ToolRead, filepath.Join(cwd, "secretsX"), false},
		// Read rules do not reach the edit tools (only a match-all one does).
		{protocol.ToolEdit, filepath.Join(cwd, "secrets", "key"), false},
	}
	for _, c := range cases {
		if runtime.GOOS == "windows" && strings.HasPrefix(c.path, "/") {
			continue // a drive-less POSIX absolute path
		}
		dec := d.Decide(pathCall(c.tool, c.path), ModeBypassPermissions, rules, nil)
		if got := dec.Behavior == BehaviorDeny; got != c.deny {
			t.Errorf("%s %s: deny=%v want %v (%s)", c.tool, c.path, got, c.deny, dec.Reason)
		}
	}
	// A path given relative / with ~ is normalized before matching.
	if dec := d.Decide(pathCall(protocol.ToolGrep, "~/.ssh"), ModeBypassPermissions, rules, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("~/.ssh not normalized: %s", dec.Behavior)
	}
	if dec := d.Decide(pathCall(protocol.ToolGlob, "secrets/../secrets"), ModeBypassPermissions, rules, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("relative dotted path not normalized: %s", dec.Behavior)
	}
}

// A symlink is judged on both its own spelling and where it resolves: a deny
// on the target refuses the link; an allow must cover both.
func TestPathRulesSymlinkSpellings(t *testing.T) {
	d, cwd, _, _ := pathRuleFixture(t)
	link := filepath.Join(cwd, "src", "innocent")
	if err := os.Symlink(filepath.Join(cwd, "secrets"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Read", Content: "./secrets/**", Source: "projectSettings"}}
	if dec := d.Decide(pathCall(protocol.ToolRead, filepath.Join(link, "key")), ModeBypassPermissions, deny, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("deny bypassed through symlink: %s", dec.Behavior)
	}
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Edit", Content: "./src/**", Source: "session"}}
	if dec := d.Decide(pathCall(protocol.ToolWrite, filepath.Join(cwd, "src", "a.go")), ModeDefault, allow, nil); dec.Behavior != BehaviorAllow {
		t.Errorf("Edit allow rule not applied to Write in src: %s", dec.Behavior)
	}
	if dec := d.Decide(pathCall(protocol.ToolWrite, filepath.Join(link, "key")), ModeDefault, allow, nil); dec.Behavior == BehaviorAllow {
		t.Errorf("Edit allow under src granted a write that lands in secrets via a symlink")
	}
}

// Edit rules govern Write / MultiEdit / NotebookEdit; a whole-filesystem Read
// deny reaches the edit tools, a whole-filesystem Edit deny reaches
// NotebookEdit.
func TestPathRulesCrossToolPropagation(t *testing.T) {
	d, cwd, _, _ := pathRuleFixture(t)
	target := filepath.Join(cwd, "src", "x.ipynb")
	editDeny := RuleSet{{Behavior: RuleDeny, ToolName: "Edit", Content: "./src/**", Source: "projectSettings"}}
	for _, tl := range []protocol.Tool{protocol.ToolEdit, protocol.ToolWrite, protocol.ToolMultiEdit, protocol.ToolNotebookEdit} {
		if dec := d.Decide(pathCall(tl, target), ModeBypassPermissions, editDeny, nil); dec.Behavior != BehaviorDeny {
			t.Errorf("Edit(./src/**) deny missed %s: %s", tl, dec.Behavior)
		}
	}
	if dec := d.Decide(pathCall(protocol.ToolRead, target), ModeBypassPermissions, editDeny, nil); dec.Behavior == BehaviorDeny {
		t.Errorf("Edit deny leaked onto Read")
	}
	if runtime.GOOS == "windows" {
		return // the "//" root below is the POSIX filesystem root
	}
	readAll := RuleSet{{Behavior: RuleDeny, ToolName: "Read", Content: "//**", Source: "policySettings"}}
	for _, tl := range []protocol.Tool{protocol.ToolRead, protocol.ToolGrep, protocol.ToolGlob, protocol.ToolEdit, protocol.ToolWrite} {
		if dec := d.Decide(pathCall(tl, target), ModeBypassPermissions, readAll, nil); dec.Behavior != BehaviorDeny {
			t.Errorf("Read(//**) deny missed %s: %s", tl, dec.Behavior)
		}
	}
	readAsk := RuleSet{{Behavior: RuleAsk, ToolName: "Read", Content: "//**", Source: "policySettings"}}
	if dec := d.Decide(pathCall(protocol.ToolGrep, target), ModeAuto, readAsk, nil); dec.Behavior != BehaviorAsk {
		t.Errorf("Read(//**) ask missed Grep: %s", dec.Behavior)
	}
	if dec := d.Decide(pathCall(protocol.ToolNotebookEdit, target), ModeBypassPermissions, readAll, nil); dec.Behavior == BehaviorDeny {
		t.Errorf("Read(//**) deny reached NotebookEdit (a Read rule must not cover NotebookEdit)")
	}
	editAll := RuleSet{{Behavior: RuleDeny, ToolName: "Edit", Content: "//**", Source: "policySettings"}}
	if dec := d.Decide(pathCall(protocol.ToolNotebookEdit, target), ModeBypassPermissions, editAll, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("Edit(//**) deny missed NotebookEdit: %s", dec.Behavior)
	}
	// The root-anchored rule a "don't ask again" saves ("//abs") allows the file.
	saved := RuleSet{{Behavior: RuleAllow, ToolName: "Edit", Content: "/" + target, Source: "session"}}
	if dec := d.Decide(pathCall(protocol.ToolEdit, target), ModeDefault, saved, nil); dec.Behavior != BehaviorAllow {
		t.Errorf("saved //abs allow rule not honoured: %s", dec.Behavior)
	}
}

func TestReadDeniedForSearchResults(t *testing.T) {
	d, cwd, _, _ := pathRuleFixture(t)
	rules := RuleSet{{Behavior: RuleDeny, ToolName: "Read", Content: "secrets/**", Source: "projectSettings"}}
	if !d.ReadDenied(rules, filepath.Join(cwd, "secrets", "a")) {
		t.Error("search result under a denied dir not excluded")
	}
	if d.ReadDenied(rules, filepath.Join(cwd, "src", "a")) {
		t.Error("allowed result excluded")
	}
	if d.ReadDenied(nil, filepath.Join(cwd, "secrets", "a")) {
		t.Error("no rules but excluded")
	}
}
