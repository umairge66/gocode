package toolrun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/automode"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
)

// A Bash command that will run sandboxed with autoAllowBashIfSandboxed is
// allowed by the Bash permission check before auto mode's classifier; one that
// will not run sandboxed is classified.
func TestAutoModeSandboxedBashSkipsClassifier(t *testing.T) {
	skipOnWindows(t)
	c := &countingClassifier{res: automode.Result{Verdict: automode.VerdictAllow}}
	sb := &fakeSandbox{enabled: true, autoAllow: true}
	d := New(toolreg.ModeAuto, nil, WithSandbox(sb), WithAutoClassifier(c))
	res, err := d.Dispatch(context.Background(), bashInput(t, "/bin/echo hi", nil))
	if err != nil || res.IsError || !strings.Contains(res.Content, "SANDBOXED") {
		t.Fatalf("sandboxed command: %v %q", err, res.Content)
	}
	if c.calls() != 0 {
		t.Fatalf("classifier called %d times for a sandboxed command", c.calls())
	}

	off := &fakeSandbox{enabled: true, autoAllow: false}
	d = New(toolreg.ModeAuto, nil, WithSandbox(off), WithAutoClassifier(c))
	if res, _ := d.Dispatch(context.Background(), bashInput(t, "/bin/echo hi", nil)); res.IsError {
		t.Fatalf("classified command: %q", res.Content)
	}
	if c.calls() != 1 {
		t.Fatalf("classifier calls = %d, want 1 without the sandbox auto-allow", c.calls())
	}
}

// The session's classifier reuses an allow: the same call twice costs one
// provider request.
func TestAutoModeVerdictCacheEndToEnd(t *testing.T) {
	proj, outside := autoFixture(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		b, _ := json.Marshal(map[string]any{
			"model":   "m",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "<block>no</block>"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	mc := provider.NewModelCaller(&provider.OpenAI{APIKey: "k", BaseURL: srv.URL}, "m")
	mc.MaxRetries = 0
	c := &automode.Classifier{Caller: mc, ParseRetries: -1}
	d := New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithAutoClassifier(c))
	ctx := agent.WithTranscript(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("write f.txt"))})
	in, _ := json.Marshal(writeInput(filepath.Join(outside, "f.txt")))
	for i := 0; i < 2; i++ {
		res, err := d.Dispatch(ctx, agent.ToolUse{ID: "tu_1", Name: string(protocol.ToolWrite), Input: in})
		if err != nil || res.IsError {
			t.Fatalf("write %d: %v %+v", i, err, res)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("provider requests = %d, want 1 (second verdict from the cache)", n)
	}
}
