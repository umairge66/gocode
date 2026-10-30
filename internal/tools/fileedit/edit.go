package fileedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// ToolNameEdit is the Edit tool name.
const ToolNameEdit = "Edit"

// toolNameNotebookEdit is referenced by the .ipynb refusal.
const toolNameNotebookEdit = "NotebookEdit"

// MaxEditableFileSize is the largest file Edit will touch.
const MaxEditableFileSize = 1073741824

// Edit validation and result messages.
const (
	MsgNoChanges       = "No changes to make: old_string and new_string are exactly the same."
	MsgCannotCreate    = "Cannot create new file - file already exists."
	msgNotebookEdit    = "File is a Jupyter Notebook. Use the " + toolNameNotebookEdit + " to edit this file."
	msgStringNotFound  = "String to replace not found in file.\nString: %s"
	msgMultipleMatches = "Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: %s"
	// msgStateCurrent is appended to a successful edit result.
	msgStateCurrent = " (file state is current in your context — no need to Read it back)"
)

// Multi-edit application errors.
const (
	MsgSubstringOfPrevious = "Cannot edit file: old_string is a substring of a new_string from a previous edit."
	MsgEditNotApplied      = "String not found in file. Failed to apply edit."
	MsgIdentical           = "Original and edited file match exactly. Failed to apply edit."
)

// EditPrompt is the model-facing usage text (tab separator, default
// uniqueness bullet).
const EditPrompt = "Performs exact string replacements in files.\n\nUsage:" +
	"\n- You must use your `Read` tool at least once in the conversation before editing. This tool will error if you attempt an edit without reading the file." +
	"\n- When editing text from Read tool output, ensure you preserve the exact indentation (tabs/spaces) as it appears AFTER the line number prefix. The line number prefix format is: line number + tab. Everything after that is the actual file content to match. Never include any part of the line number prefix in the old_string or new_string." +
	"\n- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required." +
	"\n- Only use emojis if the user explicitly requests it. Avoid adding emojis to files unless asked." +
	"\n- The edit will FAIL if `old_string` is not unique in the file. Either provide a larger string with more surrounding context to make it unique or use `replace_all` to change every instance of `old_string`." +
	"\n- Use `replace_all` for replacing and renaming strings across the file. This parameter is useful if you want to rename a variable for instance."

// editInputSchema is the Edit input schema.
var editInputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"file_path":{"type":"string","description":"The absolute path to the file to modify"},` +
	`"old_string":{"type":"string","description":"The text to replace"},` +
	`"new_string":{"type":"string","description":"The text to replace it with (must be different from old_string)"},` +
	`"replace_all":{"type":"boolean","default":false,"description":"Replace all occurrences of old_string (default false)"}` +
	`},"required":["file_path","old_string","new_string"],"additionalProperties":false}`)

// EditInput is the Edit input.
type EditInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditResult reports what Edit did.
type EditResult struct {
	FilePath     string
	OldString    string // the string actually matched in the file
	NewString    string // the string actually written
	OriginalFile string // LF-normalized content before the edit ("" if created)
	UpdatedFile  string // LF-normalized content after the edit
	ReplaceAll   bool
	Created      bool
	Text         string // tool_result text
}

// Edit input errors.
var (
	ErrEditEmptyPath = errors.New("file_path is required")
	ErrEditNotAbs    = errors.New("file_path must be an absolute path, not a relative path")
)

// EditTool performs exact string replacement.
type EditTool struct {
	// Tracker gates edits on a prior, still-current Read. When nil, edits of
	// existing files fail closed with ErrFileNotRead.
	Tracker *FileStateTracker
}

// Name returns the tool name.
func (EditTool) Name() string { return ToolNameEdit }

// Prompt returns the model-facing usage text.
func (EditTool) Prompt() string { return EditPrompt }

// InputSchema returns the JSON input schema.
func (EditTool) InputSchema() json.RawMessage { return editInputSchema }

// editOp is one replacement.
type editOp struct {
	OldString  string
	NewString  string
	ReplaceAll bool
}

// Execute applies in to the file.
func (t EditTool) Execute(ctx context.Context, in EditInput) (EditResult, error) {
	if err := ctx.Err(); err != nil {
		return EditResult{}, err
	}
	if in.OldString == in.NewString {
		return EditResult{}, errors.New(MsgNoChanges)
	}
	res, err := runEdits(t.Tracker, in.FilePath, []editOp{{in.OldString, in.NewString, in.ReplaceAll}}, false)
	if err != nil {
		return EditResult{}, err
	}
	out := EditResult{
		FilePath:     in.FilePath,
		OriginalFile: res.original,
		UpdatedFile:  res.updated,
		ReplaceAll:   in.ReplaceAll,
		Created:      res.created,
	}
	if len(res.applied) == 1 {
		out.OldString, out.NewString = res.applied[0].OldString, res.applied[0].NewString
	}
	if in.ReplaceAll {
		out.Text = fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.%s", in.FilePath, msgStateCurrent)
	} else {
		out.Text = fmt.Sprintf("The file %s has been updated successfully.%s", in.FilePath, msgStateCurrent)
	}
	return out, nil
}

// --- shared engine ----------------------------------------------------------

type editOutcome struct {
	original string
	updated  string
	created  bool
	applied  []editOp // with actual (normalized) old/new strings
}

// indexedErr prefixes an error with the failing edit index for MultiEdit.
func indexedErr(multi bool, i int, msg string) error {
	if multi {
		return fmt.Errorf("edit %d: %s", i, msg)
	}
	return errors.New(msg)
}

// runEdits validates and applies ops to path atomically (all or nothing).
func runEdits(tracker *FileStateTracker, path string, ops []editOp, multi bool) (editOutcome, error) {
	if path == "" {
		return editOutcome{}, ErrEditEmptyPath
	}
	if !filepath.IsAbs(path) {
		return editOutcome{}, ErrEditNotAbs
	}
	path = filepath.Clean(path)

	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return editOutcome{}, eisdir(path)
		}
		if info.Size() > MaxEditableFileSize {
			return editOutcome{}, fmt.Errorf("File is too large to edit (%s). Maximum editable file size is %s.", formatBytes(info.Size()), formatBytes(MaxEditableFileSize))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return editOutcome{}, err
	}

	tf, err := loadText(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return editOutcome{}, err
	}

	createMode := len(ops) > 0 && ops[0].OldString == ""
	if !exists {
		if !createMode {
			return editOutcome{}, errors.New(fileNotExistMessage(path))
		}
		updated, applied, err := applyEdits("", ops, multi)
		if err != nil {
			return editOutcome{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return editOutcome{}, err
		}
		data := []byte(updated)
		if err := writeAtomic(path, data, 0o644); err != nil {
			return editOutcome{}, err
		}
		if tracker != nil {
			_ = tracker.RecordFromDisk(path)
		}
		return editOutcome{original: "", updated: updated, created: true, applied: applied}, nil
	}

	if createMode && strings.TrimSpace(strings.TrimPrefix(tf.content, utf8BOM)) != "" {
		return editOutcome{}, errors.New(MsgCannotCreate)
	}
	if strings.HasSuffix(strings.ToLower(path), ".ipynb") {
		return editOutcome{}, errors.New(msgNotebookEdit)
	}
	if tracker == nil {
		return editOutcome{}, ErrFileNotRead
	}
	if err := tracker.Check(path); err != nil {
		return editOutcome{}, err
	}
	if tf.notText != notTextNone {
		return editOutcome{}, errors.New(notTextEditMessage(tf.notText))
	}

	updated, applied, err := applyEdits(tf.content, ops, multi)
	if err != nil {
		return editOutcome{}, err
	}
	if createMode {
		// Keep a leading BOM when filling an empty file.
		if strings.HasPrefix(tf.content, utf8BOM) && updated != "" && !strings.HasPrefix(updated, utf8BOM) {
			updated = utf8BOM + updated
		}
	}
	if err := writeAtomic(path, encodeForDisk(updated, tf.crlf), tf.mode); err != nil {
		return editOutcome{}, err
	}
	if tracker != nil {
		_ = tracker.RecordFromDisk(path)
	}
	return editOutcome{original: tf.content, updated: updated, applied: applied}, nil
}

// applyEdits applies ops in order against the evolving content. It returns
// an error naming the failing index (for multi) and never partially applies.
func applyEdits(content string, ops []editOp, multi bool) (string, []editOp, error) {
	cur := content
	var prevNew []string
	var applied []editOp
	for i, op := range ops {
		if op.OldString == op.NewString {
			return "", nil, indexedErr(multi, i, MsgNoChanges)
		}
		if op.OldString == "" {
			if i != 0 || strings.TrimSpace(strings.TrimPrefix(cur, utf8BOM)) != "" {
				return "", nil, indexedErr(multi, i, MsgCannotCreate)
			}
			cur = op.NewString
			prevNew = append(prevNew, op.NewString)
			applied = append(applied, op)
			continue
		}
		trimmed := strings.TrimRight(op.OldString, "\n")
		for _, pn := range prevNew {
			if trimmed != "" && strings.Contains(pn, trimmed) {
				return "", nil, indexedErr(multi, i, MsgSubstringOfPrevious)
			}
		}
		actual, ok := findActualString(cur, op.OldString)
		if !ok {
			return "", nil, indexedErr(multi, i, fmt.Sprintf(msgStringNotFound, op.OldString))
		}
		if n := strings.Count(cur, actual); n > 1 && !op.ReplaceAll {
			return "", nil, indexedErr(multi, i, fmt.Sprintf(msgMultipleMatches, n, op.OldString))
		}
		newStr := preserveEscapeStyle(op.OldString, actual, preserveQuoteStyle(op.OldString, actual, op.NewString))
		next := applyReplace(cur, actual, newStr, op.ReplaceAll)
		if next == cur {
			return "", nil, indexedErr(multi, i, MsgEditNotApplied)
		}
		cur = next
		prevNew = append(prevNew, op.NewString)
		applied = append(applied, editOp{OldString: actual, NewString: newStr, ReplaceAll: op.ReplaceAll})
	}
	if cur == content {
		return "", nil, errors.New(MsgIdentical)
	}
	return cur, applied, nil
}

// applyReplace replaces old with new (first or all). When new is empty and
// old does not end in a newline but old+"\n" occurs, the trailing newline is
// removed too so deleting a line does not leave a blank one.
func applyReplace(content, old, new string, all bool) string {
	n := 1
	if all {
		n = -1
	}
	if new == "" && !strings.HasSuffix(old, "\n") && strings.Contains(content, old+"\n") {
		return strings.Replace(content, old+"\n", new, n)
	}
	return strings.Replace(content, old, new, n)
}

// Curly quotes the model often straightens.
const (
	leftSingle  = '‘'
	rightSingle = '’'
	leftDouble  = '“'
	rightDouble = '”'
)

// normalizeQuotes maps curly quotes to straight ones rune-for-rune.
func normalizeQuotes(s string) []rune {
	rs := []rune(s)
	for i, r := range rs {
		switch r {
		case leftSingle, rightSingle:
			rs[i] = '\''
		case leftDouble, rightDouble:
			rs[i] = '"'
		}
	}
	return rs
}

var uEscape = regexp.MustCompile(`(\\\\)|\\u([0-9a-fA-F]{4})`)

// decodeUEscapes turns \uXXXX escapes into characters. Each escape is one
// UTF-16 code unit, so an escaped
// surrogate pair (\uD83D\uDE00) decodes to the single character it encodes.
func decodeUEscapes(s string) string {
	locs := uEscape.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	var units []uint16
	flush := func() {
		if len(units) > 0 {
			b.WriteString(string(utf16.Decode(units)))
			units = units[:0]
		}
	}
	prev := 0
	for _, loc := range locs {
		if loc[0] > prev {
			flush()
			b.WriteString(s[prev:loc[0]])
		}
		if loc[2] >= 0 { // escaped backslash: kept verbatim
			flush()
			b.WriteString(s[loc[0]:loc[1]])
		} else {
			v, _ := strconv.ParseUint(s[loc[4]:loc[5]], 16, 16)
			units = append(units, uint16(v))
		}
		prev = loc[1]
	}
	flush()
	b.WriteString(s[prev:])
	return b.String()
}

// hasNonASCIIUnit reports whether s holds any UTF-16 code unit >= 0x80
// .
func hasNonASCIIUnit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// escapedLen is the length old would have with every non-ASCII code unit
// spelled as a six-character \uXXXX escape.
func escapedLen(u []uint16) int {
	n := len(u)
	for _, c := range u {
		if c >= 0x80 {
			n += 5
		}
	}
	return n
}

// backslashesBefore counts the run of backslashes ending just before n
// .
func backslashesBefore(s string, n int) int {
	r := 0
	for n-1-r >= 0 && s[n-1-r] == '\\' {
		r++
	}
	return r
}

// findEscapedSpelling is the reverse escape search: old spells
// characters literally while content holds them as \uXXXX escapes. It builds
// a pattern with each non-ASCII code unit as a case-insensitive escape and
// returns content's spelling of the first match whose escape backslashes are
// not themselves escaped, provided that is also the first occurrence of that
// text in content.
func findEscapedSpelling(old, content string) (string, bool) {
	if backslashesBefore(old, len(old))%2 == 1 {
		return "", false
	}
	u := utf16.Encode([]rune(old))
	var offs []int
	if len(old) > 0 && old[0] == '\\' {
		offs = append(offs, 0)
	}
	var pat strings.Builder
	pos := 0
	for _, c := range u {
		if c >= 0x80 {
			offs = append(offs, pos)
			pos += 6
			pat.WriteString(`\\u`)
			for _, h := range fmt.Sprintf("%04x", c) {
				if h >= 'a' {
					pat.WriteString("[" + string(h) + strings.ToUpper(string(h)) + "]")
				} else {
					pat.WriteRune(h)
				}
			}
			continue
		}
		pos++
		pat.WriteString(regexp.QuoteMeta(string(rune(c))))
	}
	re, err := regexp.Compile(pat.String())
	if err != nil {
		return "", false
	}
	// The match is pure ASCII, so byte offsets equal UTF-16 code-unit
	// offsets.
	for start := 0; start <= len(content); {
		loc := re.FindStringIndex(content[start:])
		if loc == nil {
			return "", false
		}
		l := start + loc[0]
		ok := true
		for _, a := range offs {
			if backslashesBefore(content, l+a)%2 != 0 {
				ok = false
				break
			}
		}
		if ok {
			m := content[l : start+loc[1]]
			if strings.Index(content, m) == l {
				return m, true
			}
			return "", false
		}
		start = l + 1
	}
	return "", false
}

// preserveEscapeStyle adapts new_string to the escape spelling actually found
// in the file. When old_string spelled characters literally but
// the file holds \uXXXX escapes, new_string's non-ASCII code units are
// re-escaped, reusing the file's hex spelling per character and otherwise
// the file's dominant hex case. When old_string used escapes that decoded to
// the file's text, new_string's escapes are decoded likewise.
func preserveEscapeStyle(old, actual, new string) string {
	if old == actual {
		return new
	}
	ou := utf16.Encode([]rune(old))
	if hasNonASCIIUnit(old) && escapedLen(ou) == len(actual) {
		if _, ok := findEscapedSpelling(old, actual); ok {
			hex := map[uint16]string{}
			upper, lower := 0, 0
			l := 0
			for _, c := range ou {
				if c < 0x80 {
					l++
					continue
				}
				h := actual[l+2 : l+6]
				hex[c] = h
				for _, f := range h {
					if f >= 'a' && f <= 'f' {
						lower++
					} else if f >= 'A' && f <= 'F' {
						upper++
					}
				}
				l += 6
			}
			var b strings.Builder
			for _, c := range utf16.Encode([]rune(new)) {
				if c < 0x80 {
					b.WriteByte(byte(c))
					continue
				}
				if h, ok := hex[c]; ok {
					b.WriteString(`\u` + h)
					continue
				}
				h := fmt.Sprintf("%04x", c)
				if upper > lower {
					h = strings.ToUpper(h)
				}
				b.WriteString(`\u` + h)
			}
			return b.String()
		}
	}
	if uEscapeAny.MatchString(old) && decodeUEscapes(old) == actual {
		return decodeUEscapes(new)
	}
	return new
}

// uEscapeAny matches a single \uXXXX escape.
var uEscapeAny = regexp.MustCompile(`\\u[0-9a-fA-F]{4}`)

// findActualString finds the text in content that old refers to: an exact
// match, else a match after curly-quote normalization (returning the file's
// own spelling), else a match after decoding \uXXXX escapes.
// When old spells non-ASCII characters literally while the file holds them as
// \uXXXX escapes, the escaped spelling is searched for.
func findActualString(content, old string) (string, bool) {
	if strings.Contains(content, old) {
		return old, true
	}
	nc := normalizeQuotes(content)
	no := normalizeQuotes(old)
	if idx := runeIndex(nc, no); idx >= 0 {
		return string([]rune(content)[idx : idx+len(no)]), true
	}
	if uEscapeAny.MatchString(old) {
		if d := decodeUEscapes(old); d != old && strings.Contains(content, d) {
			return d, true
		}
	}
	if hasNonASCIIUnit(old) {
		if escapedLen(utf16.Encode([]rune(old))) > len(utf16.Encode([]rune(content))) || !strings.Contains(content, `\u`) {
			return "", false
		}
		return findEscapedSpelling(old, content)
	}
	return "", false
}

func runeIndex(hay, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		for j := range needle {
			if hay[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

// preserveQuoteStyle: when the file's text used curly quotes that old_string
// spelled straight, new_string's straight quotes are curled the same way.
func preserveQuoteStyle(old, actual, new string) string {
	if old == actual {
		return new
	}
	hasDouble := strings.ContainsRune(actual, leftDouble) || strings.ContainsRune(actual, rightDouble)
	hasSingle := strings.ContainsRune(actual, leftSingle) || strings.ContainsRune(actual, rightSingle)
	if !hasDouble && !hasSingle {
		return new
	}
	rs := []rune(new)
	out := make([]rune, len(rs))
	for i, r := range rs {
		out[i] = r
		switch {
		case hasDouble && r == '"':
			if openingContext(rs, i) {
				out[i] = leftDouble
			} else {
				out[i] = rightDouble
			}
		case hasSingle && r == '\'':
			prevLetter := i > 0 && unicode.IsLetter(rs[i-1])
			nextLetter := i < len(rs)-1 && unicode.IsLetter(rs[i+1])
			if prevLetter && nextLetter {
				out[i] = rightSingle // apostrophe
			} else if openingContext(rs, i) {
				out[i] = leftSingle
			} else {
				out[i] = rightSingle
			}
		}
	}
	return string(out)
}

// openingContext reports whether a quote at i opens.
func openingContext(rs []rune, i int) bool {
	if i == 0 {
		return true
	}
	switch rs[i-1] {
	case ' ', '\t', '\n', '\r', '(', '[', '{', '—', '–':
		return true
	}
	return false
}
