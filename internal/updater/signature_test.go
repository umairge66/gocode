package updater

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/updater/releasesig"
)

// publishWithSig registers a release whose checksums.txt is sums and whose
// checksums.txt.sig is sig (omitted when nil).
func (f *fakeGitHub) publishWithSig(tag string, bin, sums, sig []byte) *Release {
	a := f.asset("gocode_linux_amd64", bin)
	c := f.asset("checksums.txt", sums)
	assets := []Asset{c, a}
	if sig != nil {
		assets = append(assets, f.asset("checksums.txt.sig", sig))
	}
	rel := &Release{TagName: tag, Assets: assets}
	f.latest = rel
	f.tags[tag] = rel
	return rel
}

func mustKey(t *testing.T) releasesig.SecretKey {
	t.Helper()
	k, err := releasesig.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustSign(t *testing.T, k releasesig.SecretKey, msg []byte, tc string) []byte {
	t.Helper()
	s, err := releasesig.Sign(k, msg, tc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const goodTC = "timestamp:1\tfile:checksums.txt\thashed"

func applyLatest(t *testing.T, u *Updater) error {
	t.Helper()
	res, err := u.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.Apply(context.Background(), res.Release)
	return err
}

func assertNotInstalled(t *testing.T, u *Updater, gh *fakeGitHub, restarted *string) {
	t.Helper()
	if got, _ := os.ReadFile(u.Executable); string(got) != "OLD" || *restarted != "" {
		t.Fatalf("executable replaced (%q) or restarted (%q)", got, *restarted)
	}
	if slices.Contains(gh.downloads(), "gocode_linux_amd64") {
		t.Fatal("platform binary downloaded before the signature was verified")
	}
}

func TestSignedReleaseInstalls(t *testing.T) {
	gh := newFakeGitHub(t)
	bin := []byte("NEW SIGNED BINARY")
	sums := checksumsFor(map[string][]byte{"gocode_linux_amd64": bin})
	gh.publishWithSig("v1.3.0", bin, sums, mustSign(t, gh.sk, sums, goodTC+"\tversion:1.3.0"))
	u, _ := newTestUpdater(t, gh, "1.2.0")
	if err := applyLatest(t, u); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(u.Executable); string(got) != string(bin) {
		t.Fatalf("executable = %q", got)
	}
	if u.VerifiedKey.ID != gh.sk.ID {
		t.Fatalf("VerifiedKey = %s, want %s", u.VerifiedKey.ID, gh.sk.ID)
	}
}

func TestSignatureRefusals(t *testing.T) {
	bin := []byte("NEW")
	other := []byte("EVIL")
	cases := []struct {
		name  string
		setup func(t *testing.T, gh *fakeGitHub, u *Updater)
		check func(t *testing.T, err error)
	}{
		{"tampered checksums.txt", func(t *testing.T, gh *fakeGitHub, u *Updater) {
			good := checksumsFor(map[string][]byte{"gocode_linux_amd64": bin})
			evil := checksumsFor(map[string][]byte{"gocode_linux_amd64": other})
			gh.publishWithSig("v1.3.0", other, evil, mustSign(t, gh.sk, good, goodTC))
		}, func(t *testing.T, err error) {
			if !errors.Is(err, releasesig.ErrBadSignature) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"wrong key", func(t *testing.T, gh *fakeGitHub, u *Updater) {
			sums := checksumsFor(map[string][]byte{"gocode_linux_amd64": bin})
			gh.publishWithSig("v1.3.0", bin, sums, mustSign(t, mustKey(t), sums, goodTC))
		}, func(t *testing.T, err error) {
			if !errors.Is(err, releasesig.ErrUntrustedKey) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"missing sig", func(t *testing.T, gh *fakeGitHub, u *Updater) {
			gh.publishWithSig("v1.3.0", bin, checksumsFor(map[string][]byte{"gocode_linux_amd64": bin}), nil)
		}, func(t *testing.T, err error) {
			var se *SignatureError
			if !errors.As(err, &se) || !strings.Contains(err.Error(), "checksums.txt.sig") {
				t.Fatalf("err = %v", err)
			}
		}},
		{"garbage sig", func(t *testing.T, gh *fakeGitHub, u *Updater) {
			gh.publishWithSig("v1.3.0", bin, checksumsFor(map[string][]byte{"gocode_linux_amd64": bin}), []byte("not a signature\n"))
		}, func(t *testing.T, err error) {
			var se *SignatureError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"replayed older manifest", func(t *testing.T, gh *fakeGitHub, u *Updater) {
			sums := checksumsFor(map[string][]byte{"gocode_linux_amd64": bin})
			gh.publishWithSig("v1.3.0", bin, sums, mustSign(t, gh.sk, sums, goodTC+"\tversion:1.2.5"))
		}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "version") {
				t.Fatalf("err = %v", err)
			}
		}},
		{"no key compiled in", func(t *testing.T, gh *fakeGitHub, u *Updater) {
			sums := checksumsFor(map[string][]byte{"gocode_linux_amd64": bin})
			gh.publishWithSig("v1.3.0", bin, sums, mustSign(t, gh.sk, sums, goodTC))
			u.TrustedKeys = nil // fall back to ReleasePublicKeys, empty in this build
		}, func(t *testing.T, err error) {
			if !errors.Is(err, ErrNoSigningKey) || !strings.Contains(err.Error(), "-ldflags") {
				t.Fatalf("err = %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ReleasePublicKeys != "" {
				t.Fatal("test build unexpectedly has ReleasePublicKeys set")
			}
			gh := newFakeGitHub(t)
			u, restarted := newTestUpdater(t, gh, "1.2.0")
			tc.setup(t, gh, u)
			err := applyLatest(t, u)
			tc.check(t, err)
			assertNotInstalled(t, u, gh, restarted)
		})
	}
}

func TestNoKeyMakesNoDownloads(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.publish("v1.3.0", "gocode_linux_amd64", []byte("NEW"), nil)
	u, _ := newTestUpdater(t, gh, "1.2.0")
	u.TrustedKeys = []releasesig.PublicKey{} // explicitly none
	if err := applyLatest(t, u); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("err = %v", err)
	}
	if d := gh.downloads(); len(d) != 0 {
		t.Fatalf("downloads without a key: %v", d)
	}
}

// Compiled-in keys (the -ldflags path) with rotation: both the old and the
// new key are accepted while both are listed.
func TestCompiledKeysRotation(t *testing.T) {
	oldKey, newKey := mustKey(t), mustKey(t)
	saved := ReleasePublicKeys
	t.Cleanup(func() { ReleasePublicKeys = saved })
	ReleasePublicKeys = oldKey.Public().String() + "," + newKey.Public().String()

	keys, err := CompiledKeys()
	if err != nil || len(keys) != 2 {
		t.Fatalf("CompiledKeys = %v, %v", keys, err)
	}
	for _, signer := range []releasesig.SecretKey{newKey, oldKey} {
		gh := newFakeGitHub(t)
		bin := []byte("BIN " + signer.ID.String())
		sums := checksumsFor(map[string][]byte{"gocode_linux_amd64": bin})
		gh.publishWithSig("v1.3.0", bin, sums, mustSign(t, signer, sums, goodTC))
		u, _ := newTestUpdater(t, gh, "1.2.0")
		u.TrustedKeys = nil // use ReleasePublicKeys
		if err := applyLatest(t, u); err != nil {
			t.Fatalf("signer %s: %v", signer.ID, err)
		}
		if u.VerifiedKey.ID != signer.ID {
			t.Fatalf("verified by %s, want %s", u.VerifiedKey.ID, signer.ID)
		}
	}

	// After rotation completes (old key dropped), old signatures are refused.
	ReleasePublicKeys = newKey.Public().String()
	gh := newFakeGitHub(t)
	sums := checksumsFor(map[string][]byte{"gocode_linux_amd64": []byte("X")})
	gh.publishWithSig("v1.3.0", []byte("X"), sums, mustSign(t, oldKey, sums, goodTC))
	u, restarted := newTestUpdater(t, gh, "1.2.0")
	u.TrustedKeys = nil
	if err := applyLatest(t, u); !errors.Is(err, releasesig.ErrUntrustedKey) {
		t.Fatalf("err = %v", err)
	}
	assertNotInstalled(t, u, gh, restarted)

	ReleasePublicKeys = "not-a-key"
	if _, err := CompiledKeys(); err == nil || errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("malformed key list: err = %v", err)
	}
}
