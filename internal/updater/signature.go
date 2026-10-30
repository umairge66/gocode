package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/umairge66/gocode/internal/updater/releasesig"
)

// ReleasePublicKeys lists the trusted release-signing public keys, compiled in
// at build time:
//
//	go build -ldflags "-X github.com/umairge66/gocode/internal/updater.ReleasePublicKeys=RWQ…" ./cmd/gocode
//
// Each key is the base64 minisign-format public key printed by
// `go run ./cmd/gocode-release keygen` (the key id is embedded in it).
// Several keys, separated by commas, are all trusted — list the old and the
// new key together while rotating. Empty (the default, dev builds) means
// update/install refuse to run, because nothing could be verified.
var ReleasePublicKeys = ""

// SignatureAssetName is the release asset holding the signature over
// checksums.txt.
const SignatureAssetName = ChecksumsAssetName + ".sig"

// maxSignatureBytes bounds checksums.txt.sig.
const maxSignatureBytes = 4 << 10

// ErrNoSigningKey reports a build without any compiled-in release key.
var ErrNoSigningKey = errors.New("This build has no release signing key, so updates can't be verified. " +
	"Build with -ldflags \"-X github.com/umairge66/gocode/internal/updater.ReleasePublicKeys=<public key>\" " +
	"(see scripts/RELEASE-SIGNING.md) or download the release manually.")

// CompiledKeys parses ReleasePublicKeys. It returns ErrNoSigningKey when no
// key was compiled in.
func CompiledKeys() ([]releasesig.PublicKey, error) {
	keys, err := releasesig.ParsePublicKeys(ReleasePublicKeys)
	if err != nil {
		return nil, fmt.Errorf("updater: compiled-in ReleasePublicKeys is invalid (%v); refusing to update", err)
	}
	if len(keys) == 0 {
		return nil, ErrNoSigningKey
	}
	return keys, nil
}

// SignatureError reports a missing or invalid release signature.
type SignatureError struct {
	Release string
	Reason  string
	Err     error
}

func (e *SignatureError) Error() string {
	return fmt.Sprintf("updater: release %s: %s; refusing to install", e.Release, e.Reason)
}

func (e *SignatureError) Unwrap() error { return e.Err }

// trustedKeys returns the Updater's keys (tests) or the compiled-in ones.
func (u *Updater) trustedKeys() ([]releasesig.PublicKey, error) {
	if u.TrustedKeys != nil {
		if len(u.TrustedKeys) == 0 {
			return nil, ErrNoSigningKey
		}
		return u.TrustedKeys, nil
	}
	return CompiledKeys()
}

// fetchVerifiedChecksums downloads checksums.txt and checksums.txt.sig,
// verifies the signature against keys and only then parses the checksums.
// When the signed trusted comment names a file or version, they must match
// checksums.txt and the release being installed (blocks replaying an older
// signed manifest under a newer tag).
func (c *Client) fetchVerifiedChecksums(ctx context.Context, rel *Release, stagingDir string, keys []releasesig.PublicKey) (map[string]string, releasesig.PublicKey, error) {
	sumsAsset, ok := findAsset(rel.Assets, ChecksumsAssetName)
	if !ok {
		return nil, releasesig.PublicKey{}, fmt.Errorf("updater: release %s has no %s asset; refusing to install unverified binaries", rel.TagName, ChecksumsAssetName)
	}
	sigAsset, ok := findAsset(rel.Assets, SignatureAssetName)
	if !ok {
		return nil, releasesig.PublicKey{}, &SignatureError{Release: rel.TagName, Reason: "no " + SignatureAssetName + " asset (the release is not signed)"}
	}
	sums, err := c.fetchSmall(ctx, sumsAsset, stagingDir, maxChecksumsBytes)
	if err != nil {
		return nil, releasesig.PublicKey{}, err
	}
	sig, err := c.fetchSmall(ctx, sigAsset, stagingDir, maxSignatureBytes)
	if err != nil {
		return nil, releasesig.PublicKey{}, err
	}
	s, key, err := releasesig.Verify(keys, sums, sig)
	if err != nil {
		return nil, releasesig.PublicKey{}, &SignatureError{Release: rel.TagName, Reason: "invalid " + SignatureAssetName + ": " + err.Error(), Err: err}
	}
	if f, ok := releasesig.TrustedField(s.TrustedComment, "file"); ok && f != ChecksumsAssetName {
		return nil, releasesig.PublicKey{}, &SignatureError{Release: rel.TagName, Reason: fmt.Sprintf("signature is for %q, not %s", f, ChecksumsAssetName)}
	}
	if v, ok := releasesig.TrustedField(s.TrustedComment, "version"); ok {
		want, _ := rel.Version()
		got, gok := ParseVersion(v)
		if !gok || got.Normalized() != want {
			return nil, releasesig.PublicKey{}, &SignatureError{Release: rel.TagName, Reason: fmt.Sprintf("signed manifest is for version %q, not %s", v, want)}
		}
	}
	parsed, err := ParseChecksums(bytes.NewReader(sums))
	if err != nil {
		return nil, releasesig.PublicKey{}, err
	}
	return parsed, key, nil
}

// fetchSmall downloads a small asset into stagingDir and returns its bytes.
func (c *Client) fetchSmall(ctx context.Context, asset Asset, stagingDir string, limit int64) ([]byte, error) {
	if asset.Size > limit {
		return nil, fmt.Errorf("updater: %s is implausibly large (%d bytes)", asset.Name, asset.Size)
	}
	p := filepath.Join(stagingDir, filepath.Base(asset.Name))
	if _, err := c.downloadAsset(ctx, asset, p); err != nil {
		return nil, err
	}
	defer os.Remove(p)
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("updater: %s is implausibly large", asset.Name)
	}
	return b, nil
}
