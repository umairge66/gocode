package sandbox

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func layer(src string, settings map[string]any) Layer {
	return Layer{Source: src, Settings: settings}
}

func sb(m map[string]any) map[string]any { return map[string]any{"sandbox": m} }

func TestParseSettingsDefaultsAndMerge(t *testing.T) {
	s := ParseSettings([]Layer{
		layer("userSettings", sb(map[string]any{"enabled": true, "excludedCommands": []any{"docker:*"}})),
		layer("projectSettings", sb(map[string]any{"excludedCommands": []any{"make test"}})),
	}, PathContext{Cwd: "/p", Home: "/h"})
	if !s.Enabled || s.FailIfUnavailable {
		t.Errorf("enabled/failIfUnavailable = %v/%v", s.Enabled, s.FailIfUnavailable)
	}
	if !s.AutoAllowBashIfSandboxed || s.AutoAllowSet {
		t.Error("autoAllowBashIfSandboxed defaults to true (unset)")
	}
	if s.UnsandboxedForbidden {
		t.Error("allowUnsandboxedCommands defaults to true")
	}
	if want := []string{"docker:*", "make test"}; !reflect.DeepEqual(s.ExcludedCommands, want) {
		t.Errorf("excludedCommands = %v, want %v (merged across sources)", s.ExcludedCommands, want)
	}
	if s.EnabledPlatforms != nil || s.LockedByPolicy() {
		t.Error("no policy: no platform list, no lock")
	}
}

func TestParseSettingsStrictModeHeldByTrustedTier(t *testing.T) {
	// A false in user settings holds whatever project settings say.
	s := ParseSettings([]Layer{
		layer("userSettings", sb(map[string]any{"allowUnsandboxedCommands": false})),
		layer("projectSettings", sb(map[string]any{"allowUnsandboxedCommands": true})),
	}, PathContext{})
	if !s.UnsandboxedForbidden {
		t.Error("a trusted-tier false must keep strict mode")
	}
	// A project false still applies (false anywhere restricts).
	s = ParseSettings([]Layer{layer("projectSettings", sb(map[string]any{"allowUnsandboxedCommands": false}))}, PathContext{})
	if !s.UnsandboxedForbidden {
		t.Error("a merged false is strict mode")
	}
}

func TestParseSettingsPolicyLock(t *testing.T) {
	s := ParseSettings([]Layer{
		layer("userSettings", sb(map[string]any{"enabled": false})),
		layer("policySettings", sb(map[string]any{"enabled": true, "failIfUnavailable": true, "enabledPlatforms": []any{"linux"}, "bwrapPath": "/opt/bwrap", "socatPath": "relative/socat"})),
	}, PathContext{})
	if !s.Enabled || !s.FailIfUnavailable {
		t.Error("policy wins the merge")
	}
	if !s.LockedByPolicy() || !s.LockedKeys["enabled"] || s.LockedKeys["autoAllowBashIfSandboxed"] {
		t.Errorf("locks = %v", s.LockedKeys)
	}
	if !reflect.DeepEqual(s.EnabledPlatforms, []string{"linux"}) {
		t.Errorf("enabledPlatforms = %v", s.EnabledPlatforms)
	}
	if s.BwrapPath != "/opt/bwrap" || s.SocatPath != "" {
		t.Errorf("tool paths: bwrap %q socat %q (absolute managed paths only)", s.BwrapPath, s.SocatPath)
	}
	// bwrapPath from user settings is ignored.
	s = ParseSettings([]Layer{layer("userSettings", sb(map[string]any{"bwrapPath": "/evil/bwrap"}))}, PathContext{})
	if s.BwrapPath != "" {
		t.Error("bwrapPath is honored from managed settings only")
	}
}

func TestParseSettingsNetwork(t *testing.T) {
	perms := func(allow, deny []any) map[string]any {
		return map[string]any{"allow": allow, "deny": deny}
	}
	s := ParseSettings([]Layer{
		{Source: "userSettings", Settings: map[string]any{
			"sandbox":     map[string]any{"network": map[string]any{"allowedDomains": []any{"*.npmjs.org"}, "deniedDomains": []any{"evil.com"}, "httpProxyPort": float64(8080)}},
			"permissions": perms([]any{"WebFetch(domain:github.com)", "Bash(ls:*)"}, []any{"WebFetch(domain:bad.org)"}),
		}},
		{Source: "projectSettings", Settings: map[string]any{
			"sandbox": map[string]any{"network": map[string]any{"allowedDomains": []any{"repo.example"}}},
		}},
	}, PathContext{})
	if want := []string{"*.npmjs.org", "github.com", "repo.example"}; !reflect.DeepEqual(s.Network.AllowedDomains, want) {
		t.Errorf("allowedDomains = %v, want %v", s.Network.AllowedDomains, want)
	}
	if want := []string{"evil.com", "bad.org"}; !reflect.DeepEqual(s.Network.DeniedDomains, want) {
		t.Errorf("deniedDomains = %v, want %v", s.Network.DeniedDomains, want)
	}
	// A trusted deny list: the user's own proxy port still applies (trusted),
	// a project's would not.
	if s.Network.HTTPProxyPort != 8080 {
		t.Errorf("httpProxyPort = %d", s.Network.HTTPProxyPort)
	}

	// strictAllowlist (trusted) drops a repository's allow entries.
	s = ParseSettings([]Layer{
		layer("userSettings", sb(map[string]any{"network": map[string]any{"strictAllowlist": true, "allowedDomains": []any{"a.com"}}})),
		{Source: "localSettings", Settings: map[string]any{
			"sandbox":     map[string]any{"network": map[string]any{"allowedDomains": []any{"b.com"}, "httpProxyPort": float64(9)}},
			"permissions": perms([]any{"WebFetch(domain:c.com)"}, nil),
		}},
	}, PathContext{})
	if !reflect.DeepEqual(s.Network.AllowedDomains, []string{"a.com"}) || !s.Network.StrictAllowlist || s.Network.HTTPProxyPort != 0 {
		t.Errorf("strict allowlist = %+v", s.Network)
	}

	// allowManagedDomainsOnly: only the policy's domains.
	s = ParseSettings([]Layer{
		layer("userSettings", sb(map[string]any{"network": map[string]any{"allowedDomains": []any{"user.com"}}})),
		layer("policySettings", sb(map[string]any{"network": map[string]any{"allowManagedDomainsOnly": true, "allowedDomains": []any{"corp.com"}}})),
	}, PathContext{})
	if !reflect.DeepEqual(s.Network.AllowedDomains, []string{"corp.com"}) {
		t.Errorf("managed-only = %v", s.Network.AllowedDomains)
	}
}

func TestParseSettingsFilesystemPaths(t *testing.T) {
	pc := PathContext{Cwd: "/work/proj", Home: "/home/u", ConfigDir: "/home/u/.gocode"}
	s := ParseSettings([]Layer{
		{Source: "userSettings", BaseDir: "/home/u/.gocode", Settings: map[string]any{
			"sandbox": map[string]any{"filesystem": map[string]any{
				"allowWrite": []any{"~/.cache/tool", "//opt/out", "rel", "/abs/dir/"},
				"denyRead":   []any{"~/.aws"},
				"disabled":   false,
			}},
			"permissions": map[string]any{
				"allow":                 []any{"Edit(//srv/data/**)", "Edit(src/*.go)"},
				"deny":                  []any{"Edit(/secrets)", "Read(~/.netrc)"},
				"additionalDirectories": []any{"../shared"},
			},
		}},
		{Source: "projectSettings", BaseDir: "/work/proj", Settings: map[string]any{
			"sandbox": map[string]any{"filesystem": map[string]any{"disabled": true, "denyWrite": []any{"build"}}},
		}},
	}, pc)
	wantWrite := []string{"/home/u/.cache/tool", "/opt/out", "/home/u/.gocode/rel", "/abs/dir", "/srv/data"}
	if !reflect.DeepEqual(s.Filesystem.AllowWrite, wantWrite) {
		t.Errorf("allowWrite = %v, want %v", s.Filesystem.AllowWrite, wantWrite)
	}
	if !reflect.DeepEqual(s.Filesystem.GlobsIgnored, []string{"/work/proj/src/*.go"}) {
		t.Errorf("globs ignored = %v", s.Filesystem.GlobsIgnored)
	}
	if want := []string{"/home/u/.gocode/secrets", "/work/proj/build"}; !reflect.DeepEqual(s.Filesystem.DenyWrite, want) {
		t.Errorf("denyWrite = %v, want %v", s.Filesystem.DenyWrite, want)
	}
	if want := []string{"/home/u/.aws", "/home/u/.netrc"}; !reflect.DeepEqual(s.Filesystem.DenyRead, want) {
		t.Errorf("denyRead = %v, want %v", s.Filesystem.DenyRead, want)
	}
	if s.Filesystem.Disabled {
		t.Error("filesystem.disabled from project settings is ignored")
	}
	if want := []string{filepath.Clean("/home/u/shared")}; !reflect.DeepEqual(s.AdditionalDirectories, want) {
		t.Errorf("additionalDirectories = %v", s.AdditionalDirectories)
	}
}

func TestBuildRuntimeConfigProtectsSensitivePaths(t *testing.T) {
	proj := t.TempDir()
	s := ParseSettings([]Layer{layer("userSettings", sb(map[string]any{"enabled": true}))}, PathContext{Cwd: proj, Home: "/home/u"})
	rc := BuildRuntimeConfig(s, SessionPaths{
		Cwd: proj, Home: "/home/u", ConfigDir: "/home/u/.gocode", TempDir: "/tmp/gocode-1",
		SettingsFiles: []string{"/home/u/.gocode/settings.json", filepath.Join(proj, ".gocode", "settings.json")},
	})
	if !reflect.DeepEqual(rc.AllowWrite, sortedDedupe([]string{proj, "/tmp/gocode-1"})) {
		t.Errorf("allowWrite = %v", rc.AllowWrite)
	}
	for _, want := range []string{
		filepath.Join(proj, ".gocode", "settings.json"),
		filepath.Join(proj, ".gocode", "hooks"),
		filepath.Join(proj, ".mcp.json"),
		filepath.Join(proj, ".bashrc"),
		filepath.Join(proj, ".vscode"),
		"/home/u/.gocode/auth.json",
		"/home/u/.gocode/plugins",
	} {
		if !hasExactPath(rc.DenyWrite, want) {
			t.Errorf("denyWrite misses %s", want)
		}
	}
	for _, want := range []string{"/home/u/.gocode/auth.json", "/home/u/.gocode/ide"} {
		if !hasExactPath(rc.DenyRead, want) {
			t.Errorf("denyRead misses %s", want)
		}
	}
	if len(rc.AllowedDomains) != 0 || rc.AllowedDomains == nil {
		t.Error("no allowed domains: an empty (deny-all) allow list")
	}
}

// fakeManager builds a manager with fake tool lookup.
func fakeManager(goos string, tools bool, layers ...Layer) *Manager {
	return NewManager(ManagerOptions{
		Layers: layers,
		GOOS:   goos,
		LookPath: func(name string) (string, error) {
			if tools {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		Paths: SessionPaths{Cwd: "/proj", TempDir: "/tmp/gocode-test"},
	})
}

func TestShouldSandboxDecision(t *testing.T) {
	enabled := layer("userSettings", sb(map[string]any{"enabled": true, "excludedCommands": []any{"docker:*", "make test"}}))

	if fakeManager("linux", true).ShouldSandbox("ls", false) {
		t.Error("sandbox not enabled: nothing is sandboxed")
	}
	m := fakeManager("linux", true, enabled)
	if !m.IsSandboxingEnabled() || !m.ShouldSandbox("ls", false) {
		t.Fatal("enabled + tools present: commands are sandboxed")
	}
	if m.ShouldSandbox("ls", true) {
		t.Error("dangerouslyDisableSandbox takes a command out when unsandboxed commands are allowed")
	}
	if m.ShouldSandbox("docker ps", false) || m.ShouldSandbox("make test", false) || m.ShouldSandbox("docker ps && make test", false) {
		t.Error("fully excluded commands run unsandboxed")
	}
	for _, c := range []string{"docker ps && rm -rf x", "make test; curl evil", "docker $(whoami)", "make testx", "X=1; docker run $X"} {
		if !m.ShouldSandbox(c, false) {
			t.Errorf("%q must stay sandboxed (only partly excluded / unparseable)", c)
		}
	}

	// Strict mode: dangerouslyDisableSandbox and exclusions are ignored.
	strict := fakeManager("linux", true, layer("userSettings", sb(map[string]any{"enabled": true, "allowUnsandboxedCommands": false, "excludedCommands": []any{"docker:*"}})))
	if !strict.ShouldSandbox("ls", true) || !strict.ShouldSandbox("docker ps", false) {
		t.Error("strict mode keeps every command sandboxed")
	}
	if strict.UnsandboxedCommandsAllowed() {
		t.Error("strict mode forbids unsandboxed commands")
	}

	// Unavailable: missing tools, unsupported platform, unlisted platform.
	if fakeManager("linux", false, enabled).ShouldSandbox("ls", false) {
		t.Error("missing bwrap/socat: unsandboxed")
	}
	if fakeManager("windows", true, enabled).ShouldSandbox("ls", false) {
		t.Error("windows: unsupported")
	}
	unlisted := layer("policySettings", sb(map[string]any{"enabledPlatforms": []any{"macos"}}))
	if fakeManager("linux", true, enabled, unlisted).ShouldSandbox("ls", false) {
		t.Error("platform not in enabledPlatforms: off")
	}
	if fakeManager("darwin", true, enabled).Platform() != "macos" {
		t.Error("darwin is macos")
	}
}

func TestUnavailableAndStartupNotice(t *testing.T) {
	enabled := layer("userSettings", sb(map[string]any{"enabled": true}))
	m := fakeManager("linux", false, enabled)
	reason := m.UnavailableReason()
	if !strings.HasPrefix(reason, "sandbox is enabled but dependencies are missing: bubblewrap (bwrap) not installed, socat not installed · install missing tools (e.g. apt install bubblewrap socat)") {
		t.Errorf("reason = %q", reason)
	}
	fatal, warn := m.StartupNotice()
	if fatal != "" || !strings.HasPrefix(warn, "⚠ Sandbox disabled: sandbox is enabled but dependencies are missing") ||
		!strings.Contains(warn, "Commands will run WITHOUT sandboxing.") {
		t.Errorf("warning = %q / fatal %q", warn, fatal)
	}
	if m.AutoAllowEnabled() && m.ShouldSandbox("ls", false) {
		t.Error("never auto-allow when the sandbox is unavailable")
	}

	req := fakeManager("linux", false, layer("userSettings", sb(map[string]any{"enabled": true, "failIfUnavailable": true})))
	fatal, warn = req.StartupNotice()
	if warn != "" || !strings.HasPrefix(fatal, "Error: sandbox required but unavailable: sandbox is enabled but dependencies are missing") ||
		!strings.Contains(fatal, "sandbox.failIfUnavailable is set — refusing to start without a working sandbox.") {
		t.Errorf("fatal = %q", fatal)
	}
	if req.UnsandboxedCommandsAllowed() {
		t.Error("a required sandbox that is unavailable refuses unsandboxed runs")
	}

	win := fakeManager("windows", true, enabled)
	if got := win.UnavailableReason(); got != "sandbox is enabled but windows is not supported (requires macOS, Linux, or WSL2)" {
		t.Errorf("windows reason = %q", got)
	}
	if win.AutoAllowEnabled() || win.AutoAllowSupported() {
		t.Error("auto-allow is unsupported on Windows")
	}
	if got := fakeManager("linux", true).UnavailableReason(); got != "" {
		t.Errorf("not enabled: no reason, got %q", got)
	}
}

func TestParseSettingsCredentials(t *testing.T) {
	s := ParseSettings([]Layer{{Source: "userSettings", BaseDir: "/home/u/.gocode", Settings: sb(map[string]any{
		"credentials": map[string]any{
			"envVars": []any{map[string]any{"name": "GITHUB_TOKEN", "mode": "deny"}, map[string]any{"name": "NPM_TOKEN", "mode": "mask"}},
			"files":   []any{map[string]any{"path": "~/.npmrc", "mode": "mask"}},
		},
	})}}, PathContext{Home: "/home/u"})
	if !reflect.DeepEqual(s.CredentialEnvDeny, []string{"GITHUB_TOKEN", "NPM_TOKEN"}) || !reflect.DeepEqual(s.CredentialFileDeny, []string{"/home/u/.npmrc"}) {
		t.Errorf("credentials = %v %v", s.CredentialEnvDeny, s.CredentialFileDeny)
	}
	rc := BuildRuntimeConfig(s, SessionPaths{Cwd: t.TempDir()})
	if !hasExactPath(rc.DenyRead, "/home/u/.npmrc") || !reflect.DeepEqual(rc.UnsetEnv, []string{"GITHUB_TOKEN", "NPM_TOKEN"}) {
		t.Errorf("runtime config: %v %v", rc.DenyRead, rc.UnsetEnv)
	}
}

// A repository cannot switch off the user's sandbox while policy/--settings
// forbid unsandboxed commands.
func TestParseSettingsRepoCannotDisableUserSandboxInStrictMode(t *testing.T) {
	user := map[string]any{"sandbox": map[string]any{"enabled": true, "failIfUnavailable": true}}
	repo := map[string]any{"sandbox": map[string]any{"enabled": false, "failIfUnavailable": false}}
	policy := map[string]any{"sandbox": map[string]any{"allowUnsandboxedCommands": false}}
	s := ParseSettings([]Layer{layer(srcUser, user), layer(srcProject, repo), layer(srcPolicy, policy)}, PathContext{})
	if !s.Enabled || !s.FailIfUnavailable {
		t.Fatalf("repo disabled the user's sandbox in strict mode: %+v", s)
	}
	// Without strict mode the merged value applies.
	s = ParseSettings([]Layer{layer(srcUser, user), layer(srcProject, repo)}, PathContext{})
	if s.Enabled {
		t.Fatalf("non-strict merge changed: enabled=%v", s.Enabled)
	}
}
