package compaction

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/apiclient"
)

func toolTurn(id, name, path, result string) []Message {
	in, _ := json.Marshal(map[string]string{"file_path": path})
	return []Message{
		{Role: RoleAssistant, Content: []ContentBlock{{Type: apiclient.BlockToolUse, ID: id, Name: name, Input: in}}},
		{Role: RoleUser, Content: []ContentBlock{{Type: apiclient.BlockToolResult, ToolUseID: id, Content: result}}},
	}
}

// history builds a user prompt followed by n tool turns whose results are size
// chars each.
func history(n, size int, name string) []Message {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{{Type: apiclient.BlockText, Text: "go"}}}}
	for i := 0; i < n; i++ {
		msgs = append(msgs, toolTurn(fmt.Sprintf("t%d", i), name, fmt.Sprintf("/f%d", i), strings.Repeat("x", size))...)
	}
	return msgs
}

func TestMicrocompactClearsOldKeepsRecentAndPairing(t *testing.T) {
	// 8 Read turns of 40000 chars (=10000 tokens) each; keep 5 → clear 3 → 30000 saved.
	msgs := history(8, 40000, "Read")
	res, ok := Microcompact(msgs, MicrocompactKeepRecent)
	if !ok {
		t.Fatal("microcompact did not apply")
	}
	if res.TokensSaved != 30000 || len(res.ClearedIDs) != 3 {
		t.Fatalf("saved=%d cleared=%v", res.TokensSaved, res.ClearedIDs)
	}
	if len(res.Messages) != len(msgs) {
		t.Fatalf("message count changed: %d → %d", len(msgs), len(res.Messages))
	}
	for i := 0; i < 8; i++ {
		use := res.Messages[1+2*i].Content[0]
		result := res.Messages[2+2*i].Content[0]
		if use.Type != apiclient.BlockToolUse || result.Type != apiclient.BlockToolResult || result.ToolUseID != use.ID {
			t.Fatalf("pairing broken at turn %d", i)
		}
		cleared := result.Content == ClearedToolResultPlaceholder
		if (i < 3) != cleared {
			t.Errorf("turn %d cleared=%v", i, cleared)
		}
	}
	// The input slice is not mutated.
	if msgs[2].Content[0].Content == ClearedToolResultPlaceholder {
		t.Error("input history mutated")
	}
	if strings.Join(res.ClearedReadPaths, ",") != "/f0,/f1,/f2" {
		t.Errorf("cleared read paths = %v", res.ClearedReadPaths)
	}
	// Re-running finds nothing new to clear (already-cleared results are skipped).
	if _, again := Microcompact(res.Messages, MicrocompactKeepRecent); again {
		t.Error("second microcompact re-applied")
	}
}

func TestMicrocompactBelowMinimumSavingsIsNoop(t *testing.T) {
	// 3 cleared results of 1000 chars → 750 tokens < 20000.
	if _, ok := Microcompact(history(8, 1000, "Bash"), MicrocompactKeepRecent); ok {
		t.Fatal("applied below the minimum savings")
	}
	// Ineligible tools never count.
	if _, ok := Microcompact(history(8, 400000, "Agent"), MicrocompactKeepRecent); ok {
		t.Fatal("cleared an ineligible tool's results")
	}
	// Not more than keepRecent eligible uses → nothing to clear.
	if _, ok := Microcompact(history(5, 400000, "Grep"), MicrocompactKeepRecent); ok {
		t.Fatal("cleared within the keep-recent window")
	}
}

func TestMicrocompactEligibleSet(t *testing.T) {
	for _, n := range []string{"Read", "Bash", "PowerShell", "Grep", "Glob", "WebSearch", "WebFetch", "Edit", "Write", "mcp__container__Read"} {
		if !MicrocompactEligible(n) {
			t.Errorf("%s should be eligible", n)
		}
	}
	for _, n := range []string{"Agent", "TodoWrite", "mcp__other__Read", "NotebookEdit"} {
		if MicrocompactEligible(n) {
			t.Errorf("%s should not be eligible", n)
		}
	}
}

func TestMicrocompactDropsImages(t *testing.T) {
	msgs := history(6, 100, "Read")
	// The oldest result carries 12 images (24000 tokens) → enough to clear.
	msgs[2].Content[0].Images = make([]apiclient.ToolResultImage, 12)
	res, ok := Microcompact(msgs, MicrocompactKeepRecent)
	if !ok {
		t.Fatal("image-heavy result not cleared")
	}
	b := res.Messages[2].Content[0]
	if b.Content != ClearedToolResultPlaceholder || b.Images != nil {
		t.Fatalf("image result = %+v", b)
	}
}

func TestEstimateTokensCountsAllBlocks(t *testing.T) {
	in := json.RawMessage(`{"command":"` + strings.Repeat("a", 392) + `"}`) // 406 chars
	msgs := []Message{
		{Role: RoleUser, Content: []ContentBlock{{Type: apiclient.BlockText, Text: strings.Repeat("t", 400)}}},
		{Role: RoleAssistant, Content: []ContentBlock{
			{Type: apiclient.BlockThinking, Thinking: strings.Repeat("h", 40)},
			{Type: apiclient.BlockToolUse, ID: "x", Name: "Bash", Input: in},
		}},
		{Role: RoleUser, Content: []ContentBlock{{Type: apiclient.BlockToolResult, ToolUseID: "x", Content: strings.Repeat("r", 800),
			Images: []apiclient.ToolResultImage{{}}}}},
	}
	// text 100 + thinking 10 + tool_use round((4+406)/4)=103 (102.5 rounds up) + result 200 + image 2000.
	if got, want := EstimateTokens(msgs), 100+10+103+200+2000; got != want {
		t.Fatalf("EstimateTokens = %d, want %d", got, want)
	}
	if got := ContextUsageTokens(apiclient.Usage{InputTokens: 10, CacheCreationInputTokens: 20, CacheReadInputTokens: 30, OutputTokens: 5}); got != 65 {
		t.Fatalf("ContextUsageTokens = %d, want 65", got)
	}
}
