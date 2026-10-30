// ruleset.go implements the rule-matching engine — the
// allowOnly / allowAllExcept / allBeneathItsDirectory matcher that sits behind
// ruleengine.go's RuleEngine seam: the rule model, the match/decision
// semantics over the compiled rule set and the spelling set, the "could not be
// determined" refusal messages and the sandbox-plant check, with the rule-set
// shapes (allowOnly / allowAllExcept / denyWithinAllow, and the
// allBeneathItsDirectory subtree expansion) taken from the filesystem
// permission config.
//
// MODEL
//
//   - Rules of one behavior ("deny"/"ask"/"allow") for one tool kind
//     ("read"/"edit") are grouped by root into {patternMap, matchers}. Each rule
//     is {ruleValue:{ruleContent}, source}; its content splits into
//     {relativePattern, root} (root defaulting to the cwd).
//   - The core matcher makes the resolved path relative to each rule root
//     (path.relative, dropping roots the path is not under), optionally
//     case-folds (lowercase, when Windows or caseInsensitiveTarget), and tests it
//     against that root's gitignore-style matchers *in reverse* (last rule
//     wins). A bare pattern `ge` matches the exact relative path; a
//     `ge + "/**"` entry matches the directory and everything beneath it (this
//     `/**` expansion is "allBeneathItsDirectory"). It returns the matched rule
//     value, or null.
//   - A wrapper around it, on a "deny" miss, retries once through the WSL
//     "second root mount" alternate spelling; for "allow" or asSpelledOnly it
//     returns the core matcher's result directly.
//   - The macOS sandbox config names the set shapes directly: denyAllExcept /
//     filesystem.allowOnly (deny-by-default allowlist), allowAllExcept
//     (allow-by-default denylist), and denyWithinAllow (deny carve-outs scanned
//     before the allowlist).
//   - An unresolvable path gets a "could not be determined" reason; the
//     sandbox-plant fallback detects a write whose spelling sits inside the
//     allowed dirs but whose real landing (after symlinks) escapes them.
//
// Dependencies on settings (the allowed working directories, the case-fold
// platform flag, the rule lists) are injected as RuleSetEngine fields here
// rather than read from globals.
package pathguard

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/umairge66/gocode/internal/redact"
	"github.com/umairge66/gocode/internal/textutil"
)

// ReasonTypeWorkingDir is the decisionReason category attached to a
// path refused for being outside the allowed working directories.
const ReasonTypeWorkingDir = "workingDir"

// RuleMode selects how a RuleSetEngine interprets its rule list. The three
// modes are the filesystem-permission shapes the matcher is compiled against.
type RuleMode int

const (
	// ModeAllowOnly is deny-by-default: a path is allowed only when it matches
	// one of Rules (an allowlist entry — an exact relative path/glob, or a
	// subtree when the pattern ends in "/**"). Anything else is refused with the
	// "you haven't granted it yet" / "outside the allowed working directories"
	// reason.
	ModeAllowOnly RuleMode = iota

	// ModeAllowAllExcept is allow-by-default: a path is allowed unless it
	// matches one of Rules (a deny entry), which refuses it with the rule's
	// Reason (decisionReason type "rule"). This is the macOS "allowAllExcept"
	// shape.
	ModeAllowAllExcept

	// ModeAllBeneathItsDirectory is deny-by-default containment: each rule names
	// a directory, and a path is allowed only when it IS that directory or lies
	// beneath it. This is the `ge + "/**"` expansion the matcher applies to every
	// rule, with no per-segment globbing.
	ModeAllBeneathItsDirectory
)

// Rule is a single compiled permission rule: a pattern resolved against a root
// directory, optionally scoped to particular operations.
type Rule struct {
	// Root is the absolute, slash-style directory the pattern is resolved
	// against (defaulting to the cwd when the rule carries no root). An empty
	// Root matches against the engine's first WorkingDir, or "/".
	Root string
	// Pattern is the rule body relative to Root. "", "**" and "/**" all mean the
	// whole Root subtree. A trailing "/**" requests subtree containment; a bare
	// pattern matches the exact relative path, with "*" / "?" globbing *within*
	// a path segment. In ModeAllBeneathItsDirectory the pattern is always
	// treated as a directory prefix (subtree containment).
	Pattern string
	// Ops restricts the rule to these operations; empty means every operation.
	Ops []Operation
	// Reason overrides the refusal text for a deny match in ModeAllowAllExcept.
	// Empty uses a default.
	Reason string
}

// RuleSetEngine is a concrete RuleEngine implementing the
// allowOnly / allowAllExcept / allBeneathItsDirectory matching semantics.
type RuleSetEngine struct {
	// Mode selects the default behavior and how Rules are interpreted.
	Mode RuleMode
	// Rules is the allowlist (ModeAllowOnly / ModeAllBeneathItsDirectory) or the
	// denylist (ModeAllowAllExcept), scanned in order; the first applicable
	// match decides.
	Rules []Rule
	// DenyWithinAllow are deny carve-outs scanned BEFORE Rules in the
	// allow-by-containment modes, so a denied subpath inside an allowed tree is
	// still refused. Ignored in ModeAllowAllExcept (where Rules already denies).
	DenyWithinAllow []Rule
	// CaseInsensitive folds case before matching a DENY rule (the
	// caseInsensitiveTarget option). The allow side is never folded: an allow
	// rule must match the spelling's exact case. Platform "windows" implies the same
	// deny-side fold.
	CaseInsensitive bool
	// WorkingDirs are the allowed working directories. They phrase a deny
	// ("…is outside the allowed working directories" vs "…you haven't granted
	// it yet"), and bound reads under Restricted and
	// BlockReadsOutsideWorkingDirectories.
	WorkingDirs []string

	// Platform is the host platform ("windows", "wsl", "linux",
	// "darwin"; "" is treated as a non-Windows POSIX host). "windows" turns on
	// the matcher's spelling fold ("\" -> "/", "C:\" -> "/c/") on both the path
	// and the rule root, the deny-side case fold, and the Windows-only parts of
	// the suspicious-spelling check (UNC/WebDAV forms; with "wsl" also the
	// colon-past-drive / alternate-data-stream check).
	Platform string
	// Restricted is the --restricted session flag: file
	// tools are confined to WorkingDirs, and every safety refusal carries the
	// "restrictedMode" circuit breaker.
	Restricted bool
	// BlockReadsOutsideWorkingDirectories is the
	// permissions.blockReadsOutsideWorkingDirectories setting (circuit breaker
	// "outsideReadsBlocked").
	BlockReadsOutsideWorkingDirectories bool
	// TrustedNetworkDirectories are network-share roots under which a UNC
	// spelling is not treated as suspicious.
	TrustedNetworkDirectories []string
}

var _ RuleEngine = RuleSetEngine{}

// Evaluate implements RuleEngine: it is EvaluateSpellings over the single
// resolved spelling.
func (e RuleSetEngine) Evaluate(resolvedPath string, op Operation, canonical bool) *Decision {
	return e.EvaluateSpellings(PathSpellings{
		Requested:  resolvedPath,
		Spellings:  []string{resolvedPath},
		Unresolved: !canonical,
	}, op)
}

// EvaluateSpellings judges the full spelling set of a target in this order:
//
//  1. deny scan: a deny rule matching ANY spelling (Spellings plus DenyOnly)
//     refuses with that rule;
//  2. an unresolved walk refuses with the "could not be determined" text,
//     a safetyCheck with the outsideReadsBlocked / restrictedMode breaker
//     where applicable;
//  3. --restricted confines every spelling to WorkingDirs;
//  4. blockReadsOutsideWorkingDirectories refuses a read with a spelling
//     outside WorkingDirs;
//  5. a suspicious Windows spelling (SuspiciousWindowsSpelling) refuses a
//     write as a safetyCheck with breaker suspiciousWindowsPath (restrictedMode
//     under --restricted), and sends a read for manual
//     approval;
//  6. the mode's allow logic: allow-by-default for ModeAllowAllExcept, else
//     EVERY spelling must match an allow rule.
func (e RuleSetEngine) EvaluateSpellings(s PathSpellings, op Operation) *Decision {
	spellings := s.Spellings
	if len(spellings) == 0 {
		spellings = []string{s.Requested}
	}
	p := toSlashPath(spellings[len(spellings)-1]) // the landing, reported as ResolvedPath
	requested := s.Requested
	if requested == "" {
		requested = spellings[len(spellings)-1]
	}

	// 1. Deny scan over every spelling, deny-only extras included.
	denyRules := e.DenyWithinAllow
	if e.Mode == ModeAllowAllExcept {
		denyRules = e.Rules
	}
	for _, sp := range append(append([]string(nil), spellings...), s.DenyOnly...) {
		if r, ok := e.matchFirst(denyRules, sp, op, e.foldDeny()); ok {
			return e.denyByRule(p, r)
		}
	}

	// 2. Unresolved walk.
	if s.Unresolved {
		undetermined := MsgUndetermined(toSlashPath(requested))
		if op == OpRead && !e.Restricted {
			if e.BlockReadsOutsideWorkingDirectories {
				return safetyDeny(p, undetermined, circuitBreakerOutsideReads)
			}
			return &Decision{
				Allowed:      false,
				ResolvedPath: p,
				Reason:       &DecisionReason{Type: ReasonTypeOther, Reason: undetermined},
			}
		}
		cb := ""
		if e.Restricted {
			cb = circuitBreakerRestrictedMode
		}
		return safetyDeny(p, undetermined, cb)
	}

	// 3. --restricted: every spelling inside the working directories.
	if e.Restricted && !e.allWithinWorkingDirs(spellings) {
		return &Decision{
			Allowed:      false,
			ResolvedPath: p,
			Reason:       &DecisionReason{Type: ReasonTypeOther, Reason: ReasonRestrictedOutside},
		}
	}

	// 4. blockReadsOutsideWorkingDirectories.
	if op == OpRead && e.BlockReadsOutsideWorkingDirectories && !e.allWithinWorkingDirs(spellings) {
		return safetyDeny(p, ReasonReadsOutsideBlocked, circuitBreakerOutsideReads)
	}

	// 5. Spellings that string matching cannot fold.
	for _, sp := range spellings {
		if !IsSuspiciousWindowsSpelling(sp, e.Platform, e.TrustedNetworkDirectories) {
			continue
		}
		if op.isMutating() {
			cb := circuitBreakerSuspiciousWinPath
			if e.Restricted {
				cb = circuitBreakerRestrictedMode
			}
			return safetyDeny(p, MsgSuspiciousWindowsWrite(requested), cb)
		}
		return &Decision{
			Allowed:      false,
			ResolvedPath: p,
			Reason:       &DecisionReason{Type: ReasonTypeOther, Reason: ReasonSuspiciousWindowsRead},
		}
	}

	// 6. Mode allow logic.
	if e.Mode == ModeAllowAllExcept {
		return &Decision{Allowed: true, ResolvedPath: p}
	}
	for _, sp := range spellings {
		if _, ok := e.matchFirst(e.Rules, sp, op, false); !ok {
			return e.denyNotGranted(p, op)
		}
	}
	return &Decision{Allowed: true, ResolvedPath: p}
}

// Verbatim refusal reasons for the confinement circuit breakers.
const (
	ReasonRestrictedOutside   = "--restricted: path outside the working directory"
	ReasonReadsOutsideBlocked = "Reads outside the working directories are blocked (permissions.blockReadsOutsideWorkingDirectories). Add the directory with /add-dir, or remove that setting."
)

// safetyDeny builds the engine's safetyCheck refusal: never classifier-approvable,
// with the named circuit breaker when cb is non-empty.
func safetyDeny(p, reason, cb string) *Decision {
	return &Decision{
		Allowed:      false,
		ResolvedPath: p,
		Reason: &DecisionReason{
			Type:           ReasonTypeSafetyCheck,
			Reason:         reason,
			CircuitBreaker: cb,
		},
	}
}

// allWithinWorkingDirs reports whether every spelling lies within some working
// directory, compared without case folding and with UNC-shape parity (a UNC
// spelling is never inside a drive or POSIX working directory). An empty
// WorkingDirs confines to nothing.
func (e RuleSetEngine) allWithinWorkingDirs(spellings []string) bool {
	for _, sp := range spellings {
		inside := false
		for _, d := range e.WorkingDirs {
			if pathWithin(sp, d, false, true) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

// foldDeny reports whether deny matching case-folds: only for a non-allow
// rule, on Windows or when the target is case-insensitive.
func (e RuleSetEngine) foldDeny() bool {
	return e.CaseInsensitive || e.Platform == "windows"
}

// matchFirst returns the first rule in rules that applies to op and whose
// pattern matches p, case-folding when fold.
func (e RuleSetEngine) matchFirst(rules []Rule, p string, op Operation, fold bool) (Rule, bool) {
	for _, r := range rules {
		if !opApplies(r.Ops, op) {
			continue
		}
		if e.ruleMatches(r, p, fold) {
			return r, true
		}
	}
	return Rule{}, false
}

// ruleMatches reports whether rule r matches path p.
//
//   - Subtree form (ModeAllBeneathItsDirectory, or a Pattern of ""/"**"/"/**"
//     or one ending in "/**"): p must be the rule directory or lie beneath it.
//   - Exact/glob form (the other modes): the path made relative to the rule
//     root must glob-match the pattern, "*"/"?" matching within a segment.
//
// On Platform "windows" both the path and the root get the foldWindowsSpelling fold first.
func (e RuleSetEngine) ruleMatches(r Rule, p string, fold bool) bool {
	root := e.ruleRoot(r)
	pat := strings.TrimPrefix(r.Pattern, "/")

	subtree := e.Mode == ModeAllBeneathItsDirectory ||
		pat == "" || pat == "**" || strings.HasSuffix(pat, "/**")
	if subtree {
		dir := strings.TrimSuffix(pat, "**")
		dir = strings.TrimSuffix(dir, "/")
		full := joinSlash(root, dir)
		return withinRoot(e.norm(p, fold), e.norm(full, fold))
	}

	rel, ok := relUnder(e.norm(root, fold), e.norm(p, fold))
	if !ok {
		return false
	}
	if fold {
		pat = foldCase(pat)
	}
	return globMatch(pat, rel)
}

// contains reports whether p is dir or lies beneath it, with the engine's
// CaseInsensitive fold applied. Used only to phrase a refusal.
func (e RuleSetEngine) contains(dir, p string) bool {
	return withinRoot(e.norm(p, e.CaseInsensitive), e.norm(dir, e.CaseInsensitive))
}

// ruleRoot resolves r.Root, defaulting an empty root to the first WorkingDir or
// "/".
func (e RuleSetEngine) ruleRoot(r Rule) string {
	if r.Root != "" {
		return r.Root
	}
	if len(e.WorkingDirs) > 0 && e.WorkingDirs[0] != "" {
		return e.WorkingDirs[0]
	}
	return "/"
}

// norm is the spelling fold applied before comparing: foldWindowsSpelling on Windows (else
// plain backslash-to-slash), then foldCase when fold.
func (e RuleSetEngine) norm(s string, fold bool) string {
	if e.Platform == "windows" {
		s = foldWindowsSpelling(s)
	} else {
		s = toSlashPath(s)
	}
	if fold {
		s = foldCase(s)
	}
	return s
}

// denyByRule refuses p with a matched deny rule's reason (decisionReason type
// "rule").
func (e RuleSetEngine) denyByRule(p string, r Rule) *Decision {
	reason := r.Reason
	if reason == "" {
		reason = "Path '" + p + "' matches a deny rule"
	}
	return &Decision{
		Allowed:      false,
		ResolvedPath: p,
		Reason:       &DecisionReason{Type: ReasonTypeRule, Reason: reason},
	}
}

// denyNotGranted refuses a path that no allow rule covered. When the path also
// lies outside every WorkingDir the reason says so;
// otherwise it is the plain "not granted yet" reason.
func (e RuleSetEngine) denyNotGranted(p string, op Operation) *Decision {
	verb := "read"
	if op.isMutating() {
		verb = "write to"
	}
	if e.outsideWorkingDirs(p) {
		return &Decision{
			Allowed:      false,
			ResolvedPath: p,
			Reason: &DecisionReason{
				Type:   ReasonTypeWorkingDir,
				Reason: MsgOutsideWorkingDirs(p),
			},
		}
	}
	return &Decision{
		Allowed:      false,
		ResolvedPath: p,
		Reason: &DecisionReason{
			Type:   ReasonTypeWorkingDir,
			Reason: MsgNotGranted(verb, p),
		},
	}
}

// outsideWorkingDirs reports whether p is outside every configured WorkingDir.
// An empty WorkingDirs is treated as "no containment configured" → not outside,
// so the plain not-granted reason is used.
func (e RuleSetEngine) outsideWorkingDirs(p string) bool {
	if len(e.WorkingDirs) == 0 {
		return false
	}
	for _, d := range e.WorkingDirs {
		if e.contains(d, p) {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// sandbox-plant fallback
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// verbatim refusal messages
// -----------------------------------------------------------------------------

// MsgUndetermined is the "could not be determined" reason for an unresolvable
// path.
func MsgUndetermined(path string) string {
	return "Where " + displayPath(path) + " leads on disk could not be determined " +
		"(a link or directory on the way could not be examined, or the links do not resolve)"
}

// MsgNotGranted is the "haven't granted it yet" message. verb is "read" or
// "write to".
func MsgNotGranted(verb, path string) string {
	return "gocode requested permissions to " + verb + " " + displayPath(path) +
		", but you haven't granted it yet."
}

// MsgOutsideWorkingDirs is the deny reason for a path that resolved outside the
// allowed working directories.
func MsgOutsideWorkingDirs(path string) string {
	return "Path resolves to " + displayPath(path) +
		", which is outside the allowed working directories"
}

// displayPath strips ANSI escapes (appending U+FFFD when any were present),
// replaces control, bidi-override, line/paragraph-separator, private-use and
// unassigned characters with U+FFFD and then drops invisible characters, then
// truncates to 160 UTF-16 code units without splitting a surrogate pair,
// appending "… [+N chars]".
func displayPath(p string) string {
	n := textutil.StripANSI(p)
	if n != p {
		n += "\uFFFD"
	}
	n = strings.Map(func(r rune) rune {
		if isDisplayUnsafe(r) {
			return '\uFFFD'
		}
		return r
	}, n)
	n = redact.StripInvisibleStable(n)
	return truncateForDisplay(n, displayPathMax)
}

// displayPathMax is the display cap for paths in messages.
const displayPathMax = 160

// isDisplayUnsafe reports membership in the display-unsafe class:
// [\x00-\x1f\x7f-\x9f\u061c\u2028\u2029\u202a-\u202e\u2066-\u2069\p{Co}\p{Cn}].
func isDisplayUnsafe(r rune) bool {
	switch {
	case r <= 0x1f, r >= 0x7f && r <= 0x9f, r == 0x061c, r == 0x2028, r == 0x2029,
		r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case unicode.Is(unicode.Co, r):
		return true
	}
	for _, t := range unicode.Categories {
		if unicode.Is(t, r) {
			return false
		}
	}
	return true // unassigned (Cn)
}

// truncateForDisplay keeps at most max UTF-16 code units (never splitting a
// surrogate pair) and reports how many were cut.
func truncateForDisplay(s string, max int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= max {
		return s
	}
	k := max
	if k > 0 && utf16.IsSurrogate(rune(u[k-1])) && u[k-1] < 0xDC00 {
		k-- // do not end on a high surrogate
	}
	return string(utf16.Decode(u[:k])) + "\u2026 [+" + strconv.Itoa(len(u)-k) + " chars]"
}

// -----------------------------------------------------------------------------
// low-level match helpers
// -----------------------------------------------------------------------------

// opApplies reports whether a rule scoped to ops applies to op (empty = all).
func opApplies(ops []Operation, op Operation) bool {
	if len(ops) == 0 {
		return true
	}
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

// foldCase folds case: lowercase, plus the dotless-i and long-s normalizations
// (ı -> i, ſ -> s).
func foldCase(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "ı", "i")
	s = strings.ReplaceAll(s, "ſ", "s")
	return s
}

// joinSlash joins a root and a relative dir with forward slashes, without the
// lexical cleaning that would collapse a trailing separator. An empty rel
// returns root unchanged.
func joinSlash(root, rel string) string {
	root = strings.TrimSuffix(toSlashPath(root), "/")
	if root == "" {
		root = "/"
	}
	rel = strings.Trim(toSlashPath(rel), "/")
	if rel == "" {
		return root
	}
	if root == "/" {
		return "/" + rel
	}
	return root + "/" + rel
}

// relUnder returns p made relative to root when p is root or lies beneath it;
// ok is false when p is not under root (path.relative, with the
// "outside the root" cases — a null or ".."-leading result — rejected).
func relUnder(root, p string) (string, bool) {
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		root = "/"
	}
	if p == root {
		return "", true
	}
	if root == "/" {
		return strings.TrimPrefix(p, "/"), true
	}
	if strings.HasPrefix(p, root+"/") {
		return p[len(root)+1:], true
	}
	return "", false
}

// globMatch reports whether the slash path rel matches the gitignore-ish
// pattern pat. "*" and "?" match within a single segment; "**" (as a whole
// segment) matches any number of segments. Matching is segment-wise and anchored
// at both ends.
func globMatch(pat, rel string) bool {
	if pat == "" {
		return rel == ""
	}
	return segMatch(strings.Split(pat, "/"), strings.Split(rel, "/"))
}

// segMatch matches pattern segments against path segments, treating a "**"
// pattern segment as matching zero or more path segments.
func segMatch(pat, seg []string) bool {
	if len(pat) == 0 {
		return len(seg) == 0
	}
	if pat[0] == "**" {
		// "**" matches zero or more segments.
		for i := 0; i <= len(seg); i++ {
			if segMatch(pat[1:], seg[i:]) {
				return true
			}
		}
		return false
	}
	if len(seg) == 0 {
		return false
	}
	if !oneSegMatch(pat[0], seg[0]) {
		return false
	}
	return segMatch(pat[1:], seg[1:])
}

// oneSegMatch matches a single pattern segment (with "*"/"?" wildcards) against
// a single path segment.
func oneSegMatch(pat, seg string) bool {
	// Fast path: no wildcard.
	if !strings.ContainsAny(pat, "*?") {
		return pat == seg
	}
	return wildMatch(pat, seg)
}

// wildMatch is a classic iterative "*"/"?" glob matcher over a single segment.
func wildMatch(pat, s string) bool {
	px, sx := 0, 0
	star, mark := -1, 0
	for sx < len(s) {
		if px < len(pat) && (pat[px] == '?' || pat[px] == s[sx]) {
			px++
			sx++
			continue
		}
		if px < len(pat) && pat[px] == '*' {
			star = px
			mark = sx
			px++
			continue
		}
		if star != -1 {
			px = star + 1
			mark++
			sx = mark
			continue
		}
		return false
	}
	for px < len(pat) && pat[px] == '*' {
		px++
	}
	return px == len(pat)
}
