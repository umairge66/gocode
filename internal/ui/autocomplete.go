package ui

import (
	"sort"
	"strings"
)

// Suggestion is one row of the composer slash-command autocomplete menu. The
// host (internal/cli) supplies the full list once at session start (and refreshes
// it when the set changes) via SetCommandsMsg / SetCommandsCmd; the UI filters
// and renders it as the user types.
//
//   - Name is the bare command name without the leading slash (e.g. "model").
//     It is what the filter query is matched against.
//   - Display is the user-facing label shown in the menu (e.g. "/model").
//   - Description is the dimmed one-line summary shown after the name.
//   - Value is the text the composer is completed to when the row is chosen
//     (e.g. "/model"); the completer appends a trailing space.
//   - ArgumentHint, when non-empty, marks a command that expects arguments (the
//     "argument-hint" frontmatter). Enter on
//     such a row completes it into the composer (so the user can type the args)
//     rather than running it with no arguments; a bare no-arg command runs on the
//     first Enter.
type Suggestion struct {
	Name         string
	Display      string
	Description  string
	Value        string
	ArgumentHint string
	// Kind groups the entry in the modern command palette: "" for a command,
	// "skill" for a skill, "mcp" for an MCP prompt.
	Kind string
}

// commandMenuMaxRows caps how many suggestion rows are drawn at once; a longer
// filtered set scrolls (windows) so the selected row stays visible.
const commandMenuMaxRows = 10

// CommandTrigger is the character that opens the command menu when it begins the
// composer buffer. A seam is left for other triggers such as "@" (file) and
// "!" (bash); only "/" is implemented here.
const CommandTrigger = '/'

// commandQuery returns the command-name query the composer currently expresses
// and whether the slash-command trigger is active. The trigger is active when:
//
//   - the buffer starts with "/", and
//   - the cursor is within the leading command token (the run of non-space runes
//     after the "/", before any argument) — i.e. the user is still typing the
//     command name, not its arguments.
//
// The returned query is the token after the "/" (lower-cased by the caller as
// needed). Once a space is typed after the command name (so the user has moved
// on to arguments) the trigger is no longer active and the menu closes.
func commandQuery(c Composer) (string, bool) {
	r := []rune(c.Value)
	if len(r) == 0 || r[0] != CommandTrigger {
		return "", false
	}
	// End of the command token: first space, or end of buffer.
	end := len(r)
	for i := 1; i < len(r); i++ {
		if r[i] == ' ' || r[i] == '\t' {
			end = i
			break
		}
	}
	cur := c.Cursor
	if cur < 0 {
		cur = 0
	}
	if cur > len(r) {
		cur = len(r)
	}
	// Active only while the cursor sits inside the command token (1..end).
	if cur < 1 || cur > end {
		return "", false
	}
	// The command name may only contain [a-zA-Z0-9.:_-]; any other character
	// closes the menu (it is no longer a slash command).
	token := r[1:end]
	for _, ch := range token {
		if !isCommandNameRune(ch) {
			return "", false
		}
	}
	return string(token), true
}

// isCommandNameRune reports whether ch is allowed in a slash-command name.
func isCommandNameRune(ch rune) bool {
	switch {
	case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		return true
	case ch == '.' || ch == ':' || ch == '-' || ch == '_':
		return true
	default:
		return false
	}
}

// CommandMenuActive reports whether the slash-command menu should be shown: the
// trigger is active, the menu has not been dismissed (Esc), and at least one
// command matches the current query.
func (vs ViewState) CommandMenuActive() bool {
	if vs.MenuDismissed || len(vs.Commands) == 0 {
		return false
	}
	_, ok := commandQuery(vs.Composer)
	if !ok {
		return false
	}
	return len(vs.FilteredCommands()) > 0
}

// FilteredCommands returns the suggestions matching the current command query,
// ordered by menu rank. An empty query (just "/")
// returns every command in the host-provided order (builtins, then custom
// commands). For a non-empty query the match is case-insensitive and fuzzy: a
// command matches when the query is an exact name, a name prefix, a subsequence
// of the name, or the prefix of a word in its description. Matches are ranked
// exact-name first, then name-prefix, then subsequence, then description-word,
// with the shorter name winning ties (then original order).
func (vs ViewState) FilteredCommands() []Suggestion {
	q, ok := commandQuery(vs.Composer)
	if !ok {
		return nil
	}
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return vs.Commands
	}
	type scored struct {
		s    Suggestion
		rank int
		tie  int
		idx  int
	}
	var matched []scored
	for i, s := range vs.Commands {
		if r, ok := matchRank(q, strings.ToLower(s.Name), strings.ToLower(s.Description)); ok {
			matched = append(matched, scored{s: s, rank: r, tie: len([]rune(s.Name)), idx: i})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].rank != matched[j].rank {
			return matched[i].rank < matched[j].rank
		}
		if matched[i].tie != matched[j].tie {
			return matched[i].tie < matched[j].tie
		}
		return matched[i].idx < matched[j].idx
	})
	out := make([]Suggestion, len(matched))
	for i := range matched {
		out[i] = matched[i].s
	}
	return out
}

// matchRank scores a (lower-cased) command name/description against a lower-cased
// query, returning the match rank (lower is better) and whether it matched:
//
//	0 — the name equals the query exactly
//	1 — the name starts with the query
//	2 — the query is a subsequence of the name (fuzzy)
//	3 — a word in the description starts with the query
func matchRank(q, name, desc string) (int, bool) {
	switch {
	case name == q:
		return 0, true
	case strings.HasPrefix(name, q):
		return 1, true
	case isSubsequence(q, name):
		return 2, true
	}
	for _, w := range strings.Fields(desc) {
		if strings.HasPrefix(w, q) {
			return 3, true
		}
	}
	return 0, false
}

// isSubsequence reports whether every rune of q appears in s in order (not
// necessarily contiguously) — the lightweight fuzzy name match.
func isSubsequence(q, s string) bool {
	if q == "" {
		return true
	}
	qr := []rune(q)
	i := 0
	for _, r := range s {
		if r == qr[i] {
			i++
			if i == len(qr) {
				return true
			}
		}
	}
	return false
}

// MenuSelection returns the index of the highlighted row within the filtered set,
// clamped into range. It is 0 when the menu is empty.
func (vs ViewState) MenuSelection() int {
	n := len(vs.FilteredCommands())
	if n == 0 {
		return 0
	}
	sel := vs.MenuSelected
	if sel < 0 {
		sel = 0
	}
	if sel >= n {
		sel = n - 1
	}
	return sel
}

// MenuEnterSubmits reports whether the typed command token already equals a
// command name exactly (case-insensitive) — i.e. the user has typed a full,
// valid command. It is a query helper retained for callers; the Enter key now
// RUNS the highlighted suggestion in one press regardless (model.go), so this is
// no longer what drives Enter.
func (vs ViewState) MenuEnterSubmits() bool {
	q, ok := commandQuery(vs.Composer)
	if !ok || q == "" {
		return false
	}
	for _, s := range vs.Commands {
		if strings.EqualFold(s.Name, q) {
			return true
		}
	}
	return false
}

// SelectedSuggestion returns the currently highlighted suggestion, if any.
func (vs ViewState) SelectedSuggestion() (Suggestion, bool) {
	items := vs.FilteredCommands()
	if len(items) == 0 {
		return Suggestion{}, false
	}
	return items[vs.MenuSelection()], true
}

// VisibleCommands returns the windowed slice of filtered suggestions currently
// drawn and the index (within the filtered set) of the first returned row. The
// window of commandMenuMaxRows rows scrolls to keep the selected row visible.
func (vs ViewState) VisibleCommands() ([]Suggestion, int) {
	items := vs.FilteredCommands()
	if len(items) <= commandMenuMaxRows {
		return items, 0
	}
	sel := vs.MenuSelection()
	start := sel - commandMenuMaxRows + 1
	if start < 0 {
		start = 0
	}
	end := start + commandMenuMaxRows
	if end > len(items) {
		end = len(items)
		start = end - commandMenuMaxRows
		if start < 0 {
			start = 0
		}
	}
	return items[start:end], start
}
