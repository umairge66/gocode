package agenttool

import (
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/subagent"
)

// Once the session's spend cap is reached the Agent tool refuses to spawn,
// with a tool error naming the cap; children inherit the check.
func TestAgentRefusedAtSpendCap(t *testing.T) {
	h := newHarness(nil)
	var spent error
	h.tool.Budget = func() error { return spent }
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p"}); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	spent = errors.New("Spend cap reached: $2.00 spent of the $2.00 limit")
	for _, tool := range []*AgentTool{h.tool, h.tool.ForChild(1, "task_1", "general-purpose")} {
		_, err := call(t, tool, map[string]any{"description": "d", "prompt": "p"})
		if !errors.Is(err, subagent.ErrSpendCap) || !strings.Contains(err.Error(), "$2.00 spent of the $2.00 limit") {
			t.Fatalf("err = %v", err)
		}
	}
}

// With the concurrency cap's worth of agents running, a spawn fails with
// "Concurrent subagent limit reached".
func TestAgentConcurrentLimit(t *testing.T) {
	h := newHarness(nil)
	h.tool.MaxConcurrent = 1
	release := h.tool.Tasks.TakeConcurrencySlot() // one agent already running
	_, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p"})
	if !errors.Is(err, subagent.ErrConcurrentLimit) || !strings.Contains(err.Error(), "Concurrent subagent limit reached") {
		t.Fatalf("err = %v", err)
	}
	release()
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p"}); err != nil {
		t.Fatalf("after the running agent ended: %v", err)
	}
}
