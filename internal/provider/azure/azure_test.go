package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

// All tests use 127.0.0.1 httptest servers, fake keys/tokens and a fake `az`
// script; nothing reads the user's ~/.azure.

func fakeEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type rec struct {
	method, path, rawQuery string
	header                 http.Header
	body                   map[string]any
}

type fakeAzure struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []rec
	replies []func(w http.ResponseWriter)
}

func newFakeAzure(t *testing.T, replies ...func(w http.ResponseWriter)) *fakeAzure {
	f := &fakeAzure{replies: replies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		n := len(f.reqs)
		f.reqs = append(f.reqs, rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), m})
		f.mu.Unlock()
		if n < len(f.replies) {
			f.replies[n](w)
			return
		}
		w.WriteHeader(500)
	}))
	t.Cleanup(f.Close)
	return f
}

func jsonReply(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func sse(chunks ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
}

const okBody = `{"id":"c1","model":"gpt-5","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`

func userReq(model string) provider.ChatRequest {
	return provider.ChatRequest{Model: model, MaxTokens: 256, Messages: []provider.ChatMessage{{Role: "user", Content: "x"}}}
}

func TestAPIKeyV1(t *testing.T) {
	f := newFakeAzure(t, jsonReply(200, okBody))
	p, err := FromEnv(Options{Getenv: fakeEnv(map[string]string{
		EnvEndpoint: f.URL + "/openai/v1/", EnvAPIKey: "fake-key",
	})})
	if err != nil {
		t.Fatal(err)
	}
	if !p.UsesV1() || p.AuthMode() != "api-key" || p.Endpoint() != f.URL {
		t.Fatalf("v1=%v mode=%s endpoint=%s", p.UsesV1(), p.AuthMode(), p.Endpoint())
	}
	resp, err := p.Chat(context.Background(), userReq("my-gpt5"))
	if err != nil || resp.Content != "hi" || resp.Usage.TotalTokens != 4 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	r := f.reqs[0]
	if r.path != "/openai/v1/chat/completions" || r.rawQuery != "" {
		t.Fatalf("url = %s?%s", r.path, r.rawQuery)
	}
	if r.header.Get("api-key") != "fake-key" || r.header.Get("Authorization") != "" {
		t.Fatalf("headers = %v", r.header)
	}
	if r.body["model"] != "my-gpt5" || r.body["max_completion_tokens"] != float64(256) {
		t.Fatalf("body = %v", r.body)
	}
}

func TestDeploymentsURLStyleAndLegacyDowngrade(t *testing.T) {
	f := newFakeAzure(t, jsonReply(200, okBody), sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), jsonReply(200, okBody))
	p, err := New(Config{Endpoint: f.URL, APIKey: "k", APIVersion: "2024-10-21"})
	if err != nil {
		t.Fatal(err)
	}
	req := userReq("prod-gpt4o")
	req.Effort = "high"
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	r := f.reqs[0]
	if r.path != "/openai/deployments/prod-gpt4o/chat/completions" || r.rawQuery != "api-version=2024-10-21" {
		t.Fatalf("url = %s?%s", r.path, r.rawQuery)
	}
	if r.body["max_completion_tokens"] != float64(256) {
		t.Fatalf("2024-10-21 should keep max_completion_tokens: %v", r.body)
	}
	if _, ok := r.body["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort sent on 2024-10-21: %v", r.body)
	}
	if got := p.ChatURL("d"); got != f.URL+"/openai/deployments/d/chat/completions?api-version=2024-10-21" {
		t.Fatal(got)
	}

	// An old GA version: max_tokens, no stream_options.
	old, _ := New(Config{Endpoint: f.URL, APIKey: "k", APIVersion: "2024-06-01"})
	if _, err := old.StreamChat(context.Background(), userReq("dep"), nil); err != nil {
		t.Fatal(err)
	}
	b := f.reqs[1].body
	if b["max_tokens"] != float64(256) || b["max_completion_tokens"] != nil || b["stream_options"] != nil || b["stream"] != true {
		t.Fatalf("legacy body = %v", b)
	}

	// v1 preview adds the query.
	pv, _ := New(Config{Endpoint: f.URL, APIKey: "k", APIVersion: "preview"})
	if _, err := pv.Chat(context.Background(), userReq("dep")); err != nil {
		t.Fatal(err)
	}
	if f.reqs[2].path != "/openai/v1/chat/completions" || f.reqs[2].rawQuery != "api-version=preview" {
		t.Fatalf("preview url = %s?%s", f.reqs[2].path, f.reqs[2].rawQuery)
	}
	if _, err := New(Config{Endpoint: f.URL, APIKey: "k", APIVersion: "banana"}); err == nil {
		t.Fatal("bad version accepted")
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, bad := range []string{"", "http://example.com", "https://user:pw@x.openai.azure.com", "not a url"} {
		if _, err := New(Config{Endpoint: bad, APIKey: "k"}); err == nil {
			t.Errorf("endpoint %q accepted", bad)
		}
	}
	p, err := New(Config{Endpoint: "https://res.openai.azure.com/openai", APIKey: "k"})
	if err != nil || p.Endpoint() != "https://res.openai.azure.com" {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := New(Config{Endpoint: "https://res.openai.azure.com"}); err == nil {
		t.Fatal("no credentials accepted")
	}
}

func TestStreamToolCallRoundTrip(t *testing.T) {
	f := newFakeAzure(t,
		sse(
			`{"id":"","choices":[],"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{}}]}`,
			`{"id":"chatcmpl-1","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"grep","arguments":""}}]}}]}`,
			`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pattern\":"}}]}}]}`,
			`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"TODO\"}"}}]}}]}`,
			`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":7,"total_tokens":27}}`,
		),
		sse(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"content":"found 3"},"finish_reason":"stop"}]}`),
	)
	p, _ := New(Config{Endpoint: f.URL, APIKey: "k"})
	req := userReq("dep")
	req.Tools = []provider.ChatTool{{Name: "grep", Parameters: json.RawMessage(`{"type":"object"}`)}}
	resp, err := p.StreamChat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_1" || resp.ToolCalls[0].Arguments != `{"pattern":"TODO"}` || resp.FinishReason != "tool_calls" || resp.Usage.TotalTokens != 27 {
		t.Fatalf("resp = %+v", resp)
	}
	if f.reqs[0].body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("include_usage missing: %v", f.reqs[0].body)
	}
	req.Messages = append(req.Messages,
		provider.ChatMessage{Role: "assistant", ToolCalls: resp.ToolCalls},
		provider.ChatMessage{Role: "tool", ToolCallID: "call_1", Content: "3 matches"})
	var got strings.Builder
	resp2, err := p.StreamChat(context.Background(), req, func(d string) { got.WriteString(d) })
	if err != nil || resp2.Content != "found 3" || got.String() != "found 3" {
		t.Fatalf("turn2 %q %v", resp2.Content, err)
	}
	msgs := f.reqs[1].body["messages"].([]any)
	if msgs[2].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %v", msgs[2])
	}
}

func TestErrorMapping(t *testing.T) {
	f := newFakeAzure(t,
		jsonReply(429, `{"error":{"code":"429","message":"Requests to the ChatCompletions_Create Operation have exceeded token rate limit of your current quota tier."}}`),
		jsonReply(401, `{"error":{"code":"401","message":"Access denied due to invalid subscription key or wrong API endpoint."}}`),
		jsonReply(404, `{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`),
	)
	p, _ := New(Config{Endpoint: f.URL, APIKey: "very-secret-key"})
	cases := []struct {
		cat   provider.Category
		retry bool
		want  string
	}{
		{provider.CategoryRateLimit, true, "Rate limited by Azure OpenAI"},
		{provider.CategoryAuth, false, "AZURE_OPENAI_API_KEY"},
		{provider.CategoryUnknown, false, `deployment "dep" was not found`},
	}
	for i, c := range cases {
		_, err := p.Chat(context.Background(), userReq("dep"))
		var ae *provider.APIError
		if !errors.As(err, &ae) || ae.Category != c.cat || ae.Retryable != c.retry || ae.Provider != "azure" || !strings.Contains(ae.Message, c.want) {
			t.Fatalf("case %d: %#v", i, err)
		}
		if strings.Contains(ae.Message, "very-secret-key") {
			t.Fatal("key leaked")
		}
	}
}

func TestClientCredentials(t *testing.T) {
	var hits atomic.Int32
	var form url.Values
	var path string
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = r.ParseForm()
		form, path = r.PostForm, r.URL.Path
		_, _ = io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"fake-entra-token"}`)
	}))
	defer tokSrv.Close()
	f := newFakeAzure(t, jsonReply(200, okBody), jsonReply(200, okBody))
	p, err := FromEnv(Options{Getenv: fakeEnv(map[string]string{
		EnvEndpoint: f.URL, EnvTenantID: "tenant-1", EnvClientID: "client-1", EnvClientSecret: "fake-secret", EnvAuthorityHost: tokSrv.URL,
	})})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := p.Chat(context.Background(), userReq("dep")); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("token endpoint hits = %d (no caching)", hits.Load())
	}
	if path != "/tenant-1/oauth2/v2.0/token" || form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "client-1" ||
		form.Get("client_secret") != "fake-secret" || form.Get("scope") != DefaultScope {
		t.Fatalf("path=%s form=%v", path, form)
	}
	if got := f.reqs[1].header.Get("Authorization"); got != "Bearer fake-entra-token" || f.reqs[1].header.Get("api-key") != "" {
		t.Fatalf("auth header = %q", got)
	}
}

func TestClientCredentialsFailureIsAuth(t *testing.T) {
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.\r\nTrace ID: x"}`)
	}))
	defer tokSrv.Close()
	p, _ := New(Config{Endpoint: "https://res.openai.azure.com", Tokens: &ClientCredentials{TenantID: "t", ClientID: "c", ClientSecret: "fake-secret-value", AuthorityHost: tokSrv.URL}})
	_, err := p.Chat(context.Background(), userReq("dep"))
	var ae *provider.APIError
	if !errors.As(err, &ae) || ae.Category != provider.CategoryAuth || ae.Retryable {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "AADSTS7000215") || strings.Contains(err.Error(), "fake-secret-value") || strings.Contains(err.Error(), "Trace ID") {
		t.Fatalf("message = %q", err.Error())
	}
}

// writeFakeAz puts a fake `az` first on PATH that records its args and prints
// a token. Unix only (a shell script).
func writeFakeAz(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake az is a shell script")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "az")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestAzureCLI(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	dir := writeFakeAz(t, fmt.Sprintf(`echo "$@" >> "$(dirname "$0")/calls"
echo '{"accessToken":"fake-cli-token","expiresOn":"2099-01-01 00:00:00.000000","expires_on":%d,"tokenType":"Bearer"}'
`, exp))
	f := newFakeAzure(t, jsonReply(200, okBody), jsonReply(200, okBody))
	p, err := FromEnv(Options{Getenv: fakeEnv(map[string]string{EnvEndpoint: f.URL, EnvTenantID: "tid"})})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := p.Chat(context.Background(), userReq("dep")); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reqs[0].header.Get("Authorization"); got != "Bearer fake-cli-token" {
		t.Fatalf("auth = %q", got)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 1 {
		t.Fatalf("az ran %d times (no caching): %q", len(lines), calls)
	}
	if lines[0] != "account get-access-token --output json --scope "+DefaultScope+" --tenant tid" {
		t.Fatalf("args = %q", lines[0])
	}
	tok, _ := (&AzureCLI{}).Token(context.Background())
	if tok.Expiry.Unix() != exp {
		t.Fatalf("expiry = %v", tok.Expiry)
	}
}

func TestAzureCLINotLoggedIn(t *testing.T) {
	writeFakeAz(t, `echo "ERROR: Please run 'az login' to setup account." >&2
exit 1
`)
	p, _ := New(Config{Endpoint: "https://res.openai.azure.com", Tokens: &AzureCLI{}})
	_, err := p.Chat(context.Background(), userReq("dep"))
	var ae *provider.APIError
	if !errors.As(err, &ae) || ae.Category != provider.CategoryAuth || !strings.Contains(err.Error(), "az login") {
		t.Fatalf("err = %v", err)
	}
}

func TestEntra401InvalidatesToken(t *testing.T) {
	f := newFakeAzure(t, jsonReply(401, `{"error":{"code":"401","message":"expired"}}`), jsonReply(200, okBody))
	var n atomic.Int32
	src := tokenFunc(func(context.Context) (Token, error) {
		return Token{AccessToken: fmt.Sprintf("t%d", n.Add(1)), Expiry: time.Now().Add(time.Hour)}, nil
	})
	p, _ := New(Config{Endpoint: f.URL, Tokens: src})
	_, _ = p.Chat(context.Background(), userReq("dep"))
	if _, err := p.Chat(context.Background(), userReq("dep")); err != nil {
		t.Fatal(err)
	}
	if f.reqs[1].header.Get("Authorization") != "Bearer t2" {
		t.Fatalf("token not refreshed after 401: %q", f.reqs[1].header.Get("Authorization"))
	}
}

type tokenFunc func(context.Context) (Token, error)

func (f tokenFunc) Token(ctx context.Context) (Token, error) { return f(ctx) }

func TestListModels(t *testing.T) {
	f := newFakeAzure(t,
		jsonReply(200, `{"data":[{"id":"gpt5-prod","model":"gpt-5","status":"succeeded"},{"id":"old","model":"gpt-35-turbo","status":"deleting"}]}`),
		jsonReply(404, `{"error":{"code":"404","message":"Resource not found"}}`),
		jsonReply(404, `{}`),
	)
	p, _ := New(Config{Endpoint: f.URL, APIKey: "k", Deployments: []string{"typed-dep"}})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gpt5-prod" || models[0].Description != "model: gpt-5" || models[1].ID != "typed-dep" {
		t.Fatalf("models = %+v", models)
	}
	if f.reqs[0].path != "/openai/deployments" || f.reqs[0].rawQuery != "api-version="+legacyListAPIVersion || f.reqs[0].header.Get("api-key") != "k" {
		t.Fatalf("req = %+v", f.reqs[0])
	}
	// Listing unsupported → remembered names.
	models, err = p.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "typed-dep" {
		t.Fatalf("fallback = %+v %v", models, err)
	}
	// Nothing known → a clear error.
	p2, _ := New(Config{Endpoint: f.URL, APIKey: "k"})
	if _, err := p2.ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "deployment name") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoDeployment(t *testing.T) {
	p, _ := New(Config{Endpoint: "https://res.openai.azure.com", APIKey: "k"})
	if _, err := p.Chat(context.Background(), userReq("")); err == nil || !strings.Contains(err.Error(), "deployment") {
		t.Fatalf("err = %v", err)
	}
}
