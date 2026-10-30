package vertex

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

// All tests run against 127.0.0.1 httptest servers with fake credentials;
// discovery is driven by an injected getenv/home so no real gcloud file is
// ever read.

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func noHome() (string, error) { return "", errors.New("no home in tests") }

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// tokenServer is a fake OAuth token endpoint.
type tokenServer struct {
	*httptest.Server
	hits  atomic.Int32
	mu    sync.Mutex
	forms []url.Values
	fail  int // status to fail with (0 = succeed)
}

func newTokenServer(t *testing.T) *tokenServer {
	ts := &tokenServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.hits.Add(1)
		_ = r.ParseForm()
		ts.mu.Lock()
		ts.forms = append(ts.forms, r.PostForm)
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if ts.fail != 0 {
			w.WriteHeader(ts.fail)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":"fake-access-%d","expires_in":3600,"token_type":"Bearer"}`, ts.hits.Load())
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tokenServer) lastForm() url.Values {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.forms[len(ts.forms)-1]
}

func TestServiceAccountJWTSigning(t *testing.T) {
	key, keyPEM := testKey(t)
	ts := newTokenServer(t)
	sa, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "proj-from-file", "private_key_id": "kid-1",
		"private_key": keyPEM, "client_email": "bot@proj.iam.gserviceaccount.com",
		"token_uri": "https://evil.example.com/token", // must be ignored
	})
	fixed := time.Unix(1_700_000_000, 0)
	creds, err := CredentialsFromJSON(sa, CredentialOptions{TokenURL: ts.URL + "/token", Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	if creds.ProjectID != "proj-from-file" {
		t.Fatalf("project = %q", creds.ProjectID)
	}
	tok, err := creds.Tokens.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "fake-access-1" || !tok.Expiry.Equal(fixed.Add(time.Hour)) {
		t.Fatalf("token = %+v", tok)
	}
	form := ts.lastForm()
	if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant_type = %q", form.Get("grant_type"))
	}
	parts := strings.Split(form.Get("assertion"), ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d parts", len(parts))
	}
	dec := func(s string) map[string]any {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	h, c := dec(parts[0]), dec(parts[1])
	if h["alg"] != "RS256" || h["typ"] != "JWT" || h["kid"] != "kid-1" {
		t.Fatalf("header = %v", h)
	}
	if c["iss"] != "bot@proj.iam.gserviceaccount.com" || c["scope"] != cloudPlatformScope || c["aud"] != ts.URL+"/token" {
		t.Fatalf("claims = %v", c)
	}
	if c["iat"].(float64) != 1_700_000_000 || c["exp"].(float64) != 1_700_003_600 {
		t.Fatalf("iat/exp = %v/%v", c["iat"], c["exp"])
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestChooseTokenURL(t *testing.T) {
	cases := map[string]string{
		"":                                     defaultTokenURL,
		"https://oauth2.googleapis.com/token":  "https://oauth2.googleapis.com/token",
		"https://sts.googleapis.com/v1/token":  "https://sts.googleapis.com/v1/token",
		"http://oauth2.googleapis.com/token":   defaultTokenURL,
		"https://attacker.example/token":       defaultTokenURL,
		"https://googleapis.com.evil.io/token": defaultTokenURL,
	}
	for in, want := range cases {
		if got := chooseTokenURL("", in); got != want {
			t.Errorf("chooseTokenURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeADC(t *testing.T, dir string, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	p := filepath.Join(dir, "application_default_credentials.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuthorizedUserRefreshAndCaching(t *testing.T) {
	ts := newTokenServer(t)
	dir := t.TempDir()
	writeADC(t, dir, map[string]string{
		"type": "authorized_user", "client_id": "cid", "client_secret": "csecret",
		"refresh_token": "fake-refresh", "quota_project_id": "quota-proj",
	})
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	opts := CredentialOptions{
		Getenv:   fakeEnv(map[string]string{"CLOUDSDK_CONFIG": dir}),
		HomeDir:  noHome,
		TokenURL: ts.URL,
		Now:      clock,
	}
	creds, err := FindDefaultCredentials(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if creds.QuotaProject != "quota-proj" || creds.ProjectID != "quota-proj" {
		t.Fatalf("creds = %+v", creds)
	}
	// Swap in the controllable clock for the cache too.
	creds.Tokens.(*cachingSource).now = clock
	for i := 0; i < 3; i++ {
		tok, err := creds.Tokens.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tok.AccessToken != "fake-access-1" {
			t.Fatalf("token %d = %q (cache miss)", i, tok.AccessToken)
		}
	}
	if ts.hits.Load() != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", ts.hits.Load())
	}
	f := ts.lastForm()
	if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "fake-refresh" || f.Get("client_id") != "cid" || f.Get("client_secret") != "csecret" {
		t.Fatalf("form = %v", f)
	}
	// Near expiry → refresh.
	mu.Lock()
	now = now.Add(59 * time.Minute)
	mu.Unlock()
	tok, err := creds.Tokens.Token(context.Background())
	if err != nil || tok.AccessToken != "fake-access-2" {
		t.Fatalf("after expiry: %q %v", tok.AccessToken, err)
	}
	// Invalidate → refresh.
	creds.Tokens.(invalidator).Invalidate()
	tok, _ = creds.Tokens.Token(context.Background())
	if tok.AccessToken != "fake-access-3" {
		t.Fatalf("after invalidate: %q", tok.AccessToken)
	}
}

func TestRefreshFailureIsAuthErrorWithoutSecrets(t *testing.T) {
	ts := newTokenServer(t)
	ts.fail = http.StatusBadRequest
	dir := t.TempDir()
	writeADC(t, dir, map[string]string{"type": "authorized_user", "client_id": "cid", "client_secret": "csecret", "refresh_token": "super-secret-refresh"})
	opts := CredentialOptions{Getenv: fakeEnv(map[string]string{"CLOUDSDK_CONFIG": dir}), HomeDir: noHome, TokenURL: ts.URL}
	creds, err := FindDefaultCredentials(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Project: "p", Tokens: creds.Tokens, BaseURL: "http://127.0.0.1:1/x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), provider.ChatRequest{Model: "gemini-2.5-pro"})
	var ae *provider.APIError
	if !errors.As(err, &ae) || ae.Category != provider.CategoryAuth || ae.Retryable {
		t.Fatalf("err = %#v", err)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(ae.Detail, "super-secret") || strings.Contains(err.Error(), "csecret") {
		t.Fatalf("secret leaked: %q / %q", err.Error(), ae.Detail)
	}
	if !strings.Contains(ae.Detail, "invalid_grant") {
		t.Fatalf("detail = %q", ae.Detail)
	}
}

func TestDiscoveryOrderAndErrors(t *testing.T) {
	// Nothing configured, not on GCE → ErrNoCredentials.
	_, err := FindDefaultCredentials(context.Background(), CredentialOptions{
		Getenv: fakeEnv(nil), HomeDir: noHome, DMIProductFile: filepath.Join(t.TempDir(), "missing"),
	})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v", err)
	}
	// Unsupported type.
	dir := t.TempDir()
	p := writeADC(t, dir, map[string]string{"type": "external_account"})
	_, err = FindDefaultCredentials(context.Background(), CredentialOptions{
		Getenv: fakeEnv(map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": p}), HomeDir: noHome,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported credential type") {
		t.Fatalf("err = %v", err)
	}
	// GOOGLE_APPLICATION_CREDENTIALS wins over gcloud ADC.
	_, keyPEM := testKey(t)
	saDir := t.TempDir()
	saPath := filepath.Join(saDir, "sa.json")
	b, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "sa-proj", "private_key": keyPEM, "client_email": "x@y"})
	_ = os.WriteFile(saPath, b, 0o600)
	adcDir := t.TempDir()
	writeADC(t, adcDir, map[string]string{"type": "authorized_user", "client_id": "c", "refresh_token": "r"})
	c, err := FindDefaultCredentials(context.Background(), CredentialOptions{
		Getenv: fakeEnv(map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": saPath, "CLOUDSDK_CONFIG": adcDir}), HomeDir: noHome,
	})
	if err != nil || c.ProjectID != "sa-proj" || c.Source != saPath {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

func TestMetadataServer(t *testing.T) {
	var gotFlavor, gotPath string
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFlavor, gotPath = r.Header.Get("Metadata-Flavor"), r.URL.Path
		_, _ = io.WriteString(w, `{"access_token":"fake-md-token","expires_in":1800}`)
	}))
	defer md.Close()
	host := strings.TrimPrefix(md.URL, "http://")
	c, err := FindDefaultCredentials(context.Background(), CredentialOptions{
		Getenv: fakeEnv(map[string]string{"GCE_METADATA_HOST": host}), HomeDir: noHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Tokens.Token(context.Background())
	if err != nil || tok.AccessToken != "fake-md-token" {
		t.Fatalf("tok=%q err=%v", tok.AccessToken, err)
	}
	if gotFlavor != "Google" || gotPath != "/computeMetadata/v1/instance/service-accounts/default/token" {
		t.Fatalf("flavor=%q path=%q", gotFlavor, gotPath)
	}
	// DMI detection.
	dmi := filepath.Join(t.TempDir(), "product_name")
	_ = os.WriteFile(dmi, []byte("Google Compute Engine\n"), 0o644)
	if !onGCE(CredentialOptions{Getenv: fakeEnv(nil), DMIProductFile: dmi}) && isLinux() {
		t.Fatal("DMI product name not detected")
	}
	_ = os.WriteFile(dmi, []byte("ThinkPad X1\n"), 0o644)
	if onGCE(CredentialOptions{Getenv: fakeEnv(nil), DMIProductFile: dmi}) {
		t.Fatal("false GCE detection")
	}
}

func TestEndpointURLs(t *testing.T) {
	for loc, want := range map[string]string{
		"global":      "https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/endpoints/openapi",
		"us-central1": "https://us-central1-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-central1/endpoints/openapi",
		"":            "https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/endpoints/openapi",
	} {
		p, err := New(Config{Project: "my-proj", Location: loc, Tokens: StaticTokenSource("t")})
		if err != nil {
			t.Fatal(err)
		}
		if p.Endpoint() != want {
			t.Errorf("%q: %s, want %s", loc, p.Endpoint(), want)
		}
	}
	if _, err := New(Config{Location: "global", Tokens: StaticTokenSource("t")}); err == nil {
		t.Fatal("missing project accepted")
	}
	if _, err := New(Config{Project: "p", Location: "bad/loc", Tokens: StaticTokenSource("t")}); err == nil {
		t.Fatal("bad location accepted")
	}
	if WireModel("gemini-2.5-pro") != "google/gemini-2.5-pro" || WireModel("meta/llama") != "meta/llama" {
		t.Fatal("WireModel")
	}
}

func TestFromEnv(t *testing.T) {
	ts := newTokenServer(t)
	dir := t.TempDir()
	writeADC(t, dir, map[string]string{"type": "authorized_user", "client_id": "c", "refresh_token": "r"})
	p, err := FromEnv(context.Background(), Options{Credentials: CredentialOptions{
		Getenv:   fakeEnv(map[string]string{"CLOUDSDK_CONFIG": dir, "GOOGLE_CLOUD_PROJECT": "fallback-proj", "VERTEXAI_LOCATION": "europe-west4"}),
		HomeDir:  noHome,
		TokenURL: ts.URL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Project() != "fallback-proj" || p.Location() != "europe-west4" {
		t.Fatalf("project=%q location=%q", p.Project(), p.Location())
	}
	if !strings.HasPrefix(p.Endpoint(), "https://europe-west4-aiplatform.googleapis.com/") {
		t.Fatal(p.Endpoint())
	}
}

// fakeVertex records requests and replies with scripted bodies.
type fakeVertex struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []recorded
	replies []func(w http.ResponseWriter)
}

type recorded struct {
	path, auth, quota, accept string
	body                      map[string]any
}

func newFakeVertex(t *testing.T, replies ...func(w http.ResponseWriter)) *fakeVertex {
	fv := &fakeVertex{replies: replies}
	fv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		fv.mu.Lock()
		n := len(fv.reqs)
		fv.reqs = append(fv.reqs, recorded{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-goog-user-project"), r.Header.Get("Accept"), m})
		fv.mu.Unlock()
		if n < len(fv.replies) {
			fv.replies[n](w)
			return
		}
		w.WriteHeader(500)
	}))
	t.Cleanup(fv.Close)
	return fv
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

func jsonReply(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestStreamToolCallRoundTripWithSignatures(t *testing.T) {
	fv := newFakeVertex(t,
		// Turn 1: text, then two parallel tool calls each in one chunk, both
		// with upstream index 0 and no id on the second; the first carries
		// the thought signature.
		sse(
			`{"id":"r1","model":"google/gemini-3.8-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me "}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"check."}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"},"extra_content":{"google":{"thought_signature":"SIG_A"}}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		),
		sse(`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`),
	)
	p, err := New(Config{Project: "proj", Location: "global", Tokens: StaticTokenSource("fake-token"), QuotaProject: "qp", BaseURL: fv.URL + "/v1/projects/proj/locations/global/endpoints/openapi"})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	req := provider.ChatRequest{
		Model:     "gemini-3.8-flash",
		Messages:  []provider.ChatMessage{{Role: provider.RoleSystem, Content: "sys"}, {Role: provider.RoleUser, Content: "read a and b"}},
		Tools:     []provider.ChatTool{{Name: "read_file", Description: "read", Parameters: json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"path":{"type":"string"}}}`)}},
		MaxTokens: 1000,
		Effort:    "high",
	}
	resp, err := p.StreamChat(context.Background(), req, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Let me check." || strings.Join(deltas, "") != "Let me check." {
		t.Fatalf("content=%q deltas=%v", resp.Content, deltas)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].ID != "call_a" || resp.ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("call 0 = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID == "" || resp.ToolCalls[1].ID == "call_a" || resp.ToolCalls[1].Arguments != `{"path":"b.go"}` {
		t.Fatalf("call 1 = %+v", resp.ToolCalls[1])
	}
	if resp.FinishReason != "tool_calls" || resp.Usage.TotalTokens != 15 {
		t.Fatalf("finish=%q usage=%+v", resp.FinishReason, resp.Usage)
	}

	r := fv.reqs[0]
	if r.path != "/v1/projects/proj/locations/global/endpoints/openapi/chat/completions" {
		t.Fatalf("path = %s", r.path)
	}
	if r.auth != "Bearer fake-token" || r.quota != "qp" || r.accept != "text/event-stream" {
		t.Fatalf("headers = %+v", r)
	}
	if r.body["model"] != "google/gemini-3.8-flash" || r.body["stream"] != true || r.body["reasoning_effort"] != "high" || r.body["max_completion_tokens"] != float64(1000) {
		t.Fatalf("body = %v", r.body)
	}
	params := r.body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if _, ok := params["$schema"]; ok || params["type"] != "object" {
		t.Fatalf("parameters = %v", params)
	}

	// Turn 2: replay history; the signature must come back on call_a.
	req.Messages = append(req.Messages,
		provider.ChatMessage{Role: provider.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls},
		provider.ChatMessage{Role: provider.RoleTool, ToolCallID: resp.ToolCalls[0].ID, Content: "A"},
		provider.ChatMessage{Role: provider.RoleTool, ToolCallID: resp.ToolCalls[1].ID, Content: "B"},
	)
	resp2, err := p.StreamChat(context.Background(), req, nil)
	if err != nil || resp2.Content != "done" {
		t.Fatalf("turn 2: %q %v", resp2.Content, err)
	}
	msgs := fv.reqs[1].body["messages"].([]any)
	asst := msgs[2].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if sig := signatureOf(calls[0].(map[string]any)); sig != "SIG_A" {
		t.Fatalf("signature not replayed: %v", calls[0])
	}
	if sig := signatureOf(calls[1].(map[string]any)); sig != "" {
		t.Fatalf("second parallel call got signature %q", sig)
	}
}

func TestSentinelForUnknownHistory(t *testing.T) {
	fv := newFakeVertex(t,
		jsonReply(200, `{"id":"x","model":"google/gemini-3.1-pro","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`),
		jsonReply(200, `{"id":"y","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`),
	)
	p, _ := New(Config{Project: "p", Tokens: StaticTokenSource("t"), BaseURL: fv.URL})
	hist := []provider.ChatMessage{
		{Role: provider.RoleUser, Content: "hi"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ChatToolCall{{ID: "old_1", Name: "ls", Arguments: "{}"}}},
		{Role: provider.RoleTool, ToolCallID: "old_1", Content: "x"},
	}
	if _, err := p.Chat(context.Background(), provider.ChatRequest{Model: "gemini-3.1-pro", Messages: hist}); err != nil {
		t.Fatal(err)
	}
	c := fv.reqs[0].body["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if signatureOf(c) != skipSignature {
		t.Fatalf("gemini-3 history without signature: %v", c)
	}
	// Gemini 2.5 does not enforce signatures → no sentinel.
	if _, err := p.Chat(context.Background(), provider.ChatRequest{Model: "gemini-2.5-pro", Messages: hist}); err != nil {
		t.Fatal(err)
	}
	c = fv.reqs[1].body["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if _, ok := c["extra_content"]; ok {
		t.Fatalf("gemini-2.5 got extra_content: %v", c)
	}
}

func TestNonStreamToolCallsGetIDsAndSignatures(t *testing.T) {
	fv := newFakeVertex(t,
		jsonReply(200, `{"id":"x","choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"ls","arguments":"{}"},"extra_content":{"google":{"thought_signature":"SIG_NS"}}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`),
	)
	p, _ := New(Config{Project: "p", Tokens: StaticTokenSource("t"), BaseURL: fv.URL})
	resp, err := p.Chat(context.Background(), provider.ChatRequest{Model: "gemini-3-flash", Messages: []provider.ChatMessage{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID == "" || resp.Usage.TotalTokens != 3 {
		t.Fatalf("resp = %+v", resp)
	}
	if sig, _ := p.sigs.get(resp.ToolCalls[0].ID); sig != "SIG_NS" {
		t.Fatalf("signature not recorded: %q", sig)
	}
}

func TestErrorMapping(t *testing.T) {
	fv := newFakeVertex(t,
		jsonReply(429, `[{"error":{"code":429,"message":"Quota exceeded for aiplatform.googleapis.com/generate_content_requests_per_minute","status":"RESOURCE_EXHAUSTED"}}]`),
		jsonReply(429, `{"error":{"code":429,"message":"Quota exceeded for quota metric","status":"RESOURCE_EXHAUSTED"}}`),
		jsonReply(401, `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`),
		jsonReply(403, `{"error":{"code":403,"message":"Permission 'aiplatform.endpoints.predict' denied","status":"PERMISSION_DENIED"}}`),
	)
	inv := &countingSource{}
	p, _ := New(Config{Project: "p", Tokens: inv, BaseURL: fv.URL})
	req := provider.ChatRequest{Model: "gemini-2.5-flash", Messages: []provider.ChatMessage{{Role: "user", Content: "x"}}}
	check := func(wantCat provider.Category, wantRetry bool) {
		t.Helper()
		_, err := p.Chat(context.Background(), req)
		var ae *provider.APIError
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v", err)
		}
		if ae.Category != wantCat || ae.Retryable != wantRetry || ae.Provider != "vertex" || !strings.Contains(ae.Message, "Vertex AI") {
			t.Fatalf("got %+v", ae)
		}
	}
	check(provider.CategoryRateLimit, true)
	check(provider.CategoryRateLimit, true)
	before := inv.calls.Load()
	check(provider.CategoryAuth, false)
	// The 401 invalidated the cache → the next call mints a new token.
	check(provider.CategoryAuth, false)
	if inv.calls.Load() != before+1 {
		t.Fatalf("token source calls %d → %d, want a refresh after 401", before, inv.calls.Load())
	}
}

type countingSource struct{ calls atomic.Int32 }

func (c *countingSource) Token(context.Context) (Token, error) {
	n := c.calls.Add(1)
	return Token{AccessToken: fmt.Sprintf("tok-%d", n), Expiry: time.Now().Add(time.Hour)}, nil
}

func TestTokenNotSentToOtherHosts(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token sent to foreign host")
		}
	}))
	defer other.Close()
	tr := &transport{base: http.DefaultClient, host: "vertex.invalid", token: "secret", sigs: &signatureStore{}, tokens: StaticTokenSource("x")}
	req, _ := http.NewRequest(http.MethodGet, other.URL, nil)
	resp, err := tr.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestListModels(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1beta1/publishers/google/models" {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `{"publisherModels":[{"name":"publishers/google/models/gemini-2.5-pro","launchStage":"GA"},{"name":"publishers/google/models/gemini-9-flash","launchStage":"PUBLIC_PREVIEW"},{"name":"publishers/google/models/gemini-embedding-2"},{"name":"publishers/google/models/imagen-4"}]}`)
	}))
	defer srv.Close()
	p, _ := New(Config{Project: "p", Tokens: StaticTokenSource("fake"), BaseURL: srv.URL, APIHost: srv.URL})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gemini-2.5-pro" || models[0].ContextLength == 0 || models[1].ID != "gemini-9-flash" || models[1].Description != "public_preview" {
		t.Fatalf("models = %+v", models)
	}
	if auth != "Bearer fake" {
		t.Fatalf("auth = %q", auth)
	}
	// Failure → static list.
	p2, _ := New(Config{Project: "p", Tokens: StaticTokenSource("fake"), BaseURL: srv.URL, APIHost: srv.URL + "/nope"})
	models, err = p2.ListModels(context.Background())
	if err != nil || len(models) != len(StaticModels) {
		t.Fatalf("fallback: %d %v", len(models), err)
	}
}

func isLinux() bool { return runtime.GOOS == "linux" }
