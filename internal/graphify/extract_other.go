package graphify

import (
	"regexp"
	"strings"
)

// sqlIdent is one possibly quoted, possibly schema-qualified SQL name.
const sqlIdent = "(?:[\\w$]+|\"[^\"]+\"|\\[[^\\]]+\\]|`[^`]+`)(?:\\s*\\.\\s*(?:[\\w$]+|\"[^\"]+\"|\\[[^\\]]+\\]|`[^`]+`))*"

var (
	sqlCreateRe = regexp.MustCompile(`(?i)^\s*create\s+(?:or\s+(?:replace|alter)\s+)?(?:(?:temp|temporary|unlogged|materialized|unique|global|external|virtual)\s+)*` +
		`(table|view|function|procedure|proc|trigger|index|type)\s+(?:if\s+not\s+exists\s+)?(` + sqlIdent + `)`)
	sqlRefRe = regexp.MustCompile(`(?i)\b(from|join|references|into|update|table|on)\s+(?:only\s+)?(` + sqlIdent + `)`)
	// words that follow FROM / JOIN / ON without naming a table
	sqlNotName = map[string]bool{"select": true, "lateral": true, "unnest": true, "values": true, "set": true, "delete": true,
		"update": true, "insert": true, "conflict": true, "commit": true, "if": true, "exists": true, "not": true, "each": true}
)

// sqlName is the bare name of a possibly quoted, qualified identifier:
// [dbo].[Customer] and "public"."customer" are Customer and customer.
func sqlName(ident string) string {
	if i := strings.LastIndex(ident, "."); i >= 0 {
		ident = ident[i+1:]
	}
	return strings.Trim(strings.TrimSpace(ident), "\"`[]")
}

// extractSQL reads the schema a SQL file declares: tables, views and types
// (as types), functions and procedures (as functions), and what each one
// refers to — foreign keys, the tables a view or routine reads and writes,
// the routines it calls. A statement outside any of those belongs to the
// file.
func extractSQL(rel string, src []byte) *FileExtraction {
	fe := newFileExtraction(rel, "sql", "sql", "code")
	code, _ := scrub(src, sqlSpec)
	current, currentName, routine := rel, "", false
	for i, line := range strings.Split(string(code), "\n") {
		lineNo := i + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := sqlCreateRe.FindStringSubmatch(line); m != nil {
			name := sqlName(m[2])
			current, currentName, routine = rel, "", false
			switch strings.ToLower(m[1]) {
			case "table", "view", "type":
				current, currentName = fe.addDef(KindType, name, "", lineNo), name
			case "function", "procedure", "proc":
				current, currentName, routine = fe.addDef(KindFunction, name, "", lineNo), name, true
			}
		}
		for _, m := range sqlRefRe.FindAllStringSubmatch(line, -1) {
			name := sqlName(m[2])
			if name == "" || strings.EqualFold(name, currentName) || sqlNotName[strings.ToLower(name)] {
				continue
			}
			relation := "uses"
			if strings.EqualFold(m[1], "references") {
				relation = "references"
			}
			fe.addRef(Ref{From: current, Name: name, Relation: relation, Line: lineNo})
		}
		for _, c := range findCalls(line, nil) {
			if name := line[c[0]:c[1]]; !strings.EqualFold(name, currentName) {
				fe.addRef(Ref{From: current, Name: name, Relation: "calls", Line: lineNo})
			}
		}
		// A table or view ends with its statement; a routine's body holds
		// statements of its own and runs to the next CREATE.
		if !routine && strings.Contains(line, ";") {
			current, currentName = rel, ""
		}
	}
	return fe
}

var scriptBlockRe = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script\s*>`)

// extractEmbeddedJS reads the <script> blocks of a single-file component
// (.vue, .svelte) as JavaScript / TypeScript. Everything outside them is
// blanked, so line numbers stay those of the file.
func extractEmbeddedJS(rel string, src []byte) *FileExtraction {
	masked := make([]byte, len(src))
	for i, c := range src {
		masked[i] = ' '
		if c == '\n' {
			masked[i] = '\n'
		}
	}
	for _, m := range scriptBlockRe.FindAllSubmatchIndex(src, -1) {
		copy(masked[m[2]:m[3]], src[m[2]:m[3]])
	}
	fe := extractGeneric(rel, masked, specForExt(".ts"))
	fe.Lang = strings.TrimPrefix(strings.ToLower(rel[strings.LastIndex(rel, "."):]), ".")
	return fe
}
