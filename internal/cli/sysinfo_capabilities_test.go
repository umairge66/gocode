package cli

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
)

// TestContextTextShowsWindowAndSource: /context reports the active model's
// context window from the capability table together with its source.
func TestContextTextShowsWindowAndSource(t *testing.T) {
	cases := []struct {
		backend provider.Provider
		model   string
		want    []string
	}{
		{&provider.Anthropic{APIKey: "k"}, "claude-opus-5-5", []string{"Context window:   1M tokens (source: built-in)", "Max output:       128k tokens", "Remaining:"}},
		{&provider.OpenRouter{APIKey: "k"}, "openai/gpt-4o", []string{"Context window:   128k tokens (source: built-in)", "Max output:       16.4k tokens"}},
		{&provider.OpenRouter{APIKey: "k"}, "acme/unknown-model", []string{"Context window:   128k tokens (source: default)"}},
	}
	for _, c := range cases {
		conv := conversation.New(provider.NewModelCaller(c.backend, c.model), fakeToolDispatcher{})
		out := cmdInfo{conv: conv}.contextText()
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: contextText missing %q:\n%s", c.model, w, out)
			}
		}
		if strings.Contains(out, "unknown (not fetched offline") {
			t.Errorf("%s: still shows the old unknown window:\n%s", c.model, out)
		}
	}
}

// TestModelPickerDetailBadges: the /model picker detail shows the window and
// the vision / effort badges.
func TestModelPickerDetailBadges(t *testing.T) {
	got := modelDetail("anthropic", provider.ModelInfo{ID: "claude-opus-5-5", Description: "offline fallback"})
	if got != "1M ctx · vision · effort · offline fallback" {
		t.Errorf("opus detail = %q", got)
	}
	// The listing's own window wins; gpt-4o has vision but no effort.
	if got := modelDetail("openrouter", provider.ModelInfo{ID: "openai/gpt-4o", ContextLength: 128000}); got != "128k ctx · vision" {
		t.Errorf("gpt-4o detail = %q", got)
	}
	// Unknown model: no invented badges.
	if got := modelDetail("openrouter", provider.ModelInfo{ID: "acme/x"}); got != "" {
		t.Errorf("unknown detail = %q", got)
	}
	if formatTokenCount(1047576) != "1.05M" || formatTokenCount(200000) != "200k" || formatTokenCount(512) != "512" {
		t.Error("formatTokenCount")
	}
}

// TestStatusTextShowsWindow: /status shows the context window, its source and
// the capability badges.
func TestStatusTextShowsWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conv := conversation.New(provider.NewModelCaller(&provider.Anthropic{APIKey: "k"}, "claude-sonnet-4-5[1m]"), fakeToolDispatcher{})
	out := cmdInfo{conv: conv, env: &Env{}}.statusText()
	for _, w := range []string{"Context:       1M tokens (built-in)", "Capabilities:  vision"} {
		if !strings.Contains(out, w) {
			t.Errorf("statusText missing %q:\n%s", w, out)
		}
	}
}

// TestModelPickerDetailLifecycle: a deprecated model carries a "deprecated"
// badge and the listing's own window; an active one does not.
func TestModelPickerDetailLifecycle(t *testing.T) {
	got := modelDetail("anthropic", provider.ModelInfo{ID: "claude-x-1", ContextLength: 500000, Lifecycle: provider.LifecycleDeprecated})
	if !strings.Contains(got, "500k ctx") || !strings.Contains(got, "deprecated") {
		t.Errorf("deprecated detail = %q", got)
	}
	if got := modelDetail("anthropic", provider.ModelInfo{ID: "claude-x-1", Lifecycle: provider.LifecycleActive}); strings.Contains(got, "deprecated") {
		t.Errorf("active detail = %q", got)
	}
	if n := (cmdInfo{}).lifecycleNotice(""); n != "" {
		t.Errorf("empty model notice = %q", n)
	}
}
