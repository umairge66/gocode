package cli

import (
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// providerBaseURL is a provider's user-configured API base URL, resolved from
// the environment and the settings "env" blocks. The zero value means "not
// configured" (the provider's built-in endpoint is used).
type providerBaseURL struct {
	// URL is the validated base URL ("" when not configured or invalid).
	URL string
	// EnvVar is the variable that supplied it (e.g. OPENAI_BASE_URL).
	EnvVar string
	// Origin says where EnvVar was set: "environment" or "<source> env"
	// (e.g. "userSettings env").
	Origin string
	// Err is set when a value was configured but is not a usable http(s) URL;
	// the session then refuses to build the provider rather than fall back to
	// the default endpoint silently.
	Err error
	// raw is the configured value as written (shown, redacted, when invalid).
	raw string
	// Ignored lists base-URL variables found in a project/local settings "env"
	// block, which are never applied (see resolveProviderBaseURL).
	Ignored []string
}

// configured reports whether a base URL (valid or not) was configured.
func (b providerBaseURL) configured() bool { return b.EnvVar != "" }

// display renders the configured value for /status, doctor and auth status,
// with any embedded credentials masked.
func (b providerBaseURL) display() string {
	return fmt.Sprintf("%s (%s, %s)", provider.RedactBaseURL(b.rawOrURL()), b.EnvVar, b.Origin)
}

func (b providerBaseURL) rawOrURL() string {
	if b.URL != "" {
		return b.URL
	}
	return b.raw
}

// trustedBaseURLSources are the settings sources whose "env" block may set a
// provider base URL: the user, flag and policy "env" blocks (settings values
// override the launch environment, policy last). Project/local base URLs are
// never applied, even in a folder trusted through the light trust dialog
// (trust.go), since a repository could otherwise redirect the user's API key
// to its own server.
var trustedBaseURLSources = map[settingspolicy.Source]bool{
	settingspolicy.SourceUser:   true,
	settingspolicy.SourceFlag:   true,
	settingspolicy.SourcePolicy: true,
}

// resolveProviderBaseURL resolves the base URL configured for provider p. The
// effective environment is the launch environment overlaid by the trusted
// settings "env" blocks (user < flag < policy); then p's variables are
// consulted in precedence order
// (provider.BaseURLEnvVars). An unknown provider has no base URL.
func resolveProviderBaseURL(env *Env, p string) providerBaseURL {
	if len(provider.BaseURLEnvVars(p)) == 0 {
		return providerBaseURL{}
	}
	return resolveProviderBaseURLFrom(env, loadedSettingsSources(env), p)
}

// loadedSettingsSources loads the effective settings sources once (in
// canonical order), keeping only the loaded ones.
func loadedSettingsSources(env *Env) []settingsload.OrderedResult {
	var out []settingsload.OrderedResult
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState == settingspolicy.LoadLoaded {
			out = append(out, or)
		}
	}
	return out
}

// resolveProviderBaseURLFrom is resolveProviderBaseURL over pre-loaded
// settings sources.
func resolveProviderBaseURLFrom(env *Env, sources []settingsload.OrderedResult, p string) providerBaseURL {
	names := provider.BaseURLEnvVars(p)
	if len(names) == 0 {
		return providerBaseURL{}
	}
	type val struct{ v, origin string }
	merged := map[string]val{}
	getenv := envGetenv(env)
	for _, n := range names {
		if v := strings.TrimSpace(getenv(n)); v != "" {
			merged[n] = val{v, "environment"}
		}
	}
	var ignored []string
	for _, or := range sources {
		block, _ := or.Result.Settings["env"].(map[string]any)
		for _, n := range names {
			s, ok := block[n].(string)
			if !ok || strings.TrimSpace(s) == "" {
				continue
			}
			if !trustedBaseURLSources[or.Source] {
				ignored = append(ignored, fmt.Sprintf("%s (%s env)", n, or.Source))
				continue
			}
			merged[n] = val{strings.TrimSpace(s), string(or.Source) + " env"}
		}
	}
	out := providerBaseURL{Ignored: ignored}
	for _, n := range names {
		v, ok := merged[n]
		if !ok {
			continue
		}
		out.EnvVar, out.Origin, out.raw = n, v.origin, v.v
		u, err := provider.ValidateBaseURL(v.v)
		if err != nil {
			out.Err = fmt.Errorf("%s: %v", n, err)
			return out
		}
		out.URL = u
		return out
	}
	return out
}

// resolveProviderBaseURLs resolves every known provider's base URL once, so a
// mid-session /provider switch pairs the new provider's key with that
// provider's own configured endpoint (never another provider's).
func resolveProviderBaseURLs(env *Env) map[string]providerBaseURL {
	ids := validProviders()
	out := make(map[string]providerBaseURL, len(ids))
	sources := loadedSettingsSources(env)
	for _, p := range ids {
		out[p] = resolveProviderBaseURLFrom(env, sources, p)
	}
	return out
}
