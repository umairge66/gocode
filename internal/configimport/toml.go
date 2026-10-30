package configimport

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TOML reader for Codex config.toml and Gemini command .toml files.
//
// Go's stdlib has no TOML and go.mod may not gain a dependency, so this is a
// stdlib-only recursive-descent reader over the whole document (not line
// based) covering everything real Codex / Gemini files use:
//
//   - comments, blank lines, CRLF line endings
//   - bare, quoted ("a.b") and dotted (a.b.c = 1) keys
//   - [table.path] and [[array.of.tables]] headers (quoted segments allowed)
//   - basic strings with every TOML escape (\b \t \n \f \r \" \\ \e \uXXXX
//     \UXXXXXXXX), literal strings, and both multi-line forms (leading newline
//     trimmed, line-ending backslash folding in """ strings)
//   - integers (with _ separators, 0x/0o/0b prefixes), floats (inf/nan too),
//     booleans
//   - arrays spanning multiple lines with comments and trailing commas
//   - inline tables { k = "v", nested.k = 1 } (the usual shape of a Codex
//     `env = { … }` / `http_headers = { … }`)
//
// Dates/times are kept as their literal text (the importer never reads them).
// Malformed input is an error, which the importers report as an unmappable
// "Could not parse" item — nothing is guessed.

// parseTOML parses a TOML document into nested map[string]any / []any values.
func parseTOML(input string) (map[string]any, error) {
	p := &tomlParser{s: strings.TrimPrefix(input, "\uFEFF")}
	return p.document()
}

type tomlParser struct {
	s string
	i int
}

func (p *tomlParser) errf(format string, a ...any) error {
	line := 1 + strings.Count(p.s[:min(p.i, len(p.s))], "\n")
	return fmt.Errorf("toml: line %d: %s", line, fmt.Sprintf(format, a...))
}

func (p *tomlParser) eof() bool { return p.i >= len(p.s) }

func (p *tomlParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *tomlParser) hasPrefix(pre string) bool { return strings.HasPrefix(p.s[p.i:], pre) }

// skipWS skips spaces and tabs on the current line.
func (p *tomlParser) skipWS() {
	for !p.eof() && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// skipComment skips a # comment up to (not including) the newline.
func (p *tomlParser) skipComment() {
	if p.peek() == '#' {
		for !p.eof() && p.s[p.i] != '\n' {
			p.i++
		}
	}
}

// skipBlank skips whitespace, newlines and comments (between statements and
// inside arrays).
func (p *tomlParser) skipBlank() {
	for !p.eof() {
		switch p.s[p.i] {
		case ' ', '\t', '\r', '\n':
			p.i++
		case '#':
			p.skipComment()
		default:
			return
		}
	}
}

// endOfLine requires only whitespace / a comment before the next newline.
func (p *tomlParser) endOfLine() error {
	p.skipWS()
	p.skipComment()
	if p.eof() {
		return nil
	}
	if p.hasPrefix("\r\n") {
		p.i += 2
		return nil
	}
	if p.s[p.i] == '\n' {
		p.i++
		return nil
	}
	return p.errf("unexpected %q after value", p.s[p.i])
}

func (p *tomlParser) document() (map[string]any, error) {
	root := map[string]any{}
	cur := root
	for {
		p.skipBlank()
		if p.eof() {
			return root, nil
		}
		if p.hasPrefix("[[") {
			p.i += 2
			path, err := p.keyPath("]]")
			if err != nil {
				return nil, err
			}
			tbl := map[string]any{}
			if err := appendArrayTable(root, path, tbl); err != nil {
				return nil, p.errf("%v", err)
			}
			cur = tbl
		} else if p.peek() == '[' {
			p.i++
			path, err := p.keyPath("]")
			if err != nil {
				return nil, err
			}
			t, err := descendTable(root, path)
			if err != nil {
				return nil, p.errf("%v", err)
			}
			cur = t
		} else {
			if err := p.keyValue(cur); err != nil {
				return nil, err
			}
		}
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

// keyPath reads a (dotted) key followed by the closing delimiter of a header.
func (p *tomlParser) keyPath(closer string) ([]string, error) {
	p.skipWS()
	path, err := p.dottedKey()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if !p.hasPrefix(closer) {
		return nil, p.errf("expected %q to close table header", closer)
	}
	p.i += len(closer)
	return path, nil
}

// dottedKey reads key ( '.' key )*.
func (p *tomlParser) dottedKey() ([]string, error) {
	var parts []string
	for {
		p.skipWS()
		k, err := p.simpleKey()
		if err != nil {
			return nil, err
		}
		parts = append(parts, k)
		p.skipWS()
		if p.peek() != '.' {
			return parts, nil
		}
		p.i++
	}
}

func isBareKeyByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func (p *tomlParser) simpleKey() (string, error) {
	switch p.peek() {
	case '"':
		return p.basicString()
	case '\'':
		return p.literalString()
	}
	start := p.i
	for !p.eof() && isBareKeyByte(p.s[p.i]) {
		p.i++
	}
	if p.i == start {
		return "", p.errf("expected a key")
	}
	return p.s[start:p.i], nil
}

// keyValue parses `key = value` into tbl (dotted keys create sub-tables).
func (p *tomlParser) keyValue(tbl map[string]any) error {
	path, err := p.dottedKey()
	if err != nil {
		return err
	}
	p.skipWS()
	if p.peek() != '=' {
		return p.errf("expected '=' after key %q", strings.Join(path, "."))
	}
	p.i++
	p.skipWS()
	v, err := p.value()
	if err != nil {
		return err
	}
	parent, err := descendTable(tbl, path[:len(path)-1])
	if err != nil {
		return p.errf("%v", err)
	}
	key := path[len(path)-1]
	if _, dup := parent[key]; dup {
		return p.errf("duplicate key %q", strings.Join(path, "."))
	}
	parent[key] = v
	return nil
}

func (p *tomlParser) value() (any, error) {
	switch {
	case p.hasPrefix(`"""`):
		return p.multilineBasic()
	case p.hasPrefix(`'''`):
		return p.multilineLiteral()
	case p.peek() == '"':
		return p.basicString()
	case p.peek() == '\'':
		return p.literalString()
	case p.peek() == '[':
		return p.array()
	case p.peek() == '{':
		return p.inlineTable()
	}
	start := p.i
	for !p.eof() {
		c := p.s[p.i]
		if c == ',' || c == ']' || c == '}' || c == '#' || c == '\n' || c == '\r' {
			break
		}
		p.i++
	}
	tok := strings.TrimRight(p.s[start:p.i], " \t")
	// A space inside a bare token is only legal in a date-time ("1979-05-27 07:32:00").
	p.i = start + len(tok)
	if tok == "" {
		return nil, p.errf("missing value")
	}
	return tomlScalar(tok)
}

func tomlScalar(tok string) (any, error) {
	switch tok {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "inf", "+inf":
		return math.Inf(1), nil
	case "-inf":
		return math.Inf(-1), nil
	case "nan", "+nan", "-nan":
		return math.NaN(), nil
	}
	clean := strings.ReplaceAll(tok, "_", "")
	if strings.HasPrefix(clean, "0x") || strings.HasPrefix(clean, "0o") || strings.HasPrefix(clean, "0b") {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[clean[1]]
		if n, err := strconv.ParseInt(clean[2:], base, 64); err == nil {
			return n, nil
		}
		return nil, fmt.Errorf("toml: invalid integer %q", tok)
	}
	if n, err := strconv.ParseInt(clean, 10, 64); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(clean, 64); err == nil {
		return f, nil
	}
	// Dates / times: keep the literal text.
	if len(tok) >= 8 && (tok[0] >= '0' && tok[0] <= '9') && strings.ContainsAny(tok, "-:") {
		return tok, nil
	}
	return nil, fmt.Errorf("toml: invalid value %q", tok)
}

func (p *tomlParser) basicString() (string, error) {
	p.i++ // opening quote
	var b strings.Builder
	for {
		if p.eof() || p.s[p.i] == '\n' {
			return "", p.errf("unterminated string")
		}
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return b.String(), nil
		}
		if c == '\\' {
			if err := p.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		b.WriteByte(c)
		p.i++
	}
}

// escape decodes one backslash escape at p.i.
func (p *tomlParser) escape(b *strings.Builder) error {
	if p.i+1 >= len(p.s) {
		return p.errf("unterminated escape")
	}
	e := p.s[p.i+1]
	p.i += 2
	switch e {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case 'e':
		b.WriteByte(0x1b)
	case '"':
		b.WriteByte('"')
	case '\\':
		b.WriteByte('\\')
	case 'u', 'U':
		n := 4
		if e == 'U' {
			n = 8
		}
		if p.i+n > len(p.s) {
			return p.errf("short unicode escape")
		}
		v, err := strconv.ParseUint(p.s[p.i:p.i+n], 16, 32)
		if err != nil || !utf8.ValidRune(rune(v)) {
			return p.errf("invalid unicode escape")
		}
		b.WriteRune(rune(v))
		p.i += n
	default:
		return p.errf("invalid escape \\%c", e)
	}
	return nil
}

func (p *tomlParser) literalString() (string, error) {
	p.i++
	start := p.i
	for !p.eof() && p.s[p.i] != '\'' {
		if p.s[p.i] == '\n' {
			return "", p.errf("unterminated literal string")
		}
		p.i++
	}
	if p.eof() {
		return "", p.errf("unterminated literal string")
	}
	out := p.s[start:p.i]
	p.i++
	return out, nil
}

// skipFirstNewline trims a newline immediately after a multi-line opener.
func (p *tomlParser) skipFirstNewline() {
	if p.hasPrefix("\r\n") {
		p.i += 2
	} else if p.peek() == '\n' {
		p.i++
	}
}

func (p *tomlParser) multilineBasic() (string, error) {
	p.i += 3
	p.skipFirstNewline()
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.errf("unterminated multi-line string")
		}
		if p.hasPrefix(`"""`) {
			// Up to two extra quotes may sit right before the closing delimiter.
			extra := 0
			for extra < 2 && p.i+3+extra < len(p.s) && p.s[p.i+3+extra] == '"' {
				extra++
			}
			b.WriteString(strings.Repeat(`"`, extra))
			p.i += 3 + extra
			return b.String(), nil
		}
		c := p.s[p.i]
		if c == '\\' {
			// Line-ending backslash: trim the newline and following whitespace.
			j := p.i + 1
			for j < len(p.s) && (p.s[j] == ' ' || p.s[j] == '\t') {
				j++
			}
			if j < len(p.s) && (p.s[j] == '\n' || p.s[j] == '\r') {
				for j < len(p.s) && (p.s[j] == ' ' || p.s[j] == '\t' || p.s[j] == '\n' || p.s[j] == '\r') {
					j++
				}
				p.i = j
				continue
			}
			if err := p.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		if c == '\r' && p.i+1 < len(p.s) && p.s[p.i+1] == '\n' {
			p.i++
			continue
		}
		b.WriteByte(c)
		p.i++
	}
}

func (p *tomlParser) multilineLiteral() (string, error) {
	p.i += 3
	p.skipFirstNewline()
	end := strings.Index(p.s[p.i:], "'''")
	if end == -1 {
		return "", p.errf("unterminated multi-line literal string")
	}
	out := p.s[p.i : p.i+end]
	p.i += end + 3
	for i := 0; i < 2 && p.peek() == '\''; i++ {
		out += "'"
		p.i++
	}
	return strings.ReplaceAll(out, "\r\n", "\n"), nil
}

func (p *tomlParser) array() ([]any, error) {
	p.i++ // [
	out := []any{}
	for {
		p.skipBlank()
		if p.eof() {
			return nil, p.errf("unterminated array")
		}
		if p.peek() == ']' {
			p.i++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipBlank()
		switch p.peek() {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, nil
		default:
			return nil, p.errf("expected ',' or ']' in array")
		}
	}
}

func (p *tomlParser) inlineTable() (map[string]any, error) {
	p.i++ // {
	tbl := map[string]any{}
	p.skipBlank()
	if p.peek() == '}' {
		p.i++
		return tbl, nil
	}
	for {
		p.skipBlank()
		if err := p.keyValue(tbl); err != nil {
			return nil, err
		}
		p.skipBlank()
		switch p.peek() {
		case ',':
			p.i++
			p.skipBlank()
			if p.peek() == '}' { // lenient: trailing comma (TOML 1.1)
				p.i++
				return tbl, nil
			}
		case '}':
			p.i++
			return tbl, nil
		default:
			return nil, p.errf("expected ',' or '}' in inline table")
		}
	}
}

// descendTable walks/creates nested tables along path. When a segment names an
// array of tables, it descends into that array's last element (TOML semantics
// for [a.b] after [[a]]).
func descendTable(root map[string]any, path []string) (map[string]any, error) {
	cur := root
	for _, seg := range path {
		switch next := cur[seg].(type) {
		case nil:
			tbl := map[string]any{}
			cur[seg] = tbl
			cur = tbl
		case map[string]any:
			cur = next
		case []any:
			if len(next) == 0 {
				return nil, fmt.Errorf("key %q is an empty array, not a table", seg)
			}
			tbl, ok := next[len(next)-1].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("key %q is an array of values, not tables", seg)
			}
			cur = tbl
		default:
			return nil, fmt.Errorf("key %q is a value, not a table", seg)
		}
	}
	return cur, nil
}

func appendArrayTable(root map[string]any, path []string, tbl map[string]any) error {
	parent, err := descendTable(root, path[:len(path)-1])
	if err != nil {
		return err
	}
	key := path[len(path)-1]
	switch existing := parent[key].(type) {
	case nil:
		parent[key] = []any{tbl}
	case []any:
		parent[key] = append(existing, tbl)
	default:
		return fmt.Errorf("key %q is not an array of tables", key)
	}
	return nil
}
