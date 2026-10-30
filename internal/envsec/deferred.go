package envsec

// This file declares interfaces for collaborators implemented outside this
// package.

// SecretScanner recognizes credential material embedded in free-form text
// (e.g. `user:pass@host` URLs, `Authorization:` headers, inline tokens, Slack/
// Discord/Teams webhook URLs, sonar.login=... assignments).
//
// The detection logic needs regex look-ahead/look-behind, which Go's RE2
// engine does not support, so it is implemented with hand-written scanners in
// the sibling internal/redact package. This interface is satisfied by RedactScanner in
// scanner.go.
type SecretScanner interface {
	// ContainsSecret reports whether text appears to contain a credential.
	ContainsSecret(text string) bool
}

// SettingsStore models loading, merging
// and validating the layered settings sources (user / project / local / policy
// / flag), including per-OS managed-settings "policy helpers" and remote-
// settings verification.
//
// The concrete implementation is internal/settingsload.Store (file-backed
// user/project/local/policy/flag tiers); MDM and remote server-managed
// settings are not supported.
type SettingsStore interface {
	// RawKeyPresence reports "present", "absent" or "unknowable" for a settings
	// key across the configured sources.
	RawKeyPresence(key string) (string, error)
}
