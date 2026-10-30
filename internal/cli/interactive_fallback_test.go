package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
)

// writeCwdSettings writes <cwd>/.gocode/<name>.
func writeCwdSettings(t *testing.T, env *Env, name, body string) {
	t.Helper()
	cwd, _ := env.Cwd()
	dir := filepath.Join(cwd, ".gocode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveFallbackModels_Precedence: --fallback-model > GOCODE_FALLBACK_MODEL
// > settings; the setting is a string or an array, a higher-priority source
// replaces a lower one, an empty value clears, and the policy filters.
func TestResolveFallbackModels_Precedence(t *testing.T) {
	join := func(s *printSession) string { return strings.Join(s.FallbackModels, ",") }

	env := providerTestEnv(t, nil, `{"fallbackModel":["u1","u2"]}`)
	if got := join(newPrintSession(env, nil)); got != "u1,u2" {
		t.Errorf("user array = %q", got)
	}
	writeCwdSettings(t, env, "settings.json", `{"fallbackModel":"p1"}`)
	if got := join(newPrintSession(env, nil)); got != "p1" {
		t.Errorf("project string beats user = %q", got)
	}
	writeCwdSettings(t, env, "settings.local.json", `{"fallbackModel":"l1, l2"}`)
	if got := join(newPrintSession(env, nil)); got != "l1,l2" {
		t.Errorf("local comma string = %q", got)
	}
	writeCwdSettings(t, env, "settings.local.json", `{"fallbackModel":""}`)
	if got := join(newPrintSession(env, nil)); got != "" {
		t.Errorf("local \"\" should clear, got %q", got)
	}

	envVar := providerTestEnv(t, map[string]string{envFallbackModel: "e1,e2"}, `{"fallbackModel":["u1"]}`)
	if got := join(newPrintSession(envVar, nil)); got != "e1,e2" {
		t.Errorf("env beats settings = %q", got)
	}
	if got := join(newPrintSession(envVar, &DispatchDefaults{FallbackModel: "f1"})); got != "f1" {
		t.Errorf("flag beats env = %q", got)
	}

	pol := providerTestEnv(t, map[string]string{envFallbackModel: "gpt-5,claude-sonnet-4-5"},
		`{"provider":"anthropic","availableModels":["sonnet"]}`)
	if got := join(newPrintSession(pol, nil)); got != "claude-sonnet-4-5" {
		t.Errorf("policy filter = %q", got)
	}
}

// TestFallbackCommand: /fallback sets the live chain, persists it to local
// settings, shows it in /status, refuses policy-denied and main models, and
// `off` clears it (persisted as "" so a user-level value is overridden).
func TestFallbackCommand(t *testing.T) {
	env := providerTestEnv(t, nil, `{"fallbackModel":["user-fb"]}`)
	cwd, _ := env.Cwd()
	ps := newPrintSession(env, nil)
	pc := provider.NewModelCaller(&effortRecorder{}, "main:free")
	conv := &conversation.Conversation{Model: pc}
	conv.SetFallbackModels(ps.FallbackModels)
	ctl := fallbackControl{conv: conv, print: ps, env: env}

	if got := ctl.command(""); got != "Fallback model: user-fb. Clear it with /fallback off." {
		t.Errorf("current = %q", got)
	}
	if got := ctl.command("cohere/north-mini-code:free"); got != "Fallback model set to cohere/north-mini-code:free." {
		t.Errorf("set = %q", got)
	}
	if got := strings.Join(conv.FallbackModelList(), ","); got != "cohere/north-mini-code:free" {
		t.Errorf("live chain = %q", got)
	}
	if got := localSettings(t, cwd)["fallbackModel"]; got != "cohere/north-mini-code:free" {
		t.Errorf("persisted = %v", got)
	}
	if got := strings.Join(newPrintSession(env, nil).FallbackModels, ","); got != "cohere/north-mini-code:free" {
		t.Errorf("next session = %q", got)
	}
	status := cmdInfo{conv: conv, print: ps, env: env}.statusText()
	if !strings.Contains(status, "  Fallback model: cohere/north-mini-code:free\n") {
		t.Errorf("status missing the fallback row:\n%s", status)
	}

	// A chain is persisted as an array; the main model is dropped from it.
	if got := ctl.command("a, main:free, b"); got != "Fallback model set to a → b." {
		t.Errorf("chain = %q", got)
	}
	if arr, ok := localSettings(t, cwd)["fallbackModel"].([]any); !ok || len(arr) != 2 || arr[0] != "a" || arr[1] != "b" {
		t.Errorf("persisted chain = %v", localSettings(t, cwd)["fallbackModel"])
	}
	if got := ctl.command("main:free"); !strings.Contains(got, "already the main model") {
		t.Errorf("main model = %q", got)
	}

	// off clears live and overrides the user-level setting next session.
	if got := ctl.command("off"); got != "Fallback model off." {
		t.Errorf("off = %q", got)
	}
	if len(conv.FallbackModelList()) != 0 || len(ps.FallbackModels) != 0 {
		t.Error("off did not clear the live chain")
	}
	if got := newPrintSession(env, nil).FallbackModels; len(got) != 0 {
		t.Errorf("after off next session = %v", got)
	}
	if !strings.Contains(cmdInfo{conv: conv, print: ps, env: env}.statusText(), "  Fallback model: none\n") {
		t.Error("status should show none")
	}
	if got := ctl.command("help"); !strings.Contains(got, "Usage: /fallback") {
		t.Errorf("help = %q", got)
	}

	// The model policy refuses a fallback outside availableModels.
	polEnv := providerTestEnv(t, nil, `{"availableModels":["allowed-model"],"availableModelsMatch":"exact"}`)
	pctl := fallbackControl{conv: &conversation.Conversation{Model: pc}, print: &printSession{}, env: polEnv}
	if got := pctl.command("other-model"); got != "Model 'other-model' is not available. Your organization restricts model selection." {
		t.Errorf("policy refusal = %q", got)
	}
	if got := pctl.command("allowed-model"); got != "Fallback model set to allowed-model." {
		t.Errorf("policy allowed = %q", got)
	}
}

// TestFallbackIsBuiltinCommand: /fallback is a dialog built-in and -p prints
// guidance for it.
func TestFallbackIsBuiltinCommand(t *testing.T) {
	tbl := builtincmds.NewTable()
	eff, ok := tbl.Dispatch("/fallback off")
	if !ok || eff.Kind != builtincmds.EffectOpenDialog || eff.Dialog != "fallback" || strings.TrimSpace(eff.Args) != "off" {
		t.Fatalf("dispatch = %+v", eff)
	}
	if got := renderLocalEffect(&eff); !strings.Contains(got, "--fallback-model") {
		t.Errorf("-p guidance = %q", got)
	}
}

// TestFallbackCommandOverProvider: a chain set with /fallback is used by the
// next turn when the main model is rate limited.
func TestFallbackCommandOverProvider(t *testing.T) {
	env := providerTestEnv(t, nil, "")
	rl := &rateLimitedProvider{}
	pc := provider.NewModelCaller(rl, "main:free")
	pc.MaxRetries = 0
	conv := conversation.New(pc, nil)
	conv.FallbackNotice = provider.FallbackNotice
	fallbackControl{conv: conv, print: &printSession{}, env: env}.command("backup")
	res, err := conv.Submit(context.Background(), "hi")
	if err != nil || res.Err != nil {
		t.Fatalf("err=%v res.Err=%v", err, res.Err)
	}
	if len(res.Notices) != 1 || res.Notices[0] != "Switched to backup due to rate limits on main:free" {
		t.Fatalf("notices = %v", res.Notices)
	}
	if strings.Join(rl.models, ",") != "main:free,backup" {
		t.Fatalf("models = %v", rl.models)
	}
}

// rateLimitedProvider rate-limits every model but "backup".
type rateLimitedProvider struct{ models []string }

func (r *rateLimitedProvider) Name() string { return "openrouter" }
func (r *rateLimitedProvider) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	r.models = append(r.models, req.Model)
	if req.Model != "backup" {
		return provider.ChatResponse{}, &provider.APIError{Provider: "openrouter", Status: 429, Category: provider.CategoryRateLimit,
			Message: "Rate limited by openrouter.", Retryable: true, Detail: "Rate limit exceeded: free-models-per-min."}
	}
	return provider.ChatResponse{Content: "ok", FinishReason: provider.FinishStop}, nil
}
func (r *rateLimitedProvider) StreamChat(ctx context.Context, req provider.ChatRequest, _ func(string)) (provider.ChatResponse, error) {
	return r.Chat(ctx, req)
}
