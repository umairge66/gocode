// Package filetools implements the Glob, Grep and NotebookEdit tools.
package filetools

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// errNotDir is returned when a search path exists but is not a directory.
var errNotDir = errors.New("path is not a directory")

// GlobMaxResults is the truncation cap applied to glob output. The output
// schema documents "truncated ... (limited to 100 files)".
const GlobMaxResults = 100

// GlobInput is the input schema for the Glob tool.
type GlobInput struct {
	// Pattern is required: "The glob pattern to match files against".
	Pattern string `json:"pattern"`
	// Path is optional: "The directory to search in. If not specified, the
	// current working directory will be used." Must be a valid directory.
	Path string `json:"path,omitempty"`
}

// GlobResult is the output schema for the Glob tool.
type GlobResult struct {
	// DurationMs: "Time taken to execute the search in milliseconds".
	DurationMs int64 `json:"durationMs"`
	// NumFiles: "Number of file paths returned (after any truncation)".
	NumFiles int `json:"numFiles"`
	// Filenames: "Array of file paths that match the pattern".
	Filenames []string `json:"filenames"`
	// Truncated: whether results were truncated (limited to GlobMaxResults).
	Truncated bool `json:"truncated"`
	// TotalMatches: total number of matching files before truncation.
	TotalMatches int `json:"totalMatches"`
	// Notice is a one-line engine notice (not part of the output schema).
	Notice string `json:"notice,omitempty"`
}

// GlobTool implements the Glob tool: fast file pattern matching that returns
// matching file paths sorted by modification time (oldest first). Engine
// supplies the session's hidden / ignore settings and working directory; a
// nil Engine uses the defaults.
//
// The tool is read-only and concurrency-safe.
type GlobTool struct {
	Engine *SearchEngine
	// Exclude, when set, drops a result (an absolute path) the session's
	// permissions do not let the model read — a file a Read deny rule covers,
	// or one outside the working directories under
	// blockReadsOutsideWorkingDirectories.
	Exclude func(absPath string) bool
}

// Description is the default (non-lean) description text.
func (GlobTool) Description() string {
	return "- Fast file pattern matching tool that works with any codebase size\n" +
		"- Supports glob patterns like \"**/*.js\" or \"src/**/*.ts\"\n" +
		"- Returns matching file paths sorted by modification time\n" +
		"- Use this tool when you need to find files by name patterns"
}

// Execute lists the files under the search directory that match the
// pattern, oldest first, capped at GlobMaxResults.
//
// Pattern semantics are gitignore-style globs: "*" stays within
// a path segment, "**" spans segments, "{a,b}" alternates. A pattern without a
// "/" matches at any depth (so "*.js" finds .js files in every folder); an absolute pattern's literal leading directories become the
// search directory.
func (t GlobTool) Execute(in GlobInput) (GlobResult, error) {
	started := time.Now()
	root := in.Path
	if root == "" {
		root = t.Engine.cwd()
	}
	pattern := filepath.ToSlash(in.Pattern)
	if base, rest, ok := splitAbsoluteGlob(in.Pattern); ok {
		root, pattern = base, rest
	}
	pattern = strings.TrimPrefix(pattern, "./")
	root, err := filepath.Abs(root)
	if err != nil {
		return GlobResult{}, err
	}

	// Directory validation: the search path must exist and be a
	// directory.
	info, err := os.Stat(root)
	if err != nil {
		return GlobResult{}, err
	}
	if !info.IsDir() {
		return GlobResult{}, &os.PathError{Op: "glob", Path: root, Err: errNotDir}
	}

	hidden, respectIgnore := t.Engine.globOptions()
	globs := append([]string{pattern}, vcsExcludeGlobs()...)

	files := globWalk(root, globs, hidden, respectIgnore)

	type match struct {
		path  string
		mtime int64
	}
	pol := newPathPolicy(root, hidden, respectIgnore)
	matches := make([]match, 0, len(files))
	for _, p := range files {
		if !pol.allowed(p) {
			continue
		}
		if t.Exclude != nil && t.Exclude(p) {
			continue
		}
		var mt int64
		if fi, err := os.Stat(p); err == nil {
			mt = fi.ModTime().UnixNano()
		}
		matches = append(matches, match{p, mt})
	}
	// Oldest first; ties broken by path for deterministic output.
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].mtime != matches[j].mtime {
			return matches[i].mtime < matches[j].mtime
		}
		return matches[i].path < matches[j].path
	})

	total := len(matches)
	truncated := false
	if len(matches) > GlobMaxResults {
		matches = matches[:GlobMaxResults]
		truncated = true
	}
	cwd := t.Engine.cwd()
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = displayPath(m.path, cwd)
	}
	return GlobResult{
		DurationMs:   time.Since(started).Milliseconds(),
		NumFiles:     len(names),
		Filenames:    names,
		Truncated:    truncated,
		TotalMatches: total,
		Notice:       t.Engine.takeNotice(),
	}, nil
}

// splitAbsoluteGlob splits an absolute pattern into its literal leading
// directory and the glob remainder ("/src/**/*.go" → "/src", "**/*.go").
func SearchRoot(in GlobInput) string {
	if base, _, ok := splitAbsoluteGlob(in.Pattern); ok {
		return base
	}
	return in.Path
}

func splitAbsoluteGlob(pattern string) (base, rest string, ok bool) {
	if !filepath.IsAbs(pattern) {
		return "", "", false
	}
	slashed := filepath.ToSlash(pattern)
	meta := strings.IndexAny(slashed, "*?[{")
	if meta < 0 {
		return filepath.Dir(pattern), filepath.Base(pattern), true
	}
	cut := strings.LastIndex(slashed[:meta], "/")
	if cut < 0 {
		return "", "", false
	}
	base = slashed[:cut]
	if base == "" || (len(base) == 2 && base[1] == ':') {
		base += "/"
	}
	return filepath.FromSlash(base), slashed[cut+1:], true
}

// globWalk lists the files under root that the pattern (and the VCS
// excludes) select, under the hidden and ignore-file rules. Listing needs no
// file contents, so it runs on gocode's walker whichever search engine is in
// use.
func globWalk(root string, globs []string, hidden, respectIgnore bool) []string {
	filter, chain := newWalkFilter(root, hidden, respectIgnore, newOverrideSet(root, globs), nil)
	return walkFiles(root, filter, chain)
}
