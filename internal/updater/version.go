// Package updater implements the self-updater behind `gocode update` and
// `gocode install`. Releases are distributed through GitHub Releases of a
// configured repository; there is no gocode download server.
//
// # Release source
//
// The GitHub REST API is queried over an injectable Doer:
//
//   - stable channel (default): GET {api}/repos/{owner}/{repo}/releases/latest
//     (GitHub excludes drafts and prereleases).
//   - "latest" channel (opt-in): GET {api}/repos/{owner}/{repo}/releases?per_page=50,
//     choosing the highest semver among non-draft releases, prereleases
//     included.
//   - explicit version: GET {api}/repos/{owner}/{repo}/releases/tags/{tag}
//     (tried as "v<version>" and "<version>").
//
// Every request sends Accept: application/vnd.github+json,
// X-GitHub-Api-Version: 2022-11-28 and User-Agent: gocode-updater, plus
// Authorization: Bearer $GITHUB_TOKEN only when that variable is set (and
// only to the API host). The API base is overridable (GitHub Enterprise,
// tests). Nothing about the user or installation is sent.
//
// # Repository configuration
//
// Precedence: $GOCODE_UPDATE_REPO > settings "updateRepo" > the build-time
// DefaultRepo, set with
//
//	-ldflags "-X github.com/umairge66/gocode/internal/updater.DefaultRepo=owner/repo"
//
// The build's own version is CurrentVersion ("dev" by default), set with
//
//	-ldflags "-X github.com/umairge66/gocode/internal/updater.CurrentVersion=1.2.3"
//
// # Release asset convention
//
// Each release (tag "v1.2.3" or "1.2.3", semver) must publish, per platform,
// ONE of:
//
//	gocode_<goos>_<goarch>            raw executable (".exe" appended on windows)
//	gocode_<goos>_<goarch>.tar.gz     archive containing "gocode"
//	gocode_<goos>_<goarch>.zip        archive containing "gocode" / "gocode.exe"
//
// using Go's GOOS/GOARCH spelling (e.g. gocode_linux_amd64,
// gocode_darwin_arm64.tar.gz, gocode_windows_amd64.exe). Archives may nest the
// binary in a directory; entries with absolute or ".." paths reject the
// archive. Every release must also publish checksums.txt in sha256sum /
// goreleaser format — one "<sha256-hex>  <asset-name>" line per asset. The
// SHA-256 check is mandatory: a missing checksums.txt, a missing line, or a
// mismatch refuses the install.
//
// Files: version.go (semver), github.go (release API), repo.go (repository
// configuration), assets.go (asset selection, checksums, archive extraction),
// install.go (download/verify, atomic replace), update.go (check + apply),
// restart*.go (handoff to the new binary).
package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// -----------------------------------------------------------------------------
// Semver parse + compare.
//
// The three predicates:
//
//	order(a,b) == 1    (a >  b)   GreaterThan
//	order(a,b) >= 0    (a >= b)   AtLeast
//	order(a,b) == -1   (a <  b)   LessThan

// Version is a parsed semantic version. Build metadata is retained for the
// normalized string form but, per semver 2.0.0, is ignored for precedence.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease []string // dot-separated identifiers, empty when none
	Build      string   // raw build metadata (after '+'), empty when none
}

// String renders the version in normalized form (no leading 'v').
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Prerelease) > 0 {
		s += "-" + strings.Join(v.Prerelease, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Normalized returns the major.minor.patch[-prerelease] form, dropping build
// metadata, like node-semver's `.version` field.
func (v Version) Normalized() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Prerelease) > 0 {
		s += "-" + strings.Join(v.Prerelease, ".")
	}
	return s
}

// ParseVersion parses a strict semver string, tolerating a single leading 'v'.
// It reports ok=false for anything that is not major.minor.patch with optional
// prerelease / build, like node-semver parse.
func ParseVersion(s string) (Version, bool) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return Version{}, false
	}

	var v Version

	// Split off build metadata (everything after the first '+').
	if i := strings.IndexByte(raw, '+'); i >= 0 {
		v.Build = raw[i+1:]
		raw = raw[:i]
		if v.Build == "" || !isValidDotSeparated(v.Build, true) {
			return Version{}, false
		}
	}

	// Split off prerelease (everything after the first '-').
	var pre string
	if i := strings.IndexByte(raw, '-'); i >= 0 {
		pre = raw[i+1:]
		raw = raw[:i]
		if pre == "" {
			return Version{}, false // trailing '-' with empty prerelease
		}
	}

	// Core: exactly three numeric, non-negative, no-leading-zero fields.
	core := strings.Split(raw, ".")
	if len(core) != 3 {
		return Version{}, false
	}
	nums := [3]*int{&v.Major, &v.Minor, &v.Patch}
	for i, part := range core {
		n, ok := parseNumericID(part)
		if !ok {
			return Version{}, false
		}
		*nums[i] = n
	}

	if pre != "" {
		if !isValidDotSeparated(pre, false) {
			return Version{}, false
		}
		v.Prerelease = strings.Split(pre, ".")
	}

	return v, true
}

// IsValid reports whether s is a well-formed semver version: the loose regex
// plus a successful parse.
func IsValid(s string) bool {
	_, ok := ParseVersion(s)
	return ok
}

// Compare returns -1, 0, or 1 as a is less than, equal to, or greater than b,
// following semver 2.0.0 precedence (build metadata ignored). It errors if
// either operand is not a valid version ("Invalid SemVer").
func Compare(a, b string) (int, error) {
	va, ok := ParseVersion(a)
	if !ok {
		return 0, fmt.Errorf("updater: invalid semver %q", a)
	}
	vb, ok := ParseVersion(b)
	if !ok {
		return 0, fmt.Errorf("updater: invalid semver %q", b)
	}
	return va.Compare(vb), nil
}

// Compare returns -1, 0, or 1 comparing v to o by semver precedence.
func (v Version) Compare(o Version) int {
	for _, d := range []int{
		cmpInt(v.Major, o.Major),
		cmpInt(v.Minor, o.Minor),
		cmpInt(v.Patch, o.Patch),
	} {
		if d != 0 {
			return d
		}
	}
	return comparePrerelease(v.Prerelease, o.Prerelease)
}

// GreaterThan reports a > b. It returns false if either is invalid,
// matching the callers that guarded on a prior validity check.
func GreaterThan(a, b string) bool { return cmpOrZero(a, b) == 1 }

// AtLeast reports a >= b.
func AtLeast(a, b string) bool { return cmpOrZero(a, b) >= 0 }

// LessThan reports a < b.
func LessThan(a, b string) bool { return cmpOrZero(a, b) == -1 }

// cmpOrZero compares a and b, returning 0 on any parse error. The predicates
// are only meaningful after a validity check, so a defensive 0 here preserves
// "neither strictly greater nor less".
func cmpOrZero(a, b string) int {
	c, err := Compare(a, b)
	if err != nil {
		return 0
	}
	return c
}

// comparePrerelease implements semver prerelease precedence: a version with a
// prerelease has lower precedence than one without; identifiers are compared
// left to right, numeric identifiers numerically and others lexically, with
// numeric always lower than alphanumeric; a longer identifier list wins when
// all shared identifiers are equal.
func comparePrerelease(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0: // no prerelease > has prerelease
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if d := compareIdentifier(a[i], b[i]); d != 0 {
			return d
		}
	}
	return cmpInt(len(a), len(b))
}

func compareIdentifier(a, b string) int {
	an, aNum := parseNumericID(a)
	bn, bNum := parseNumericID(b)
	switch {
	case aNum && bNum:
		return cmpInt(an, bn)
	case aNum: // numeric < alphanumeric
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// parseNumericID parses a non-negative integer that has no leading zero (unless
// it is exactly "0"), matching semver's numeric identifier rule.
func parseNumericID(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// isValidDotSeparated validates prerelease/build identifier lists. Build
// identifiers (build=true) may carry leading zeros; prerelease numeric
// identifiers may not.
func isValidDotSeparated(s string, build bool) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		allDigits := true
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
				return false
			}
			if c < '0' || c > '9' {
				allDigits = false
			}
		}
		if !build && allDigits && len(id) > 1 && id[0] == '0' {
			return false // prerelease numeric identifier with leading zero
		}
	}
	return true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
