package cli

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/ui"
)

// sawSystemMsg reports whether a system message containing every substring was
// pushed to the program within the timeout.
func sawSystemMsg(prog *fakeProgram, substrs ...string) bool {
	return prog.waitFor(func(m tea.Msg) bool {
		// Command output is a transcript notice, or (multi-line reports) the
		// modern style's info panel.
		var text string
		switch t := m.(type) {
		case ui.AppendSystemMsg:
			text = t.Text
		case ui.ShowInfoPanelMsg:
			text = t.Text
		default:
			return false
		}
		for _, s := range substrs {
			if !strings.Contains(text, s) {
				return false
			}
		}
		return true
	}, 2*time.Second)
}

// startCommandSession boots a fake-backed interactive session with the given
// provider key present, returning the driving fake program.
func startCommandSession(t *testing.T, vars map[string]string) *fakeProgram {
	t.Helper()
	env, _, _ := dialogEnv(t.TempDir(), vars)
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)
	return prog
}

// TestStatusCommandRendersContent drives /status and asserts the status content
// lists the expected fields (version, provider, model, key source, MCP count).
func TestStatusCommandRendersContent(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()

	submitLine(prog, "/status")
	if !sawSystemMsg(prog, "gocode status", "Provider:", "openrouter", "Model:", "API key:", "MCP servers:") {
		t.Fatal("/status did not render the expected status content")
	}
	// The key SOURCE is shown, never the key itself.
	if !sawSystemMsg(prog, "source: OPENROUTER_API_KEY") {
		t.Fatal("/status did not report the key source")
	}
}

// TestContextCommandRendersBreakdown drives /context and asserts the (approximate)
// context-usage breakdown replaces the old stub.
func TestContextCommandRendersBreakdown(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()

	submitLine(prog, "/context")
	if !sawSystemMsg(prog, "Context usage", "System prompt:", "Tools:", "Conversation:", "Context used:") {
		t.Fatal("/context did not render the usage breakdown")
	}
	if sawSystemMsg(prog, "Context usage is reported by the host.") {
		t.Fatal("/context still shows the old stub")
	}
}

// TestUsageCommandRendersTotals drives /usage and asserts the live cost report
// (the /cost layout) replaces the stub text.
func TestUsageCommandRendersTotals(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()

	submitLine(prog, "/usage")
	if !sawSystemMsg(prog, "Session usage", "Total cost:", "Total duration", "Assistant turns:") {
		t.Fatal("/usage did not render the cost report")
	}
	if sawSystemMsg(prog, builtincmds.UsageStubText) {
		t.Fatal("/usage still shows the old stub")
	}
}

// TestUsageTextRealTotals seeds the session ledger with known tokens and asserts
// usageText renders those real numbers (not the stub) for the shared renderer
// behind both the interactive system message and the -p stdout path.
func TestUsageTextRealTotals(t *testing.T) {
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	conv.Cost.AddTurn("gocode-opus-4-8", cost.Usage{InputTokens: 1234, OutputTokens: 567})
	conv.History = []agent.Message{{Role: agent.RoleUser}, {Role: agent.RoleAssistant}}

	out := cmdInfo{conv: conv}.usageText()
	for _, want := range []string{"Session usage", "Total cost:", "1234 input", "567 output", "Assistant turns:       1", "gocode-opus-4-8"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usageText missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, builtincmds.UsageStubText) {
		t.Fatalf("usageText still shows the stub:\n%s", out)
	}
}

// TestConfigCommandRendersSettings drives /config show and asserts the
// effective settings (provider, model, theme, permission mode, update repo)
// plus how to change them are shown.
func TestConfigCommandRendersSettings(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()

	submitLine(prog, "/config show")
	if !sawSystemMsg(prog, "gocode configuration", "Provider:", "Model:", "Theme:", "Permission mode:", "Update repo:") {
		t.Fatal("/config show did not render the effective settings")
	}
	if !sawSystemMsg(prog, "change: /provider, --provider or GOCODE_PROVIDER", "Run /config to change these") {
		t.Fatal("/config show does not point at /provider and /config")
	}
}

// TestDoctorCommandRendersDiagnostics drives /doctor and asserts the pass/fail
// diagnostic lines (version, OS/arch, API key, MCP, update repo).
func TestDoctorCommandRendersDiagnostics(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()

	submitLine(prog, "/doctor")
	if !sawSystemMsg(prog, "gocode doctor", "Version:", "OS/arch:", "API key for openrouter:", "MCP servers:", "Update repo:") {
		t.Fatal("/doctor did not render the diagnostics")
	}
	// The current provider has a key, so the API-key check passes.
	if !sawSystemMsg(prog, "API key for openrouter: present") {
		t.Fatal("/doctor did not report the API key as present")
	}
}

// TestProviderCommandListsAndSwitches drives /provider with no argument: it shows
// the current provider, opens a provider picker listing the supported providers,
// and on selection switches the active provider live (a key is present for the
// target, so no key prompt follows).
func TestProviderCommandListsAndSwitches(t *testing.T) {
	// Both providers have keys, so switching is a live switch (no key prompt).
	prog := startCommandSession(t, map[string]string{
		"OPENROUTER_API_KEY": "sk-or-env",
		"ANTHROPIC_API_KEY":  "sk-ant-env",
	})
	defer prog.Quit()

	submitLine(prog, "/provider")

	// It echoes the current provider.
	if !sawSystemMsg(prog, "Current provider: openrouter") {
		t.Fatal("/provider did not show the current provider")
	}
	// It opens a provider picker listing the supported providers.
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		if !ok || d.Dialog == nil || d.Dialog.Kind != ui.DialogList {
			return false
		}
		var seen int
		for _, it := range d.Dialog.Items {
			switch it.Value {
			case providerOpenRouter, providerAnthropic, providerOpenAI:
				seen++
			}
		}
		return seen == 3
	}, 2*time.Second) {
		t.Fatal("/provider did not open a picker listing the three providers")
	}

	// Select "anthropic" in the picker and confirm.
	submitLine(prog, "anthropic")

	// A live switch: the active provider becomes anthropic (no key prompt).
	if !sawSystemMsg(prog, "Provider set to anthropic") {
		t.Fatal("/provider did not perform a live switch to anthropic")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		f, ok := m.(ui.SetFooterModelMsg)
		return ok && f.Model == ""
	}, 2*time.Second) {
		t.Fatal("/provider live switch did not update the footer model")
	}
}

// TestInfoCommandsOpenPanel asserts informational commands send their output to
// the info panel, not into the transcript.
func TestInfoCommandsOpenPanel(t *testing.T) {
	for _, cmd := range []string{"/help", "/status", "/version", "/config show"} {
		t.Run(cmd, func(t *testing.T) {
			prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
			defer prog.Quit()
			submitLine(prog, cmd)
			if !prog.waitFor(func(m tea.Msg) bool {
				p, ok := m.(ui.ShowInfoPanelMsg)
				return ok && strings.TrimSpace(p.Text) != ""
			}, 2*time.Second) {
				t.Fatalf("%s did not open the info panel", cmd)
			}
		})
	}
}
