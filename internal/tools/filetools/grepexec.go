package filetools

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

// grepMaxLineChars is the length (UTF-16 units) above which a line is replaced by an omission marker.
const grepMaxLineChars = 500

// binarySniffBytes is how much of a file the Go engine checks for a NUL byte
// to classify it as binary.
const binarySniffBytes = 64 * 1024

// grepHit is one output line of content mode.
type grepHit struct {
	line int    // 1-based line number (0 for the binary-file note)
	ctx  bool   // a context line (rendered with '-')
	text string // the line text (no terminator)
	raw  bool   // a note rendered verbatim after "path:" (binary file matches)
}

// grepFile is everything one file contributes to a search result.
type grepFile struct {
	path  string // absolute path as searched
	count int    // count mode: matching lines (multiline: distinct start lines)
	hits  []grepHit
}

// grepRequest is the normalized Grep input both engines run.
type grepRequest struct {
	pattern       string
	mode          string
	root          string // absolute search path
	rootIsDir     bool
	globs         []string // overrides in order (user globs, then VCS excludes)
	typ           string
	caseInsens    bool
	multiline     bool
	onlyMatching  bool
	before, after int
	contextSet    bool // any context flag > 0 (enables "--" separators)
	hidden        bool
	respectIgnore bool
}

// splitGrepGlobs splits the glob parameter on whitespace,
// then on commas unless the piece uses {a,b} braces.
func splitGrepGlobs(glob string) []string {
	var out []string
	for _, piece := range strings.Fields(glob) {
		if strings.Contains(piece, "{") && strings.Contains(piece, "}") {
			out = append(out, piece)
			continue
		}
		for _, g := range strings.Split(piece, ",") {
			if g != "" {
				out = append(out, g)
			}
		}
	}
	return out
}

func vcsExcludeGlobs() []string {
	out := make([]string, len(vcsDirs))
	for i, d := range vcsDirs {
		out[i] = "!" + d
	}
	return out
}

// Execute runs the search with the session's engine (ugrep-accelerated when
// available, else the built-in Go engine alone) and renders the result shape.
// Both produce identical results for the same tree and settings.
func (t GrepTool) Execute(in GrepInput) (GrepResult, error) {
	mode := in.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	if mode != "content" && mode != "count" && mode != "files_with_matches" {
		return GrepResult{}, fmt.Errorf("invalid output_mode %q (want content, files_with_matches or count)", mode)
	}
	root := in.Path
	if root == "" {
		root = t.Engine.cwd()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return GrepResult{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return GrepResult{}, err
	}

	req := grepRequest{
		pattern:      in.Pattern,
		mode:         mode,
		root:         root,
		rootIsDir:    info.IsDir(),
		typ:          in.Type,
		caseInsens:   in.CaseInsensitive,
		multiline:    in.Multiline,
		onlyMatching: in.OnlyMatching && mode == "content",
	}
	req.hidden, req.respectIgnore = t.Engine.grepOptions()
	req.globs = append(splitGrepGlobs(in.Glob), vcsExcludeGlobs()...)
	if mode == "content" {
		// "context" wins over -C; -B/-A apply only when neither is given.
		switch {
		case in.ContextLong != nil:
			req.before, req.after = *in.ContextLong, *in.ContextLong
		case in.Context != nil:
			req.before, req.after = *in.Context, *in.Context
		default:
			if in.BeforeContext != nil {
				req.before = *in.BeforeContext
			}
			if in.AfterContext != nil {
				req.after = *in.AfterContext
			}
		}
		if req.before < 0 {
			req.before = 0
		}
		if req.after < 0 {
			req.after = 0
		}
		for _, p := range []*int{in.ContextLong, in.Context, in.BeforeContext, in.AfterContext} {
			if p != nil && *p > 0 {
				req.contextSet = true
			}
		}
	}

	files, err := grepSearch(t.Engine, req)
	if err != nil {
		return GrepResult{}, err
	}
	if req.rootIsDir {
		if pol := newPathPolicy(req.root, req.hidden, req.respectIgnore); pol != nil {
			kept := files[:0]
			for _, f := range files {
				if pol.allowed(f.path) {
					kept = append(kept, f)
				}
			}
			files = kept
		}
	}

	if t.Exclude != nil {
		kept := files[:0]
		for _, f := range files {
			if !t.Exclude(f.path) {
				kept = append(kept, f)
			}
		}
		files = kept
	}

	lineNumbers := true
	if in.LineNumbers != nil {
		lineNumbers = *in.LineNumbers
	}
	headLimit := GrepDefaultHeadLimit
	if in.HeadLimit != nil {
		headLimit = *in.HeadLimit
	}
	res := buildGrepResult(req, files, t.Engine.cwd(), lineNumbers, headLimit, in.Offset)
	res.Notice = t.Engine.takeNotice()
	return res, nil
}

// displayPath shows a path relative to the working directory when it lies
// inside it, else as given (absolute).
func displayPath(path, cwd string) string {
	if cwd == "" {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

func buildGrepResult(req grepRequest, files []grepFile, cwd string, lineNumbers bool, headLimit, offset int) GrepResult {
	switch req.mode {
	case "files_with_matches":
		type fm struct {
			path  string
			mtime int64
		}
		list := make([]fm, len(files))
		for i, f := range files {
			var mt int64
			if fi, err := os.Stat(f.path); err == nil {
				mt = fi.ModTime().UnixNano()
			}
			list[i] = fm{f.path, mt}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].mtime != list[j].mtime {
				return list[i].mtime > list[j].mtime
			}
			return list[i].path < list[j].path
		})
		names := make([]string, len(list))
		for i, f := range list {
			names[i] = displayPath(f.path, cwd)
		}
		limited, applied := applyLimitOffset(names, headLimit, offset)
		res := GrepResult{Mode: req.mode, NumFiles: len(limited), Filenames: limited, TotalFiles: len(names)}
		setLimitOffset(&res, applied, offset)
		return res

	case "count":
		sortGrepFiles(files)
		lines := make([]string, 0, len(files))
		names := make([]string, 0, len(files))
		total := 0
		for _, f := range files {
			d := displayPath(f.path, cwd)
			lines = append(lines, d+":"+strconv.Itoa(f.count))
			names = append(names, d)
			total += f.count
		}
		limited, applied := applyLimitOffset(lines, headLimit, offset)
		res := GrepResult{
			Mode:       req.mode,
			Content:    strings.Join(limited, "\n"),
			NumFiles:   len(files),
			Filenames:  names,
			NumMatches: total,
			TotalFiles: len(files),
		}
		setLimitOffset(&res, applied, offset)
		return res

	default: // content
		sortGrepFiles(files)
		var items []string
		var names []string
		matches := 0
		prevPath := ""
		prevLine := -1
		for _, f := range files {
			if len(f.hits) == 0 {
				continue
			}
			d := displayPath(f.path, cwd)
			names = append(names, d)
			for _, h := range f.hits {
				if req.contextSet && prevPath != "" && !h.raw && (f.path != prevPath || h.line > prevLine+1) {
					items = append(items, "--")
				}
				items = append(items, formatGrepHit(d, req.rootIsDir, lineNumbers, h))
				if !h.ctx && !h.raw {
					matches++
				}
				if !h.raw {
					prevPath, prevLine = f.path, h.line
				}
			}
		}
		limited, applied := applyLimitOffset(items, headLimit, offset)
		res := GrepResult{
			Mode:       req.mode,
			Content:    strings.Join(limited, "\n"),
			NumFiles:   len(names),
			Filenames:  names,
			NumLines:   len(limited),
			NumMatches: matches,
			TotalFiles: len(names),
			TotalLines: len(items),
		}
		setLimitOffset(&res, applied, offset)
		return res
	}
}

// formatGrepHit renders one content line: "path:N:text" for a match,
// "path-N-text" for context; without line numbers "path:text"/"path-text";
// for a single-file search the path is omitted.
func formatGrepHit(display string, rootIsDir, lineNumbers bool, h grepHit) string {
	if h.raw {
		if rootIsDir {
			return display + ":" + h.text
		}
		return h.text
	}
	sep := ":"
	kind := "match"
	if h.ctx {
		sep, kind = "-", "context"
	}
	text := h.text
	if utf16Len(text) > grepMaxLineChars {
		if kind == "match" {
			text = "[Omitted long matching line]"
		} else {
			text = "[Omitted long context line]"
		}
	}
	body := text
	if lineNumbers {
		body = strconv.Itoa(h.line) + sep + text
	}
	if rootIsDir {
		return display + sep + body
	}
	return body
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
		if utf16.RuneLen(r) < 0 {
			n += 2
		}
	}
	return n
}

func sortGrepFiles(files []grepFile) {
	sort.SliceStable(files, func(i, j int) bool { return files[i].path < files[j].path })
}

func setLimitOffset(res *GrepResult, appliedLimit *int, offset int) {
	res.AppliedLimit = appliedLimit
	if offset > 0 {
		o := offset
		res.AppliedOffset = &o
	}
}

// applyLimitOffset skips `offset` entries, then keep at most
// `limit` (0 == unlimited). appliedLimit is non-nil only when truncation
// actually dropped entries.
func applyLimitOffset(items []string, limit, offset int) ([]string, *int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(items) {
		offset = len(items)
	}
	rest := items[offset:]
	if limit == 0 {
		return rest, nil
	}
	if limit < 0 {
		limit = GrepDefaultHeadLimit
	}
	if len(rest) > limit {
		l := limit
		return rest[:limit], &l
	}
	return rest, nil
}

// ---------------------------------------------------------------- built-in --

// grepSearch runs a request: the walker picks the candidate files, ugrep
// (when the engine has one) narrows them to the files whose content matches,
// and the Go engine searches what is left and renders the hits. Results come
// back in walk order. A ugrep that rejects the pattern or fails leaves the
// whole candidate list to the Go engine; one that cannot start at all turns
// ugrep off for the session.
func grepSearch(e *SearchEngine, req grepRequest) ([]grepFile, error) {
	re, err := compileGrepPattern(req)
	if err != nil {
		return nil, err
	}
	paths, err := grepCandidates(req)
	if err != nil {
		return nil, err
	}
	// A multiline pattern is matched against whole files; ugrep matches a
	// Perl pattern across lines only when it spells out a newline, so it
	// could miss files there and the Go engine searches them all.
	if ug := e.UgrepPath(); ug != "" && len(paths) > 0 && !req.multiline {
		selected, uerr := ugrepSelect(e, ug, req, paths)
		switch {
		case uerr == nil:
			paths = selected
		case isStartFailure(uerr):
			e.markFailed(uerr)
		case errors.Is(uerr, errUgrepTimeout):
			return nil, fmt.Errorf("The search timed out after %d seconds. The search may have matched files but did not complete in time. Try searching a more specific path or pattern.", e.timeoutSeconds())
		}
	}
	return searchFiles(paths, re, req), nil
}

// compileGrepPattern compiles the request's pattern for the Go engine: line
// anchors per line, plus -i and multiline dot-all as requested.
func compileGrepPattern(req grepRequest) (*regexp.Regexp, error) {
	flags := "m"
	if req.caseInsens {
		flags += "i"
	}
	if req.multiline {
		flags += "s"
	}
	re, err := regexp.Compile("(?" + flags + ")" + req.pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	return re, nil
}

// grepCandidates lists the files a request searches: the named file, or the
// walk of the directory under the ignore, hidden, glob and type rules.
func grepCandidates(req grepRequest) ([]string, error) {
	if !req.rootIsDir {
		// An explicitly named file is always searched.
		return []string{req.root}, nil
	}
	var types *typeMatcher
	if req.typ != "" {
		tm, ok := newTypeMatcher(req.typ)
		if !ok {
			return nil, fmt.Errorf("Search failed — the pattern, glob, or file type was rejected without searching:\nunrecognized file type: %s", req.typ)
		}
		types = tm
	}
	filter, chain := newWalkFilter(req.root, req.hidden, req.respectIgnore, newOverrideSet(req.root, req.globs), types)
	return walkFiles(req.root, filter, chain), nil
}

// ugrepArgs is the ugrep argv that lists, NUL-terminated, which of the files
// named on stdin have a line matching the pattern. The pattern runs as a
// Perl-compatible regex, whose syntax covers the Go engine's; the Go engine
// then confirms each listed file and renders its matches.
func ugrepArgs(req grepRequest) []string {
	a := []string{"--no-config", "-l", "-0", "-s", "-Y", "-P", "--color=never"}
	if req.caseInsens {
		a = append(a, "-i")
	}
	return append(a, "-e", req.pattern, "--from=-")
}

// ugrepSelect returns the paths (in their given order) ugrep reports as
// matching. A name holding a newline cannot be passed on the newline-separated
// list, so it is always kept for the Go engine to search.
func ugrepSelect(e *SearchEngine, ug string, req grepRequest, paths []string) ([]string, error) {
	var list bytes.Buffer
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\r") {
			continue
		}
		list.WriteString(p)
		list.WriteByte('\n')
	}
	run, err := e.runUgrep(ug, ugrepArgs(req), list.Bytes())
	if err != nil {
		return nil, err
	}
	if run.overflow || (run.code != 0 && run.code != 1) {
		return nil, fmt.Errorf("ugrep exited with status %d", run.code)
	}
	hit := make(map[string]bool)
	for _, p := range bytes.Split(run.out, []byte{0}) {
		if s := strings.TrimPrefix(string(p), "\n"); s != "" {
			hit[filepath.Clean(s)] = true
		}
	}
	kept := make([]string, 0, len(hit))
	for _, p := range paths {
		if hit[filepath.Clean(p)] || strings.ContainsAny(p, "\n\r") {
			kept = append(kept, p)
		}
	}
	return kept, nil
}

// searchFiles searches paths with the Go engine in a bounded worker pool and
// collects the files with matches in the given order.
func searchFiles(paths []string, re *regexp.Regexp, req grepRequest) []grepFile {
	results := make([]grepFile, len(paths))
	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	if workers > len(paths) {
		workers = len(paths)
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				results[i] = searchFileBuiltin(paths[i], re, req)
			}
		}()
	}
	for i := range paths {
		next <- i
	}
	close(next)
	wg.Wait()

	out := results[:0]
	for _, r := range results {
		if r.count > 0 {
			out = append(out, r)
		}
	}
	return out
}

// searchFileBuiltin searches one file; a file with a NUL byte in its first
// 64KiB is binary and skipped.
func searchFileBuiltin(path string, re *regexp.Regexp, req grepRequest) grepFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return grepFile{}
	}
	sniff := data
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return grepFile{}
	}
	f := grepFile{path: path}
	text := string(data)
	// Line start offsets.
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' && i+1 < len(text) {
			starts = append(starts, i+1)
		}
	}
	nLines := len(starts)
	if len(text) == 0 {
		nLines = 0
	}
	lineAt := func(off int) int { // 0-based line index of byte offset
		return sort.Search(len(starts), func(i int) bool { return starts[i] > off }) - 1
	}
	lineText := func(i int) string {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1] - 1
		} else if strings.HasSuffix(text, "\n") {
			end = len(text) - 1
		}
		return text[starts[i]:end]
	}

	matched := make(map[int]bool)
	type sub struct {
		line int
		text string
	}
	var subs []sub

	if req.multiline {
		startLines := make(map[int]bool)
		for _, loc := range re.FindAllStringIndex(text, -1) {
			if loc[1] == loc[0] && loc[0] >= len(text) && nLines > 0 {
				continue
			}
			first := lineAt(loc[0])
			last := first
			if loc[1] > loc[0] {
				last = lineAt(loc[1] - 1)
			}
			startLines[first] = true
			for l := first; l <= last; l++ {
				matched[l] = true
			}
			if req.onlyMatching && loc[1] > loc[0] {
				for k, piece := range strings.Split(text[loc[0]:loc[1]], "\n") {
					subs = append(subs, sub{first + k, piece})
				}
			}
		}
		f.count = len(startLines)
	} else {
		for i := 0; i < nLines; i++ {
			lt := lineText(i)
			if !re.MatchString(lt) {
				continue
			}
			matched[i] = true
			f.count++
			if req.onlyMatching {
				for _, m := range re.FindAllString(lt, -1) {
					if m != "" {
						subs = append(subs, sub{i, m})
					}
				}
			}
		}
	}
	if req.mode != "content" || f.count == 0 {
		return f
	}

	if req.onlyMatching {
		for _, s := range subs {
			f.hits = append(f.hits, grepHit{line: s.line + 1, text: strings.TrimSuffix(s.text, "\r")})
		}
		return f
	}
	emit := make(map[int]bool, len(matched))
	for i := range matched {
		lo, hi := i-req.before, i+req.after
		if lo < 0 {
			lo = 0
		}
		if hi > nLines-1 {
			hi = nLines - 1
		}
		for j := lo; j <= hi; j++ {
			emit[j] = true
		}
	}
	order := make([]int, 0, len(emit))
	for i := range emit {
		order = append(order, i)
	}
	sort.Ints(order)
	for _, i := range order {
		f.hits = append(f.hits, grepHit{line: i + 1, ctx: !matched[i], text: strings.TrimSuffix(lineText(i), "\r")})
	}
	return f
}
