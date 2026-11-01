package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcp/mcptest"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/platform"
)

// missingServer is a stdio command that cannot start, so health checks fail
// fast without spawning anything real.
const missingServer = "/nonexistent/gocode-test-mcp-server"

func (f *subcmdFixture) localSettingsPath() string {
	return filepath.Join(f.cwd, ".gocode", "settings.local.json")
}

func (f *subcmdFixture) mcpJSONPath() string { return filepath.Join(f.cwd, ".mcp.json") }

// mcpServersIn returns the mcpServers table of a JSON file (nil if absent).
func mcpServersIn(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	m, _ := doc["mcpServers"].(map[string]any)
	return m
}

// TestMCPAddListRemoveRoundTrip: an added server lands in the file of its
// scope, is read back by the session's MCP config loading (`mcp list`), and is
// gone after `mcp remove`.
func TestMCPAddListRemoveRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		scopeArgs []string
		scope     string
		file      func(f *subcmdFixture) string
		status    string
	}{
		{"default-local", nil, "local", (*subcmdFixture).localSettingsPath, "\u2717 Failed to connect"},
		{"local", []string{"-s", "local"}, "local", (*subcmdFixture).localSettingsPath, "\u2717 Failed to connect"},
		{"user", []string{"--scope", "user"}, "user", (*subcmdFixture).userSettingsPath, "\u2717 Failed to connect"},
		{"project", []string{"--scope=project"}, "project", (*subcmdFixture).mcpJSONPath, "\u23F8 Pending approval (run `gocode` to approve)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSubcmdFixture(t)
			args := append([]string{"mcp", "add"}, c.scopeArgs...)
			args = append(args, "srv", "-e", "API_KEY=${MY_KEY}", "--", missingServer, "--port", "1", "-h")
			code, out, errOut := f.run(t, args...)
			if code != 0 {
				t.Fatalf("add exit %d stderr=%q", code, errOut)
			}
			wantOut := "Added stdio MCP server srv with command: " + missingServer + " --port 1 -h to " + c.scope + " config\n" +
				"File modified: " + c.file(f) + "\n"
			if out != wantOut {
				t.Fatalf("add stdout = %q, want %q", out, wantOut)
			}
			entry, _ := mcpServersIn(t, c.file(f))["srv"].(map[string]any)
			if entry == nil {
				t.Fatalf("srv not written to %s", c.file(f))
			}
			if entry["type"] != "stdio" || entry["command"] != missingServer {
				t.Errorf("entry = %v", entry)
			}
			if got, _ := json.Marshal(entry["args"]); string(got) != `["--port","1","-h"]` {
				t.Errorf("args = %s", got)
			}
			if env, _ := entry["env"].(map[string]any); env["API_KEY"] != "${MY_KEY}" {
				t.Errorf("env = %v", entry["env"])
			}

			code, out, _ = f.run(t, "mcp", "list")
			if code != 0 {
				t.Fatalf("list exit %d", code)
			}
			if !strings.HasPrefix(out, "Checking MCP server health\u2026\n\n") {
				t.Errorf("list stdout = %q", out)
			}
			if !strings.Contains(out, "srv: "+missingServer+" --port 1 -h - "+c.status) {
				t.Errorf("list stdout = %q, want status %q", out, c.status)
			}

			code, out, errOut = f.run(t, "mcp", "remove", "srv")
			if code != 0 {
				t.Fatalf("remove exit %d stderr=%q", code, errOut)
			}
			if want := "Removed MCP server \"srv\" from " + c.scope + " config\nFile modified: " + c.file(f) + "\n"; out != want {
				t.Errorf("remove stdout = %q, want %q", out, want)
			}
			if _, ok := mcpServersIn(t, c.file(f))["srv"]; ok {
				t.Error("srv still present after remove")
			}
			_, out, _ = f.run(t, "mcp", "list")
			if out != "No MCP servers configured. Use `gocode mcp add` to add a server.\n" {
				t.Errorf("list after remove = %q", out)
			}
		})
	}
}

// TestMCPAddPositionalAndVariadic covers the accepted operand forms: a bare
// command with arguments, and a variadic -e that stops at "--".
func TestMCPAddPositionalAndVariadic(t *testing.T) {
	f := newSubcmdFixture(t)
	if code, _, errOut := f.run(t, "mcp", "add", "plain", "node", "server.js"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if code, _, errOut := f.run(t, "mcp", "add", "vars", "-e", "A=1", "B=x=y", "--", "cmd", "--flag"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	servers := mcpServersIn(t, f.localSettingsPath())
	plain, _ := servers["plain"].(map[string]any)
	if plain["command"] != "node" || plain["args"].([]any)[0] != "server.js" {
		t.Errorf("plain = %v", plain)
	}
	vars, _ := servers["vars"].(map[string]any)
	env, _ := vars["env"].(map[string]any)
	if env["A"] != "1" || env["B"] != "x=y" || vars["command"] != "cmd" || vars["args"].([]any)[0] != "--flag" {
		t.Errorf("vars = %v", vars)
	}
}

// TestMCPAddRemote: HTTP / SSE servers with headers; secret-looking header
// values are never echoed.
func TestMCPAddRemote(t *testing.T) {
	f := newSubcmdFixture(t)
	code, out, errOut := f.run(t, "mcp", "add", "--transport", "http", "api", "https://user:pw@example.com/mcp/?token=abc",
		"-H", "Authorization: Bearer sk-secret", "X-Team: core", "--client-id", "cid", "--callback-port", "8123")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "sk-secret") || strings.Contains(out, "pw@") || strings.Contains(out, "token=abc") {
		t.Errorf("secret echoed: %q", out)
	}
	want := "Added HTTP MCP server api with URL: https://example.com/mcp to local config\n" +
		"Headers: {\n  \"Authorization\": \"[REDACTED]\",\n  \"X-Team\": \"core\"\n}\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("stdout = %q, want prefix %q", out, want)
	}
	api, _ := mcpServersIn(t, f.localSettingsPath())["api"].(map[string]any)
	headers, _ := api["headers"].(map[string]any)
	oauth, _ := api["oauth"].(map[string]any)
	if api["type"] != "http" || headers["Authorization"] != "Bearer sk-secret" || oauth["clientId"] != "cid" || oauth["callbackPort"] != float64(8123) {
		t.Errorf("api = %v", api)
	}

	code, out, _ = f.run(t, "mcp", "add", "-t", "sse", "-s", "user", "events", "https://example.com/sse")
	if code != 0 || !strings.HasPrefix(out, "Added SSE MCP server events with URL: https://example.com/sse to user config\n") {
		t.Errorf("sse add: %d %q", code, out)
	}

	// A URL without --transport is a stdio command, with a warning.
	code, _, errOut = f.run(t, "mcp", "add", "oops", "https://example.com/mcp")
	if code != 0 || !strings.Contains(errOut, "looks like a URL") || !strings.Contains(errOut, "gocode mcp add --transport http oops https://example.com/mcp") {
		t.Errorf("url-as-stdio: %d %q", code, errOut)
	}
}

// TestMCPClientSecretStored: --client-secret keeps the secret in the OAuth
// credential store, never in the server config.
func TestMCPClientSecretStored(t *testing.T) {
	f := newSubcmdFixture(t)
	f.vars["MCP_CLIENT_SECRET"] = "s3cr3t"
	code, _, errOut := f.run(t, "mcp", "add", "-t", "http", "api", "https://example.com/mcp", "--client-id", "cid", "--client-secret")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if data, _ := os.ReadFile(f.localSettingsPath()); strings.Contains(string(data), "s3cr3t") {
		t.Fatal("client secret written to settings")
	}
	env, _, _ := f.env()
	store := mcpAuthStore(env)
	key := mcp.ServerKey("api", mcp.ServerConfig{Type: mcp.TransportHTTP, URL: "https://example.com/mcp"})
	if store == nil || store.ClientSecret(key) != "s3cr3t" {
		t.Fatal("client secret not stored")
	}
	delete(f.vars, "MCP_CLIENT_SECRET")
	code, _, errOut = f.run(t, "mcp", "add", "-t", "http", "api2", "https://example.com/mcp", "--client-id", "cid", "--client-secret")
	if code != 1 || !strings.Contains(errOut, "No TTY available to prompt for client secret") {
		t.Errorf("no-tty secret: %d %q", code, errOut)
	}
	if _, ok := mcpServersIn(t, f.localSettingsPath())["api2"]; ok {
		t.Error("server added although the secret could not be read")
	}
}

// TestMCPErrors: validation failures exit 1 with the expected messages and
// leave the files untouched.
func TestMCPErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"invalid-name", []string{"mcp", "add", "bad name", "cmd"}, "Invalid name bad name. Names can only contain letters, numbers, hyphens, and underscores."},
		{"collision", []string{"mcp", "add", "dup", "cmd"}, "MCP server dup already exists in local config"},
		{"collision-project", []string{"mcp", "add", "-s", "project", "pdup", "cmd"}, "MCP server pdup already exists in .mcp.json"},
		{"invalid-scope", []string{"mcp", "add", "-s", "global", "x", "cmd"}, "Invalid scope: global. Must be one of: local, user, project, dynamic, enterprise, gocodeai, managed, agent"},
		{"dynamic-scope", []string{"mcp", "add", "-s", "dynamic", "x", "cmd"}, "Cannot add MCP server to scope: dynamic"},
		{"invalid-transport", []string{"mcp", "add", "-t", "grpc", "x", "cmd"}, "Invalid transport type: grpc. Must be one of: stdio, sse, http (or streamable-http)"},
		{"invalid-header", []string{"mcp", "add", "-t", "http", "x", "https://e.com", "-H", "nocolon"}, `Invalid header format: "nocolon". Expected format: "Header-Name: value"`},
		{"invalid-env", []string{"mcp", "add", "x", "-e", "NOVALUE", "--", "cmd"}, "Invalid environment variable format: NOVALUE, environment variables should be added as: -e KEY1=value1 -e KEY2=value2"},
		{"callback-port", []string{"mcp", "add", "-t", "http", "x", "https://e.com", "--callback-port", "99999"}, "Error: --callback-port must be an integer in [1, 65535]"},
		{"missing-arg", []string{"mcp", "add", "x"}, "error: missing required argument 'commandOrUrl'"},
		{"unknown-option", []string{"mcp", "add", "x", "cmd", "--frobnicate"}, "error: unknown option '--frobnicate'"},
		{"add-json-invalid", []string{"mcp", "add-json", "j", "{not json"}, "Invalid configuration: : Invalid input"},
		{"add-json-empty-command", []string{"mcp", "add-json", "j", `{"command":""}`}, "Invalid configuration: command: Command cannot be empty"},
		{"add-json-host-only", []string{"mcp", "add-json", "j", `{"type":"sdk","name":"x"}`}, `Cannot add MCP server "j": type "sdk" servers are registered by the host application at runtime, not stored in configuration`},
		{"remove-not-found", []string{"mcp", "remove", "dupe"}, `No MCP server named "dupe". Did you mean "dup"? Run ` + "`gocode mcp list`" + ` to see all.`},
		{"remove-wrong-scope", []string{"mcp", "remove", "-s", "user", "dup"}, `No MCP server named "dup" in user scope`},
		{"too-many", []string{"mcp", "remove", "a", "b"}, "error: too many arguments for 'remove'. Expected 1 argument but got 2."},
		{"unknown-verb", []string{"mcp", "frob"}, "error: unknown command 'frob'"},
		{"get-not-found", []string{"mcp", "get", "zzz"}, `No MCP server named "zzz". Configured servers: dup`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSubcmdFixture(t)
			subcmdWriteFile(t, f.localSettingsPath(), `{"mcpServers":{"dup":{"command":"`+missingServer+`"}}}`)
			subcmdWriteFile(t, f.mcpJSONPath(), `{"mcpServers":{"pdup":{"command":"x"}}}`)
			subcmdWriteFile(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{}`)
			before := subcmdReadFile(t, f.localSettingsPath())
			code, out, errOut := f.run(t, c.args...)
			if code != 1 {
				t.Fatalf("exit = %d, stdout=%q stderr=%q", code, out, errOut)
			}
			if !strings.Contains(errOut, c.want) {
				t.Errorf("stderr = %q, want %q", errOut, c.want)
			}
			if after := subcmdReadFile(t, f.localSettingsPath()); after != before {
				t.Errorf("local settings changed: %q", after)
			}
		})
	}
}

// TestMCPRemoveAmbiguous: a name in several scopes needs -s.
func TestMCPRemoveAmbiguous(t *testing.T) {
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.localSettingsPath(), `{"mcpServers":{"both":{"command":"a"}}}`)
	subcmdWriteFile(t, f.userSettingsPath(), `{"mcpServers":{"both":{"command":"b"}}}`)
	code, _, errOut := f.run(t, "mcp", "remove", "both")
	if code != 1 || !strings.Contains(errOut, `MCP server "both" exists in multiple scopes:`) ||
		!strings.Contains(errOut, "gocode mcp remove both -s local") || !strings.Contains(errOut, "gocode mcp remove both -s user") {
		t.Fatalf("ambiguous remove: %d %q", code, errOut)
	}
	code, out, _ := f.run(t, "mcp", "remove", "both", "-s", "user")
	if code != 0 || !strings.HasPrefix(out, "Removed MCP server both from user config\n") {
		t.Fatalf("scoped remove: %d %q", code, out)
	}
	if _, ok := mcpServersIn(t, f.localSettingsPath())["both"]; !ok {
		t.Error("local entry removed too")
	}
}

// TestMCPWritePreservesFile: unrelated keys and the permission bits survive.
func TestMCPWritePreservesFile(t *testing.T) {
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.userSettingsPath(), `{"theme":"dark","mcpServers":{"old":{"command":"x"}}}`)
	if err := os.Chmod(f.userSettingsPath(), 0o640); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := f.run(t, "mcp", "add-json", "-s", "user", "new", `{"type":"http","url":"https://e.com/mcp"}`); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(subcmdReadFile(t, f.userSettingsPath())), &doc); err != nil {
		t.Fatal(err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if doc["theme"] != "dark" || servers["old"] == nil || servers["new"] == nil {
		t.Errorf("doc = %v", doc)
	}
	if st, _ := os.Stat(f.userSettingsPath()); st.Mode().Perm() != 0o640 && runtime.GOOS != "windows" {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	// A malformed file is never clobbered.
	subcmdWriteFile(t, f.mcpJSONPath(), `{"mcpServers": [}`)
	code, _, errOut := f.run(t, "mcp", "add", "-s", "project", "p", "cmd")
	if code != 1 || !strings.Contains(errOut, ".mcp.json is malformed") {
		t.Errorf("malformed .mcp.json: %d %q", code, errOut)
	}
	if subcmdReadFile(t, f.mcpJSONPath()) != `{"mcpServers": [}` {
		t.Error(".mcp.json rewritten")
	}
}

// TestMCPAddJSON: the server type is reported and the config read back.
func TestMCPAddJSON(t *testing.T) {
	f := newSubcmdFixture(t)
	code, out, errOut := f.run(t, "mcp", "add-json", "web", `{"type":"sse","url":"https://e.com/sse"}`)
	if code != 0 || out != "Added sse MCP server web to local config\n" {
		t.Fatalf("add-json: %d %q %q", code, out, errOut)
	}
	code, out, _ = f.run(t, "mcp", "add-json", "-s", "project", "loc", `{"command":"`+missingServer+`"}`)
	if code != 0 || out != "Added stdio MCP server loc to project config\n" {
		t.Fatalf("add-json stdio: %d %q", code, out)
	}
	if _, ok := mcpServersIn(t, f.mcpJSONPath())["loc"]; !ok {
		t.Error("loc not in .mcp.json")
	}
}

// TestMCPGet: config details (raw, unexpanded values) and pending approval.
func TestMCPGet(t *testing.T) {
	f := newSubcmdFixture(t)
	f.vars["MY_KEY"] = "expanded-secret-value"
	subcmdWriteFile(t, f.localSettingsPath(), `{"mcpServers":{"tool":{"command":"`+missingServer+`","args":["a"],"env":{"API_KEY":"${MY_KEY}"},"timeout":500}}}`)
	subcmdWriteFile(t, f.mcpJSONPath(), `{"mcpServers":{"shared":{"type":"http","url":"https://e.com/mcp"}}}`)

	code, out, errOut := f.run(t, "mcp", "get", "tool")
	if code != 0 {
		t.Fatalf("get exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"tool:\n  Scope: Local config (private to you in this project)\n  Status: \u2717 Failed to connect",
		"  Type: stdio\n  Command: " + missingServer + "\n  Args: a\n  Environment:\n    API_KEY=${MY_KEY}\n",
		"  Timeout: 500ms (ignored: below 1000ms minimum)\n",
		"\nTo remove this server, run: gocode mcp remove tool -s local\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("get stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "expanded-secret-value") {
		t.Error("expanded env value echoed")
	}

	code, out, _ = f.run(t, "mcp", "get", "shared")
	if code != 0 || !strings.Contains(out, "  Scope: Project config (shared via .mcp.json)\n  Status: \u23F8 Pending approval (run `gocode` to approve)\n  Type: http\n  URL: https://e.com/mcp\n") {
		t.Errorf("pending get: %d %q", code, out)
	}
}

// TestMCPResetProjectChoices clears the approval keys only and reports what
// other settings still decide.
func TestMCPResetProjectChoices(t *testing.T) {
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.mcpJSONPath(), `{"mcpServers":{"a":{"command":"x"},"b":{"command":"y"},"c":{"command":"z"}}}`)
	subcmdWriteFile(t, f.localSettingsPath(), `{"model":"m","enabledMcpjsonServers":["a","b"],"disabledMcpjsonServers":["c"],"enableAllProjectMcpServers":false}`)
	subcmdWriteFile(t, f.userSettingsPath(), `{"enabledMcpjsonServers":["a"]}`)

	code, out, errOut := f.run(t, "mcp", "reset-project-choices")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := "Project-scoped (.mcp.json) server approvals and rejections stored for this project have been reset.\n" +
		"1 server (a) is still approved by other settings and will connect automatically without prompting.\n" +
		"You will be prompted for approval next time you start gocode.\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(subcmdReadFile(t, f.localSettingsPath())), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "m" || len(doc) != 1 {
		t.Errorf("local settings after reset = %v", doc)
	}
	_, out, _ = f.run(t, "mcp", "list")
	if !strings.Contains(out, "b: y  - \u23F8 Pending approval") || !strings.Contains(out, "c: z  - \u23F8 Pending approval") {
		t.Errorf("list after reset = %q", out)
	}
}

// TestMCPHelp: help never starts a session and documents the subcommands.
func TestMCPHelp(t *testing.T) {
	f := newSubcmdFixture(t)
	code, out, _ := f.run(t, "mcp", "--help")
	if code != 0 || !strings.Contains(out, "Configure and manage MCP servers") || !strings.Contains(out, "add-json <name> <json>") || !strings.Contains(out, "reset-project-choices") {
		t.Errorf("mcp --help: %d %q", code, out)
	}
	code, out, _ = f.run(t, "mcp", "add", "--help")
	if code != 0 || !strings.Contains(out, "gocode mcp add [options] <name> <commandOrUrl> [args...]") || !strings.Contains(out, "-t, --transport <transport>") {
		t.Errorf("mcp add --help: %d %q", code, out)
	}
	code, out, _ = f.run(t, "help", "mcp", "remove")
	if code != 0 || !strings.Contains(out, "gocode mcp remove [options] <name>") {
		t.Errorf("help mcp remove: %d %q", code, out)
	}
	code, out, errOut := f.run(t, "mcp")
	if code != 1 || out != "" || !strings.Contains(errOut, "Configure and manage MCP servers") {
		t.Errorf("bare mcp: %d %q %q", code, out, errOut)
	}
}

// TestMCPAddFromDesktop maps to the Claude Desktop config on macOS / WSL and
// refuses elsewhere.
func TestMCPAddFromDesktop(t *testing.T) {
	old := mcpDesktopPlatform
	defer func() { mcpDesktopPlatform = old }()

	f := newSubcmdFixture(t)
	mcpDesktopPlatform = func() string { return platform.PlatformLinux }
	code, _, errOut := f.run(t, "mcp", "add-from-gocode-desktop")
	if code != 1 || !strings.Contains(errOut, "Unsupported platform - Claude Desktop integration only works on macOS and WSL.") {
		t.Fatalf("linux: %d %q", code, errOut)
	}

	mcpDesktopPlatform = func() string { return platform.PlatformMacOS }
	code, out, _ := f.run(t, "mcp", "add-from-gocode-desktop")
	if code != 0 || !strings.Contains(out, "No MCP servers found in Claude Desktop configuration or configuration file does not exist.") {
		t.Fatalf("no config: %d %q", code, out)
	}
	subcmdWriteFile(t, filepath.Join(f.home, "Library", "Application Support", "Claude", "claude_desktop_config.json"),
		`{"mcpServers":{"fs":{"command":"npx","args":["fs-server"]},"dup":{"command":"x"},"remote":{"type":"http","url":"https://e.com"}}}`)
	subcmdWriteFile(t, f.userSettingsPath(), `{"mcpServers":{"dup":{"command":"`+missingServer+`"}}}`)
	code, out, errOut = f.run(t, "mcp", "add-from-gocode-desktop", "-s", "user")
	if code != 0 || !strings.Contains(out, "Found 2 MCP servers in Claude Desktop.") || !strings.Contains(out, "Successfully imported 1 MCP server to user config.") {
		t.Fatalf("import: %d %q %q", code, out, errOut)
	}
	servers := mcpServersIn(t, f.userSettingsPath())
	if servers["fs"] == nil || servers["dup_1"] != nil || len(servers) != 2 {
		t.Errorf("user servers = %v", servers)
	}
	// On a terminal, an explicitly selected colliding server gets a suffix.
	f.stdinTTY, f.stdin = true, "1\n"
	code, out, _ = f.run(t, "mcp", "add-from-gocode-desktop", "-s", "user")
	if code != 0 || !strings.Contains(out, "Successfully imported 1 MCP server to user config.") {
		t.Fatalf("tty import: %d %q", code, out)
	}
	if mcpServersIn(t, f.userSettingsPath())["dup_1"] == nil {
		t.Error("dup_1 not imported")
	}
}

// TestMCPLoginLogout runs the OAuth sign-in against a local test server
// (loopback only) and clears it again.
func TestMCPLoginLogout(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	var calls int
	var mu sync.Mutex
	old := mcpLoginOpenBrowser
	mcpLoginOpenBrowser = mcptest.Browser(&calls, &mu)
	defer func() { mcpLoginOpenBrowser = old }()

	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.localSettingsPath(), `{"mcpServers":{"remote":{"type":"http","url":"`+srv.URL()+`"},"local":{"command":"`+missingServer+`"}}}`)
	subcmdWriteFile(t, f.mcpJSONPath(), `{"mcpServers":{"pend":{"type":"http","url":"https://e.com"}}}`)

	code, _, errOut := f.run(t, "mcp", "login", "local")
	if code != 1 || !strings.Contains(errOut, `"local" doesn't support OAuth login`) {
		t.Errorf("stdio login: %d %q", code, errOut)
	}
	code, _, errOut = f.run(t, "mcp", "login", "pend")
	if code != 1 || !strings.Contains(errOut, `"pend" is from .mcp.json and awaiting approval.`) {
		t.Errorf("pending login: %d %q", code, errOut)
	}
	code, out, errOut := f.run(t, "mcp", "login", "remote")
	if code != 0 || !strings.Contains(out, `Starting authentication for "remote"`) || !strings.Contains(out, `Authenticated with "remote". Its tools are now available in gocode.`) {
		t.Fatalf("login: %d %q %q", code, out, errOut)
	}
	_, out, _ = f.run(t, "mcp", "list")
	if !strings.Contains(out, "remote: "+srv.URL()+" (HTTP) - \u2713 Connected") {
		t.Errorf("list after login = %q", out)
	}
	code, out, _ = f.run(t, "mcp", "logout", "remote")
	if code != 0 || out != "Signed out of \"remote\". Run `gocode mcp login remote` to authenticate again.\n" {
		t.Errorf("logout: %d %q", code, out)
	}
	_, out, _ = f.run(t, "mcp", "list")
	if !strings.Contains(out, "remote: "+srv.URL()+" (HTTP) - ! Needs authentication") {
		t.Errorf("list after logout = %q", out)
	}
	code, _, errOut = f.run(t, "mcp", "logout", "local")
	if code != 1 || !strings.Contains(errOut, `"local" doesn't use OAuth`) {
		t.Errorf("stdio logout: %d %q", code, errOut)
	}
}

// syncWriter is a goroutine-safe stdout that hands the authorization URL to a
// callback (the test's stand-in for a user on another machine).
type syncWriter struct {
	mu    sync.Mutex
	buf   strings.Builder
	onURL func(string)
	seen  bool
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	const lead = "Visit this URL to authorize:\n  "
	if s := w.buf.String(); !w.seen && strings.Contains(s, lead) {
		rest := s[strings.Index(s, lead)+len(lead):]
		if nl := strings.Index(rest, "\n"); nl != -1 {
			w.seen = true
			go w.onURL(rest[:nl])
		}
	}
	return len(p), nil
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestMCPLoginNoBrowserPaste: with --no-browser the URL is printed and the
// redirect URL pasted on stdin completes the sign-in; the tokens land in the
// store under the key a session uses, so a later connection needs no re-auth.
func TestMCPLoginNoBrowserPaste(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	old := mcpLoginOpenBrowser
	mcpLoginOpenBrowser = func(string) error { t.Error("browser opened despite --no-browser"); return nil }
	defer func() { mcpLoginOpenBrowser = old }()

	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.localSettingsPath(), `{"mcpServers":{"remote":{"type":"http","url":"`+srv.URL()+`"}}}`)
	env, _, errb := f.env()
	pr, pw := io.Pipe()
	defer pw.Close()
	out := &syncWriter{onURL: func(u string) {
		// The "user" opens the URL elsewhere and copies the address the
		// authorization server redirects to (the loopback is never reached).
		noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := noFollow.Get(u)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
		_, _ = io.WriteString(pw, "not a redirect\n"+resp.Header.Get("Location")+"\n")
	}}
	env.Stdout, env.Stdin = out, pr
	code := Run(env, []string{"gocode", "mcp", "login", "remote", "--no-browser"})
	if code != 0 {
		t.Fatalf("login exit %d: %s\n%s", code, errb.String(), out.String())
	}
	got := out.String()
	for _, want := range []string{
		"Starting authentication for \"remote\"\u2026\n",
		"Waiting for authorization\u2026 (^C to cancel)\n",
		"That doesn't look like a redirect URL for this sign-in",
		"Authenticated with \"remote\". Its tools are now available in gocode.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
	store := mcpAuthStore(env)
	cfg := mcp.ServerConfig{Type: mcp.TransportHTTP, URL: srv.URL()}
	e, ok, err := store.Get(mcp.ServerKey("remote", cfg))
	if err != nil || !ok || e.AccessToken == "" {
		t.Fatalf("token not stored: %+v %v %v", e, ok, err)
	}
	h := mcphost.Start(context.Background(), map[string]mcp.ServerConfig{"remote": cfg},
		mcphost.Options{Auth: store, Getenv: func(string) string { return "" }, ConnectTimeout: 5 * time.Second})
	defer h.Close()
	if st := h.Status(); len(st) != 1 || st[0].State != mcphost.StateConnected {
		t.Fatalf("session connection after login: %+v", st)
	}
	if reg, ex, _ := srv.Counts(); reg != 1 || ex != 1 {
		t.Errorf("registrations/exchanges = %d/%d", reg, ex)
	}

	code, o, _ := f.run(t, "mcp", "logout", "remote")
	if code != 0 || !strings.HasPrefix(o, "Signed out of \"remote\".") {
		t.Fatalf("logout: %d %q", code, o)
	}
	if _, ok, _ := store.Get(mcp.ServerKey("remote", cfg)); ok {
		t.Error("tokens still stored after logout")
	}
	code, o, _ = f.run(t, "mcp", "logout", "remote")
	if code != 0 || o != "No stored OAuth credentials for \"remote\" \u2014 nothing to sign out of.\n" {
		t.Errorf("second logout: %d %q", code, o)
	}
	code, _, e2 := f.run(t, "mcp", "login", "nosuch")
	if code != 1 || !strings.Contains(e2, `No MCP server named "nosuch".`) {
		t.Errorf("unknown login: %d %q", code, e2)
	}
}

// `mcp get` never prints a stored credential: secret-named headers and env
// values are withheld (a ${VAR} reference is still shown), others are kept.
func TestMCPGetMasksStoredSecrets(t *testing.T) {
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.localSettingsPath(), `{"mcpServers":{
		"remote":{"type":"http","url":"https://e.com/mcp","headers":{"Authorization":"Bearer secret123","X-Region":"eu"}},
		"local":{"command":"`+missingServer+`","env":{"GITHUB_TOKEN":"ghp_plainvalue","API_KEY":"${MY_KEY}","LOG_LEVEL":"debug"}}}}`)
	_, out, _ := f.run(t, "mcp", "get", "remote")
	if strings.Contains(out, "secret123") || !strings.Contains(out, "Authorization: [REDACTED]") || !strings.Contains(out, "X-Region: eu") {
		t.Errorf("remote headers not masked as expected:\n%s", out)
	}
	_, out, _ = f.run(t, "mcp", "get", "local")
	if strings.Contains(out, "ghp_plainvalue") || !strings.Contains(out, "GITHUB_TOKEN=[REDACTED]") ||
		!strings.Contains(out, "API_KEY=${MY_KEY}") || !strings.Contains(out, "LOG_LEVEL=debug") {
		t.Errorf("local env not masked as expected:\n%s", out)
	}
}
