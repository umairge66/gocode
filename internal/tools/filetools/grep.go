package filetools

// GrepDefaultHeadLimit is the default cap on output lines/entries when
// head_limit is unspecified. Passing 0 means unlimited.
const GrepDefaultHeadLimit = 250

// vcsDirs are version-control metadata directories that are always excluded
// (applied as "!<dir>" glob overrides), even when hidden files are searched.
var vcsDirs = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// GrepInput is the input schema for the Grep tool.
type GrepInput struct {
	// Pattern is required: "The regular expression pattern to search for in
	// file contents".
	Pattern string `json:"pattern"`
	// Path is optional: the file or directory to search in. Defaults to the
	// current working directory.
	Path string `json:"path,omitempty"`
	// Glob filters files by gitignore-style globs, e.g. "*.js", "*.{ts,tsx}".
	Glob string `json:"glob,omitempty"`
	// OutputMode is "content", "files_with_matches" (default) or "count".
	OutputMode string `json:"output_mode,omitempty"`
	// BeforeContext is the -B leading context (content mode only).
	BeforeContext *int `json:"-B,omitempty"`
	// AfterContext is the -A trailing context (content mode only).
	AfterContext *int `json:"-A,omitempty"`
	// Context is an alias for -C (content mode only).
	Context *int `json:"-C,omitempty"`
	// ContextLong is the -C context on both sides (content mode only).
	ContextLong *int `json:"context,omitempty"`
	// LineNumbers shows line numbers, -n (content mode only). Defaults to true.
	LineNumbers *bool `json:"-n,omitempty"`
	// CaseInsensitive matches case-insensitively, -i.
	CaseInsensitive bool `json:"-i,omitempty"`
	// OnlyMatching prints only the matched parts, -o (content mode only).
	// Defaults to false.
	OnlyMatching bool `json:"-o,omitempty"`
	// Type filters by file type (e.g. "js", "py", "rust", "go", "java"; see
	// fileTypeGlobs).
	Type string `json:"type,omitempty"`
	// HeadLimit caps output to first N lines/entries ("| head -N"). Defaults to
	// GrepDefaultHeadLimit; 0 means unlimited.
	HeadLimit *int `json:"head_limit,omitempty"`
	// Offset skips the first N lines/entries before applying HeadLimit.
	Offset int `json:"offset,omitempty"`
	// Multiline enables patterns that span lines, with "." matching newlines.
	Multiline bool `json:"multiline,omitempty"`
}

// GrepResult is the output schema for the Grep tool.
type GrepResult struct {
	Mode          string   `json:"mode,omitempty"`
	NumFiles      int      `json:"numFiles"`
	Filenames     []string `json:"filenames"`
	Content       string   `json:"content,omitempty"`
	NumLines      int      `json:"numLines,omitempty"`
	NumMatches    int      `json:"numMatches,omitempty"`
	TotalFiles    int      `json:"totalFiles,omitempty"`
	TotalLines    int      `json:"totalLines,omitempty"`
	AppliedLimit  *int     `json:"appliedLimit,omitempty"`
	AppliedOffset *int     `json:"appliedOffset,omitempty"`
	// Notice is a one-line engine notice (e.g. ugrep could not start and the
	// built-in engine was used). Not part of the output schema.
	Notice string `json:"notice,omitempty"`
}

// GrepTool implements the Grep tool: a regex content search. Engine runs it
// (ugrep-accelerated or the built-in Go engine alone); a nil Engine runs the
// built-in engine with the default settings.
type GrepTool struct {
	Engine *SearchEngine
	// Exclude, when set, drops a matched file (an absolute path) the session's
	// permissions do not let the model read (see GlobTool.Exclude).
	Exclude func(absPath string) bool
}

// GrepCompactDescription is the compact description a session
// advertises: the same rules as Description, without examples.
const GrepCompactDescription = "Regex content search. ALWAYS use it for search; NEVER run `grep` or `rg` in Bash.\n" +
	"- RE2-style regex: escape literal braces (`interface\\{\\}` finds `interface{}`).\n" +
	"- output_mode: files_with_matches (default; paths), content (matching lines), count.\n" +
	"- Patterns match within one line; multiline: true lets them span lines."

// Description is the default (non-lean) description text, with tool-name
// references written as literals.
func (GrepTool) Description() string {
	return "A powerful regex search tool for file contents\n\n" +
		"  Usage:\n" +
		"  - ALWAYS use Grep for search tasks. NEVER invoke `grep` or `rg` as a Bash command. " +
		"The Grep tool has been optimized for correct permissions and access.\n" +
		"  - Supports full regex syntax (e.g., \"log.*Error\", \"function\\s+\\w+\")\n" +
		"  - Filter files with glob parameter (e.g., \"*.js\", \"**/*.tsx\") or type parameter (e.g., \"js\", \"py\", \"rust\")\n" +
		"  - Output modes: \"content\" shows matching lines, \"files_with_matches\" shows only file paths (default), \"count\" shows match counts\n" +
		"  - Pattern syntax: RE2-style regex (not POSIX grep) - literal braces need escaping (use `interface\\{\\}` to find `interface{}` in Go code)\n" +
		"  - Multiline matching: By default patterns match within single lines only. For cross-line patterns like `struct \\{[\\s\\S]*?field`, use `multiline: true`"
}
