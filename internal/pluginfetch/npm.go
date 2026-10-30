package pluginfetch

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/updater"
)

// defaultNPMRegistry is the registry a bare npm source resolves against.
const defaultNPMRegistry = "https://registry.npmjs.org"

// npmSRIAlgos is the Subresource-Integrity digest priority used to verify a
// resolved tarball: the strongest algorithm the registry reports wins.
var npmSRIAlgos = []string{"sha512", "sha384", "sha256", "sha1"}

// fetchNPM materializes an npm plugin source. The installer resolves
// the package version against the registry, downloads the resolved tarball
// (verifying its integrity when the entry pins one), and extracts it with no
// install scripts.
//
// Two paths are supported:
//
//   - Direct tarball: an npm source whose Version or Package is already an
//     "https://…/-/pkg-x.y.z.tgz" tarball reference is downloaded, has its
//     SHA256 pin verified when present, and is extracted.
//   - Registry resolution: otherwise the packument at <registry>/<pkg> is
//     fetched through the Doer, the version is resolved from a dist-tag
//     (default "latest") or an explicit version/range, and the resolved
//     dist.tarball is downloaded and verified against dist.integrity (base64
//     SRI) and/or dist.shasum (hex sha1) before extraction.
func (f *Fetcher) fetchNPM(ctx context.Context, src plugins.Source, dest string) (string, error) {
	pkg := strings.TrimSpace(src.Package)
	if pkg == "" {
		return "", fmt.Errorf("pluginfetch: npm source requires a package")
	}

	if tarball, ok := npmTarballURL(src); ok {
		data, err := f.download(ctx, tarball, nil, strings.TrimSpace(src.SHA256) != "")
		if err != nil {
			return "", err
		}
		// expectedIntegrity maps to the archive sha256 pin when the entry
		// carries one.
		if err := verifySHA256(data, src.SHA256, tarball); err != nil {
			return "", err
		}
		if err := mkdirAll(dest); err != nil {
			return "", err
		}
		if err := extractArchive(data, dest); err != nil {
			return "", err
		}
		// npm tarballs wrap their contents in a top-level "package/" directory;
		// the loader's wrapper-promotion handles that, so dest is returned
		// as-is here.
		return dest, nil
	}

	// Registry resolution: GET the packument, resolve the version, download and
	// verify dist.tarball.
	registry := strings.TrimSpace(src.Registry)
	if registry == "" {
		registry = defaultNPMRegistry
	}

	// A plain-http registry is accepted only when the tarball is verified
	// against the source's own sha256 pin (the packument's integrity would
	// come over the same unauthenticated channel), or for a loopback host.
	srcPinned := strings.TrimSpace(src.SHA256) != ""
	packument, err := f.fetchPackument(ctx, registry, pkg, srcPinned)
	if err != nil {
		return "", err
	}

	meta, resolved, err := resolveNPMVersion(packument, src.Version)
	if err != nil {
		return "", fmt.Errorf("pluginfetch: resolve npm %s@%s: %w", pkg, npmSpecOrLatest(src.Version), err)
	}
	if strings.TrimSpace(meta.Dist.Tarball) == "" {
		return "", fmt.Errorf("pluginfetch: npm %s@%s (%s): registry reported no tarball URL", pkg, npmSpecOrLatest(src.Version), resolved)
	}

	// The tarball is pinned by the source's sha256 or by the packument's SRI
	// integrity — the latter only counts when the packument itself came over
	// https:// (or from a loopback registry).
	tarballPinned := srcPinned || (strings.TrimSpace(meta.Dist.Integrity) != "" && checkDownloadURL(npmPackumentURL(registry, pkg), false) == nil)
	data, err := f.download(ctx, meta.Dist.Tarball, nil, tarballPinned)
	if err != nil {
		return "", err
	}
	// Verify the registry-reported integrity (SRI) and/or shasum before
	// extracting; reject a mismatch.
	if err := verifyNPMIntegrity(data, meta.Dist.Integrity, meta.Dist.Shasum, fmt.Sprintf("%s@%s", pkg, resolved)); err != nil {
		return "", err
	}
	// Honor a caller-supplied content pin as well, if any.
	if err := verifySHA256(data, src.SHA256, meta.Dist.Tarball); err != nil {
		return "", err
	}

	if err := mkdirAll(dest); err != nil {
		return "", err
	}
	if err := extractArchive(data, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// npmTarballURL returns a direct tarball URL if the source already names one
// (in Version or Package), avoiding registry metadata resolution.
func npmTarballURL(src plugins.Source) (string, bool) {
	for _, c := range []string{src.Version, src.Package} {
		c = strings.TrimSpace(c)
		if strings.HasPrefix(c, "http://") || strings.HasPrefix(c, "https://") {
			if strings.HasSuffix(c, ".tgz") || strings.HasSuffix(c, ".tar.gz") {
				return c, true
			}
		}
	}
	return "", false
}

// -----------------------------------------------------------------------------
// Packument fetch + version resolution
// -----------------------------------------------------------------------------

// npmPackument is the subset of the registry packument the resolver reads.
type npmPackument struct {
	DistTags map[string]string         `json:"dist-tags"`
	Versions map[string]npmVersionMeta `json:"versions"`
}

// npmVersionMeta is one entry of the packument "versions" map.
type npmVersionMeta struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
		Shasum    string `json:"shasum"`
	} `json:"dist"`
}

// fetchPackument downloads and parses <registry>/<pkg>. A scoped package name
// ("@scope/pkg") has its slash percent-encoded ("@scope%2fpkg") so the registry
// treats the whole name as one path segment.
func (f *Fetcher) fetchPackument(ctx context.Context, registry, pkg string, pinned bool) (*npmPackument, error) {
	url := npmPackumentURL(registry, pkg)
	data, err := f.download(ctx, url, map[string]string{"Accept": "application/json"}, pinned)
	if err != nil {
		return nil, err
	}
	var pm npmPackument
	if err := json.Unmarshal(data, &pm); err != nil {
		return nil, fmt.Errorf("pluginfetch: parse npm packument for %s: %w", pkg, err)
	}
	if len(pm.Versions) == 0 {
		return nil, fmt.Errorf("pluginfetch: npm packument for %s listed no versions", pkg)
	}
	return &pm, nil
}

// npmPackumentURL builds the packument URL, encoding a scoped name's slash.
func npmPackumentURL(registry, pkg string) string {
	reg := strings.TrimRight(registry, "/")
	enc := pkg
	if strings.HasPrefix(pkg, "@") {
		if i := strings.IndexByte(pkg, '/'); i >= 0 {
			enc = pkg[:i] + "%2f" + pkg[i+1:]
		}
	}
	return reg + "/" + enc
}

func npmSpecOrLatest(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "latest"
	}
	return spec
}

// resolveNPMVersion resolves spec against a packument, returning the selected
// version entry and its version string. Resolution order:
// an empty spec means the "latest" dist-tag; a dist-tag name resolves through
// dist-tags; an exact version is taken as-is; anything else is treated as a
// semver range and the highest satisfying version wins.
func resolveNPMVersion(pm *npmPackument, spec string) (npmVersionMeta, string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = "latest"
	}

	// dist-tag (handles "latest" and any named tag).
	if v, ok := pm.DistTags[spec]; ok {
		if meta, ok := pm.Versions[v]; ok {
			return meta, v, nil
		}
		return npmVersionMeta{}, "", fmt.Errorf("dist-tag %q points to missing version %q", spec, v)
	}

	// exact version (tolerating a single leading 'v').
	if meta, ok := pm.Versions[spec]; ok {
		return meta, spec, nil
	}
	if trimmed := strings.TrimPrefix(spec, "v"); trimmed != spec {
		if meta, ok := pm.Versions[trimmed]; ok {
			return meta, trimmed, nil
		}
	}

	// range: pick the highest satisfying version.
	best, ok := maxSatisfyingNPM(pm.Versions, spec)
	if !ok {
		return npmVersionMeta{}, "", fmt.Errorf("no version matches %q", spec)
	}
	return pm.Versions[best], best, nil
}

// maxSatisfyingNPM returns the highest version key that satisfies the range
// spec, comparing with semver precedence.
func maxSatisfyingNPM(versions map[string]npmVersionMeta, spec string) (string, bool) {
	var best string
	var bestVer updater.Version
	have := false
	for key := range versions {
		v, ok := updater.ParseVersion(key)
		if !ok {
			continue
		}
		if !satisfiesRange(v, spec) {
			continue
		}
		if !have || v.Compare(bestVer) > 0 {
			best, bestVer, have = key, v, true
		}
	}
	return best, have
}

// satisfiesRange reports whether v satisfies a minimal semver range.
func satisfiesRange(v updater.Version, spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "*" || strings.EqualFold(spec, "x") || strings.EqualFold(spec, "latest") {
		return true
	}
	// OR across "||"-separated clauses.
	for _, clause := range strings.Split(spec, "||") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		if satisfiesClause(v, clause) {
			return true
		}
	}
	return false
}

// satisfiesClause evaluates one AND-joined set of comparators.
func satisfiesClause(v updater.Version, clause string) bool {
	for _, comp := range strings.Fields(clause) {
		if !satisfiesComparator(v, comp) {
			return false
		}
	}
	return true
}

// satisfiesComparator evaluates a single comparator ("^1.2.3", ">=1.0.0", …).
// An unparseable operand never satisfies.
func satisfiesComparator(v updater.Version, comp string) bool {
	if comp == "*" || strings.EqualFold(comp, "x") {
		return true
	}
	op, operand := splitComparator(comp)
	base, ok := updater.ParseVersion(operand)
	if !ok {
		return false
	}
	switch op {
	case "^":
		return v.Compare(base) >= 0 && v.Compare(caretUpper(base)) < 0
	case "~":
		return v.Compare(base) >= 0 && v.Compare(tildeUpper(base)) < 0
	case ">=":
		return v.Compare(base) >= 0
	case "<=":
		return v.Compare(base) <= 0
	case ">":
		return v.Compare(base) > 0
	case "<":
		return v.Compare(base) < 0
	default: // "" or "="
		return v.Compare(base) == 0
	}
}

// splitComparator separates a comparator's operator prefix from its version.
func splitComparator(comp string) (op, operand string) {
	for _, p := range []string{">=", "<=", "^", "~", ">", "<", "="} {
		if strings.HasPrefix(comp, p) {
			return p, strings.TrimSpace(comp[len(p):])
		}
	}
	return "", comp
}

// caretUpper returns the exclusive upper bound for a caret range per npm rules:
// ^1.2.3 → 2.0.0, ^0.2.3 → 0.3.0, ^0.0.3 → 0.0.4.
func caretUpper(v updater.Version) updater.Version {
	switch {
	case v.Major > 0:
		return updater.Version{Major: v.Major + 1}
	case v.Minor > 0:
		return updater.Version{Minor: v.Minor + 1}
	default:
		return updater.Version{Patch: v.Patch + 1}
	}
}

// tildeUpper returns the exclusive upper bound for a tilde range:
// ~1.2.3 → 1.3.0, ~1.2 → 1.3.0, ~1 → 2.0.0 (approximated via the parsed value).
func tildeUpper(v updater.Version) updater.Version {
	return updater.Version{Major: v.Major, Minor: v.Minor + 1}
}

// -----------------------------------------------------------------------------
// Integrity verification
// -----------------------------------------------------------------------------

// verifyNPMIntegrity verifies downloaded tarball bytes against the registry's
// reported integrity. If dist.integrity is present it is checked as a base64
// Subresource-Integrity value (strongest reported algorithm wins); otherwise if
// dist.shasum is present it is checked as a hex sha1. A mismatch is rejected. No
// reported value leaves the download unverified.
func verifyNPMIntegrity(data []byte, integrity, shasum, spec string) error {
	if strings.TrimSpace(integrity) != "" {
		if verifySRI(data, integrity) {
			return nil
		}
		return fmt.Errorf("pluginfetch: integrity check failed for npm %s: tarball does not match reported integrity %q; not installed", spec, strings.TrimSpace(integrity))
	}
	if strings.TrimSpace(shasum) != "" {
		sum := sha1.Sum(data)
		got := hex.EncodeToString(sum[:])
		if strings.EqualFold(got, strings.TrimSpace(shasum)) {
			return nil
		}
		return fmt.Errorf("pluginfetch: integrity check failed for npm %s: expected shasum %s, got %s; not installed", spec, strings.ToLower(strings.TrimSpace(shasum)), got)
	}
	return nil
}

// verifySRI checks data against a (possibly space-separated, multi-algorithm)
// Subresource-Integrity string. The strongest algorithm present is computed and
// one of its reported digests must match.
func verifySRI(data []byte, integrity string) bool {
	hashes := strings.Fields(integrity)
	for _, algo := range npmSRIAlgos {
		prefix := algo + "-"
		var want []string
		for _, h := range hashes {
			if strings.HasPrefix(h, prefix) {
				want = append(want, h)
			}
		}
		if len(want) == 0 {
			continue
		}
		var sum []byte
		switch algo {
		case "sha512":
			s := sha512.Sum512(data)
			sum = s[:]
		case "sha384":
			s := sha512.Sum384(data)
			sum = s[:]
		case "sha256":
			s := sha256.Sum256(data)
			sum = s[:]
		case "sha1":
			s := sha1.Sum(data)
			sum = s[:]
		}
		got := algo + "-" + base64.StdEncoding.EncodeToString(sum)
		for _, w := range want {
			if w == got {
				return true
			}
		}
		// The strongest reported algorithm did not match: reject.
		return false
	}
	return false
}
