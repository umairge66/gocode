package compaction

import (
	"testing"

	"github.com/umairge66/gocode/internal/apiclient"
)

// standard 200k window with an 8k output cap:
//
//	effectiveWindow = 200000 - 8192 = 191808
//	threshold       = 191808 - 13000 = 178808
//	ceiling         = 200000 - 8192 = 191808
//	blockLimit      = 191808 - 3000 = 188808
//	warnBoundary    = 178808 - 20000 = 158808
var testWindow = Window{ContextWindow: 200000, MaxOutputTokens: 8192}

func testPlanner() Planner {
	return Planner{
		Window:         testWindow,
		Config:         Config{Enabled: true},
		EstimateTokens: func(ms []Message) int { return len(ms) * 10000 },
	}
}

func TestWindowMath(t *testing.T) {
	if got := testWindow.EffectiveWindow(); got != 191808 {
		t.Fatalf("EffectiveWindow = %d, want 191808", got)
	}
	if got := testWindow.Ceiling(); got != 191808 {
		t.Fatalf("Ceiling = %d, want 191808", got)
	}
	if got := Threshold(testWindow.EffectiveWindow(), Config{Enabled: true}); got != 178808 {
		t.Fatalf("Threshold = %d, want 178808", got)
	}
}

func TestClassifyAndShouldCompact(t *testing.T) {
	p := testPlanner()
	cases := []struct {
		name    string
		tokens  int
		level   Level
		compact bool
	}{
		{"ok under warn", 100000, LevelOK, false},
		{"warn", 160000, LevelWarn, false},
		{"at threshold", 178808, LevelCompact, true},
		{"over threshold", 180000, LevelCompact, true},
		{"at blocking limit", 188808, LevelBlocked, true},
		{"over blocking limit", 195000, LevelBlocked, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Classify(tc.tokens)
			if got.Level != tc.level {
				t.Errorf("Classify(%d).Level = %q, want %q", tc.tokens, got.Level, tc.level)
			}
			if sc := p.ShouldCompact(tc.tokens); sc != tc.compact {
				t.Errorf("ShouldCompact(%d) = %v, want %v", tc.tokens, sc, tc.compact)
			}
		})
	}
}

// When autocompact is disabled, the threshold is not applied; only the hard
// ceiling blocks.
func TestClassifyDisabled(t *testing.T) {
	p := Planner{Window: testWindow, Config: Config{Enabled: false}}
	if got := p.Classify(180000).Level; got != LevelWarn {
		// disabled: S=effectiveWindow(191808), warn=171808; 180000>=171808 but
		// below blockLimit 188808 → warn, never "compact".
		t.Fatalf("disabled Classify(180000) = %q, want warn", got)
	}
	if p.ShouldCompact(180000) {
		t.Fatal("ShouldCompact should be false when disabled and below ceiling")
	}
	if got := p.Classify(190000).Level; got != LevelBlocked {
		t.Fatalf("disabled Classify(190000) = %q, want blocked", got)
	}
}

// PctOverride lowers the threshold to floor(window*pct/100) when that is below
// the base threshold.
func TestThresholdPctOverride(t *testing.T) {
	// 50% of 191808 = 95904, which is below the base 178808 → override wins.
	got := Threshold(testWindow.EffectiveWindow(), Config{Enabled: true, PctOverride: 50})
	if got != 95904 {
		t.Fatalf("Threshold with 50%% override = %d, want 95904", got)
	}
}

func TestPrefixOverflow(t *testing.T) {
	p := testPlanner() // threshold 178808
	// fixed prefix 190000, messages 5000 → prefix alone (185000) > threshold.
	if !p.PrefixOverflow(190000, 5000) {
		t.Fatal("expected prefix overflow when fixed prefix exceeds threshold")
	}
	// modest prefix → no overflow.
	if p.PrefixOverflow(50000, 5000) {
		t.Fatal("did not expect prefix overflow for a small prefix")
	}
}

func userText(s string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{{Type: apiclient.BlockText, Text: s}}}
}
func assistantText(s string) Message {
	return Message{Role: RoleAssistant, Content: []ContentBlock{{Type: apiclient.BlockText, Text: s}}}
}
func toolResult(id string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{{Type: apiclient.BlockToolResult, ToolUseID: id, Content: "ok"}}}
}

// three turn-groups:
//
//	g1: user "u1", assistant tool_use, user tool_result
//	g2: user "u2", assistant "a2"
//	g3: user "u3", assistant "a3"
func threeGroupConversation() []Message {
	return []Message{
		userText("u1"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: apiclient.BlockToolUse, ID: "t1", Name: "Read"}}},
		toolResult("t1"),
		userText("u2"),
		assistantText("a2"),
		userText("u3"),
		assistantText("a3"),
	}
}

func TestGroupTurns(t *testing.T) {
	groups := groupTurns(threeGroupConversation())
	if len(groups) != 3 {
		t.Fatalf("groupTurns = %d groups, want 3", len(groups))
	}
	if len(groups[0]) != 3 || len(groups[1]) != 2 || len(groups[2]) != 2 {
		t.Fatalf("group sizes = %d/%d/%d, want 3/2/2", len(groups[0]), len(groups[1]), len(groups[2]))
	}
}

// Plan not needed below the threshold.
func TestPlanNotNeeded(t *testing.T) {
	p := testPlanner()
	plan := p.Plan(threeGroupConversation(), 100000)
	if plan.Needed {
		t.Fatal("plan should not be needed below threshold")
	}
	if plan.Level != LevelOK {
		t.Fatalf("level = %q, want ok", plan.Level)
	}
}

// Stock autocompact (KeepRecentGroups=0): the whole conversation is summarized,
// nothing kept verbatim.
func TestPlanFullReplace(t *testing.T) {
	p := testPlanner()
	msgs := threeGroupConversation()
	plan := p.Plan(msgs, 185000) // over threshold
	if !plan.Needed || plan.Level != LevelCompact {
		t.Fatalf("plan Needed=%v Level=%q, want needed compact", plan.Needed, plan.Level)
	}
	if len(plan.Summarize) != len(msgs) {
		t.Fatalf("Summarize = %d msgs, want all %d", len(plan.Summarize), len(msgs))
	}
	if len(plan.Keep) != 0 {
		t.Fatalf("Keep = %d, want 0 (full replace)", len(plan.Keep))
	}
}

// Retention plan: keep the most-recent group verbatim, summarize the earlier
// ("middle") groups.
func TestPlanKeepsRecentCompactsMiddle(t *testing.T) {
	p := testPlanner()
	p.KeepRecentGroups = 1
	msgs := threeGroupConversation()
	plan := p.Plan(msgs, 190000) // blocked → needed
	if !plan.Needed {
		t.Fatal("plan should be needed")
	}
	// Summarize = g1+g2 = 3+2 = 5 messages; Keep = g3 = 2 messages.
	if len(plan.Summarize) != 5 {
		t.Fatalf("Summarize = %d, want 5 (g1+g2)", len(plan.Summarize))
	}
	if len(plan.Keep) != 2 {
		t.Fatalf("Keep = %d, want 2 (recent g3)", len(plan.Keep))
	}
	// the kept tail must be the most-recent group (u3/a3).
	if plan.Keep[0].Content[0].Text != "u3" {
		t.Fatalf("kept tail starts with %q, want u3", plan.Keep[0].Content[0].Text)
	}
}

func TestTruncateHead(t *testing.T) {
	p := testPlanner() // EstimateTokens = 10000/message
	msgs := threeGroupConversation()
	// API rounds: [u1] [tool_use, tool_result, u2] [a2, u3] [a3].

	// default fraction path (overflow unknown): drop max(1, floor(4*0.2)) = 1
	// leading round; the kept slice starts with an assistant turn, so the meta
	// truncation marker is prepended (6 kept + 1).
	got := p.TruncateHead(msgs, 0)
	if len(got) != 7 {
		t.Fatalf("TruncateHead default = %d msgs, want 7", len(got))
	}
	if got[0].Role != RoleUser || got[0].Content[0].Text != TruncationMarker {
		t.Fatalf("first msg = %+v, want the truncation marker", got[0])
	}
	if got[1].Role != RoleAssistant || got[1].Content[0].Type != apiclient.BlockToolUse {
		t.Fatalf("second msg = %+v, want the tool_use round", got[1])
	}

	// overflow-driven: free 25000 tokens. round1 = 10000, round2 = 30000 →
	// 40000 >= 25000 after two rounds: keep [a2 u3] [a3] behind the marker.
	got = p.TruncateHead(msgs, 25000)
	if len(got) != 4 || got[0].Content[0].Text != TruncationMarker || got[1].Content[0].Text != "a2" {
		t.Fatalf("overflow truncation = %d msgs starting %q, want marker + a2 u3 a3", len(got), textOf(got))
	}

	// A retry strips the previous marker before regrouping, so markers never
	// stack: [a2 u3] [a3] → drop one round → marker + a3.
	again := p.TruncateHead(got, 0)
	if len(again) != 2 || again[0].Content[0].Text != TruncationMarker || again[1].Content[0].Text != "a3" {
		t.Fatalf("second truncation = %d msgs, want marker + a3", len(again))
	}

	// too little history → nil.
	if p.TruncateHead([]Message{userText("only")}, 0) != nil {
		t.Fatal("TruncateHead with <2 groups should return nil")
	}
}

func TestGroupByRound(t *testing.T) {
	withID := func(m Message, id string) Message { m.ID = id; return m }
	msgs := []Message{
		userText("u1"),
		withID(assistantText("a1-part1"), "msg_1"),
		withID(assistantText("a1-part2"), "msg_1"), // same response: same round
		toolResult("t1"),
		withID(assistantText("a2"), "msg_2"),
		userText("u2"),
		assistantText("a3"), // no id: its own round
		assistantText("a4"),
	}
	groups := GroupByRound(msgs)
	sizes := []int{}
	for _, g := range groups {
		sizes = append(sizes, len(g))
	}
	want := []int{1, 3, 2, 1, 1}
	if len(sizes) != len(want) {
		t.Fatalf("group sizes = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("group sizes = %v, want %v", sizes, want)
		}
	}
}

func textOf(ms []Message) string {
	if len(ms) == 0 || len(ms[0].Content) == 0 {
		return ""
	}
	return ms[0].Content[0].Text
}
