package pathguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestSessionFiles builds a SessionFiles over a scratch tree:
// <tmp>/home (home), <tmp>/home/.config/gocode (config), <tmp>/proj (cwd).
func newTestSessionFiles(t *testing.T) (*SessionFiles, string) {
	t.Helper()
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
	return &SessionFiles{
		ConfigDir:   cfg,
		HomeDir:     home,
		OriginalCwd: proj,
		Cwd:         proj,
		Platform:    "linux",
		SettingsFiles: []string{
			filepath.Join(cfg, "settings.json"),
			filepath.Join(proj, ".gocode", "settings.json"),
			filepath.Join(proj, ".gocode", "settings.local.json"),
			filepath.Join(root, "etc", "gocode", "managed-settings.json"),
		},
		ManagedDropInDirs: []string{filepath.Join(root, "etc", "gocode", "managed-settings.d")},
	}, root
}

func TestFoldSegment(t *testing.T) {
	cases := map[string]string{
		".Git":          ".git",
		".git.":         ".git",
		".git  ":        ".git",
		".git:stream":   ".git",
		".gi\u200ct":    ".git",
		"...":           "...",
		"Settings.JSON": "settings.json",
		"\u0131dea":     "idea",
	}
	for in, want := range cases {
		if got := foldSegment(in); got != want {
			t.Errorf("foldSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckInternalWriteHardDenies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	f, root := newTestSessionFiles(t)
	f.HostCredsFile = filepath.Join(root, "creds", "host.json")

	cases := []struct {
		path string
		want string
	}{
		{filepath.Join(f.ConfigDir, "seed-admin", "x", "config"), MsgSeedAdminDenied},
		{filepath.Join(f.ConfigDir, "SEED-ADMIN"), MsgSeedAdminDenied},
		{filepath.Join(f.HomeDir, ".gocode", "state", "settings-review.json"), MsgSettingsReviewDenied},
		{filepath.Join(f.HomeDir, ".gocode", "state"), MsgSettingsReviewDenied},
		{filepath.Join(f.ConfigDir, "jobs", "abc", "adopt.json"), MsgAdoptJSONDenied},
		{filepath.Join(f.HomeDir, ".gocode", "jobs", "abc", "adopt.json.tmp"), MsgAdoptJSONDenied},
		{filepath.Join(root, "creds", "HOST.json"), MsgHostCredsDenied},
	}
	for _, c := range cases {
		v := f.CheckInternalWrite(c.path, f.Spellings(c.path), false)
		if v.Behavior != FileDeny || v.Message != c.want {
			t.Errorf("%s: got %+v, want deny %q", c.path, v, c.want)
		}
	}

	// Not hard-denied: a sibling of the state store, a deeper jobs path.
	for _, p := range []string{
		filepath.Join(f.HomeDir, ".gocode", "state", "other.json"),
		filepath.Join(f.ConfigDir, "jobs", "abc", "sub", "adopt.json"),
	} {
		if v := f.CheckInternalWrite(p, f.Spellings(p), false); v.Behavior == FileDeny {
			t.Errorf("%s: unexpected deny %+v", p, v)
		}
	}

	f.Diskless = true
	if v := f.CheckInternalWrite(filepath.Join(f.Cwd, "a.txt"), nil, false); v.Behavior != FileDeny || v.Message != MsgDisklessWriteDenied {
		t.Errorf("diskless: got %+v", v)
	}
}

func TestCheckInternalWriteAllowlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	f, _ := newTestSessionFiles(t)
	f.PlansDir = filepath.Join(f.ConfigDir, "plans")
	f.PlanSlug = "brave-otter"

	allowed := map[string]string{
		filepath.Join(f.Cwd, ".gocode", "agent-memory", "reviewer", "MEMORY.md"):      MsgAgentMemoryAllowed,
		filepath.Join(f.Cwd, ".gocode", "agent-memory-local", "reviewer", "notes.md"): MsgAgentMemoryAllowed,
		filepath.Join(f.ConfigDir, "agent-memory", "reviewer", "topic", "x.md"):       MsgAgentMemoryAllowed,
		filepath.Join(f.PlansDir, "brave-otter.md"):                                   MsgPlanFileAllowed,
		filepath.Join(f.PlansDir, "brave-otter-agent-a1.md"):                          MsgPlanFileAllowed,
		filepath.Join(f.Cwd, ".gocode", "launch.json"):                                MsgLaunchConfigAllowed,
	}
	for p, msg := range allowed {
		v := f.CheckInternalWrite(p, f.Spellings(p), false)
		if v.Behavior != FileAllow || v.Message != msg {
			t.Errorf("%s: got %+v, want allow %q", p, v, msg)
		}
	}

	passthroughs := []string{
		filepath.Join(f.Cwd, ".gocode", "agent-memory", "reviewer", "x.txt"),       // not .md
		filepath.Join(f.Cwd, ".gocode", "agent-memory", "r", ".git", "hooks.md"),   // excluded segment
		filepath.Join(f.Cwd, ".gocode", "agent-memory", "r", "skills", "SKILL.md"), // excluded segment
		filepath.Join(f.PlansDir, "other-slug.md"),
		filepath.Join(f.PlansDir, "sub", "brave-otter.md"),
		filepath.Join(f.Cwd, "src", "main.go"),
	}
	for _, p := range passthroughs {
		if v := f.CheckInternalWrite(p, f.Spellings(p), false); v.Behavior != FilePassthrough {
			t.Errorf("%s: got %+v, want passthrough", p, v)
		}
	}

	// --restricted drops the agent-memory and launch-config entries, but not
	// the plan file.
	f.Restricted = true
	p := filepath.Join(f.Cwd, ".gocode", "agent-memory", "reviewer", "MEMORY.md")
	if v := f.CheckInternalWrite(p, f.Spellings(p), false); v.Behavior != FilePassthrough {
		t.Errorf("restricted agent memory: got %+v", v)
	}
	p = filepath.Join(f.PlansDir, "brave-otter.md")
	if v := f.CheckInternalWrite(p, f.Spellings(p), true); v.Behavior != FileAllow {
		t.Errorf("restricted plan file: got %+v", v)
	}
}

// TestCheckInternalWriteEverySpellingMustAllow: a symlink inside an
// agent-memory dir pointing out of it is not allowlisted.
func TestCheckInternalWriteEverySpellingMustAllow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	f, root := newTestSessionFiles(t)
	memDir := filepath.Join(f.Cwd, ".gocode", "agent-memory", "r")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(memDir, "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sp := f.Spellings(link)
	if len(sp) != 2 || sp[1] != outside {
		t.Fatalf("spellings = %v", sp)
	}
	if v := f.CheckInternalWrite(link, sp, false); v.Behavior != FilePassthrough {
		t.Errorf("got %+v, want passthrough", v)
	}
}

func TestCheckWriteSafety(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	f, root := newTestSessionFiles(t)
	notGranted := func(p string) string { return MsgWriteNotGranted(p) }

	unsafe := map[string]string{
		filepath.Join(f.Cwd, ".gocode", "settings.json"):                     notGranted(filepath.Join(f.Cwd, ".gocode", "settings.json")),
		filepath.Join(f.Cwd, "sub", ".gocode", "settings.local.json"):        notGranted(filepath.Join(f.Cwd, "sub", ".gocode", "settings.local.json")),
		filepath.Join(f.ConfigDir, "settings.json"):                          notGranted(filepath.Join(f.ConfigDir, "settings.json")),
		filepath.Join(f.HomeDir, ".gocode", "cowork_settings.json"):          notGranted(filepath.Join(f.HomeDir, ".gocode", "cowork_settings.json")),
		filepath.Join(root, "etc", "gocode", "managed-settings.d", "a.json"): notGranted(filepath.Join(root, "etc", "gocode", "managed-settings.d", "a.json")),
		filepath.Join(f.Cwd, ".gocode"):                                      notGranted(filepath.Join(f.Cwd, ".gocode")),
		filepath.Join(f.Cwd, ".gocode", "commands", "deploy.md"):             notGranted(filepath.Join(f.Cwd, ".gocode", "commands", "deploy.md")),
		filepath.Join(f.Cwd, ".gocode", "skills", "s", "SKILL.md"):           notGranted(filepath.Join(f.Cwd, ".gocode", "skills", "s", "SKILL.md")),
		filepath.Join(f.Cwd, ".bashrc"):                                      MsgSensitiveFile(filepath.Join(f.Cwd, ".bashrc")),
		filepath.Join(f.HomeDir, ".ZSHRC"):                                   MsgSensitiveFile(filepath.Join(f.HomeDir, ".ZSHRC")),
		filepath.Join(f.Cwd, ".git", "config"):                               MsgSensitiveFile(filepath.Join(f.Cwd, ".git", "config")),
		filepath.Join(f.Cwd, ".vscode", "tasks.json"):                        MsgSensitiveFile(filepath.Join(f.Cwd, ".vscode", "tasks.json")),
		filepath.Join(f.HomeDir, ".config", "git", "config"):                 MsgSensitiveFile(filepath.Join(f.HomeDir, ".config", "git", "config")),
		filepath.Join(f.Cwd, ".gocode", "other.txt"):                         MsgSensitiveFile(filepath.Join(f.Cwd, ".gocode", "other.txt")),
		filepath.Join(f.Cwd, ".mcp.json"):                                    MsgSensitiveFile(filepath.Join(f.Cwd, ".mcp.json")),
		filepath.Join(f.Cwd, ".gocode", "worktrees", "w", ".gocode", "x"):    MsgSensitiveFile(filepath.Join(f.Cwd, ".gocode", "worktrees", "w", ".gocode", "x")),
	}
	for p, msg := range unsafe {
		ws := f.CheckWriteSafety(p, f.Spellings(p))
		if ws.Safe || ws.Message != msg {
			t.Errorf("%s: got %+v, want unsafe %q", p, ws, msg)
		}
	}

	safe := []string{
		filepath.Join(f.Cwd, "src", "main.go"),
		filepath.Join(f.Cwd, "settings.json"),
		filepath.Join(f.Cwd, ".gocode", "worktrees", "w", "src", "a.go"),
		filepath.Join(f.Cwd, "docs", "git", "config"),
	}
	for _, p := range safe {
		if ws := f.CheckWriteSafety(p, f.Spellings(p)); !ws.Safe {
			t.Errorf("%s: got unsafe %+v", p, ws)
		}
	}

	// A settings-file hit carries the gocodeSettingsFile breaker.
	p := filepath.Join(f.Cwd, ".gocode", "settings.json")
	if ws := f.CheckWriteSafety(p, nil); ws.CircuitBreaker != CircuitBreakerSettingsFile {
		t.Errorf("breaker = %q", ws.CircuitBreaker)
	}
	// Control characters in the path are escaped in the message.
	if got := MsgSensitiveFile("/a/.bashrc\n"); !strings.Contains(got, "&#10;") {
		t.Errorf("message not escaped: %q", got)
	}
}

// TestCheckWriteSafetySettingsLink: a path a symlinked settings file leads to
// is a settings file, and the message names the link.
func TestCheckWriteSafetySettingsLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	f, root := newTestSessionFiles(t)
	target := filepath.Join(root, "dotfiles", "gocode-settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.ConfigDir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	ws := f.CheckWriteSafety(target, f.Spellings(target))
	if ws.Safe || ws.CircuitBreaker != CircuitBreakerSettingsFile {
		t.Fatalf("got %+v", ws)
	}
	if !strings.Contains(ws.Message, "leads here through a link") {
		t.Errorf("message = %q", ws.Message)
	}
	// Writing through the link itself names no link.
	ws = f.CheckWriteSafety(link, f.Spellings(link))
	if ws.Safe || strings.Contains(ws.Message, "through a link") {
		t.Errorf("via link: %+v", ws)
	}
}

func TestWithinWorkingDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	f, root := newTestSessionFiles(t)
	in := filepath.Join(f.Cwd, "a", "b.go")
	if !f.WithinWorkingDirs(f.Spellings(in)) {
		t.Error("cwd file not within")
	}
	out := filepath.Join(root, "elsewhere", "b.go")
	if f.WithinWorkingDirs(f.Spellings(out)) {
		t.Error("outside file within")
	}
	f.AdditionalWorkingDirs = []string{filepath.Join(root, "elsewhere")}
	if !f.WithinWorkingDirs(f.Spellings(out)) {
		t.Error("added dir file not within")
	}
	// A symlink in cwd pointing outside is not within (every spelling).
	if err := os.Symlink(filepath.Join(root, "home"), filepath.Join(f.Cwd, "h")); err != nil {
		t.Fatal(err)
	}
	if f.WithinWorkingDirs(f.Spellings(filepath.Join(f.Cwd, "h", "x"))) {
		t.Error("symlinked escape counted as within")
	}
	if (&SessionFiles{}).WithinWorkingDirs([]string{"/a"}) {
		t.Error("no working dirs must confine to nothing")
	}
}

func TestToWindowsSpelling(t *testing.T) {
	f := &SessionFiles{Platform: "windows"}
	cases := map[string]string{
		"/c/Users/x":      `C:\Users\x`,
		"/cygdrive/d/a/b": `D:\a\b`,
		`C:\already`:      `C:\already`,
		"/usr/lib":        "/usr/lib",
	}
	for in, want := range cases {
		if got := f.toWindowsSpelling(in); got != want {
			t.Errorf("toWindowsSpelling(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (&SessionFiles{Platform: "linux"}).toWindowsSpelling("/c/x"); got != "/c/x" {
		t.Errorf("linux converted: %q", got)
	}
}
