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

// fileDecider returns a Decider with session file checks over a scratch tree.
func fileDecider(t *testing.T) (*Decider, *pathguard.SessionFiles, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	home := filepath.Join(root, "home")
	cfg := filepath.Join(home, ".config", "gocode")
	proj := filepath.Join(root, "proj")
	for _, d := range []string{cfg, proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &pathguard.SessionFiles{
		ConfigDir: cfg, HomeDir: home, OriginalCwd: proj, Cwd: proj, Platform: "linux",
		SettingsFiles: []string{filepath.Join(cfg, "settings.json")},
	}
	return &Decider{Registry: DefaultRegistry(), Files: f}, f, root
}

func writeCall(p string) Call {
	return Call{Tool: protocol.ToolWrite, Rule: "Write", RuleContent: p, Path: p, Op: pathguard.OpWrite}
}

func TestFileChecksSensitiveFileAsksDespiteAllowRule(t *testing.T) {
	d, f, _ := fileDecider(t)
	p := filepath.Join(f.Cwd, ".bashrc")
	rules := RuleSet{{Behavior: RuleAllow, ToolName: "Write"}}
	for _, m := range []Mode{ModeDefault, ModeAcceptEdits, ModeAuto, ModePlan} {
		got := d.Decide(writeCall(p), m, rules, nil)
		if got.Behavior != BehaviorAsk || got.ReasonType != ReasonSafetyCheck {
			t.Errorf("%s: got %+v, want safetyCheck ask", m, got)
		}
		if !strings.Contains(got.Reason, "which is a sensitive file") {
			t.Errorf("%s: reason %q", m, got.Reason)
		}
	}
	// dontAsk turns the ask into a denial downstream; the decider still asks.
	if got := d.Decide(writeCall(p), ModeDontAsk, nil, nil); got.Behavior != BehaviorAsk {
		t.Errorf("dontAsk: %+v", got)
	}
	// bypassPermissions allows: the breaker is not bypass-immune.
	if got := d.Decide(writeCall(p), ModeBypassPermissions, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("bypass: %+v", got)
	}
	// Read of a sensitive file is untouched.
	rc := Call{Tool: protocol.ToolRead, Rule: "Read", RuleContent: p, Path: p, Op: pathguard.OpRead}
	if got := d.Decide(rc, ModeDefault, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("read: %+v", got)
	}
}

func TestFileChecksSettingsFileAsks(t *testing.T) {
	d, f, _ := fileDecider(t)
	p := filepath.Join(f.Cwd, ".gocode", "settings.local.json")
	got := d.Decide(writeCall(p), ModeAcceptEdits, nil, nil)
	if got.Behavior != BehaviorAsk || got.ReasonType != ReasonSafetyCheck || got.Reason != pathguard.MsgWriteNotGranted(p) {
		t.Errorf("got %+v", got)
	}
}

func TestFileChecksHardDenyHoldsUnderBypass(t *testing.T) {
	d, f, _ := fileDecider(t)
	p := filepath.Join(f.ConfigDir, "seed-admin", "repo", "config")
	for _, m := range []Mode{ModeDefault, ModeBypassPermissions, ModeAcceptEdits} {
		got := d.Decide(writeCall(p), m, RuleSet{{Behavior: RuleAllow, ToolName: "Write"}}, nil)
		if got.Behavior != BehaviorDeny || got.Reason != pathguard.MsgSeedAdminDenied {
			t.Errorf("%s: got %+v", m, got)
		}
	}
}

func TestFileChecksAgentMemoryAllowlist(t *testing.T) {
	d, f, _ := fileDecider(t)
	p := filepath.Join(f.Cwd, ".gocode", "agent-memory", "reviewer", "MEMORY.md")
	for _, m := range []Mode{ModeDefault, ModePlan, ModeDontAsk, ModeAcceptEdits} {
		got := d.Decide(writeCall(p), m, nil, nil)
		if got.Behavior != BehaviorAllow || got.Reason != pathguard.MsgAgentMemoryAllowed {
			t.Errorf("%s: got %+v", m, got)
		}
	}
	// A deny rule still wins over the allowlist.
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Write"}}
	if got := d.Decide(writeCall(p), ModeDefault, deny, nil); got.Behavior != BehaviorDeny {
		t.Errorf("deny rule: %+v", got)
	}
	// So does an ask rule.
	ask := RuleSet{{Behavior: RuleAsk, ToolName: "Write"}}
	if got := d.Decide(writeCall(p), ModeDefault, ask, nil); got.Behavior != BehaviorAsk || got.ReasonType != ReasonRule {
		t.Errorf("ask rule: %+v", got)
	}
}

func TestFileChecksPlanModeMessage(t *testing.T) {
	d, f, _ := fileDecider(t)
	p := filepath.Join(f.Cwd, "main.go")
	got := d.Decide(writeCall(p), ModePlan, nil, nil)
	if got.Behavior != BehaviorAsk || got.ReasonType != ReasonMode || got.Mode != ModePlan {
		t.Fatalf("got %+v", got)
	}
	if want := "Cannot write to " + p + " while in plan mode."; got.Reason != want {
		t.Errorf("reason %q, want %q", got.Reason, want)
	}
	// The session's plan file is allowed in plan mode.
	f.PlansDir = filepath.Join(f.ConfigDir, "plans")
	f.PlanSlug = "quiet-heron"
	pf := filepath.Join(f.PlansDir, "quiet-heron.md")
	if got := d.Decide(writeCall(pf), ModePlan, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("plan file: %+v", got)
	}
}

func TestFileChecksAcceptEditsOnlyInsideWorkingDirs(t *testing.T) {
	d, f, root := fileDecider(t)
	in := filepath.Join(f.Cwd, "src", "a.go")
	if got := d.Decide(writeCall(in), ModeAcceptEdits, nil, nil); got.Behavior != BehaviorAllow || got.Mode != ModeAcceptEdits {
		t.Errorf("inside: %+v", got)
	}
	out := filepath.Join(root, "other", "a.go")
	got := d.Decide(writeCall(out), ModeAcceptEdits, nil, nil)
	if got.Behavior != BehaviorAsk || got.Reason != pathguard.MsgWriteNotGranted(out) {
		t.Errorf("outside: %+v", got)
	}
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Write"}}
	if got := d.Decide(writeCall(out), ModeAcceptEdits, allow, nil); got.Behavior != BehaviorAllow || got.ReasonType != ReasonRule {
		t.Errorf("outside with allow rule: %+v", got)
	}
	// Without session file checks the generic acceptEdits allow is unchanged.
	plain := &Decider{Registry: DefaultRegistry()}
	if got := plain.Decide(writeCall(out), ModeAcceptEdits, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("no Files: %+v", got)
	}
}

func TestFileChecksDefaultModeOrdinaryWriteUnchanged(t *testing.T) {
	d, f, _ := fileDecider(t)
	p := filepath.Join(f.Cwd, "src", "a.go")
	if got := d.Decide(writeCall(p), ModeDefault, nil, nil); got.Behavior != BehaviorAsk || got.ReasonType == ReasonSafetyCheck {
		t.Errorf("default: %+v", got)
	}
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Write"}}
	if got := d.Decide(writeCall(p), ModeDefault, allow, nil); got.Behavior != BehaviorAllow {
		t.Errorf("allow rule: %+v", got)
	}
}
