package cli

import (
	"testing"

	"github.com/umairge66/gocode/internal/provider"
)

// The providers with their own wire are registered (via providers_extra.go)
// and construct from their usual environment.
func TestExtraProvidersRegistered(t *testing.T) {
	env := map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "fake-secret", "AWS_REGION": "us-west-2",
		"AZURE_OPENAI_API_ENDPOINT": "https://example.openai.azure.com", "AZURE_OPENAI_API_KEY": "fake-azure-key",
	}
	getenv := func(k string) string { return env[k] }
	ids := map[string]bool{}
	for _, id := range provider.ProviderIDs() {
		ids[id] = true
	}
	for _, id := range []string{"bedrock", "vertex", "azure"} {
		if !ids[id] {
			t.Fatalf("%s not registered; have %v", id, provider.ProviderIDs())
		}
	}
	for _, id := range []string{"bedrock", "azure"} {
		p, err := provider.New(id, provider.Config{Getenv: getenv})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if p.Name() != id {
			t.Fatalf("%s: Name() = %q", id, p.Name())
		}
	}
}
