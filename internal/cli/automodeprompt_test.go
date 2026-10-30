package cli

import (
	"testing"

	"github.com/umairge66/gocode/internal/automode"
)

// The classifier prompt variant: $GOCODE_CODE_AUTO_MODE_PROMPT, else settings
// autoMode.prompt from a trusted source (local/project settings are ignored),
// else "" (automode.DefaultPrompt). Unrecognized values are ignored.
func TestAutoModePromptVariant(t *testing.T) {
	f := newSubcmdFixture(t)
	env, _, _ := f.env()
	if got := autoModePromptVariant(env); got != "" {
		t.Fatalf("default = %q", got)
	}
	c := newAutoModeClassifier(env, unconfiguredCaller{provider: "openai"}, nil)
	if r := c.Rules(); r != nil {
		t.Fatalf("no rules, no variant: Rules() = %+v, want nil", r)
	}

	subcmdWriteFile(t, f.localSettingsPath(), `{"autoMode":{"prompt":"compact"}}`)
	if got := autoModePromptVariant(env); got != "" {
		t.Fatalf("local settings must not select the prompt, got %q", got)
	}

	subcmdWriteFile(t, f.userSettingsPath(), `{"autoMode":{"prompt":"Compact","soft_deny":["x"]}}`)
	if got := autoModePromptVariant(env); got != automode.PromptCompact {
		t.Fatalf("settings = %q", got)
	}
	if r := c.Rules(); r == nil || r.Prompt != automode.PromptCompact || len(r.SoftDeny) != 1 {
		t.Fatalf("rules = %+v", r)
	}

	subcmdWriteFile(t, f.userSettingsPath(), `{"autoMode":{"prompt":"compact"}}`)
	if r := c.Rules(); r == nil || r.Prompt != automode.PromptCompact || len(r.SoftDeny)+len(r.Allow)+len(r.HardDeny)+len(r.Environment) != 0 {
		t.Fatalf("variant-only rules = %+v", r)
	}

	f.vars["GOCODE_CODE_AUTO_MODE_PROMPT"] = "full"
	if got := autoModePromptVariant(env); got != automode.PromptFull {
		t.Fatalf("env = %q", got)
	}
	f.vars["GOCODE_CODE_AUTO_MODE_PROMPT"] = "tiny"
	if got := autoModePromptVariant(env); got != automode.PromptCompact {
		t.Fatalf("bad env value should fall through to settings, got %q", got)
	}
	subcmdWriteFile(t, f.userSettingsPath(), `{"autoMode":{"prompt":"bogus"}}`)
	if got := autoModePromptVariant(env); got != "" {
		t.Fatalf("bogus settings = %q", got)
	}
}
