package configimport

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Format helpers: the YAML frontmatter emitter/splitter and the JSONC reader
// the importer needs (the TOML reader lives in toml.go).
//
// Codex config.toml and Gemini command .toml files are TOML; Cursor mcp.json
// and Gemini settings.json may be JSONC. Go's stdlib has no TOML and no JSON5
// and go.mod may not gain a dependency, so these are stdlib-only readers.
// parseJSONC covers JSON plus // and /* */ comments and trailing commas (what Cursor/Gemini write); JSON5-only syntax (unquoted keys,
// single-quoted strings, hex numbers) is rejected as a parse error, which the
// importers surface as an "unexpected structure" unmappable item rather than
// guessing.

// fmField is one ordered frontmatter entry. Val is string, bool, or []string.
type fmField struct {
	key string
	val any
}

// emitFrontmatter renders ordered fields as a YAML frontmatter body (the text
// that sits between the `---` fences). A single-element []string is emitted as a
// scalar; longer slices use flow style.
func emitFrontmatter(fields []fmField) string {
	var b strings.Builder
	for _, f := range fields {
		switch v := f.val.(type) {
		case string:
			b.WriteString(f.key + ": " + yamlScalar(v) + "\n")
		case bool:
			b.WriteString(f.key + ": " + strconv.FormatBool(v) + "\n")
		case []string:
			if len(v) == 1 {
				b.WriteString(f.key + ": " + yamlScalar(v[0]) + "\n")
				continue
			}
			parts := make([]string, len(v))
			for i, s := range v {
				parts[i] = yamlScalar(s)
			}
			b.WriteString(f.key + ": [" + strings.Join(parts, ", ") + "]\n")
		}
	}
	return b.String()
}

// yamlScalar quotes a scalar when YAML would otherwise mis-parse it.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`\n") || strings.TrimSpace(s) != s {
		return strconv.Quote(s)
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off":
		return strconv.Quote(s)
	}
	return s
}

// frontmatter is the result of splitting a markdown file's leading `---` block.
// order preserves the keys' source order (frontmatter is re-emitted in
// insertion order when rewriting a skill's frontmatter).
type frontmatter struct {
	fields     map[string]any
	order      []string
	content    string
	parseError bool
	// hadFrontmatter is false when the input had no leading --- block.
	hadFrontmatter bool
}

// splitFrontmatter separates a leading `---`-fenced YAML block from the body.
// When there is no frontmatter, fields is empty and content is the whole input.
func splitFrontmatter(raw string) frontmatter {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") && s != "---" {
		return frontmatter{fields: map[string]any{}, content: s}
	}
	rest := strings.TrimPrefix(s, "---\n")
	// closing fence: a line that is exactly "---".
	idx := indexFence(rest)
	if idx == -1 {
		return frontmatter{fields: map[string]any{}, content: s}
	}
	block := rest[:idx]
	body := rest[idx:]
	body = strings.TrimPrefix(body, "---")
	body = strings.TrimPrefix(body, "\n")
	fields, order, err := parseYAMLBlock(block)
	if err != nil {
		return frontmatter{parseError: true}
	}
	return frontmatter{fields: fields, order: order, content: body, hadFrontmatter: true}
}

func indexFence(s string) int {
	off := 0
	for {
		nl := strings.IndexByte(s[off:], '\n')
		var line string
		if nl == -1 {
			line = s[off:]
		} else {
			line = s[off : off+nl]
		}
		if strings.TrimRight(line, " \t") == "---" {
			return off
		}
		if nl == -1 {
			return -1
		}
		off += nl + 1
	}
}

// parseYAMLBlock parses the YAML-subset used by rule/skill frontmatter: scalar
// key: value pairs, inline flow arrays `[a, b]`, and block lists.
func parseYAMLBlock(block string) (map[string]any, []string, error) {
	out := map[string]any{}
	var order []string
	set := func(k string, v any) {
		if _, ok := out[k]; !ok {
			order = append(order, k)
		}
		out[k] = v
	}
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			// stray indented line with no owning key: ignore.
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon == -1 {
			return nil, nil, fmt.Errorf("invalid frontmatter line: %q", line)
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		if val == "" {
			// possible block list on following indented lines.
			var items []string
			for i+1 < len(lines) {
				next := lines[i+1]
				t := strings.TrimSpace(next)
				if strings.HasPrefix(t, "- ") || t == "-" {
					items = append(items, scalarValue(strings.TrimSpace(strings.TrimPrefix(t, "-"))))
					i++
					continue
				}
				if next != "" && (strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\t")) {
					i++
					continue
				}
				break
			}
			if items != nil {
				set(key, items)
			} else {
				set(key, "")
			}
			continue
		}
		if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
			set(key, parseFlowArray(val))
			continue
		}
		set(key, typedScalar(scalarValue(val)))
	}
	return out, order, nil
}

func parseFlowArray(v string) []string {
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return []string{}
	}
	parts := splitTopLevel(inner, ',')
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, scalarValue(strings.TrimSpace(p)))
	}
	return out
}

// scalarValue unquotes a YAML scalar.
func scalarValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"') {
		if uq, err := strconv.Unquote(v); err == nil {
			return uq
		}
	}
	if len(v) >= 2 && (v[0] == '\'' && v[len(v)-1] == '\'') {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

// typedScalar coerces bare YAML scalars to bool where applicable; else string.
func typedScalar(v string) any {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	return v
}

// splitTopLevel splits on sep, ignoring sep inside quotes/brackets.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	depth := 0
	inBasic, inLiteral := false, false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && !inLiteral:
			inBasic = !inBasic
		case c == '\'' && !inBasic:
			inLiteral = !inLiteral
		case (c == '[' || c == '{') && !inBasic && !inLiteral:
			depth++
		case (c == ']' || c == '}') && !inBasic && !inLiteral:
			depth--
		case c == sep && depth == 0 && !inBasic && !inLiteral:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// --- minimal JSONC / JSON5 -------------------------------------------------

// parseJSONC strips // and /* */ comments and trailing commas, then unmarshals.
// It is not a full JSON5 parser (no unquoted keys, single-quoted strings or
// hex); real mcp.json / settings.json files use plain JSON with
// comments, which this covers.
func parseJSONC(input string, out any) error {
	cleaned := stripJSONComments(input)
	cleaned = stripTrailingCommas(cleaned)
	return json.Unmarshal([]byte(cleaned), out)
}

func stripJSONComments(s string) string {
	var b strings.Builder
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++ // skip the '/'
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func stripTrailingCommas(s string) string {
	var b strings.Builder
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // drop trailing comma
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}
