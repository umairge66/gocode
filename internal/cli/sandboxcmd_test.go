package cli

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/sandbox"
)

// statusManager builds a sandbox manager over in-memory settings layers with
// a fake tool lookup, so the status is computed without touching the host.
func statusManager(goos string, haveTools bool, layers ...sandbox.Layer) *sandbox.Manager {
	look := func(name string) (string, error) {
		if haveTools {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	return sandbox.NewManager(sandbox.ManagerOptions{
		Layers:   layers,
		GOOS:     goos,
		LookPath: look,
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Paths:    sandbox.SessionPaths{Cwd: "/proj", TempDir: "/tmp/gocode-test"},
	})
}

// User namespaces switched off in the kernel are a dependency error.
func TestSandboxStatusUserNamespaces(t *testing.T) {
	user := sandbox.Layer{Source: "userSettings", Settings: map[string]any{"sandbox": map[string]any{"enabled": true}}}
	for file, want := range map[string]string{
		"/proc/sys/user/max_user_namespaces":         "user namespaces are disabled",
		"/proc/sys/kernel/unprivileged_userns_clone": "unprivileged user namespaces are disabled",
	} {
		m := sandbox.NewManager(sandbox.ManagerOptions{
			Layers: []sandbox.Layer{user}, GOOS: "linux",
			LookPath: func(n string) (string, error) { return "/nonexistent/" + n, nil },
			ReadFile: func(p string) ([]byte, error) {
				if p == file {
					return []byte("0\n"), nil
				}
				return nil, os.ErrNotExist
			},
			Paths: sandbox.SessionPaths{Cwd: "/proj", TempDir: "/tmp/gocode-test"},
		})
		st := computeSandboxStatus(m, "linux")
		if st.Installed || st.Enabled || len(st.Reasons) != 1 || !strings.Contains(st.Reasons[0], want) {
			t.Errorf("%s: %+v", file, st)
		}
	}
}

func TestSandboxStatusFromSettings(t *testing.T) {
	user := sandbox.Layer{Source: "userSettings", Settings: map[string]any{"sandbox": map[string]any{
		"enabled": true, "allowUnsandboxedCommands": false, "filesystem": map[string]any{"disabled": true}, "autoAllowBashIfSandboxed": false,
	}}}

	// Linux with bwrap + socat present: the sandbox really runs, and the
	// available/installed/reasons trio reports the live dependency probe.
	st := computeSandboxStatus(statusManager("linux", true, user), "linux")
	if st.StatusVersion != 3 || !st.Available || !st.Installed || len(st.Reasons) != 0 {
		t.Fatalf("install fields = %+v", st)
	}
	if !st.Supported || !st.Enabled || st.EnabledSource != "settings" || st.UnavailableReason != nil {
		t.Fatalf("enabled fields = %+v", st)
	}
	if !st.StrictMode || st.StrictModeSource != "settings" || st.FilesystemPolicy != "relaxed" {
		t.Fatalf("strict/fs = %+v", st)
	}
	if st.AutoAllowBashIfSandboxed || st.AutoAllowBashIfSandboxedSource != "settings" {
		t.Fatalf("auto-allow = %+v", st)
	}

	// Linux without the tools: enabled in settings, not running, with the
	// dependency reason.
	miss := computeSandboxStatus(statusManager("linux", false, user), "linux")
	if miss.Installed || !miss.Available || len(miss.Reasons) != 2 || miss.Reasons[0] != "bubblewrap (bwrap) not installed" {
		t.Fatalf("missing deps install fields = %+v", miss)
	}
	if miss.Enabled || miss.UnavailableReason == nil ||
		!strings.Contains(*miss.UnavailableReason, "dependencies are missing: bubblewrap (bwrap) not installed, socat not installed") {
		t.Fatalf("missing deps = %+v (reason %v)", miss, deref(miss.UnavailableReason))
	}

	win := computeSandboxStatus(statusManager("windows", true), "windows")
	if win.AutoAllowBashIfSandboxedSource != "unsupported" || win.AutoAllowBashIfSandboxed || win.FilesystemPolicy != "strict" || win.UnavailableReason != nil {
		t.Fatalf("windows = %+v", win)
	}
	if win.Reasons[0] != "The Windows sandbox is not enabled on this build." {
		t.Fatalf("windows gate = %q", win.Reasons[0])
	}
	winOn := computeSandboxStatus(statusManager("windows", true, user), "windows")
	if winOn.Enabled || winOn.UnavailableReason == nil || *winOn.UnavailableReason != "sandbox is enabled but windows is not supported (requires macOS, Linux, or WSL2)" {
		t.Fatalf("windows enabled = %+v (reason %v)", winOn, deref(winOn.UnavailableReason))
	}

	policy := sandbox.Layer{Source: "policySettings", Settings: map[string]any{"sandbox": map[string]any{"enabled": true, "enabledPlatforms": []any{"macos"}}}}
	pol := computeSandboxStatus(statusManager("linux", true, policy), "linux")
	if pol.Supported || pol.Enabled || pol.EnabledSource != "policy" || pol.UnavailableReason != nil {
		t.Fatalf("policy-unlisted = %+v", pol)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestSandboxCommands(t *testing.T) {
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.userSettingsPath(), `{"sandbox":{"enabled":true}}`)
	code, out, _ := f.run(t, "sandbox", "status")
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status not one JSON line: %q", out)
	}
	if st["enabledSource"] != "settings" || st["statusVersion"].(float64) != 3 {
		t.Fatalf("status = %v", st)
	}
	code, out, _ = f.run(t, "sandbox", "install")
	if code != 1 || !strings.HasPrefix(out, `{"status":"unavailable","message":`) {
		t.Fatalf("install exit %d out %q", code, out)
	}
	if runtime.GOOS != "windows" && !strings.Contains(out, "only available on native Windows") {
		t.Fatalf("install message %q", out)
	}
}
