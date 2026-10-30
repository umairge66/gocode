package sandbox

import (
	"strings"

	"github.com/umairge66/gocode/internal/envsec"
)

// Secret-env scrubbing. A sandboxed command inherits the gocode process
// environment (bwrap / sandbox-exec drop only the names passed with
// --unsetenv / `env -u`). Provider API keys and other credential-bearing
// variables exported into gocode's environment would otherwise be visible to
// every sandboxed command (verified: OPENROUTER_API_KEY readable inside the
// sandbox). By default the sandbox strips every variable that looks like a
// credential, so a command the model runs — or a compromised dependency it
// builds — cannot exfiltrate the user's keys. The user re-admits specific
// variables with the setting `sandbox.credentials.passThroughEnvVars`
// (user / --settings / managed settings only). Variables pointing at host IPC
// sockets are unset too (ipc.go).
//
// No data leaves the machine; the scrub only removes names from the child
// environment.

// providerEnvVarNames are the API-key environment variables of every provider
// gocode authenticates to directly (internal/auth). They are always stripped
// (unless explicitly allowed) even when not currently exported, so the list is
// independent of the live environment.
func providerEnvVarNames() []string {
	return envsec.ProviderKeyEnvNames()
}

// secretNamePrefixes are name prefixes whose variables carry provider
// configuration and credentials. The provider families are stripped wholesale
// (ANTHROPIC_*, OPENAI_*, OPENROUTER_*) so a renamed or future key variable is
// caught too.
var secretNamePrefixes = []string{"ANTHROPIC_", "OPENAI_", "OPENROUTER_"}

// secretNameSubstrings are substrings (checked case-insensitively on the
// variable name) that reliably indicate a secret value. They catch, among
// others, GITHUB_TOKEN / GH_TOKEN (TOKEN), MCP_CLIENT_SECRET (SECRET),
// AWS_ACCESS_KEY_ID (ACCESS_KEY), AWS_SECRET_ACCESS_KEY (SECRET),
// AWS_SESSION_TOKEN (TOKEN), and any *_API_KEY / *_PASSWORD / *_CREDENTIALS.
var secretNameSubstrings = []string{
	"SECRET", "TOKEN", "PASSWORD", "PASSWD", "PASSPHRASE",
	"APIKEY", "API_KEY", "ACCESSKEY", "ACCESS_KEY", "PRIVATE_KEY",
	"CREDENTIAL", "SESSION_KEY", "BEARER", "AUTH_TOKEN",
	// internal/redact's sensitive-assignment key heuristic (account /
	// subscription keys, "<x>_auth").
	"ACCOUNT_KEY", "ACCOUNTKEY", "SUBSCRIPTION_KEY", "SUBSCRIPTIONKEY", "PRIVATEKEY",
}

// secretNameSuffixes catch "<x>_AUTH" (NPM_CONFIG__AUTH, CONSUL_HTTP_AUTH).
var secretNameSuffixes = []string{"_AUTH"}

// notSecretNames match a heuristic but carry no secret.
var notSecretNames = map[string]bool{"TOKENIZERS_PARALLELISM": true}

// isSecretEnvName reports whether an environment variable name is treated as
// credential-bearing by the default scrub: a name in the envsec credential
// table, a provider-family prefix, or a secret-indicating substring.
func isSecretEnvName(name string) bool {
	if name == "" {
		return false
	}
	if envsec.IsCredentialEnvVar(name) {
		return true
	}
	u := strings.ToUpper(name)
	// GitHub Actions exposes inputs as INPUT_<NAME>; fold that spelling.
	u = strings.TrimPrefix(u, "INPUT_")
	if notSecretNames[u] {
		return false
	}
	for _, sfx := range secretNameSuffixes {
		if strings.HasSuffix(u, sfx) {
			return true
		}
	}
	for _, p := range secretNamePrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	for _, s := range secretNameSubstrings {
		if strings.Contains(u, s) {
			return true
		}
	}
	return false
}

// SecretEnvNames returns the credential-bearing variable names to strip from a
// sandboxed command's environment: every known provider key variable, plus any
// name in environ that isSecretEnvName reports, minus the names the user has
// explicitly allowed. environ is a list of "NAME=VALUE" entries (os.Environ
// form); only the names are inspected, never the values.
func SecretEnvNames(environ []string, allow map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" || allow[name] || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, n := range providerEnvVarNames() {
		add(n)
	}
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if isSecretEnvName(name) {
			add(name)
		}
	}
	return out
}

// mergeSecretUnset combines the explicit credential-deny list with the default
// secret-env scrub. Explicitly denied names are always unset (an explicit deny
// is never overridden by the allow list); the default scrub honors allow.
func mergeSecretUnset(explicitDeny, environ, allow []string) []string {
	allowSet := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowSet[a] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range explicitDeny {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range SecretEnvNames(environ, allowSet) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
