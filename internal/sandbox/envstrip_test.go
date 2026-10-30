package sandbox

import (
	"strings"
	"testing"
)

func TestSecretEnvNames(t *testing.T) {
	environ := []string{
		"OPENROUTER_API_KEY=x", "ANTHROPIC_AUTH_TOKEN=x", "OPENAI_ORG_ID=x", "OPENROUTER_BASE_URL=x",
		"GITHUB_TOKEN=x", "GH_TOKEN=x", "MCP_CLIENT_SECRET=x", "AWS_SECRET_ACCESS_KEY=x",
		"AWS_SESSION_TOKEN=x", "AWS_ACCESS_KEY_ID=x", "AZURE_STORAGE_ACCOUNT_KEY=x", "NPM_CONFIG__AUTH=x",
		"GOCODE_API_KEY=x", "HF_TOKEN=x", "db_password=x", "INPUT_GITHUB_TOKEN=x", "VAULT_TOKEN=x",
		// Not secrets:
		"PATH=/bin", "HOME=/h", "PWD=/p", "TERM=xterm", "LANG=C", "TOKENIZERS_PARALLELISM=false", "GOCODE_MODEL=m",
	}
	got := SecretEnvNames(environ, nil)
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		wantSecret := !map[string]bool{"PATH": true, "HOME": true, "PWD": true, "TERM": true, "LANG": true, "TOKENIZERS_PARALLELISM": true, "GOCODE_MODEL": true}[name]
		if set[name] != wantSecret {
			t.Errorf("%s stripped = %v, want %v", name, set[name], wantSecret)
		}
	}
	// Provider key names are stripped even when not currently exported.
	for _, n := range []string{"OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if !hasExactPath(SecretEnvNames(nil, nil), n) {
			t.Errorf("%s not in the default scrub", n)
		}
	}
	// The pass-through list re-admits a name.
	if hasExactPath(SecretEnvNames(environ, map[string]bool{"GH_TOKEN": true}), "GH_TOKEN") {
		t.Error("pass-through ignored")
	}
}
