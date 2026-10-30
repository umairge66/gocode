// Package secretpaths is the credential/secret path registry used when the
// admin-tier settings merge engine inherits settings from a lower tier.
//
// The merge engine strips these keys from every lower admin tier before it is
// deep-merged beneath the winning tier, so credential helpers, auth/gateway
// pins and composition-control keys are never inherited from a lower-priority
// source.
package secretpaths

// SecretPaths is the registry of top-level settings keys treated as
// credential/secret or otherwise non-inheritable by the admin-tier merge.
// StripSecrets removes exactly these keys from a lower tier.
//
// Every entry is a single top-level
// key (no nested/dotted paths), which matters for StripSecrets' behavior.
var SecretPaths = []string{
	// Credential helpers plus the OTel/proxy header helpers.
	"apiKeyHelper",
	"awsAuthRefresh",
	"awsCredentialExport",
	"gcpAuthRefresh",
	"otelHeadersHelper",
	"proxyAuthHelper",
	// Auth / gateway pins.
	"forceLoginOrgUUID",
	"forceLoginMethod",
	"forceLoginGatewayUrl",
	"gatewayInternalNetworks",
	// Composition-control keys a lower tier must not contribute.
	"parentSettingsBehavior",
	"env",
	"modelPicker",
	// Policy-helper configuration.
	"policyHelper",
	"policyHelpers",
	// Managed-sources composition controls.
	"managedSourcesBehavior",
	"wslInheritsWindowsSettings",
}

// StripSecrets returns a new settings object equal to the input with every key
// in SecretPaths removed. The input map is never mutated.
//
// There is no masking/replacement value — the keys are removed entirely
// (deleted, not blanked).
//
// The retained values are deep-cloned so the returned object is
// fully independent of the input: the merge engine mutates the
// sanitized tier afterward (DeletePath for non-inheritable nested paths, and
// the sandbox/enabledPlatforms scoping), and an independent clone keeps those
// mutations from leaking back into the caller's original tier.
//
// StripSecrets(nil) returns a non-nil empty map.
func StripSecrets(settings map[string]any) map[string]any {
	out := make(map[string]any, len(settings))
	if settings == nil {
		return out
	}
	secret := make(map[string]struct{}, len(SecretPaths))
	for _, k := range SecretPaths {
		secret[k] = struct{}{}
	}
	for k, v := range settings {
		if _, isSecret := secret[k]; isSecret {
			continue
		}
		out[k] = cloneValue(v)
	}
	return out
}

// cloneValue deep-copies the JSON-shaped values a settings object holds (maps,
// slices and scalars). Scalars and any other reference types are returned as
// is; settings are decoded JSON, so only map[string]any and []any need to be
// walked.
func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = cloneValue(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = cloneValue(vv)
		}
		return s
	default:
		return v
	}
}
