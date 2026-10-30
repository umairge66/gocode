package bedrock

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

var fixedNow = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

const (
	fakeKeyID   = "AKIDTESTFAKE0001"
	fakeSecret  = "fakeSecretKeyForTestsOnly0000000000000000"
	fakeSession = "fakeSessionTokenForTestsOnly"
	fakeBearer  = "ABSKfakebedrockapikeyfortests"
)

// recorded is what the fake server saw for one request.
type recorded struct {
	method, path, rawQuery string
	header                 http.Header
	host                   string
	body                   []byte
}

type fakeBedrock struct {
	t       *testing.T
	mu      sync.Mutex
	reqs    []recorded
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) (*fakeBedrock, *httptest.Server) {
	f := &fakeBedrock{t: t, handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{method: r.Method, path: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, header: r.Header.Clone(), host: r.Host, body: body})
		f.mu.Unlock()
		f.handler(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeBedrock) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		f.t.Fatal("no request recorded")
	}
	return f.reqs[len(f.reqs)-1]
}

// sigv4Provider builds a provider with fake static (temporary) keys pointed
// at srv. The environment is empty and HOME is a temp dir.
func sigv4Provider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	cfg := Config{
		Region:          "us-west-2",
		AccessKeyID:     fakeKeyID,
		SecretAccessKey: fakeSecret,
		SessionToken:    fakeSession,
		BaseURL:         srv.URL,
		ControlBaseURL:  srv.URL,
		HTTP:            srv.Client(),
		Getenv:          func(string) string { return "" },
		HomeDir:         t.TempDir(),
		Now:             func() time.Time { return fixedNow },
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func bearerProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	p, err := New(Config{
		Region:  "eu-central-1",
		BaseURL: srv.URL, ControlBaseURL: srv.URL, HTTP: srv.Client(),
		Getenv:  func(k string) string { return map[string]string{EnvBearerToken: fakeBearer}[k] },
		HomeDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// verifySigV4 recomputes the signature from what the server received, using
// only the headers the client declared as signed, and checks it matches.
func verifySigV4(t *testing.T, rec recorded, region string) {
	t.Helper()
	auth := rec.header.Get("Authorization")
	wantPrefix := "AWS4-HMAC-SHA256 Credential=" + fakeKeyID + "/20261027/" + region + "/bedrock/aws4_request, SignedHeaders="
	if !strings.HasPrefix(auth, wantPrefix) {
		t.Fatalf("Authorization = %q", auth)
	}
	rest := strings.TrimPrefix(auth, wantPrefix)
	parts := strings.SplitN(rest, ", Signature=", 2)
	signed := strings.Split(parts[0], ";")
	for _, must := range []string{"host", "x-amz-date", "x-amz-security-token", "content-type"} {
		if must == "content-type" && len(rec.body) == 0 {
			continue
		}
		found := false
		for _, s := range signed {
			found = found || s == must
		}
		if !found {
			t.Errorf("%s not signed (%v)", must, signed)
		}
	}
	if rec.header.Get("X-Amz-Date") != "20261027T093000Z" {
		t.Errorf("X-Amz-Date = %q", rec.header.Get("X-Amz-Date"))
	}
	if rec.header.Get("X-Amz-Security-Token") != fakeSession {
		t.Errorf("session token header missing")
	}
	// Rebuild the request as received and re-sign it independently.
	r, _ := http.NewRequest(rec.method, "http://"+rec.host+rec.path, nil)
	if rec.rawQuery != "" {
		r.URL.RawQuery = rec.rawQuery
	}
	r.Host = rec.host
	for _, h := range signed {
		if h == "host" {
			continue
		}
		r.Header[http.CanonicalHeaderKey(h)] = rec.header.Values(h)
	}
	canon, _ := canonicalRequest(r, hashHex(rec.body))
	sts := stringToSign("20261027T093000Z", "20261027/"+region+"/bedrock/aws4_request", canon)
	sig := hex.EncodeToString(hmacSHA256(signingKey(fakeSecret, "20261027", region, "bedrock"), []byte(sts)))
	if parts[1] != sig {
		t.Errorf("signature mismatch: got %s want %s\ncanonical:\n%s", parts[1], sig, canon)
	}
}

func streamResponse(w http.ResponseWriter, frames ...[]byte) {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.Header().Set("X-Amzn-RequestId", "req-123")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		_, _ = w.Write(f)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

func toolRequest() provider.ChatRequest {
	temp := 0.2
	return provider.ChatRequest{
		Model: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Messages: []provider.ChatMessage{
			{Role: provider.RoleSystem, Content: "You are a coding agent."},
			{Role: provider.RoleUser, Content: "list files"},
			{Role: provider.RoleAssistant, Content: "Looking.", ToolCalls: []provider.ChatToolCall{{ID: "tu_1", Name: "ls", Arguments: `{"path":"."}`}}},
			{Role: provider.RoleTool, ToolCallID: "tu_1", Name: "ls", Content: "a.go\nb.go",
				Images: []provider.ChatImage{{Base64: "aGVsbG8=", MediaType: "image/jpeg"}}},
			{Role: provider.RoleUser, Content: "now read a.go"},
		},
		Tools: []provider.ChatTool{{Name: "ls", Description: "List files", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:   1024,
		Temperature: &temp,
		CacheSystem: true,
		Effort:      "high",
	}
}

func TestStreamTextAndToolUseRoundTrip(t *testing.T) {
	f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		streamResponse(w,
			eventFrame("messageStart", `{"role":"assistant","p":"abc"}`),
			eventFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"Let me think."}}}`),
			eventFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"signature":"sig"}}}`),
			eventFrame("contentBlockStop", `{"contentBlockIndex":0}`),
			eventFrame("contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"Reading "}}`),
			eventFrame("contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"it now."}}`),
			eventFrame("contentBlockStop", `{"contentBlockIndex":1}`),
			eventFrame("contentBlockStart", `{"contentBlockIndex":2,"start":{"toolUse":{"toolUseId":"tu_2","name":"read"}}}`),
			eventFrame("contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"{\"path\":"}}}`),
			eventFrame("contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"\"a.go\"}"}}}`),
			eventFrame("contentBlockStop", `{"contentBlockIndex":2}`),
			eventFrame("messageStop", `{"stopReason":"tool_use"}`),
			eventFrame("metadata", `{"usage":{"inputTokens":120,"outputTokens":40,"totalTokens":2160,"cacheReadInputTokens":1500,"cacheWriteInputTokens":500},"metrics":{"latencyMs":321}}`),
		)
	})
	p := sigv4Provider(t, srv)
	req := toolRequest()
	var reasoning, deltas []string
	req.OnReasoning = func(d string) { reasoning = append(reasoning, d) }
	resp, err := p.StreamChat(context.Background(), req, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}

	rec := f.last()
	if rec.method != "POST" || rec.path != "/model/us.anthropic.claude-haiku-4-5-20251001-v1%3A0/converse-stream" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
	if rec.header.Get("Accept") != "application/vnd.amazon.eventstream" || rec.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", rec.header)
	}
	verifySigV4(t, rec, "us-west-2")

	// Request JSON.
	var got map[string]any
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Fatal(err)
	}
	wantJSON := `{
	  "messages":[
	    {"role":"user","content":[{"text":"list files"}]},
	    {"role":"assistant","content":[{"text":"Looking."},{"toolUse":{"toolUseId":"tu_1","name":"ls","input":{"path":"."}}}]},
	    {"role":"user","content":[
	      {"toolResult":{"toolUseId":"tu_1","content":[{"text":"a.go\nb.go"},{"image":{"format":"jpeg","source":{"bytes":"aGVsbG8="}}}]}},
	      {"text":"now read a.go"}]}
	  ],
	  "system":[{"text":"You are a coding agent."},{"cachePoint":{"type":"default"}}],
	  "inferenceConfig":{"maxTokens":1024,"temperature":0.2},
	  "toolConfig":{"tools":[
	    {"toolSpec":{"name":"ls","description":"List files","inputSchema":{"json":{"type":"object","properties":{"path":{"type":"string"}}}}}},
	    {"toolSpec":{"name":"read","inputSchema":{"json":{"type":"object"}}}}]},
	  "additionalModelRequestFields":{"output_config":{"effort":"high"}}
	}`
	var want map[string]any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if !bytes.Equal(gb, wb) {
		t.Errorf("request body:\n%s\nwant:\n%s", gb, wb)
	}

	// Response.
	if strings.Join(deltas, "") != "Reading it now." || len(deltas) != 2 {
		t.Errorf("deltas = %q", deltas)
	}
	if strings.Join(reasoning, "") != "Let me think." {
		t.Errorf("reasoning = %q", reasoning)
	}
	if resp.Content != "Reading it now." || resp.FinishReason != provider.FinishToolCalls || resp.ID != "req-123" {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0] != (provider.ChatToolCall{ID: "tu_2", Name: "read", Arguments: `{"path":"a.go"}`}) {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	wantUsage := provider.ChatUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 2160, CacheReadTokens: 1500, CacheCreationTokens: 500}
	if resp.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", resp.Usage, wantUsage)
	}
}

func TestConverseNonStreamingWithBearer(t *testing.T) {
	f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("X-Amzn-RequestId", "req-9")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[
			{"reasoningContent":{"reasoningText":{"text":"hmm","signature":"s"}}},
			{"text":"Done."},
			{"toolUse":{"toolUseId":"t1","name":"read","input":{"path":"x"}}}]}},
			"stopReason":"max_tokens","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15},"metrics":{"latencyMs":1}}`)
	})
	p := bearerProvider(t, srv)
	resp, err := p.Chat(context.Background(), provider.ChatRequest{
		Model:    "global.anthropic.claude-sonnet-4-6[1m]",
		Messages: []provider.ChatMessage{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := f.last()
	if got := rec.header.Get("Authorization"); got != "Bearer "+fakeBearer {
		t.Errorf("Authorization = %q", got)
	}
	if rec.header.Get("X-Amz-Date") != "" || rec.header.Get("X-Amz-Security-Token") != "" {
		t.Errorf("bearer requests must not be SigV4-signed: %v", rec.header)
	}
	if rec.path != "/model/global.anthropic.claude-sonnet-4-6/converse" {
		t.Errorf("path = %s ([1m] must be stripped)", rec.path)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.body, &body)
	amrf, _ := body["additionalModelRequestFields"].(map[string]any)
	if beta, _ := amrf["anthropic_beta"].([]any); len(beta) != 1 || beta[0] != context1MBeta {
		t.Errorf("1M beta not requested: %v", body["additionalModelRequestFields"])
	}
	if ic, _ := body["inferenceConfig"].(map[string]any); ic["maxTokens"] != float64(defaultMaxTokens) {
		t.Errorf("inferenceConfig = %v", body["inferenceConfig"])
	}
	if _, ok := body["system"]; ok {
		t.Errorf("no system expected")
	}
	if resp.Content != "Done." || resp.FinishReason != provider.FinishLength || resp.ID != "req-9" ||
		resp.Model != "global.anthropic.claude-sonnet-4-6[1m]" {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments != `{"path":"x"}` {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Usage != (provider.ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func apiErr(t *testing.T, err error) *provider.APIError {
	t.Helper()
	var ae *provider.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %T %v, want *provider.APIError", err, err)
	}
	return ae
}

func TestHTTPErrorMapping(t *testing.T) {
	tests := []struct {
		status    int
		errType   string
		message   string
		cat       provider.Category
		retryable bool
		msgHas    string
		detailHas string
	}{
		{429, "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/", "Too many requests, please wait before trying again.",
			provider.CategoryRateLimit, true, "Rate limited", ""},
		{503, "ServiceUnavailableException", "Bedrock is unable to process your request.", provider.CategoryServer, true, "temporarily unavailable", ""},
		{429, "ModelNotReadyException", "Model is not ready", provider.CategoryServer, true, "", ""},
		{424, "ModelErrorException", "The model failed", provider.CategoryServer, true, "", ""},
		{403, "AccessDeniedException", "You don't have access to the model with the specified model ID.", provider.CategoryAuth, false,
			"Enable model access", "permission_error model: us.anthropic.claude-haiku-4-5-20251001-v1:0"},
		{403, "UnrecognizedClientException", "The security token included in the request is invalid.", provider.CategoryAuth, false, "AWS credentials", ""},
		{400, "ValidationException", "The provided model identifier is invalid.", provider.CategoryUnknown, false, "does not recognise", "not a valid model id"},
		{400, "ValidationException", "Input is too long for requested model.", provider.CategoryUnknown, false, "Prompt is too long", ""},
		{400, "ValidationException", "messages.0: bad", provider.CategoryUnknown, false, "HTTP 400", ""},
		{404, "ResourceNotFoundException", "Model not found", provider.CategoryUnknown, false, "", "model_not_found"},
		{500, "", "", provider.CategoryServer, true, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.errType+"_"+tc.message, func(t *testing.T) {
			_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
				if tc.errType != "" {
					w.Header().Set("X-Amzn-ErrorType", tc.errType)
				}
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":`+mustJSON(tc.message)+`}`)
			})
			p := sigv4Provider(t, srv)
			_, err := p.StreamChat(context.Background(), toolRequest(), nil)
			ae := apiErr(t, err)
			if ae.Status != tc.status || ae.Category != tc.cat || ae.Retryable != tc.retryable || ae.Provider != "bedrock" {
				t.Errorf("got status=%d cat=%s retry=%v, want %d %s %v (%s)", ae.Status, ae.Category, ae.Retryable, tc.status, tc.cat, tc.retryable, ae.Message)
			}
			if !strings.Contains(ae.Message, tc.msgHas) || !strings.Contains(ae.Detail, tc.detailHas) {
				t.Errorf("message %q / detail %q", ae.Message, ae.Detail)
			}
			if ae.RetryAfter != "3" {
				t.Errorf("RetryAfter = %q", ae.RetryAfter)
			}
		})
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestMidStreamThrottlingException(t *testing.T) {
	_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		streamResponse(w,
			eventFrame("messageStart", `{"role":"assistant"}`),
			eventFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"par"}}`),
			exceptionFrame("throttlingException", `{"message":"Too many tokens, please wait before trying again."}`),
		)
	})
	p := sigv4Provider(t, srv)
	_, err := p.StreamChat(context.Background(), toolRequest(), nil)
	ae := apiErr(t, err)
	if ae.Category != provider.CategoryRateLimit || !ae.Retryable || ae.Status != 0 {
		t.Errorf("ae = %+v", ae)
	}

	_, srv2 := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		streamResponse(w, exceptionFrame("modelStreamErrorException", `{"message":"boom","originalStatusCode":500}`))
	})
	_, err = sigv4Provider(t, srv2).StreamChat(context.Background(), toolRequest(), nil)
	if ae := apiErr(t, err); ae.Category != provider.CategoryServer || !ae.Retryable {
		t.Errorf("model stream error = %+v", ae)
	}

	// A stream cut before messageStop is a retryable connection failure.
	_, srv3 := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		streamResponse(w, eventFrame("messageStart", `{"role":"assistant"}`))
	})
	_, err = sigv4Provider(t, srv3).StreamChat(context.Background(), toolRequest(), nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated stream err = %v", err)
	}
}

func TestErrorsNeverEchoCredentials(t *testing.T) {
	_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("X-Amzn-ErrorType", "InvalidSignatureException")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"message":"The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method. Consult the service documentation for details.\n\nThe Canonical String for this request should have been\n'POST\n/model\nx-amz-security-token:`+fakeSession+`'\n\nThe String-to-Sign should have been ..."}`)
	})
	p := sigv4Provider(t, srv)
	_, err := p.Chat(context.Background(), toolRequest())
	ae := apiErr(t, err)
	all := ae.Message + " " + ae.Detail + " " + err.Error()
	for _, s := range []string{fakeSession, fakeSecret} {
		if strings.Contains(all, s) {
			t.Errorf("secret leaked in error: %s", all)
		}
	}
	if ae.Category != provider.CategoryAuth {
		t.Errorf("category = %s", ae.Category)
	}
	if scrub("token "+fakeBearer+" end", []string{fakeBearer}) != "token *** end" {
		t.Errorf("scrub failed")
	}
}

func TestPromptCacheOnlyForSupportedModels(t *testing.T) {
	req := provider.ChatRequest{
		Model:       "anthropic.claude-3-haiku-20240307-v1:0",
		Messages:    []provider.ChatMessage{{Role: provider.RoleSystem, Content: "sys"}, {Role: provider.RoleUser, Content: "hi"}},
		CacheSystem: true,
		Effort:      "low",
	}
	b, _ := buildConverseBody(req, req.Model)
	if bytes.Contains(b, []byte("cachePoint")) {
		t.Errorf("cachePoint sent to Claude 3 Haiku: %s", b)
	}
	req.Model = "meta.llama3-70b-instruct-v1:0"
	b, _ = buildConverseBody(req, req.Model)
	if bytes.Contains(b, []byte("cachePoint")) || bytes.Contains(b, []byte("output_config")) {
		t.Errorf("non-Claude model got Claude-only fields: %s", b)
	}
	req.Model, req.CacheSystem = "us.anthropic.claude-opus-5-5", false
	b, _ = buildConverseBody(req, req.Model)
	if bytes.Contains(b, []byte("cachePoint")) {
		t.Errorf("cachePoint without CacheSystem: %s", b)
	}
}

func TestToolHistoryWithoutToolsGetsToolConfig(t *testing.T) {
	req := provider.ChatRequest{
		Model: "us.anthropic.claude-sonnet-4-6",
		Messages: []provider.ChatMessage{
			{Role: provider.RoleUser, Content: "go"},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ChatToolCall{{ID: "a", Name: "bash", Arguments: ""}}},
			{Role: provider.RoleTool, ToolCallID: "a"},
			{Role: provider.RoleUser, Content: "summarize"},
		},
	}
	b, _ := buildConverseBody(req, req.Model)
	var body struct {
		Messages   []cvMessage   `json:"messages"`
		ToolConfig *cvToolConfig `json:"toolConfig"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if body.ToolConfig == nil || len(body.ToolConfig.Tools) != 1 || body.ToolConfig.Tools[0].ToolSpec.Name != "bash" {
		t.Errorf("toolConfig = %s", b)
	}
	tr := body.Messages[2].Content[0].ToolResult
	if tr == nil || tr.Content[0].Text == nil || *tr.Content[0].Text == "" {
		t.Errorf("empty tool result must carry placeholder text: %s", b)
	}
	if string(body.Messages[1].Content[0].ToolUse.Input) != "{}" {
		t.Errorf("empty args must become {}: %s", b)
	}
}

func TestListModels(t *testing.T) {
	var pages int
	f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch r.URL.Path {
		case "/foundation-models":
			_, _ = io.WriteString(w, `{"modelSummaries":[
				{"modelId":"anthropic.claude-sonnet-4-6","modelName":"Claude Sonnet 4.6","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["INFERENCE_PROFILE"],"responseStreamingSupported":true},
				{"modelId":"anthropic.claude-3-haiku-20240307-v1:0","modelName":"Claude 3 Haiku","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["ON_DEMAND"],"responseStreamingSupported":true,"modelLifecycle":{"status":"LEGACY"}},
				{"modelId":"amazon.titan-embed-text-v2:0","modelName":"Titan Embed","providerName":"Amazon","inferenceTypesSupported":["ON_DEMAND"],"responseStreamingSupported":false}]}`)
		case "/inference-profiles":
			pages++
			if r.URL.Query().Get("nextToken") == "" {
				_, _ = io.WriteString(w, `{"inferenceProfileSummaries":[{"inferenceProfileId":"us.anthropic.claude-sonnet-4-6","inferenceProfileName":"US Claude Sonnet 4.6","status":"ACTIVE","type":"SYSTEM_DEFINED"}],"nextToken":"tok/2"}`)
			} else {
				_, _ = io.WriteString(w, `{"inferenceProfileSummaries":[{"inferenceProfileId":"global.anthropic.claude-opus-5-5","inferenceProfileName":"Global Claude Opus 5.5","status":"ACTIVE"}]}`)
			}
		default:
			w.WriteHeader(404)
		}
	})
	p := sigv4Provider(t, srv)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	want := "anthropic.claude-3-haiku-20240307-v1:0,global.anthropic.claude-opus-5-5,us.anthropic.claude-sonnet-4-6"
	if strings.Join(ids, ",") != want {
		t.Errorf("ids = %v, want %s", ids, want)
	}
	if models[0].Lifecycle != provider.LifecycleDeprecated || models[1].ContextLength != 1_000_000 {
		t.Errorf("models = %+v", models)
	}
	if pages != 2 {
		t.Errorf("pagination: %d pages", pages)
	}
	rec := f.last()
	if rec.method != "GET" || !strings.Contains(rec.rawQuery, "nextToken=tok%2F2") {
		t.Errorf("last request = %s %s?%s", rec.method, rec.path, rec.rawQuery)
	}
	verifySigV4(t, rec, "us-west-2")

	// Listing forbidden (inference-only IAM): fall back to the static list.
	_, srv2 := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"message":"not authorized to perform: bedrock:ListFoundationModels"}`)
	})
	p2 := bearerProvider(t, srv2)
	models, err = p2.ListModels(context.Background())
	if err != nil || len(models) != len(StaticModels()) {
		t.Errorf("fallback = %d models, err %v", len(models), err)
	}
	if _, err := p2.LiveModels(context.Background()); err == nil {
		t.Errorf("LiveModels should surface the error")
	}
	if _, err := provider.ListModels(context.Background(), p2); err != nil {
		t.Errorf("provider.ListModels: %v", err)
	}
}

func TestStaticModelsAndHelpers(t *testing.T) {
	ms := StaticModels()
	seen := map[string]bool{}
	for _, m := range ms {
		if seen[m.ID] || !strings.Contains(m.ID, "anthropic.claude") || m.ContextLength == 0 {
			t.Errorf("bad static entry %+v", m)
		}
		seen[m.ID] = true
	}
	if !seen[DefaultModel] {
		t.Errorf("DefaultModel %s not in static list", DefaultModel)
	}
	for in, want := range map[string]string{
		"us.anthropic.claude-opus-5-5":      "anthropic.claude-opus-5-5",
		"global.anthropic.claude-haiku-5-5": "anthropic.claude-haiku-5-5",
		"anthropic.claude-sonnet-4-6":       "anthropic.claude-sonnet-4-6",
		"arn:aws:bedrock:us-east-1:111122223333:inference-profile/eu.anthropic.claude-sonnet-5-5": "anthropic.claude-sonnet-5-5",
	} {
		if got := baseModelID(in); got != want {
			t.Errorf("baseModelID(%s) = %s", in, got)
		}
	}
	for stop, want := range map[string]string{
		"end_turn": provider.FinishStop, "tool_use": provider.FinishToolCalls, "max_tokens": provider.FinishLength,
		"guardrail_intervened": provider.FinishContentFilter, "content_filtered": provider.FinishContentFilter,
		"model_context_window_exceeded": provider.FinishLength,
	} {
		if got := finishReason(stop); got != want {
			t.Errorf("finishReason(%s) = %s", stop, got)
		}
	}
}

func TestRedirectsRefused(t *testing.T) {
	_, target := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		t.Errorf("credentials followed a redirect to %s", r.URL)
	})
	_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	})
	p, err := New(Config{
		Region: "us-east-1", BaseURL: srv.URL, BearerToken: fakeBearer,
		Getenv: func(string) string { return "" }, HomeDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), provider.ChatRequest{Model: "us.anthropic.claude-opus-5-5"}); err == nil {
		t.Errorf("redirect was followed")
	}
}
