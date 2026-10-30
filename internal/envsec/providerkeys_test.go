package envsec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/auth"
)

func TestStripProviderKeys(t *testing.T) {
	in := []string{
		"OPENROUTER_API_KEY=x", "ANTHROPIC_API_KEY=x", "OPENAI_API_KEY=x", "openai_api_key=x",
		"GROQ_API_KEY=x", "HF_TOKEN=x",
		"AWS_SECRET_ACCESS_KEY=x", "GITHUB_TOKEN=x", "PATH=/bin", "GOCODE_CONFIG_DIR=/c", "OPENROUTER_BASE_URL=u", "=C:=x",
	}
	want := []string{"AWS_SECRET_ACCESS_KEY=x", "GITHUB_TOKEN=x", "PATH=/bin", "GOCODE_CONFIG_DIR=/c", "OPENROUTER_BASE_URL=u", "=C:=x"}
	if got := StripProviderKeys(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := StripProviderKeys(nil); got == nil {
		t.Fatal("nil input must yield a non-nil slice")
	}
	t.Setenv("OPENROUTER_API_KEY", "fake")
	for _, kv := range ChildEnv(nil) {
		if len(kv) >= 19 && kv[:19] == "OPENROUTER_API_KEY=" {
			t.Fatal("ChildEnv kept the key")
		}
	}
}

// TestStripProviderKeys_AllRegistryProviders: every provider key variable of
// internal/auth's table (the registry's new providers included) is stripped
// from child processes and treated as a secret name, while their base-URL
// variables and unrelated HF_* settings pass through.
func TestStripProviderKeys_AllRegistryProviders(t *testing.T) {
	keys := []string{
		"VERCEL_API_KEY", "GEMINI_API_KEY", "ZAI_API_KEY", "MINIMAX_API_KEY", "SYNTHETIC_API_KEY",
		"HF_TOKEN", "CEREBRAS_API_KEY", "IONET_API_KEY", "ALIBABA_SINGAPORE_API_KEY", "ALIBABA_US_API_KEY",
		"GROQ_API_KEY", "AVIAN_API_KEY", "OPENCODE_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY",
	}
	var in []string
	for _, k := range keys {
		if !IsProviderKeyEnvName(k) || !IsProviderKeyEnvName(strings.ToLower(k)) {
			t.Errorf("%s is not a provider key name", k)
		}
		if !IsSecretEnvName(k) {
			t.Errorf("%s is not a secret name", k)
		}
		in = append(in, k+"=fake")
	}
	in = append(in, "GROQ_BASE_URL=u", "HF_HOME=/h")
	if got := StripProviderKeys(in); !reflect.DeepEqual(got, []string{"GROQ_BASE_URL=u", "HF_HOME=/h"}) {
		t.Fatalf("got %v", got)
	}
}

// TestProviderKeyNames_FollowRegistration: a provider registered after
// start-up (auth.RegisterProvider, as provider.Register does) is stripped too.
func TestProviderKeyNames_FollowRegistration(t *testing.T) {
	if IsProviderKeyEnvName("ENVSEC_TEST_LATE_KEY") {
		t.Fatal("precondition")
	}
	auth.RegisterProvider("envsec-test-late", "ENVSEC_TEST_LATE_KEY")
	t.Cleanup(func() { auth.UnregisterProvider("envsec-test-late") })
	if !IsProviderKeyEnvName("ENVSEC_TEST_LATE_KEY") {
		t.Fatal("late-registered key variable is not stripped")
	}
}
