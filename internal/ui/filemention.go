package ui

import (
	"strings"
)

// FileCandidate is one row of the composer "@" file-mention autocomplete menu.
// The host (internal/cli) walks the working tree once at session start and
// supplies the full list via SetFiles / SetFilesMsg; the UI filters and renders
// it as the user types an "@<query>" token.
//
//   - Path is the candidate path RELATIVE to the working directory, using "/"
//     separators and WITHOUT a trailing slash even for a directory (the menu
//     adds the slash for display and on insert). It is both what the fuzzy
//     filter matches and what is inserted after the "@".
//   - IsDir marks a directory. A directory is shown with a trailing "/" and,
//     when chosen, is inserted as "@<path>/" with NO trailing space so the menu
//     stays open and the user can drill into it; a file is inserted as
//     "@<path> " (trailing space) and closes the menu.
type FileCandidate struct {
	Path  string
	IsDir bool
}

// fileMenuMaxRows caps how many file rows are drawn at once; a longer filtered
// set scrolls (windows) so the selected row stays visible. As with the "/" menu
// this is a fixed representative window rather than a height-derived count.
const fileMenuMaxRows = 10

// FileTrigger is the character that opens the file-mention menu. Unlike the "/"
// command trigger it is NOT anchored to the buffer start: an "@" preceded by
// whitespace (or at the start) anywhere in the line opens it.
const FileTrigger = '@'

// isMentionPathRune reports whether ch may appear in an "@" mention path token.
func isMentionPathRune(ch rune) bool {
	switch {
	case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		return true
	case ch == '_' || ch == '-' || ch == '.' || ch == '/' || ch == '\\':
		return true
	case ch == '(' || ch == ')' || ch == '[' || ch == ']' || ch == '~' || ch == ':':
		return true
	case ch > 0x7f:
		// Accept non-ASCII runes (letters/marks in other scripts) — the
		// \p{L}\p{N}\p{M} classes. Control runes never reach the composer buffer.
		return true
	default:
		return false
	}
}

// isMentionBoundary reports whether ch is a rune that may immediately precede the
// triggering "@" (start-of-word boundary): a
// whitespace class; "@" is a valid mention start only at index 0 or just after
// whitespace, so "foo@bar" (email-like) does NOT trigger.
func isMentionBoundary(ch rune) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

// fileMention locates the "@<query>" mention token the composer currently
// expresses, if any. It returns the query (the path fragment after "@"), the
// rune offset of the triggering "@", the rune offset just past the end of the
// token, and whether a mention is active.
//
// A mention is active when, scanning the text before the cursor, the nearest
// "@" that begins a mention token (at the buffer start or just after whitespace)
// is found AND every rune from that "@" up to the cursor is a path rune. The
// token is then extended forward past the cursor over any further path runes, so
// editing in the middle of a mention still targets the whole token.
func fileMention(c Composer) (query string, start, end int, ok bool) {
	r := []rune(c.Value)
	cur := c.Cursor
	if cur < 0 {
		cur = 0
	}
	if cur > len(r) {
		cur = len(r)
	}
	// Find the nearest boundary-anchored "@" at or before the cursor.
	at := -1
	for i := cur - 1; i >= 0; i-- {
		if r[i] == FileTrigger {
			if i == 0 || isMentionBoundary(r[i-1]) {
				at = i
			}
			break
		}
		if !isMentionPathRune(r[i]) {
			// A non-path rune between here and the cursor means no mention token
			// reaches the cursor.
			break
		}
	}
	if at < 0 {
		return "", 0, 0, false
	}
	// Every rune from just after "@" up to the cursor must be a path rune.
	for i := at + 1; i < cur; i++ {
		if !isMentionPathRune(r[i]) {
			return "", 0, 0, false
		}
	}
	// Extend the token forward from the cursor over further path runes.
	e := cur
	for e < len(r) && isMentionPathRune(r[e]) {
		e++
	}
	return string(r[at+1 : e]), at, e, true
}

// FileMenuActive reports whether the "@" file-mention menu should be shown: a
// mention token is active, the host has supplied candidates, the menu was not
// dismissed (Esc), and at least one candidate matches the query. The "/" command
// menu takes precedence (a buffer beginning with "/" drives commands), so the two
// overlays never show at once.
func (vs ViewState) FileMenuActive() bool {
	if vs.FileMenuDismissed || len(vs.Files) == 0 {
		return false
	}
	if vs.CommandMenuActive() {
		return false
	}
	if _, _, _, ok := fileMention(vs.Composer); !ok {
		return false
	}
	return len(vs.FilteredFiles()) > 0
}

// FilteredFiles returns the file candidates matching the current mention query,
// ranked best-match first (see rankFiles). An empty query (just "@") returns
// every candidate in host order. The returned set is capped so a huge index
// never produces an unbounded menu.
func (vs ViewState) FilteredFiles() []FileCandidate {
	hits := vs.fileHits()
	if hits == nil {
		return nil
	}
	out := make([]FileCandidate, len(hits))
	for i := range hits {
		out[i] = hits[i].c
	}
	return out
}

// fileHits returns the ranked matches for the current mention query, with the
// matched rune positions of each path for highlighting (nil for an empty
// query).
func (vs ViewState) fileHits() []fileHit {
	q, _, _, ok := fileMention(vs.Composer)
	if !ok {
		return nil
	}
	q = strings.TrimSpace(q)
	if q == "" {
		files := capFiles(vs.Files)
		out := make([]fileHit, len(files))
		for i, c := range files {
			out[i] = fileHit{c: c}
		}
		return out
	}
	return rankFilesCached(q, vs.Files)
}

// maxFilteredFiles bounds the ranked set; the window (VisibleFiles) scrolls
// within it.
const maxFilteredFiles = 200

// capFiles bounds the filtered set to a sane maximum so the fuzzy menu never
// walks an unbounded list; the window (VisibleFiles) scrolls within it.
func capFiles(in []FileCandidate) []FileCandidate {
	if len(in) > maxFilteredFiles {
		return in[:maxFilteredFiles]
	}
	return in
}

// FileMenuSelection returns the highlighted row index within the filtered file
// set, clamped into range (0 when empty).
func (vs ViewState) FileMenuSelection() int {
	n := len(vs.FilteredFiles())
	if n == 0 {
		return 0
	}
	sel := vs.FileMenuSelected
	if sel < 0 {
		sel = 0
	}
	if sel >= n {
		sel = n - 1
	}
	return sel
}

// SelectedFileCandidate returns the currently highlighted candidate, if any.
func (vs ViewState) SelectedFileCandidate() (FileCandidate, bool) {
	items := vs.FilteredFiles()
	if len(items) == 0 {
		return FileCandidate{}, false
	}
	return items[vs.FileMenuSelection()], true
}

// VisibleFiles returns the windowed slice of filtered candidates currently drawn
// and the index (within the filtered set) of the first returned row. The window
// of fileMenuMaxRows rows scrolls to keep the selected row visible.
func (vs ViewState) VisibleFiles() ([]FileCandidate, int) {
	items := vs.FilteredFiles()
	if len(items) <= fileMenuMaxRows {
		return items, 0
	}
	sel := vs.FileMenuSelection()
	start := sel - fileMenuMaxRows + 1
	if start < 0 {
		start = 0
	}
	end := start + fileMenuMaxRows
	if end > len(items) {
		end = len(items)
		start = end - fileMenuMaxRows
		if start < 0 {
			start = 0
		}
	}
	return items[start:end], start
}

// VisibleFileMatches returns, for each row VisibleFiles returns, the rune
// positions of the path the query matched (nil when nothing is highlighted).
func (vs ViewState) VisibleFileMatches() [][]int {
	hits := vs.fileHits()
	items, start := vs.VisibleFiles()
	out := make([][]int, len(items))
	for i := range items {
		if start+i < len(hits) {
			out[i] = hits[start+i].pos
		}
	}
	return out
}

// FileMentionDisplay is the label a candidate shows in the menu: the path, with a
// trailing "/" for a directory.
func (c FileCandidate) FileMentionDisplay() string {
	if c.IsDir {
		return c.Path + "/"
	}
	return c.Path
}
