package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// Every request in this file goes to a 127.0.0.1 httptest server with a fake
// key; no real provider is contacted.

const compatTestKey = "test-key-not-real"

// compatSpecs returns the registry's OpenAI-compatible providers built by the
// shared Compatible client.
func compatSpecs(t *testing.T) []Spec {
	t.Helper()
	var out []Spec
	for _, s := range Providers() {
		if s.API != APIOpenAIChat {
			continue
		}
		p, err := s.New(Config{APIKey: "k"})
		if err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
		if _, ok := p.(*Compatible); ok {
			out = append(out, s)
		}
	}
	if len(out) < 16 {
		t.Fatalf("only %d compatible providers registered", len(out))
	}
	return out
}

// recordedRequest is what the test server saw.
type recordedRequest struct {
	method, path string
	header       http.Header
	body         map[string]any
}

func compatRecorder(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []recordedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone()}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

// TestCompatible_PerProviderWire: for every compatible provider, a
// non-streaming request reaches <root>+ChatPath with the spec's auth header
// and extra headers, carries the model id and the spec's output-cap field,
// and encodes an effort per the spec's reasoning style.
func TestCompatible_PerProviderWire(t *testing.T) {
	for _, s := range compatSpecs(t) {
		s := s
		t.Run(s.ID, func(t *testing.T) {
			srv, got := compatRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"r1","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
			})
			p, err := New(s.ID, Config{APIKey: compatTestKey, BaseURL: srv.URL + "/v9"})
			if err != nil {
				t.Fatal(err)
			}
			if p.Name() != s.ID {
				t.Fatalf("Name() = %q", p.Name())
			}
			resp, err := p.Chat(context.Background(), ChatRequest{
				Model:     "vendor/model-x",
				Messages:  []ChatMessage{{Role: RoleUser, Content: "hi"}},
				MaxTokens: 123,
				Effort:    "high",
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Content != "ok" || resp.Usage.TotalTokens != 4 {
				t.Fatalf("resp = %+v", resp)
			}
			reqs := got()
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			r := reqs[0]
			if r.method != http.MethodPost || r.path != "/v9"+s.chatPath() {
				t.Fatalf("%s %s, want POST /v9%s", r.method, r.path, s.chatPath())
			}
			switch s.Auth {
			case AuthXAPIKey:
				if r.header.Get("x-api-key") != compatTestKey || r.header.Get("Authorization") != "" {
					t.Fatalf("auth headers: %v", r.header)
				}
			default:
				if r.header.Get("Authorization") != "Bearer "+compatTestKey {
					t.Fatalf("Authorization = %q", r.header.Get("Authorization"))
				}
			}
			for k, v := range s.ExtraHeaders {
				if r.header.Get(k) != v {
					t.Errorf("header %s = %q, want %q", k, r.header.Get(k), v)
				}
			}
			if r.body["model"] != "vendor/model-x" {
				t.Fatalf("model = %v", r.body["model"])
			}
			capField, other := "max_tokens", "max_completion_tokens"
			if s.TokenParam == TokenParamMaxCompletionTokens {
				capField, other = other, capField
			}
			if r.body[capField] != float64(123) || r.body[other] != nil {
				t.Fatalf("cap fields: %s=%v %s=%v", capField, r.body[capField], other, r.body[other])
			}
			checkReasoning(t, s.Reasoning, r.body)
		})
	}
}

// checkReasoning asserts the body carries exactly the spec's effort encoding.
func checkReasoning(t *testing.T, style ReasoningParam, body map[string]any) {
	t.Helper()
	fields := map[string]any{
		"reasoning_effort": body["reasoning_effort"],
		"reasoning":        body["reasoning"],
		"thinking":         body["thinking"],
		"enable_thinking":  body["enable_thinking"],
	}
	want := map[string]bool{}
	switch style {
	case ReasoningEffort:
		want["reasoning_effort"] = true
		if body["reasoning_effort"] != "high" {
			t.Errorf("reasoning_effort = %v", body["reasoning_effort"])
		}
	case ReasoningObject:
		want["reasoning"] = true
	case ReasoningEnableThinking:
		want["enable_thinking"] = true
		if body["enable_thinking"] != true {
			t.Errorf("enable_thinking = %v", body["enable_thinking"])
		}
	}
	for k, v := range fields {
		if (v != nil) != want[k] {
			t.Errorf("reasoning field %s = %v (style %s)", k, v, style)
		}
	}
}

// TestCompatible_NoEffortSendsNoReasoningField: without an effort the body
// carries no reasoning field on any provider.
func TestCompatible_NoEffortSendsNoReasoningField(t *testing.T) {
	for _, s := range compatSpecs(t) {
		body, err := buildOAIBody(ChatRequest{Model: "m"}, true, (&Compatible{Spec: &s}).bodyOptions())
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"reasoning_effort", "reasoning", "thinking", "enable_thinking"} {
			if strings.Contains(string(body), `"`+f+`"`) {
				t.Errorf("%s: %s sent without effort: %s", s.ID, f, body)
			}
		}
		hasUsage := strings.Contains(string(body), `"stream_options":{"include_usage":true}`)
		if hasUsage == s.NoStreamUsage {
			t.Errorf("%s: stream_options present=%v, NoStreamUsage=%v", s.ID, hasUsage, s.NoStreamUsage)
		}
	}
}

// TestCompatible_ListModels: a provider with a list endpoint GETs
// <root>+ModelsPath with its auth and reports the context window the server
// exposes; one without serves its static list and makes no request.
func TestCompatible_ListModels(t *testing.T) {
	for _, s := range compatSpecs(t) {
		s := s
		t.Run(s.ID, func(t *testing.T) {
			srv, got := compatRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"object":"list","data":[
					{"id":"b-model","context_window":131072},
					{"id":"a-model","context_length":"32768"},
					{"id":"c-model"}]}`)
			})
			p, _ := New(s.ID, Config{APIKey: compatTestKey, BaseURL: srv.URL + "/root"})
			models, err := ListModels(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			reqs := got()
			if s.ModelsPath == "" {
				if len(reqs) != 0 {
					t.Fatalf("no list endpoint, but %d requests", len(reqs))
				}
				if len(models) == 0 || len(models) != len(s.StaticModels) {
					t.Fatalf("static list = %v", models)
				}
				return
			}
			if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].path != "/root"+s.ModelsPath {
				t.Fatalf("requests = %+v", reqs)
			}
			if s.Auth != AuthXAPIKey && reqs[0].header.Get("Authorization") != "Bearer "+compatTestKey {
				t.Fatalf("models request auth = %q", reqs[0].header.Get("Authorization"))
			}
			if len(models) != 3 || models[0].ID != "a-model" || models[0].ContextLength != 32768 ||
				models[1].ContextLength != 131072 || models[2].ContextLength != 0 {
				t.Fatalf("models = %+v", models)
			}
		})
	}
}

// TestCompatible_ListModels_BareArray: some servers return a bare JSON array.
func TestCompatible_ListModels_BareArray(t *testing.T) {
	srv, _ := compatRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"x","max_model_len":8192}]`)
	})
	models, err := (&Compatible{Spec: lookupSpec("groq"), APIKey: "k", BaseURL: srv.URL}).ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ContextLength != 8192 {
		t.Fatalf("models = %+v, %v", models, err)
	}
}

// TestCompatible_LiveContextWindow: a context window the provider's /models
// reported (cached per provider and base URL) feeds CapabilitiesFor.
func TestCompatible_LiveContextWindow(t *testing.T) {
	cat := newLiveCatalog("", "groq-models.json", "https://api.groq.example", catalogTTL)
	cat.store("https://api.groq.example", []liveModel{{ID: "llama-x", ContextWindow: 131072, MaxOutput: 32768}})
	caps := capabilitiesWith(nil, cat, "groq", "llama-x")
	if caps.ContextWindow != 131072 || caps.MaxOutputLimit != 32768 || caps.Source != SourceServer {
		t.Fatalf("caps = %+v", caps)
	}
	if c := capabilitiesWith(nil, cat, "groq", "unknown-model"); c.Source != SourceDefault {
		t.Fatalf("unknown model caps = %+v", c)
	}
	if liveCatalogFor("groq") == nil || liveCatalogFor("openrouter") != nil {
		t.Fatal("liveCatalogFor: groq needs a cache, openrouter has its own catalog")
	}
}

// sseToolCallServer streams a tool call (arguments split across chunks) on
// the first request and a final text answer on the second, recording both.
func sseToolCallServer(t *testing.T, round *int32, bodies *[]map[string]any, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		*bodies = append(*bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(round, 1)
		var chunks []string
		if n == 1 {
			chunks = []string{
				`{"id":"c1","model":"m","choices":[{"delta":{"role":"assistant","reasoning_content":"thinking..."}}]}`,
				`{"id":"c1","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"echo","arguments":"{\"msg\":"}}]}}]}`,
				`{"id":"c1","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ping\"}"}}]}}]}`,
				`{"id":"c1","model":"m","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			}
		} else {
			chunks = []string{
				`{"id":"c2","model":"m","choices":[{"delta":{"content":"the answer "}}]}`,
				`{"id":"c2","model":"m","choices":[{"delta":{"content":"is pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24}}`,
			}
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCompatible_StreamedToolCallRoundTrip drives agent.Loop over the
// streaming path for representative providers (Groq, xAI, Z.ai, Alibaba):
// the streamed, fragment-split tool call is dispatched, its result goes back
// as a role:"tool" message, and the final streamed text ends the turn.
func TestCompatible_StreamedToolCallRoundTrip(t *testing.T) {
	for _, id := range []string{"groq", "xai", "zai", "alibaba-sg"} {
		id := id
		t.Run(id, func(t *testing.T) {
			var (
				round  int32
				mu     sync.Mutex
				bodies []map[string]any
			)
			srv := sseToolCallServer(t, &round, &bodies, &mu)
			p, err := New(id, Config{APIKey: compatTestKey, BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			mc := NewModelCaller(p, "test-model")
			disp := &fakeDispatcher{content: "pong"}
			var text, thinking strings.Builder
			loop := &agent.Loop{
				Model:         mc,
				Tools:         disp,
				System:        "sys",
				ToolSchemas:   []agent.ToolSchema{{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
				MaxTurns:      5,
				StreamHandler: &agent.StreamHandler{OnText: func(d string) { text.WriteString(d) }, OnThinking: func(d string) { thinking.WriteString(d) }},
			}
			res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("say pong"))})
			if res.Reason != agent.ReasonCompleted {
				t.Fatalf("reason = %q (err %v)", res.Reason, res.Err)
			}
			if disp.lastNm != "echo" || string(disp.lastIn) != `{"msg":"ping"}` {
				t.Fatalf("dispatched %s(%s)", disp.lastNm, disp.lastIn)
			}
			if text.String() != "the answer is pong" {
				t.Fatalf("streamed text = %q", text.String())
			}
			if thinking.String() != "thinking..." {
				t.Fatalf("streamed reasoning = %q", thinking.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("%d requests", len(bodies))
			}
			for i, b := range bodies {
				if b["stream"] != true || b["model"] != "test-model" {
					t.Fatalf("request %d: stream=%v model=%v", i, b["stream"], b["model"])
				}
				tools, _ := b["tools"].([]any)
				if len(tools) != 1 {
					t.Fatalf("request %d: tools = %v", i, b["tools"])
				}
			}
			msgs, _ := bodies[1]["messages"].([]any)
			var sawCall, sawResult bool
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) == 1 {
					sawCall = true
				}
				if mm["role"] == "tool" && mm["tool_call_id"] == "call_7" && mm["content"] == "pong" {
					sawResult = true
				}
			}
			if !sawCall || !sawResult {
				t.Fatalf("second request lacks the tool round: %v", msgs)
			}
			if _, u, ok := mc.LastTurnUsage(); !ok || u.OutputTokens != 4 {
				t.Fatalf("usage = %+v, %v", u, ok)
			}
		})
	}
}
