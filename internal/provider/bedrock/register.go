package bedrock

import (
	"os"

	"github.com/umairge66/gocode/internal/provider"
)

// The registry entry: Bedrock has its own wire (Converse) and finds its own
// AWS credentials, so the bearer token is optional and the base URL defaults
// to the region's bedrock-runtime endpoint.
func init() {
	provider.MustRegister(provider.Spec{
		ID:             "bedrock",
		DisplayName:    "Amazon Bedrock",
		KeyEnvVars:     []string{"AWS_BEARER_TOKEN_BEDROCK"},
		KeyOptional:    true,
		BaseURLEnvVars: []string{"AWS_ENDPOINT_URL_BEDROCK_RUNTIME"},
		API:            provider.APICustom,
		Auth:           provider.AuthCustom,
		StaticModels:   StaticModels(),
		Docs:           "https://docs.aws.amazon.com/bedrock/latest/userguide/conversation-inference.html",
		New: func(c provider.Config) (provider.Provider, error) {
			base := c.ExactBaseURL
			if base == "" {
				base = c.BaseURL
			}
			getenv := c.Getenv
			if getenv == nil {
				getenv = os.Getenv
			}
			return New(Config{BearerToken: c.APIKey, BaseURL: base, HTTP: c.HTTP, Getenv: getenv})
		},
	})
}
