package azure

import (
	"os"

	"github.com/umairge66/gocode/internal/provider"
)

// The registry entry: Azure OpenAI needs an endpoint and either an api-key or
// a Microsoft Entra ID token (client credentials or the Azure CLI); the model
// the user picks is the deployment name.
func init() {
	provider.MustRegister(provider.Spec{
		ID:          ProviderName,
		DisplayName: "Azure OpenAI",
		KeyEnvVars:  []string{EnvAPIKey},
		KeyOptional: true,
		API:         provider.APICustom,
		Auth:        provider.AuthCustom,
		Docs:        "https://learn.microsoft.com/en-us/azure/ai-foundry/openai/reference",
		New: func(c provider.Config) (provider.Provider, error) {
			getenv := c.Getenv
			if getenv == nil {
				getenv = os.Getenv
			}
			env := getenv
			if c.APIKey != "" {
				// A key from auth.json (/login) counts like the env var.
				env = func(k string) string {
					if k == EnvAPIKey {
						return c.APIKey
					}
					return getenv(k)
				}
			}
			return FromEnv(Options{Getenv: env, HTTP: c.HTTP})
		},
	})
}
