package commands

import (
	"regexp"
	"strings"
)

// ShellDisabledPlaceholder replaces every `!`-block of a command or skill when
// the disableSkillShellExecution setting is in effect: nothing runs and the
// model sees this text where the block was.
const ShellDisabledPlaceholder = "[shell command execution disabled by policy]"

// disabledFencedBlock is the fenced ```! ... ``` form as the placeholder pass
// matches it (unlike ExtractShellBlocks it also replaces an empty block).
var disabledFencedBlock = regexp.MustCompile("(?s)```!\\s*\\n?.*?\\n?```")

// inlineCodeSpan matches a single-line `code` span.
var inlineCodeSpan = regexp.MustCompile("`[^`\\n]+`")

// ReplaceShellBlocks replaces every `!`-block in body — fenced and inline —
// with placeholder, running nothing. It is the no-execution pass used
// when shell execution is disabled by policy.
func ReplaceShellBlocks(body, placeholder string) string {
	out := disabledFencedBlock.ReplaceAllLiteralString(body, placeholder)
	if !strings.Contains(out, "!`") {
		return out
	}
	spans := inlineShellSpans(maskCodeSpans(out))
	for i := len(spans) - 1; i >= 0; i-- {
		out = out[:spans[i][0]] + placeholder + out[spans[i][1]:]
	}
	return out
}

// maskCodeSpans blanks the inside of every single-line `code` span that is not
// itself a !`cmd` block (or preceded by another backtick), keeping byte offsets, so a "!`"
// inside ordinary inline code is not mistaken for a shell block.
func maskCodeSpans(s string) string {
	b := []byte(s)
	for _, m := range inlineCodeSpan.FindAllStringIndex(s, -1) {
		if m[0] > 0 && (s[m[0]-1] == '!' || s[m[0]-1] == '`') {
			continue
		}
		for i := m[0] + 1; i < m[1]-1; i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

// inlineShellSpans returns the [start,end) byte spans of the inline !`cmd`
// blocks in s: "!" at the start of a line or after whitespace, then a
// backtick-delimited non-empty body (which may span lines). RE2 has no
// lookbehind, so the boundary is checked by hand.
func inlineShellSpans(s string) [][2]int {
	var out [][2]int
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '!' || s[i+1] != '`' {
			continue
		}
		if i > 0 && !isShellBoundary(s[i-1]) {
			continue
		}
		end := strings.IndexByte(s[i+2:], '`')
		if end <= 0 { // no closing backtick, or an empty body
			continue
		}
		stop := i + 2 + end + 1
		out = append(out, [2]int{i, stop})
		i = stop - 1
	}
	return out
}

// isShellBoundary reports whether c may precede an inline `!` marker (JS \s,
// ASCII subset; a newline covers the /m start-of-line case).
func isShellBoundary(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// ShellPolicyApplies reports whether the disableSkillShellExecution setting
// covers a command from src: user, project and local markdown commands and
// plugin commands/skills are covered; builtin and bundled ones are not. (Commands
// and skills that managed policy itself ships — source "policySettings" —
// would be exempt too; no command is loaded from that source.)
func ShellPolicyApplies(src Source) bool {
	switch src {
	case SourceUser, SourceProject, SourceLocal, SourcePlugin:
		return true
	}
	return false
}
