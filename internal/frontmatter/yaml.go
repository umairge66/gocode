// Package frontmatter parses the YAML frontmatter block at the head of the
// markdown files gocode loads (custom slash commands, SKILL.md files and agent
// definitions).
//
// Parse supports this YAML subset:
//
//   - block mappings (`key: value`) nested to any depth, with plain, single- or
//     double-quoted keys;
//   - block sequences (`- item`), including `- key: value` compact mappings and
//     a sequence written at the same indentation as its parent key;
//   - plain scalars (multi-line plain continuations are folded with spaces),
//     single-quoted (” escape) and double-quoted scalars (full YAML escape set,
//     multi-line folding);
//   - core-schema typing of plain scalars: null/~, true/false, integers
//     (decimal, 0x, 0o), floats (incl. .inf/.nan);
//   - flow sequences `[a, b]` and flow mappings `{k: v}`, nested, possibly
//     spanning several lines;
//   - literal `|` and folded `>` block scalars with chomping (`-`/`+`) and an
//     optional indentation indicator;
//   - comments (`#` at line start or after whitespace, never inside quotes);
//   - CRLF line endings.
//
// Unsupported (rejected or read literally): anchors/aliases and tags are read
// as part of a plain scalar; complex keys (`? `) and multi-document streams are
// syntax errors. Malformed input yields a *SyntaxError carrying a 1-based line
// number.
package frontmatter

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SyntaxError is a malformed-YAML error at a 1-based line of the parsed text.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// ErrNotTable is returned by Parse when the block is valid YAML but its root is
// a sequence or a scalar rather than a mapping. Such a file is kept with an
// empty frontmatter.
var ErrNotTable = errors.New("frontmatter is a list or a single value, not a table")

// Parse parses a frontmatter block into a mapping. An empty or comment-only
// block yields an empty, non-nil map. Values are string, bool, int64, float64,
// nil, []any or map[string]any.
func Parse(src string) (map[string]any, error) {
	v, err := ParseValue(src)
	if err != nil {
		return map[string]any{}, err
	}
	switch x := v.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return x, nil
	default:
		return map[string]any{}, ErrNotTable
	}
}

// ParseValue parses a YAML document of the supported subset and returns its
// root value (nil for an empty document).
func ParseValue(src string) (any, error) {
	p := newParser(src)
	p.skipInsignificant()
	if p.eof() {
		return nil, nil
	}
	first := p.cur()
	v, err := p.parseNode(first.indent, -1)
	if err != nil {
		return nil, err
	}
	p.skipInsignificant()
	if !p.eof() {
		l := p.cur()
		if l.indent > first.indent {
			return nil, p.errAt(l, "unexpected indentation")
		}
		return nil, p.errAt(l, fmt.Sprintf("unexpected content %q", l.text))
	}
	return v, nil
}

type srcLine struct {
	num    int    // 1-based
	raw    string // without the trailing \r
	indent int    // count of leading spaces
	text   string // raw[indent:], trailing whitespace trimmed
}

type parser struct {
	lines []srcLine
	pos   int
}

func newParser(src string) *parser {
	src = strings.TrimPrefix(src, "\ufeff")
	parts := strings.Split(src, "\n")
	lines := make([]srcLine, len(parts))
	for i, raw := range parts {
		raw = strings.TrimSuffix(raw, "\r")
		ind := 0
		for ind < len(raw) && raw[ind] == ' ' {
			ind++
		}
		lines[i] = srcLine{num: i + 1, raw: raw, indent: ind, text: strings.TrimRight(raw[ind:], " \t")}
	}
	return &parser{lines: lines}
}

func (p *parser) eof() bool     { return p.pos >= len(p.lines) }
func (p *parser) cur() *srcLine { return &p.lines[p.pos] }
func (p *parser) errAt(l *srcLine, msg string) error {
	return &SyntaxError{Line: l.num, Msg: msg}
}

// insignificant reports whether a line is blank or a whole-line comment.
func insignificant(l *srcLine) bool {
	t := strings.TrimLeft(l.text, " \t")
	return t == "" || t[0] == '#'
}

func (p *parser) skipInsignificant() {
	for !p.eof() && insignificant(p.cur()) {
		p.pos++
	}
}

// checkTabs rejects a structural line indented with a tab (YAML forbids tabs
// in indentation).
func (p *parser) checkTabs(l *srcLine) error {
	if strings.HasPrefix(l.text, "\t") {
		return p.errAt(l, "tab characters are not allowed in indentation")
	}
	return nil
}

func isSeqItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ") || strings.HasPrefix(text, "-\t")
}

// parseNode parses the block node starting at the current line, whose
// indentation is ind. parent is the indentation of the enclosing node (-1 at
// the root).
func (p *parser) parseNode(ind, parent int) (any, error) {
	p.skipInsignificant()
	if p.eof() {
		return nil, nil
	}
	l := p.cur()
	if err := p.checkTabs(l); err != nil {
		return nil, err
	}
	if isSeqItem(l.text) {
		return p.parseSeq(ind)
	}
	if _, _, ok, err := splitKey(l.text); err != nil {
		return nil, p.errAt(l, err.Error())
	} else if ok {
		return p.parseMap(ind)
	}
	// A bare scalar node (e.g. a multi-line plain value on the next line).
	p.pos++
	return p.parseInlineValue(l, l.text, parent)
}

func (p *parser) parseMap(ind int) (any, error) {
	m := map[string]any{}
	for {
		p.skipInsignificant()
		if p.eof() {
			break
		}
		l := p.cur()
		if l.indent < ind {
			break
		}
		if l.indent > ind {
			return nil, p.errAt(l, "unexpected indentation")
		}
		if err := p.checkTabs(l); err != nil {
			return nil, err
		}
		if isSeqItem(l.text) {
			// A sequence at the mapping's own indentation only follows an empty
			// key value; anything else is a structural error.
			return nil, p.errAt(l, "unexpected sequence item inside a mapping")
		}
		key, rest, ok, err := splitKey(l.text)
		if err != nil {
			return nil, p.errAt(l, err.Error())
		}
		if !ok {
			return nil, p.errAt(l, fmt.Sprintf("expected \"key: value\", got %q", l.text))
		}
		p.pos++
		v, err := p.parseValue(l, rest, ind, true)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
	return m, nil
}

func (p *parser) parseSeq(ind int) (any, error) {
	out := []any{}
	for {
		p.skipInsignificant()
		if p.eof() {
			break
		}
		l := p.cur()
		if l.indent < ind {
			break
		}
		if l.indent > ind {
			return nil, p.errAt(l, "unexpected indentation")
		}
		if err := p.checkTabs(l); err != nil {
			return nil, err
		}
		if !isSeqItem(l.text) {
			break
		}
		after := l.text[1:]
		spaces := 0
		for spaces < len(after) && (after[spaces] == ' ' || after[spaces] == '\t') {
			spaces++
		}
		rest := after[spaces:]
		var v any
		var err error
		_, _, isKey, kerr := splitKey(rest)
		switch {
		case rest != "" && rest[0] != '#' && (isSeqItem(rest) || (kerr == nil && isKey)):
			// Compact nested node ("- key: v" / "- - x"): re-read this line as
			// starting at the item's content column.
			col := ind + 1 + spaces
			l.indent = col
			l.text = rest
			v, err = p.parseNode(col, ind)
		default:
			p.pos++
			v, err = p.parseValue(l, rest, ind, false)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// parseValue parses the value that follows a "key:" (inMap) or a "- " marker.
// rest is the remainder of the introducing line; parent is the indentation of
// the mapping/sequence that owns the value.
func (p *parser) parseValue(l *srcLine, rest string, parent int, inMap bool) (any, error) {
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" || rest[0] == '#' {
		p.skipInsignificant()
		if p.eof() {
			return nil, nil
		}
		next := p.cur()
		if next.indent > parent {
			return p.parseNode(next.indent, parent)
		}
		if inMap && next.indent == parent && isSeqItem(next.text) {
			return p.parseSeq(parent)
		}
		return nil, nil
	}
	return p.parseInlineValue(l, rest, parent)
}

// parseInlineValue parses a scalar or flow value that starts in rest (the tail
// of line l, already consumed) and may continue on following lines indented
// deeper than parent.
func (p *parser) parseInlineValue(l *srcLine, rest string, parent int) (any, error) {
	switch rest[0] {
	case '|', '>':
		return p.parseBlockScalar(l, rest, parent)
	case '"', '\'':
		return p.parseQuoted(l, rest)
	case '[', '{':
		return p.parseFlow(l, rest)
	}
	return p.parsePlain(rest, parent)
}

func (p *parser) parsePlain(rest string, parent int) (any, error) {
	s, hadComment := stripPlainComment(rest)
	if hadComment {
		return resolvePlain(s), nil
	}
	var b strings.Builder
	b.WriteString(s)
	multi := false
	blanks := 0
	for !p.eof() {
		n := p.cur()
		t := strings.TrimLeft(n.text, " \t")
		if t == "" {
			blanks++
			p.pos++
			continue
		}
		if n.indent <= parent || t[0] == '#' {
			break
		}
		if n.indent == 0 && parent < 0 {
			break
		}
		if _, _, isKey, _ := splitKey(t); isKey {
			return nil, p.errAt(n, "unexpected indentation (a mapping entry cannot continue a plain scalar)")
		}
		p.pos++
		part, comment := stripPlainComment(t)
		if blanks > 0 {
			b.WriteString(strings.Repeat("\n", blanks))
		} else {
			b.WriteByte(' ')
		}
		blanks = 0
		b.WriteString(part)
		multi = true
		if comment {
			break
		}
	}
	if multi {
		return b.String(), nil
	}
	return resolvePlain(s), nil
}

// stripPlainComment cuts a "# comment" that follows whitespace in a plain
// scalar and trims the result.
func stripPlainComment(s string) (string, bool) {
	if strings.HasPrefix(s, "#") {
		return "", true
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimRight(s[:i], " \t"), true
		}
	}
	return strings.TrimSpace(s), false
}

var (
	reInt    = regexp.MustCompile(`^[-+]?[0-9]+$`)
	reOct    = regexp.MustCompile(`^0o[0-7]+$`)
	reHex    = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	reFloat  = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
	reInf    = regexp.MustCompile(`^[-+]?\.(inf|Inf|INF)$`)
	reNaN    = regexp.MustCompile(`^\.(nan|NaN|NAN)$`)
	nullWord = map[string]bool{"": true, "~": true, "null": true, "Null": true, "NULL": true}
)

// resolvePlain applies YAML 1.2 core-schema typing to a single-line plain
// scalar.
func resolvePlain(s string) any {
	if nullWord[s] {
		return nil
	}
	switch s {
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	if reInt.MatchString(s) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	if reOct.MatchString(s) {
		if n, err := strconv.ParseInt(s[2:], 8, 64); err == nil {
			return n
		}
	}
	if reHex.MatchString(s) {
		if n, err := strconv.ParseInt(s[2:], 16, 64); err == nil {
			return n
		}
	}
	if reFloat.MatchString(s) {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	if reInf.MatchString(s) {
		if s[0] == '-' {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	if reNaN.MatchString(s) {
		return math.NaN()
	}
	return s
}

// splitKey splits "key: rest". ok is false when the text is not a mapping
// entry; err is set for a malformed quoted key.
func splitKey(text string) (key, rest string, ok bool, err error) {
	if text == "" || text[0] == '#' || text[0] == '[' || text[0] == '{' || isSeqItem(text) {
		return "", "", false, nil
	}
	if strings.HasPrefix(text, "? ") || text == "?" {
		return "", "", false, errors.New("complex mapping keys (\"? \") are not supported")
	}
	if text[0] == '"' || text[0] == '\'' {
		end, cerr := closingQuote(text, 0)
		if cerr != nil {
			return "", "", false, nil // a quoted scalar, not a key
		}
		after := strings.TrimLeft(text[end+1:], " \t")
		if !strings.HasPrefix(after, ":") || (len(after) > 1 && after[1] != ' ' && after[1] != '\t') {
			return "", "", false, nil
		}
		k, derr := decodeQuoted(text[:end+1])
		if derr != nil {
			return "", "", false, derr
		}
		return k, after[1:], true, nil
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '#' && i > 0 && (text[i-1] == ' ' || text[i-1] == '\t') {
			return "", "", false, nil
		}
		if c == ':' && (i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\t') {
			k := strings.TrimRight(text[:i], " \t")
			if k == "" {
				return "", "", false, nil
			}
			return k, text[i+1:], true, nil
		}
	}
	return "", "", false, nil
}

// closingQuote returns the index of the quote closing the scalar opened at
// s[start].
func closingQuote(s string, start int) (int, error) {
	q := s[start]
	for i := start + 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q:
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i, nil
		}
	}
	return -1, errors.New("unterminated quoted string")
}

func (p *parser) parseQuoted(l *srcLine, rest string) (any, error) {
	raw := rest
	end, err := closingQuote(raw, 0)
	for err != nil {
		if p.eof() {
			return nil, p.errAt(l, "unterminated quoted string")
		}
		raw += "\n" + p.cur().raw
		p.pos++
		end, err = closingQuote(raw, 0)
	}
	tail := strings.TrimLeft(raw[end+1:], " \t")
	if tail != "" && tail[0] != '#' {
		return nil, p.errAt(l, fmt.Sprintf("unexpected text after quoted string: %q", tail))
	}
	s, derr := decodeQuoted(raw[:end+1])
	if derr != nil {
		return nil, p.errAt(l, derr.Error())
	}
	return s, nil
}

// decodeQuoted decodes a complete single- or double-quoted scalar (including
// its quotes), folding embedded line breaks the YAML way: a single break
// becomes a space, each empty line becomes "\n", surrounding whitespace is
// dropped, and in a double-quoted scalar a trailing "\" escapes the break.
func decodeQuoted(q string) (string, error) {
	quote := q[0]
	lines := strings.Split(q[1:len(q)-1], "\n")
	last := len(lines) - 1
	parts := make([]string, len(lines))
	esc := make([]bool, len(lines))
	for i, ln := range lines {
		if i > 0 {
			ln = strings.TrimLeft(ln, " \t")
		}
		if i < last {
			if quote == '"' && oddTrailingBackslashes(ln) {
				ln = ln[:len(ln)-1]
				esc[i] = true
			} else {
				ln = strings.TrimRight(ln, " \t")
			}
		}
		if quote == '"' {
			seg, err := unescapeDouble(ln)
			if err != nil {
				return "", err
			}
			parts[i] = seg
		} else {
			parts[i] = strings.ReplaceAll(ln, "''", "'")
		}
	}
	var b strings.Builder
	b.WriteString(parts[0])
	pendingEsc, empties := esc[0], 0
	for i := 1; i <= last; i++ {
		if parts[i] == "" && i < last && !esc[i] {
			empties++
			continue
		}
		switch {
		case pendingEsc && empties == 0:
		case empties == 0:
			b.WriteByte(' ')
		default:
			b.WriteString(strings.Repeat("\n", empties))
		}
		b.WriteString(parts[i])
		pendingEsc, empties = esc[i], 0
	}
	return b.String(), nil
}

func oddTrailingBackslashes(s string) bool {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// unescapeDouble processes the YAML double-quoted escape sequences.
func unescapeDouble(s string) (string, error) {
	if !strings.Contains(s, "\\") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", errors.New("dangling backslash in double-quoted string")
		}
		switch s[i] {
		case '0':
			b.WriteByte(0)
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 't', '\t':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'v':
			b.WriteByte('\v')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'e':
			b.WriteByte(0x1b)
		case ' ':
			b.WriteByte(' ')
		case '"':
			b.WriteByte('"')
		case '/':
			b.WriteByte('/')
		case '\\':
			b.WriteByte('\\')
		case 'N':
			b.WriteString("\u0085")
		case '_':
			b.WriteString(" ")
		case 'L':
			b.WriteString(" ")
		case 'P':
			b.WriteString(" ")
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[i]]
			if i+1+n > len(s) {
				return "", fmt.Errorf("truncated \\%c escape", s[i])
			}
			v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if err != nil || !utf8.ValidRune(rune(v)) {
				return "", fmt.Errorf("invalid \\%c escape %q", s[i], s[i+1:i+1+n])
			}
			b.WriteRune(rune(v))
			i += n
		default:
			return "", fmt.Errorf("invalid escape sequence \\%c", s[i])
		}
	}
	return b.String(), nil
}

// parseBlockScalar parses a literal (|) or folded (>) block scalar whose
// header is rest.
func (p *parser) parseBlockScalar(l *srcLine, rest string, parent int) (any, error) {
	style := rest[0]
	header := rest[1:]
	chomp := byte(0)
	explicit := 0
	i := 0
	for ; i < len(header) && i < 2; i++ {
		c := header[i]
		switch {
		case (c == '-' || c == '+') && chomp == 0:
			chomp = c
		case c >= '1' && c <= '9' && explicit == 0:
			explicit = int(c - '0')
		default:
			goto done
		}
	}
done:
	tail := strings.TrimLeft(header[i:], " \t")
	if tail != "" && tail[0] != '#' {
		return nil, p.errAt(l, fmt.Sprintf("invalid block scalar header %q", rest))
	}
	if tail == "" && i < len(header) && strings.TrimSpace(header[i:]) != "" {
		return nil, p.errAt(l, fmt.Sprintf("invalid block scalar header %q", rest))
	}
	base := parent
	if base < 0 {
		base = 0
	}
	contentIndent := -1
	if explicit > 0 {
		contentIndent = base + explicit
		if parent < 0 {
			contentIndent = explicit
		}
	}
	var content []string
	for !p.eof() {
		n := p.cur()
		if strings.TrimSpace(n.raw) == "" {
			content = append(content, "")
			p.pos++
			continue
		}
		if contentIndent < 0 {
			if n.indent <= parent {
				break
			}
			contentIndent = n.indent
		}
		if n.indent < contentIndent {
			break
		}
		content = append(content, n.raw[contentIndent:])
		p.pos++
	}
	// Separate trailing blank lines (subject to chomping).
	trailing := 0
	for len(content) > 0 && content[len(content)-1] == "" {
		content = content[:len(content)-1]
		trailing++
	}
	var body string
	if style == '|' {
		body = strings.Join(content, "\n")
	} else {
		body = foldLines(content)
	}
	switch chomp {
	case '-':
		return body, nil
	case '+':
		if len(content) == 0 {
			return strings.Repeat("\n", trailing), nil
		}
		return body + "\n" + strings.Repeat("\n", trailing), nil
	default:
		if len(content) == 0 {
			return "", nil
		}
		return body + "\n", nil
	}
}

// foldLines applies folded-scalar line folding: adjacent non-empty lines at the
// content indentation join with a space; blank lines become newlines; lines
// that are more indented keep their surrounding breaks.
func foldLines(lines []string) string {
	var b strings.Builder
	first := true
	empties := 0
	prevMore := false
	for _, ln := range lines {
		if ln == "" {
			empties++
			continue
		}
		more := ln[0] == ' ' || ln[0] == '\t'
		switch {
		case first:
			b.WriteString(strings.Repeat("\n", empties))
		case empties == 0 && !more && !prevMore:
			b.WriteByte(' ')
		case !more && !prevMore:
			b.WriteString(strings.Repeat("\n", empties))
		default:
			b.WriteString(strings.Repeat("\n", empties+1))
		}
		b.WriteString(ln)
		first, empties, prevMore = false, 0, more
	}
	return b.String()
}

// parseFlow parses a flow collection that starts in rest, pulling in further
// lines until its brackets balance.
func (p *parser) parseFlow(l *srcLine, rest string) (any, error) {
	raw := rest
	for !flowClosed(raw) {
		if p.eof() {
			return nil, p.errAt(l, "unterminated flow collection (missing closing bracket)")
		}
		raw += " " + strings.TrimSpace(p.cur().raw)
		p.pos++
	}
	fp := &flowParser{s: raw}
	v, err := fp.value(0)
	if err != nil {
		return nil, p.errAt(l, err.Error())
	}
	fp.ws()
	if fp.i < len(fp.s) && fp.s[fp.i] != '#' {
		return nil, p.errAt(l, fmt.Sprintf("unexpected text after flow collection: %q", fp.s[fp.i:]))
	}
	return v, nil
}

// flowClosed reports whether the first flow collection in s is balanced.
func flowClosed(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\'':
			end, err := closingQuote(s, i)
			if err != nil {
				return false
			}
			i = end
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				return true
			}
		case '#':
			if i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
				// A comment inside an open flow collection runs to end of line;
				// the joined text has lost line boundaries, so stop here.
				return false
			}
		}
	}
	return false
}

type flowParser struct {
	s string
	i int
}

func (f *flowParser) ws() {
	for f.i < len(f.s) && (f.s[f.i] == ' ' || f.s[f.i] == '\t' || f.s[f.i] == '\n') {
		f.i++
	}
}

func (f *flowParser) value(depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("flow collection nested too deeply")
	}
	f.ws()
	if f.i >= len(f.s) {
		return nil, errors.New("unexpected end of flow collection")
	}
	switch f.s[f.i] {
	case '[':
		f.i++
		out := []any{}
		for {
			f.ws()
			if f.i >= len(f.s) {
				return nil, errors.New("unterminated flow sequence")
			}
			if f.s[f.i] == ']' {
				f.i++
				return out, nil
			}
			if f.s[f.i] == ',' {
				return nil, errors.New("empty entry in flow sequence")
			}
			v, err := f.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			f.ws()
			if f.i < len(f.s) && f.s[f.i] == ',' {
				f.i++
				continue
			}
			if f.i < len(f.s) && f.s[f.i] == ']' {
				continue
			}
			return nil, errors.New("expected ',' or ']' in flow sequence")
		}
	case '{':
		f.i++
		out := map[string]any{}
		for {
			f.ws()
			if f.i >= len(f.s) {
				return nil, errors.New("unterminated flow mapping")
			}
			if f.s[f.i] == '}' {
				f.i++
				return out, nil
			}
			k, err := f.scalar(true)
			if err != nil {
				return nil, err
			}
			key, _ := k.(string)
			if k != nil && key == "" {
				key = String(k)
			}
			f.ws()
			var v any
			if f.i < len(f.s) && f.s[f.i] == ':' {
				f.i++
				f.ws()
				if f.i < len(f.s) && (f.s[f.i] == ',' || f.s[f.i] == '}') {
					v = nil
				} else if v, err = f.value(depth + 1); err != nil {
					return nil, err
				}
			}
			out[key] = v
			f.ws()
			if f.i < len(f.s) && f.s[f.i] == ',' {
				f.i++
				continue
			}
			if f.i < len(f.s) && f.s[f.i] == '}' {
				continue
			}
			return nil, errors.New("expected ',' or '}' in flow mapping")
		}
	}
	return f.scalar(false)
}

// scalar reads a quoted or plain flow scalar. In key position a plain scalar
// also stops at ':' followed by whitespace.
func (f *flowParser) scalar(key bool) (any, error) {
	if f.i < len(f.s) && (f.s[f.i] == '"' || f.s[f.i] == '\'') {
		end, err := closingQuote(f.s, f.i)
		if err != nil {
			return nil, err
		}
		s, err := decodeQuoted(f.s[f.i : end+1])
		if err != nil {
			return nil, err
		}
		f.i = end + 1
		return s, nil
	}
	start := f.i
	for f.i < len(f.s) {
		c := f.s[f.i]
		if c == ',' || c == ']' || c == '}' || c == '[' || c == '{' {
			break
		}
		if c == ':' && (f.i+1 == len(f.s) || strings.ContainsRune(" \t,]}", rune(f.s[f.i+1]))) && key {
			break
		}
		if c == '#' && f.i > start && (f.s[f.i-1] == ' ' || f.s[f.i-1] == '\t') {
			break
		}
		f.i++
	}
	tok := strings.TrimSpace(f.s[start:f.i])
	if tok == "" && !key {
		return nil, errors.New("empty value in flow collection")
	}
	return resolvePlain(tok), nil
}
