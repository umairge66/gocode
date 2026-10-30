package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/web"
)

// TestRunPrintSession_AdvertisesDispatcherTools inspects the provider request
// body of a -p run (fake transport, no network) and asserts every dispatchable
// tool — including Read/Edit/MultiEdit/WebFetch/WebSearch — is advertised,
// while the tools a non-interactive session cannot use (AskUserQuestion, and
// the browser tools without --chrome) are not.
func TestRunPrintSession_AdvertisesDispatcherTools(t *testing.T) {
	env, _, errb := newTestEnv()
	doer := &fakeDoer{body: openRouterReplyJSON}
	s := &printSession{
		Model:     testModel,
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		HTTP:      doer,
		APIKey:    "test-key",
		newCaller: defaultNewCaller,
	}
	if code := runPrintSession(env, s, "hi"); code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, errb.String())
	}
	var body struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(doer.gotBody, &body); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, doer.gotBody)
	}
	got := map[string]bool{}
	for _, tl := range body.Tools {
		got[tl.Function.Name] = true
	}
	for _, want := range []string{"Bash", "Write", "Glob", "Grep", "Read", "Edit", "TodoWrite", "ToolSearch"} {
		if !got[want] {
			t.Errorf("request did not advertise %s (got %v)", want, got)
		}
	}
	// Deferred tools are loaded on demand with ToolSearch.
	for _, deferred := range []string{"NotebookEdit", "MultiEdit", "WebFetch", "WebSearch"} {
		if got[deferred] {
			t.Errorf("deferred tool %s advertised before it was loaded (got %v)", deferred, got)
		}
	}
	if got["Diff"] || got["Mods"] {
		t.Errorf("non-callable Diff/Mods advertised: %v", got)
	}
	for _, unwanted := range []string{"AskUserQuestion", "navigate", "computer", "screenshot", "browser_batch"} {
		if got[unwanted] {
			t.Errorf("non-interactive request advertised %s", unwanted)
		}
	}
}

// summaryCaller records the request it receives and returns a fixed reply.
type summaryCaller struct {
	req   agent.ModelRequest
	reply string
	err   error
}

func (r *summaryCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	r.req = req
	if r.err != nil {
		return agent.ModelResponse{}, r.err
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock(r.reply)), StopReason: agent.StopEndTurn}, nil
}

// TestWebFetchSummarizerUsesSessionCaller: the summarizer sends
// BuildApplyPrompt(content, prompt) as the only user message, with no tools,
// through the session's ModelCaller and returns the assistant text.
func TestWebFetchSummarizerUsesSessionCaller(t *testing.T) {
	rc := &summaryCaller{reply: "  the answer  "}
	sum := newWebFetchSummarizer(rc)
	out, err := sum.Summarize(context.Background(), "PAGE TEXT", "what?")
	if err != nil || out != "the answer" {
		t.Fatalf("Summarize = %q, %v", out, err)
	}
	if len(rc.req.Tools) != 0 || rc.req.System != "" {
		t.Fatalf("summarizer request must carry no tools/system: %+v", rc.req)
	}
	if len(rc.req.Messages) != 1 || rc.req.Messages[0].Role != agent.RoleUser ||
		rc.req.Messages[0].Content[0].Text != web.BuildApplyPrompt("PAGE TEXT", "what?") {
		t.Fatalf("summarizer messages = %+v", rc.req.Messages)
	}

	rc.err = errors.New("boom")
	if _, err := sum.Summarize(context.Background(), "x", "y"); err == nil {
		t.Fatal("caller error not propagated")
	}
	if newWebFetchSummarizer(nil) != nil {
		t.Fatal("nil caller should yield nil summarizer")
	}
}

// TestSessionDispatcherWiresSummarizer: the session dispatcher carries the
// provider-bound summarizer and advertises the same schemas it can run.
func TestSessionDispatcherWiresSummarizer(t *testing.T) {
	env, _, _ := newTestEnv()
	rc := &summaryCaller{reply: "ok"}
	d := newSessionDispatcher(toolreg.ModeDefault, env, rc)
	if d.WebFetchSummarizer == nil {
		t.Fatal("WebFetch summarizer not wired")
	}
	if _, err := d.WebFetchSummarizer.Summarize(context.Background(), "c", "p"); err != nil || len(rc.req.Messages) != 1 {
		t.Fatalf("summarizer did not reach the session caller: %v %+v", err, rc.req)
	}
	names := map[string]bool{}
	for _, s := range d.Schemas() {
		names[s.Name] = true
	}
	if !names["Read"] || !names["WebFetch"] {
		t.Fatalf("schemas = %v", names)
	}
}

// TestPrintVersionReportsUpdaterVersion: --version reports the same version
// `gocode update` uses (updater.CurrentVersion via the updateCurrentVersion seam).
func TestPrintVersionReportsUpdaterVersion(t *testing.T) {
	saved := updateCurrentVersion
	t.Cleanup(func() { updateCurrentVersion = saved })
	updateCurrentVersion = func() string { return "9.8.7" }

	env, out, _ := newTestEnv()
	if code := printVersion(env, false); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := strings.TrimSpace(out.String()); got != "9.8.7 (gocode)" {
		t.Fatalf("--version = %q, want %q", got, "9.8.7 (gocode)")
	}

	// Default (unstamped) build: "dev", never a hardcoded literal.
	updateCurrentVersion = saved
	out.Reset()
	printVersion(env, false)
	if got := strings.TrimSpace(out.String()); strings.Contains(got, "2.1.306") || got != Build.Version+" (gocode)" {
		t.Fatalf("--version = %q, want %q", got, Build.Version+" (gocode)")
	}
}
