package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/apiclient"
)

// fakeCaller is a scripted ModelCaller: it returns results from a queue, so a
// test can simulate a "Prompt is too long" error followed by success.
type fakeCaller struct {
	results []SummarizeResult
	calls   []SummarizeRequest
}

func (f *fakeCaller) Call(_ context.Context, req SummarizeRequest) (SummarizeResult, error) {
	f.calls = append(f.calls, req)
	r := f.results[0]
	if len(f.results) > 1 {
		f.results = f.results[1:]
	}
	return r, nil
}

func testCompactor(caller ModelCaller) Compactor {
	return Compactor{
		Planner: Planner{
			Window:         testWindow,
			Config:         Config{Enabled: true},
			EstimateTokens: func(ms []Message) int { return len(ms) * 10000 },
		},
		Caller:          caller,
		MaxOutputTokens: 8192,
	}
}

func TestBuildSummaryPromptContents(t *testing.T) {
	p := BuildSummaryPrompt(PromptOptions{AnalysisFirst: true})
	for _, want := range []string{
		"CRITICAL: Respond with TEXT ONLY",
		"detailed summary of the conversation so far",
		"<analysis>",
		"1. Primary Request and Intent",
		"6. All user messages",
		"REMINDER: Do NOT call any tools",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("analysis-first prompt missing %q", want)
		}
	}

	// summary-only variant must not request an <analysis> scratchpad block.
	s := BuildSummaryPrompt(PromptOptions{AnalysisFirst: false})
	if strings.Contains(s, "wrap your analysis in <analysis> tags") {
		t.Error("summary-only prompt should not ask for an analysis block")
	}

	// capped + custom instructions.
	c := BuildSummaryPrompt(PromptOptions{Capped: true, CustomInstructions: "focus on go code"})
	if !strings.Contains(c, "in 2000 words or fewer") {
		t.Error("capped prompt missing the word cap")
	}
	if !strings.Contains(c, "Additional Instructions:\nfocus on go code") {
		t.Error("custom instructions not appended")
	}
}

func TestExtractSummary(t *testing.T) {
	in := "<analysis>\nmy private reasoning\n</analysis>\n\n<summary>\nDid X and Y.\n</summary>"
	got := ExtractSummary(in)
	if strings.Contains(got, "my private reasoning") {
		t.Error("analysis block should be stripped")
	}
	if got != "Summary:\nDid X and Y." {
		t.Fatalf("ExtractSummary = %q, want %q", got, "Summary:\nDid X and Y.")
	}
}

func TestParseOverflowTokens(t *testing.T) {
	got := parseOverflowTokens("Prompt is too long: 250000 tokens > 200000 maximum")
	if got != 50000 {
		t.Fatalf("parseOverflowTokens = %d, want 50000", got)
	}
	if parseOverflowTokens("some other error") != 0 {
		t.Error("unparseable error should yield 0")
	}
}

// A clean run: fake model returns a summary; the rebuilt list is the summary
// message followed by the kept recent tail.
func TestCompactRun(t *testing.T) {
	caller := &fakeCaller{results: []SummarizeResult{
		{Text: "<summary>\nUser asked to add compaction. Implemented Planner.\n</summary>"},
	}}
	c := testCompactor(caller)
	c.Planner.KeepRecentGroups = 1

	msgs := threeGroupConversation()
	res, err := c.Compact(context.Background(), msgs, 190000, TriggerManual)
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if res.Attempts != 1 || res.HeadTruncated {
		t.Fatalf("Attempts=%d HeadTruncated=%v, want 1/false", res.Attempts, res.HeadTruncated)
	}

	// the summarized span (g1+g2 = 5 messages) was what the model saw.
	if len(caller.calls) != 1 || len(caller.calls[0].Messages) != 5 {
		t.Fatalf("model saw %d calls / %d msgs, want 1/5", len(caller.calls), msgLen(caller.calls))
	}

	// rebuilt list: summary message + the kept tail (g3 = 2 messages) = 3.
	if len(res.Messages) != 3 {
		t.Fatalf("rebuilt = %d messages, want 3 (summary + g3)", len(res.Messages))
	}
	sum := res.Messages[0]
	if sum.Role != RoleUser {
		t.Errorf("summary message role = %q, want user", sum.Role)
	}
	body := sum.Content[0].Text
	if !strings.Contains(body, continuationHeader) {
		t.Error("summary message missing continuation header")
	}
	if !strings.Contains(body, "Summary:\nUser asked to add compaction. Implemented Planner.") {
		t.Errorf("summary message missing unwrapped summary, got: %q", body)
	}
	// the kept tail is preserved verbatim after the summary.
	if res.Messages[1].Content[0].Text != "u3" {
		t.Errorf("kept tail not preserved; msg[1] = %q, want u3", res.Messages[1].Content[0].Text)
	}
	if res.BoundaryMarker.Trigger != TriggerManual || res.BoundaryMarker.MessagesSummarized != 5 {
		t.Errorf("boundary marker = %+v, want manual / 5 summarized", res.BoundaryMarker)
	}
}

// Compact surfaces the summarizer turn's Usage on the returned Result so a host
// can account the compaction's own cost in its local ledger.
func TestCompactSurfacesUsage(t *testing.T) {
	want := apiclient.Usage{InputTokens: 1234, OutputTokens: 567, CacheReadInputTokens: 89}
	caller := &fakeCaller{results: []SummarizeResult{
		{Text: "<summary>done</summary>", Usage: want},
	}}
	c := testCompactor(caller)

	res, err := c.Compact(context.Background(), threeGroupConversation(), 190000, TriggerManual)
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if res.Usage.InputTokens != want.InputTokens ||
		res.Usage.OutputTokens != want.OutputTokens ||
		res.Usage.CacheReadInputTokens != want.CacheReadInputTokens {
		t.Fatalf("Result.Usage = %+v, want %+v", res.Usage, want)
	}
}

// Head-truncation retry: first call reports "Prompt is too long", the retry (on
// a truncated prefix) succeeds.
func TestCompactHeadTruncationRetry(t *testing.T) {
	caller := &fakeCaller{results: []SummarizeResult{
		{APIError: "Prompt is too long: 250000 tokens > 200000 maximum"},
		{Text: "<summary>Recovered after truncation.</summary>"},
	}}
	c := testCompactor(caller)

	res, err := c.Compact(context.Background(), threeGroupConversation(), 190000, TriggerAuto)
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if res.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", res.Attempts)
	}
	if !res.HeadTruncated {
		t.Fatal("HeadTruncated should be true")
	}
	if !strings.Contains(res.Summary.Content[0].Text, headTruncatedNote) {
		t.Error("truncated summary should carry the head-truncation note")
	}
	// the retry saw a smaller prefix than the first attempt.
	if len(caller.calls) != 2 || len(caller.calls[1].Messages) >= len(caller.calls[0].Messages) {
		t.Fatalf("retry prefix not smaller: %d then %d", len(caller.calls[0].Messages), len(caller.calls[1].Messages))
	}
}

// Exhausting the retry budget surfaces ErrPromptTooLong.
func TestCompactPromptTooLongExhausted(t *testing.T) {
	caller := &fakeCaller{results: []SummarizeResult{
		{APIError: "Prompt is too long: 999999 tokens > 200000 maximum"},
	}}
	c := testCompactor(caller)
	_, err := c.Compact(context.Background(), threeGroupConversation(), 190000, TriggerAuto)
	if err != ErrPromptTooLong {
		t.Fatalf("err = %v, want ErrPromptTooLong", err)
	}
}

// PreCompact hook can block compaction.
func TestCompactPreCompactHookBlocks(t *testing.T) {
	caller := &fakeCaller{results: []SummarizeResult{{Text: "<summary>x</summary>"}}}
	c := testCompactor(caller)
	c.PreCompact = func(_ context.Context, _ CompactTrigger, _ string) (HookResult, error) {
		return HookResult{Blocked: true, BlockReason: "policy"}, nil
	}
	_, err := c.Compact(context.Background(), threeGroupConversation(), 190000, TriggerManual)
	if err != ErrBlockedByHook {
		t.Fatalf("err = %v, want ErrBlockedByHook", err)
	}
}

// PreCompact hook extra instructions are merged into the summary prompt.
func TestCompactPreCompactHookInstructions(t *testing.T) {
	caller := &fakeCaller{results: []SummarizeResult{{Text: "<summary>x</summary>"}}}
	c := testCompactor(caller)
	c.PreCompact = func(_ context.Context, _ CompactTrigger, _ string) (HookResult, error) {
		return HookResult{ExtraInstructions: "preserve the deploy steps"}, nil
	}
	if _, err := c.Compact(context.Background(), threeGroupConversation(), 190000, TriggerManual); err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	prompt := caller.calls[0].Prompt.Content[0].Text
	if !strings.Contains(prompt, "preserve the deploy steps") {
		t.Error("hook extra instructions not merged into the summary prompt")
	}
}

func TestCompactEmpty(t *testing.T) {
	c := testCompactor(&fakeCaller{results: []SummarizeResult{{Text: "<summary>x</summary>"}}})
	if _, err := c.Compact(context.Background(), nil, 0, TriggerAuto); err != ErrNotEnoughMessages {
		t.Fatalf("err = %v, want ErrNotEnoughMessages", err)
	}
}

func msgLen(calls []SummarizeRequest) int {
	if len(calls) == 0 {
		return 0
	}
	return len(calls[0].Messages)
}
