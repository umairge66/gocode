package toolrun

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Shell output shown to the model keeps very long lines (minified files,
// one-line JSON, bundled assets) from using up the whole inline budget: a
// line longer than longLineLimit characters is shortened to its first
// longLineKeep characters plus a note. Only the text returned to the model is
// changed; the command itself, and anything it pipes between programs, sees
// the full lines, and the complete output is saved to a file when possible.
const (
	longLineLimit = 2000
	longLineKeep  = 500
)

// cutLongLines shortens every line longer than longLineLimit characters and
// reports whether any line was shortened.
func cutLongLines(content string) (string, bool) {
	if utf8.RuneCountInString(content) <= longLineLimit {
		return content, false
	}
	lines := strings.SplitAfter(content, "\n")
	cut := false
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		n := utf8.RuneCountInString(body)
		if n <= longLineLimit {
			continue
		}
		keep, count := body, 0
		for j := range body {
			if count == longLineKeep {
				keep = body[:j]
				break
			}
			count++
		}
		nl := ""
		if strings.HasSuffix(line, "\n") {
			nl = "\n"
		}
		lines[i] = fmt.Sprintf("%s … [line cut: %d more characters]%s", keep, n-longLineKeep, nl)
		cut = true
	}
	if !cut {
		return content, false
	}
	return strings.Join(lines, ""), true
}

// capShellText applies the long-line cut and the inline size limit to shell
// output for the model. The full output is saved under dir when anything is
// shortened; framed reports that the text was replaced by a saved-output or
// truncated-output block.
func capShellText(dir, id, content string, limit int) (out string, framed bool) {
	short, cut := cutLongLines(content)
	if !cut {
		if limit == 0 || utf8.RuneCountInString(content) <= limit {
			return content, false
		}
		if path, err := persistToolResult(dir, id, content); err == nil {
			return persistedOutputMessage(path, content), true
		}
		return truncatedOutputMessage(content, limit), true
	}
	path, err := persistToolResult(dir, id, content)
	if limit != 0 && utf8.RuneCountInString(short) > limit {
		if err == nil {
			return persistedOutputMessage(path, short), true
		}
		return truncatedOutputMessage(short, limit), true
	}
	if err == nil {
		short += "\n[Some long lines were cut. Full output saved to: " + path + "]"
	}
	return short, false
}
