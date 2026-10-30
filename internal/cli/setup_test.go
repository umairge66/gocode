package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
)

// testModel is the explicit model id tests pass now that there is no built-in
// default model.
const testModel = "anthropic/claude-3.5-sonnet"

func TestIsSetupError(t *testing.T) {
	for _, err := range []error{errNoModel, errNoCredentials, noCredentialsError(providerAnthropic), noCredentialsError(providerOpenAI)} {
		if !isSetupError(err) {
			t.Errorf("isSetupError(%v) = false, want true", err)
		}
	}
	if isSetupError(unknownProviderError("nope", "flag")) {
		t.Error("an invalid provider must not count as a setup error")
	}
}

func TestActivateCaller_Progression(t *testing.T) {
	s := &printSession{Provider: providerOpenRouter, newCaller: defaultNewCaller}
	conv := conversation.New(placeholderFor(s), nil)

	// No key, no model: stays on the placeholder, which asks for a key.
	if ok, err := activateCaller(conv, s); ok || err != nil {
		t.Fatalf("no key: activate = %v, %v", ok, err)
	}
	if u, isU := conv.Model.(unconfiguredCaller); !isU || !strings.Contains(u.reason(), "/login") {
		t.Fatalf("no key: placeholder = %#v", conv.Model)
	}

	// Key but no model: still the placeholder, now asking for a model.
	s.APIKey = "k"
	if ok, err := activateCaller(conv, s); ok || err != nil {
		t.Fatalf("no model: activate = %v, %v", ok, err)
	}
	if u, isU := conv.Model.(unconfiguredCaller); !isU || !strings.Contains(u.reason(), "/model") {
		t.Fatalf("no model: placeholder = %#v", conv.Model)
	}

	// Key + model: the real caller is installed.
	s.Model = "some/model"
	if ok, err := activateCaller(conv, s); !ok || err != nil {
		t.Fatalf("configured: activate = %v, %v", ok, err)
	}
	mc, isMC := conv.Model.(*provider.ModelCaller)
	if !isMC || mc.CurrentModel() != "some/model" || conv.ModelID != "some/model" {
		t.Fatalf("configured: caller = %#v (model id %q)", conv.Model, conv.ModelID)
	}
}

func TestUnconfiguredCallerErrorsActionably(t *testing.T) {
	_, err := placeholderFor(&printSession{Provider: providerAnthropic}).CallModel(context.Background(), agent.ModelRequest{})
	if err == nil || !strings.Contains(err.Error(), "No API key for anthropic") {
		t.Fatalf("err = %v", err)
	}
}

// A configured apiKeyHelper is not supported: -p must say so instead of
// silently ignoring it; with no helper set there is no notice.
func TestAPIKeyHelperNotice(t *testing.T) {
	for _, tc := range []struct {
		settings string
		want     bool
	}{
		{`{"apiKeyHelper": "op read op://vault/key"}`, true},
		{`{"apiKeyHelper": {"openrouter": "pass show or"}}`, true},
		{`{"model": "x"}`, false},
	} {
		env := providerTestEnv(t, nil, tc.settings)
		var out, errb strings.Builder
		env.Stdout, env.Stderr = &out, &errb
		if code := Run(env, []string{"gocode", "-p", "hi"}); code != 1 {
			t.Fatalf("%s: exit = %d, want 1", tc.settings, code)
		}
		if got := strings.Contains(errb.String(), "apiKeyHelper is set"); got != tc.want {
			t.Errorf("%s: notice shown = %v, want %v; stderr=%q", tc.settings, got, tc.want, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("%s: stdout = %q, want empty", tc.settings, out.String())
		}
	}
}

// liveCaller must follow the conversation's current caller, so components wired
// at startup pick up a /login or /model swap.
func TestLiveCallerFollowsSwap(t *testing.T) {
	var cur agent.ModelCaller = placeholderFor(&printSession{Provider: providerOpenRouter})
	live := liveCaller{get: func() agent.ModelCaller { return cur }}
	if _, err := live.CallModel(context.Background(), agent.ModelRequest{}); err == nil {
		t.Fatal("placeholder phase: expected an error")
	}
	cur = fakeCaller{reply: "real"}
	if _, err := live.CallModel(context.Background(), agent.ModelRequest{}); err != nil {
		t.Fatalf("after swap: %v", err)
	}
}
