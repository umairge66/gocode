// Package releasesig signs and verifies gocode release manifests
// (checksums.txt) with Ed25519, in the minisign file format.
//
// Formats (all minisign-compatible, see https://jedisct1.github.io/minisign/):
//
//   - Public key: base64 of "Ed" || key id (8 bytes) || Ed25519 public key
//     (32 bytes) — 56 base64 characters. This single string is what gets
//     compiled into gocode; the key id travels inside it. As a file it is
//     preceded by an "untrusted comment:" line.
//   - Signature (checksums.txt.sig): four lines —
//     "untrusted comment: …", base64("ED" || key id || Ed25519 signature of
//     BLAKE2b-512(file)), "trusted comment: …", base64(Ed25519 signature of
//     (signature || trusted comment)). The trusted comment is authenticated;
//     the untrusted one is not. Legacy "Ed" signatures (Ed25519 directly over
//     the file) are also accepted.
//   - Secret key: minisign's secret-key file with key derivation disabled
//     (as written by `minisign -G -W`): it is NOT password-encrypted, so keep
//     the file offline or in a CI secret.
//
// Because the formats match, `minisign -S -s <key> -m checksums.txt -x
// checksums.txt.sig` and `minisign -V -p <key>.pub -m checksums.txt -x
// checksums.txt.sig` interoperate with gocode-release's output.
package releasesig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	algEd     = [2]byte{'E', 'd'} // key algorithm; legacy (non-prehashed) signatures
	algHashed = [2]byte{'E', 'D'} // prehashed signatures (BLAKE2b-512)
	chkAlgB2  = [2]byte{'B', '2'}
)

const (
	publicKeyLen  = 2 + 8 + ed25519.PublicKeySize                            // 42
	sigLen        = 2 + 8 + ed25519.SignatureSize                            // 74
	secretKeyLen  = 2 + 2 + 2 + 32 + 8 + 8 + 8 + ed25519.PrivateKeySize + 32 // 158
	commentPrefix = "untrusted comment: "
	trustedPrefix = "trusted comment: "
)

// ErrUntrustedKey reports a signature made by a key that is not trusted.
var ErrUntrustedKey = errors.New("signature was made by a key that is not trusted")

// ErrBadSignature reports a signature that does not verify.
var ErrBadSignature = errors.New("signature verification failed")

// KeyID identifies a key; it is embedded in public keys and signatures.
type KeyID [8]byte

// String renders the id the way minisign prints it (16 upper-case hex digits).
func (k KeyID) String() string {
	return fmt.Sprintf("%016X", binary.LittleEndian.Uint64(k[:]))
}

// PublicKey is a trusted release-signing public key.
type PublicKey struct {
	ID  KeyID
	Key ed25519.PublicKey
}

// String returns the base64 public key (the value to compile in).
func (p PublicKey) String() string {
	b := make([]byte, 0, publicKeyLen)
	b = append(b, algEd[:]...)
	b = append(b, p.ID[:]...)
	b = append(b, p.Key...)
	return base64.StdEncoding.EncodeToString(b)
}

// File returns the public key as a minisign .pub file.
func (p PublicKey) File() []byte {
	return []byte(commentPrefix + "minisign public key " + p.ID.String() + "\n" + p.String() + "\n")
}

// ParsePublicKey parses a base64 public key, or the contents of a minisign
// .pub file (the comment line is skipped).
func ParsePublicKey(s string) (PublicKey, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, commentPrefix) {
		_, rest, _ := strings.Cut(s, "\n")
		s = strings.TrimSpace(rest)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != publicKeyLen {
		return PublicKey{}, fmt.Errorf("invalid public key %q: want base64 of %d bytes", truncate(s), publicKeyLen)
	}
	if [2]byte(raw[:2]) != algEd {
		return PublicKey{}, fmt.Errorf("invalid public key %q: unsupported algorithm %q", truncate(s), raw[:2])
	}
	var p PublicKey
	copy(p.ID[:], raw[2:10])
	p.Key = ed25519.PublicKey(bytes.Clone(raw[10:]))
	return p, nil
}

// ParsePublicKeys parses a list of base64 public keys separated by commas
// and/or whitespace. An empty list yields no keys and no error.
func ParsePublicKeys(list string) ([]PublicKey, error) {
	fields := strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	var out []PublicKey
	for i, f := range fields {
		k, err := ParsePublicKey(f)
		if err != nil {
			return nil, fmt.Errorf("key #%d: %w", i+1, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// SecretKey is a release-signing private key.
type SecretKey struct {
	ID  KeyID
	Key ed25519.PrivateKey
}

// GenerateKey creates a new key pair with a random key id. rnd nil means
// crypto/rand.
func GenerateKey(rnd io.Reader) (SecretKey, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	var sk SecretKey
	if _, err := io.ReadFull(rnd, sk.ID[:]); err != nil {
		return SecretKey{}, err
	}
	_, priv, err := ed25519.GenerateKey(rnd)
	if err != nil {
		return SecretKey{}, err
	}
	sk.Key = priv
	return sk, nil
}

// Public returns the matching public key.
func (s SecretKey) Public() PublicKey {
	return PublicKey{ID: s.ID, Key: bytes.Clone(s.Key.Public().(ed25519.PublicKey))}
}

func secretChecksum(id KeyID, key []byte) []byte {
	return blake2bSum(32, algEd[:], id[:], key)
}

// File encodes the key as an unencrypted minisign secret-key file.
func (s SecretKey) File() []byte {
	b := make([]byte, 0, secretKeyLen)
	b = append(b, algEd[:]...)
	b = append(b, 0, 0) // kdf algorithm: none (unencrypted)
	b = append(b, chkAlgB2[:]...)
	b = append(b, make([]byte, 32+8+8)...) // kdf salt, opslimit, memlimit (unused)
	b = append(b, s.ID[:]...)
	b = append(b, s.Key...)
	b = append(b, secretChecksum(s.ID, s.Key)...)
	return []byte(commentPrefix + "gocode release signing secret key " + s.ID.String() +
		" (unencrypted minisign format; keep it secret)\n" + base64.StdEncoding.EncodeToString(b) + "\n")
}

// ParseSecretKey decodes an unencrypted minisign secret-key file.
func ParseSecretKey(data []byte) (SecretKey, error) {
	lines := splitLines(data)
	if len(lines) < 2 || !strings.HasPrefix(lines[0], commentPrefix) {
		return SecretKey{}, errors.New("invalid secret key file: expected an untrusted comment line followed by the key")
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) != secretKeyLen {
		return SecretKey{}, errors.New("invalid secret key file: malformed key data")
	}
	if [2]byte(raw[0:2]) != algEd || [2]byte(raw[4:6]) != chkAlgB2 {
		return SecretKey{}, errors.New("invalid secret key file: unsupported algorithm")
	}
	if raw[2] != 0 || raw[3] != 0 {
		return SecretKey{}, errors.New("secret key is password-encrypted (minisign default); sign with `minisign -S` instead, or create an unencrypted key with gocode-release keygen")
	}
	var sk SecretKey
	copy(sk.ID[:], raw[54:62])
	sk.Key = ed25519.PrivateKey(bytes.Clone(raw[62 : 62+ed25519.PrivateKeySize]))
	if !bytes.Equal(secretChecksum(sk.ID, sk.Key), raw[62+ed25519.PrivateKeySize:]) {
		return SecretKey{}, errors.New("invalid secret key file: checksum mismatch (corrupted?)")
	}
	// The embedded public half must match the seed.
	if !bytes.Equal(ed25519.NewKeyFromSeed(sk.Key.Seed()), sk.Key) {
		return SecretKey{}, errors.New("invalid secret key file: inconsistent key material")
	}
	return sk, nil
}

// Signature is a parsed signature file.
type Signature struct {
	Algorithm        [2]byte
	KeyID            KeyID
	Sig              []byte
	UntrustedComment string
	TrustedComment   string
	GlobalSig        []byte
}

// Sign produces a prehashed ("ED") minisign signature file for msg. The
// trusted comment is authenticated together with the signature; it must not
// contain a newline.
func Sign(sk SecretKey, msg []byte, trustedComment string) ([]byte, error) {
	if strings.ContainsAny(trustedComment, "\r\n") {
		return nil, errors.New("trusted comment must be a single line")
	}
	sig := ed25519.Sign(sk.Key, blake2bSum(64, msg))
	global := ed25519.Sign(sk.Key, append(bytes.Clone(sig), trustedComment...))
	b := make([]byte, 0, sigLen)
	b = append(b, algHashed[:]...)
	b = append(b, sk.ID[:]...)
	b = append(b, sig...)
	var out bytes.Buffer
	out.WriteString(commentPrefix + "signature from gocode-release secret key " + sk.ID.String() + "\n")
	out.WriteString(base64.StdEncoding.EncodeToString(b) + "\n")
	out.WriteString(trustedPrefix + trustedComment + "\n")
	out.WriteString(base64.StdEncoding.EncodeToString(global) + "\n")
	return out.Bytes(), nil
}

// ParseSignature parses a minisign signature file.
func ParseSignature(data []byte) (Signature, error) {
	lines := splitLines(data)
	if len(lines) < 4 {
		return Signature{}, errors.New("malformed signature file: expected 4 lines")
	}
	if !strings.HasPrefix(lines[0], commentPrefix) || !strings.HasPrefix(lines[2], trustedPrefix) {
		return Signature{}, errors.New("malformed signature file: missing comment lines")
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) != sigLen {
		return Signature{}, errors.New("malformed signature file: bad signature encoding")
	}
	alg := [2]byte(raw[:2])
	if alg != algEd && alg != algHashed {
		return Signature{}, fmt.Errorf("malformed signature file: unsupported algorithm %q", raw[:2])
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != ed25519.SignatureSize {
		return Signature{}, errors.New("malformed signature file: bad global signature encoding")
	}
	s := Signature{
		Algorithm:        alg,
		Sig:              raw[10:],
		UntrustedComment: strings.TrimPrefix(lines[0], commentPrefix),
		TrustedComment:   strings.TrimPrefix(lines[2], trustedPrefix),
		GlobalSig:        global,
	}
	copy(s.KeyID[:], raw[2:10])
	return s, nil
}

// Verify checks sigFile over msg against the trusted keys and returns the
// parsed signature and the key that verified it. Both the file signature and
// the trusted-comment signature must verify.
func Verify(trusted []PublicKey, msg, sigFile []byte) (Signature, PublicKey, error) {
	s, err := ParseSignature(sigFile)
	if err != nil {
		return Signature{}, PublicKey{}, err
	}
	var key *PublicKey
	for i := range trusted {
		if trusted[i].ID == s.KeyID {
			key = &trusted[i]
			break
		}
	}
	if key == nil {
		ids := make([]string, len(trusted))
		for i, k := range trusted {
			ids[i] = k.ID.String()
		}
		return s, PublicKey{}, fmt.Errorf("%w: signed by key %s, trusted keys: %s", ErrUntrustedKey, s.KeyID, strings.Join(ids, ", "))
	}
	signed := msg
	if s.Algorithm == algHashed {
		signed = blake2bSum(64, msg)
	}
	if !ed25519.Verify(key.Key, signed, s.Sig) {
		return s, *key, fmt.Errorf("%w (key %s): the file does not match its signature", ErrBadSignature, key.ID)
	}
	if !ed25519.Verify(key.Key, append(bytes.Clone(s.Sig), s.TrustedComment...), s.GlobalSig) {
		return s, *key, fmt.Errorf("%w (key %s): the trusted comment was altered", ErrBadSignature, key.ID)
	}
	return s, *key, nil
}

// TrustedField returns the value of a "name:value" field in a tab-separated
// trusted comment (minisign style: "timestamp:…\tfile:…\thashed").
func TrustedField(trustedComment, name string) (string, bool) {
	for _, f := range strings.Split(trustedComment, "\t") {
		if k, v, ok := strings.Cut(f, ":"); ok && k == name {
			return v, true
		}
	}
	return "", false
}

func splitLines(data []byte) []string {
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func truncate(s string) string {
	if len(s) > 20 {
		return s[:20] + "…"
	}
	return s
}
