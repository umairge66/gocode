package pathguard

import (
	"path"
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/pathclass"
)

// winspelling.go keeps a Windows file from being reached under a spelling a
// permission rule does not cover. The job is split in two:
//
//  1. Rule matching folds the spellings Windows itself treats as one name:
//     the matcher converts "\" to "/" and "C:\" to "/c/" on both the path and
//     the rule root, and case-folds the deny side when the platform
//     is Windows or the target is case-insensitive. It then runs over EVERY
//     spelling the symlink walk produced: a deny rule
//     hit on any spelling refuses, while an allow rule must match all of them.
//  2. Every spelling that cannot be folded reliably by string comparison
//     (8.3 short names, alternate data streams, trailing dots/spaces, "\\?\" /
//     "\\.\" / "\??\" device prefixes, DOS device suffixes, "..." runs, UNC
//     and WebDAV forms) is refused outright, whatever the rules say: writes get
//     a safetyCheck with circuitBreaker "suspiciousWindowsPath", and
//     reads are sent for manual approval.
//
// This file holds part 2 and the spelling fold used by
// part 1; RuleSetEngine.EvaluateSpellings (ruleset.go) puts them together.

// Suspicious Windows spelling kinds.
const (
	WinSpellingNTDeviceNamespace    = "nt_device_namespace"
	WinSpellingColonPastDrive       = "colon_past_drive_position"
	WinSpellingTildeDigit           = "tilde_digit"
	WinSpellingDevicePathPrefix     = "device_path_prefix"
	WinSpellingTrailingDotOrSpace   = "trailing_dot_or_whitespace"
	WinSpellingDOSDeviceSuffix      = "dos_device_suffix"
	WinSpellingDotRunSegment        = "dot_run_segment"
	WinSpellingUNCOrWebDAV          = "unc_or_webdav_form"
	circuitBreakerSuspiciousWinPath = "suspiciousWindowsPath"
	circuitBreakerRestrictedMode    = "restrictedMode"
	circuitBreakerOutsideReads      = "outsideReadsBlocked"
)

// jsSpace is the code-point set of the ECMAScript \s class (Go's \s is ASCII only).
const jsSpace = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	// winTildeDigit is /~\d/: the mangling marker of an 8.3 short name
	// ("PROGRA~1"), anywhere in the path.
	winTildeDigit = regexp.MustCompile(`~[0-9]`)
	// winTrailingDotSpace is /[.\s]+$/ (ECMAScript \s), tested per segment.
	winTrailingDotSpace = regexp.MustCompile(`[.` + jsSpace + `]+$`)
	// winDOSDeviceSuffix matches a DOS device name suffix.
	winDOSDeviceSuffix = regexp.MustCompile(`(?i)\.(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])$`)
	// winDotRun is /(^|\/|\\)\.{3,}(\/|\\|$)/.
	winDotRun = regexp.MustCompile(`(^|/|\\)\.{3,}(/|\\|$)`)
)

// SuspiciousWindowsSpelling classifies a path spelling that the permission
// layer refuses to match by string comparison, returning one of the
// WinSpelling* kinds, or "" when the spelling is ordinary. platform is the
// host platform ("windows", "wsl", "linux", "darwin", …): the
// colon-past-drive (alternate data stream) check applies on Windows and WSL,
// the UNC/WebDAV check on Windows only (the UNC detector's own gate); the rest
// apply on every platform. trusted are the trustedNetworkDirectories under
// which a UNC spelling is accepted.
func SuspiciousWindowsSpelling(e, platform string, trusted []string) string {
	if pathclass.IsNTDevicePath(e) {
		return WinSpellingNTDeviceNamespace
	}
	if platform == "windows" || platform == "wsl" {
		if len(e) > 2 && strings.IndexByte(e[2:], ':') != -1 {
			return WinSpellingColonPastDrive
		}
	}
	if winTildeDigit.MatchString(e) {
		return WinSpellingTildeDigit
	}
	if strings.HasPrefix(e, `\\?\`) || strings.HasPrefix(e, `\\.\`) ||
		strings.HasPrefix(e, "//?/") || strings.HasPrefix(e, "//./") {
		return WinSpellingDevicePathPrefix
	}
	for _, seg := range strings.FieldsFunc(e, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "." || seg == ".." {
			continue
		}
		if winTrailingDotSpace.MatchString(seg) {
			return WinSpellingTrailingDotOrSpace
		}
	}
	if winDOSDeviceSuffix.MatchString(e) {
		return WinSpellingDOSDeviceSuffix
	}
	if winDotRun.MatchString(e) {
		return WinSpellingDotRunSegment
	}
	if platform == "windows" && IsUNCPathSpelling(e, true) && !pathclass.IsWSLPath(e) &&
		!underTrustedNetworkDir(e, trusted) {
		return WinSpellingUNCOrWebDAV
	}
	return ""
}

// IsSuspiciousWindowsSpelling reports SuspiciousWindowsSpelling(...) != "".
func IsSuspiciousWindowsSpelling(e, platform string, trusted []string) bool {
	return SuspiciousWindowsSpelling(e, platform, trusted) != ""
}

// Verbatim refusal texts for a suspicious spelling.
const (
	ReasonSuspiciousWindowsRead = "Path contains suspicious Windows-specific patterns (alternate data streams, short names, long path prefixes, or three or more consecutive dots) that require manual verification"
)

// MsgSuspiciousWindowsWrite is the refusal for a write whose spelling is
// suspicious.
func MsgSuspiciousWindowsWrite(p string) string {
	return "gocode requested permissions to write to " + displayPath(p) +
		", which contains a suspicious Windows path pattern that requires manual approval."
}

// foldWindowsSpelling is the Windows-to-POSIX spelling fold applied before
// rule matching. A UNC spelling keeps its leading pair; "C:\x" becomes "/c/x"
// (drive letter lower-cased); every backslash becomes "/".
func foldWindowsSpelling(e string) string {
	if strings.HasPrefix(e, `\\`) {
		return toSlashPath(e)
	}
	if len(e) >= 3 && isASCIILetter(e[0]) && e[1] == ':' && isSep(e[2]) {
		return "/" + strings.ToLower(e[:1]) + toSlashPath(e[2:])
	}
	return toSlashPath(e)
}

// underTrustedNetworkDir reports whether e lies within one of the
// trustedNetworkDirectories, comparing only spellings of the same UNC shape
// and never for a spelling with a "." / ".." segment.
func underTrustedNetworkDir(e string, trusted []string) bool {
	if len(trusted) == 0 || pathclass.HasDotSegment(e) {
		return false
	}
	for _, s := range trusted {
		if pathclass.IsUNCOrDevicePath(e) != pathclass.IsUNCOrDevicePath(s) {
			continue
		}
		if pathWithin(e, s, true, false) {
			return true
		}
	}
	return false
}

// The macOS /private aliases pathWithin unifies before comparing.
var (
	privateVarAlias     = regexp.MustCompile(`^/private/var/`)
	privateTmpAlias     = regexp.MustCompile(`^/private/tmp(/|$)`)
	privateVarAliasFold = regexp.MustCompile(`(?i)^/private/var/`)
	privateTmpAliasFold = regexp.MustCompile(`(?i)^/private/tmp(/|$)`)
)

// pathWithin reports whether e is n or lies beneath it.
// caseFold folds both sides with foldCase; uncShapeParity first refuses any pair
// whose UNC/device shape differs, so a "\\host\share" spelling is
// never "inside" a drive-letter working directory and vice versa. Both sides
// get the foldWindowsSpelling fold and a lexical clean and the
// /private/var, /private/tmp aliases are unified.
func pathWithin(e, n string, caseFold, uncShapeParity bool) bool {
	if uncShapeParity && pathclass.IsUNCOrDevicePath(e) != pathclass.IsUNCOrDevicePath(n) {
		return false
	}
	privVar, privTmp := privateVarAlias, privateTmpAlias
	if caseFold {
		privVar, privTmp = privateVarAliasFold, privateTmpAliasFold
	}
	norm := func(s string) string {
		s = path.Clean(foldWindowsSpelling(s))
		s = privVar.ReplaceAllString(s, "/var/")
		s = privTmp.ReplaceAllString(s, "/tmp$1")
		if caseFold {
			s = foldCase(s)
		}
		return s
	}
	return withinRoot(norm(e), norm(n))
}
