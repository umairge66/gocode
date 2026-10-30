// Package ids generates and validates identifiers: random and name-based
// (v5) UUIDs, agent ids, safe-name tokens, and the default session id.
package ids

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"regexp"
)

// -----------------------------------------------------------------------------
// Name validators and UUID utilities.
// -----------------------------------------------------------------------------

// RandomUUID returns a random (version 4) UUID string, like Node's
// crypto.randomUUID.
func RandomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return formatUUID(b[:])
}

// UUIDv5 returns the RFC 4122 version-5 (name-based, SHA-1) UUID of name within
// the given namespace UUID. The namespace may contain hyphens.
func UUIDv5(name, namespace string) string {
	nsClean := regexp.MustCompile(`-`).ReplaceAllString(namespace, "")
	nsBytes, err := hex.DecodeString(nsClean)
	if err != nil {
		// Invalid hex input is a programming error.
		panic(err)
	}
	h := sha1.New()
	h.Write(nsBytes)
	h.Write([]byte(name))
	sum := h.Sum(nil)[:16]
	sum[6] = sum[6]&0x0f | 0x50 // version 5
	sum[8] = sum[8]&0x3f | 0x80 // RFC 4122 variant
	return formatUUID(sum)
}

// uuidRe matches the canonical 8-4-4-4-12 UUID textual form (any version),
// case-insensitive.
var uuidRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidateUUID returns (t, true) if t is a canonical UUID string, or
// ("", false) otherwise.
func ValidateUUID(t string) (string, bool) {
	if uuidRe.MatchString(t) {
		return t, true
	}
	return "", false
}

// formatUUID renders 16 bytes as a canonical 8-4-4-4-12 UUID string.
func formatUUID(b []byte) string {
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
