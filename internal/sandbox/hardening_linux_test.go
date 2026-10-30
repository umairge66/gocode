//go:build linux

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newHardeningEnv is newSandboxEnv with a hook to adjust the manager options
// before the manager is built.
func newHardeningEnv(t *testing.T, sandboxSettings map[string]any, adjust func(*ManagerOptions)) *sandboxEnv {
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
	settings := map[string]any{"enabled": true}
	for k, v := range sandboxSettings {
		settings[k] = v
	}
	opts := ManagerOptions{
		Layers: []Layer{{Source: "userSettings", Settings: map[string]any{"sandbox": settings}, BaseDir: e.config}},
		Paths: SessionPaths{
			Cwd: e.proj, Home: e.home, ConfigDir: e.config, TempDir: filepath.Join(root, "tmp"),
			SettingsFiles: []string{filepath.Join(e.config, "settings.json")},
		},
	}
	if adjust != nil {
		adjust(&opts)
	}
	e.m = NewManager(opts)
	t.Cleanup(func() { _ = e.m.Close() })
	if !e.m.IsSandboxingEnabled() {
		t.Fatalf("sandbox not enabled: %s", e.m.UnavailableReason())
	}
	if err := e.m.Start(); err != nil {
		t.Fatal(err)
	}
	return e
}

// Provider keys and other credential variables never reach a sandboxed
// command; passThroughEnvVars (user settings) re-admits a named one; a
// repository cannot.
func TestLinuxSandboxStripsSecretEnv(t *testing.T) {
	fake := "fake-test-value-not-a-key"
	secrets := []string{"OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"OPENAI_ORG_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "MCP_CLIENT_SECRET", "AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN", "AWS_ACCESS_KEY_ID", "NPM_TOKEN", "MY_SERVICE_PASSWORD", "GOCODE_API_KEY", "PASS_ME_TOKEN"}
	for _, n := range secrets {
		t.Setenv(n, fake)
	}
	t.Setenv("HARMLESS_SETTING", "visible")
	e := newHardeningEnv(t, map[string]any{"credentials": map[string]any{"passThroughEnvVars": []any{"PASS_ME_TOKEN"}}}, nil)
	out, code := e.run(t, "env")
	if code != 0 {
		t.Fatalf("env failed: %d %s", code, out)
	}
	for _, n := range secrets {
		has := strings.Contains(out, n+"="+fake)
		if n == "PASS_ME_TOKEN" {
			if !has {
				t.Errorf("passThroughEnvVars %s not passed through", n)
			}
			continue
		}
		if has {
			t.Errorf("%s visible inside the sandbox", n)
		}
	}
	if !strings.Contains(out, "HARMLESS_SETTING=visible") {
		t.Error("ordinary variables must stay")
	}

	// A repository's passThroughEnvVars is ignored.
	repo := ParseSettings([]Layer{{Source: srcProject, Settings: map[string]any{"sandbox": map[string]any{
		"credentials": map[string]any{"passThroughEnvVars": []any{"OPENROUTER_API_KEY"}}}}}}, PathContext{})
	if len(repo.CredentialEnvAllow) != 0 {
		t.Errorf("project settings passed a key through: %v", repo.CredentialEnvAllow)
	}
	// An explicit envVars deny wins over a pass-through.
	both := ParseSettings([]Layer{{Source: srcUser, Settings: map[string]any{"sandbox": map[string]any{
		"credentials": map[string]any{"passThroughEnvVars": []any{"X_TOKEN"}, "envVars": []any{map[string]any{"name": "X_TOKEN", "mode": "deny"}}}}}}}, PathContext{})
	if got := mergeSecretUnset(both.CredentialEnvDeny, []string{"X_TOKEN=1"}, both.CredentialEnvAllow); !hasExactPath(got, "X_TOKEN") {
		t.Errorf("explicit deny lost to pass-through: %v", got)
	}
}

// Writes land only under the writable roots; a symlink inside a writable root
// does not make its outside target writable.
func TestLinuxSandboxSymlinkCannotEscapeWritableRoot(t *testing.T) {
	e := newHardeningEnv(t, nil, nil)
	outDir := filepath.Join(e.home, "outdir")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(e.home, "outfile")
	if err := os.WriteFile(outFile, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"dirlink": outDir, "filelink": outFile, "dangling": filepath.Join(e.home, "newfile")} {
		if err := os.Symlink(target, filepath.Join(e.proj, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []string{"echo x > dirlink/new", "echo x > filelink", "echo x >> filelink", "echo x > dangling",
		"cp /etc/hostname dirlink/", "ln -s /etc/passwd dirlink/l", "mkdir dirlink/d", "touch " + filepath.Join(e.home, "direct")} {
		if _, code := e.run(t, cmd); code == 0 {
			t.Errorf("%q succeeded", cmd)
		}
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("outside dir modified: %v", entries)
	}
	if b, _ := os.ReadFile(outFile); string(b) != "orig" {
		t.Errorf("outside file modified: %q", b)
	}
	for _, p := range []string{filepath.Join(e.home, "newfile"), filepath.Join(e.home, "direct")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s created", p)
		}
	}
	// The project itself stays writable through a symlinked subdirectory.
	if err := os.Mkdir(filepath.Join(e.proj, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.proj, "real"), filepath.Join(e.proj, "inner")); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run(t, "echo ok > inner/f && cat real/f"); code != 0 || !strings.Contains(out, "ok") {
		t.Errorf("in-root symlink write failed: %d %q", code, out)
	}
}

// Protected paths stay read-only: existing or not, nested deeper than a
// shallow scan depth, a repository created by the command, the config
// home and shell rc files when the home directory is a writable root.
func TestLinuxSandboxProtectedPaths(t *testing.T) {
	var e *sandboxEnv
	e = newHardeningEnv(t, nil, func(o *ManagerOptions) {
		o.Paths.AddDirs = []string{o.Paths.Home} // the home dir is writable too
	})
	deep := filepath.Join(e.proj, "a", "b", "c", "d", "e", "f")
	if err := os.MkdirAll(filepath.Join(deep, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mods := filepath.Join(e.proj, "sub", ".git", "modules", "lib", "hooks")
	if err := os.MkdirAll(mods, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"git init -q . ",
		"mkdir -p .git/hooks && echo evil > .git/hooks/post-checkout",
		"echo evil > " + filepath.Join(deep, ".git", "hooks", "pre-commit"),
		"echo '[core] hooksPath=/x' >> " + filepath.Join(deep, ".git", "config"),
		"echo evil > " + filepath.Join(mods, "post-checkout"),
		"mkdir -p .gocode && echo '{}' > .gocode/settings.local.json",
		"echo '{}' > .mcp.json",
		"echo evil > .zshenv",
		"mkdir -p .vscode && echo '{}' > .vscode/tasks.json",
		"echo evil >> " + filepath.Join(e.home, ".bashrc"),
		"echo evil > " + filepath.Join(e.home, ".zshenv"),
		"echo evil > " + filepath.Join(e.home, ".ssh", "authorized_keys"),
		"echo evil > " + filepath.Join(e.config, "anything.json"),
		"mkdir -p " + filepath.Join(e.config, "hooks"),
	} {
		if out, code := e.run(t, cmd); code == 0 {
			t.Errorf("%q succeeded (%s)", cmd, out)
		}
	}
	for _, p := range []string{
		filepath.Join(e.proj, ".git"), filepath.Join(deep, ".git", "hooks", "pre-commit"),
		filepath.Join(mods, "post-checkout"), filepath.Join(e.proj, ".zshenv"), filepath.Join(e.home, ".bashrc"),
		filepath.Join(e.config, "anything.json"),
	} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists after the commands", p)
		}
	}
	// Ordinary writes in both roots still work.
	if out, code := e.run(t, "echo ok > notes.txt && echo ok > "+filepath.Join(e.home, "notes.txt")+" && echo done"); code != 0 || !strings.Contains(out, "done") {
		t.Errorf("ordinary writes failed: %d %q", code, out)
	}
}

// A protected path reached through a symlink inside a writable root is
// refused (the link itself could be replaced).
func TestLinuxSandboxRefusesSymlinkedProtectedPath(t *testing.T) {
	e := newHardeningEnv(t, nil, nil)
	if err := os.WriteFile(filepath.Join(e.home, "mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.home, "mcp.json"), filepath.Join(e.proj, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.Wrap(context.Background(), "true", "/bin/sh", e.proj)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Wrap with a symlinked .mcp.json: %v", err)
	}
	// Nothing was left behind by the refused plan.
	for _, p := range []string{".bashrc", ".gocode", ".git"} {
		if _, err := os.Lstat(filepath.Join(e.proj, p)); err == nil {
			t.Errorf("placeholder %s left after a refused wrap", p)
		}
	}
}

func listenUnix(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("HOST-IPC-REACHED\n"))
			_ = c.Close()
		}
	}()
}

// Host IPC sockets are not reachable from inside: the runtime dir is masked,
// a socket named by SSH_AUTH_SOCK is masked and the variable unset; an
// allowUnixSockets entry stays reachable.
func TestLinuxSandboxHostIPCUnreachable(t *testing.T) {
	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat not installed")
	}
	// Short socket paths (sun_path limit): a private dir under /tmp.
	base, err := os.MkdirTemp("/tmp", "gsbx-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	runtimeDir := filepath.Join(base, "run")
	agentDir := filepath.Join(base, "ag")
	allowedDir := filepath.Join(base, "ok")
	for _, d := range []string{runtimeDir, agentDir, allowedDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	busSock := filepath.Join(runtimeDir, "bus")
	agentSock := filepath.Join(agentDir, "agent.1")
	allowedSock := filepath.Join(allowedDir, "svc.sock")
	laterSock := filepath.Join(runtimeDir, "later.sock")
	for _, s := range []string{busSock, agentSock, allowedSock} {
		listenUnix(t, s)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("SSH_AUTH_SOCK", agentSock)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+busSock)
	e := newHardeningEnv(t, map[string]any{"network": map[string]any{"allowUnixSockets": []any{allowedSock}}}, nil)
	listenUnix(t, laterSock) // created after the session started: still hidden

	probe := func(p string) string {
		out, _ := e.run(t, "socat -T2 - UNIX-CONNECT:"+p+" </dev/null 2>&1; true")
		return out
	}
	for _, p := range []string{busSock, agentSock, laterSock} {
		if out := probe(p); strings.Contains(out, "HOST-IPC-REACHED") {
			t.Errorf("host socket %s reachable from the sandbox", p)
		}
	}
	if out := probe(allowedSock); !strings.Contains(out, "HOST-IPC-REACHED") {
		t.Errorf("allowUnixSockets entry not reachable: %q", out)
	}
	out, _ := e.run(t, `echo "[$SSH_AUTH_SOCK][$DBUS_SESSION_BUS_ADDRESS]"`)
	if strings.TrimSpace(out) != "[][]" {
		t.Errorf("socket env vars visible: %q", out)
	}
	// The proxy bridge still works (the one socket bound in).
	out, _ = e.run(t, `exec 3<>/dev/tcp/127.0.0.1/3128 && printf 'GET http://example.invalid/ HTTP/1.0\r\n\r\n' >&3 && timeout 5 cat <&3`)
	if !strings.Contains(out, "403") {
		t.Errorf("proxy bridge unreachable: %q", out)
	}

	// allowAllUnixSockets turns masking off.
	e2 := newHardeningEnv(t, map[string]any{"network": map[string]any{"allowAllUnixSockets": true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w, err := e2.m.Wrap(ctx, "socat -T2 - UNIX-CONNECT:"+busSock+" </dev/null", "/bin/sh", e2.proj)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Release()
	b, _ := exec.CommandContext(ctx, w.Argv[0], w.Argv[1:]...).CombinedOutput()
	if !strings.Contains(string(b), "HOST-IPC-REACHED") {
		t.Errorf("allowAllUnixSockets: %q", b)
	}
}
