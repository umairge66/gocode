package commands

import (
	"fmt"
	"regexp"
	"strings"
)

// maxExpandedLen is the ceiling on a command/skill/hook prompt plus the
// arguments substituted into it.
const maxExpandedLen = 4_000_000

// maxNamedArgs / maxNamedArgLen bound the named-placeholder table.
const (
	maxNamedArgs   = 64
	maxNamedArgLen = 64
)

// ExpandOptions controls argument substitution.
type ExpandOptions struct {
	// ArgNames are named positional placeholders (frontmatter `arguments`).
	// `$name` substitutes the positional token at the name's index: the first
	// declared name maps to token[0], the second to token[1], and so on.
	ArgNames []string
	// Append, when true, appends "\n\nARGUMENTS: <args>" if the template used
	// no placeholder at all (so typed arguments are never silently dropped).
	Append bool
	// Transform is applied to every substituted value (used to shell-escape
	// values so they cannot inject into `!`-command blocks). nil = identity.
	Transform func(string) string
}

// Expand substitutes argument placeholders in a command's prompt template.
//
// Placeholders, in resolution order:
//
//   - `$name`          — a named placeholder (not followed by '[' or a word
//     character); substitutes token[index-of-name].
//   - `$ARGUMENTS[n]`  — the n-th whitespace/shell token (0-based). Out of
//     range leaves the literal text untouched.
//   - `$N`             — `$0`,`$1`,… the N-th token (0-based, same indexing as
//     `$ARGUMENTS[N]`). NOTE: `$1` is the
//     SECOND token; `$0`/`$ARGUMENTS[0]` is the first. Out of range is left
//     literal.
//   - `$ARGUMENTS`     — the entire (untokenized) argument string.
//
// A placeholder preceded by a backslash (`\$ARGUMENTS`) is emitted literally
// (the escaping backslash is consumed). If no placeholder matched and Append is
// set and args is non-empty, "\n\nARGUMENTS: <args>" is appended.
//
// Because this is a single left-to-right pass, substituted values are never
// re-scanned; no sentinel wrapping is needed.
func Expand(template, args string, opts ExpandOptions) (string, error) {
	transform := opts.Transform
	if transform == nil {
		transform = func(s string) string { return s }
	}

	// Named-placeholder table: non-empty names <= maxNamedArgLen, capped at
	// maxNamedArgs, longest first so a longer name wins over a prefix.
	var names []argName
	for i, n := range opts.ArgNames {
		if n == "" || len(n) > maxNamedArgLen {
			continue
		}
		names = append(names, argName{name: n, idx: i})
		if len(names) >= maxNamedArgs {
			break
		}
	}
	// stable sort by descending name length.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && len(names[j].name) > len(names[j-1].name); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}

	tokens := tokenizeArgs(args)

	total := len(template)
	var overflow bool
	substituted := false
	// sub records a substitution's value through the transform + length budget.
	sub := func(v string) string {
		v = transform(v)
		total += len(v)
		if total > maxExpandedLen {
			overflow = true
		}
		substituted = true
		return v
	}

	var b strings.Builder
	b.Grow(len(template))
	i := 0
	n := len(template)
	for i < n {
		c := template[i]
		// \$placeholder -> literal $placeholder (consume the backslash).
		if c == '\\' && i+1 < n && template[i+1] == '$' {
			if isPlaceholderStart(template[i+1:], names) {
				b.WriteByte('$')
				i += 2
				continue
			}
		}
		if c == '$' {
			if repl, adv, ok := expandAt(template[i:], names, tokens, args, sub); ok {
				b.WriteString(repl)
				i += adv
				continue
			}
		}
		b.WriteByte(c)
		i++
	}

	out := b.String()
	if overflow {
		return "", fmt.Errorf("commands: expanded prompt exceeds %d characters; use shorter arguments or fewer placeholders such as $ARGUMENTS", maxExpandedLen)
	}
	if !substituted && opts.Append && args != "" {
		out += "\n\nARGUMENTS: " + transform(args)
	}
	return out, nil
}

// argName is one named positional placeholder and the token index it maps to.
type argName struct {
	name string
	idx  int
}

// isPlaceholderStart reports whether s (which begins with '$') starts with a
// recognised placeholder token. Used by the backslash-escape check so that only
// a real placeholder is "un-escaped" by a preceding backslash.
func isPlaceholderStart(s string, names []argName) bool {
	_, _, ok := expandAt(s, names, nil, "", func(string) string { return "" })
	return ok
}

// expandAt attempts to expand a placeholder at the start of s (s[0]=='$'),
// returning the replacement text and the number of bytes consumed from s.
func expandAt(s string, names []argName, tokens []string, args string, sub func(string) string) (string, int, bool) {
	// 1. named placeholder: $name not followed by '[' or a word char.
	for _, nm := range names {
		if len(s) >= 1+len(nm.name) && s[1:1+len(nm.name)] == nm.name {
			after := s[1+len(nm.name):]
			if !startsWithBracketOrWord(after) {
				// A named placeholder always substitutes (an undefined
				// token yields the empty string and still marks the template
				// as having used a placeholder).
				if nm.idx < len(tokens) {
					return sub(tokens[nm.idx]), 1 + len(nm.name), true
				}
				return sub(""), 1 + len(nm.name), true
			}
		}
	}
	// 2. $ARGUMENTS[n]
	if strings.HasPrefix(s, "$ARGUMENTS[") {
		rest := s[len("$ARGUMENTS["):]
		digits := leadingDigits(rest)
		if digits != "" && len(rest) > len(digits) && rest[len(digits)] == ']' {
			consumed := len("$ARGUMENTS[") + len(digits) + 1
			idx := atoi(digits)
			if idx < len(tokens) {
				return sub(tokens[idx]), consumed, true
			}
			// out of range: leave the literal "$ARGUMENTS[n]".
			return s[:consumed], consumed, true
		}
	}
	// 3. $N (digits) not followed by a word char.
	if len(s) > 1 && isDigit(s[1]) {
		digits := leadingDigits(s[1:])
		after := s[1+len(digits):]
		if !startsWithWord(after) {
			consumed := 1 + len(digits)
			idx := atoi(digits)
			if idx < len(tokens) {
				return sub(tokens[idx]), consumed, true
			}
			// out of range: leave literal "$N".
			return s[:consumed], consumed, true
		}
	}
	// 4. $ARGUMENTS (whole string).
	if strings.HasPrefix(s, "$ARGUMENTS") {
		return sub(args), len("$ARGUMENTS"), true
	}
	return "", 0, false
}

func startsWithBracketOrWord(s string) bool {
	if s == "" {
		return false
	}
	return s[0] == '[' || isWord(s[0])
}

func startsWithWord(s string) bool {
	return s != "" && isWord(s[0])
}

func isWord(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i]
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// tokenizeArgs splits an argument string into positional tokens: it tokenizes
// shell-style and falls back to a plain `/\s+/` split when that yields
// nothing.
//
// Reference behavior: the argument string is parsed as a bash command line
// with a tree-sitter grammar, the first command node is located (descending
// through pipelines / redirections / variable assignments), and that
// command's argument words are emitted, UNQUOTING each one:
//
//   - a word has its backslash escapes removed (`/\\(.)/` -> the escaped char);
//   - a double-quoted "string" / single-quoted 'raw_string' has its surrounding
//     quotes stripped;
//   - adjacent pieces (`foo"bar"`) concatenate into one token;
//   - emission STOPS at a command-substitution / expansion node — i.e. a token
//     containing `$(...)`, backticks or `${...}` terminates the token list.
//
// This implementation is a quote-aware splitter (single quotes, double quotes
// and backslash escapes, with concatenation) that reproduces that unquoting for
// the common cases, then falls back to whitespace splitting.
//
// Documented approximation / unsupported edges (vs the full tree-sitter path):
//   - Command substitution is NOT executed here and does NOT truncate the token
//     list; a `$(...)` / backtick token is kept as literal text rather than
//     stopping emission as the tree-sitter path does. (Command substitution is executed
//     only by the shell when a `!`-block runs — see ExpandShellBlocks.)
//   - Redirections, pipelines and variable assignments embedded in the argument
//     string are not stripped; they are split as ordinary whitespace tokens.
//   - ANSI-C `$'...'` quoting is treated as a literal `$` followed by a raw
//     string rather than being C-unescaped.
//
// These cases do not arise for normal slash-command arguments; the splitter is
// exact for plain words and single/double-quoted spans.
func tokenizeArgs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var tokens []string
	var cur strings.Builder
	inToken := false
	var quote byte // 0, '\'' or '"'
	flush := func() {
		if inToken {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inToken = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\' || s[i+1] == '$' || s[i+1] == '`') {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inToken = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inToken = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			flush()
		default:
			cur.WriteByte(c)
			inToken = true
		}
	}
	flush()
	return tokens
}

// ShellEscape neutralises the `!`-shell-exec markers in a substituted value so
// that user arguments cannot inject shell commands into a command's `!`-blocks.
//
//	e.replace(/`!/g,"` !").replace(/!`/g,"! `").replace(/(^|\s)!/gm,"$1\\!")
func ShellEscape(s string) string {
	s = strings.ReplaceAll(s, "`!", "` !")
	s = strings.ReplaceAll(s, "!`", "! `")
	// Escape '!' at start-of-line or after whitespace. Go regexp lacks the
	// multiline (^|\s) capture rewrite cleanly, so scan manually.
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '!' {
			atBoundary := i == 0 || s[i-1] == '\n' || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\r' || s[i-1] == '\f' || s[i-1] == '\v'
			if atBoundary {
				b.WriteString("\\!")
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ShellBlock is a `!`-prefixed shell command embedded in a command body. Its
// output is injected into the prompt before the body is sent to the model.
type ShellBlock struct {
	Raw     string // the full matched text, including the `!` markers
	Command string // the shell command, trimmed and CRLF-normalised
	At      int    // byte offset of the match in the body
}

// fencedShellBlock matches a ```! ... ``` fenced block.
var fencedShellBlock = regexp.MustCompile("(?s)```!\\s*\\n?(.*?)\\n?```")

// ExtractShellBlocks finds the `!`-prefixed shell blocks in a command body,
// both the fenced form (```! ... ```) and the inline form (!`cmd`).
//
// Executing these blocks — running each command and splicing its stdout into
// the prompt — is implemented by ExpandShellBlocks, which takes
// an injectable ShellRunner so the live-session wiring and the unit tests share
// one code path.
func ExtractShellBlocks(body string) []ShellBlock {
	var out []ShellBlock
	for _, m := range fencedShellBlock.FindAllStringSubmatchIndex(body, -1) {
		cmd := normalizeShellCommand(body[m[2]:m[3]])
		if cmd != "" {
			out = append(out, ShellBlock{Raw: body[m[0]:m[1]], Command: cmd, At: m[0]})
		}
	}
	// Inline !`cmd` at a start-of-line/whitespace boundary.
	for i := 0; i < len(body); i++ {
		if body[i] != '!' || i+1 >= len(body) || body[i+1] != '`' {
			continue
		}
		boundary := i == 0 || body[i-1] == '\n' || body[i-1] == ' ' || body[i-1] == '\t' || body[i-1] == '\r' || body[i-1] == '\f' || body[i-1] == '\v'
		if !boundary {
			continue
		}
		end := strings.IndexByte(body[i+2:], '`')
		if end < 0 {
			continue
		}
		raw := body[i : i+2+end+1]
		cmd := normalizeShellCommand(body[i+2 : i+2+end])
		if cmd != "" {
			out = append(out, ShellBlock{Raw: raw, Command: cmd, At: i})
		}
		i = i + 2 + end
	}
	return out
}

// normalizeShellCommand trims and normalises CRLF.
func normalizeShellCommand(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
}
