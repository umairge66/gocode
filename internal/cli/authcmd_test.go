package cli

import (
	"strings"
	"testing"
)

// TestAuthStatus_ReportsSourcesWithoutKeys asserts `auth status` reports each
// provider's key source (env var name / settings) and never prints the key.
func TestAuthStatus_ReportsSourcesWithoutKeys(t *testing.T) {
	env := providerTestEnv(t,
		map[string]string{"OPENROUTER_API_KEY": "sk-or-SECRET-env"},
		`{"apiKeys":{"openai":"sk-oa-SECRET-settings"}}`)
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb

	code := Run(env, []string{"gocode", "auth", "status"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (active openrouter has a key); stderr=%q", code, errb.String())
	}
	got := out.String()
	if strings.Contains(got, "SECRET") {
		t.Fatalf("auth status leaked a key:\n%s", got)
	}
	for _, want := range []string{
		"openrouter key found (source: OPENROUTER_API_KEY)",
		"anthropic  no key (set ANTHROPIC_API_KEY",
		"openai     key found (source: settings)",
		"Active provider: openrouter",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestAuthStatus_BareAuthAndNoKey covers `gocode auth` (== status) with no
// keys: exit 1 and no GOCODE_API_KEY fallback.
func TestAuthStatus_BareAuthAndNoKey(t *testing.T) {
	env := providerTestEnv(t, map[string]string{"GOCODE_API_KEY": "sk-generic-SECRET"}, "")
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb

	if code := Run(env, []string{"gocode", "auth"}); code != 1 {
		t.Fatalf("exit = %d, want 1 (no provider key)", code)
	}
	if strings.Contains(out.String(), "key found") || strings.Contains(out.String(), "SECRET") {
		t.Fatalf("GOCODE_API_KEY must not count as a provider key:\n%s", out.String())
	}
}

// TestAuth_OAuthVerbsRemoved asserts the gocode OAuth verbs are gone.
func TestAuth_OAuthVerbsRemoved(t *testing.T) {
	for _, verb := range []string{"login", "logout", "setup-token"} {
		env := providerTestEnv(t, nil, "")
		var out, errb strings.Builder
		env.Stdout, env.Stderr = &out, &errb
		if code := Run(env, []string{"gocode", "auth", verb}); code != 1 {
			t.Errorf("auth %s exit = %d, want 1", verb, code)
		}
		if !strings.Contains(errb.String(), "unknown command") {
			t.Errorf("auth %s stderr = %q", verb, errb.String())
		}
	}
}

// TestPrintSession_IgnoresGenericAPIKey asserts GOCODE_API_KEY no longer
// satisfies the print path: it still asks for OPENROUTER_API_KEY.
func TestPrintSession_IgnoresGenericAPIKey(t *testing.T) {
	env := providerTestEnv(t, map[string]string{"GOCODE_API_KEY": "x"}, "")
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb
	if code := Run(env, []string{"gocode", "-p", "hi"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("stderr = %q, want the set-OPENROUTER_API_KEY message", errb.String())
	}
}
