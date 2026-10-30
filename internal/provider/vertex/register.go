package vertex

import (
	"context"

	"github.com/umairge66/gocode/internal/provider"
)

// The registry entry: Vertex AI authenticates with Google Application Default
// Credentials (no API key) and reads VERTEXAI_PROJECT / VERTEXAI_LOCATION.
func init() {
	provider.MustRegister(provider.Spec{
		ID:           ProviderName,
		DisplayName:  "Google Cloud Vertex AI (Gemini)",
		KeyOptional:  true,
		API:          provider.APICustom,
		Auth:         provider.AuthCustom,
		StaticModels: StaticModels,
		Docs:         "https://docs.cloud.google.com/vertex-ai/generative-ai/docs/migrate/openai/overview",
		New: func(c provider.Config) (provider.Provider, error) {
			return FromEnv(context.Background(), Options{Credentials: CredentialOptions{Getenv: c.Getenv}, HTTP: c.HTTP})
		},
	})
}
