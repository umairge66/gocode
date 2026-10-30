package frontmatter

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Document is a markdown file split into its parsed frontmatter and body.
type Document struct {
	// Data is the parsed frontmatter mapping; never nil. It is empty when the
	// file has no frontmatter or the block failed to parse.
	Data map[string]any
	// Body is the markdown after the frontmatter block (the whole file, BOM
	// stripped, when there is none).
	Body string
	// HasFrontmatter reports that a "---" fenced block was found.
	HasFrontmatter bool
	// Block is the raw frontmatter text between the fences.
	Block string
	// Err is the parse failure, when the block was not valid YAML even after
	// the lenient retry. Its line numbers are file line numbers. The file is
	// still usable: Data is empty (all keys take their defaults) and Body is
	// set, so the file still loads (with a warning).
	Err error
	// Problem is a non-fatal note (e.g. the block is a list, not a mapping).
	Problem string
}

// Split separates a leading "---" fenced frontmatter block from the body. The
// opening fence is the first line ("---" plus optional trailing blanks); the
// closing fence is the next line consisting of "---" (plus optional trailing
// blanks). CRLF line endings are accepted and a leading UTF-8 BOM is dropped.
func Split(doc string) (block, body string, ok bool) {
	doc = strings.TrimPrefix(doc, "\ufeff")
	if !strings.HasPrefix(doc, "---") {
		return "", doc, false
	}
	nl := strings.IndexByte(doc, '\n')
	if nl < 0 || strings.TrimRight(doc[:nl], " \t\r") != "---" {
		return "", doc, false
	}
	rest := doc[nl+1:]
	consumed := 0
	for consumed <= len(rest) {
		end := strings.IndexByte(rest[consumed:], '\n')
		var ln string
		if end < 0 {
			ln = rest[consumed:]
		} else {
			ln = rest[consumed : consumed+end+1]
		}
		if strings.TrimRight(ln, " \t\r\n") == "---" {
			return rest[:consumed], rest[consumed+len(ln):], true
		}
		if end < 0 {
			break
		}
		consumed += end + 1
	}
	return "", doc, false
}

// ParseDocument splits doc and parses its frontmatter.
//
// The block is parsed as YAML;
// when that fails it is parsed again after (a) double-quoting top-level plain
// values that hold YAML-special characters
// and (b) replacing leading tabs with two spaces each. If that also fails the
// frontmatter is ignored (empty Data, Err set) and the body is still returned.
func ParseDocument(doc string) Document {
	block, body, ok := Split(doc)
	if !ok {
		return Document{Data: map[string]any{}, Body: body}
	}
	d := Document{Data: map[string]any{}, Body: body, HasFrontmatter: true, Block: block}
	data, err := Parse(block)
	if err != nil && !errors.Is(err, ErrNotTable) {
		retry := tabsToSpaces(quoteLossyValues(block))
		data, err = Parse(retry)
	}
	switch {
	case err == nil:
		d.Data = data
	case errors.Is(err, ErrNotTable):
		d.Problem = "is a list or a single value, not a table"
	default:
		d.Err = fileLineError(err)
	}
	return d
}

// fileLineError shifts a block-relative SyntaxError to file line numbers (the
// block starts on line 2, after the opening fence).
func fileLineError(err error) error {
	var se *SyntaxError
	if errors.As(err, &se) {
		return &SyntaxError{Line: se.Line + 1, Msg: se.Msg}
	}
	return err
}

var (
	reLossyLine = regexp.MustCompile(`^([a-zA-Z_-]+):\s+(\S.*)$`)
	reLossyChar = regexp.MustCompile("[{}\\[\\]*&#!|>%@`]|: ")
	reLeadTabs  = regexp.MustCompile(`(?m)^\t+`)
)

// quoteLossyValues rewrites a top-level `key: value` whose
// unquoted value contains a YAML-special character as a
// double-quoted string so it reads literally.
func quoteLossyValues(block string) string {
	lines := strings.Split(block, "\n")
	for i, ln := range lines {
		cr := strings.HasSuffix(ln, "\r")
		t := strings.TrimSuffix(ln, "\r")
		m := reLossyLine.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		v := strings.TrimRight(m[2], " \t")
		if (strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`)) || (strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'")) {
			continue
		}
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			if pv, err := ParseValue(v); err == nil {
				if _, isList := pv.([]any); isList {
					continue
				}
			}
		}
		if !reLossyChar.MatchString(v) {
			continue
		}
		q := strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`)
		lines[i] = m[1] + `: "` + q + `"`
		if cr {
			lines[i] += "\r"
		}
	}
	return strings.Join(lines, "\n")
}

func tabsToSpaces(block string) string {
	return reLeadTabs.ReplaceAllStringFunc(block, func(t string) string {
		return strings.Repeat("  ", len(t))
	})
}

// --- value coercion helpers (JavaScript-compatible) ---

// String renders a frontmatter value the way JavaScript's String() does for
// the types YAML produces: strings as-is, booleans "true"/"false", numbers in
// shortest form (1.0 -> "1"), arrays comma-joined, null/undefined -> "".
// Mappings render as "".
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, 1):
			return "Infinity"
		case math.IsInf(x, -1):
			return "-Infinity"
		case math.Abs(x) >= 1e21 || (x != 0 && math.Abs(x) < 1e-6):
			return strconv.FormatFloat(x, 'g', -1, 64)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = String(e)
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(x, ",")
	}
	return ""
}

// Scalar returns the string form of a scalar value (string, bool or number)
// and false for null, lists and mappings.
func Scalar(v any) (string, bool) {
	switch v.(type) {
	case string, bool, int, int64, float64:
		return String(v), true
	}
	return "", false
}

// Bool coerces a boolean-ish frontmatter value. ok is false when the value is
// absent or unrecognised.
func Bool(v any) (val, ok bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string, int, int64, float64:
		s := strings.ToLower(strings.TrimSpace(String(x)))
		switch s {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		}
	}
	return false, false
}

// ToolList coerces a tool-list field ("allowed-tools" and friends), which may
// be a string or a YAML list. present is false for an absent/null value. A
// "*" entry collapses the list to ["*"].
func ToolList(v any) (list []string, present bool) {
	if v == nil {
		return nil, false
	}
	var raw []string
	switch x := v.(type) {
	case string:
		if x == "" {
			return []string{}, true
		}
		raw = []string{x}
	case bool:
		if !x {
			return []string{}, true
		}
		return []string{}, true
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				raw = append(raw, s)
			}
		}
	case []string:
		raw = x
	default:
		return []string{}, true
	}
	out := SplitTools(raw)
	for _, s := range out {
		if s == "*" {
			return []string{"*"}, true
		}
	}
	return out, true
}

// SplitTools splits tool specs on commas and spaces that are outside
// parentheses, so "Bash(git add:*), Read Write" -> [Bash(git add:*) Read
// Write].
func SplitTools(in []string) []string {
	out := []string{}
	for _, t := range in {
		var cur strings.Builder
		depth := 0
		flush := func() {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
		for _, r := range t {
			switch {
			case r == '(':
				depth++
				cur.WriteRune(r)
			case r == ')':
				if depth > 0 {
					depth--
				}
				cur.WriteRune(r)
			case (r == ',' || r == ' ' || r == '\t' || r == '\n') && depth == 0:
				flush()
			default:
				cur.WriteRune(r)
			}
		}
		flush()
	}
	return out
}
