package envsec

import (
	"os"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/auth"
)

// Provider-key stripping for child processes.
//
// gocode reads its own model-provider API keys from the environment
// (OPENROUTER_API_KEY, ANTHROPIC_API_KEY, OPENAI_API_KEY, GROQ_API_KEY,
// XAI_API_KEY, HF_TOKEN, … — every variable of auth's provider table — plus the GOCODE_*
// credential variables). Child processes that gocode spawns on the user's
// behalf — LSP servers, MCP stdio servers, hooks, the status line command,
// monitors, the Bash tool, background tasks — never need those keys, so they
// are removed from the environment those children inherit. Only gocode's own
// key variables are removed: the user's other credentials (AWS_*,
// GITHUB_TOKEN, ...) stay, because tools legitimately use them. The sandboxed
// Bash path strips far more (internal/sandbox/envstrip.go). There is no
// opt-out setting; the opt-in GOCODE_CODE_SUBPROCESS_ENV_SCRUB scrub is a
// different, broader mechanism.

// ProviderKeyEnvNames returns the environment variable names that carry
// gocode's own provider credentials: every provider key variable from
// internal/auth plus the GOCODE_-prefixed credential names of the credential
// table. The result is a fresh slice.
func ProviderKeyEnvNames() []string {
	out := auth.AllProviderEnvVars()
	for _, n := range credentialEnvNames {
		if strings.HasPrefix(n, "GOCODE_") {
			out = append(out, n)
		}
	}
	return out
}

// providerKeyNameSet is the upper-cased ProviderKeyEnvNames set. It is built
// on demand (and rebuilt when the provider table grows) rather than at package
// init, so key variables of providers registered later — from another
// package's init through auth.RegisterProvider — are stripped too.
var (
	providerKeyNameMu  sync.Mutex
	providerKeyNameSet map[string]bool
	providerKeyNameLen int
)

func providerKeyNames() map[string]bool {
	names := ProviderKeyEnvNames()
	providerKeyNameMu.Lock()
	defer providerKeyNameMu.Unlock()
	if providerKeyNameSet == nil || providerKeyNameLen != len(names) {
		m := make(map[string]bool, len(names))
		for _, n := range names {
			m[strings.ToUpper(n)] = true
		}
		providerKeyNameSet, providerKeyNameLen = m, len(names)
	}
	return providerKeyNameSet
}

// IsProviderKeyEnvName reports whether name is one of gocode's own provider
// credential variables (case-insensitive, as Windows environment names are).
func IsProviderKeyEnvName(name string) bool {
	return providerKeyNames()[strings.ToUpper(name)]
}

// StripProviderKeys returns env ("NAME=VALUE" entries, os.Environ form)
// without gocode's own provider credential variables. The input is not
// modified; a nil input yields a non-nil (possibly empty) slice so callers can
// assign it to exec.Cmd.Env without falling back to inheritance.
func StripProviderKeys(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			name = kv[:i]
		}
		if IsProviderKeyEnvName(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ChildEnv is the environment for a child process that would otherwise
// inherit gocode's environment: env when non-nil (the caller's explicit
// environment), else os.Environ(), with provider credentials removed.
func ChildEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	return StripProviderKeys(env)
}
