package mcphost

import (
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

func src(s settingspolicy.Source, settings map[string]any) settingsload.OrderedResult {
	return settingsload.OrderedResult{Source: s, Result: settingspolicy.LoadResult{Settings: settings}}
}

func noLookup(string) (string, bool) { return "", false }

func skippedReason(g Gathered, name string) string {
	for _, s := range g.Skipped {
		if s.Name == name {
			return s.Reason
		}
	}
	return ""
}

func TestNaming(t *testing.T) {
	cases := map[[2]string]string{
		{"github", "create_issue"}: "mcp__github__create_issue",
		{"my server", "do.it"}:     "mcp__my_server__do_it",
		{"plugin:demo:db", "q-1"}:  "mcp__plugin_demo_db__q-1",
		{"ünï", "tool/with:chars"}: "mcp___n___tool_with_chars",
		{"a__b", "c"}:              "mcp__a__b__c",
	}
	for in, want := range cases {
		if got := ToolName(in[0], in[1]); got != want {
			t.Errorf("ToolName(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	if NormalizeName("ünï") != "_n_" {
		// each non-ASCII rune becomes one "_"
		t.Errorf("NormalizeName(ünï) = %q", NormalizeName("ünï"))
	}
	if !sameServerName("my server", "my_server") || sameServerName("plugin:a:b", "plugin_a_b") {
		t.Error("sameServerName")
	}
}

func TestGatherPrecedence(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/repo/.mcp.json", []byte(`{"mcpServers":{"shared":{"command":"project"},"proj":{"command":"p","args":["${HOME_X:-dflt}"]}}}`))
	g := Gather(Inputs{
		Settings: []settingsload.OrderedResult{
			src(settingspolicy.SourceUser, map[string]any{
				"enableAllProjectMcpServers": true,
				"mcpServers":                 map[string]any{"shared": map[string]any{"command": "user-settings"}, "useronly": map[string]any{"command": "u"}},
			}),
		},
		User:       map[string]mcp.ServerConfig{"shared": {Command: "user-global"}},
		Local:      map[string]mcp.ServerConfig{"shared": {Command: "local"}, "loc": {Command: "l"}},
		Managed:    map[string]mcp.ServerConfig{"mgd": {Command: "m"}},
		Dynamic:    map[string]mcp.ServerConfig{"dyn": {Command: "d"}},
		FS:         m,
		ProjectDir: "/repo",
		LookupEnv:  noLookup,
	})
	if len(g.Errors) > 0 {
		t.Fatal(g.Errors)
	}
	want := map[string]string{"shared": "local", "useronly": "u", "proj": "p", "loc": "l", "mgd": "m", "dyn": "d"}
	if len(g.Servers) != len(want) {
		t.Fatalf("servers = %+v", g.Servers)
	}
	for n, c := range want {
		if g.Servers[n].Command != c {
			t.Errorf("%s = %+v, want command %q", n, g.Servers[n], c)
		}
	}
	if g.Scopes["shared"] != ScopeLocal || g.Scopes["proj"] != ScopeProject || g.Scopes["mgd"] != ScopeManaged || g.Scopes["dyn"] != ScopeDynamic {
		t.Errorf("scopes = %+v", g.Scopes)
	}
	if g.Servers["proj"].Args[0] != "dflt" {
		t.Errorf("env default not expanded: %+v", g.Servers["proj"])
	}

	// Without local, the project (approved) wins over user.
	g = Gather(Inputs{
		Settings:   []settingsload.OrderedResult{src(settingspolicy.SourceUser, map[string]any{"enableAllProjectMcpServers": true})},
		User:       map[string]mcp.ServerConfig{"shared": {Command: "user"}},
		FS:         m,
		ProjectDir: "/repo",
		LookupEnv:  noLookup,
	})
	if g.Servers["shared"].Command != "project" {
		t.Errorf("project should override user: %+v", g.Servers["shared"])
	}
}

func TestGatherProjectWalkDeeperWins(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/work/.mcp.json", []byte(`{"mcpServers":{"a":{"command":"outer"},"b":{"command":"outer-b"}}}`))
	m.WriteFile("/work/repo/.mcp.json", []byte(`{"mcpServers":{"a":{"command":"inner"}}}`))
	g := Gather(Inputs{FS: m, ProjectDir: "/work/repo", AutoApproveProject: true, LookupEnv: noLookup})
	if g.Servers["a"].Command != "inner" || g.Servers["b"].Command != "outer-b" {
		t.Fatalf("servers = %+v", g.Servers)
	}
}

func TestGatherProjectApproval(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/repo/.mcp.json", []byte(`{"mcpServers":{"ok server":{"command":"a"},"pending":{"command":"b"},"nope":{"command":"c"},"shadow":{"command":"d"}}}`))
	in := Inputs{
		Settings: []settingsload.OrderedResult{
			src(settingspolicy.SourceUser, map[string]any{"enabledMcpjsonServers": []any{"ok_server", "nope"}}),
			src(settingspolicy.SourceLocal, map[string]any{"disabledMcpjsonServers": []any{"nope"}}),
			// A repository cannot approve its own servers unless trusted.
			src(settingspolicy.SourceProject, map[string]any{"enableAllProjectMcpServers": true}),
		},
		User:       map[string]mcp.ServerConfig{"shadow": {Command: "user"}},
		FS:         m,
		ProjectDir: "/repo",
		LookupEnv:  noLookup,
	}
	g := Gather(in)
	if _, ok := g.Servers["ok server"]; !ok {
		t.Error("ok server should be approved via normalized name match")
	}
	if _, ok := g.Servers["pending"]; ok || skippedReason(g, "pending") != ReasonPendingApproval {
		t.Errorf("pending: %+v", g.Skipped)
	}
	if _, ok := g.Servers["nope"]; ok || skippedReason(g, "nope") != ReasonRejected {
		t.Errorf("nope (disabled wins over enabled): %+v", g.Skipped)
	}
	if g.Servers["shadow"].Command != "user" || skippedReason(g, "shadow") != "" {
		t.Errorf("shadowed pending project server should silently yield to user: %+v %+v", g.Servers["shadow"], g.Skipped)
	}

	in.TrustProjectSettings = true
	g = Gather(in)
	if _, ok := g.Servers["pending"]; !ok {
		t.Error("trusted project settings should approve all")
	}
	if _, ok := g.Servers["nope"]; ok {
		t.Error("disabledMcpjsonServers still rejects")
	}

	in.TrustProjectSettings = false
	in.AutoApproveProject = true
	g = Gather(in)
	if _, ok := g.Servers["pending"]; !ok {
		t.Error("non-interactive auto-approve")
	}
	if _, ok := g.Servers["nope"]; ok {
		t.Error("auto-approve must not override a rejection")
	}
}

func TestGatherPolicy(t *testing.T) {
	servers := map[string]mcp.ServerConfig{
		"named":   {Command: "n"},
		"cmd":     {Command: "npx", Args: []string{"-y", "good"}},
		"badcmd":  {Command: "npx", Args: []string{"-y", "evil"}},
		"remote":  {Type: mcp.TransportHTTP, URL: "https://mcp.example.com/v1/sse"},
		"remote2": {Type: mcp.TransportHTTP, URL: "https://other.example.org/"},
	}
	// Denylist by name, command and url pattern.
	g := Gather(Inputs{
		Settings: []settingsload.OrderedResult{
			src(settingspolicy.SourceUser, map[string]any{"deniedMcpServers": []any{map[string]any{"serverName": "named"}}}),
			src(settingspolicy.SourcePolicy, map[string]any{"deniedMcpServers": []any{
				map[string]any{"serverCommand": []any{"npx", "-y", "evil"}},
				map[string]any{"serverUrl": "https://*.example.org/*"},
			}}),
		},
		User:      servers,
		Managed:   map[string]mcp.ServerConfig{"named": {Command: "managed-but-denied"}},
		LookupEnv: noLookup,
	})
	for _, n := range []string{"named", "badcmd", "remote2"} {
		if _, ok := g.Servers[n]; ok || skippedReason(g, n) != ReasonDenied {
			t.Errorf("%s should be denied: %+v", n, g.Skipped)
		}
	}
	for _, n := range []string{"cmd", "remote"} {
		if _, ok := g.Servers[n]; !ok {
			t.Errorf("%s should be allowed", n)
		}
	}

	// Allowlist: command entries gate stdio servers, url entries gate remote
	// ones, managed servers bypass it, everything else by name.
	g = Gather(Inputs{
		Settings: []settingsload.OrderedResult{
			src(settingspolicy.SourceUser, map[string]any{"allowedMcpServers": []any{map[string]any{"serverName": "ignored-when-managed-only"}}}),
			src(settingspolicy.SourcePolicy, map[string]any{
				"allowManagedMcpServersOnly": true,
				"allowedMcpServers": []any{
					map[string]any{"serverName": "named"},
					map[string]any{"serverCommand": []any{"npx", "-y", "good"}},
					map[string]any{"serverUrl": "https://mcp.example.com/*"},
				},
			}),
		},
		User:      servers,
		Managed:   map[string]mcp.ServerConfig{"mgd": {Command: "anything"}},
		LookupEnv: noLookup,
	})
	for _, n := range []string{"cmd", "remote", "mgd"} {
		if _, ok := g.Servers[n]; !ok {
			t.Errorf("%s should be allowed: %+v", n, g.Skipped)
		}
	}
	// "named" is stdio and the allowlist has command entries → must match by command.
	for _, n := range []string{"named", "badcmd", "remote2"} {
		if _, ok := g.Servers[n]; ok || skippedReason(g, n) != ReasonNotAllowed {
			t.Errorf("%s should not be allowed: %+v", n, g.Skipped)
		}
	}

	// Empty allowlist blocks every non-managed server.
	g = Gather(Inputs{
		Settings:  []settingsload.OrderedResult{src(settingspolicy.SourcePolicy, map[string]any{"allowedMcpServers": []any{}})},
		User:      servers,
		Managed:   map[string]mcp.ServerConfig{"mgd": {Command: "m"}},
		LookupEnv: noLookup,
	})
	if len(g.Servers) != 1 || g.Servers["mgd"].Command != "m" {
		t.Errorf("servers = %+v", g.Servers)
	}
}

func TestGatherManagedExclusiveAndDisabled(t *testing.T) {
	g := Gather(Inputs{
		User:             map[string]mcp.ServerConfig{"u": {Command: "u"}},
		Managed:          map[string]mcp.ServerConfig{"m": {Command: "m"}},
		ManagedExclusive: true,
		LookupEnv:        noLookup,
	})
	if len(g.Servers) != 1 || skippedReason(g, "u") != ReasonManagedExclusive {
		t.Errorf("%+v", g)
	}
	g = Gather(Inputs{User: map[string]mcp.ServerConfig{"u x": {Command: "u"}}, Disabled: []string{"u_x"}, LookupEnv: noLookup})
	if len(g.Servers) != 0 || skippedReason(g, "u x") != ReasonDisabled {
		t.Errorf("%+v", g)
	}
}

func TestGatherPluginServers(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/plugins/demo/.claude-plugin/plugin.json", []byte(`{"name":"demo","version":"1.0.0"}`))
	m.WriteFile("/plugins/demo/.mcp.json", []byte(`{"mcpServers":{
		"db":{"command":"${CLAUDE_PLUGIN_ROOT}/bin/db","args":["--data","${CLAUDE_PLUGIN_DATA}","--proj","${CLAUDE_PROJECT_DIR}"]},
		"dup":{"command":"shared-cmd"}}}`))
	p, err := plugins.LoadPluginDir(m, "/plugins/demo", "mkt", plugins.OriginKind("marketplace"))
	if err != nil {
		t.Fatal(err)
	}
	g := Gather(Inputs{
		FS:            m,
		ProjectDir:    "/repo",
		Plugins:       []*plugins.Plugin{p},
		PluginDataDir: func(p *plugins.Plugin) string { return "/data/" + p.ID() },
		User:          map[string]mcp.ServerConfig{"mine": {Command: "shared-cmd"}},
		LookupEnv:     noLookup,
	})
	db, ok := g.Servers["plugin:demo:db"]
	if !ok {
		t.Fatalf("servers = %+v errs=%v", g.Servers, g.Errors)
	}
	if db.Command != "/plugins/demo/bin/db" || strings.Join(db.Args, " ") != "--data /data/demo@mkt --proj /repo" {
		t.Errorf("not expanded: %+v", db)
	}
	if g.Scopes["plugin:demo:db"] != ScopePlugin {
		t.Errorf("scope = %s", g.Scopes["plugin:demo:db"])
	}
	if _, ok := g.Servers["plugin:demo:dup"]; ok || !strings.Contains(skippedReason(g, "plugin:demo:dup"), `"mine"`) {
		t.Errorf("duplicate plugin server should be suppressed: %+v", g.Skipped)
	}
	if ToolName("plugin:demo:db", "query") != "mcp__plugin_demo_db__query" {
		t.Error("plugin tool name")
	}
	// Plugin approval names are exact.
	if sameServerName("plugin:demo:db", "plugin_demo_db") {
		t.Error("plugin names must match exactly")
	}
}

func TestTimeoutsAndLimits(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if d := ConnectTimeoutFromEnv(env(nil)); d != 30*time.Second {
		t.Errorf("connect default %v", d)
	}
	if d := ConnectTimeoutFromEnv(env(map[string]string{"MCP_TIMEOUT": "5000"})); d != 5*time.Second {
		t.Errorf("connect env %v", d)
	}
	if d := ToolTimeout(mcp.ServerConfig{}, 0); d != 100_000_000*time.Millisecond {
		t.Errorf("tool default %v", d)
	}
	if d := ToolTimeout(mcp.ServerConfig{Timeout: 999}, 0); d != 100_000_000*time.Millisecond {
		t.Errorf("config timeout < 1000 ignored: %v", d)
	}
	if d := ToolTimeout(mcp.ServerConfig{Timeout: 2500}, time.Minute); d != 2500*time.Millisecond {
		t.Errorf("config wins: %v", d)
	}
	if d := ToolTimeout(mcp.ServerConfig{}, ToolTimeoutFromEnv(env(map[string]string{"MCP_TOOL_TIMEOUT": "10"}))); d != time.Second {
		t.Errorf("clamped to 1s: %v", d)
	}
	if MaxOutputTokensFromEnv(env(nil)) != 25000 || MaxOutputTokensFromEnv(env(map[string]string{"MAX_MCP_OUTPUT_TOKENS": "7"})) != 7 {
		t.Error("max tokens")
	}
	small := strings.Repeat("a", 400) // 100 tokens == budget
	if out, cut := TruncateOutput(small, 100); cut || out != small {
		t.Error("at budget should not truncate")
	}
	out, cut := TruncateOutput(strings.Repeat("é", 300), 100) // 600 bytes
	if !cut || !strings.Contains(out, "[OUTPUT TRUNCATED - exceeded 100 token limit]") || !strings.HasPrefix(out, strings.Repeat("é", 200)+"\n\n") {
		t.Errorf("utf8 truncation: %q", out)
	}
}

func TestFlattenContent(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "text", Text: "a"},
		{Type: "image", MimeType: "image/png"},
		{Type: "audio"},
		{Type: "resource", Resource: []byte(`{"uri":"file:///x","text":"inline"}`)},
		{Type: "resource", Resource: []byte(`{"uri":"file:///bin","mimeType":"application/octet-stream","blob":"AA=="}`)},
	}}
	want := "a\n[Image: image/png]\n[Audio]\ninline\n[Resource: application/octet-stream file:///bin]"
	if got := FlattenContent(res); got != want {
		t.Errorf("got %q", got)
	}
	if got := FlattenContent(&mcp.CallToolResult{StructuredContent: []byte(`{"k":1}`)}); got != `{"k":1}` {
		t.Errorf("structured: %q", got)
	}
}
