package settingsload

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/settingspolicy"
)

// hasWarningContaining reports whether any warning contains sub.
func hasWarningContaining(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestValidateUnknownKeyWarns(t *testing.T) {
	r := ParseContent("settings.json", `{"model": "sonnet", "totallyBogusKey": 1}`)

	if r.LoadState != settingspolicy.LoadLoaded {
		t.Fatalf("loadState = %q; want loaded (unknown key must not fail the load)", r.LoadState)
	}
	// The valid key is still applied; the unknown key is NOT dropped.
	if r.Settings["model"] != "sonnet" {
		t.Errorf("model not applied: %v", r.Settings["model"])
	}
	if _, ok := r.Settings["totallyBogusKey"]; !ok {
		t.Errorf("unknown key was dropped from settings; validation must be advisory only")
	}
	if !hasWarningContaining(r.Warnings, "totallyBogusKey") {
		t.Errorf("expected a warning naming the unknown key; got %v", r.Warnings)
	}
	if !hasWarningContaining(r.Warnings, "Unrecognized field") {
		t.Errorf("expected the 'Unrecognized field' wording; got %v", r.Warnings)
	}
	// No fatal/error diagnostics for an advisory schema violation.
	if len(r.Errors) != 0 {
		t.Errorf("unknown key produced hard errors %v; want none", r.Errors)
	}
}

func TestValidateWrongTypeWarns(t *testing.T) {
	// autoUpdatesChannel is a string; verbose is a boolean. Feed a bad type to
	// each and confirm it warns but still loads with the value intact.
	r := ParseContent("settings.json", `{"verbose": "yes", "cleanupPeriodDays": "soon"}`)

	if r.LoadState != settingspolicy.LoadLoaded {
		t.Fatalf("loadState = %q; want loaded (wrong type must not fail the load)", r.LoadState)
	}
	if r.Settings["verbose"] != "yes" {
		t.Errorf("verbose value was altered/dropped: %v", r.Settings["verbose"])
	}
	if !hasWarningContaining(r.Warnings, "verbose") ||
		!hasWarningContaining(r.Warnings, "Expected boolean, but received string") {
		t.Errorf("expected a boolean-type warning for verbose; got %v", r.Warnings)
	}
	if !hasWarningContaining(r.Warnings, "cleanupPeriodDays") ||
		!hasWarningContaining(r.Warnings, "Expected number, but received string") {
		t.Errorf("expected a number-type warning for cleanupPeriodDays; got %v", r.Warnings)
	}
	if len(r.Errors) != 0 {
		t.Errorf("wrong type produced hard errors %v; want none", r.Errors)
	}
}

func TestValidateValidSettingsNoWarnings(t *testing.T) {
	content := `{
		"model": "opus",
		"verbose": true,
		"cleanupPeriodDays": 30,
		"env": {"FOO": "bar"},
		"permissions": {},
		"enabledMcpjsonServers": ["a", "b"],
		"apiKeyHelper": "echo key",
		"theme": "dark",
		"$schema": "https://example/schema.json"
	}`
	r := ParseContent("settings.json", content)

	if r.LoadState != settingspolicy.LoadLoaded {
		t.Fatalf("loadState = %q; want loaded", r.LoadState)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("fully valid settings produced warnings: %v", r.Warnings)
	}
}

func TestValidateUnionTypes(t *testing.T) {
	// forceLoginOrgUUID accepts string OR array; strictPluginOnlyCustomization
	// accepts boolean OR array. Both forms must be warning-free.
	for _, content := range []string{
		`{"forceLoginOrgUUID": "org-123"}`,
		`{"forceLoginOrgUUID": ["org-1", "org-2"]}`,
		`{"strictPluginOnlyCustomization": true}`,
		`{"strictPluginOnlyCustomization": ["skills", "hooks"]}`,
	} {
		r := ParseContent("settings.json", content)
		if len(r.Warnings) != 0 {
			t.Errorf("union-typed value %s warned unexpectedly: %v", content, r.Warnings)
		}
	}

	// A type outside the union still warns.
	r := ParseContent("settings.json", `{"forceLoginOrgUUID": 5}`)
	if !hasWarningContaining(r.Warnings, "forceLoginOrgUUID") ||
		!hasWarningContaining(r.Warnings, "string or array") {
		t.Errorf("expected a union-type warning; got %v", r.Warnings)
	}
}

func TestValidateNullIsUnset(t *testing.T) {
	// A null value for an optional field is treated as unset, not a violation.
	r := ParseContent("settings.json", `{"model": null, "verbose": null}`)
	if len(r.Warnings) != 0 {
		t.Errorf("null-valued known keys warned: %v", r.Warnings)
	}
}

func TestValidateDoesNotChangeMalformedStates(t *testing.T) {
	// Unparsable content keeps its state and carries no warnings.
	if r := ParseContent("settings.json", `{"model": `); r.LoadState != settingspolicy.LoadUnparsable {
		t.Errorf("bad JSON: loadState = %q; want unparsable", r.LoadState)
	} else if len(r.Warnings) != 0 {
		t.Errorf("unparsable content should carry no warnings; got %v", r.Warnings)
	}

	// Non-object JSON stays malformed with no warnings.
	if r := ParseContent("settings.json", `[1,2,3]`); r.LoadState != settingspolicy.LoadMalformed {
		t.Errorf("non-object JSON: loadState = %q; want malformed", r.LoadState)
	} else if len(r.Warnings) != 0 {
		t.Errorf("malformed content should carry no warnings; got %v", r.Warnings)
	}

	// Empty content: loaded, no warnings.
	if r := ParseContent("settings.json", "   "); r.LoadState != settingspolicy.LoadLoaded || len(r.Warnings) != 0 {
		t.Errorf("empty content: state %q warnings %v; want loaded with no warnings", r.LoadState, r.Warnings)
	}
}

func TestValidationLeavesEffectiveSettingsUnchanged(t *testing.T) {
	// The merged/effective settings must be identical whether or not the object
	// carries schema violations: validation only observes, never mutates.
	content := `{"model": "opus", "bogusKey": {"nested": 1}, "verbose": "nope"}`
	r := ParseContent("settings.json", content)

	if got := len(r.Settings); got != 3 {
		t.Fatalf("settings key count = %d; want 3 (validation must not add/drop keys)", got)
	}
	for _, k := range []string{"model", "bogusKey", "verbose"} {
		if _, ok := r.Settings[k]; !ok {
			t.Errorf("key %q missing from settings after validation", k)
		}
	}
}

func TestValidateSearchSettingsNoWarnings(t *testing.T) {
	r := ParseContent("settings.json", `{
		"respectGitignore": false,
		"search": {"useUgrep": true, "ugrepPath": "/usr/bin/ugrep", "preferSystemUgrep": false, "includeHidden": false}
	}`)
	if len(r.Warnings) != 0 {
		t.Fatalf("search settings must not warn, got %v", r.Warnings)
	}
	// The ripgrep keys of earlier builds are unknown now, like any typo.
	r = ParseContent("settings.json", `{"search": {"useRipgrep": false, "ripgrepPath": "/usr/bin/rg", "preferSystemRipgrep": true}}`)
	for _, k := range []string{"useRipgrep", "ripgrepPath", "preferSystemRipgrep"} {
		if !hasWarningContaining(r.Warnings, "Unrecognized field: search."+k) {
			t.Errorf("search.%s must warn as unknown, got %v", k, r.Warnings)
		}
	}
	r = ParseContent("settings.json", `{"search": {"useUgrep": "yes"}}`)
	if len(r.Warnings) != 1 {
		t.Fatalf("a mistyped useUgrep must warn, got %v", r.Warnings)
	}
}

func TestValidateSearchSettingsNestedWarnings(t *testing.T) {
	r := ParseContent("settings.json", `{"search": {"includeHidden": "yes", "bogus": 1}}`)
	if !hasWarningContaining(r.Warnings, "Unrecognized field: search.bogus") {
		t.Errorf("expected nested unknown-key warning, got %v", r.Warnings)
	}
	if !hasWarningContaining(r.Warnings, "search.includeHidden: Expected boolean, but received string") {
		t.Errorf("expected nested type warning, got %v", r.Warnings)
	}
}

// TestValidateUISettings: every `ui` setting is known; a wrong type warns.
func TestValidateUISettings(t *testing.T) {
	r := ParseContent("settings.json", `{"ui": {"style": "modern", "icons": "nerd", "animations": false, "sidebar": "auto", "theme": "midnight"}}`)
	if len(r.Warnings) != 0 {
		t.Fatalf("ui settings must not warn, got %v", r.Warnings)
	}
	r = ParseContent("settings.json", `{"ui": {"animations": "no"}}`)
	if !hasWarningContaining(r.Warnings, "ui.animations: Expected boolean, but received string") {
		t.Errorf("expected a type warning for ui.animations, got %v", r.Warnings)
	}
}
