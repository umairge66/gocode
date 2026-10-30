package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestOpenRouterListModels asserts the models endpoint path, the optional Bearer
// header, and that data[].{id,name,context_length,description} parse and sort.
func TestOpenRouterListModels(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[
			{"id":"z/model","name":"Zed","context_length":8000,"description":"last"},
			{"id":"a/model","name":"Aye","context_length":128000,"description":"first"},
			{"id":"m/model","context_length":4096}
		]}`)
	}))
	defer srv.Close()

	p := &OpenRouter{APIKey: "sk-or", BaseURL: srv.URL}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/models" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-or" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	// Sorted by ID.
	if models[0].ID != "a/model" || models[1].ID != "m/model" || models[2].ID != "z/model" {
		t.Errorf("order = %v %v %v", models[0].ID, models[1].ID, models[2].ID)
	}
	if models[0].Name != "Aye" || models[0].ContextLength != 128000 || models[0].Description != "first" {
		t.Errorf("parsed fields = %+v", models[0])
	}
	// Missing name falls back to the id.
	if models[1].Name != "m/model" {
		t.Errorf("name fallback = %q", models[1].Name)
	}
}

// TestOpenRouterListModelsNoKey asserts the Bearer is omitted when no key is set
// (the catalog needs none).
func TestOpenRouterListModelsNoKey(t *testing.T) {
	var gotAuth string
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, hadAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"x","name":"X"}]}`)
	}))
	defer srv.Close()

	if _, err := (&OpenRouter{BaseURL: srv.URL}).ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if hadAuth || gotAuth != "" {
		t.Errorf("Authorization must be omitted without a key, got %q", gotAuth)
	}
}

// TestOpenAIListModels asserts the /models path, the Bearer + org/project
// headers, that only ids are read (name=id, context length 0), and sorting.
func TestOpenAIListModels(t *testing.T) {
	var gotPath, gotAuth, gotOrg string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotOrg = r.Header.Get("OpenAI-Organization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[
			{"id":"gpt-5","object":"model"},
			{"id":"gpt-4o","object":"model"}
		]}`)
	}))
	defer srv.Close()

	p := &OpenAI{APIKey: "sk-oa", BaseURL: srv.URL + "/v1", Organization: "org-1"}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-oa" || gotOrg != "org-1" {
		t.Errorf("headers auth=%q org=%q", gotAuth, gotOrg)
	}
	if len(models) != 2 || models[0].ID != "gpt-4o" || models[1].ID != "gpt-5" {
		t.Fatalf("models = %+v", models)
	}
	if models[0].Name != "gpt-4o" || models[0].ContextLength != 0 {
		t.Errorf("name/context = %+v", models[0])
	}
}

// TestOpenAIListModelsPaging asserts the has_more/after cursor is followed.
func TestOpenAIListModelsPaging(t *testing.T) {
	var calls int32
	var gotAfter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) == 1 {
			io.WriteString(w, `{"data":[{"id":"gpt-4o"}],"has_more":true}`)
			return
		}
		gotAfter = r.URL.Query().Get("after")
		io.WriteString(w, `{"data":[{"id":"gpt-5"}],"has_more":false}`)
	}))
	defer srv.Close()

	models, err := (&OpenAI{APIKey: "k", BaseURL: srv.URL}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d", calls)
	}
	if gotAfter != "gpt-4o" {
		t.Errorf("after cursor = %q", gotAfter)
	}
	if len(models) != 2 || models[0].ID != "gpt-4o" || models[1].ID != "gpt-5" {
		t.Errorf("models = %+v", models)
	}
}

// TestAnthropicListModels asserts the /v1/models path, the x-api-key +
// anthropic-version headers, and data[].{id,display_name} parsing.
func TestAnthropicListModels(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[
			{"type":"model","id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5"},
			{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5"},
			{"type":"model","id":"claude-bare-1"}
		],"has_more":false}`)
	}))
	defer srv.Close()

	p := &Anthropic{APIKey: "sk-ant", BaseURL: srv.URL}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "sk-ant" || gotVersion != anthropicDefaultVersion {
		t.Errorf("headers key=%q version=%q", gotKey, gotVersion)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ID != "claude-bare-1" || models[1].ID != "claude-opus-5-5" || models[2].ID != "claude-sonnet-5-5" {
		t.Errorf("order = %+v", models)
	}
	if models[1].Name != "Claude Opus 5.5" {
		t.Errorf("display_name = %q", models[1].Name)
	}
	// Missing display_name falls back to the id.
	if models[0].Name != "claude-bare-1" {
		t.Errorf("name fallback = %q", models[0].Name)
	}
}

// TestAnthropicListModelsPaging asserts the has_more/after_id cursor is followed.
func TestAnthropicListModelsPaging(t *testing.T) {
	var calls int32
	var gotAfter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) == 1 {
			io.WriteString(w, `{"data":[{"id":"claude-a","display_name":"A"}],"has_more":true,"last_id":"claude-a"}`)
			return
		}
		gotAfter = r.URL.Query().Get("after_id")
		io.WriteString(w, `{"data":[{"id":"claude-b","display_name":"B"}],"has_more":false}`)
	}))
	defer srv.Close()

	models, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d", calls)
	}
	if gotAfter != "claude-a" {
		t.Errorf("after_id cursor = %q", gotAfter)
	}
	if len(models) != 2 || models[0].ID != "claude-a" || models[1].ID != "claude-b" {
		t.Errorf("models = %+v", models)
	}
}

// TestAnthropicListModelsNoKeyFallback asserts the static fallback is returned
// without any network call when no API key is set.
func TestAnthropicListModelsNoKeyFallback(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// BaseURL points at the server, but no key is set: the live endpoint must
	// NOT be called.
	models, err := (&Anthropic{BaseURL: srv.URL}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Errorf("endpoint must not be called without a key")
	}
	if len(models) != len(anthropicFallbackModels) {
		t.Fatalf("models = %+v", models)
	}
	want := map[string]bool{
		"claude-opus-5-5":   true,
		"claude-sonnet-5-5": true,
		"claude-haiku-5-5":  true,
		"claude-fable-5-1":  true,
	}
	for _, m := range models {
		if !want[m.ID] {
			t.Errorf("unexpected fallback id %q", m.ID)
		}
		if m.Description == "" {
			t.Errorf("fallback %q should carry a note", m.ID)
		}
	}
	// Sorted.
	if models[0].ID != "claude-fable-5-1" {
		t.Errorf("fallback not sorted: %+v", models)
	}
}

// TestListModelsHelper asserts the type-assert helper succeeds for a provider
// that supports listing and errors for one that does not.
func TestListModelsHelper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"x","name":"X"}]}`)
	}))
	defer srv.Close()

	models, err := ListModels(context.Background(), &OpenRouter{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("ListModels helper: %v", err)
	}
	if len(models) != 1 || models[0].ID != "x" {
		t.Errorf("models = %+v", models)
	}

	_, err = ListModels(context.Background(), noListProvider{})
	if err == nil || !strings.Contains(err.Error(), "does not support model listing") {
		t.Fatalf("unsupported err = %v", err)
	}
}

// noListProvider is a Provider that does NOT implement ModelLister, exercising
// the helper's unsupported path.
type noListProvider struct{}

func (noListProvider) Name() string { return "nolist" }
func (noListProvider) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, nil
}
func (noListProvider) StreamChat(context.Context, ChatRequest, func(string)) (ChatResponse, error) {
	return ChatResponse{}, nil
}

// TestCurrentProvider asserts the adapter exposes its underlying Provider so the
// cli can reach ListModels.
func TestCurrentProvider(t *testing.T) {
	p := &OpenAI{APIKey: "k"}
	adapter := NewModelCaller(p, "gpt-5")
	if adapter.CurrentProvider() != Provider(p) {
		t.Errorf("CurrentProvider did not return the injected provider")
	}
}
