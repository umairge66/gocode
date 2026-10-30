package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// flakyServer answers the first `fail` requests with 529 overloaded, then OK.
func flakyServer(t *testing.T, fail int32) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) <= fail {
			w.WriteHeader(529)
			io.WriteString(w, `{"error":{"message":"Overloaded","type":"overloaded_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openRouterReplyJSON)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func noSleep(ctx context.Context, d time.Duration) error { return ctx.Err() }

type msgRecorder struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (r *msgRecorder) send(m tea.Msg) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
}

func (r *msgRecorder) retryTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.msgs {
		if rs, ok := m.(ui.SetRetryStatusMsg); ok {
			out = append(out, rs.Text)
		}
	}
	return out
}

// The interactive observer pushes the retry status to the UI and clears it when
// the retry starts — and keeps doing so for a caller rebuilt by a later
// /login · /provider swap (activateCaller), i.e. it follows the LIVE caller.
func TestInteractiveRetryStatusFollowsLiveCaller(t *testing.T) {
	srv, hits := flakyServer(t, 1)
	s := &printSession{
		Model:      testModel,
		Mode:       toolreg.ModeDefault,
		Provider:   providerOpenRouter,
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		newCaller:  defaultNewCaller,
		MaxRetries: 10,
		retrySleep: noSleep,
	}
	rec := &msgRecorder{}
	s.setRetryObserver(interactiveRetryObserver(rec.send))

	conv := &conversation.Conversation{}
	if ok, err := activateCaller(conv, s); !ok || err != nil {
		t.Fatalf("activateCaller = %v, %v", ok, err)
	}
	req := agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Content: []agent.ContentBlock{agent.TextBlock("hi")}}}}
	if _, err := conv.Model.CallModel(context.Background(), req); err != nil {
		t.Fatalf("CallModel: %v", err)
	}
	got := rec.retryTexts()
	if len(got) != 2 || got[0] != "API error · Retrying in 1s · attempt 1/10" || got[1] != "" {
		t.Fatalf("retry status msgs = %q, want [show, clear]", got)
	}

	// Simulate /login: the session rebuilds the caller; the hook must follow.
	atomic.StoreInt32(hits, 0)
	first := conv.Model
	if ok, err := activateCaller(conv, s); !ok || err != nil {
		t.Fatalf("re-activate = %v, %v", ok, err)
	}
	if conv.Model == first {
		t.Fatal("caller was not rebuilt")
	}
	if _, err := conv.Model.CallModel(context.Background(), req); err != nil {
		t.Fatalf("CallModel (rebuilt): %v", err)
	}
	if got := rec.retryTexts(); len(got) != 4 || !strings.Contains(got[2], "Retrying in") || got[3] != "" {
		t.Fatalf("after swap msgs = %q, want another show/clear pair", got)
	}
}

// The status message reaches the real ui model's status line while a turn is
// active.
func TestInteractiveRetryStatusRendersInStatusLine(t *testing.T) {
	m := ui.NewModel(100)
	var tm tea.Model = m
	tm, _ = tm.Update(ui.SetStatusMsg{Status: ui.Status{Kind: ui.StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	obs := interactiveRetryObserver(func(msg tea.Msg) { tm, _ = tm.Update(msg) })
	err := &provider.APIError{Status: 529, Category: provider.CategoryServer, Detail: "Overloaded"}
	obs.onRetry(3, 10, 4*time.Second, err)
	if line := tm.(*ui.Model).State().Status.StatusLine(); !strings.HasPrefix(line, "API overloaded · Retrying in 4s · attempt 3/10") {
		t.Fatalf("status line = %q", line)
	}
	obs.onStart(3, 10)
	if line := tm.(*ui.Model).State().Status.StatusLine(); !strings.HasPrefix(line, "Thinking") {
		t.Fatalf("status line after start = %q", line)
	}
}

// -p: a retry writes one note to stderr only; stdout carries just the answer.
func TestPrintRetryNoteOnStderrOnly(t *testing.T) {
	srv, hits := flakyServer(t, 2)
	env, out, errb := newTestEnv()
	s := &printSession{
		Model:      testModel,
		Mode:       toolreg.ModeDefault,
		Provider:   providerOpenRouter,
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		newCaller:  defaultNewCaller,
		MaxRetries: 10,
		retrySleep: noSleep,
	}
	if code := runPrintSession(env, s, "hi there"); code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "Hello from the model." {
		t.Fatalf("stdout = %q", got)
	}
	if atomic.LoadInt32(hits) != 3 {
		t.Fatalf("requests = %d, want 3", *hits)
	}
	notes := strings.Count(errb.String(), "gocode: API overloaded, retrying in ")
	if notes != 2 || !strings.Contains(errb.String(), "(1/10)") || !strings.Contains(errb.String(), "(2/10)") {
		t.Fatalf("stderr = %q, want two retry notes", errb.String())
	}
}

// -p: exhausting the budget reports "gave up after N retries" plus the
// category suggestion; newPrintSession honors GOCODE_MAX_RETRIES.
func TestPrintRetryExhausted(t *testing.T) {
	srv, _ := flakyServer(t, 100)
	env, out, errb := newTestEnv()
	s := &printSession{
		Model:      testModel,
		Mode:       toolreg.ModeDefault,
		Provider:   providerOpenRouter,
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		newCaller:  defaultNewCaller,
		MaxRetries: 2,
		retrySleep: noSleep,
	}
	if code := runPrintSession(env, s, "hi"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errb.String(), "gave up after 2 retries") || !strings.Contains(errb.String(), "Repeated 529 Overloaded errors") {
		t.Fatalf("stderr = %q", errb.String())
	}

	env.Getenv = func(k string) string {
		if k == provider.EnvMaxRetries {
			return "0"
		}
		return ""
	}
	if ps := newPrintSession(env, nil); ps.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0 from GOCODE_MAX_RETRIES", ps.MaxRetries)
	}
}

// Subagent callers inherit the retry budget but not the main status hook.
func TestSubagentCallerRetriesWithoutHook(t *testing.T) {
	parent := provider.NewModelCaller(&provider.OpenRouter{APIKey: "k"}, "m")
	parent.MaxRetries = 7
	parent.Sleep = noSleep
	parent.OnRetry = func(int, int, time.Duration, error) {}
	child := subagentModelFactory(providerOpenRouter, parent, nil)("").(*provider.ModelCaller)
	if child.MaxRetries != 7 || child.Sleep == nil || child.OnRetry != nil {
		t.Fatalf("child retry config = %d/%v/%v", child.MaxRetries, child.Sleep != nil, child.OnRetry != nil)
	}
}
