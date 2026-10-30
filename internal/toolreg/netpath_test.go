package toolreg

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

func readCall(tool protocol.Tool, p string) Call {
	return Call{Tool: tool, Rule: string(tool), RuleContent: p, Path: p, Op: pathguard.OpRead}
}

// netDecider builds a session over a temp dir holding a symlink onto the /net
// automount map. The link target is never touched by the permission walk.
func netDecider(t *testing.T) (*Decider, *pathguard.SessionFiles, string) {
	t.Helper()
	proj := t.TempDir()
	if err := os.Symlink("/net/fileserver/export", filepath.Join(proj, "share")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	f := &pathguard.SessionFiles{OriginalCwd: proj, Cwd: proj, Platform: "linux", Stalls: &fsfacade.StallSet{}}
	return &Decider{Registry: DefaultRegistry(), Files: f}, f, proj
}

func TestReadThroughLinkOntoAutomountAsks(t *testing.T) {
	d, _, proj := netDecider(t)
	p := filepath.Join(proj, "share", "data.csv")
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Read"}}
	for _, tool := range []protocol.Tool{protocol.ToolRead, protocol.ToolGlob, protocol.ToolGrep} {
		for _, m := range []Mode{ModeDefault, ModeAcceptEdits, ModePlan, ModeAuto} {
			got := d.Decide(readCall(tool, p), m, allow, nil)
			if got.Behavior != BehaviorAsk || got.ReasonType != ReasonSafetyCheck ||
				!strings.Contains(got.Reason, "under the /net automount map") {
				t.Errorf("%s/%s: got %+v", tool, m, got)
			}
		}
	}
	// Bypass skips the safety ask (no bypass-immune circuit breaker).
	if got := d.Decide(readCall(protocol.ToolRead, p), ModeBypassPermissions, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("bypass: %+v", got)
	}
	// A direct /net path asks too.
	if got := d.Decide(readCall(protocol.ToolRead, "/net/host/etc/passwd"), ModeDefault, nil, nil); got.Behavior != BehaviorAsk || got.ReasonType != ReasonSafetyCheck {
		t.Errorf("direct: %+v", got)
	}
	// A deny rule still wins.
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Read"}}
	if got := d.Decide(readCall(protocol.ToolRead, p), ModeDefault, deny, nil); got.Behavior != BehaviorDeny {
		t.Errorf("deny rule: %+v", got)
	}
}

func TestReadLocalUnchanged(t *testing.T) {
	d, _, proj := netDecider(t)
	p := filepath.Join(proj, "a.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := d.Decide(readCall(protocol.ToolRead, p), ModeDefault, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("local read: %+v", got)
	}
}

func TestReadTrustedNetworkDirectoryExempt(t *testing.T) {
	d, f, proj := netDecider(t)
	f.TrustedNetworkDirectories = []string{"/net/fileserver/export"}
	// No network safety check for a trusted share; the link still lands
	// outside the working directories, so the read asks as an outside read.
	if got := d.Decide(readCall(protocol.ToolRead, filepath.Join(proj, "share", "a")), ModeDefault, nil, nil); got.Behavior != BehaviorAsk || got.ReasonType != ReasonWorkingDir {
		t.Errorf("trusted: %+v", got)
	}
}

// Writes through a link onto the automount map are asked about as a
// sensitive file (checked on the surfaced spelling).
func TestWriteThroughLinkOntoAutomountAsks(t *testing.T) {
	d, _, proj := netDecider(t)
	p := filepath.Join(proj, "share", "out.txt")
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Write"}}
	got := d.Decide(writeCall(p), ModeAcceptEdits, allow, nil)
	if got.Behavior != BehaviorAsk || got.ReasonType != ReasonSafetyCheck {
		t.Errorf("write: %+v", got)
	}
}

// stalledFS blocks every probe below root until released.
type stalledFS struct {
	fsfacade.OSFS
	root    string
	release chan struct{}
}

func (s stalledFS) Lstat(name string) (fs.FileInfo, error) {
	if name == s.root || strings.HasPrefix(name, s.root+string(filepath.Separator)) {
		<-s.release
	}
	return s.OSFS.Lstat(name)
}

// A stalled mount never hangs the permission check: the probe times out and
// the read or write is refused — in every mode, bypass included.
func TestStalledMountRefusedNotHung(t *testing.T) {
	proj := t.TempDir()
	dead := filepath.Join(proj, "nfs")
	if err := os.MkdirAll(filepath.Join(dead, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	f := &pathguard.SessionFiles{
		OriginalCwd: proj, Cwd: proj, Platform: "linux",
		FS:           stalledFS{root: dead, release: release},
		ProbeTimeout: 40 * time.Millisecond,
		Stalls:       &fsfacade.StallSet{},
	}
	d := &Decider{Registry: DefaultRegistry(), Files: f}
	start := time.Now()
	got := d.Decide(readCall(protocol.ToolRead, filepath.Join(dead, "dir", "f")), ModeBypassPermissions, nil, nil)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("permission check hung for %v", time.Since(start))
	}
	if got.Behavior != BehaviorDeny || !strings.Contains(got.Reason, "where it leads on disk could not be determined") {
		t.Errorf("read: %+v", got)
	}
	got = d.Decide(writeCall(filepath.Join(dead, "dir", "g")), ModeBypassPermissions, nil, nil)
	if got.Behavior != BehaviorDeny || !strings.HasPrefix(got.Reason, "Refusing to write ") {
		t.Errorf("write: %+v", got)
	}
}
