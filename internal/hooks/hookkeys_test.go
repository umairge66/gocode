package hooks

import (
	"encoding/json"
	"testing"
)

// Multi-word hook keys load in either spelling: camelCase (the Claude
// Code-compatible form the /hooks editor and plugin hooks files use) and
// snake_case. camelCase wins when both are present.
func TestHookKeysAcceptBothSpellings(t *testing.T) {
	camel := `{"type":"command","command":"x","onFailure":"continue","statusMessage":"s","asyncRewake":true,"rewakeMessage":"m","rewakeSummary":"r"}`
	snake := `{"type":"command","command":"x","on_failure":"continue","status_message":"s","async_rewake":true,"rewake_message":"m","rewake_summary":"r"}`
	for name, raw := range map[string]string{"camelCase": camel, "snake_case": snake} {
		var h CommandHook
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if h.OnFailure != OnFailureContinue || h.StatusMessage != "s" || !h.AsyncRewake || h.RewakeMessage != "m" || h.RewakeSummary != "r" {
			t.Errorf("%s: hook = %+v", name, h)
		}
	}
	var http CommandHook
	if err := json.Unmarshal([]byte(`{"type":"http","url":"https://x","allowed_env_vars":["A"]}`), &http); err != nil || len(http.AllowedEnvVars) != 1 {
		t.Errorf("allowed_env_vars: %+v %v", http, err)
	}
	var mcp CommandHook
	if err := json.Unmarshal([]byte(`{"type":"mcp_prompt","server":"s","prompt_name":"p"}`), &mcp); err != nil || mcp.PromptName != "p" {
		t.Errorf("prompt_name: %+v %v", mcp, err)
	}
	var both CommandHook
	if err := json.Unmarshal([]byte(`{"type":"command","command":"x","onFailure":"block","on_failure":"continue"}`), &both); err != nil || both.OnFailure != OnFailureBlock {
		t.Errorf("both spellings: %+v %v, want the camelCase value", both, err)
	}
	// Through the settings event map too.
	var cfg Config
	if err := json.Unmarshal([]byte(`{"PreToolUse":[{"hooks":[{"type":"command","command":"x","onFailure":"continue"}]}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg[EventPreToolUse][0].Hooks[0].NormalizeOnFailure() != OnFailureContinue {
		t.Errorf("config hook = %+v", cfg[EventPreToolUse][0].Hooks[0])
	}
}

func TestFiredEvents(t *testing.T) {
	for _, ev := range []Event{EventPreToolUse, EventPostToolUseFailure, EventPermissionDenied, EventNotification, EventSubagentStart} {
		if !IsFired(ev) {
			t.Errorf("%s should be fired", ev)
		}
	}
	for _, ev := range []Event{EventSubagentStop, "PostToolBatch", "Elicitation"} {
		if IsFired(ev) {
			t.Errorf("%s is not fired", ev)
		}
	}
}
