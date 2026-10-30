package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/updater/releasesig"
)

func runCmd(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb, func(k string) string { return env[k] }, func() time.Time { return time.Unix(1700000000, 0) })
	return code, out.String(), errb.String()
}

func TestKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "keys", "release.key")

	code, out, errs := runCmd(t, nil, "keygen", "-out", keyPath)
	if code != 0 {
		t.Fatalf("keygen exit %d: %s", code, errs)
	}
	m := regexp.MustCompile(`Public key: (RW\S+)`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "-X github.com/umairge66/gocode/internal/updater.ReleasePublicKeys="+m[1]) {
		t.Fatalf("keygen output lacks public key / ldflags snippet:\n%s", out)
	}
	pub, err := releasesig.ParsePublicKey(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("private key mode = %v, %v", fi.Mode(), err)
		}
	}
	pubFile, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := releasesig.ParsePublicKey(string(pubFile)); err != nil || p.String() != pub.String() {
		t.Fatalf(".pub file = %v, %v", p, err)
	}

	// keygen never overwrites.
	before, _ := os.ReadFile(keyPath)
	if code, _, errs := runCmd(t, nil, "keygen", "-out", keyPath); code == 0 || !strings.Contains(errs, "refusing to overwrite") {
		t.Fatalf("keygen overwrite: exit %d %s", code, errs)
	}
	if after, _ := os.ReadFile(keyPath); !bytes.Equal(before, after) {
		t.Fatal("private key changed")
	}

	sums := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sums, []byte(strings.Repeat("a", 64)+"  gocode_linux_amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// sign without any key source fails.
	if code, _, errs := runCmd(t, nil, "sign", sums); code == 0 || !strings.Contains(errs, "GOCODE_RELEASE_KEY_FILE") {
		t.Fatalf("sign without key: exit %d %s", code, errs)
	}
	// sign with the key path from the environment.
	if code, _, errs := runCmd(t, map[string]string{"GOCODE_RELEASE_KEY_FILE": keyPath}, "sign", "-version", "v1.2.3", sums); code != 0 {
		t.Fatalf("sign exit %d: %s", code, errs)
	}
	sig, err := os.ReadFile(sums + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := os.ReadFile(sums)
	s, _, err := releasesig.Verify([]releasesig.PublicKey{pub}, msg, sig)
	if err != nil {
		t.Fatal(err)
	}
	if want := "timestamp:1700000000\tfile:checksums.txt\thashed\tversion:1.2.3"; s.TrustedComment != want {
		t.Fatalf("trusted comment = %q, want %q", s.TrustedComment, want)
	}
	// Existing signature is not overwritten without -force; -key flag works.
	if code, _, errs := runCmd(t, nil, "sign", "-key", keyPath, sums); code == 0 || !strings.Contains(errs, "already exists") {
		t.Fatalf("sign overwrite: exit %d %s", code, errs)
	}
	if code, _, errs := runCmd(t, nil, "sign", "-key", keyPath, "-force", sums); code != 0 {
		t.Fatalf("sign -force: exit %d %s", code, errs)
	}

	// verify: by key string, by .pub file, and with a second (rotation) key.
	other, _ := releasesig.GenerateKey(nil)
	if code, out, errs := runCmd(t, nil, "verify", "-pubkey", other.Public().String()+","+pub.String(), sums); code != 0 || !strings.Contains(out, "Signature OK") {
		t.Fatalf("verify: exit %d %s %s", code, out, errs)
	}
	if code, _, errs := runCmd(t, nil, "verify", "-pubkey-file", keyPath+".pub", sums, sums+".sig"); code != 0 {
		t.Fatalf("verify -pubkey-file: exit %d %s", code, errs)
	}
	if code, _, _ := runCmd(t, nil, "verify", "-pubkey", other.Public().String(), sums); code == 0 {
		t.Fatal("verify with the wrong key succeeded")
	}
	if code, _, _ := runCmd(t, nil, "verify", sums); code == 0 {
		t.Fatal("verify without a key succeeded")
	}
	if err := os.WriteFile(sums, []byte(strings.Repeat("b", 64)+"  gocode_linux_amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runCmd(t, nil, "verify", "-pubkey", pub.String(), sums); code == 0 || !strings.Contains(errs, "verification failed") {
		t.Fatalf("tampered file verified: exit %d %s", code, errs)
	}
}

func TestUsage(t *testing.T) {
	if code, _, _ := runCmd(t, nil); code != 2 {
		t.Fatalf("no args exit %d", code)
	}
	if code, _, _ := runCmd(t, nil, "bogus"); code != 2 {
		t.Fatalf("bogus exit %d", code)
	}
	if code, _, _ := runCmd(t, nil, "keygen"); code == 0 {
		t.Fatal("keygen without -out succeeded")
	}
}
