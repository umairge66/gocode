package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// mcpPermEnv is an isolated Env: HOME / XDG_CONFIG_HOME and the cwd are temp
// dirs, so no real settings are read. userSettings, when non-empty, is written
// as the user settings.json.
func mcpPermEnv(t *testing.T, userSettings string) (*Env, string, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	cwd := t.TempDir()
	if userSettings != "" {
		dir := filepath.Join(home, ".config", "gocode")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(userSettings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	vars := map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config")}
	var errb bytes.Buffer
	env := &Env{
		Getenv:             func(k string) string { return vars[k] },
		Setenv:             func(string, string) {},
		Stdout:             io.Discard,
		Stderr:             &errb,
		Cwd:                func() (string, error) { return cwd, nil },
		ManagedSettingsDir: filepath.Join(home, "managed"),
		trustCheck:         func(string) bool { return true },
	}
	return env, cwd, &errb
}

// permClient is a fake MCP client wired to a served Server over in-process
// pipes. elicit, when non-nil, answers elicitation/create requests.
type permClient struct {
	t       *testing.T
	in      *io.PipeWriter
	sc      *bufio.Scanner
	elicit  func(params json.RawMessage) any
	elicits []json.RawMessage
	cancel  context.CancelFunc
	nextID  int
}

func startPermClient(t *testing.T, env *Env, defaults *DispatchDefaults, withElicitation bool, elicit func(json.RawMessage) any) *permClient {
	t.Helper()
	srv, _ := buildMcpServeServer(env, defaults)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = srv.ServeStdio(ctx, inR, outW)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	c := &permClient{t: t, in: inW, sc: sc, elicit: elicit, cancel: cancel}
	t.Cleanup(func() { inW.Close(); cancel() })

	caps := map[string]any{}
	if withElicitation {
		caps["elicitation"] = map[string]any{}
	}
	c.call("initialize", map[string]any{
		"protocolVersion": mcp.LatestProtocolVersion,
		"capabilities":    caps,
		"clientInfo":      map[string]any{"name": "fake", "version": "0"},
	})
	return c
}

func (c *permClient) send(v any) {
	b, _ := json.Marshal(v)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// call sends a request and returns its result, answering any server→client
// elicitation/create in between.
func (c *permClient) call(method string, params any) json.RawMessage {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	deadline := time.AfterFunc(20*time.Second, func() { c.in.Close() })
	defer deadline.Stop()
	for c.sc.Scan() {
		line := strings.TrimSpace(c.sc.Text())
		if line == "" {
			continue
		}
		msg, err := mcp.DecodeMessage([]byte(line))
		if err != nil {
			c.t.Fatalf("decode: %v", err)
		}
		switch {
		case msg.Request != nil:
			if msg.Request.Method != "elicitation/create" {
				c.t.Fatalf("unexpected server request %q", msg.Request.Method)
			}
			c.elicits = append(c.elicits, msg.Request.Params)
			if c.elicit == nil {
				c.t.Fatalf("server sent elicitation/create to a client without the capability")
			}
			c.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.Request.ID), "result": c.elicit(msg.Request.Params)})
		case msg.Response != nil:
			if strings.TrimSpace(string(msg.Response.ID)) != itoa(id) {
				continue
			}
			if msg.Response.Error != nil {
				c.t.Fatalf("%s error: %+v", method, msg.Response.Error)
			}
			return msg.Response.Result
		}
	}
	c.t.Fatalf("server closed before answering %s", method)
	return nil
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (c *permClient) callTool(name string, args any) mcp.CallToolResult {
	c.t.Helper()
	raw := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	var res mcp.CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		c.t.Fatalf("decode call result: %v", err)
	}
	return res
}

func resultText(r mcp.CallToolResult) string {
	var parts []string
	for _, b := range r.Content {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

func writeArgs(cwd, name string) (map[string]any, string) {
	p := filepath.Join(cwd, name)
	return map[string]any{"file_path": p, "content": "hello\n"}, p
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func acceptWith(decision string) func(json.RawMessage) any {
	return func(json.RawMessage) any {
		return map[string]any{"action": "accept", "content": map[string]any{"decision": decision}}
	}
}

// Allowed by an --allowedTools rule: runs without asking.
func TestMcpServePermission_AllowedByRuleRuns(t *testing.T) {
	env, cwd, _ := mcpPermEnv(t, "")
	c := startPermClient(t, env, &DispatchDefaults{AllowedTools: []string{"Write"}}, true, acceptWith("deny"))
	args, p := writeArgs(cwd, "a.txt")
	res := c.callTool("Write", args)
	if res.IsError || !fileExists(p) {
		t.Fatalf("allowed Write should run: isError=%v text=%q", res.IsError, resultText(res))
	}
	if len(c.elicits) != 0 {
		t.Errorf("an allowed call must not prompt, got %d elicitation(s)", len(c.elicits))
	}
}

// Needs a prompt; the client accepts "Allow once": runs, and the message shows
// the exact path.
func TestMcpServePermission_ElicitAcceptRuns(t *testing.T) {
	env, cwd, _ := mcpPermEnv(t, "")
	c := startPermClient(t, env, &DispatchDefaults{}, true, acceptWith("allow_once"))
	args, p := writeArgs(cwd, "b.txt")
	res := c.callTool("Write", args)
	if res.IsError || !fileExists(p) {
		t.Fatalf("approved Write should run: isError=%v text=%q", res.IsError, resultText(res))
	}
	if len(c.elicits) != 1 {
		t.Fatalf("want exactly one elicitation, got %d", len(c.elicits))
	}
	var params struct {
		Message         string         `json:"message"`
		RequestedSchema map[string]any `json:"requestedSchema"`
	}
	if err := json.Unmarshal(c.elicits[0], &params); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(params.Message, "Write") || !strings.Contains(params.Message, p) {
		t.Errorf("elicitation message should name the tool and exact path, got %q", params.Message)
	}
	schema, _ := json.Marshal(params.RequestedSchema)
	if strings.Contains(string(schema), "always") {
		t.Errorf("no 'always' option may be offered: %s", schema)
	}
	// Allow once is not remembered: the next call asks again.
	args2, _ := writeArgs(cwd, "b2.txt")
	c.callTool("Write", args2)
	if len(c.elicits) != 2 {
		t.Errorf("allow once must not persist; want 2 elicitations, got %d", len(c.elicits))
	}
}

// The client declines (or accepts with Deny, or cancels): refused, not run.
func TestMcpServePermission_ElicitDeclineRefused(t *testing.T) {
	for name, answer := range map[string]func(json.RawMessage) any{
		"decline": func(json.RawMessage) any { return map[string]any{"action": "decline"} },
		"cancel":  func(json.RawMessage) any { return map[string]any{"action": "cancel"} },
		"deny":    acceptWith("deny"),
		"garbage": acceptWith("yes please"),
	} {
		t.Run(name, func(t *testing.T) {
			env, cwd, _ := mcpPermEnv(t, "")
			c := startPermClient(t, env, &DispatchDefaults{}, true, answer)
			args, p := writeArgs(cwd, "c.txt")
			res := c.callTool("Write", args)
			if !res.IsError || fileExists(p) {
				t.Fatalf("declined Write must be refused: isError=%v exists=%v", res.IsError, fileExists(p))
			}
			if len(c.elicits) != 1 {
				t.Errorf("want one elicitation, got %d", len(c.elicits))
			}
		})
	}
}

// No elicitation capability: refused with guidance naming --allowedTools,
// settings allow rules and --permission-mode.
func TestMcpServePermission_NoElicitationRefusedWithGuidance(t *testing.T) {
	env, cwd, _ := mcpPermEnv(t, "")
	c := startPermClient(t, env, &DispatchDefaults{}, false, nil)
	args, p := writeArgs(cwd, "d.txt")
	res := c.callTool("Write", args)
	if !res.IsError || fileExists(p) {
		t.Fatalf("Write without a prompt surface must be refused: isError=%v exists=%v", res.IsError, fileExists(p))
	}
	text := resultText(res)
	for _, want := range []string{"elicitation", "--allowedTools", "permissions.allow", "--permission-mode"} {
		if !strings.Contains(text, want) {
			t.Errorf("guidance missing %q: %q", want, text)
		}
	}
}

// A deny rule refuses the call before any prompt, even with elicitation.
func TestMcpServePermission_DenyRuleRefused(t *testing.T) {
	env, cwd, _ := mcpPermEnv(t, `{"permissions":{"deny":["Write"]}}`)
	c := startPermClient(t, env, &DispatchDefaults{}, true, acceptWith("allow_once"))
	args, p := writeArgs(cwd, "e.txt")
	res := c.callTool("Write", args)
	if !res.IsError || fileExists(p) {
		t.Fatalf("denied Write must be refused: isError=%v text=%q", res.IsError, resultText(res))
	}
	if len(c.elicits) != 0 {
		t.Errorf("a deny rule must not prompt, got %d elicitation(s)", len(c.elicits))
	}

	// --disallowedTools works the same way.
	env2, cwd2, _ := mcpPermEnv(t, "")
	c2 := startPermClient(t, env2, &DispatchDefaults{DisallowedTools: []string{"Write"}}, true, acceptWith("allow_once"))
	args2, p2 := writeArgs(cwd2, "f.txt")
	if res := c2.callTool("Write", args2); !res.IsError || fileExists(p2) {
		t.Fatalf("--disallowedTools Write must be refused: %q", resultText(res))
	}
}

// bypassPermissions only from a flag: a settings defaultMode of
// bypassPermissions is not honoured by mcp serve.
func TestMcpServeMode_BypassOnlyByFlag(t *testing.T) {
	env, _, _ := mcpPermEnv(t, `{"permissions":{"defaultMode":"bypassPermissions"}}`)
	if m, _ := mcpServeMode(env, nil); m == "bypassPermissions" {
		t.Fatalf("settings defaultMode must not enable bypass in mcp serve")
	}
	if m, _ := mcpServeMode(env, &DispatchDefaults{PermissionMode: "acceptEdits"}); m != "acceptEdits" {
		t.Errorf("--permission-mode acceptEdits: got %q", m)
	}
	env2, _, _ := mcpPermEnv(t, `{"permissions":{"defaultMode":"acceptEdits"}}`)
	if m, _ := mcpServeMode(env2, nil); m != "acceptEdits" {
		t.Errorf("settings defaultMode acceptEdits should apply, got %q", m)
	}
	env3, _, _ := mcpPermEnv(t, `{"permissions":{"disableBypassPermissionsMode":"disable"}}`)
	if m, _ := mcpServeMode(env3, &DispatchDefaults{PermissionMode: "bypassPermissions"}); m == "bypassPermissions" {
		t.Errorf("disableBypassPermissionsMode must still refuse the flag")
	}
}

func TestSupportsFormElicitation(t *testing.T) {
	for raw, want := range map[string]bool{
		``: false, `null`: false, `{}`: true, `{"form":{}}`: true,
		`{"url":{}}`: false, `{"form":{},"url":{}}`: true,
	} {
		if got := supportsFormElicitation(json.RawMessage(raw)); got != want {
			t.Errorf("supportsFormElicitation(%s) = %v, want %v", raw, got, want)
		}
	}
}
