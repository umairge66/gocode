package configimport

import (
	"regexp"
	"strings"
)

// Shell-exec safety gate.
//
// Gocode runs `!`-prefixed shell markers in command / skill / rule bodies. In
// Codex and Cursor those same markers are inert text, and in Gemini the shell
// syntax differs. So before mapping a body the importer checks whether importing
// it would silently turn inert text into a live shell execution, or let typed
// `$ARGUMENTS` re-pair into one. When it would, the item is refused (becomes
// unmappable / skipped) rather than mapped.

var (
	// argPlaceholder matches Gocode argument placeholders: $ARGUMENTS[n],
	// $ARGUMENTS, or $<digits> not followed by a word char.
	argPlaceholder = regexp.MustCompile(`\$ARGUMENTS\[\d+\]|\$ARGUMENTS|\$\d+`)
	// precededByTextClass is the set of chars after which a '!' is "ordinary
	// punctuation", not a shell-exec marker.
	precededByTextClass = regexp.MustCompile(`[a-zA-RT-Z_).,;:'"?!>-]`)
	// dollarWord masks $-words when scanning for dangling '!'.
	dollarWord = regexp.MustCompile(`\$[\p{L}\p{N}_.-]+`)
)

// countArgPlaceholders returns the number of $ARGUMENTS/$n placeholders, honoring
// the "$<digits> not followed by a word char" rule.
func countArgPlaceholders(s string) int {
	n := 0
	for _, loc := range argPlaceholder.FindAllStringIndex(s, -1) {
		m := s[loc[0]:loc[1]]
		if strings.HasPrefix(m, "$ARGUMENTS") {
			n++
			continue
		}
		// $<digits>: reject if the next char is a word char.
		end := loc[1]
		if end < len(s) && isWordByte(s[end]) {
			continue
		}
		n++
	}
	return n
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// stripSafeMarkers removes `!`...“ markers that sit at start-of-line or after
// whitespace (the well-formed shell-exec form).
func stripSafeMarkers(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '!' && atMarkerBoundary(s, i) && i+1 < len(s) && s[i+1] == '`' {
			// find closing backtick
			j := strings.IndexByte(s[i+2:], '`')
			if j != -1 {
				i = i + 2 + j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// atMarkerBoundary reports whether position i is at line start or right after a
// whitespace char.
func atMarkerBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	p := s[i-1]
	return p == ' ' || p == '\t' || p == '\n' || p == '\r' || p == '\f' || p == '\v'
}

// hasDanglingMarker reports whether s contains a '!' at a marker boundary
// immediately followed by '`' or '$'.
func hasDanglingMarker(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '!' && atMarkerBoundary(s, i) && i+1 < len(s) && (s[i+1] == '`' || s[i+1] == '$') {
			return true
		}
	}
	return false
}

// hasDanglingBang reports a '!' not preceded by a char in
// precededByTextClass, after masking $-words to spaces.
func hasDanglingBang(n string) bool {
	masked := dollarWord.ReplaceAllStringFunc(n, func(m string) string {
		return strings.Repeat(" ", len(m))
	})
	for i := 0; i < len(n); i++ {
		if n[i] != '!' {
			continue
		}
		var prev string
		if i-1 >= 0 && i-1 < len(masked) {
			prev = string(masked[i-1])
		}
		if !precededByTextClass.MatchString(prev) {
			return true
		}
	}
	return false
}

// validateArgPlaceholders returns a reason the
// body must be ported manually, or "" when it is safe to import. `blocks` are
// the shell-block command strings associated with the body (empty for most
// sources; populated for Gemini's translated !{…} blocks).
func validateArgPlaceholders(n string, blocks []string) string {
	r := stripSafeMarkers(n)
	if hasDanglingMarker(r) {
		return "Contains a dangling shell-exec marker that argument substitution could complete into a live command at invocation time. Port it manually."
	}
	if countArgPlaceholders(n) == 0 {
		return ""
	}
	var mixes bool
	if r == n {
		mixes = hasDanglingBang(n)
	} else {
		mixes = strings.Contains(r, "!")
	}
	if strings.Contains(r, "`") || mixes {
		return "Mixes argument placeholders with backticks or '!' outside its shell blocks — argument substitution at invocation time could assemble a live shell-exec marker from them. Port it manually."
	}
	for _, s := range blocks {
		if strings.Contains(s, "!") {
			return "One of its shell blocks contains '!' while the command also has argument placeholders — a backtick in the typed arguments could re-pair the marker into executing text the block never consented to. Port it manually."
		}
	}
	for _, s := range blocks {
		if countArgPlaceholders(s) > 0 {
			return "One of its shell blocks contains an argument placeholder — Gemini shell-escapes `{{args}}` inside `!{…}`, gocode's `$ARGUMENTS` substitution doesn't, so importing would let typed arguments inject shell commands. Port it manually."
		}
	}
	return ""
}

// hasShellExecMarker reports whether a body carries a Gocode shell-exec marker:
// a ```! fenced block, or a `!`…“ marker at a marker boundary.
func hasShellExecMarker(n string) bool {
	if strings.Contains(n, "```!") {
		return true
	}
	if !strings.Contains(n, "!`") {
		return false
	}
	for i := 0; i < len(n); i++ {
		if n[i] != '!' || !atMarkerBoundary(n, i) || i+1 >= len(n) || n[i+1] != '`' {
			continue
		}
		j := strings.IndexByte(n[i+2:], '`')
		if j != -1 && j >= 0 { // non-empty content before closing backtick ([^`]+)
			if j >= 1 {
				return true
			}
		}
	}
	return false
}

// escapeHTMLComment neutralizes HTML-comment delimiters in untrusted text so it
// can't break out of an injected marker comment.
func escapeHTMLComment(n string) string {
	n = strings.ReplaceAll(n, "-->", "—>")
	n = strings.ReplaceAll(n, "<!--", "<!—")
	return n
}
