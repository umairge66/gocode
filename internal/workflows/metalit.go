package workflows

import (
	"fmt"
	"strconv"
	"strings"
)

// This file is the hand-rolled pure-literal scanner (the fast path of the
// meta parser). It parses exactly the subset the `meta` object is
// allowed to be — a PURE LITERAL: nested objects/arrays, strings (single,
// double, and non-interpolated template literals), numbers (including negative),
// booleans and null. No variables, calls, spreads or template interpolation.
//
// Behaviors:
//   - JS whitespace plus //-line and /* */ block comments are skipped as
//     trivia.
//   - trailing commas allowed in objects and arrays.
//   - object keys may be identifiers or quoted strings; computed/spread/method
//     keys and the reserved names __proto__/constructor/prototype are rejected.
//   - a number may not be immediately followed by an identifier char or `.`.
//   - template literals reject interpolation (`${`) and raw newlines in the
//     single/double forms.

// isWS reports whether r is JS whitespace (`\s` plus the exotic line
// separators). We treat the common ASCII set plus U+2028/U+2029.
func isWS(r byte) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// skipTrivia advances past whitespace and // and /* */ comments.
func skipTrivia(s string, i int) int {
	for i < len(s) {
		c := s[i]
		if isWS(c) {
			i++
			continue
		}
		if c == '/' && i+1 < len(s) {
			switch s[i+1] {
			case '/':
				i += 2
				for i < len(s) && s[i] != '\n' && s[i] != '\r' {
					i++
				}
				continue
			case '*':
				i += 2
				for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
					i++
				}
				if i+1 < len(s) {
					i += 2
				} else {
					i = len(s)
				}
				continue
			}
		}
		break
	}
	return i
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c > 127
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// parseValue parses one literal value starting at index n (already at a
// non-trivia char). It returns the value, the index just past it, or an error.
func parseValue(s string, n int) (any, int, error) {
	if n >= len(s) {
		return nil, 0, fmt.Errorf("unexpected end of input")
	}
	switch s[n] {
	case '{':
		return parseObject(s, n)
	case '[':
		return parseArray(s, n)
	case '\'', '"', '`':
		return parseString(s, n)
	case '-':
		v, end, err := parseNumber(s, skipTrivia(s, n+1))
		if err != nil {
			return nil, 0, fmt.Errorf("only negative-number unary allowed in meta")
		}
		return -v, end, nil
	}
	// number?
	if v, end, err := parseNumber(s, n); err == nil {
		return v, end, nil
	}
	// identifier keyword
	word, end := scanIdent(s, n)
	switch word {
	case "true":
		return true, end, nil
	case "false":
		return false, end, nil
	case "null":
		return nil, end, nil
	}
	if word == "" {
		return nil, 0, fmt.Errorf("non-literal node in meta at offset %d", n)
	}
	return nil, 0, fmt.Errorf("non-literal identifier %q in meta", word)
}

// scanIdent reads an identifier at n (identifier-start already implied by
// caller or validated here); returns "" if n is not an identifier start.
func scanIdent(s string, n int) (string, int) {
	if n >= len(s) || !isIdentStart(s[n]) {
		return "", n
	}
	i := n + 1
	for i < len(s) && isIdentPart(s[i]) {
		i++
	}
	return s[n:i], i
}

// parseObject parses `{ key: value, ... }`.
func parseObject(s string, n int) (any, int, error) {
	out := map[string]any{}
	i := n + 1
	first := true
	for {
		i = skipTrivia(s, i)
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated object literal")
		}
		if s[i] == '}' {
			return out, i + 1, nil
		}
		if !first {
			if s[i] != ',' {
				return nil, 0, fmt.Errorf("expected ',' in object literal")
			}
			i = skipTrivia(s, i+1)
			if i < len(s) && s[i] == '}' {
				return out, i + 1, nil // trailing comma
			}
		}
		first = false

		key, keyEnd, err := parseKey(s, i)
		if err != nil {
			return nil, 0, err
		}
		if reservedKeys[key] {
			return nil, 0, fmt.Errorf("reserved key name not allowed in meta: %s", key)
		}
		i = skipTrivia(s, keyEnd)
		if i >= len(s) || s[i] != ':' {
			return nil, 0, fmt.Errorf("expected ':' after key %q in meta", key)
		}
		i = skipTrivia(s, i+1)
		val, valEnd, err := parseValue(s, i)
		if err != nil {
			return nil, 0, err
		}
		out[key] = val
		i = valEnd
	}
}

// parseKey reads an object key: a quoted string or a bare identifier.
func parseKey(s string, n int) (string, int, error) {
	if n < len(s) && (s[n] == '\'' || s[n] == '"') {
		v, end, err := parseString(s, n)
		if err != nil {
			return "", 0, err
		}
		return v.(string), end, nil
	}
	word, end := scanIdent(s, n)
	if word == "" {
		return "", 0, fmt.Errorf("unsupported key in meta at offset %d", n)
	}
	return word, end, nil
}

// parseArray parses `[ value, ... ]`; sparse arrays and
// spreads are rejected (a missing element or `...` simply fails to parse here).
func parseArray(s string, n int) (any, int, error) {
	out := []any{}
	i := n + 1
	first := true
	for {
		i = skipTrivia(s, i)
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated array literal")
		}
		if s[i] == ']' {
			return out, i + 1, nil
		}
		if !first {
			if s[i] != ',' {
				return nil, 0, fmt.Errorf("expected ',' in array literal")
			}
			i = skipTrivia(s, i+1)
			if i < len(s) && s[i] == ']' {
				return out, i + 1, nil // trailing comma
			}
		}
		first = false
		val, valEnd, err := parseValue(s, i)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, val)
		i = valEnd
	}
}

// parseNumber parses a JS numeric literal; it fails (so the caller can try an
// identifier) when n is not a digit or `.`. A number may not be followed by an
// identifier char or `.`.
func parseNumber(s string, n int) (float64, int, error) {
	i := n
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
			i++
		}
	}
	if i == n || (i == n+1 && s[n] == '.') {
		return 0, 0, fmt.Errorf("not a number")
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		k := j
		for k < len(s) && (s[k] >= '0' && s[k] <= '9') {
			k++
		}
		if k > j {
			i = k
		}
	}
	if i < len(s) && (isIdentPart(s[i]) || s[i] == '.') {
		return 0, 0, fmt.Errorf("number followed by identifier/dot")
	}
	f, err := strconv.ParseFloat(s[n:i], 64)
	if err != nil {
		return 0, 0, err
	}
	return f, i, nil
}

// parseString parses a single-quote, double-quote, or (non-interpolated)
// template-literal string, with escapes.
func parseString(s string, n int) (any, int, error) {
	quote := s[n]
	backtick := quote == '`'
	var b strings.Builder
	i := n + 1
	for i < len(s) {
		c := s[i]
		switch {
		case c == quote:
			return b.String(), i + 1, nil
		case c == '\\':
			val, end, ok := parseEscape(s, i)
			if !ok {
				return nil, 0, fmt.Errorf("invalid escape in string literal")
			}
			b.WriteString(val)
			i = end
		case backtick && c == '$' && i+1 < len(s) && s[i+1] == '{':
			return nil, 0, fmt.Errorf("template interpolation not allowed in meta")
		case !backtick && (c == '\n' || c == '\r'):
			return nil, 0, fmt.Errorf("unterminated string literal")
		case backtick && c == '\r':
			b.WriteByte('\n')
			if i+1 < len(s) && s[i+1] == '\n' {
				i += 2
			} else {
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return nil, 0, fmt.Errorf("unterminated string literal")
}

// parseEscape handles a backslash escape at index n (s[n]=='\\').
func parseEscape(s string, n int) (string, int, bool) {
	if n+1 >= len(s) {
		return "", 0, false
	}
	c := s[n+1]
	switch c {
	case 'n':
		return "\n", n + 2, true
	case 'r':
		return "\r", n + 2, true
	case 't':
		return "\t", n + 2, true
	case 'b':
		return "\b", n + 2, true
	case 'f':
		return "\f", n + 2, true
	case 'v':
		return "\v", n + 2, true
	case '0':
		if n+2 < len(s) && s[n+2] >= '0' && s[n+2] <= '9' {
			return "", 0, false
		}
		return "\x00", n + 2, true
	case 'x':
		if n+3 < len(s) {
			if r, err := strconv.ParseInt(s[n+2:n+4], 16, 32); err == nil {
				return string(rune(r)), n + 4, true
			}
		}
		return "", 0, false
	case 'u':
		if n+2 < len(s) && s[n+2] == '{' {
			end := strings.IndexByte(s[n+3:], '}')
			if end < 0 {
				return "", 0, false
			}
			if r, err := strconv.ParseInt(s[n+3:n+3+end], 16, 32); err == nil && r <= 0x10FFFF {
				return string(rune(r)), n + 3 + end + 1, true
			}
			return "", 0, false
		}
		if n+5 < len(s) || n+6 == len(s) {
			if r, err := strconv.ParseInt(s[n+2:n+6], 16, 32); err == nil {
				return string(rune(r)), n + 6, true
			}
		}
		return "", 0, false
	case '\r':
		if n+2 < len(s) && s[n+2] == '\n' {
			return "", n + 3, true
		}
		return "", n + 2, true
	case '\n':
		return "", n + 2, true
	default:
		if c >= '1' && c <= '9' {
			return "", 0, false
		}
		return string(c), n + 2, true
	}
}

// asiEnd: after the meta object ends at n,
// a statement terminator must follow. Returns the index to start the body from,
// and whether it is valid.
//
//   - a `;` consumes it and the body starts after it;
//   - end-of-input is valid (body empty, starts at n);
//   - otherwise a newline must separate the object from the next token, and the
//     next token must not be `in`/`instanceof` (which would make the `=`
//     expression continue). The body then starts at n.
func asiEnd(s string, n int) (int, bool) {
	t := skipTrivia(s, n)
	if t < len(s) && s[t] == ';' {
		return t + 1, true
	}
	if t >= len(s) {
		return n, true
	}
	if !strings.ContainsAny(s[n:t], "\n\r") {
		return 0, false
	}
	word, _ := scanIdent(s, t)
	if word == "in" || word == "instanceof" {
		return 0, false
	}
	return n, true
}
