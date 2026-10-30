package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/toolreg"
)

// modelPolicyFor loads the policy of a hermetic settings tree.
func modelPolicyFor(t *testing.T, user, managed string, prov string) *modelPolicy {
	t.Helper()
	fsys := fsfacade.NewMemFS()
	if user != "" {
		fsys.WriteFile("/home/u/.config/gocode/settings.json", []byte(user))
	}
	if managed != "" {
		fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(managed))
	}
	ctx := policyTestCtx(settingsload.OSPolicyReadout{})
	return modelPolicyFrom(settingsload.EffectiveSources(fsys, ctx, settingsload.CanonicalSourceOrder), prov)
}

func cascade(prov string, entries ...string) *modelPolicy {
	return &modelPolicy{provider: prov, availableSet: true, available: entries}
}

func TestModelPolicyAllowlistMatching(t *testing.T) {
	cases := []struct {
		name    string
		p       *modelPolicy
		model   string
		allowed bool
	}{
		{"no list allows everything", &modelPolicy{provider: providerAnthropic}, "gpt-5", true},
		{"empty list allows nothing", cascade(providerAnthropic), "claude-opus-4-5", false},
		{"family entry allows its family", cascade(providerAnthropic, "opus"), "claude-opus-4-1", true},
		{"family entry refuses other families", cascade(providerAnthropic, "opus"), "claude-sonnet-4-5", false},
		{"gocode-prefixed family entry", cascade(providerAnthropic, "claude-opus"), "claude-opus-4-1", true},
		{"alias resolves through the provider table", cascade(providerAnthropic, "opus"), "opus", true},
		{"alias outside the list", cascade(providerAnthropic, "sonnet"), "opus", false},
		{"id entry allows its dated variant", cascade(providerAnthropic, "claude-opus-4-5"), "claude-opus-4-5-20251101", true},
		{"id entry never a longer number", cascade(providerAnthropic, "claude-opus-4"), "claude-opus-45", false},
		{"version prefix opus-4 allows 4.x", cascade(providerAnthropic, "opus-4"), "claude-opus-4-5", true},
		{"version prefix opus-4 refuses 5", cascade(providerAnthropic, "opus-4"), "claude-opus-5-5", false},
		{"version prefix on an OpenRouter id", cascade(providerOpenRouter, "opus-4-5"), "anthropic/claude-opus-4.5", true},
		{"narrowed family: only the named version", cascade(providerAnthropic, "opus", "opus-4-5"), "claude-opus-4-6", false},
		{"narrowed family keeps the named version", cascade(providerAnthropic, "opus", "opus-4-5"), "claude-opus-4-5-20251101", true},
		{"non-Claude prefix entry", cascade(providerOpenAI, "gpt-5"), "gpt-5-mini", true},
		{"non-Claude prefix needs a boundary", cascade(providerOpenAI, "gpt-5"), "gpt-50", false},
		{"[1m] suffix is ignored", cascade(providerAnthropic, "claude-sonnet-4-6"), "claude-sonnet-4-6[1m]", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.allows(tc.model); got != tc.allowed {
				t.Fatalf("allows(%q) = %v, want %v", tc.model, got, tc.allowed)
			}
		})
	}
}

func TestModelPolicyExactMatch(t *testing.T) {
	p := modelPolicyFor(t, "", `{"availableModels":["opus-4","sonnet"],"availableModelsMatch":"exact"}`, providerAnthropic)
	for model, want := range map[string]bool{
		"claude-opus-4-20250514":   true,  // the exact version (date variant)
		"claude-opus-4-0":          true,  // 4 == 4.0
		"claude-opus-4-5":          false, // exact: no version-prefix match
		"claude-sonnet-4-5":        true,  // a family entry still allows its family
		"claude-haiku-4-5-2025111": false,
	} {
		if got := p.allows(model); got != want {
			t.Errorf("exact allows(%q) = %v, want %v", model, got, want)
		}
	}
	// Exact matching blocks even the default model outside the policy list
	//.
	if p.isDefaultModel("claude-opus-4-5", "claude-opus-4-5") {
		t.Errorf("exact-blocked default must not be exempt")
	}
	// Prefix matching (the default) keeps the version prefix.
	q := modelPolicyFor(t, "", `{"availableModels":["opus-4"]}`, providerAnthropic)
	if !q.allows("claude-opus-4-5") {
		t.Errorf("prefix mode: opus-4 must allow claude-opus-4-5")
	}
}

func TestModelPolicyModelOverrides(t *testing.T) {
	// A provider-specific id an admin mapped a listed model to counts as that
	// model; the policy's own map applies when it sets the list.
	p := modelPolicyFor(t, `{"modelOverrides":{"claude-opus-4-5":"user-proxy-opus"}}`,
		`{"availableModels":["claude-opus-4-5"],"modelOverrides":{"claude-opus-4-5":"corp-proxy-opus"}}`, providerAnthropic)
	if !p.allows("corp-proxy-opus") {
		t.Errorf("policy override target must count as its canonical model")
	}
	if p.allows("user-proxy-opus") {
		t.Errorf("a user modelOverrides entry must not remap past a policy allowlist")
	}
	// Without a policy list the cascaded overrides apply.
	q := modelPolicyFor(t, `{"availableModels":["claude-sonnet-4-5"],"modelOverrides":{"claude-sonnet-4-5":"my-sonnet"}}`, "", providerAnthropic)
	if !q.allows("my-sonnet") || q.allows("other-model") {
		t.Errorf("cascaded override mapping wrong")
	}
}

func TestModelPolicyDeniedModels(t *testing.T) {
	p := modelPolicyFor(t, "", `{"deniedModels":["claude-opus-4","haiku"]}`, providerAnthropic)
	for model, want := range map[string]bool{
		"claude-opus-4-5":   false, // a major-only entry blocks every 4.x
		"claude-opus-5-5":   true,
		"claude-haiku-4-5":  false,
		"anthropic/haiku-x": false, // family word
		"claude-sonnet-4-5": true,
	} {
		if got := p.allows(model); got != want {
			t.Errorf("denied allows(%q) = %v, want %v", model, got, want)
		}
	}
	// deniedModels also beats the default-model exemption.
	if p.allowsOrDefault("claude-opus-4-5", "claude-opus-4-5") {
		t.Errorf("a denied default model must stay refused")
	}
	// A user-settings deniedModels is not policy: ignored.
	q := modelPolicyFor(t, `{"deniedModels":["opus"]}`, "", providerAnthropic)
	if !q.allows("claude-opus-4-5") {
		t.Errorf("deniedModels is read from the admin policy only")
	}
}

func TestModelPolicyLoadFailure(t *testing.T) {
	// An unparsable managed-settings.json with no surviving tier: every model
	// is refused, the default model stays usable, and
	// cascade-trust enforcement is refused.
	p := modelPolicyFor(t, `{"availableModels":["opus"],"enforceAvailableModels":true}`, `{not json`, providerAnthropic)
	if !p.policyFailed {
		t.Fatalf("an unparsable policy file must count as a failed policy source")
	}
	if p.allows("claude-opus-4-5") || p.allows("gpt-5") {
		t.Errorf("a failed policy source must refuse every model")
	}
	if !p.allowsOrDefault("claude-opus-4-5", "claude-opus-4-5") {
		t.Errorf("the default model stays allowed")
	}
	if e := p.enforcement(); e.state != enforcementRefused {
		t.Errorf("enforcement = %+v, want refused", e)
	}
	if p.enforcedDefault() != "" {
		t.Errorf("no enforced default while the policy is refused")
	}
	found := false
	for _, n := range p.Notices() {
		if strings.Contains(n, "refusing cascade-trust mode") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing the refused-cascade notice: %v", p.Notices())
	}
	// An explicit allowlist (the enforcement path) skips the refusal.
	if !p.allowsWith("claude-opus-4-5", allowOptions{allowlist: []string{"opus"}, hasAllowlist: true}) {
		t.Errorf("explicit allowlist must skip the load-failure refusal")
	}
}

func TestModelPolicyEnforcement(t *testing.T) {
	// Policy-owned list + enforce: Default resolves to the first allowed entry.
	p := modelPolicyFor(t, "", `{"availableModels":["sonnet","opus"],"enforceAvailableModels":true}`, providerAnthropic)
	if e := p.enforcement(); e.state != enforcementActive {
		t.Fatalf("enforcement = %+v", e)
	}
	if got := p.enforcedDefault(); got != "claude-sonnet-5-5" {
		t.Errorf("enforcedDefault = %q", got)
	}
	// The enforce flag without a policy list does nothing.
	q := modelPolicyFor(t, `{"availableModels":["sonnet"]}`, `{"enforceAvailableModels":true}`, providerAnthropic)
	if q.enforcedDefault() != "" {
		t.Errorf("enforce without a policy-owned list must be inactive")
	}
	// No policy at all: user settings are cascade-trusted.
	u := modelPolicyFor(t, `{"availableModels":["claude-haiku-4-5"],"enforceAvailableModels":true}`, "", providerOpenRouter)
	if got := u.enforcedDefault(); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("cascade-trusted enforcedDefault = %q", got)
	}
	// A policy that exists (any key) withdraws the cascade trust.
	w := modelPolicyFor(t, `{"availableModels":["claude-haiku-4-5"],"enforceAvailableModels":true}`, `{"permissions":{"deny":["WebFetch"]}}`, providerOpenRouter)
	if w.enforcedDefault() != "" {
		t.Errorf("user enforcement must not apply under an admin policy")
	}
}

func TestModelPolicyStartupFallbackSwitch(t *testing.T) {
	p := cascade(providerAnthropic, "claude-opus-4-5", "sonnet")
	// Allowed --model passes.
	if m, r := p.startupModel("claude-sonnet-4-5"); m != "claude-sonnet-4-5" || r != "" {
		t.Errorf("allowed startup = %q,%q", m, r)
	}
	// A refused family alias steps down to the newest allowed model of it.
	if m, r := p.startupModel("opus"); m != "claude-opus-4-5" || r != "opus" {
		t.Errorf("step-down startup = %q,%q", m, r)
	}
	// A refused concrete model is dropped and reported.
	m, r := p.startupModel("gpt-5")
	if m != "" || r != "gpt-5" {
		t.Errorf("refused startup = %q,%q", m, r)
	}
	if got := restrictedModelNotice(r, m); !strings.Contains(got, `Model "gpt-5" is restricted by your organization's settings.`) {
		t.Errorf("notice = %q", got)
	}
	if got := restrictedModelNotice("gpt-5", "claude-sonnet-5-5"); got != `Model "gpt-5" is restricted by your organization's settings. Using claude-sonnet-5-5 instead.` {
		t.Errorf("Vw notice = %q", got)
	}
	// Fallback models keep only allowed entries.
	if got := p.fallbackModels([]string{"gpt-5", "claude-sonnet-4-6", "claude-opus-4-1"}); len(got) != 1 || got[0] != "claude-sonnet-4-6" {
		t.Errorf("fallbacks = %v", got)
	}
	// /model <id>.
	if m, msg := p.switchModel("gpt-5"); m != "" || msg != "Model 'gpt-5' is not available. Your organization restricts model selection." {
		t.Errorf("switch refused = %q,%q", m, msg)
	}
	if m, msg := p.switchModel("opus"); m != "claude-opus-4-5" || msg != "" {
		t.Errorf("switch step-down = %q,%q", m, msg)
	}
	// Picker.
	got := p.pickerModels([]provider.ModelInfo{{ID: "gpt-5"}, {ID: "claude-sonnet-4-6"}, {ID: "claude-opus-4-5-20251101"}, {ID: "claude-opus-4-6"}})
	if len(got) != 2 || got[0].ID != "claude-sonnet-4-6" || got[1].ID != "claude-opus-4-5-20251101" {
		t.Errorf("picker = %+v", got)
	}
	// Subagents: allowed runs, refused family steps down, refused id inherits.
	if got := p.subagentModel("sonnet", "parent"); got != "claude-sonnet-5-5" {
		t.Errorf("subagent sonnet = %q", got)
	}
	if got := p.subagentModel("opus", "parent"); got != "claude-opus-4-5" {
		t.Errorf("subagent opus = %q", got)
	}
	if got := p.subagentModel("claude-opus-4-6", "parent"); got != "parent" {
		t.Errorf("subagent refused = %q", got)
	}
}

// The live wiring: --model and the fallback chain go through the policy at
// session construction, and `/model <id>` through the conversation gate.
func TestModelPolicySessionWiring(t *testing.T) {
	env, _, _, _ := sessionEnv(t, `{"provider":"anthropic","availableModels":["sonnet"],"fallbackModel":["claude-opus-4-5","claude-sonnet-4-5"]}`)
	s := newPrintSession(env, &DispatchDefaults{Model: "claude-opus-4-5"})
	if s.Model != "" || s.restrictedModel != "claude-opus-4-5" {
		t.Fatalf("restricted --model: model=%q restricted=%q", s.Model, s.restrictedModel)
	}
	if len(s.FallbackModels) != 1 || s.FallbackModels[0] != "claude-sonnet-4-5" {
		t.Fatalf("fallbacks = %v", s.FallbackModels)
	}
	s = newPrintSession(env, &DispatchDefaults{Model: "claude-sonnet-4-5"})
	if s.Model != "claude-sonnet-4-5" || s.restrictedModel != "" {
		t.Fatalf("allowed --model: %q %q", s.Model, s.restrictedModel)
	}
}

// `/model <id>` goes through the conversation's model gate: a refused model
// prints the refusal and leaves the model as it was; a family alias
// steps down.
func TestModelPolicySlashModelGate(t *testing.T) {
	env, _, _, _ := sessionEnv(t, `{"availableModels":["claude-opus-4-5","sonnet"]}`)
	caller := &skillTurnCaller{model: "claude-sonnet-4-5", effort: "medium"}
	s := &printSession{
		Model: "claude-sonnet-4-5", Mode: toolreg.ModeDefault, Provider: providerAnthropic,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")
	before := rt.conv.ModelID
	res, err := rt.conv.Submit(context.Background(), "/model gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect == nil || res.Effect.Kind != builtincmds.EffectPrintText ||
		res.Effect.Text != "Model 'gpt-5' is not available. Your organization restricts model selection." {
		t.Fatalf("refusal effect = %+v", res.Effect)
	}
	if rt.conv.ModelID != before {
		t.Fatalf("model changed to %q", rt.conv.ModelID)
	}
	res, err = rt.conv.Submit(context.Background(), "/model opus")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect == nil || res.Effect.Kind != builtincmds.EffectSetModel || res.Effect.Model != "claude-opus-4-5" || rt.conv.ModelID != "claude-opus-4-5" {
		t.Fatalf("step-down effect = %+v model=%q", res.Effect, rt.conv.ModelID)
	}
}
