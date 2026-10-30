package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/ui"
)

// TestModelListErrorText: the catalog error is a short human message, never a
// Go type name, and /login is suggested only when no key is set or the key
// was rejected.
func TestModelListErrorText(t *testing.T) {
	decodeErr := fmt.Errorf("provider/xai: decode models response: %w", errors.New("json: cannot unmarshal array into Go struct field openAIModelWire.data.pricing of type provider.listedPricing"))
	got := modelListErrorText(decodeErr, true, "/model", false)
	if strings.Contains(got, "/login") || strings.Contains(got, "openAIModelWire") || strings.Contains(got, "json:") {
		t.Fatalf("with a key: %q", got)
	}
	if !strings.Contains(got, "Could not list models") || !strings.Contains(got, "/model <id>") {
		t.Fatalf("with a key: %q", got)
	}
	if got := modelListErrorText(decodeErr, false, "/fallback", false); !strings.Contains(got, "/login") || !strings.Contains(got, "/fallback <id>") {
		t.Fatalf("without a key: %q", got)
	}
	authErr := &provider.APIError{Provider: "xai", Status: 401, Category: provider.CategoryAuth, Message: "Invalid API key for xai."}
	if got := modelListErrorText(authErr, true, "/model", false); !strings.Contains(got, "Invalid API key for xai") || !strings.Contains(got, "/login") {
		t.Fatalf("rejected key: %q", got)
	}
	if got := modelListErrorText(context.DeadlineExceeded, true, "/model", false); !strings.Contains(got, "timed out") {
		t.Fatalf("timeout: %q", got)
	}
	if got := modelListErrorText(decodeErr, true, "/model", true); !strings.Contains(got, "openAIModelWire") {
		t.Fatalf("debug detail missing: %q", got)
	}
}

// TestVersionCommandShowsRunningVersion: /version shows the same line as
// `gocode --version`.
func TestVersionCommandShowsRunningVersion(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()
	submitLine(prog, "/version")
	if !sawSystemMsg(prog, versionLine()) {
		t.Fatal("/version did not show the running version")
	}
	if !strings.HasSuffix(versionLine(), " (gocode)") {
		t.Fatalf("versionLine = %q", versionLine())
	}
}

// TestStatusShowsModelAndPermissionMode: /status always has a Model line and
// the permission mode.
func TestStatusShowsModelAndPermissionMode(t *testing.T) {
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	conv.Mode = "acceptEdits"
	out := cmdInfo{conv: conv, print: &printSession{Provider: providerOpenRouter, Model: "m-1"}}.statusText()
	if !strings.Contains(out, "Model:         m-1") || !strings.Contains(out, "Permission mode: acceptEdits") {
		t.Fatalf("status:\n%s", out)
	}
	out = cmdInfo{conv: conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{}), print: &printSession{Provider: providerOpenRouter}}.statusText()
	if !strings.Contains(out, "Model:         not set") || !strings.Contains(out, "Permission mode: default") {
		t.Fatalf("status without model:\n%s", out)
	}
}

// TestDoctorUpdateRepoInformational: an unset update repo is not a failure.
func TestDoctorUpdateRepoInformational(t *testing.T) {
	env, _, _ := dialogEnv(t.TempDir(), nil)
	out := cmdInfo{conv: conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{}), env: env, print: &printSession{Provider: providerOpenRouter}}.doctorText()
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Update repo") && strings.Contains(ln, "✗") {
			t.Fatalf("unset update repo reported as a failure: %q", ln)
		}
	}
	if !strings.Contains(out, "Update repo: not configured") {
		t.Fatalf("doctor:\n%s", out)
	}
}

// TestUsageTextNoDuplicateLine: with no usage the zero totals appear once;
// code changes recorded on the ledger are shown.
func TestUsageTextNoDuplicateLine(t *testing.T) {
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	out := cmdInfo{conv: conv}.usageText()
	if strings.Contains(out, "Total tokens:") || strings.Count(out, "0 input, 0 output") != 1 {
		t.Fatalf("usage without usage:\n%s", out)
	}
	conv.Cost.AddLines(7, 2)
	conv.Cost.AddTurn("m", cost.Usage{InputTokens: 10, OutputTokens: 5})
	out = cmdInfo{conv: conv}.usageText()
	if !strings.Contains(out, "7 lines added, 2 lines removed") || !strings.Contains(out, "Total tokens:") {
		t.Fatalf("usage:\n%s", out)
	}
}

// TestCompactionReportCountsToolOutput: the freed estimate covers tool calls
// and results, not only text.
func TestCompactionReportCountsToolOutput(t *testing.T) {
	big := strings.Repeat("x", 8000)
	pre := []agent.Message{
		agent.UserMessage(agent.TextBlock("read it")),
		agent.AssistantMessage(agent.ToolUseBlock("t1", "Read", []byte(`{"file_path":"/a"}`))),
		agent.UserMessage(agent.ToolResultBlock("t1", big, false)),
		agent.AssistantMessage(agent.TextBlock("done")),
	}
	preTokens, preMsgs := compactionSnapshot(pre)
	if preTokens < 2000 {
		t.Fatalf("pre estimate = %d", preTokens)
	}
	post := []agent.Message{agent.UserMessage(agent.TextBlock("This session is being continued from a previous conversation. Summary: read a file."))}
	got := compactionReport(conversation.Result{Compacted: true, History: post}, preTokens, preMsgs)
	if strings.Contains(got, "freed ~0 tokens") || !strings.Contains(got, "summarized 4 messages") {
		t.Fatalf("report = %q", got)
	}
}

// TestProviderPickerMarksCurrentAndKeys: the picker marks the current
// provider (and starts on it) and the providers with a key.
func TestProviderPickerMarksCurrentAndKeys(t *testing.T) {
	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env", "XAI_API_KEY": "xai-key"})
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, "xai", "grok-4", progCh)
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	defer prog.Quit()
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/provider")
	var dlg *ui.Dialog
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		if ok && d.Dialog != nil && d.Dialog.Kind == ui.DialogList {
			dlg = d.Dialog
			return true
		}
		return false
	}, 2*time.Second) {
		t.Fatal("/provider did not open the picker")
	}
	details := map[string]string{}
	var values []string
	for _, it := range dlg.Items {
		details[it.Value] = it.Detail
		values = append(values, it.Value)
	}
	if !strings.HasPrefix(details["xai"], "current · key set") {
		t.Errorf("xai detail = %q", details["xai"])
	}
	if !strings.Contains(details["openrouter"], "key set") || strings.Contains(details["openrouter"], "current") {
		t.Errorf("openrouter detail = %q", details["openrouter"])
	}
	if strings.Contains(details["groq"], "key set") {
		t.Errorf("groq detail = %q", details["groq"])
	}
	if i := slices.Index(values, "xai"); dlg.Selected != i {
		t.Errorf("cursor on %d, want %d (xai)", dlg.Selected, i)
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEscape})
}

// TestClearResetsTranscriptView: /clear removes the previous transcript from
// the screen.
func TestClearResetsTranscriptView(t *testing.T) {
	prog := startCommandSession(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	defer prog.Quit()
	submitLine(prog, "/clear")
	if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.ResetTranscriptMsg); return ok }, 2*time.Second) {
		t.Fatal("/clear did not reset the transcript view")
	}
}

// TestWebSearchNotOfferedWithoutBackend: without a search key WebSearch is
// neither sent nor findable through ToolSearch.
func TestWebSearchNotOfferedWithoutBackend(t *testing.T) {
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	p := newFakeProvider(t, fakeTurn{text: "ok", in: 1, out: 1})
	f := headlessFixture(t, p)
	f.vars["ENABLE_TOOL_SEARCH"] = "false"
	if code, _, errOut := f.run(t, "-p", "hi"); code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	tools := p.toolsOf(0)
	if slices.Contains(tools, "WebSearch") || !slices.Contains(tools, "WebFetch") {
		t.Fatalf("tools = %v", tools)
	}
}
