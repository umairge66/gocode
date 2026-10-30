package agenttool

import (
	"slices"
	"strings"
	"testing"
)

// TestAgentStatuslineSetup: the statusline-setup built-in spawns by name with
// only Read and Edit, its own system prompt and the "sonnet" model alias, and
// stays out of the agent listing.
func TestAgentStatuslineSetup(t *testing.T) {
	h := newHarness(nil)
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "Configure my statusLine from my shell PS1 configuration", "subagent_type": "statusline-setup"}); err != nil {
		t.Fatalf("spawn statusline-setup: %v", err)
	}
	if len(h.allowed) != 1 || !slices.Equal(h.allowed[0], []string{"Read", "Edit"}) {
		t.Fatalf("allowed tools = %v", h.allowed)
	}
	if len(h.models) != 1 || h.models[0] != "sonnet" {
		t.Fatalf("model = %v", h.models)
	}
	sys := h.model.systems[0]
	for _, want := range []string{"status line setup agent for gocode", "statusLine", "~/.config/gocode/settings.json", "GOCODE_CONFIG_DIR"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(h.tool.AgentListing(), "statusline-setup") {
		t.Error("statusline-setup is listed")
	}
}
