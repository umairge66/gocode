package redact

import (
	"strings"
	"testing"
)

// Red-team regression: OpenRouter is the primary provider, so its key shape
// must be redacted wherever Redact is applied (bg-session details, mcp list).
func TestRedactOpenRouterKey(t *testing.T) {
	key := "sk-or-v1-" + strings.Repeat("0a1b2c3d", 8)
	for _, s := range []string{
		"key " + key + " end",
		`{"apiKey":"` + key + `"}`,
		"OPENROUTER_API_KEY=" + key,
		"Authorization: Bearer " + key,
	} {
		got := Redact(s)
		if strings.Contains(got, key) || !strings.Contains(got, Placeholder) {
			t.Errorf("Redact(%q) did not redact the OpenRouter key: %q", strings.ReplaceAll(s, key, "<key>"), strings.ReplaceAll(got, key, "<key>"))
		}
	}
}
