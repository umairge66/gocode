package fileedit

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Read token budget.
//
// A text Read is also bounded by an estimated token count (characters / 4,
// or / 2 for JSON files, which tokenize densely). The default budget is
// DefaultMaxReadTokens; GOCODE_CODE_FILE_READ_MAX_OUTPUT_TOKENS overrides it.
//
//   - A whole-file read (no offset past line 1, no limit) over the budget is
//     not refused: it returns the first page that fits, with a PARTIAL view
//     banner naming the offset/limit of the next page. A file whose lines are
//     too long to page by line returns its first characters instead.
//   - A read with offset or limit that is still over the budget fails with
//     the "exceeds maximum allowed tokens" error, so the model narrows it.
//   - allow_large lifts the budget.

// DefaultMaxReadTokens is the default token budget of one Read.
const DefaultMaxReadTokens = 25000

// maxReadTokensEnv overrides DefaultMaxReadTokens when set to a positive
// integer.
const maxReadTokensEnv = "GOCODE_CODE_FILE_READ_MAX_OUTPUT_TOKENS"

// maxReadTokens is the token budget for this tool.
func (r ReadTool) maxReadTokens() int {
	if r.MaxTokens > 0 {
		return r.MaxTokens
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(maxReadTokensEnv))); err == nil && v > 0 {
		return v
	}
	return DefaultMaxReadTokens
}

// charsPerToken is the estimate's characters-per-token ratio for a file
// extension (without the dot).
func charsPerToken(ext string) int {
	switch ext {
	case "json", "jsonl", "jsonc":
		return 2
	}
	return 4
}

// estimateReadTokens is the rough token count of s.
func estimateReadTokens(s, ext string) int {
	return int(math.Round(float64(utf8.RuneCountInString(s)) / float64(charsPerToken(ext))))
}

// readTokensExceeded is the error for a narrowed read still over budget.
func readTokensExceeded(tokens, limit int) error {
	return fmt.Errorf("File content (%d tokens) exceeds maximum allowed tokens (%d). Use offset and limit parameters to read specific portions of the file, or search for specific content instead of reading the whole file.", tokens, limit)
}

// tokenPage is the first page of an over-budget whole-file read.
type tokenPage struct {
	lines   []string // the lines shown (the last one may be cut mid-line)
	byChars bool     // the page was cut by characters, not whole lines
	chars   int      // characters shown (byChars)
}

// firstTokenPage picks the leading lines of content (already split into
// lines) whose estimate fits limit: start from the proportional share with a
// 15% margin and shrink by 30% up to six times; if even that does not fit (or
// is blank), cut by characters instead.
func firstTokenPage(lines []string, content string, tokens, limit int) tokenPage {
	n := utf8.RuneCountInString(content)
	ratio := math.Max(0.5, float64(n)/math.Max(1, float64(tokens)))
	est := func(s string) float64 { return float64(utf8.RuneCountInString(s)) / ratio }

	count := max(1, min(len(lines), int(math.Floor(float64(len(lines))*float64(limit)/math.Max(1, float64(tokens))*0.85))))
	page := strings.Join(lines[:count], "\n")
	for i := 0; i < 6; i++ {
		if est(page) <= float64(limit) || count <= 1 {
			break
		}
		count = max(1, int(math.Floor(float64(count)*0.7)))
		page = strings.Join(lines[:count], "\n")
	}
	if est(page) <= float64(limit) && strings.TrimSpace(page) != "" {
		return tokenPage{lines: lines[:count]}
	}

	rs := []rune(content)
	keep := max(1, int(math.Floor(float64(limit)*ratio*0.85)))
	for i := 0; i < 6; i++ {
		page = string(rs[:min(keep, len(rs))])
		if est(page) <= float64(limit) {
			break
		}
		keep = max(1, int(math.Floor(float64(keep)*0.7)))
	}
	return tokenPage{lines: strings.Split(page, "\n"), byChars: true, chars: utf8.RuneCountInString(page)}
}

// tokenPageBanner is the PARTIAL view notice of an over-budget whole-file read.
// search names what finds a specific section ("Grep", or "`grep` in Bash").
func tokenPageBanner(path string, p tokenPage, totalLines, totalChars, tokens, limit int, search string) string {
	if !p.byChars && len(p.lines) < totalLines {
		n := len(p.lines)
		return partialViewPrefix + fmt.Sprintf("%s: showing lines 1-%d of %d total (%d tokens, cap %d). Call %s with offset=%d limit=%d for the next page, or %s to find a specific section. Do NOT answer from this page alone if the answer may be further in the file.]",
			path, n, totalLines, tokens, limit, ToolNameRead, n+1, n, search)
	}
	return partialViewPrefix + fmt.Sprintf("%s: showing the first %d of %d characters (%d tokens, cap %d); this file has very long lines and cannot be paginated by line. Use %s to find a specific section, or %s with offset/limit to page through it. Do NOT answer from this excerpt alone if the answer may be elsewhere in the file.]",
		path, p.chars, totalChars, tokens, limit, search, ToolNameRead)
}
