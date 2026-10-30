package redact

import (
	"regexp"
	"strings"
)

// maxFreeTextScan bounds how much input the free-text scanner inspects.
const maxFreeTextScan = 8192

// ---------------------------------------------------------------------------
// Regex tables (RE2 forms).
// ---------------------------------------------------------------------------

// webhookURLRe matches Slack / Discord / Teams incoming-webhook URLs. No
// look-around needed.
var webhookURLRe = regexp.MustCompile(`(?i)https://(?:hooks\.slack\.com/(?:services|workflows|triggers)/|(?:ptb\.|canary\.)?discord(?:app)?\.com/api/webhooks/\d+/|[\w.-]+\.webhook\.office\.com/webhookb2/)[\w/@.~-]{16,}`)

// packageRefRe is the allow-list of version/digest-pinned package references
// that must NOT be treated as credentials. No look-around needed.
var packageRefRe = regexp.MustCompile(`(?i)^(?:[\w.-]+(?::[\w.-]+)?@sha(?:256|384|512):[0-9a-f]{32,}|npm:(?:@[\w.-]+/)?[\w.-]+@[\w.^~<>=*|+-]*|[a-z_][\w.-]*:[\w.-]+:v?\d[\w.+\[\](),-]*@(?:jar|war|ear|aar|apk|aab|pom|zip|tar|tgz|module|klib|exe|dll|so|dylib)?)$`)

// privateKeyMarkerRe matches a PEM private-key BEGIN marker anywhere in text.
var privateKeyMarkerRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`)

// hostStrictRe recognises an authority that looks like a real host
// (optionally with port) followed by a path/query/fragment or end. The
// look-aheads for "TLD contains a letter" and "host immediately followed by
// :port" are rewritten to equivalent consuming RE2 forms:
//   - (?=[a-z0-9-]*[a-z])[a-z0-9-]+  ==>  [a-z0-9-]*[a-z][a-z0-9-]*
//   - [a-z0-9_.-]+(?=:\d+(?:[/?#]|$)) ==>  split into its own :port alternative
var hostStrictRe = regexp.MustCompile(`(?i)^(?:(?:localhost|[a-z0-9_-]+(?:\.[a-z0-9_-]+)*\.[a-z0-9-]*[a-z][a-z0-9-]*|\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-f:.]+\])(?::\d+)?(?:[/?#]|$)|[a-z0-9_.-]+:\d+(?:[/?#]|$))`)

// hostLooseRe is the looser host recogniser. The look-ahead
// (?=[a-z0-9._-]*[a-z]) ("contains a letter") is rewritten to the equivalent
// [a-z0-9._-]*[a-z][a-z0-9._-]*.
var hostLooseRe = regexp.MustCompile(`(?i)^(?:[a-z0-9._-]*[a-z][a-z0-9._-]*|\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-f:.]+\])(?::\d+)?(?:[/?#]|$)`)

// sensitiveAssignCoreRe matches `<...key...> = <value>` assignments
// whose key names a credential. Two negative look-aheads
// that RE2 cannot express are handled by post-filters in matchesSensitiveAssign:
//   - (?!-*jobserver-auth\s*[=:])   — exclude the make jobserver-auth key
//   - (?!(?:true|false|...)(?:$|...)) — exclude boolean/placeholder values
//
// Group 1 captures the value.
var sensitiveAssignCoreRe = regexp.MustCompile(`(?i)(?:(?:^|[;&?#,{]|/:)\s*|\s)["']?(?:[a-z0-9_.-]{0,64}(?:password|passwd|pwd|secret|token|(?:account|access|api|private|subscription)[-_]?key|signature|sig|credential)|[a-z0-9_.-]{0,63}[_.-]auth)["']?\s*[=:]\s*["']?([^;&,\s"']+)`)

// assignValueStopwords are the placeholder values the second look-ahead excludes.
var assignValueStopwords = map[string]bool{
	"true": true, "false": true, "none": true, "null": true, "yes": true,
	"no": true, "on": true, "off": true, "enabled": true, "disabled": true,
	"required": true, "optional": true,
}

// matchesSensitiveAssign applies sensitiveAssignCoreRe plus its two negative
// look-ahead post-filters.
func matchesSensitiveAssign(s string) bool {
	for _, m := range sensitiveAssignCoreRe.FindAllStringSubmatchIndex(s, -1) {
		matched := s[m[0]:m[1]]
		if strings.Contains(strings.ToLower(matched), "jobserver-auth") {
			continue // (?!-*jobserver-auth\s*[=:])
		}
		if m[2] >= 0 {
			val := strings.ToLower(s[m[2]:m[3]])
			if assignValueStopwords[val] {
				continue // (?!(?:true|false|...)(?:$|[;&,\s"']))
			}
		}
		return true
	}
	return false
}

// sonarLoginRe, authorizationRe: ContainsSecret patterns with no look-around.
var (
	sonarLoginRe    = regexp.MustCompile(`["']?sonar\.login["']?\s*[:=]\s*["']?[^\s"',}]{8,}`)
	authorizationRe = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*["']?[a-z][a-z0-9_-]*\s+[A-Za-z0-9._~+/=-]{8,}`)
)

// looksLikeCredential recognises a userinfo value that looks like a credential.
var credentialPrefixRe = regexp.MustCompile(`^(?:gh[opusr]_|github_pat_|glpat-|xox[abpr]-|sk-|pk-|AKIA|eyJ|ya29\.|npm_)`)
var credentialDigitRe = regexp.MustCompile(`[0-9]`)
var credentialLetterRe = regexp.MustCompile(`(?i)[a-z]`)

func looksLikeCredential(e string) bool {
	if credentialPrefixRe.MatchString(e) {
		return true
	}
	return len([]rune(e)) >= 20 && credentialDigitRe.MatchString(e) && credentialLetterRe.MatchString(e)
}

// looksLikeHostPort reports whether `e` (the text before the
// last '@') is really a `host:port/path` authority rather than credentials, so
// the caller should NOT flag it as a secret.
var (
	hostPortRe    = regexp.MustCompile(`(?is)^([a-z0-9_-]+(?:\.[a-z0-9_-]+)*):\d+([/?#].*)$`)
	queryCharRe   = regexp.MustCompile(`[?#&]`)
	versionLikeRe = regexp.MustCompile(`(?i)^(?:v?\d|sha\d*:)`)
)

func looksLikeHostPort(e, n string) bool {
	m := hostPortRe.FindStringSubmatch(e)
	if m == nil {
		return false
	}
	if queryCharRe.MatchString(m[2]) {
		return true
	}
	if hostStrictRe.MatchString(n) {
		return false
	}
	if strings.HasSuffix(e, "/") {
		return true
	}
	if versionLikeRe.MatchString(n) {
		return true
	}
	return !hostLooseRe.MatchString(n)
}

// ---------------------------------------------------------------------------
// Hand-written scanners for the RE2-incompatible ContainsSecret patterns.
// ---------------------------------------------------------------------------

func isTokenChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
		b == '_' || b == '~' || b == '+' || b == '=' || b == '-'
}

// matchesTokenHeader implements the pattern
//
//	/\b(?:[Tt]oken|TOKEN)\s+(?=[...]*[0-9])(?=[...]*[A-Z])(?=([...]{20,}))\1(?![\/.])/
//
// It finds a "Token " header followed by a run of [A-Za-z0-9_~+=-] that is at
// least 20 chars, contains a digit and an uppercase letter, and is not followed
// by '/' or '.'. The back-reference \1 and look-aheads are not expressible in
// RE2, so this is a small hand scanner. (The look-aheads only search the same
// character class as the run, so "the maximal run has a digit and an upper"
// is exact; \1 then consumes that whole run, so the (?![\/.]) test applies to
// the character after it.)
func matchesTokenHeader(s string) bool {
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(?:[Tt]oken|TOKEN)` + jsSpace + `+`)
	for _, loc := range re.FindAllStringIndex(s, -1) {
		i := loc[1]
		j := i
		hasDigit, hasUpper := false, false
		for j < len(s) && isTokenChar(s[j]) {
			c := s[j]
			if c >= '0' && c <= '9' {
				hasDigit = true
			}
			if c >= 'A' && c <= 'Z' {
				hasUpper = true
			}
			j++
		}
		if j-i >= 20 && hasDigit && hasUpper {
			if j >= len(s) || (s[j] != '/' && s[j] != '.') {
				return true
			}
		}
	}
	return false
}

// matchesAuthScheme implements the Bearer/Basic pattern
//
//	/\b(?:Bearer|Basic)\s+(?=<alt1>|<alt2>|<alt3>)[A-Za-z0-9._~+\/=-]{8,}/
//
// where the look-ahead requires the value to contain a digit, a special char in
// {_ ~ + / =}, or two camelCase humps ([a-z][A-Z] whose upper is not followed
// by a lower-case letter). Every look-ahead scans only the value class, so each
// alternative is a property of the maximal value run, checked by hand here.
func matchesAuthScheme(s string) bool {
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(?:Bearer|Basic)` + jsSpace + `+`)
	for _, loc := range re.FindAllStringIndex(s, -1) {
		i := loc[1]
		j := i
		hasDigit, hasSpecial := false, false
		for j < len(s) {
			c := s[j]
			if !(isAuthValChar(c)) {
				break
			}
			if c >= '0' && c <= '9' {
				hasDigit = true
			}
			if c == '_' || c == '~' || c == '+' || c == '/' || c == '=' {
				hasSpecial = true
			}
			j++
		}
		humps := 0
		for k := i; k+1 < j && humps < 2; k++ {
			if isLower(s[k]) && s[k+1] >= 'A' && s[k+1] <= 'Z' && (k+2 >= len(s) || !isLower(s[k+2])) {
				humps++
				k++
			}
		}
		if j-i >= 8 && (hasDigit || hasSpecial || humps >= 2) {
			return true
		}
	}
	return false
}

func isLower(b byte) bool { return b >= 'a' && b <= 'z' }

// jsSpace is a character class for JavaScript's \s: RE2's \s plus \v and
// the Unicode space separators, NBSP, BOM and the line/paragraph separators.
const jsSpace = `[\s\v\x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

func isAuthValChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
		b == '.' || b == '_' || b == '~' || b == '+' || b == '/' || b == '=' || b == '-'
}

// scpLikeRe matches the scheme-less `user:...@host` form. The two negative
// look-aheads after the first ':' ((?!//) and (?![\\/])) are emulated by the
// post-check in matchesScpLike (the char after the colon must not be '/' or '\').
var scpLikeRe = regexp.MustCompile(`^[^\s/@:]+:([^\s@]*)@[^\s/@]+`)
var winDrivePathRe = regexp.MustCompile(`(?i)^[a-z]:[\\/]`)
var mailtoLikeRe = regexp.MustCompile(`(?i)^(?:mailto|sips?|xmpp|im|acct):`)
var schemeCharRe = regexp.MustCompile(`(?i)[a-z0-9+.-]`)
var whitespaceRe = regexp.MustCompile(`\s`)

func matchesScpLike(n string) bool {
	loc := scpLikeRe.FindStringSubmatchIndex(n)
	if loc == nil {
		return false
	}
	// The colon is just before group 1 (which may be empty).
	colon := loc[2] - 1
	if colon+1 < len(n) {
		c := n[colon+1]
		if c == '/' || c == '\\' {
			return false // (?!//) / (?![\\/])
		}
	}
	return true
}

// firstWhitespace returns the byte index of the first whitespace rune in s, or
// len(s) if none (like a /\s|$/ search).
func firstWhitespace(s string) int {
	if loc := whitespaceRe.FindStringIndex(s); loc != nil {
		return loc[0]
	}
	return len(s)
}

func lastIndexAtOrBefore(s, sub string, from int) int {
	if from < 0 {
		return -1
	}
	if from > len(s) {
		from = len(s)
	}
	// Like lastIndexOf(sub, from): a match may start at any index <= from.
	end := from + len(sub)
	if end > len(s) {
		end = len(s)
	}
	return strings.LastIndex(s[:end], sub)
}

// ContainsSecret reports whether free-form text appears to contain a credential:
// a `user:pass@host` URL (with or without scheme), a credential assignment, a
// webhook URL, a PEM private key, or an Authorization/Bearer/Token header.
//
// Patterns that need RE2-incompatible features (look-around, back-references)
// are handled by the rewritten regex tables above and the hand-written header
// scanners.
func ContainsSecret(e string) bool {
	n := e
	if r := []rune(e); len(r) > maxFreeTextScan {
		n = string(r[:maxFreeTextScan])
	}

	// Does the authority starting at S carry userinfo credentials?
	authorityHasCreds := func(S string) bool {
		h := firstWhitespace(S)
		y := S[:h]
		R := strings.LastIndex(y, "@")
		if R == -1 {
			return false
		}
		A := y[:R]
		if looksLikeHostPort(A, y[R+1:]) {
			return false
		}
		if strings.Contains(A, ":") {
			return true
		}
		return !strings.ContainsAny(A, "/?#") && looksLikeCredential(A)
	}

	// Scan each "scheme://" occurrence.
	for s := strings.Index(n, "://"); s != -1; {
		if s > 0 && schemeCharRe.MatchString(string(n[s-1])) {
			if authorityHasCreds(n[s+3:]) {
				return true
			}
		}
		rest := strings.Index(n[s+3:], "://")
		if rest == -1 {
			break
		}
		s = s + 3 + rest
	}
	if strings.HasPrefix(n, "//") && authorityHasCreds(n[2:]) {
		return true
	}

	// scp-like `user:pass@host` branch (&&-chained guards).
	if matchesScpLike(n) && !winDrivePathRe.MatchString(n) && !packageRefRe.MatchString(n) && !mailtoLikeRe.MatchString(n) {
		w := firstWhitespace(n)
		at := lastIndexAtOrBefore(n, "@", w)
		if at >= 0 {
			if !looksLikeHostPort(n[:at], n[at+1:]) {
				return true
			}
		} else if !looksLikeHostPort("", n) {
			return true
		}
	}

	return matchesSensitiveAssign(n) ||
		webhookURLRe.MatchString(n) ||
		sonarLoginRe.MatchString(n) ||
		privateKeyMarkerRe.MatchString(n) ||
		authorizationRe.MatchString(n) ||
		matchesTokenHeader(n) ||
		matchesAuthScheme(n)
}
