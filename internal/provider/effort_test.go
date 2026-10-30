package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// capturedReq is one request a test server saw: headers + raw JSON body.
type capturedReq struct {
	header http.Header
	body   map[string]any
	raw    string
}

// recordingServer answers every request with respond(n) (n = 0-based request
// index) and records what it received.
func recordingServer(t *testing.T, respond func(w http.ResponseWriter, n int)) (*httptest.Server, func() []capturedReq) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []capturedReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, capturedReq{header: r.Header.Clone(), body: body, raw: string(raw)})
		mu.Unlock()
		respond(w, n)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedReq(nil), reqs...)
	}
}

const antOK = `{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
const oaiOK = `{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func userReq() agent.ModelRequest {
	return agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Content: []agent.ContentBlock{agent.TextBlock("hi")}}}}
}

func TestNormalizeEffort(t *testing.T) {
	for in, want := range map[string]string{"low": "low", " HIGH ": "high", "med": "medium", "xhigh": "xhigh", "max": "max"} {
		if got, ok := NormalizeEffort(in); !ok || got != want {
			t.Errorf("NormalizeEffort(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "auto", "extreme", "5"} {
		if _, ok := NormalizeEffort(in); ok {
			t.Errorf("NormalizeEffort(%q) accepted", in)
		}
	}
}

// TestAnthropicEffortWire: effort set → output_config.effort + the effort beta;
// unset → neither (body identical to a request built without the field).
func TestAnthropicEffortWire(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, _ int) { io.WriteString(w, antOK) })
	mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-opus-5-5")

	if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
		t.Fatal(err)
	}
	mc.SetEffort("max")
	// The JSON stub is not SSE; only the request matters here.
	_, _ = mc.CallModelStream(context.Background(), userReq(), nil)
	reqs := got()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	unset := reqs[0]
	if _, ok := unset.body["output_config"]; ok || strings.Contains(unset.raw, "effort") {
		t.Errorf("unset effort leaked into body: %s", unset.raw)
	}
	if h := unset.header.Get("anthropic-beta"); h != "" {
		t.Errorf("unset effort sent anthropic-beta %q", h)
	}
	// MaxTokens: the capability table's default output cap for claude-opus-5-5
	// (catalog max_output_tokens.default) — applied with or without effort.
	want, _ := buildAnthropicBody(ChatRequest{Model: "claude-opus-5-5", MaxTokens: 128000, Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}}, false, false)
	if unset.raw != string(want) {
		t.Errorf("unset body changed:\n got %s\nwant %s", unset.raw, want)
	}

	set := reqs[1]
	oc, _ := set.body["output_config"].(map[string]any)
	if oc["effort"] != "max" {
		t.Errorf("output_config = %v", set.body["output_config"])
	}
	if h := set.header.Get("anthropic-beta"); h != anthropicEffortBeta {
		t.Errorf("anthropic-beta = %q", h)
	}
}

func TestAddBetaHeaderMerges(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "a-1, b-2")
	addBetaHeader(h, anthropicEffortBeta)
	addBetaHeader(h, anthropicEffortBeta)
	if got := h.Get("anthropic-beta"); got != "a-1,b-2,"+anthropicEffortBeta {
		t.Errorf("merged = %q", got)
	}
}

// TestOpenAIEffortWire: reasoning_effort, clamped (max → high); unset omitted.
func TestOpenAIEffortWire(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, _ int) { io.WriteString(w, oaiOK) })
	mc := NewModelCaller(&OpenAI{APIKey: "k", BaseURL: srv.URL}, "gpt-5")
	for _, lvl := range []string{"", "low", "xhigh", "max"} {
		mc.SetEffort(lvl)
		if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
			t.Fatal(err)
		}
	}
	reqs := got()
	if _, ok := reqs[0].body["reasoning_effort"]; ok {
		t.Errorf("unset sent reasoning_effort: %s", reqs[0].raw)
	}
	for i, want := range []string{"low", "high", "high"} {
		b := reqs[i+1].body
		if b["reasoning_effort"] != want {
			t.Errorf("req %d reasoning_effort = %v want %s", i+1, b["reasoning_effort"], want)
		}
		if _, ok := b["reasoning"]; ok {
			t.Errorf("openai sent reasoning object")
		}
	}
}

// TestOpenRouterEffortWire: reasoning.effort, clamped (xhigh → high).
func TestOpenRouterEffortWire(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, _ int) { io.WriteString(w, oaiOK) })
	mc := NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: srv.URL}, "anthropic/claude-opus-5-5")
	for _, lvl := range []string{"", "medium", "xhigh"} {
		mc.SetEffort(lvl)
		if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
			t.Fatal(err)
		}
	}
	reqs := got()
	if _, ok := reqs[0].body["reasoning"]; ok {
		t.Errorf("unset sent reasoning: %s", reqs[0].raw)
	}
	for i, want := range []string{"medium", "high"} {
		r, _ := reqs[i+1].body["reasoning"].(map[string]any)
		if r["effort"] != want {
			t.Errorf("req %d reasoning = %v want effort %s", i+1, reqs[i+1].body["reasoning"], want)
		}
		if _, ok := reqs[i+1].body["reasoning_effort"]; ok {
			t.Errorf("openrouter sent reasoning_effort")
		}
	}
}

// TestEffortUnsupportedDropsAndRemembers: an effort_unsupported 400 triggers
// exactly one retry without effort, and the model is remembered so the next
// request omits effort up front.
func TestEffortUnsupportedDropsAndRemembers(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
	}{
		{"effort parameter", "This model does not support the effort parameter."},
		{"extra inputs", "output_config: Extra inputs are not permitted"},
		{"requires", "output_config.effort requires a model that supports effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
				if n == 0 {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"`+tc.msg+`"}}`)
					return
				}
				io.WriteString(w, antOK)
			})
			mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-old")
			mc.SetEffort("high")
			resp, err := mc.CallModel(context.Background(), userReq())
			if err != nil {
				t.Fatalf("expected graceful retry, got %v", err)
			}
			if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "ok" {
				t.Fatalf("resp = %+v", resp)
			}
			reqs := got()
			if len(reqs) != 2 {
				t.Fatalf("requests = %d, want exactly 2 (one retry)", len(reqs))
			}
			if _, ok := reqs[0].body["output_config"]; !ok {
				t.Error("first request lacked effort")
			}
			if _, ok := reqs[1].body["output_config"]; ok || reqs[1].header.Get("anthropic-beta") != "" {
				t.Error("retry still carried effort")
			}
			if mc.EffortSupported("claude-old") {
				t.Error("model not remembered as unsupported")
			}
			if mc.CurrentEffort() != "high" {
				t.Errorf("configured effort lost: %q", mc.CurrentEffort())
			}
			// Next request: effort is omitted up front (no extra round trip).
			if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
				t.Fatal(err)
			}
			reqs = got()
			if len(reqs) != 3 {
				t.Fatalf("requests = %d after follow-up, want 3", len(reqs))
			}
			if _, ok := reqs[2].body["output_config"]; ok {
				t.Error("effort re-sent to an unsupported model")
			}
			// A different model still gets effort.
			mc.SetModel("claude-new")
			_, _ = mc.CallModel(context.Background(), userReq())
			if _, ok := got()[3].body["output_config"]; !ok {
				t.Error("effort not sent to a different model")
			}
		})
	}
}

// TestEffortUnrelated400NotRetried: a 400 that is not about effort is returned
// as-is with no retry.
func TestEffortUnrelated400NotRetried(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`)
	})
	mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "m")
	mc.SetEffort("low")
	if _, err := mc.CallModel(context.Background(), userReq()); err == nil {
		t.Fatal("expected error")
	}
	if n := len(got()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

// TestEffortUnsupportedStreamRetry: the streaming path retries once too, and
// the retried stream's text is delivered.
func TestEffortUnsupportedStreamRetry(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		if n == 0 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'reasoning_effort' is not supported with this model.","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hey\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	// A model the capability table does not know (gpt-4o is KNOWN not to take
	// effort, so it is never sent there): effort is attempted and the
	// retry-on-rejection safety net drops it.
	mc := NewModelCaller(&OpenAI{APIKey: "k", BaseURL: srv.URL}, "my-reasoning-model")
	mc.SetEffort("medium")
	var deltas strings.Builder
	resp, err := mc.CallModelStream(context.Background(), userReq(), &agent.StreamHandler{OnText: func(s string) { deltas.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if deltas.String() != "hey" || resp.Message.Content[0].Text != "hey" {
		t.Fatalf("deltas %q resp %+v", deltas.String(), resp)
	}
	reqs := got()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if _, ok := reqs[1].body["reasoning_effort"]; ok {
		t.Error("retry carried reasoning_effort")
	}
}

// TestInheritEffort: a derived (subagent) caller gets the parent's level and
// shares its unsupported-model memory.
func TestInheritEffort(t *testing.T) {
	parent := NewModelCaller(&modelRecorder{}, "a")
	parent.SetEffort("xhigh")
	parent.dropEffortOnReject(&ChatRequest{Model: "a", Effort: "xhigh"}, &APIError{Status: 400, Detail: "This model does not support the effort parameter"})
	child := NewModelCaller(parent.Provider, "a")
	child.InheritEffort(parent)
	if child.CurrentEffort() != "xhigh" {
		t.Errorf("child effort = %q", child.CurrentEffort())
	}
	if child.EffortSupported("a") || !child.EffortSupported("b") {
		t.Error("unsupported memory not shared")
	}
	child.SetEffort("low")
	if parent.CurrentEffort() != "xhigh" {
		t.Error("child override leaked to parent")
	}
}
