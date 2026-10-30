package ui

import "strings"

// DialogKind identifies the active modal dialog overlaid on the transcript.
type DialogKind string

const (
	// DialogPermission asks whether to allow a tool action
	// ("Do you want to allow ...?", "Do you want to proceed?").
	DialogPermission DialogKind = "permission"
	// DialogEnterPlan is the "Enter plan mode?" confirmation.
	DialogEnterPlan DialogKind = "enter_plan"
	// DialogExitPlan is the "Exit plan mode?" confirmation.
	DialogExitPlan DialogKind = "exit_plan"
	// DialogAsk is an AskUserQuestion prompt: one question's options presented for
	// selection (single-select, or multi-select when MultiSelect is set).
	DialogAsk DialogKind = "ask"
	// DialogList is a generic scrollable, type-to-filter select list: the model
	// picker and the MCP-server list. It presents Items (not Options) with a
	// filter query, a scroll window for long lists, up/down to move, Enter to
	// choose the highlighted item's Value, and Esc to cancel.
	DialogList DialogKind = "list"
	// DialogSecret is a single-line masked text entry (the /login key prompt): the
	// typed characters are rendered as mask glyphs, never the raw secret, Enter
	// submits the entered value and Esc cancels.
	DialogSecret DialogKind = "secret"
)

// ListItem is one row of a DialogList (a filterable select list). Label is the
// user-facing line, Value is the stable token the host acts on when the row is
// chosen, and Detail is an optional dimmed trailing description (e.g. a model's
// provider/price, or an MCP server's status).
type ListItem struct {
	Label  string
	Value  string
	Detail string
}

// DialogOption is one selectable answer in a dialog. Value is the stable
// answer token the REPL acts on; Label is the user-facing line.
type DialogOption struct {
	Label string
	Value string
}

// Dialog is the view state of a modal permission/confirmation prompt. Selected
// is the index of the highlighted option.
type Dialog struct {
	Kind     DialogKind
	Title    string
	Body     []string
	Options  []DialogOption
	Selected int

	// MultiSelect allows marking several options before confirming (space toggles
	// the highlighted option, Enter confirms the marked set). It applies to
	// DialogAsk prompts whose question set multiSelect; permission/confirmation
	// dialogs leave it false and behave as a single-choice prompt.
	MultiSelect bool
	// Marked records, per option index, whether it is toggled on. It is only
	// consulted for a MultiSelect dialog and is lazily sized to len(Options).
	Marked []bool
	// AllowNone makes a MultiSelect dialog with nothing marked confirm with an
	// empty selection instead of falling back to the highlighted option — for
	// checkbox-style settings dialogs where "everything off" is a valid answer.
	AllowNone bool

	// CollectFeedback enables a one-line free-text reason after the user picks
	// FeedbackValue: the dialog does not confirm immediately but enters a
	// text-entry stage, and a second Enter confirms with the typed Feedback. It
	// is the "No, and tell gocode what to do differently" branch of the
	// permission prompt. When false (every existing dialog) the dialog confirms
	// on the first Enter and behaves exactly as before.
	CollectFeedback bool
	// FeedbackValue is the option Value that triggers the feedback stage (the
	// deny option). Empty means no option triggers it.
	FeedbackValue string
	// Feedback is the typed reason. The host reads it after the dialog confirms
	// on the FeedbackValue option.
	Feedback string
	// AwaitingFeedback reports that the dialog is in the text-entry stage (set by
	// the model after the first Enter on the FeedbackValue option).
	AwaitingFeedback bool

	// --- DialogList (generic filterable select list) ------------------------

	// Items is the full item set of a DialogList. For a list dialog the Options
	// slice is empty and selection/confirmation operate on the filtered view of
	// Items instead. For every other kind Items is nil and ignored.
	Items []ListItem
	// Filter is the current type-to-filter query of a DialogList (case-insensitive
	// substring match against each item's Label and Detail). "" shows every item.
	Filter string
	// MaxVisible caps how many rows a DialogList shows at once; the list scrolls
	// (windows) when the filtered set is larger. 0 means show all rows (no
	// windowing). It is ignored for non-list dialogs.
	MaxVisible int
	// scrollOffset is the index (within the filtered item set) of the first row
	// currently visible in the viewport window.
	scrollOffset int
	// Fuzzy switches the filter from a plain substring match to the modern
	// pickers' ranked search (match.go: words on the label, value and detail,
	// best first, with a fuzzy fallback). The modern style sets it on the
	// pickers it opens.
	Fuzzy bool
	// search caches the last ranked search.
	search *dialogSearch

	// --- DialogSecret (masked text entry) -----------------------------------

	// secret holds the raw typed text of a DialogSecret dialog. It is unexported
	// on purpose: no renderer reads it (they draw MaskGlyph per rune instead) and
	// it is never surfaced in a rendered frame, so the raw value cannot leak into
	// a snapshot. The host reads it only through the confirm callbacks.
	secret string
	// MaskGlyph is the rune string drawn once per secret character. "" defaults to
	// the bullet "•" at render time.
	MaskGlyph string
	// Reveal turns the masked entry into a plain single-line text field: the
	// typed text is drawn as entered (a permission rule, a file name). Only
	// TextInputDialog sets it; the /login key prompt never does.
	Reveal bool
}

// FeedbackTriggered reports whether the highlighted option is the one that
// opens the feedback stage.
func (d *Dialog) FeedbackTriggered() bool {
	if d == nil || !d.CollectFeedback || d.FeedbackValue == "" {
		return false
	}
	opt, ok := d.Choice()
	return ok && opt.Value == d.FeedbackValue
}

// AppendFeedback adds s to the typed reason (text-entry stage only).
func (d *Dialog) AppendFeedback(s string) {
	if d == nil || !d.AwaitingFeedback {
		return
	}
	d.Feedback += s
}

// BackspaceFeedback removes the last rune of the typed reason.
func (d *Dialog) BackspaceFeedback() {
	if d == nil || !d.AwaitingFeedback || d.Feedback == "" {
		return
	}
	r := []rune(d.Feedback)
	d.Feedback = string(r[:len(r)-1])
}

// PermissionDialog builds the standard allow/deny dialog for a tool action.
func PermissionDialog(title string, body ...string) *Dialog {
	return &Dialog{
		Kind:  DialogPermission,
		Title: title,
		Body:  body,
		Options: []DialogOption{
			{Label: "Yes", Value: "allow_once"},
			{Label: "Yes, and don't ask again", Value: "allow_always"},
			{Label: "No, and tell gocode what to do differently", Value: "deny"},
		},
	}
}

// Permission option values the host (internal/cli) maps to a permission
// response. They are the answer tokens the prompter acts on.
const (
	PermAllowOnce   = "yes"
	PermAllowAlways = "yes-dont-ask-again"
	PermAllowEdits  = "yes-accept-edits"
	PermDeny        = "no"
)

// PermissionPromptDialog builds the interactive permission prompt for a tool
// call: the option set (Yes / "don't ask again" / optionally a
// switch-to-acceptEdits option for an edit tool / No), with the No option
// opening a free-text reason. title is the "Do you want to …?" question and body
// carries the per-tool detail lines (Bash command, file path, WebFetch domain,
// …). allowAlwaysLabel is the dynamic "don't ask again" label (naming the rule
// that would be saved); "" falls back to the generic label.
func PermissionPromptDialog(title string, allowAlwaysLabel string, isEdit bool, body ...string) *Dialog {
	opts := []DialogOption{
		{Label: "Yes", Value: PermAllowOnce},
	}
	if allowAlwaysLabel == "" {
		allowAlwaysLabel = "Yes, and don't ask again"
	}
	opts = append(opts, DialogOption{Label: allowAlwaysLabel, Value: PermAllowAlways})
	if isEdit {
		opts = append(opts, DialogOption{Label: "Yes, and switch to acceptEdits for this session", Value: PermAllowEdits})
	}
	opts = append(opts, DialogOption{Label: "No, and tell gocode what to do differently (esc)", Value: PermDeny})
	return &Dialog{
		Kind:            DialogPermission,
		Title:           title,
		Body:            body,
		Options:         opts,
		CollectFeedback: true,
		FeedbackValue:   PermDeny,
	}
}

// AskDialog builds an AskUserQuestion prompt for one question: the question text
// as the title, its options for selection, and multiSelect toggled when the
// question allows several answers. Each DialogOption carries the option's
// canonical label as both Label and Value, so the host maps a reported Value
// straight back to its label.
func AskDialog(question string, multiSelect bool, options []DialogOption, body ...string) *Dialog {
	return &Dialog{
		Kind:        DialogAsk,
		Title:       question,
		Body:        body,
		Options:     options,
		MultiSelect: multiSelect,
		Marked:      make([]bool, len(options)),
	}
}

// IsMarked reports whether option i is toggled on (MultiSelect dialogs only).
func (d *Dialog) IsMarked(i int) bool {
	if d == nil || i < 0 || i >= len(d.Marked) {
		return false
	}
	return d.Marked[i]
}

// ToggleMark flips the highlighted option's marked state. It is a no-op unless
// the dialog is MultiSelect. The Marked slice is grown to cover Options if a
// dialog was constructed without AskDialog.
func (d *Dialog) ToggleMark() {
	if d == nil || !d.MultiSelect || len(d.Options) == 0 {
		return
	}
	if len(d.Marked) < len(d.Options) {
		grown := make([]bool, len(d.Options))
		copy(grown, d.Marked)
		d.Marked = grown
	}
	if d.Selected >= 0 && d.Selected < len(d.Marked) {
		d.Marked[d.Selected] = !d.Marked[d.Selected]
	}
}

// ChosenValues returns the option value(s) the user confirmed. For a MultiSelect
// dialog it returns every marked option's Value in order, falling back to the
// highlighted option when nothing is marked; for a single-select dialog it
// returns the one highlighted Value. An empty dialog returns nil.
func (d *Dialog) ChosenValues() []string {
	if d == nil {
		return nil
	}
	// A masked-secret dialog confirms with the raw entered value.
	if d.Kind == DialogSecret {
		return []string{d.secret}
	}
	// A list dialog confirms with the highlighted filtered item's Value.
	if d.Kind == DialogList {
		if opt, ok := d.Choice(); ok {
			return []string{opt.Value}
		}
		return nil
	}
	if len(d.Options) == 0 {
		return nil
	}
	if d.MultiSelect {
		var vals []string
		for i, opt := range d.Options {
			if d.IsMarked(i) {
				vals = append(vals, opt.Value)
			}
		}
		if len(vals) > 0 {
			return vals
		}
		if d.AllowNone {
			return []string{}
		}
	}
	if opt, ok := d.Choice(); ok {
		return []string{opt.Value}
	}
	return nil
}

// MoveSelection advances the highlighted option by delta, clamping to range.
// For a DialogList it moves within the current filtered item set and keeps the
// scroll window aligned so the highlighted row stays visible.
func (d *Dialog) MoveSelection(delta int) {
	if d == nil {
		return
	}
	n := d.itemCount()
	if n == 0 {
		return
	}
	d.Selected += delta
	if d.Selected < 0 {
		d.Selected = 0
	}
	if d.Selected >= n {
		d.Selected = n - 1
	}
	d.ensureVisible()
}

// itemCount is the number of selectable rows: filtered items for a DialogList,
// options otherwise.
func (d *Dialog) itemCount() int {
	if d.Kind == DialogList {
		return len(d.FilteredItems())
	}
	return len(d.Options)
}

// Choice returns the currently highlighted option, if any. For a DialogList it
// adapts the highlighted filtered item into a DialogOption{Label, Value}.
func (d *Dialog) Choice() (DialogOption, bool) {
	if d == nil {
		return DialogOption{}, false
	}
	if d.Kind == DialogList {
		items := d.FilteredItems()
		if d.Selected < 0 || d.Selected >= len(items) {
			return DialogOption{}, false
		}
		return DialogOption{Label: items[d.Selected].Label, Value: items[d.Selected].Value}, true
	}
	if d.Selected < 0 || d.Selected >= len(d.Options) {
		return DialogOption{}, false
	}
	return d.Options[d.Selected], true
}

// --- DialogList (filterable, scrollable select list) ------------------------

// ListDialog builds a generic scrollable, type-to-filter select list (the
// /model and /mcp pickers). title is the header; items are the selectable rows;
// body carries optional description lines shown above the list. maxVisible caps
// how many rows are drawn at once (the list windows/scrolls past that; 0 shows
// every row). The host receives the highlighted row's Value through the
// dialog-submit seam when the user presses Enter.
func ListDialog(title string, items []ListItem, maxVisible int, body ...string) *Dialog {
	return &Dialog{
		Kind:       DialogList,
		Title:      title,
		Body:       body,
		Items:      items,
		MaxVisible: maxVisible,
	}
}

// FilteredItems returns the items that match the current Filter query
// (case-insensitive substring match against each item's Label and Detail). An
// empty filter returns every item. The result preserves the original order.
func (d *Dialog) FilteredItems() []ListItem {
	if d == nil {
		return nil
	}
	if d.Filter == "" {
		return d.Items
	}
	if d.Fuzzy {
		items, _ := d.fuzzyFiltered()
		return items
	}
	q := strings.ToLower(d.Filter)
	out := make([]ListItem, 0, len(d.Items))
	for _, it := range d.Items {
		if strings.Contains(strings.ToLower(it.Label), q) ||
			strings.Contains(strings.ToLower(it.Detail), q) {
			out = append(out, it)
		}
	}
	return out
}

// AppendFilter adds s to the type-to-filter query and resets the selection to
// the top of the newly filtered set.
func (d *Dialog) AppendFilter(s string) {
	if d == nil || d.Kind != DialogList {
		return
	}
	d.Filter += s
	d.Selected = 0
	d.scrollOffset = 0
}

// BackspaceFilter removes the last rune of the filter query and resets the
// selection to the top of the newly filtered set.
func (d *Dialog) BackspaceFilter() {
	if d == nil || d.Kind != DialogList || d.Filter == "" {
		return
	}
	r := []rune(d.Filter)
	d.Filter = string(r[:len(r)-1])
	d.Selected = 0
	d.scrollOffset = 0
}

// ensureVisible keeps the highlighted row inside the scroll window, adjusting
// scrollOffset so Selected stays in [scrollOffset, scrollOffset+MaxVisible). It
// is a no-op for non-list dialogs and for an unbounded (MaxVisible<=0) list.
func (d *Dialog) ensureVisible() {
	if d.Kind != DialogList || d.MaxVisible <= 0 {
		d.scrollOffset = 0
		return
	}
	n := len(d.FilteredItems())
	win := d.MaxVisible
	if d.Selected < d.scrollOffset {
		d.scrollOffset = d.Selected
	}
	if d.Selected >= d.scrollOffset+win {
		d.scrollOffset = d.Selected - win + 1
	}
	maxOff := n - win
	if maxOff < 0 {
		maxOff = 0
	}
	if d.scrollOffset > maxOff {
		d.scrollOffset = maxOff
	}
	if d.scrollOffset < 0 {
		d.scrollOffset = 0
	}
}

// VisibleItems returns the windowed slice of filtered items currently drawn and
// the index (within the filtered set) of the first returned row. For an
// unbounded list it returns every filtered item starting at 0. The renderers
// use this to draw a scrolling viewport for long lists and narrow heights.
func (d *Dialog) VisibleItems() ([]ListItem, int) {
	if d == nil {
		return nil, 0
	}
	items := d.FilteredItems()
	if d.MaxVisible <= 0 || len(items) <= d.MaxVisible {
		return items, 0
	}
	start := d.scrollOffset
	if start < 0 {
		start = 0
	}
	end := start + d.MaxVisible
	if end > len(items) {
		end = len(items)
		start = end - d.MaxVisible
		if start < 0 {
			start = 0
		}
	}
	return items[start:end], start
}

// --- DialogSecret (masked single-line text entry) ---------------------------

// SecretDialog builds a masked single-line text-entry dialog (the /login key
// prompt). The typed characters are stored but never rendered: the renderers
// draw one MaskGlyph per entered rune. Enter submits the raw entered value
// through the dialog-submit seam; Esc cancels. body carries optional prompt
// lines shown above the input.
func SecretDialog(title string, body ...string) *Dialog {
	return &Dialog{
		Kind:  DialogSecret,
		Title: title,
		Body:  body,
	}
}

// TextInputDialog builds a plain single-line text-entry dialog: the same
// field as SecretDialog but with the typed text shown (Reveal). initial
// pre-fills the field (e.g. a default file name). Enter submits the entered
// text through the dialog-submit seam; Esc cancels.
func TextInputDialog(title, initial string, body ...string) *Dialog {
	return &Dialog{
		Kind:   DialogSecret,
		Title:  title,
		Body:   body,
		Reveal: true,
		secret: initial,
	}
}

// Text returns the entered text of a TextInputDialog ("" for a masked
// secret dialog, whose value is never handed out this way).
func (d *Dialog) Text() string {
	if d == nil || !d.Reveal {
		return ""
	}
	return d.secret
}

// AppendSecret adds s to the (unexported) raw secret buffer.
func (d *Dialog) AppendSecret(s string) {
	if d == nil || d.Kind != DialogSecret {
		return
	}
	d.secret += s
}

// BackspaceSecret removes the last rune of the raw secret buffer.
func (d *Dialog) BackspaceSecret() {
	if d == nil || d.Kind != DialogSecret || d.secret == "" {
		return
	}
	r := []rune(d.secret)
	d.secret = string(r[:len(r)-1])
}

// SecretLen reports the number of entered runes (for a renderer that needs the
// masked length without reading the raw value).
func (d *Dialog) SecretLen() int {
	if d == nil {
		return 0
	}
	return len([]rune(d.secret))
}

// Masked returns the display string for a DialogSecret: one mask glyph per
// entered rune (defaulting to the bullet "•"). It never returns any character
// of the raw secret, so the rendered frame cannot leak the value. A plain text
// field (Reveal) shows its text as entered, made terminal-safe.
func (d *Dialog) Masked() string {
	if d == nil {
		return ""
	}
	if d.Reveal {
		return SanitizeLine(d.secret)
	}
	glyph := d.MaskGlyph
	if glyph == "" {
		glyph = "•"
	}
	return strings.Repeat(glyph, d.SecretLen())
}

// fuzzyFiltered ranks the items against the filter (see match.go: words
// matched on the label, value and detail, a fuzzy fallback on the label and
// value), returning the matches best first and, per match, the matched rune
// indexes of its label. The result is cached until the filter or the item set
// changes, so a frame that draws the picker does not search again.
func (d *Dialog) fuzzyFiltered() ([]ListItem, [][]int) {
	if c := d.search; c != nil && c.filter == d.Filter && c.n == len(d.Items) && (len(d.Items) == 0 || c.first == &d.Items[0]) {
		return c.items, c.matches
	}
	var entries []searchEntry
	if c := d.search; c != nil && c.n == len(d.Items) && (len(d.Items) == 0 || c.first == &d.Items[0]) {
		entries = c.entries // same items: reuse their lower-cased text
	} else {
		entries = make([]searchEntry, len(d.Items))
		for i, it := range d.Items {
			entries[i] = searchEntry{label: it.Label, id: it.Value, detail: it.Detail}
		}
		prepare(entries)
	}
	hits := searchEntries(d.Filter, entries)
	items := make([]ListItem, len(hits))
	matches := make([][]int, len(hits))
	for i, h := range hits {
		items[i] = d.Items[h.index]
		matches[i] = h.match
	}
	c := &dialogSearch{filter: d.Filter, n: len(d.Items), items: items, matches: matches, entries: entries}
	if len(d.Items) > 0 {
		c.first = &d.Items[0]
	}
	d.search = c
	return items, matches
}

// dialogSearch is a list dialog's cached search result.
type dialogSearch struct {
	filter  string
	n       int
	first   *ListItem
	items   []ListItem
	matches [][]int
	entries []searchEntry
}

// MatchedRunes returns the label rune indexes the filter matched for the
// i-th filtered item (fuzzy lists only; nil otherwise).
func (d *Dialog) MatchedRunes(i int) []int {
	if d == nil || !d.Fuzzy || d.Filter == "" {
		return nil
	}
	_, matches := d.fuzzyFiltered()
	if i < 0 || i >= len(matches) {
		return nil
	}
	return matches[i]
}
