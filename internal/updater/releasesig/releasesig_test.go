package releasesig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestBlake2bVectors(t *testing.T) {
	seq := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}
	cases := []struct {
		in   []byte
		out  int
		want string
	}{
		{nil, 64, "786a02f742015903c6c6fd852552d272912f4740e15847618a86e217f71f5419d25e1031afee585313896444934eb04b903a685b1448b755d56f701afe9be2ce"},
		{[]byte("abc"), 64, "ba80a53f981c4d0d6a2797b69f12f6e94c212f14685ac4b74b12bb6fdbffa2d17d87c5392aab792dc252d5de4533cc9518d38aa8dbf1925ab92386edd4009923"},
		{[]byte("abc"), 32, "bddd813c634239723171ef3fee98579b94964e3bb1cb3e427262c8c068d52319"},
		{seq(128), 64, "2319e3789c47e2daa5fe807f61bec2a1a6537fa03f19ff32e87eecbfd64b7e0e8ccff439ac333b040f19b0c4ddd11a61e24ac1fe0f10a039806c5dcc0da3d115"},
		{seq(129), 64, "f59711d44a031d5f97a9413c065d1e614c417ede998590325f49bad2fd444d3e4418be19aec4e11449ac1a57207898bc57d76a1bcf3566292c20c683a5c4648f"},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(blake2bSum(c.out, c.in)); got != c.want {
			t.Errorf("blake2b-%d(%d bytes) = %s, want %s", c.out*8, len(c.in), got, c.want)
		}
		// Same result when fed byte by byte.
		d := newBlake2b(c.out)
		for i := range c.in {
			_, _ = d.Write(c.in[i : i+1])
		}
		if got := hex.EncodeToString(d.Sum()); got != c.want {
			t.Errorf("chunked blake2b-%d(%d bytes) = %s", c.out*8, len(c.in), got)
		}
	}
	// 768 bytes of 0..255 repeated (multi-block).
	long := bytes.Repeat(seq(256), 3)
	const want = "323e97a7a859ee63c9013debb0ca995811e73117a2f574723416e596ebc184e37a59b66d2f597df4a7c1b0d1d41a1a7f28774f46a6864d56c57b9d6c5f7302fb"
	if got := hex.EncodeToString(blake2bSum(64, long[:100], long[100:])); got != want {
		t.Errorf("blake2b long = %s", got)
	}
}

func newKey(t *testing.T) SecretKey {
	t.Helper()
	k, err := GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSignVerifyRoundTrip(t *testing.T) {
	k := newKey(t)
	msg := []byte("abc123  gocode_linux_amd64\n")
	tc := "timestamp:1\tfile:checksums.txt\thashed\tversion:1.2.3"
	sig, err := Sign(k, msg, tc)
	if err != nil {
		t.Fatal(err)
	}
	s, key, err := Verify([]PublicKey{newKey(t).Public(), k.Public()}, msg, sig)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != k.ID || s.TrustedComment != tc || s.Algorithm != algHashed {
		t.Fatalf("key=%s sig=%+v", key.ID, s)
	}
	if v, ok := TrustedField(s.TrustedComment, "version"); !ok || v != "1.2.3" {
		t.Fatalf("version field = %q %v", v, ok)
	}

	// Tampered message.
	if _, _, err := Verify([]PublicKey{k.Public()}, append(bytes.Clone(msg), 'x'), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered msg: %v", err)
	}
	// Tampered trusted comment.
	forged := bytes.Replace(sig, []byte("version:1.2.3"), []byte("version:9.9.9"), 1)
	if _, _, err := Verify([]PublicKey{k.Public()}, msg, forged); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered comment: %v", err)
	}
	// Untrusted key.
	if _, _, err := Verify([]PublicKey{newKey(t).Public()}, msg, sig); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("wrong key: %v", err)
	}
	// Same key id, different key material (forged id).
	impostor := newKey(t)
	impostor.ID = k.ID
	sig2, _ := Sign(impostor, msg, tc)
	if _, _, err := Verify([]PublicKey{k.Public()}, msg, sig2); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged key id: %v", err)
	}
	// Garbage.
	if _, _, err := Verify([]PublicKey{k.Public()}, msg, []byte("nope")); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := Sign(k, msg, "two\nlines"); err == nil {
		t.Fatal("multi-line trusted comment accepted")
	}
}

// A legacy minisign signature ("Ed": Ed25519 directly over the file) verifies.
func TestLegacySignatureAccepted(t *testing.T) {
	k := newKey(t)
	msg := []byte("hello")
	tc := "timestamp:1\tfile:checksums.txt"
	sig := ed25519.Sign(k.Key, msg)
	global := ed25519.Sign(k.Key, append(bytes.Clone(sig), tc...))
	raw := append(append([]byte("Ed"), k.ID[:]...), sig...)
	file := "untrusted comment: legacy\n" + base64.StdEncoding.EncodeToString(raw) + "\ntrusted comment: " + tc + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n"
	if _, _, err := Verify([]PublicKey{k.Public()}, msg, []byte(file)); err != nil {
		t.Fatal(err)
	}
	// CRLF line endings are tolerated too.
	if _, _, err := Verify([]PublicKey{k.Public()}, msg, []byte(strings.ReplaceAll(file, "\n", "\r\n"))); err != nil {
		t.Fatal(err)
	}
}

func TestKeyFiles(t *testing.T) {
	k := newKey(t)
	got, err := ParseSecretKey(k.File())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != k.ID || !bytes.Equal(got.Key, k.Key) {
		t.Fatal("secret key round trip mismatch")
	}

	// Corrupt one key byte: checksum must catch it.
	lines := strings.Split(string(k.File()), "\n")
	raw, _ := base64.StdEncoding.DecodeString(lines[1])
	raw[70] ^= 1
	bad := lines[0] + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
	if _, err := ParseSecretKey([]byte(bad)); err == nil {
		t.Fatal("corrupted secret key accepted")
	}
	// Encrypted (minisign default, kdf "Sc") is rejected with guidance.
	raw[70] ^= 1
	raw[2], raw[3] = 'S', 'c'
	enc := lines[0] + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
	if _, err := ParseSecretKey([]byte(enc)); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("encrypted key: %v", err)
	}

	pub := k.Public()
	s := pub.String()
	if len(s) != 56 || !strings.HasPrefix(s, "RW") {
		t.Fatalf("public key %q: want 56 chars starting with RW (minisign style)", s)
	}
	for _, in := range []string{s, string(pub.File()), "  " + s + "\n"} {
		p, err := ParsePublicKey(in)
		if err != nil || p.ID != pub.ID || !bytes.Equal(p.Key, pub.Key) {
			t.Fatalf("ParsePublicKey(%q) = %v, %v", in, p, err)
		}
	}
	k2 := newKey(t).Public()
	list, err := ParsePublicKeys(s + ", " + k2.String() + "\n")
	if err != nil || len(list) != 2 || list[1].ID != k2.ID {
		t.Fatalf("ParsePublicKeys = %v, %v", list, err)
	}
	if list, err := ParsePublicKeys(" "); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %v, %v", list, err)
	}
	if _, err := ParsePublicKeys(s + ",bogus"); err == nil || !strings.Contains(err.Error(), "key #2") {
		t.Fatalf("bogus key: %v", err)
	}
	if got := (KeyID{0x01, 0, 0, 0, 0, 0, 0, 0xAB}).String(); got != "AB00000000000001" {
		t.Fatalf("KeyID.String = %s", got)
	}
}
