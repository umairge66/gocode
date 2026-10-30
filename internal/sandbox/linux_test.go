package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// realTempDir returns a fresh temp dir by its canonical spelling, the form
// the mount planner emits (on macOS the temp dir is behind a symlink).
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBubblewrapArgvShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bubblewrap mount planning works on POSIX paths")
	}
	root := realTempDir(t)
	proj := filepath.Join(root, "project")
	ssh := filepath.Join(root, "home", ".ssh")
	key := filepath.Join(root, "home", "key.pem")
	for _, d := range []string{proj, ssh, filepath.Join(proj, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Profile{
		Network:    NetworkPolicy{Restricted: true},
		WriteAllow: []string{proj, filepath.Join(root, "missing")},
		ReadDeny:   []string{ssh, key},
		WriteDeny:  []string{filepath.Join(proj, ".git"), filepath.Join(root, "elsewhere", ".bashrc")},
		UnsetEnv:   []string{"BAD"},
		SetEnv:     map[string]string{"GOOD": "1"},
		Shell:      "bash",
	}
	args := BubblewrapArgv(p, "make build", "/bin/bash")

	if args[0] != "--new-session" || args[1] != "--die-with-parent" {
		t.Errorf("argv must start with session hardening: %v", args[:2])
	}
	j := strings.Join(args, "\x00")
	for _, want := range []string{
		"--unsetenv\x00BAD",
		"--setenv\x00GOOD\x001",
		"--unshare-net",
		"--ro-bind\x00/\x00/",
		"--bind\x00" + proj + "\x00" + proj,
		"--tmpfs\x00" + ssh,
		"--ro-bind\x00/dev/null\x00" + key,
		"--ro-bind\x00" + filepath.Join(proj, ".git") + "\x00" + filepath.Join(proj, ".git"),
		"--dev\x00/dev",
		"--unshare-pid",
		"--unshare-user\x00--cap-drop\x00ALL",
		"--proc\x00/proc",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("bwrap args missing %q\ngot: %v", strings.ReplaceAll(want, "\x00", " "), args)
		}
	}
	// A write root that does not exist, and a deny outside every write root,
	// produce no mounts.
	if strings.Contains(j, "missing") || strings.Contains(j, "elsewhere") {
		t.Errorf("unexpected mount for a missing root / an outside deny: %v", args)
	}
	n := len(args)
	if args[n-4] != "--" || args[n-3] != "/bin/bash" || args[n-2] != "-c" || args[n-1] != "make build" {
		t.Errorf("bwrap tail wrong: %v", args[n-4:])
	}
}

func TestBubblewrapNoNetUnshareWhenOpen(t *testing.T) {
	p := Profile{Network: NetworkPolicy{Restricted: false}, WriteUnrestricted: true}
	args := BubblewrapArgv(p, "echo", "/bin/bash")
	for _, a := range args {
		if a == "--unshare-net" {
			t.Error("unrestricted network must not emit --unshare-net")
		}
	}
	if !strings.Contains(strings.Join(args, " "), "--bind / /") {
		t.Errorf("unrestricted writes bind / read-write: %v", args)
	}
}

func TestBubblewrapBridge(t *testing.T) {
	p := Profile{
		Network:           NetworkPolicy{Restricted: true, Bridge: &LinuxBridge{HTTPSocket: "/tmp/g/gocode-http-1.sock", SOCKSSocket: "/tmp/g/gocode-http-1.sock", Socat: "/usr/bin/socat"}},
		WriteUnrestricted: true,
		DisableUserns:     true,
	}
	args := BubblewrapArgv(p, "curl https://example.com", "/bin/bash")
	j := strings.Join(args, "\x00")
	for _, want := range []string{
		"--bind\x00/tmp/g/gocode-http-1.sock\x00/tmp/g/gocode-http-1.sock",
		"--setenv\x00HTTPS_PROXY\x00http://localhost:3128",
		"--setenv\x00ALL_PROXY\x00http://localhost:3128",
		"--setenv\x00NO_PROXY\x00localhost,127.0.0.1,::1,169.254.0.0/16,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
		"--setenv\x00SANDBOX_RUNTIME\x001",
		"--disable-userns",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("bwrap args missing %q", strings.ReplaceAll(want, "\x00", " "))
		}
	}
	if strings.Count(j, "--bind\x00/tmp/g/gocode-http-1.sock") != 1 {
		t.Error("one mux socket is bound once")
	}
	script := args[len(args)-1]
	for _, want := range []string{
		"/usr/bin/socat TCP6-LISTEN:3128,ipv6only=0,fork,reuseaddr,backlog=4096 UNIX-CONNECT:/tmp/g/gocode-http-1.sock",
		"TCP4-LISTEN:1080,fork,reuseaddr,backlog=4096 UNIX-CONNECT:/tmp/g/gocode-http-1.sock",
		"trap 'rc=$?; kill %1 %2 2>/dev/null; exit $rc' EXIT",
		"eval 'curl https://example.com'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bridge script missing %q\n%s", want, script)
		}
	}
}

type tempEmpty struct{ dir string }

func (e tempEmpty) EmptyDir() (string, error) { return e.dir, nil }

func TestPlanLinuxMountsPlaceholders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mount placeholders are not supported on Windows")
	}
	proj := realTempDir(t)
	empty := realTempDir(t)
	settings := filepath.Join(proj, ".gocode", "settings.json")
	skills := filepath.Join(proj, ".gocode", "skills")
	rc := filepath.Join(proj, ".bashrc")
	p := Profile{WriteAllow: []string{proj}, WriteDeny: []string{settings, skills, rc}}
	plan, err := PlanLinuxMounts(p, tempEmpty{empty})
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(plan.Args, "\x00")
	// The missing .gocode directory is blocked as a whole (an ancestor of two
	// denies), .bashrc by a /dev/null file mount.
	if !strings.Contains(j, "--ro-bind\x00"+empty+"\x00"+filepath.Join(proj, ".gocode")) {
		t.Errorf("missing .gocode not blocked: %v", plan.Args)
	}
	if !strings.Contains(j, "--ro-bind\x00/dev/null\x00"+rc) {
		t.Errorf("missing .bashrc not blocked: %v", plan.Args)
	}
	if len(plan.Placeholders) != 2 {
		t.Fatalf("placeholders = %v", plan.Placeholders)
	}
	for _, ph := range plan.Placeholders {
		if _, err := os.Lstat(ph.Path); err != nil {
			t.Errorf("placeholder %s not created: %v", ph.Path, err)
		}
	}
	RemovePlaceholders(plan.Placeholders)
	for _, ph := range plan.Placeholders {
		if _, err := os.Lstat(ph.Path); err == nil {
			t.Errorf("placeholder %s not removed", ph.Path)
		}
	}
	// A path the sandbox did not create is never removed.
	f := filepath.Join(proj, "kept")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	RemovePlaceholders([]Placeholder{{Path: f}})
	if _, err := os.Stat(f); err != nil {
		t.Error("non-empty placeholder must be kept")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain/path-1.2": "plain/path-1.2",
		"a b":            "'a b'",
		"it's":           `'it'"'"'s'`,
		"":               "''",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
