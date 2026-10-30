package web

// WebSearch.
//
// The tool is backed by a pluggable SearchBackend (concrete: Brave Search API).
// Its output has the shape {query, results:[{tool_use_id,
// content:[{title,url}]} | string], durationSeconds, searchCount}.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// WebSearch strings.
const (
	ErrMsgMissingQuery    = "Error: Missing query"
	ErrMsgBothDomainLists = "Error: Cannot specify both allowed_domains and blocked_domains in the same request"
	// ErrMsgNotConfigured is returned when no SearchBackend is configured.
	ErrMsgNotConfigured = "web search not configured: set BRAVE_SEARCH_API_KEY"
	// SearchSourcesReminder ends every tool_result.
	SearchSourcesReminder = "REMINDER: You MUST include the sources above in your response to the user using markdown hyperlinks."
	// MinQueryLength is the minimum query length.
	MinQueryLength = 2
	// BraveAPIKeyEnv names the Brave Search subscription token variable.
	BraveAPIKeyEnv = "BRAVE_SEARCH_API_KEY"
	// BraveEndpoint is the Brave web search endpoint.
	BraveEndpoint = "https://api.search.brave.com/res/v1/web/search"
	// BraveMaxCount is Brave's per-request result cap.
	BraveMaxCount = 20
	// SearchToolUseID labels the single result block a backend produces.
	SearchToolUseID = "web-search-1"
)

// WebSearchInputSchema is the JSON schema of WebSearchInput.
const WebSearchInputSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "minLength": 2, "description": "The search query to use"},
    "allowed_domains": {"type": "array", "items": {"type": "string"}, "description": "Only include search results from these domains"},
    "blocked_domains": {"type": "array", "items": {"type": "string"}, "description": "Never include search results from these domains"}
  },
  "required": ["query"],
  "additionalProperties": false
}`

// WebSearchPrompt renders the tool description for the given time
// (citations-off form). It carries no "only available in the US" note, since
// that applies to provider-hosted search, not the Brave backend.
func WebSearchPrompt(now time.Time) string {
	month := now.Format("January 2006")
	return `
- Allows gocode to search the web and use the results to inform responses
- Provides up-to-date information for current events and recent data
- Returns search result information formatted as search result blocks, including links as markdown hyperlinks
- Use this tool for accessing information beyond gocode's knowledge cutoff
- Searches are performed automatically within a single API call

CRITICAL REQUIREMENT - You MUST follow this:
  - After answering the user's question, you MUST include a "Sources:" section at the end of your response
  - In the Sources section, list all relevant URLs from the search results as markdown hyperlinks: [Title](URL)
  - This is MANDATORY - never skip including sources in your response
  - Example format:

    [Your answer here]

    Sources:
    - [Source Title 1](https://example.com/1)
    - [Source Title 2](https://example.com/2)

Usage notes:
  - Domain filtering is supported to include or block specific websites

IMPORTANT - Use the correct year in search queries:
  - The current month is ` + month + `. You MUST use this year when searching for recent information, documentation, or current events.
  - Example: If the user asks for "latest React docs", search for "React documentation" with the current year, NOT last year
`
}

// WebSearchInput is the tool input.
type WebSearchInput struct {
	Query          string   `json:"query"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
}

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// SearchHits is one block of hits.
type SearchHits struct {
	ToolUseID string   `json:"tool_use_id"`
	Content   []Result `json:"content"`
}

// WebSearchOutput is the tool's output schema. Each entry of
// Results is either a SearchHits or a string (commentary / error text).
type WebSearchOutput struct {
	Query           string  `json:"query"`
	Results         []any   `json:"results"`
	DurationSeconds float64 `json:"durationSeconds"`
	SearchCount     int     `json:"searchCount,omitempty"`
}

// SearchBackend performs a web search. allowed/blocked are domain filters the
// backend must honour (at most one is non-empty).
type SearchBackend interface {
	Search(ctx context.Context, query string, allowed, blocked []string) ([]Result, error)
}

// WebSearchTool is the WebSearch tool.
type WebSearchTool struct {
	// Backend performs searches; nil means not configured.
	Backend SearchBackend
	// Now is the clock (tests); defaults to time.Now.
	Now func() time.Time
}

// Description returns the model-facing description for the current month.
func (t *WebSearchTool) Description() string { return WebSearchPrompt(t.now()) }

// InputSchema returns the JSON input schema.
func (*WebSearchTool) InputSchema() string { return WebSearchInputSchema }

func (t *WebSearchTool) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Validate applies the input checks plus the schema's
// min-length rule.
func (*WebSearchTool) Validate(in WebSearchInput) error {
	if strings.TrimSpace(in.Query) == "" {
		return errors.New(ErrMsgMissingQuery)
	}
	if len([]rune(in.Query)) < MinQueryLength {
		return fmt.Errorf("query must be at least %d characters", MinQueryLength)
	}
	if len(in.AllowedDomains) > 0 && len(in.BlockedDomains) > 0 {
		return errors.New(ErrMsgBothDomainLists)
	}
	return nil
}

// Execute validates and runs the search.
func (t *WebSearchTool) Execute(ctx context.Context, in WebSearchInput) (WebSearchOutput, error) {
	if err := t.Validate(in); err != nil {
		return WebSearchOutput{}, err
	}
	if t.Backend == nil {
		return WebSearchOutput{}, errors.New(ErrMsgNotConfigured)
	}
	start := t.now()
	hits, err := t.Backend.Search(ctx, in.Query, in.AllowedDomains, in.BlockedDomains)
	if err != nil {
		return WebSearchOutput{}, fmt.Errorf("web search failed: %w", err)
	}
	// Enforce the filters even if a backend ignored them.
	hits = FilterResults(hits, in.AllowedDomains, in.BlockedDomains)
	return WebSearchOutput{
		Query:           in.Query,
		Results:         []any{SearchHits{ToolUseID: SearchToolUseID, Content: hits}},
		DurationSeconds: t.now().Sub(start).Seconds(),
		SearchCount:     1,
	}, nil
}

// FormatResult renders the tool_result text sent to the model (citations-off
// form): each hit block
// as "Links: <JSON [{title,url}]>", strings verbatim, then the sources
// reminder.
func FormatResult(out WebSearchOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for query: %q\n\n", out.Query)
	for _, r := range out.Results {
		switch v := r.(type) {
		case string:
			b.WriteString(v + "\n\n")
		case SearchHits:
			if len(v.Content) == 0 {
				b.WriteString("No links found.\n\n")
				continue
			}
			type link struct {
				Title string `json:"title"`
				URL   string `json:"url"`
			}
			links := make([]link, len(v.Content))
			for i, h := range v.Content {
				links[i] = link{Title: h.Title, URL: h.URL}
			}
			js, _ := json.Marshal(links)
			b.WriteString("Links: " + string(js) + "\n\n")
		}
	}
	b.WriteString("\n" + SearchSourcesReminder)
	return strings.TrimSpace(b.String())
}

// normalizeDomain lowercases d and strips scheme, path, port and "www.".
func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if i := strings.LastIndexByte(d, ':'); i >= 0 && !strings.Contains(d, "]") {
		d = d[:i]
	}
	d = strings.TrimPrefix(d, "*.")
	d = strings.TrimPrefix(d, "www.")
	return strings.TrimSuffix(d, ".")
}

// hostMatches reports whether host equals domain or is a subdomain of it.
func hostMatches(host, domain string) bool {
	if domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// FilterResults applies allowed/blocked domain filters to hits.
func FilterResults(hits []Result, allowed, blocked []string) []Result {
	if len(allowed) == 0 && len(blocked) == 0 {
		return hits
	}
	norm := func(ds []string) []string {
		out := make([]string, 0, len(ds))
		for _, d := range ds {
			if n := normalizeDomain(d); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	al, bl := norm(allowed), norm(blocked)
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		u, err := url.Parse(h.URL)
		if err != nil {
			continue
		}
		host := normalizeDomain(u.Hostname())
		ok := len(al) == 0
		for _, d := range al {
			if hostMatches(host, d) {
				ok = true
				break
			}
		}
		for _, d := range bl {
			if hostMatches(host, d) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, h)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Brave Search backend
// ---------------------------------------------------------------------------

// BraveBackend queries the Brave Search API.
type BraveBackend struct {
	// APIKey is sent as X-Subscription-Token.
	APIKey string
	// Endpoint overrides BraveEndpoint (tests).
	Endpoint string
	// Doer performs requests; defaults to an http.Client with a timeout.
	Doer Doer
	// Count is the number of results to request (default/max BraveMaxCount).
	Count int
}

// NewBraveBackendFromEnv returns a BraveBackend when BRAVE_SEARCH_API_KEY is
// set, otherwise nil.
func NewBraveBackendFromEnv() *BraveBackend {
	key := strings.TrimSpace(os.Getenv(BraveAPIKeyEnv))
	if key == "" {
		return nil
	}
	return &BraveBackend{APIKey: key}
}

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// Search implements SearchBackend. Domain filters are applied client-side.
func (b *BraveBackend) Search(ctx context.Context, query string, allowed, blocked []string) ([]Result, error) {
	if b.APIKey == "" {
		return nil, errors.New(ErrMsgNotConfigured)
	}
	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = BraveEndpoint
	}
	count := b.Count
	if count <= 0 || count > BraveMaxCount {
		count = BraveMaxCount
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", fmt.Sprint(count))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.APIKey)
	doer := b.Doer
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave search: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var br braveResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&br); err != nil {
		return nil, fmt.Errorf("brave search: decoding response: %w", err)
	}
	out := make([]Result, 0, len(br.Web.Results))
	for _, r := range br.Web.Results {
		if r.URL == "" {
			continue
		}
		title := StripTags(r.Title)
		if title == "" {
			title = r.URL
		}
		out = append(out, Result{Title: title, URL: r.URL, Snippet: StripTags(r.Description)})
	}
	return FilterResults(out, allowed, blocked), nil
}
