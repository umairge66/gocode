package envsec

// IsSecretEnvName reports whether an environment variable must never be
// expanded into a value that leaves the machine toward a configurable
// endpoint (an HTTP hook header, a remote MCP server's URL or headers): the
// credential name table (IsCredentialEnvVar) and the provider API-key
// variables gocode reads. Other names (e.g. a GITHUB_TOKEN the user
// deliberately routes to a GitHub MCP server) still expand.
func IsSecretEnvName(name string) bool {
	if IsCredentialEnvVar(name) {
		return true
	}
	// Every provider API-key variable (OPENROUTER_API_KEY, ANTHROPIC_API_KEY,
	// OPENAI_API_KEY, GROQ_API_KEY, HF_TOKEN, …: auth's provider table).
	return IsProviderKeyEnvName(name)
}
