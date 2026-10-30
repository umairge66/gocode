package envsec

import "github.com/umairge66/gocode/internal/redact"

// This file wires the SecretScanner interface (see deferred.go) to its
// concrete implementation: the free-text secret scanner lives in the sibling
// internal/redact package, and envsec delegates to it.

// RedactScanner is the concrete SecretScanner backed by internal/redact. It is
// stateless, so the zero value is ready to use and safe for concurrent use.
type RedactScanner struct{}

// compile-time assertion that RedactScanner satisfies SecretScanner.
var _ SecretScanner = RedactScanner{}

// DefaultScanner is the package-level scanner callers can share.
var DefaultScanner RedactScanner

// ContainsSecret reports whether text appears to contain a credential,
// delegating to redact.ContainsSecret.
func (RedactScanner) ContainsSecret(text string) bool {
	return redact.ContainsSecret(text)
}

// ContainsSecret is a package-level convenience wrapper over DefaultScanner.
func ContainsSecret(text string) bool { return DefaultScanner.ContainsSecret(text) }
