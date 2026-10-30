package projectmemory

import (
	"regexp"
	"strings"
)

// importRe matches an @import marker: an "@" at start-of-text or after
// whitespace, followed by a path in which spaces may be escaped with a
// backslash.
var importRe = regexp.MustCompile(`(?:^|\s)@((?:[^\s\\]|\\ )+)`)

// specialPrefixRe rejects import candidates whose first run is punctuation.
var specialPrefixRe = regexp.MustCompile(`^[#%^&*()]+`)

// firstCharRe accepts a bare (non-prefixed) path only when it starts with an
// alphanumeric, dot, underscore or dash.
var firstCharRe = regexp.MustCompile(`^[a-zA-Z0-9._-]`)

// parseImports extracts the resolved @import target paths from a memory file's
// content. baseDir is the importing file's directory (for relative imports) and
// home expands "~"/"~/".
//
// Fenced code blocks (``` and ~~~) are stripped before scanning (see package
// doc). Inline codespans are not excluded.
func parseImports(content, baseDir, home string) []string {
	scanText := stripFencedCodeBlocks(content)

	var out []string
	seen := map[string]bool{}
	for _, m := range importRe.FindAllStringSubmatch(scanText, -1) {
		raw := m[1]
		if raw == "" {
			continue
		}
		// Drop a "#fragment" suffix.
		if i := strings.IndexByte(raw, '#'); i != -1 {
			raw = raw[:i]
		}
		if raw == "" {
			continue
		}
		// Unescape "\ " to a literal space.
		raw = strings.ReplaceAll(raw, `\ `, " ")
		if raw == "" {
			continue
		}
		if !acceptImport(raw) {
			continue
		}
		resolved := absoluteOf(raw, baseDir, home)
		if resolved == "" {
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	return out
}

// acceptImport reports whether an @import candidate is an importable path.
func acceptImport(h string) bool {
	if isURL(h) {
		return false
	}
	switch {
	case strings.HasPrefix(h, "./"):
		return true
	case strings.HasPrefix(h, "~/"):
		return true
	case strings.HasPrefix(h, "/") && h != "/":
		return true
	case !strings.HasPrefix(h, "@") && !specialPrefixRe.MatchString(h) && firstCharRe.MatchString(h):
		return true
	default:
		return false
	}
}

// isURL reports whether h looks like a URL: a candidate carrying a scheme
// "://" is treated as a URL and never imported.
func isURL(h string) bool {
	return strings.Contains(h, "://")
}

// absoluteOf resolves an import path against a base directory and home.
func absoluteOf(p, baseDir, home string) string {
	if (p == "~" || strings.HasPrefix(p, "~/")) && home != "" {
		h := strings.TrimRight(normalizePath(home), "/")
		if p == "~" {
			return normalizePath(h)
		}
		return normalizePath(h + "/" + p[2:])
	}
	if (p == "~" || strings.HasPrefix(p, "~/")) && home == "" {
		// Cannot expand; leave unresolvable (caller notes the miss on read).
		return ""
	}
	if isAbsolute(p) {
		return normalizePath(p)
	}
	return normalizePath(baseDir + "/" + p)
}

// absoluteRe matches an absolute path, optionally prefixed by a Windows drive
// letter.
var absoluteRe = regexp.MustCompile(`^(?:[A-Za-z]:)?[\\/]`)

func isAbsolute(p string) bool { return absoluteRe.MatchString(p) }

// stripFencedCodeBlocks removes fenced code blocks so that @imports inside code
// examples are not followed. Fences are a line whose first non-space content is
// ``` or ~~~; the matching closing fence uses the same marker character.
func stripFencedCodeBlocks(content string) string {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	var fence byte // 0 when not inside a fence; '`' or '~' otherwise
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if fence == 0 {
			if marker := fenceMarker(trimmed); marker != 0 {
				fence = marker
				b.WriteByte('\n') // keep line structure, drop the fence line content
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		// Inside a fence: look for the closing marker, drop everything.
		if marker := fenceMarker(trimmed); marker == fence {
			fence = 0
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// fenceMarker returns '`' or '~' if the line opens/closes a fence (three or more
// of the same marker char), else 0.
func fenceMarker(trimmed string) byte {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == c {
			n++
		}
		if n >= 3 {
			return c
		}
	}
	return 0
}
