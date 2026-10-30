package chromehost

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// ExtensionIDFromKey computes the Chrome extension id derived from a manifest
// "key" field. The key is the base64-encoded DER of the extension's public key
// in X.509 SubjectPublicKeyInfo form; Chrome derives the (stable) id from it so
// an unpacked extension keeps the same id across loads.
//
// Algorithm (the same one Chrome uses): SHA-256 the DER public-key bytes, take
// the first 16 bytes, hex-encode them (32 hex chars), then map each hex nibble
// 0..f to the letters a..p (so '0'->'a', '9'->'j', 'a'->'k', 'f'->'p'). The
// result is a 32-character id in [a-p].
func ExtensionIDFromKey(keyB64 string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return "", fmt.Errorf("chromehost: decode manifest key: %w", err)
	}
	if len(der) == 0 {
		return "", fmt.Errorf("chromehost: manifest key is empty")
	}
	return ExtensionIDFromDER(der), nil
}

// ExtensionIDFromDER is ExtensionIDFromKey for already-decoded DER bytes.
func ExtensionIDFromDER(der []byte) string {
	sum := sha256.Sum256(der)
	hexStr := hex.EncodeToString(sum[:16])
	out := make([]byte, len(hexStr))
	for i := 0; i < len(hexStr); i++ {
		c := hexStr[i]
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		default: // 'a'..'f'
			v = c - 'a' + 10
		}
		out[i] = 'a' + v
	}
	return string(out)
}

// ValidExtensionID reports whether id has the shape of a Chrome extension id:
// exactly 32 characters in [a-p]. It guards the id pinned into a
// native-messaging host manifest's allowed_origins.
func ValidExtensionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 'a' || id[i] > 'p' {
			return false
		}
	}
	return true
}
