package envsec

import (
	"strings"
)

// buildSet returns a set from a slice.
func buildSet(names []string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

var (
	credentialNameSet = buildSet(credentialEnvNames)
)

// IsCredentialEnvVar reports whether a variable name is known to carry a
// secret/credential value. The name is matched exactly against the credential
// table (both the bare and INPUT_-prefixed spellings are recognized).
func IsCredentialEnvVar(name string) bool {
	if _, ok := credentialNameSet[name]; ok {
		return true
	}
	if rest, ok := strings.CutPrefix(name, "INPUT_"); ok {
		_, ok := credentialNameSet[rest]
		return ok
	}
	return false
}
