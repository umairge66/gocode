package cli

import (
	"testing"
)

// The classifier model: $GOCODE_CODE_AUTO_MODE_MODEL, else settings
// autoMode.model from a trusted source (project settings are ignored), else
// the session model ("").
func TestAutoModeClassifierModel(t *testing.T) {
	f := newSubcmdFixture(t)
	env, _, _ := f.env()
	if got := autoModeClassifierModel(env); got != "" {
		t.Fatalf("default = %q, want session model", got)
	}
	subcmdWriteFile(t, f.userSettingsPath(), `{"autoMode":{"model":"small-model","soft_deny":["x"]}}`)
	if got := autoModeClassifierModel(env); got != "small-model" {
		t.Fatalf("settings = %q", got)
	}
	// The rules of the same section still parse.
	if r := userAutoModeRules(env); r == nil || len(r.SoftDeny) != 1 {
		t.Fatalf("rules = %+v", r)
	}
	f.vars["GOCODE_CODE_AUTO_MODE_MODEL"] = "env-model"
	if got := autoModeClassifierModel(env); got != "env-model" {
		t.Fatalf("env = %q", got)
	}
	c := newAutoModeClassifier(env, unconfiguredCaller{provider: "openai"}, nil)
	if _, ok := c.Caller.(*classifierCaller); !ok {
		t.Fatalf("override model not wired: %T", c.Caller)
	}
}
