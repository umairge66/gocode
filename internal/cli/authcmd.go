package cli

import (
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/provider"
)

// This file implements the `gocode auth` subcommand. Model access is via
// provider API keys only (internal/auth), so `auth` has a single verb,
// `status`, which reports — per provider — whether a key was found and its
// source (the env var name, "settings" or "apiKeyHelper"). The key itself is
// NEVER printed. There is no login/logout/setup-token: there is no gocode
// account or OAuth.

// newKeyResolver builds the provider-key resolver shared by the print path,
// the interactive path, the welcome banner and `auth status`, so all agree on
// precedence: <PROVIDER>_API_KEY > auth.json (written by /login) > settings
// "apiKeys" > apiKeyHelper. AuthFile is set to <configHome>/auth.json so a key
// saved by /login is picked up on the next run (env still wins).
func newKeyResolver(env *Env) *auth.Resolver {
	getenv := func(string) string { return "" }
	if env != nil && env.Getenv != nil {
		getenv = env.Getenv
	}
	return &auth.Resolver{
		Getenv:       getenv,
		AuthFile:     authFilePath(env),
		ProviderKeys: loadProviderKeys(env),
	}
}

// authFilePath resolves the on-disk auth.json path for the session: the default
// auth file under the resolved config home (GOCODE_CONFIG_DIR / XDG_CONFIG_HOME
// / HOME/.config/gocode). An unresolvable config home yields the bare filename.
func authFilePath(env *Env) string {
	configHome := ""
	if dir, ok := settingsContext(env).UserConfigDir(); ok {
		configHome = dir
	}
	return auth.DefaultAuthFilePath(configHome)
}

// runAuth dispatches `gocode auth [status]`. A bare `auth` behaves like
// `auth status`; any other verb is rejected.
func runAuth(env *Env, d *DispatchDefaults, rest []string) int {
	verb := ""
	seenAuth := false
	for _, x := range rest {
		if strings.HasPrefix(x, "-") {
			continue
		}
		if !seenAuth {
			seenAuth = x == "auth"
			continue
		}
		verb = x
		break
	}
	switch verb {
	case "", "status":
		return runAuthStatus(env, d)
	default:
		fmt.Fprintf(env.Stderr, "gocode auth: unknown command %q. Available: status.\n", verb)
		fmt.Fprintln(env.Stderr, "Model access uses provider API keys: set "+providerKeyVarsSummary()+", or an \"apiKeys\" map in settings.")
		return 1
	}
}

// runAuthStatus prints, for each provider, whether a key was resolved and from
// which source. It exits 0 when the active provider has a key, 1 otherwise.
func runAuthStatus(env *Env, d *DispatchDefaults) int {
	active, perr := resolveProvider(env, d)
	resolver := newKeyResolver(env)

	activeOK := false
	for _, p := range validProviders() {
		envName := strings.Join(auth.ProviderEnvVars(auth.Provider(p)), "/")
		marker := " "
		if p == active {
			marker = "*"
		}
		_, source, ok := resolver.ResolveProviderKey(auth.Provider(p))
		cloudAuth := ""
		if spec, found := provider.Lookup(p); found && spec.KeyOptional {
			cloudAuth = cloudCredentialHint(p)
		}
		if ok {
			fmt.Fprintf(env.Stdout, "%s %-10s key found (source: %s)\n", marker, p, source)
		} else if cloudAuth != "" {
			fmt.Fprintf(env.Stdout, "%s %-10s %s\n", marker, p, cloudAuth)
		} else if envName != "" {
			fmt.Fprintf(env.Stdout, "%s %-10s no key (set %s or settings apiKeys.%s)\n", marker, p, envName, p)
		} else {
			fmt.Fprintf(env.Stdout, "%s %-10s no key (settings apiKeys.%s)\n", marker, p, p)
		}
		if p == active && ok {
			activeOK = true
		}
	}
	if perr != nil {
		fmt.Fprintf(env.Stderr, "gocode auth: %v\n", perr)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Active provider: %s\n", active)
	if bu := resolveProviderBaseURL(env, active); bu.configured() {
		fmt.Fprintf(env.Stdout, "%s: %s\n", provider.BaseURLLabel(active), bu.display())
	}
	if !activeOK {
		return 1
	}
	return 0
}

// providerKeyVarsSummary is "OPENROUTER_API_KEY, ANTHROPIC_API_KEY, … or
// XAI_API_KEY": every registry provider's key variable.
func providerKeyVarsSummary() string {
	vars := auth.AllProviderEnvVars()
	switch len(vars) {
	case 0:
		return "a provider API key"
	case 1:
		return vars[0]
	}
	return strings.Join(vars[:len(vars)-1], ", ") + " or " + vars[len(vars)-1]
}

// cloudCredentialHint describes how a provider without a single API key signs
// in (checked when the session starts, not here).
func cloudCredentialHint(p string) string {
	switch p {
	case "bedrock":
		return "AWS credentials (AWS_BEARER_TOKEN_BEDROCK, AWS access keys, or AWS_PROFILE)"
	case "vertex":
		return "Google credentials (GOOGLE_APPLICATION_CREDENTIALS or `gcloud auth application-default login`) + VERTEXAI_PROJECT"
	case "azure":
		return "AZURE_OPENAI_API_ENDPOINT + AZURE_OPENAI_API_KEY, or Entra ID (`az login` or client credentials)"
	}
	return ""
}
