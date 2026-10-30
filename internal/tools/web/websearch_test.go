package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	results          []Result
	query            string
	allowed, blocked []string
}

func (f *fakeBackend) Search(_ context.Context, q string, allowed, blocked []string) ([]Result, error) {
	f.query, f.allowed, f.blocked = q, allowed, blocked
	return f.results, nil
}

var sampleHits = []Result{
	{Title: "Go", URL: "https://go.dev/doc", Snippet: "docs"},
	{Title: "Blog", URL: "https://blog.go.dev/x"},
	{Title: "Spam", URL: "https://www.spam.com/y"},
}

func TestWebSearchFakeBackendAndFilters(t *testing.T) {
	fb := &fakeBackend{results: sampleHits}
	tool := &WebSearchTool{Backend: fb}

	out, err := tool.Execute(context.Background(), WebSearchInput{Query: "golang", AllowedDomains: []string{"go.dev"}})
	if err != nil {
		t.Fatal(err)
	}
	hits := out.Results[0].(SearchHits).Content
	if len(hits) != 2 || fb.query != "golang" || len(fb.allowed) != 1 {
		t.Fatalf("allowed filter wrong: %+v", hits)
	}

	out, err = tool.Execute(context.Background(), WebSearchInput{Query: "golang", BlockedDomains: []string{"https://spam.com/"}})
	if err != nil {
		t.Fatal(err)
	}
	if hits := out.Results[0].(SearchHits).Content; len(hits) != 2 || hits[1].URL != "https://blog.go.dev/x" {
		t.Fatalf("blocked filter wrong: %+v", hits)
	}

	text := FormatResult(out)
	if !strings.HasPrefix(text, `Web search results for query: "golang"`) ||
		!strings.Contains(text, `Links: [{"title":"Go","url":"https://go.dev/doc"},{"title":"Blog","url":"https://blog.go.dev/x"}]`) ||
		!strings.HasSuffix(text, SearchSourcesReminder) {
		t.Fatalf("format wrong:\n%s", text)
	}
	if empty := FormatResult(WebSearchOutput{Query: "q", Results: []any{SearchHits{}}}); !strings.Contains(empty, "No links found.") {
		t.Fatalf("empty format wrong: %s", empty)
	}
}

func TestWebSearchValidation(t *testing.T) {
	tool := &WebSearchTool{Backend: &fakeBackend{}}
	if _, err := tool.Execute(context.Background(), WebSearchInput{}); err == nil || err.Error() != ErrMsgMissingQuery {
		t.Errorf("missing query: %v", err)
	}
	if _, err := tool.Execute(context.Background(), WebSearchInput{Query: "x"}); err == nil {
		t.Error("1-char query accepted")
	}
	_, err := tool.Execute(context.Background(), WebSearchInput{Query: "ok", AllowedDomains: []string{"a.com"}, BlockedDomains: []string{"b.com"}})
	if err == nil || err.Error() != ErrMsgBothDomainLists {
		t.Errorf("both lists: %v", err)
	}
}

func TestWebSearchNotConfigured(t *testing.T) {
	t.Setenv(BraveAPIKeyEnv, "")
	if NewBraveBackendFromEnv() != nil {
		t.Fatal("should be unconfigured")
	}
	tool := &WebSearchTool{}
	_, err := tool.Execute(context.Background(), WebSearchInput{Query: "golang"})
	if err == nil || err.Error() != "web search not configured: set BRAVE_SEARCH_API_KEY" {
		t.Fatalf("err = %v", err)
	}
	t.Setenv(BraveAPIKeyEnv, "k")
	if NewBraveBackendFromEnv() == nil {
		t.Fatal("should be configured from env")
	}
}

func TestBraveBackendRequestShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/res/v1/web/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "go generics" {
			t.Errorf("q = %q", r.URL.Query().Get("q"))
		}
		if r.Header.Get("X-Subscription-Token") != "secret-token" {
			t.Errorf("token header = %q", r.Header.Get("X-Subscription-Token"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept = %q", r.Header.Get("Accept"))
		}
		json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": []map[string]string{
			{"title": "<strong>Generics</strong> tutorial", "url": "https://go.dev/doc/tutorial/generics", "description": "Learn <strong>generics</strong>"},
			{"title": "Other", "url": "https://example.com/g", "description": "x"},
		}}})
	}))
	defer srv.Close()
	b := &BraveBackend{APIKey: "secret-token", Endpoint: srv.URL + "/res/v1/web/search", Doer: srv.Client()}
	got, err := b.Search(context.Background(), "go generics", nil, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Generics tutorial" || got[0].Snippet != "Learn generics" || got[0].URL != "https://go.dev/doc/tutorial/generics" {
		t.Fatalf("results = %+v", got)
	}

	// Non-200 surfaces as an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer bad.Close()
	b.Endpoint = bad.URL
	if _, err := b.Search(context.Background(), "q", nil, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if BraveEndpoint != "https://api.search.brave.com/res/v1/web/search" {
		t.Fatal("endpoint constant changed")
	}
}

func TestWebSearchPrompt(t *testing.T) {
	p := (&WebSearchTool{Now: func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }}).Description()
	if !strings.Contains(p, "The current month is October 2026") || !strings.Contains(p, "Sources:") {
		t.Fatalf("prompt wrong:\n%s", p)
	}
}
