package provider

import (
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func TestResolveMaxOutput(t *testing.T) {
	sonnet45 := CapabilitiesFor("anthropic", "claude-sonnet-4-5") // default 32000, limit 64000
	unknown := CapabilitiesFor("openrouter", "acme/llm-x")
	for _, tc := range []struct {
		name    string
		caps    Capabilities
		raw     string
		want    int
		fromEnv bool
		noted   bool
	}{
		{"unset uses default", sonnet45, "", 32000, false, false},
		{"raised within limit", sonnet45, "50000", 50000, true, false},
		{"lowered", sonnet45, "4000", 4000, true, false},
		{"capped at limit", sonnet45, "200000", 64000, true, true},
		{"invalid uses default", sonnet45, "lots", 32000, false, true},
		{"zero uses default", sonnet45, "0", 32000, false, true},
		{"unknown model not capped", unknown, "90000", 90000, true, false},
	} {
		got := ResolveMaxOutput(tc.caps, tc.raw)
		if got.Tokens != tc.want || got.FromEnv != tc.fromEnv || (got.Note != "") != tc.noted {
			t.Errorf("%s: got %+v, want tokens=%d fromEnv=%v noted=%v", tc.name, got, tc.want, tc.fromEnv, tc.noted)
		}
	}
}

// The per-model upper limits are in the capability table.
func TestMaxOutputLimits(t *testing.T) {
	for model, want := range map[string]int{
		"claude-opus-5-5":   128000,
		"claude-sonnet-4-5": 64000,
		"claude-opus-4-1":   32000,
		"claude-new-thing":  128000, // unknown Claude: family output limit
	} {
		if got := CapabilitiesFor("anthropic", model).MaxOutputLimit; got != want {
			t.Errorf("%s limit = %d, want %d", model, got, want)
		}
	}
}

// GOCODE_MAX_OUTPUT_TOKENS reaches the request's max_tokens.
func TestMaxOutputEnvAppliedToRequest(t *testing.T) {
	old := getenv
	defer func() { getenv = old }()
	getenv = func(k string) string {
		if k == MaxOutputEnv {
			return "50000"
		}
		return ""
	}
	mc := NewModelCaller(nil, "claude-sonnet-4-5")
	req := mc.toChatRequest(agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}}, false)
	if req.MaxTokens != 50000 {
		t.Fatalf("max_tokens = %d, want 50000", req.MaxTokens)
	}
}
