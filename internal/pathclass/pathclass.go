// Package pathclass is the path-classification predicate library. It is the
// vocabulary that the path-vetting / symlink-resolution engine in
// internal/pathguard depends on: a family of pure, string-level predicates that
// decide what *kind* of path a string names (UNC, NT device, automount anchor,
// network root, macOS firmlink, WSL path, tilde-relative, …) and a few
// normalisers/segment helpers used to compare paths.
//
// The package contains only PURE, string-only classifiers and normalisers,
// built on path/filepath, regexp and strings. The filesystem-backed
// realpath/symlink-chain walker lives in internal/fsfacade.
//
// POSIX vs Windows: the separator/traversal/UNC predicates are Windows-aware
// (they treat both "/" and "\\" as separators). The automount / network /
// firmlink predicates are POSIX-rooted (they only fire on paths beginning with
// "/"). Some predicates take an explicit os argument.
package pathclass

import (
	"os"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Separator / traversal / UNC-ish classifiers (Windows-aware)
// ---------------------------------------------------------------------------

var reNTObjectPrefix = regexp.MustCompile(`^[\\/]\?\?[\\/]`)

// IsNTDevicePath reports whether a path uses the Windows NT object-manager
// device prefix "\??\" (either separator). As a fallback, when the string
// contains "??" it is re-checked after an approximate Windows normalisation,
// using a path.win32.normalize()-style guard.
func IsNTDevicePath(p string) bool {
	if reNTObjectPrefix.MatchString(p) {
		return true
	}
	if strings.Contains(p, "??") {
		return reNTObjectPrefix.MatchString(winNormalizeApprox(p))
	}
	return false
}

var reTwoLeadingSeps = regexp.MustCompile(`^[\\/]{2}`)

// IsUNCOrDevicePath reports whether a path begins with two separators (UNC /
// device-namespace) or is an NT device path.
func IsUNCOrDevicePath(p string) bool {
	return reTwoLeadingSeps.MatchString(p) || IsNTDevicePath(p)
}

var reDeviceNamespaceRoot = regexp.MustCompile(`^[\\/]{2}[?.][\\/]`)

// IsDeviceNamespaceRoot reports whether a path is a Windows device-namespace
// root "\\?\" or "\\.\" (either separator).
func IsDeviceNamespaceRoot(p string) bool {
	return reDeviceNamespaceRoot.MatchString(p)
}

var reDotSegLoose = regexp.MustCompile(`(^|[\\/])\.{1,2}[. ]*([\\/]|$)`)

// HasDotSegment reports whether the path contains a "." or ".." segment,
// tolerating trailing dots/spaces (a Windows filename-equivalence trick),
// with either separator.
func HasDotSegment(p string) bool {
	return reDotSegLoose.MatchString(p)
}

var (
	reDotSegWin   = regexp.MustCompile(`(^|[\\/])\.{1,2}([\\/]|$)`)
	reDotSegPosix = regexp.MustCompile(`(^|/)\.{1,2}(/|$)`)
)

// HasDotSegmentForOS reports whether the path has a strict "." or ".." segment.
// On "win32" both separators are considered; otherwise only "/".
func HasDotSegmentForOS(p, goos string) bool {
	if goos == "win32" {
		return reDotSegWin.MatchString(p)
	}
	return reDotSegPosix.MatchString(p)
}

// HasDotSegmentPOSIX is HasDotSegmentForOS(p, "linux").
func HasDotSegmentPOSIX(p string) bool {
	return HasDotSegmentForOS(p, "linux")
}

// ---------------------------------------------------------------------------
// UNC server-name extraction / comparison
// ---------------------------------------------------------------------------

var (
	reDeviceNotUNC = regexp.MustCompile(`(?i)^[\\/]{2}[?.][\\/]`)
	reUNCServer    = regexp.MustCompile(`(?i)^[\\/]{2}(?:[?.][\\/]unc[\\/])?([^\\/]+)`)
)

// UNCServerName extracts the (lower-cased) server/host component of a UNC path,
// understanding the "\\?\UNC\" and "\\.\UNC\" long forms. It returns "" when
// the path is a bare device-namespace root (e.g. "\\?\C:") or has no server.
func UNCServerName(p string) string {
	// Device-namespace prefix that is NOT "...\UNC\" has no server.
	if reDeviceNotUNC.MatchString(p) {
		rest := p[4:] // after the 4-byte "\\?\" / "\\.\" prefix
		if !strings.HasPrefix(strings.ToLower(rest), "unc\\") &&
			!strings.HasPrefix(strings.ToLower(rest), "unc/") {
			return ""
		}
	}
	m := reUNCServer.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// CrossesUNCBoundary reports whether path p, interpreted relative to base e,
// leaves e's UNC share: it is only meaningful for UNC/device paths, and fires
// on device roots, NT device paths, dot segments, or a differing server.
func CrossesUNCBoundary(p, base string) bool {
	if !IsUNCOrDevicePath(p) {
		return false
	}
	if IsDeviceNamespaceRoot(p) || IsNTDevicePath(p) {
		return true
	}
	if HasDotSegment(p) {
		return true
	}
	ps := UNCServerName(p)
	return ps == "" || ps != UNCServerName(base)
}

// ---------------------------------------------------------------------------
// macOS firmlink detection
// ---------------------------------------------------------------------------

var (
	reFirmlinkAnywhere = regexp.MustCompile(`(?i)/\.(?:vol|file|nofollow|resolve)(?:/|$)`)
	reFirmlinkSegment  = regexp.MustCompile(`(?i)^\.(?:vol|file|nofollow|resolve)$`)
)

// IsMacFirmlinkPath reports whether a POSIX path's first resolved segment is one
// of the macOS firmlink/volume entry points: ".vol", ".file", ".nofollow" or
// ".resolve".
func IsMacFirmlinkPath(p string) bool {
	if !reFirmlinkAnywhere.MatchString(p) {
		return false
	}
	if !strings.HasPrefix(p, "/") {
		return false
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		stack = append(stack, seg)
		if len(stack) == 1 && reFirmlinkSegment.MatchString(seg) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Case folding / segment normalisation helpers (POSIX anchors)
// ---------------------------------------------------------------------------

// lowerASCII lower-cases only ASCII A–Z.
func lowerASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

var reInvisible = regexp.MustCompile("[\u200c-\u200f\u202a-\u202e\u206a-\u206f\ufeff]")

// foldInvisibleCase strips bidi/zero-width formatting characters and applies a
// round-trip upper-then-lower case fold.
func foldInvisibleCase(s string) string {
	s = reInvisible.ReplaceAllString(s, "")
	return strings.ToLower(strings.ToUpper(s))
}

// isAutomountAnchorSegments reports whether a segment slice names an automount
// anchor: ["net", host] or ["network", "servers", host] (case-insensitively).
func isAutomountAnchorSegments(segs []string) bool {
	if len(segs) == 2 && foldInvisibleCase(segs[0]) == "net" {
		return true
	}
	if len(segs) == 3 && foldInvisibleCase(segs[0]) == "network" && foldInvisibleCase(segs[1]) == "servers" {
		return true
	}
	return false
}

// AutomountAnchorHost walks a POSIX path (resolving "." and ".." lexically) and,
// as soon as its leading segments form an automount anchor, returns the
// lower-cased host component. Returns "" when there is no anchor.
func AutomountAnchorHost(p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		stack = append(stack, seg)
		if isAutomountAnchorSegments(stack) {
			return lowerASCII(seg)
		}
	}
	return ""
}

// automountAnchorPrefix returns the "/a/b" prefix of a POSIX path up to and
// including its automount anchor, or "" if there is none.
func automountAnchorPrefix(p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		stack = append(stack, seg)
		if isAutomountAnchorSegments(stack) {
			return "/" + strings.Join(stack, "/")
		}
	}
	return ""
}

// AutomountAnchor returns the "/net/host" (or "/network/servers/host") prefix
// of a POSIX path up to and including its automount anchor, or "" when the
// path has none. It is the exported form of automountAnchorPrefix, used by the
// network-aware symlink walker in internal/fsfacade.
func AutomountAnchor(p string) string { return automountAnchorPrefix(p) }

// IsAutomountPath reports whether a POSIX path contains an automount anchor
// ("/net/host" or "/network/servers/host").
func IsAutomountPath(p string) bool {
	return automountAnchorPrefix(p) != ""
}

// IsBareNetRoot reports whether a POSIX path resolves to exactly the single
// segment "net".
func IsBareNetRoot(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		stack = append(stack, seg)
	}
	return len(stack) == 1 && strings.ToLower(stack[0]) == "net"
}

// IsNetworkRoot reports whether a POSIX path's first resolved segment is
// "network" (case-insensitively).
func IsNetworkRoot(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		stack = append(stack, seg)
		if len(stack) == 1 && strings.ToLower(stack[0]) == "network" {
			return true
		}
	}
	return false
}

// IsGOPath is disabled in this build: it unconditionally returns false.
func IsGOPath(string) bool { return false }

// IsAutomountNetBareOrFirmlink reports automount, IsGOPath, bare /net, or firmlink.
func IsAutomountNetBareOrFirmlink(p string) bool {
	return IsAutomountPath(p) || IsGOPath(p) || IsBareNetRoot(p) || IsMacFirmlinkPath(p)
}

// ---------------------------------------------------------------------------
// Automount path normalisation / collection
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// WSL / UNC classification
// ---------------------------------------------------------------------------

var reWSLDistro = regexp.MustCompile(`(?i)^[\\/]{2}wsl(?:\$|\.localhost)[\\/]([^\\/]*)`)
var reDotsSpaces = regexp.MustCompile(`^\.{0,2}[. ]*$`)

// IsWSLPath reports whether a path is a "\\wsl$\distro" or
// "\\wsl.localhost\distro" UNC path with a non-degenerate distro name.
func IsWSLPath(p string) bool {
	m := reWSLDistro.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	return !reDotsSpaces.MatchString(m[1])
}

// ---------------------------------------------------------------------------
// Tilde / glob
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Trusted-root escape predicates (automount / network / firmlink crossing)
// ---------------------------------------------------------------------------

// CrossesAutomountBoundaryDarwin reports whether p crosses an automount/firmlink
// boundary relative to base. Defaults to the "linux" os where the darwin
// analysis is inert.
func CrossesAutomountBoundaryDarwin(p, base string) bool {
	if IsMacFirmlinkPath(p) {
		return true
	}
	if IsAutomountPath(p) {
		h := AutomountAnchorHost(p)
		if h == "" || h != AutomountAnchorHost(base) {
			return true
		}
	}
	return darwinAutomountEscape(p, base, "linux")
}

// automountParse is the {complete, partial} anchor decomposition used by the
// darwin automount analysis.
type automountParse struct {
	complete []string
	partial  []string
}

const automountMaxDepth = 6

// parseAutomountAnchors walks a POSIX path collecting complete and partial
// "/network/..." (and, when homeFlag, "/home/user") anchor forms, up to a depth
// of 6 segments.
func parseAutomountAnchors(p, goos string, homeFlag bool) automountParse {
	var res automountParse
	if !strings.HasPrefix(p, "/") {
		return res
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		stack = append(stack, seg)
		if len(stack) > automountMaxDepth {
			continue
		}
		candidate := "/" + strings.Join(stack, "/")
		if goos == "darwin" {
			candidate = StripMacDataVolume(candidate)
		}
		var u []string
		for _, s := range strings.Split(candidate, "/") {
			if s != "" {
				u = append(u, s)
			}
		}
		if len(u) == 0 {
			continue
		}
		head := strings.ToLower(u[0])
		if head == "network" {
			switch len(u) {
			case 1:
				res.partial = append(res.partial, "/network/")
			case 2:
				res.partial = append(res.partial, "/network/"+lowerASCII(u[1])+"/")
			case 3:
				res.complete = append(res.complete, "/network/"+lowerASCII(u[1])+"/"+lowerASCII(u[2]))
			}
		} else if homeFlag && head == "home" && len(u) == 2 {
			res.complete = append(res.complete, "/home/"+u[1])
		}
	}
	return res
}

func isSuspectDarwinAutomount(p, goos string, homeFlag bool, homeAnchor string) bool {
	if goos != "darwin" {
		return false
	}
	if IsNetworkRoot(p) || IsMacFirmlinkPath(p) {
		return true
	}
	if !strings.HasPrefix(p, "/") {
		return false
	}
	parsed := parseAutomountAnchors(p, goos, homeFlag)
	if len(parsed.partial) > 0 {
		return true
	}
	for _, s := range parsed.complete {
		if strings.HasPrefix(s, "/network/") {
			return true
		}
	}
	for _, s := range parsed.complete {
		if strings.HasPrefix(s, "/home/") && s != homeAnchor {
			return true
		}
	}
	return false
}

// darwinAutomountEscape is the automount cross-check used by EscapesTrustedRoot
// and CrossesAutomountBoundaryDarwin; inert for non-darwin os.
func darwinAutomountEscape(p, base, goos string) bool {
	homeFlag := goos == "darwin"
	if goos != "darwin" || !isSuspectDarwinAutomount(p, goos, homeFlag, currentHomeAnchor()) {
		return false
	}
	var baseAnchor string
	if bc := parseAutomountAnchors(base, goos, homeFlag).complete; len(bc) > 0 {
		baseAnchor = bc[0]
	}
	pp := parseAutomountAnchors(p, goos, homeFlag)
	if len(pp.complete) == 0 && len(pp.partial) == 0 {
		return true
	}
	if baseAnchor == "" {
		return true
	}
	for _, a := range pp.complete {
		if a != baseAnchor {
			return true
		}
	}
	for _, a := range pp.partial {
		if !strings.HasPrefix(baseAnchor, a) {
			return true
		}
	}
	return false
}

// currentHomeAnchor returns "/home/<user>" derived from the user's home dir (via
// the macOS data-volume firmlink strip), or "" on error.
func currentHomeAnchor() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`^/home/([^/]+)`).FindStringSubmatch(StripMacDataVolume(home))
	if m == nil {
		return ""
	}
	return "/home/" + m[1]
}

// ---------------------------------------------------------------------------
// macOS data-volume firmlink stripping / gocode worktree detection
// ---------------------------------------------------------------------------

var macDataVolumeRoots = []string{
	"usr/local", "usr/libexec/cups", "usr/share/snmp", "AppleInternal",
	"Applications", "Library", "Users", "Volumes", "cores", "home", "media",
	"mnt", "opt", "pkg", "private", "sw",
}

// StripMacDataVolume rewrites "/System/Volumes/Data/<root>/..." to "/<root>/..."
// when <root> is a known firmlinked top-level directory. Other paths are
// returned unchanged.
func StripMacDataVolume(p string) string {
	e := strings.Split(p, "/")
	if len(e) < 5 || e[0] != "" ||
		foldCase(at(e, 1)) != "system" || foldCase(at(e, 2)) != "volumes" || foldCase(at(e, 3)) != "data" {
		return p
	}
	n := e[4:]
	for _, root := range macDataVolumeRoots {
		parts := strings.Split(root, "/")
		if len(n) < len(parts) {
			continue
		}
		ok := true
		for i, want := range parts {
			if foldCase(at(n, i)) != foldCase(want) {
				ok = false
				break
			}
		}
		if ok {
			return "/" + strings.Join(n, "/")
		}
	}
	return p
}

// foldCase is the full-Unicode upper-then-lower fold.
func foldCase(s string) string { return strings.ToLower(strings.ToUpper(s)) }

// ---------------------------------------------------------------------------
// Lexical within-root check
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// small shared helpers
// ---------------------------------------------------------------------------

// at returns s[i] or "" when out of range.
func at(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return ""
	}
	return s[i]
}

var reDupSep = regexp.MustCompile(`\\{2,}`)

// winNormalizeApprox reproduces Node's path.win32.normalize() for IsNTDevicePath's
// "??" fallback: forward slashes become backslashes, separator runs collapse,
// and "." / ".." segments are resolved (".." never climbs above a root; in a
// relative path a leading ".." is kept), preserving a trailing separator. This
// matters for the check: "/x/../??/y" normalizes to the NT prefix "\??\y".
// Only paths starting with two separators (UNC / device roots, whose root
// parsing Node does specially) keep the plain separator collapse; such a path
// can never normalize to a single-separator "\??\" prefix anyway.
func winNormalizeApprox(p string) string {
	p = strings.ReplaceAll(p, "/", "\\")
	if strings.HasPrefix(p, "\\\\") {
		return "\\\\" + reDupSep.ReplaceAllString(p[2:], "\\")
	}
	if p == "" {
		return "."
	}
	root, rest, isAbs := "", p, false
	switch {
	case len(p) >= 2 && p[1] == ':' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z':
		root, rest = p[:2], p[2:]
		if strings.HasPrefix(rest, "\\") {
			root += "\\"
			isAbs = true
		}
	case p[0] == '\\':
		root, isAbs = "\\", true
	}
	trailing := strings.HasSuffix(rest, "\\")
	var segs []string
	for _, seg := range strings.Split(rest, "\\") {
		switch seg {
		case "", ".":
		case "..":
			if len(segs) > 0 && segs[len(segs)-1] != ".." {
				segs = segs[:len(segs)-1]
			} else if !isAbs {
				segs = append(segs, "..")
			}
		default:
			segs = append(segs, seg)
		}
	}
	tail := strings.Join(segs, "\\")
	if tail == "" && !isAbs {
		tail = "."
	}
	if tail != "" && trailing {
		tail += "\\"
	}
	return root + tail
}
