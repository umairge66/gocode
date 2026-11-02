package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// dialogEnv builds an Env with a controllable environment and a fixed cwd/home,
// so the key resolver + auth-file path point into a scratch tree (no real HOME).
func dialogEnv(home string, vars map[string]string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	get := func(k string) string {
		if k == "HOME" {
			return home
		}
		if vars != nil {
			if v, ok := vars[k]; ok {
				return v
			}
		}
		return ""
	}
	env := &Env{
		Getenv: get,
		Setenv: func(string, string) {},
		Stdout: &out,
		Stderr: &errb,
		Cwd:    func() (string, error) { return home, nil },
	}
	return env, &out, &errb
}

// msgText extracts a human-readable string from the transcript/banner messages a
// flow can push, used to assert a secret never leaks into any rendered message.
func msgText(m tea.Msg) string {
	switch t := m.(type) {
	case ui.AppendSystemMsg:
		return t.Text
	case ui.AppendUserMsg:
		return t.Text
	case ui.AppendAssistantMsg:
		return t.Text
	case ui.AppendErrorMsg:
		return t.Text
	case ui.SetStatusMsg:
		return t.Status.Label
	case ui.SetStatusLineMsg:
		return t.Line
	case ui.SetBannerMsg:
		if t.Banner != nil {
			return strings.Join(t.Banner.Lines(), "\n")
		}
	case ui.OpenDialogMsg:
		if t.Dialog != nil {
			return t.Dialog.Title + "\n" + strings.Join(t.Dialog.Body, "\n")
		}
	}
	return ""
}

// lastBanner returns the most recently sent banner, if any.
func (p *fakeProgram) lastBanner() (*ui.Banner, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b *ui.Banner
	var ok bool
	for _, m := range p.sent {
		if sb, is := m.(ui.SetBannerMsg); is {
			b, ok = sb.Banner, true
		}
	}
	return b, ok
}

// typeInto presses each rune of s as a KeyRunes keystroke.
func typeInto(p *fakeProgram, s string) {
	for _, r := range s {
		p.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// submitLine types s into the composer and presses Enter.
func submitLine(p *fakeProgram, s string) {
	typeInto(p, s)
	p.press(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// newDialogSession builds a fake-backed interactive session for the dialog flows.
func newDialogSession(env *Env, prov, model string, progCh chan *fakeProgram) *interactiveSession {
	return &interactiveSession{
		print: &printSession{
			Mode:      toolreg.ModeDefault,
			Provider:  prov,
			Model:     model,
			newCaller: func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
		},
		newProgram: func(m *ui.Model, _ *Env) teaProgram {
			p := newFakeProgram(m)
			progCh <- p
			return p
		},
		newConversation: defaultNewConversation,
		sessionID:       "dialog-test",
	}
}

// TestInteractiveSession_BannerWarnsWhenNoKey asserts the welcome banner is sent
// at startup with the no-key warning (and the footer model) when the active
// provider has no resolved key, and with no warning when a key is present.
func TestInteractiveSession_BannerWarnsWhenNoKey(t *testing.T) {
	t.Run("no key -> warning", func(t *testing.T) {
		env, _, _ := dialogEnv(t.TempDir(), nil)
		progCh := make(chan *fakeProgram, 1)
		sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
		go func() { runInteractiveSession(env, sess, "") }()
		prog := mustProgram(t, progCh)

		if !prog.waitFor(func(m tea.Msg) bool {
			b, ok := m.(ui.SetBannerMsg)
			return ok && b.Banner != nil && strings.Contains(b.Banner.Warning, "No API key for openrouter") && strings.Contains(b.Banner.Warning, "/login")
		}, 2*time.Second) {
			t.Fatal("banner with no-key warning was not sent at startup")
		}
		if !prog.waitFor(func(m tea.Msg) bool {
			f, ok := m.(ui.SetFooterModelMsg)
			return ok && f.Model == "my-model"
		}, 2*time.Second) {
			t.Fatal("footer model was not set at startup")
		}
		prog.Quit()
	})

	t.Run("key present -> no warning", func(t *testing.T) {
		env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
		progCh := make(chan *fakeProgram, 1)
		sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
		go func() { runInteractiveSession(env, sess, "") }()
		prog := mustProgram(t, progCh)

		// Wait for the startup banner, then assert it carried no warning.
		if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second) {
			t.Fatal("startup banner was not sent")
		}
		b, ok := prog.lastBanner()
		if !ok || b.Warning != "" {
			t.Fatalf("banner warning = %q, want empty (key present)", b.Warning)
		}
		prog.Quit()
	})
}

// TestInteractiveSession_ModelPicker drives /model: a fake ListModels fills the
// picker, the highlighted id is applied, and the footer updates. It also checks
// /model <id> still works unchanged.
func TestInteractiveSession_ModelPicker(t *testing.T) {
	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "start-model", progCh)
	sess.listModels = func(context.Context) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{
			{ID: "model-a", Name: "Model A", ContextLength: 1000, Description: "first"},
			{ID: "model-b", Name: "Model B"},
		}, nil
	}

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	// Let startup finish.
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	// /model with no arg opens the picker.
	submitLine(prog, "/model")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && d.Dialog.Kind == ui.DialogList && d.Dialog.Title == "Select model" && len(d.Dialog.Items) == 2
	}, 2*time.Second) {
		t.Fatal("/model did not open the model picker from ListModels")
	}

	// Select the highlighted (first) row.
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		f, ok := m.(ui.SetFooterModelMsg)
		return ok && f.Model == "model-a"
	}, 2*time.Second) {
		t.Fatal("picking a model did not update the footer")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Model set to model-a")
	}, 2*time.Second) {
		t.Fatal("picking a model did not echo the confirmation line")
	}

	// /model <id> (explicit) still works unchanged.
	submitLine(prog, "/model model-b")
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Model set to model-b")
	}, 2*time.Second) {
		t.Fatal("/model <id> did not apply the explicit model")
	}
	prog.Quit()
}

// TestInteractiveSession_ModelPickerErrorGuides asserts a ListModels error shows
// actionable guidance (set a key / pass /model <id>) and opens no picker.
func TestInteractiveSession_ModelPickerErrorGuides(t *testing.T) {
	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "start-model", progCh)
	sess.listModels = func(context.Context) ([]provider.ModelInfo, error) {
		return nil, context.DeadlineExceeded
	}
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/model")
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Could not list models") && strings.Contains(s.Text, "/model <id>")
	}, 2*time.Second) {
		t.Fatal("a ListModels error did not surface guidance")
	}
	prog.Quit()
}

// TestInteractiveSession_LoginSavesKey drives /login: the masked prompt's value
// is saved to a temp auth.json (0600), a fresh resolver then finds it, the banner
// warning clears, and the key never leaks into any rendered message.
func TestInteractiveSession_LoginSavesKey(t *testing.T) {
	home := t.TempDir()
	// Active provider openrouter has no key at start, so the banner warns.
	env, _, _ := dialogEnv(home, nil)
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	if !prog.waitFor(func(m tea.Msg) bool {
		b, ok := m.(ui.SetBannerMsg)
		return ok && b.Banner != nil && strings.Contains(b.Banner.Warning, "openrouter")
	}, 2*time.Second) {
		t.Fatal("startup banner did not warn about the missing key")
	}

	const secret = "sk-or-secret-123"
	submitLine(prog, "/login openrouter")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && d.Dialog.Kind == ui.DialogSecret && strings.Contains(d.Dialog.Title, "OPENROUTER")
	}, 2*time.Second) {
		t.Fatal("/login did not open the masked key prompt")
	}

	// Enter the key into the masked dialog and submit.
	typeInto(prog, secret)
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Saved OPENROUTER key")
	}, 2*time.Second) {
		t.Fatal("/login did not confirm the saved key")
	}

	// The key was persisted at 0600 to the default auth file.
	authPath := filepath.Join(home, ".config", "gocode", "auth.json")
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("auth file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 && runtime.GOOS != "windows" {
		t.Fatalf("auth file perm = %o, want 0600", perm)
	}
	// A fresh resolver (same precedence as the banner / auth status) finds it.
	got, _, ok := newKeyResolver(env).ResolveProviderKey(auth.ProviderOpenRouter)
	if !ok || got != secret {
		t.Fatalf("resolver did not find the saved key: ok=%v got=%q", ok, got)
	}

	// The banner warning cleared after the save.
	if !prog.waitFor(func(m tea.Msg) bool {
		b, ok := m.(ui.SetBannerMsg)
		return ok && b.Banner != nil && b.Banner.Warning == ""
	}, 2*time.Second) {
		t.Fatal("banner warning did not clear after /login")
	}

	// The secret must never appear in any rendered message.
	prog.mu.Lock()
	sent := append([]tea.Msg(nil), prog.sent...)
	prog.mu.Unlock()
	for _, m := range sent {
		if strings.Contains(msgText(m), secret) {
			t.Fatalf("the API key leaked into a rendered message: %T", m)
		}
	}
	prog.Quit()
}

// TestInteractiveSession_ProviderSwitchPromptsKey drives /provider: switching to
// a provider with no key updates the banner (new provider + warning) and chains
// into the masked key prompt.
func TestInteractiveSession_ProviderSwitchPromptsKey(t *testing.T) {
	// openrouter has a key; switch to anthropic which has none.
	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/provider anthropic")

	// The banner now names anthropic and warns about the missing key.
	if !prog.waitFor(func(m tea.Msg) bool {
		b, ok := m.(ui.SetBannerMsg)
		return ok && b.Banner != nil && b.Banner.Provider == "anthropic" && strings.Contains(b.Banner.Warning, "anthropic")
	}, 2*time.Second) {
		t.Fatal("/provider did not re-render the banner for the new provider")
	}
	// It chains into the key prompt for the new provider.
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && d.Dialog.Kind == ui.DialogSecret && strings.Contains(d.Dialog.Title, "ANTHROPIC")
	}, 2*time.Second) {
		t.Fatal("/provider did not chain into the key prompt for a keyless provider")
	}
	// The footer tracks the new provider's default model.
	if !prog.waitFor(func(m tea.Msg) bool {
		f, ok := m.(ui.SetFooterModelMsg)
		return ok && f.Model == ""
	}, 2*time.Second) {
		t.Fatal("/provider did not update the footer model")
	}
	prog.Quit()
}

// TestInteractiveSession_MCPStatus asserts /mcp surfaces the MCP server status
// (here: none configured) as a system message.
func TestInteractiveSession_MCPStatus(t *testing.T) {
	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/mcp")
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "MCP servers")
	}, 2*time.Second) {
		t.Fatal("/mcp did not surface the MCP server status")
	}
	prog.Quit()
}

// TestInteractiveSession_MCPApproval drives the project-MCP-server approval
// flow: a project .mcp.json server shows as pending in /mcp, the approval dialog
// opens, confirming it persists enabledMcpjsonServers to the local settings file
// and connects the server live (no restart).
func TestInteractiveSession_MCPApproval(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".mcp.json"), []byte(`{"mcpServers":{"proj":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	mem := echoServer()
	sess.print.mcpTransport = mem.factory()

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	// Startup notice tells the user project servers need approval.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "need approval")
	}, 2*time.Second) {
		t.Fatal("no startup pending-approval notice")
	}

	// /mcp opens the server picker: a ListDialog listing the pending project
	// server (as a skipped row) and a leading "Approve project servers…" entry.
	submitLine(prog, "/mcp")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		if !ok || d.Dialog == nil || d.Dialog.Kind != ui.DialogList || d.Dialog.Title != "MCP servers" {
			return false
		}
		var sawApprove, sawProj bool
		for _, it := range d.Dialog.Items {
			if it.Value == mcpApproveEntryValue {
				sawApprove = true
			}
			if strings.Contains(it.Label, "proj") {
				sawProj = true
			}
		}
		return sawApprove && sawProj
	}, 2*time.Second) {
		t.Fatal("/mcp did not open the server picker listing the pending project server")
	}
	// Select the leading "Approve project servers…" entry (highlighted first row)
	// → the approval dialog opens.
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && strings.Contains(d.Dialog.Title, "this project")
	}, 2*time.Second) {
		t.Fatal("approval dialog did not open")
	}

	// Confirm: the pending server is pre-marked, so Enter approves it.
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Approved") && strings.Contains(s.Text, "proj: connected")
	}, 3*time.Second) {
		t.Fatal("approval did not connect the server live")
	}
	prog.Quit()

	// Persisted to .gocode/settings.local.json for next time.
	data, _ := os.ReadFile(filepath.Join(home, ".gocode", "settings.local.json"))
	if !strings.Contains(string(data), "enabledMcpjsonServers") || !strings.Contains(string(data), "proj") {
		t.Fatalf("approval not persisted to local settings: %s", data)
	}

	// A fresh gather now treats the server as approved (no longer pending).
	g := gatherMCP(env, sess.print)
	if _, ok := g.Servers["proj"]; !ok {
		t.Fatalf("approved server still not in gathered set: %+v", g.Skipped)
	}
}

// TestInteractiveSession_MCPApproveAll approves ALL via the sentinel option,
// which persists enableAllProjectMcpServers instead of per-server names.
func TestInteractiveSession_MCPApproveAll(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".mcp.json"), []byte(`{"mcpServers":{"proj":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	sess.print.mcpTransport = echoServer().factory()

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	// /mcp opens the server picker; select its leading "Approve project servers…"
	// entry to reach the approval dialog.
	submitLine(prog, "/mcp")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && d.Dialog.Kind == ui.DialogList && d.Dialog.Title == "MCP servers"
	}, 2*time.Second) {
		t.Fatal("/mcp did not open the server picker")
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && strings.Contains(d.Dialog.Title, "this project")
	}, 2*time.Second) {
		t.Fatal("approval dialog did not open")
	}
	// Toggle the "approve ALL" sentinel (first option, highlighted) and confirm.
	prog.press(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Approved")
	}, 3*time.Second) {
		t.Fatal("approve-all did not report approval")
	}
	prog.Quit()

	data, _ := os.ReadFile(filepath.Join(home, ".gocode", "settings.local.json"))
	if !strings.Contains(string(data), "enableAllProjectMcpServers") {
		t.Fatalf("approve-all did not persist enableAllProjectMcpServers: %s", data)
	}
}

// TestInteractiveSession_MCPPickerShowsTools sets up a pre-approved project
// server that connects at startup, then drives /mcp: the server picker lists it
// with a "connected · N tools" detail, and selecting it emits a system message
// naming the server's tools.
func TestInteractiveSession_MCPPickerShowsTools(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".mcp.json"), []byte(`{"mcpServers":{"proj":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-approve all project servers so "proj" connects at startup (no prompt).
	if err := os.MkdirAll(filepath.Join(home, ".gocode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gocode", "settings.local.json"), []byte(`{"enableAllProjectMcpServers":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	sess.print.mcpTransport = echoServer().factory()

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	// /mcp opens the picker listing the connected server with a tool-count detail.
	submitLine(prog, "/mcp")
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		if !ok || d.Dialog == nil || d.Dialog.Kind != ui.DialogList || d.Dialog.Title != "MCP servers" {
			return false
		}
		for _, it := range d.Dialog.Items {
			if it.Label == "proj" && strings.Contains(it.Detail, "connected") && strings.Contains(it.Detail, "tool") {
				return true
			}
		}
		return false
	}, 3*time.Second) {
		t.Fatal("/mcp did not list the connected server with a tool-count detail")
	}
	// Select the server row (highlighted only server row) → tools system message.
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "proj: connected") && strings.Contains(s.Text, "echo")
	}, 3*time.Second) {
		t.Fatal("selecting a server did not emit its tool names")
	}
	prog.Quit()
}

// TestInteractiveSession_MCPEmpty asserts the /mcp empty state: with nothing
// configured it emits a single "No MCP servers configured." line and opens no
// dialog.
func TestInteractiveSession_MCPEmpty(t *testing.T) {
	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)

	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/mcp")
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && s.Text == "No MCP servers configured."
	}, 2*time.Second) {
		t.Fatal("/mcp empty state did not emit the no-servers line")
	}
	// No dialog should have opened for the empty state.
	if prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.OpenDialogMsg); return ok }, 300*time.Millisecond) {
		t.Fatal("/mcp empty state should not open a dialog")
	}
	prog.Quit()
}

// TestPrintLoginPrintsGuidance asserts that in -p mode, /login (and /provider)
// print actionable guidance (no masked input without a TTY).
func TestPrintLoginPrintsGuidance(t *testing.T) {
	for _, c := range []struct {
		line string
		want string
	}{
		{"/login", "interactive terminal"},
		{"/login anthropic", "ANTHROPIC_API_KEY"},
		{"/provider", "GOCODE_PROVIDER"},
	} {
		env, out, _ := dialogEnv(t.TempDir(), nil)
		s := &printSession{
			Mode:      toolreg.ModeDefault,
			Provider:  providerOpenRouter,
			newCaller: func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
		}
		if code := runPrintSession(env, s, c.line); code != 0 {
			t.Fatalf("%q: exit code = %d, want 0", c.line, code)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Fatalf("%q: stdout = %q, want to contain %q", c.line, out.String(), c.want)
		}
	}
}

// TestInteractiveSession_BannerGreetingAndVersion asserts the start-screen
// banner carries the running version, provider, model and cwd, plus the
// greeting picked once for the session by chooseGreeting.
func TestInteractiveSession_BannerGreetingAndVersion(t *testing.T) {
	savedGreeting, savedVersion := chooseGreeting, updateCurrentVersion
	t.Cleanup(func() { chooseGreeting, updateCurrentVersion = savedGreeting, savedVersion })
	var calls atomic.Int32
	chooseGreeting = func() string { calls.Add(1); return "Let's ship something." }
	updateCurrentVersion = func() string { return "dev" }

	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	defer prog.Quit()

	if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second) {
		t.Fatal("startup banner was not sent")
	}
	b, ok := prog.lastBanner()
	if !ok || b == nil {
		t.Fatal("no banner")
	}
	if b.Greeting != "Let's ship something." {
		t.Errorf("greeting = %q, want the injected one", b.Greeting)
	}
	if b.Version != "dev" || b.Provider != providerOpenRouter || !strings.HasPrefix(b.Model, "my-model") || b.Cwd == "" {
		t.Errorf("banner = %+v, want version dev, provider %s, model my-model and a cwd", *b, providerOpenRouter)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("chooseGreeting called %d times, want once per session", n)
	}
}

func TestChooseGreetingPicksFromTheList(t *testing.T) {
	for range 20 {
		g := chooseGreeting()
		if !slices.Contains(ui.Greetings, g) {
			t.Fatalf("chooseGreeting() = %q, not one of ui.Greetings", g)
		}
	}
}

// /login on a provider that already has a key says where the key comes from
// and offers what can be done there; the key itself is never shown.
func TestExistingKeyDialog(t *testing.T) {
	values := func(d *ui.Dialog) []string {
		var v []string
		for _, o := range d.Options {
			v = append(v, o.Value)
		}
		return v
	}
	saved := existingKeyDialog("openrouter", "auth.json", "~/.config/gocode/auth.json")
	if saved.Title != "OPENROUTER key is already set" || !strings.Contains(strings.Join(saved.Body, " "), "~/.config/gocode/auth.json") ||
		strings.Join(values(saved), ",") != "replace,remove,cancel" {
		t.Errorf("saved key dialog = %+v", saved)
	}
	env := existingKeyDialog("openrouter", "OPENROUTER_API_KEY", "x")
	if body := strings.Join(env.Body, " "); !strings.Contains(body, "OPENROUTER_API_KEY environment variable") || !strings.Contains(body, "shell profile") ||
		strings.Join(values(env), ",") != "replace,cancel" {
		t.Errorf("env key dialog = %+v", env)
	}
}
