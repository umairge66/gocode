package pathguard

import (
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/pathclass"
)

// unc.go holds the full Windows UNC / network-share spelling detector
// and its NT-object-prefix helper.
//
// Go's regexp has no lookbehind, so the four separator-run patterns are
// hand-scanned below; every other pattern is a direct RE2 regexp.
// Note on the "(?:@(?:\d+|ssl))?(?:[\\/]|$|\s)" tails: the host class
// [^ \t\r\n\f\v\\/]+ already admits '@' and is greedy, and the run can only stop
// at a separator, whitespace or the end, so the tail always matches; the
// patterns reduce exactly to "the separator pair is followed by a host byte".

var (
	// uncFlagLead is /^-[A-Za-z0-9]/ (a leading short-flag cluster).
	uncFlagLead = regexp.MustCompile(`^-[A-Za-z0-9]`)
	// uncFlagStrip is /^(?:-[A-Za-z0-9]+)+/.
	uncFlagStrip = regexp.MustCompile(`^(?:-[A-Za-z0-9]+)+`)
	// uncAssignPrefix is a "--name=" / "-n=" option-assignment prefix.
	uncAssignPrefix = regexp.MustCompile(`^--?[A-Za-z0-9][\w-]*=`)
	// ntObjectPrefixAnywhere is a "\??\" object-manager prefix at the start
	// or after any non-word character (e.g. "x=\??\C:\..."), either separator.
	ntObjectPrefixAnywhere = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])[\\/]\?\?(?:[\\/]|$)`)
	uncSSLPort             = regexp.MustCompile(`(?i)@SSL@\d+`)
	uncPortSSL             = regexp.MustCompile(`(?i)@\d+@SSL`)
	uncDavWWWRoot          = regexp.MustCompile(`(?i)DavWWWRoot`)
	uncLeadingIPv4         = regexp.MustCompile(`^(?:\\\\|//)(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})[\\/]`)
	uncLeadingIPv6         = regexp.MustCompile(`^(?:\\\\|//)(\[[\da-fA-F:]+\])[\\/]`)
)

// IsUNCPathSpelling is the UNC classification without its platform gate: it
// reports whether e spells a path (or, when argForm, a command argument) that
// reaches a Windows network share or the NT object namespace. argForm
// adds the leading "\\" / "//" / "\??\" check, strips
// "-flag" clusters and "--name=" / "-n=" assignment prefixes and re-checks the
// remainder, and uses the looser single-separator mixed-slash patterns.
//
// Spellings caught (any OS-agnostic string): "\\host\share", "//host/share",
// "\\host@SSL\x", "\\host@8080\x", "@SSL@443", "@443@SSL", "DavWWWRoot",
// "\\1.2.3.4\x", "\\[::1]\x", "\??\C:\x" anywhere after a non-word byte, the
// mixed "/\host\x" and "\/host\x" forms, and all of these behind "-I" /
// "--include=" prefixes or embedded after "NAME=" or any other text.
func IsUNCPathSpelling(e string, argForm bool) bool {
	if argForm && pathclass.IsUNCOrDevicePath(e) {
		return true
	}
	if argForm && uncFlagLead.MatchString(e) {
		if w := uncFlagStrip.ReplaceAllString(e, ""); w != "" && IsUNCPathSpelling(w, true) {
			return true
		}
	}
	if argForm && uncAssignPrefix.MatchString(e) {
		w := e
		for uncAssignPrefix.MatchString(w) {
			w = w[strings.IndexByte(w, '=')+1:]
		}
		if w != "" && IsUNCPathSpelling(w, true) {
			return true
		}
	}
	if hasBackslashPairHost(e) {
		return true
	}
	if ntObjectPrefixAnywhere.MatchString(e) {
		return true
	}
	if hasSlashPairHost(e) {
		return true
	}
	if argForm {
		if hasSlashBackslashHostStrict(e) || hasBackslashSlashHostStrict(e) {
			return true
		}
	} else if hasSlashBackslashHost(e) || hasBackslashSlashHost(e) {
		return true
	}
	if uncSSLPort.MatchString(e) || uncPortSSL.MatchString(e) {
		return true
	}
	if uncDavWWWRoot.MatchString(e) {
		return true
	}
	return uncLeadingIPv4.MatchString(e) || uncLeadingIPv6.MatchString(e)
}

// uncHostByte reports whether c belongs to the host class
// [^ \t\r\n\f\v\\/]. A UTF-8 lead/continuation byte (>= 0x80) is a member, as
// the corresponding UTF-16 unit is.
func uncHostByte(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\f', '\v', '\\', '/':
		return false
	}
	return true
}

// jsWordByte reports membership in the ASCII \w class.
func jsWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || isASCIILetter(c)
}

// lookbehindOK is (?<![:\w]) at index i.
func lookbehindOK(s string, i int) bool {
	return i == 0 || (s[i-1] != ':' && !jsWordByte(s[i-1]))
}

// hasBackslashPairHost: /\\\\[^ \t\r\n\f\v\\/]+…/ — "\\" followed by a host byte.
func hasBackslashPairHost(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '\\' && s[i+1] == '\\' && uncHostByte(s[i+2]) {
			return true
		}
	}
	return false
}

// hasSlashPairHost: /(?<!:)\/\/[^ \t\r\n\f\v\\/]+…/ — "//" not preceded by ':'
// (so "https://x" is not a share) followed by a host byte.
func hasSlashPairHost(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '/' && s[i+1] == '/' && (i == 0 || s[i-1] != ':') && uncHostByte(s[i+2]) {
			return true
		}
	}
	return false
}

// hostRunThenSep reports whether s[j:] starts with one or more host bytes
// followed by a separator (…[^ \t\r\n\f\v\\/]+[\\/]).
func hostRunThenSep(s string, j int) bool {
	k := j
	for k < len(s) && uncHostByte(s[k]) {
		k++
	}
	return k > j && k < len(s) && isSep(s[k])
}

// hasSlashBackslashHostStrict: /(?<![:\w])\/\\{1,}[^…]+[\\/]/.
func hasSlashBackslashHostStrict(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '/' || !lookbehindOK(s, i) {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] == '\\' {
			j++
		}
		if j > i+1 && hostRunThenSep(s, j) {
			return true
		}
	}
	return false
}

// hasSlashBackslashHost: /\/\\{2,}[^…]/.
func hasSlashBackslashHost(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '/' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] == '\\' {
			j++
		}
		if j-(i+1) >= 2 && j < len(s) && uncHostByte(s[j]) {
			return true
		}
	}
	return false
}

// hasBackslashSlashHostStrict: /(?<![:\w])\\{1,}\/[^…]+[\\/]/. The match may
// start anywhere inside a backslash run, so a run qualifies when any of its
// backslashes passes the lookbehind (in practice: run length >= 2, or a
// single backslash not preceded by ':' or a word byte).
func hasBackslashSlashHostStrict(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || !lookbehindOK(s, i) {
			continue
		}
		j := i
		for j < len(s) && s[j] == '\\' {
			j++
		}
		if j < len(s) && s[j] == '/' && hostRunThenSep(s, j+1) {
			return true
		}
	}
	return false
}

// hasBackslashSlashHost: /\\{2,}\/[^…]/.
func hasBackslashSlashHost(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		j := i
		for j < len(s) && s[j] == '\\' {
			j++
		}
		if j-i >= 2 && j+1 < len(s) && s[j] == '/' && uncHostByte(s[j+1]) {
			return true
		}
		i = j - 1
	}
	return false
}
