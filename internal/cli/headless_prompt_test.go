package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// hooksSettings configures one PreToolUse command hook.
const hooksSettings = `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"true"}]}]}}`

const (
	hooksBulletText   = "Users may configure 'hooks'"
	browserBulletText = "untrusted-web-content"
	promptedText      = "the user will be prompted so that they can approve or deny"
	scriptFileText    = "write them to a file with Write once, then edit and re-run that file"
	headlessFinalText = "Your final message is the result of the run: state what you did and what you verified, briefly."
)

// Golden hashes of the interactive session's fixed prompt, with hooks
// configured and the default permission mode: the system prompt without its
// "# Environment" section, and the serialized tool definitions with the
// default compact tool texts and with GOCODE_FULL_TOOL_DESCRIPTIONS set.
const (
	interactiveSystemGolden    = "d116ed927fb04e619e0722e164e13c347f3ed1e5b365ce482ba60143ccc84d6e"
	interactiveToolsGolden     = "075308560f0d26286ee901bfcb8ab0b08173f5eab9bb9ad487c5cee39a6ce08b"
	interactiveFullToolsGolden = "1637157f1490d768331745102fa91a28a52cb1a1216a125f1d25eacc841de3ad"
)

func withoutEnvironment(system string) string {
	if i := strings.Index(system, "\n\n# Environment\n"); i >= 0 {
		return system[:i]
	}
	return system
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func toolNames(tools []agent.ToolSchema) map[string]bool {
	out := map[string]bool{}
	for _, tool := range tools {
		out[tool.Name] = true
	}
	return out
}

func TestInteractivePromptUnchanged(t *testing.T) {
	withHooks := firstRequest(t, measureConfig{interactive: true, settings: hooksSettings})
	noHooks := firstRequest(t, measureConfig{interactive: true})

	sys := withoutEnvironment(withHooks.System)
	for _, want := range []string{hooksBulletText, browserBulletText, promptedText, scriptFileText, "# Corrections", "# Text output (does not apply to tool calls)", "For exploratory questions"} {
		if !strings.Contains(sys, want) {
			t.Errorf("interactive prompt missing %q", want)
		}
	}
	if strings.Contains(sys, headlessFinalText) {
		t.Errorf("interactive prompt carries the headless output line")
	}
	// Without hooks the hooks bullet is the only difference.
	bulletStart := strings.Index(sys, " - "+hooksBulletText)
	bulletEnd := bulletStart + strings.IndexByte(sys[bulletStart:], '\n') + 1
	if got, want := withoutEnvironment(noHooks.System), sys[:bulletStart]+sys[bulletEnd:]; got != want {
		t.Errorf("interactive prompt without hooks differs beyond the hooks bullet")
	}
	toolsJSON, _ := json.Marshal(withHooks.Tools)
	noHooksJSON, _ := json.Marshal(noHooks.Tools)
	if string(toolsJSON) != string(noHooksJSON) {
		t.Errorf("hooks changed the interactive tool definitions")
	}
	if runtime.GOOS != "linux" {
		t.Skip("golden hashes are recorded on linux")
	}
	if got := sha(sys); got != interactiveSystemGolden {
		t.Errorf("interactive system prompt changed: sha256 %s, want %s", got, interactiveSystemGolden)
	}
	if got := sha(string(toolsJSON)); got != interactiveToolsGolden {
		t.Errorf("interactive tool definitions changed: sha256 %s, want %s", got, interactiveToolsGolden)
	}
	full := firstRequest(t, measureConfig{interactive: true, settings: hooksSettings, env: map[string]string{fullToolDescriptionsEnv: "1"}})
	fullJSON, _ := json.Marshal(full.Tools)
	if got := sha(string(fullJSON)); got != interactiveFullToolsGolden {
		t.Errorf("interactive full tool definitions changed: sha256 %s, want %s", got, interactiveFullToolsGolden)
	}
}

// Golden hashes of the same interactive prompt when `grep` in Bash runs
// ugrep (a ugrep resolves): no Grep tool, and the guidance points at `grep`.
const (
	interactiveGrepViaBashSystemGolden = "25f718567e49f002bfef732b31ac0a9ddeabeb1208a27496f53f33b455ee093a"
	interactiveGrepViaBashToolsGolden  = "0650e8bf228fecd192008e0386a252cfa23f3e1d0ee158124cc657830d96124a"
)

func TestInteractivePromptGrepViaBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shadow needs Git Bash on Windows")
	}
	bin := filepath.Join(t.TempDir(), "ugrep")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := firstRequest(t, measureConfig{interactive: true, settings: hooksSettings, env: map[string]string{"GOCODE_UGREP_PATH": bin}})
	plain := firstRequest(t, measureConfig{interactive: true, settings: hooksSettings})
	names := toolNames(req.Tools)
	if names["Grep"] || !names["Glob"] || !names["Bash"] || !toolNames(plain.Tools)["Grep"] {
		t.Fatalf("tools = %v", names)
	}
	sys := withoutEnvironment(req.System)
	if !strings.Contains(sys, "searching file contents with `grep`") || strings.Contains(sys, "(Read, Edit, Write, Glob, Grep)") {
		t.Errorf("system prompt does not point at grep:\n%s", sys)
	}
	for _, tool := range req.Tools {
		if tool.Name == "Bash" && (strings.Contains(tool.Description, "Content search: Use Grep") || strings.Contains(tool.Description, "`find`, `grep`, `cat`")) {
			t.Errorf("Bash description still steers away from grep")
		}
		if strings.Contains(tool.Description, "Grep to find") || strings.Contains(tool.Description, "Read or Grep") {
			t.Errorf("%s description still names the Grep tool", tool.Name)
		}
	}
	if runtime.GOOS != "linux" {
		t.Skip("golden hashes are recorded on linux")
	}
	toolsJSON, _ := json.Marshal(req.Tools)
	if got := sha(sys); got != interactiveGrepViaBashSystemGolden {
		t.Errorf("grep-via-Bash system prompt changed: sha256 %s, want %s", got, interactiveGrepViaBashSystemGolden)
	}
	if got := sha(string(toolsJSON)); got != interactiveGrepViaBashToolsGolden {
		t.Errorf("grep-via-Bash tool definitions changed: sha256 %s, want %s", got, interactiveGrepViaBashToolsGolden)
	}
}

func TestHeadlessSessionPrompt(t *testing.T) {
	req := firstRequest(t, measureConfig{})
	for _, gone := range []string{"# Corrections", "Assume users can't see most tool calls", "For exploratory questions", hooksBulletText, browserBulletText} {
		if strings.Contains(req.System, gone) {
			t.Errorf("-p prompt contains %q", gone)
		}
	}
	for _, want := range []string{headlessFinalText, "# Executing actions with care", "# Doing tasks", "IMPORTANT: Assist with authorized security testing", scriptFileText, promptedText} {
		if !strings.Contains(req.System, want) {
			t.Errorf("-p prompt missing %q", want)
		}
	}

	if req := firstRequest(t, measureConfig{settings: hooksSettings}); !strings.Contains(req.System, hooksBulletText) {
		t.Errorf("-p prompt with hooks configured lacks the hooks bullet")
	}
	if req := firstRequest(t, measureConfig{env: map[string]string{"GOCODE_CODE_ENABLE_CFC": "1"}}); !strings.Contains(req.System, browserBulletText) {
		t.Errorf("-p prompt with browser tools lacks the browser bullet")
	}
	if req := firstRequest(t, measureConfig{interactive: true, env: map[string]string{"GOCODE_CODE_ENABLE_CFC": "0"}}); strings.Contains(req.System, browserBulletText) {
		t.Errorf("interactive prompt without browser tools has the browser bullet")
	}
	for _, interactive := range []bool{false, true} {
		req := firstRequest(t, measureConfig{interactive: interactive, mode: toolreg.ModeBypassPermissions})
		if strings.Contains(req.System, promptedText) || strings.Contains(req.System, "do not re-attempt the exact same tool call") {
			t.Errorf("interactive=%v bypass prompt still describes approval prompts", interactive)
		}
		if !strings.Contains(req.System, "Tools are executed in a user-selected permission mode.") {
			t.Errorf("interactive=%v bypass prompt lost the permission-mode sentence", interactive)
		}
	}
}

func TestHeadlessToolSet(t *testing.T) {
	headless := firstRequest(t, measureConfig{})
	interactive := firstRequest(t, measureConfig{interactive: true})
	full := firstRequest(t, measureConfig{interactive: true, env: map[string]string{fullToolDescriptionsEnv: "1"}})
	h, i, f := toolNames(headless.Tools), toolNames(interactive.Tools), toolNames(full.Tools)
	for _, n := range []string{"Workflow", "ListAgents"} {
		if h[n] || i[n] {
			t.Errorf("%s sent up front (-p %v, interactive %v); it should be deferred", n, h[n], i[n])
		}
		if !f[n] {
			t.Errorf("with %s set, interactive does not send %s up front", fullToolDescriptionsEnv, n)
		}
	}
	for _, n := range []string{"Agent", "TodoWrite", "Read", "Grep", toolsearch.Name} {
		if !h[n] {
			t.Errorf("-p does not send %s", n)
		}
	}
	// The one-time reminder names the deferred tools.
	var reminder string
	for _, m := range headless.Messages {
		for _, b := range m.Content {
			if b.AnnouncedTools != nil {
				reminder += b.Text
			}
		}
	}
	for _, n := range []string{"Workflow", "ListAgents"} {
		if !strings.Contains(reminder, n) {
			t.Errorf("deferred-tools reminder does not name %s", n)
		}
	}
}

func TestHeadlessDeferredToolsLoadViaToolSearch(t *testing.T) {
	rt, _ := buildMeasuredSession(t, measureConfig{})
	defer rt.Close("other")
	deferred := map[string]bool{}
	for _, s := range rt.dispatcher.Schemas() {
		deferred[s.Name] = s.Deferred
	}
	for _, n := range []string{"Workflow", "ListAgents"} {
		if d, ok := deferred[n]; !ok || !d {
			t.Fatalf("%s registered=%v deferred=%v in a -p session", n, ok, d)
		}
	}
	in, _ := json.Marshal(map[string]any{"query": "select:Workflow,ListAgents"})
	res, err := rt.dispatcher.Dispatch(context.Background(), agent.ToolUse{ID: "ts", Name: toolsearch.Name, Input: in})
	if err != nil || res.IsError {
		t.Fatalf("ToolSearch failed: %v %+v", err, res)
	}
	if got := strings.Join(res.LoadedTools, ","); got != "Workflow,ListAgents" {
		t.Fatalf("ToolSearch loaded %q", got)
	}
}

func TestSubagentInheritsHeadlessVariant(t *testing.T) {
	for _, c := range []struct {
		interactive, full bool
	}{{false, false}, {true, false}, {true, true}} {
		interactive, compact := c.interactive, !c.full
		cfg := measureConfig{interactive: interactive}
		if c.full {
			cfg.env = map[string]string{fullToolDescriptionsEnv: "1"}
		}
		rt, _ := buildMeasuredSession(t, cfg)
		child := rt.childDispatcher(rt.agentTool, 1, []string{"*"})
		if child.CompactTools() != compact {
			t.Errorf("interactive=%v full=%v: child dispatcher CompactTools() = %v", interactive, c.full, child.CompactTools())
		}
		parent := map[string]string{}
		for _, s := range rt.dispatcher.Schemas() {
			parent[s.Name] = s.Description
		}
		for _, s := range child.Schemas() {
			switch s.Name {
			case "TodoWrite", "Grep", "Read", "Agent":
				if s.Description != parent[s.Name] {
					t.Errorf("interactive=%v: subagent %s description differs from the parent's", interactive, s.Name)
				}
			}
		}
		if rt.agentTool.ForChild(1, "", "").CompactDescription != compact {
			t.Errorf("interactive=%v: child Agent tool variant", interactive)
		}
		rt.Close("other")
	}
}
