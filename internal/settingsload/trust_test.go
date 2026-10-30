package settingsload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

func mustSettings(t *testing.T, s string) settingspolicy.Settings {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A plain folder's settings (theme, deny rules, plan mode, …) are never
// reported nor touched.
func TestScanUntrustedPlainSettings(t *testing.T) {
	s := mustSettings(t, `{"theme":"light","permissions":{"deny":["Bash(rm:*)"],"defaultMode":"plan"},
		"sandbox":{"enabled":true},"enabledPlugins":{"x@y":false},"disableAllHooks":true,"hooks":{}}`)
	for _, src := range []settingspolicy.Source{settingspolicy.SourceProject, settingspolicy.SourceLocal} {
		if items := ScanUntrusted(src, s); len(items) != 0 {
			t.Fatalf("%s: plain settings reported: %v", src, items)
		}
		if got := StripUntrusted(src, s); len(got) != len(s) {
			t.Fatalf("%s: plain settings stripped: %v", src, got)
		}
	}
}

// Each risky item is listed and withheld; restrictive siblings survive and
// the input is never modified.
func TestScanUntrustedEachItem(t *testing.T) {
	cases := []struct {
		name, json, kind, detail string
		gone                     []string // dotted paths removed by StripUntrusted
		kept                     []string // dotted paths still present
	}{
		{"hooks", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"x"}]}],"PreToolUse":[{"hooks":[{"type":"http","url":"http://127.0.0.1"}]}]}}`, "hooks", "PreToolUse, SessionStart", []string{"hooks"}, nil},
		{"statusLine", `{"statusLine":{"type":"command","command":"echo hi"}}`, "status line command", "echo hi", []string{"statusLine"}, nil},
		{"local mcpServers", `{"mcpServers":{"a":{"command":"x"},"b":{"command":"y"}}}`, "MCP servers", "2 (a, b)", []string{"mcpServers"}, nil},
		{"enableAll", `{"enableAllProjectMcpServers":true}`, "MCP server approvals", "enableAllProjectMcpServers", []string{"enableAllProjectMcpServers"}, nil},
		{"enabledMcpjson", `{"enabledMcpjsonServers":["srv"]}`, "MCP server approvals", "enabledMcpjsonServers [srv]", []string{"enabledMcpjsonServers"}, nil},
		{"allow", `{"permissions":{"allow":["Bash(*)"],"deny":["Read(.env)"]}}`, "permissions", "Bash(*)", []string{"permissions.allow"}, []string{"permissions.deny"}},
		{"ask", `{"permissions":{"ask":["WebFetch"]}}`, "permissions", "WebFetch", []string{"permissions.ask"}, nil},
		{"addDirs", `{"permissions":{"additionalDirectories":["../secrets"]}}`, "additional directories", "../secrets", []string{"permissions.additionalDirectories"}, nil},
		{"acceptEdits", `{"permissions":{"defaultMode":"acceptEdits"}}`, "permission mode", "acceptEdits", []string{"permissions.defaultMode"}, nil},
		{"sandbox off", `{"sandbox":{"enabled":false,"failIfUnavailable":true}}`, "sandbox", "enabled: false", []string{"sandbox.enabled"}, []string{"sandbox.failIfUnavailable"}},
		{"sandbox loosen", `{"sandbox":{"excludedCommands":["docker"],"allowUnsandboxedCommands":true,"filesystem":{"allowWrite":["/"],"denyRead":["~/.ssh"]},"network":{"allowedDomains":["evil.example"]}}}`, "sandbox",
			"allowUnsandboxedCommands, excludedCommands, filesystem.allowWrite, network.allowedDomains",
			[]string{"sandbox.excludedCommands", "sandbox.allowUnsandboxedCommands", "sandbox.filesystem.allowWrite", "sandbox.network.allowedDomains"}, []string{"sandbox.filesystem.denyRead"}},
		{"plugins", `{"enabledPlugins":{"evil@market":true,"off@market":false}}`, "plugins", "evil@market", []string{"enabledPlugins.evil@market"}, []string{"enabledPlugins.off@market"}},
		{"model", `{"model":"attacker/model"}`, "model", "attacker/model", []string{"model"}, nil},
		{"provider", `{"provider":"openai"}`, "provider", "openai", []string{"provider"}, nil},
		{"fallbackModel", `{"fallbackModel":["a","b"]}`, "fallbackModel", "a, b", []string{"fallbackModel"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustSettings(t, c.json)
			before, _ := json.Marshal(s)
			items := ScanUntrusted(settingspolicy.SourceLocal, s)
			if len(items) != 1 || items[0].Kind != c.kind || items[0].Detail != c.detail {
				t.Fatalf("items = %+v, want %s: %s", items, c.kind, c.detail)
			}
			out := StripUntrusted(settingspolicy.SourceLocal, s)
			for _, p := range c.gone {
				if _, ok := settingspolicy.GetPath(out, strings.Split(p, ".")); ok {
					t.Errorf("%s still present: %v", p, out)
				}
			}
			for _, p := range c.kept {
				if _, ok := settingspolicy.GetPath(out, strings.Split(p, ".")); !ok {
					t.Errorf("%s was dropped: %v", p, out)
				}
			}
			if len(ScanUntrusted(settingspolicy.SourceLocal, out)) != 0 {
				t.Errorf("stripped settings still report items")
			}
			after, _ := json.Marshal(s)
			if string(before) != string(after) {
				t.Errorf("input modified: %s -> %s", before, after)
			}
		})
	}
}

// Project-settings mcpServers already need per-server approval, so they are
// not withheld (only local ones are).
func TestScanUntrustedProjectMCPServersLeftGated(t *testing.T) {
	s := mustSettings(t, `{"mcpServers":{"a":{"command":"x"}}}`)
	if items := ScanUntrusted(settingspolicy.SourceProject, s); len(items) != 0 {
		t.Fatalf("project mcpServers reported: %v", items)
	}
}

// EffectiveSources applies the Context's trust state to the project and local
// sources only; user settings are never touched.
func TestEffectiveSourcesUntrustedProject(t *testing.T) {
	fsys, ctx := trustFixture(t,
		`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"u"}]}]},"model":"mine"}`,
		`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"p"}]}]},"theme":"dark"}`,
		`{"permissions":{"allow":["Bash(*)"]},"mcpServers":{"loc":{"command":"x"}},"enableAllProjectMcpServers":true}`)
	ctx.UntrustedProject = true
	got := map[settingspolicy.Source]settingspolicy.Settings{}
	for _, or := range EffectiveSources(fsys, ctx, CanonicalSourceOrder) {
		got[or.Source] = or.Result.Settings
	}
	if _, ok := got[settingspolicy.SourceUser]["hooks"]; !ok || got[settingspolicy.SourceUser]["model"] != "mine" {
		t.Fatalf("user settings touched: %v", got[settingspolicy.SourceUser])
	}
	if _, ok := got[settingspolicy.SourceProject]["hooks"]; ok || got[settingspolicy.SourceProject]["theme"] != "dark" {
		t.Fatalf("project = %v", got[settingspolicy.SourceProject])
	}
	if len(ScanUntrusted(settingspolicy.SourceLocal, got[settingspolicy.SourceLocal])) != 0 {
		t.Fatalf("local = %v", got[settingspolicy.SourceLocal])
	}
	ctx.UntrustedProject = false
	for _, or := range EffectiveSources(fsys, ctx, CanonicalSourceOrder) {
		if or.Source == settingspolicy.SourceLocal && or.Result.Settings["enableAllProjectMcpServers"] != true {
			t.Fatalf("trusted local lost its keys: %v", or.Result.Settings)
		}
	}
}

// A git-tracked settings.local.json is MCP-gated like project settings, even
// when trusted.
func TestEffectiveSourcesTrackedLocal(t *testing.T) {
	fsys, ctx := trustFixture(t, "",
		`{"mcpServers":{"proj":{"command":"p"}}}`,
		`{"mcpServers":{"loc":{"command":"x"}},"enableAllProjectMcpServers":true,"enabledMcpjsonServers":["proj"],"permissions":{"deny":["Read"]}}`)
	ctx.LocalSettingsTracked = true
	for _, or := range EffectiveSources(fsys, ctx, CanonicalSourceOrder) {
		switch or.Source {
		case settingspolicy.SourceLocal:
			for _, k := range []string{"mcpServers", "enableAllProjectMcpServers", "enabledMcpjsonServers"} {
				if _, ok := or.Result.Settings[k]; ok {
					t.Errorf("local still has %s", k)
				}
			}
			if _, ok := or.Result.Settings["permissions"]; !ok {
				t.Errorf("local lost unrelated keys")
			}
		case settingspolicy.SourceProject:
			m, _ := or.Result.Settings["mcpServers"].(map[string]any)
			if m["proj"] == nil || m["loc"] == nil {
				t.Errorf("project mcpServers = %v, want proj+loc", m)
			}
		}
	}
}

// trustFixture writes user / project / local settings ("" = none) under a
// temp home and project and returns a hermetic Context over the real FS.
func trustFixture(t *testing.T, user, project, local string) (fsfacade.FS, *Context) {
	t.Helper()
	root := t.TempDir()
	ctx := &Context{
		HomeDir:                filepath.Join(root, "home"),
		Cwd:                    filepath.Join(root, "proj"),
		ManagedBaseDirOverride: filepath.Join(root, "managed"),
		Env:                    func(string) string { return "" },
		OSPolicyReader:         func() OSPolicyReadout { return OSPolicyReadout{} },
	}
	write := func(path, body string) {
		if body == "" {
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for src, body := range map[settingspolicy.Source]string{settingspolicy.SourceUser: user, settingspolicy.SourceProject: project, settingspolicy.SourceLocal: local} {
		p, ok := ctx.PathForSource(src)
		if !ok {
			t.Fatalf("no path for %s", src)
		}
		write(p, body)
	}
	return fsfacade.OSFS{}, ctx
}
