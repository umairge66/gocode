package cost

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/apiclient"
)

// Known model + token counts → exact USD, under gocode-opus-4-8 (tier_5_25:
// input $5, output $25, cache-write $6.25, cache-read $0.5 per Mtok).
func TestCostFromUsageKnownModel(t *testing.T) {
	u := Usage{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheReadInputTokens:     1_000_000,
		CacheCreationInputTokens: 1_000_000,
	}
	got, known := CostForModel("gocode-opus-4-8", u)
	if !known {
		t.Fatal("gocode-opus-4-8 should be a known model")
	}
	// 1*5 + 1*25 + 1*0.5 + 1*6.25 = 36.75
	const want = 36.75
	if got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestCostWebSearchAddedAfterMultiplier(t *testing.T) {
	u := Usage{InputTokens: 1_000_000, WebSearchRequests: 3}
	// 1*5 = 5, + 3*0.01 = 0.03 -> 5.03
	got, _ := CostForModel("gocode-opus-4-8", u)
	const want = 5.03
	if got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestCostUSGeoMultiplier(t *testing.T) {
	base := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	us := base
	us.InferenceGeo = "us"
	plain, _ := CostForModel("gocode-opus-4-8", base) // 30
	withGeo, _ := CostForModel("gocode-opus-4-8", us) // 30 * 1.1
	if plain != 30.0 {
		t.Fatalf("plain cost = %v, want 30", plain)
	}
	if diff := withGeo - plain*usGeoMultiplier; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("us geo cost = %v, want %v", withGeo, plain*usGeoMultiplier)
	}
}

func TestCacheWrite1hSplit(t *testing.T) {
	// opus-4-8: 5m write $6.25, 1h write $10 per Mtok.
	u := Usage{
		CacheCreationInputTokens: 1_000_000,
		Ephemeral1hInputTokens:   400_000, // 0.4M at $10, 0.6M at $6.25
	}
	got, _ := CostForModel("gocode-opus-4-8", u)
	want := 0.4*10 + 0.6*6.25 // 4 + 3.75 = 7.75
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestLongPromptTier(t *testing.T) {
	// haiku-5-5 switches to the long-prompt tier above 100k prompt tokens.
	small := Usage{InputTokens: 50_000}
	big := Usage{InputTokens: 150_000}
	cs, _ := CostForModel("gocode-haiku-5-5", small) // base input $0.1/Mtok
	cb, _ := CostForModel("gocode-haiku-5-5", big)   // long input $0.5/Mtok
	wantSmall := 0.05 * 0.1                          // 50k/1M * 0.1
	wantBig := 0.15 * 0.5                            // 150k/1M * 0.5 (long-prompt rate)
	if diff := cs - wantSmall; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("small-prompt cost = %v, want %v", cs, wantSmall)
	}
	if diff := cb - wantBig; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("long-prompt cost = %v, want %v", cb, wantBig)
	}
}

// Unknown model → no invented price: cost 0, known=false.
func TestUnknownModelHasNoPrice(t *testing.T) {
	u := Usage{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheReadInputTokens:     1_000_000,
		CacheCreationInputTokens: 1_000_000,
	}
	got, known := CostForModel("totally-made-up-model", u)
	if known {
		t.Fatal("made-up model must not be known")
	}
	if got != 0 {
		t.Fatalf("unknown model cost = %v, want 0 (no guessed rate)", got)
	}
}

func TestLedgerAccumulation(t *testing.T) {
	l := NewLedger()
	c1 := l.AddTurn("gocode-opus-4-8", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}) // 30
	c2 := l.AddTurn("gocode-opus-4-8", Usage{InputTokens: 1_000_000})                          // 5
	c3 := l.AddTurn("gocode-sonnet-5", Usage{InputTokens: 1_000_000})                          // tier_2_10: 2

	if c1 != 30 || c2 != 5 || c3 != 2 {
		t.Fatalf("turn costs = %v,%v,%v want 30,5,2", c1, c2, c3)
	}
	if got := l.TotalCostUSD(); got != 37 {
		t.Fatalf("total cost = %v, want 37", got)
	}
	if got := l.TotalInputTokens(); got != 3_000_000 {
		t.Fatalf("total input tokens = %d, want 3000000", got)
	}
	opus, ok := l.UsageForModel("gocode-opus-4-8")
	if !ok || opus.InputTokens != 2_000_000 || opus.CostUSD != 35 {
		t.Fatalf("opus usage = %+v, want input 2000000 cost 35", opus)
	}

	l.AddLines(10, 3)
	l.AddLines(5, 1)
	if l.TotalLinesAdded() != 15 || l.TotalLinesRemoved() != 4 {
		t.Fatalf("lines = +%d -%d, want +15 -4", l.TotalLinesAdded(), l.TotalLinesRemoved())
	}

	l.AddAPIDuration(2*time.Second, 1500*time.Millisecond)
	l.AddAPIDuration(1*time.Second, 1*time.Second)
	if l.TotalAPIDuration() != 3*time.Second {
		t.Fatalf("api duration = %v, want 3s", l.TotalAPIDuration())
	}
	if l.TotalAPIDurationWithoutRetries() != 2500*time.Millisecond {
		t.Fatalf("api duration w/o retries = %v, want 2.5s", l.TotalAPIDurationWithoutRetries())
	}
}

func TestLedgerUnknownModelFlag(t *testing.T) {
	l := NewLedger()
	if l.HasUnknownModelCost() {
		t.Fatal("new ledger should not flag unknown model")
	}
	l.AddTurn("mystery-model", Usage{InputTokens: 1000})
	if !l.HasUnknownModelCost() {
		t.Fatal("expected hasUnknownModelCost after unknown-model turn")
	}
}

// Concurrency-safe accumulation (run with -race).
func TestLedgerConcurrent(t *testing.T) {
	l := NewLedger()
	const goroutines = 16
	const turns = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < turns; i++ {
				l.AddTurn("gocode-opus-4-8", Usage{InputTokens: 1_000_000}) // $5 each
				l.AddLines(1, 1)
				l.AddToolDuration(time.Millisecond)
				_ = l.Session()
				_ = l.ModelUsage()
			}
		}()
	}
	wg.Wait()

	wantCost := float64(goroutines*turns) * 5
	if got := l.TotalCostUSD(); got != wantCost {
		t.Fatalf("concurrent total cost = %v, want %v", got, wantCost)
	}
	if got := l.TotalLinesAdded(); got != goroutines*turns {
		t.Fatalf("concurrent lines added = %d, want %d", got, goroutines*turns)
	}
	if got := l.TotalInputTokens(); got != goroutines*turns*1_000_000 {
		t.Fatalf("concurrent input tokens = %d", got)
	}
}

func TestTotalDurationUsesClock(t *testing.T) {
	l := NewLedger()
	base := time.Now()
	l.startTime = base
	l.setNow(func() time.Time { return base.Add(90 * time.Second) })
	if got := l.TotalDuration(); got != 90*time.Second {
		t.Fatalf("total duration = %v, want 90s", got)
	}
	// Clock going backwards clamps to zero.
	l.setNow(func() time.Time { return base.Add(-5 * time.Second) })
	if got := l.TotalDuration(); got != 0 {
		t.Fatalf("total duration = %v, want 0", got)
	}
}

func TestFormatUSD(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{36.75, "$36.75"},
		{1.005, "$1.00"}, // rounds to 2 decimals above $0.50
		{0.5, "$0.5000"}, // not > 0.5, so 4 decimals
		{0.0055, "$0.0055"},
		{0, "$0.0000"},
	}
	for _, c := range cases {
		if got := FormatUSD(c.in); got != c.want {
			t.Errorf("FormatUSD(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{1500 * time.Millisecond, "1s"},
		{65 * time.Second, "1m 5s"},
		{(time.Hour + time.Minute + time.Second), "1h 1m 1s"},
		{25 * time.Hour, "1d 1h 0m"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.in); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{1500, "1.5k"},
		{1_000_000, "1m"},
		{2_500_000, "2.5m"},
	}
	for _, c := range cases {
		if got := FormatCount(c.in); got != c.want {
			t.Errorf("FormatCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The /cost summary string format.
func TestCostSummaryFormat(t *testing.T) {
	l := NewLedger()
	base := time.Now()
	l.startTime = base
	l.setNow(func() time.Time { return base.Add(65 * time.Second) })
	l.AddTurn("gocode-opus-4-8", Usage{InputTokens: 100, OutputTokens: 200})
	l.AddLines(10, 1)
	l.AddAPIDuration(5*time.Second, 5*time.Second)

	got := CostSummary(l)
	want := strings.Join([]string{
		"Total cost:            $0.0055",
		"Total duration (API):  5s",
		"Total duration (wall): 1m 5s",
		"Total code changes:    10 lines added, 1 line removed",
		"Usage by model:",
		"     gocode-opus-4-8:  100 input, 200 output, 0 cache read, 0 cache write ($0.0055)",
	}, "\n")
	if got != want {
		t.Fatalf("CostSummary mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCostSummaryEmpty(t *testing.T) {
	l := NewLedger()
	base := time.Now()
	l.startTime = base
	l.setNow(func() time.Time { return base })
	got := CostSummary(l)
	if !strings.Contains(got, "Usage:                 0 input, 0 output, 0 cache read, 0 cache write") {
		t.Fatalf("empty summary missing zero usage line:\n%s", got)
	}
	if !strings.Contains(got, "Total cost:            $0.0000") {
		t.Fatalf("empty summary missing zero cost:\n%s", got)
	}
}

func TestCostSummaryUnknownModelWarning(t *testing.T) {
	l := NewLedger()
	l.AddTurn("mystery-model", Usage{InputTokens: 1000})
	got := CostSummary(l)
	if !strings.Contains(got, unknownModelWarning) {
		t.Fatalf("expected unknown-model warning in summary:\n%s", got)
	}
}

func TestUsageFromAPI(t *testing.T) {
	au := apiclient.Usage{
		InputTokens:              10,
		OutputTokens:             20,
		CacheReadInputTokens:     5,
		CacheCreationInputTokens: 3,
		ServerToolUse:            json.RawMessage(`{"web_search_requests":2}`),
	}
	u := UsageFromAPI(au)
	if u.InputTokens != 10 || u.OutputTokens != 20 || u.CacheReadInputTokens != 5 ||
		u.CacheCreationInputTokens != 3 || u.WebSearchRequests != 2 {
		t.Fatalf("UsageFromAPI = %+v", u)
	}
	// Missing / malformed server_tool_use -> zero web search, no panic.
	if u2 := UsageFromAPI(apiclient.Usage{InputTokens: 1}); u2.WebSearchRequests != 0 {
		t.Fatalf("expected 0 web search, got %d", u2.WebSearchRequests)
	}
}

func TestStatusLineSessionJSON(t *testing.T) {
	l := NewLedger()
	base := time.Now()
	l.startTime = base
	l.setNow(func() time.Time { return base.Add(2 * time.Second) })
	l.AddTurn("gocode-opus-4-8", Usage{InputTokens: 1_000_000})
	l.AddLines(4, 2)
	b, err := json.Marshal(l.Session())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{"total_cost_usd", "total_duration_ms", "total_lines_added", "total_lines_removed"} {
		if !strings.Contains(s, key) {
			t.Fatalf("session JSON missing %q: %s", key, s)
		}
	}
}

// An OpenRouter ":free" variant costs $0 at a known price, so it never moves
// the session total (or a spend cap) and does not latch the unknown-cost flag.
func TestFreeModelCostsNothing(t *testing.T) {
	l := NewLedger()
	if got := l.AddTurn("cohere/north-mini-code:free", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); got != 0 {
		t.Fatalf("free turn cost %v", got)
	}
	if l.TotalCostUSD() != 0 || l.HasUnknownModelCost() {
		t.Fatalf("total=%v unknown=%v", l.TotalCostUSD(), l.HasUnknownModelCost())
	}
	if IsFreeModel("gocode-opus-5") || !IsFreeModel("x/y:FREE") {
		t.Fatal("IsFreeModel")
	}
	if usd, known := CostForModel("unknown/paid-model", Usage{OutputTokens: 1_000_000}); usd != 0 || known {
		t.Fatalf("a paid unknown model must be unpriced, got %v known=%v", usd, known)
	}
}
