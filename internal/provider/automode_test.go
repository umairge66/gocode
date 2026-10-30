package provider

import "testing"

// SupportsAutoMode: models older than Opus 4.6 never run in auto
// mode; off the first-party API neither do Opus 4.6, Sonnet 4.6 or a Haiku
// other than 5.5; anything else (newer Claude models, non-Claude models) does.
func TestSupportsAutoMode(t *testing.T) {
	cases := []struct {
		prov, model string
		want        bool
	}{
		{"anthropic", "claude-opus-4-5", false},
		{"anthropic", "claude-opus-4-5-20251101", false},
		{"anthropic", "claude-sonnet-4-20250514", false},
		{"anthropic", "claude-3-5-haiku-20241022", false},
		{"anthropic", "claude-haiku-4-5", false},
		{"anthropic", "claude-opus-4-6", true},
		{"anthropic", "claude-sonnet-4-6", true},
		{"anthropic", "claude-opus-5-5", true},
		{"anthropic", "claude-haiku-5-5", true},
		{"openrouter", "anthropic/claude-sonnet-4.6", false},
		{"openrouter", "anthropic/claude-opus-4.6", false},
		{"openrouter", "anthropic/claude-opus-4.7", true},
		{"openrouter", "anthropic/claude-haiku-5.5", true},
		{"openrouter", "openai/gpt-5", true},
		{"openai", "gpt-5", true},
	}
	for _, c := range cases {
		if got := SupportsAutoMode(c.prov, c.model); got != c.want {
			t.Errorf("SupportsAutoMode(%q, %q) = %v, want %v", c.prov, c.model, got, c.want)
		}
	}
	if ModelKey("Anthropic/Claude-Opus-4.5[1m]") != "claude-opus-4-5" {
		t.Errorf("ModelKey = %q", ModelKey("Anthropic/Claude-Opus-4.5[1m]"))
	}
	if !Supports1MSuffix("claude-sonnet-4-5") || Supports1MSuffix("claude-opus-4-8") || Supports1MSuffix("gpt-5") {
		t.Error("Supports1MSuffix: only table models whose window grows with [1m]")
	}
}
