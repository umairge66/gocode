package hooks

import (
	"encoding/json"
	"testing"
)

func TestNormalizeOnFailure(t *testing.T) {
	if (CommandHook{}).NormalizeOnFailure() != OnFailureBlock {
		t.Error("absent on_failure should default to block")
	}
	if (CommandHook{OnFailure: OnFailureContinue}).NormalizeOnFailure() != OnFailureContinue {
		t.Error("explicit continue should be preserved")
	}
	if (CommandHook{OnFailure: OnFailureBlock}).NormalizeOnFailure() != OnFailureBlock {
		t.Error("explicit block should be preserved")
	}
}

// The settings-file form keys matcher groups by event name; it should unmarshal
// straight into Config.
func TestConfigUnmarshal(t *testing.T) {
	raw := `{
		"PreToolUse": [
			{"matcher": "Bash", "hooks": [{"type": "command", "command": "echo hi", "timeout": 30}]}
		],
		"Stop": [
			{"hooks": [{"type": "command", "command": "cleanup.sh", "on_failure": "continue"}]}
		]
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	g := cfg.Groups(EventPreToolUse)
	if len(g) != 1 || g[0].Matcher != "Bash" {
		t.Fatalf("PreToolUse = %+v", g)
	}
	h := g[0].Hooks[0]
	if h.Type != HookTypeCommand || h.Command != "echo hi" || h.TimeoutSeconds != 30 {
		t.Errorf("hook = %+v", h)
	}
	s := cfg.Groups(EventStop)
	if len(s) != 1 || s[0].Matcher != "" || s[0].Hooks[0].OnFailure != OnFailureContinue {
		t.Errorf("Stop = %+v", s)
	}
}
