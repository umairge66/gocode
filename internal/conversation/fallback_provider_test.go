package conversation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

// TestFallbackChainOverProvider walks a real provider.ModelCaller down its
// chain: the primary is rate limited (free-models-per-min), the first
// fallback fails upstream (502), the second fallback answers. The next turn
// starts on the primary again.
func TestFallbackChainOverProvider(t *testing.T) {
	var (
		mu     sync.Mutex
		served []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		served = append(served, body.Model)
		mu.Unlock()
		switch body.Model {
		case "primary:free":
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"Rate limit exceeded: free-models-per-min.","code":429}}`)
		case "second:free":
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"error":{"message":"Provider returned error","code":502}}`)
		default:
			io.WriteString(w, `{"model":"`+body.Model+`","choices":[{"message":{"role":"assistant","content":"ok from `+body.Model+`"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	mc := provider.NewModelCaller(&provider.OpenRouter{APIKey: "k", BaseURL: srv.URL}, "primary:free")
	mc.MaxRetries = 10
	mc.Sleep = func(context.Context, time.Duration) error { return nil }
	c := New(mc, fakeDispatcher{})
	c.SetFallbackModels([]string{"second:free", "third"})
	c.FallbackNotice = provider.FallbackNotice

	res, err := c.Submit(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil || res.AssistantText != "ok from third" {
		t.Fatalf("text=%q err=%v, want the second fallback's answer", res.AssistantText, res.Err)
	}
	want := []string{
		"Switched to second:free due to rate limits on primary:free",
		"Switched to third due to errors from second:free",
	}
	if strings.Join(res.Notices, "|") != strings.Join(want, "|") {
		t.Fatalf("notices = %q, want %q", res.Notices, want)
	}
	mu.Lock()
	got := strings.Join(served, ",")
	served = nil
	mu.Unlock()
	if got != "primary:free,primary:free,primary:free,second:free,third" {
		t.Fatalf("requests = %s", got)
	}
	if mc.CurrentModel() != "primary:free" || mc.FallbackModel() != "" {
		t.Fatalf("after the turn model=%q armed=%q, want the primary and nothing armed", mc.CurrentModel(), mc.FallbackModel())
	}

	// /fallback off: the next turn fails on the primary with no switch.
	c.SetFallbackModels(nil)
	mc.MaxRetries = 1
	res, _ = c.Submit(context.Background(), "again")
	if res.Err == nil || len(res.Notices) != 0 {
		t.Fatalf("err=%v notices=%v, want the plain rate-limit failure", res.Err, res.Notices)
	}
}
