package filetools

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file holds the walker that picks the files the Grep and Glob tools
// search and list, whichever engine then reads them:
//
//   - overrides (the tools' glob filters): gitignore-style globs relative to the search root;
//     the last matching glob wins; "!g" excludes, "g" includes. When any
//     include glob exists, a FILE matching none is excluded. A decisive
//     override result short-circuits every other rule (it beats .gitignore
//     and the hidden-file filter).
//   - ignore files (when respected): .rgignore > .ignore > .gitignore >
//     .git/info/exclude > the global git excludes file, deepest directory
//     first, last matching line wins within a file. Git-specific files are
//     only honored inside a git repository, and .gitignore files above the
//     repository root are not consulted.
//   - --type: a file must match one of the type's globs (base name).
//   - hidden: names starting with "." are skipped unless includeHidden, or
//     unless an ignore-file negation or the type filter whitelisted them.
//
// Explicitly named single files bypass all of this.

// globRegexp converts a gitignore-style glob to an (unanchored) regexp
// body: "*" and "?" stay within one path segment, "**" spans segments
// ("**/" may match nothing), "[...]"/"[!...]" are classes, "{a,b}" is
// alternation and "\x" escapes x.
func globRegexp(pattern string) string {
	var b strings.Builder
	runes := []rune(pattern)
	depth := 0
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '\\':
			if i+1 < len(runes) {
				i++
				b.WriteString(regexp.QuoteMeta(string(runes[i])))
			} else {
				b.WriteString(`\\`)
			}
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				// "**" is only special as a whole segment; elsewhere it is "*".
				atStart := i == 0 || runes[i-1] == '/'
				i++
				if atStart && i+1 < len(runes) && runes[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else if atStart && i+1 == len(runes) {
					b.WriteString(".*")
				} else {
					b.WriteString("[^/]*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			if j < len(runes) && (runes[j] == '!' || runes[j] == '^') {
				j++
			}
			if j < len(runes) && runes[j] == ']' {
				j++
			}
			for j < len(runes) && runes[j] != ']' {
				j++
			}
			if j >= len(runes) {
				b.WriteString(regexp.QuoteMeta("["))
				continue
			}
			body := runes[i+1 : j]
			var cls strings.Builder
			cls.WriteByte('[')
			k := 0
			if len(body) > 0 && (body[0] == '!' || body[0] == '^') {
				cls.WriteByte('^')
				k = 1
			}
			for ; k < len(body); k++ {
				r := body[k]
				if r == '\\' || r == '[' || r == ']' || (r == '^' && k == 0) {
					cls.WriteByte('\\')
				}
				cls.WriteRune(r)
			}
			cls.WriteByte(']')
			b.WriteString(cls.String())
			i = j
		case '{':
			depth++
			b.WriteString("(?:")
		case '}':
			if depth > 0 {
				depth--
				b.WriteString(")")
			} else {
				b.WriteString(`\}`)
			}
		case ',':
			if depth > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	for ; depth > 0; depth-- {
		b.WriteString(")")
	}
	return b.String()
}

// globRule is one gitignore-style line (or one override glob).
type globRule struct {
	re       *regexp.Regexp
	negate   bool // "!" prefix
	dirOnly  bool // trailing "/"
	anchored bool // contains a "/" (other than trailing): matched against the full relative path
}

// parseGlobRule parses one gitignore line; ok is false for blanks/comments.
func parseGlobRule(line string) (globRule, bool) {
	line = strings.TrimSuffix(line, "\r")
	// Trailing unescaped spaces are ignored.
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return globRule{}, false
	}
	var r globRule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") && !strings.HasSuffix(line, `\/`) {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if strings.HasPrefix(line, "/") {
		r.anchored = true
		line = strings.TrimLeft(line, "/")
	}
	if strings.Contains(line, "/") {
		r.anchored = true
	}
	if line == "" {
		return globRule{}, false
	}
	re, err := regexp.Compile("^" + globRegexp(line) + "$")
	if err != nil {
		return globRule{}, false
	}
	r.re = re
	return r, true
}

// matches reports whether the rule matches rel (slash-separated, relative to
// the rule's base directory).
func (r globRule) matches(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if r.anchored {
		return r.re.MatchString(rel)
	}
	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}
	return r.re.MatchString(base)
}

// matchResult is the outcome of a matcher for one path.
type matchResult int

const (
	matchNone matchResult = iota
	matchIgnore
	matchWhitelist
)

// ignoreFile is the parsed rules of one ignore file, rooted at dir.
type ignoreFile struct {
	dir   string
	rules []globRule
}

func loadIgnoreFile(path, dir string) *ignoreFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	f := &ignoreFile{dir: dir}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if r, ok := parseGlobRule(sc.Text()); ok {
			f.rules = append(f.rules, r)
		}
	}
	if len(f.rules) == 0 {
		return nil
	}
	return f
}

// match applies the file's rules to abs (last matching rule wins).
func (f *ignoreFile) match(abs string, isDir bool) matchResult {
	if f == nil {
		return matchNone
	}
	rel, err := filepath.Rel(f.dir, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return matchNone
	}
	rel = filepath.ToSlash(rel)
	for i := len(f.rules) - 1; i >= 0; i-- {
		if f.rules[i].matches(rel, isDir) {
			if f.rules[i].negate {
				return matchWhitelist
			}
			return matchIgnore
		}
	}
	return matchNone
}

// overrideSet is the --glob list, matched relative to the search root.
type overrideSet struct {
	root       string
	rules      []globRule // negate here means "!glob" (exclude)
	whitelists int
}

func newOverrideSet(root string, globs []string) *overrideSet {
	o := &overrideSet{root: root}
	for _, g := range globs {
		r, ok := parseGlobRule(g)
		if !ok {
			continue
		}
		if !r.negate {
			o.whitelists++
		}
		o.rules = append(o.rules, r)
	}
	return o
}

func (o *overrideSet) match(abs string, isDir bool) matchResult {
	if o == nil || len(o.rules) == 0 {
		return matchNone
	}
	rel, err := filepath.Rel(o.root, abs)
	if err != nil {
		return matchNone
	}
	rel = filepath.ToSlash(rel)
	for i := len(o.rules) - 1; i >= 0; i-- {
		if o.rules[i].matches(rel, isDir) {
			if o.rules[i].negate {
				return matchIgnore
			}
			return matchWhitelist
		}
	}
	if o.whitelists > 0 && !isDir {
		return matchIgnore
	}
	return matchNone
}

// typeMatcher is the file-type filter (base-name globs).
type typeMatcher struct{ res []*regexp.Regexp }

func newTypeMatcher(name string) (*typeMatcher, bool) {
	globs, ok := fileTypeGlobs[name]
	if !ok {
		return nil, false
	}
	t := &typeMatcher{}
	for _, g := range globs {
		if re, err := regexp.Compile("^" + globRegexp(g) + "$"); err == nil {
			t.res = append(t.res, re)
		}
	}
	return t, true
}

func (t *typeMatcher) match(abs string, isDir bool) matchResult {
	if t == nil || isDir {
		return matchNone
	}
	base := filepath.Base(abs)
	for _, re := range t.res {
		if re.MatchString(base) {
			return matchWhitelist
		}
	}
	return matchIgnore
}

// dirIgnores are the ignore files found in one directory.
type dirIgnores struct {
	dir                      string
	rgignore, ignore, gitign *ignoreFile
	hasGit                   bool
}

func loadDirIgnores(dir string) *dirIgnores {
	d := &dirIgnores{dir: dir}
	d.rgignore = loadIgnoreFile(filepath.Join(dir, ".rgignore"), dir)
	d.ignore = loadIgnoreFile(filepath.Join(dir, ".ignore"), dir)
	d.gitign = loadIgnoreFile(filepath.Join(dir, ".gitignore"), dir)
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		d.hasGit = true
	}
	return d
}

// walkFilter decides, for every entry under the search root, whether the
// built-in walker keeps it (files) or descends into it (directories).
type walkFilter struct {
	includeHidden bool
	useIgnores    bool
	overrides     *overrideSet
	types         *typeMatcher
	// repo-level rules, set when the search root is inside a git repository.
	gitExclude    *ignoreFile
	globalExclude *ignoreFile
}

// newWalkFilter prepares the filter for root, loading the ignore files of the
// root's ancestors (parents of the root are consulted too) and the
// repository-level excludes.
func newWalkFilter(root string, includeHidden, useIgnores bool, overrides *overrideSet, types *typeMatcher) (*walkFilter, []*dirIgnores) {
	f := &walkFilter{includeHidden: includeHidden, useIgnores: useIgnores, overrides: overrides, types: types}
	if !useIgnores {
		return f, nil
	}
	// Ancestors, root first then upward; reversed below to outermost-first.
	var chain []*dirIgnores
	repoRoot := ""
	for dir := root; ; {
		d := loadDirIgnores(dir)
		chain = append(chain, d)
		if d.hasGit && repoRoot == "" {
			repoRoot = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if repoRoot != "" {
		f.gitExclude = loadIgnoreFile(filepath.Join(repoRoot, ".git", "info", "exclude"), repoRoot)
		if p := globalExcludesFile(); p != "" {
			f.globalExclude = loadIgnoreFile(p, repoRoot)
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return f, chain
}

// globalExcludesFile finds git's core.excludesFile (from ~/.gitconfig or
// $XDG_CONFIG_HOME/git/config), defaulting to $XDG_CONFIG_HOME/git/ignore.
func globalExcludesFile() string {
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" && home != "" {
		xdg = filepath.Join(home, ".config")
	}
	var configs []string
	if home != "" {
		configs = append(configs, filepath.Join(home, ".gitconfig"))
	}
	if xdg != "" {
		configs = append(configs, filepath.Join(xdg, "git", "config"))
	}
	for _, c := range configs {
		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		inCore := false
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				inCore = strings.EqualFold(strings.Trim(line, "[] \t"), "core")
				continue
			}
			if !inCore {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "excludesfile") {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"`)
			if strings.HasPrefix(v, "~/") && home != "" {
				v = filepath.Join(home, v[2:])
			}
			return v
		}
	}
	if xdg != "" {
		return filepath.Join(xdg, "git", "ignore")
	}
	return ""
}

// decide applies the rule precedence for one entry; chain is the ignore stack
// from the outermost ancestor down to the entry's parent directory.
func (f *walkFilter) decide(abs, name string, isDir bool, chain []*dirIgnores) bool {
	if m := f.overrides.match(abs, isDir); m != matchNone {
		return m == matchWhitelist
	}
	whitelisted := false
	if f.useIgnores {
		m := f.ignoreMatch(abs, isDir, chain)
		if m == matchIgnore {
			return false
		}
		whitelisted = m == matchWhitelist
	}
	if f.types != nil {
		m := f.types.match(abs, isDir)
		if m == matchIgnore {
			return false
		}
		if m == matchWhitelist {
			whitelisted = true
		}
	}
	if !whitelisted && !f.includeHidden && strings.HasPrefix(name, ".") {
		return false
	}
	return true
}

func (f *walkFilter) ignoreMatch(abs string, isDir bool, chain []*dirIgnores) matchResult {
	var mRg, mIg, mGit matchResult
	anyGit := false
	for _, d := range chain {
		anyGit = anyGit || d.hasGit
	}
	sawGit := false
	for i := len(chain) - 1; i >= 0; i-- {
		d := chain[i]
		if mRg == matchNone {
			mRg = d.rgignore.match(abs, isDir)
		}
		if mIg == matchNone {
			mIg = d.ignore.match(abs, isDir)
		}
		// .gitignore applies only inside a repository, and not above the
		// nearest repository root.
		if anyGit && !sawGit && mGit == matchNone {
			mGit = d.gitign.match(abs, isDir)
		}
		sawGit = sawGit || d.hasGit
	}
	for _, m := range []matchResult{mRg, mIg, mGit} {
		if m != matchNone {
			return m
		}
	}
	if !anyGit {
		return matchNone
	}
	if m := f.gitExclude.match(abs, isDir); m != matchNone {
		return m
	}
	return f.globalExclude.match(abs, isDir)
}

// walkFiles lists the files under root the filter keeps, in lexical order.
// Symlinks are not followed. Unreadable directories are
// skipped.
func walkFiles(root string, f *walkFilter, chain []*dirIgnores) []string {
	var out []string
	var walk func(dir string, chain []*dirIgnores)
	walk = func(dir string, chain []*dirIgnores) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			abs := filepath.Join(dir, e.Name())
			isDir := e.IsDir()
			if !e.Type().IsRegular() && !isDir {
				continue
			}
			if !f.decide(abs, e.Name(), isDir, chain) {
				continue
			}
			if isDir {
				next := chain
				if f.useIgnores {
					next = append(chain[:len(chain):len(chain)], loadDirIgnores(abs))
				}
				walk(abs, next)
				continue
			}
			out = append(out, abs)
		}
	}
	walk(root, chain)
	return out
}

// pathPolicy re-checks result paths against the hidden / ignore settings.
//
// A matching glob override whitelists a file past the hidden filter and
// the ignore files, so a glob of '**/*' would list everything regardless of
// search.includeHidden / respectGitignore. To make those settings mean what
// they say, every result goes through this policy: a file is
// dropped when it (or any directory between the search root and it) is
// hidden while hidden entries are excluded, or is ignored by an ignore file
// while ignore files are respected. VCS directories are excluded separately.
type pathPolicy struct {
	root          string
	includeHidden bool
	respectIgnore bool
	filter        *walkFilter
	base          []*dirIgnores
	cache         map[string]*dirIgnores
}

// newPathPolicy returns nil when nothing needs re-checking.
func newPathPolicy(root string, includeHidden, respectIgnore bool) *pathPolicy {
	if includeHidden && !respectIgnore {
		return nil
	}
	p := &pathPolicy{root: root, includeHidden: includeHidden, respectIgnore: respectIgnore, cache: map[string]*dirIgnores{}}
	if respectIgnore {
		p.filter, p.base = newWalkFilter(root, true, true, nil, nil)
	}
	return p
}

func (p *pathPolicy) dirIgnores(dir string) *dirIgnores {
	if d, ok := p.cache[dir]; ok {
		return d
	}
	d := loadDirIgnores(dir)
	p.cache[dir] = d
	return d
}

// allowed reports whether abs (a file under root) passes the policy.
func (p *pathPolicy) allowed(abs string) bool {
	if p == nil {
		return true
	}
	rel, err := filepath.Rel(p.root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return true
	}
	comps := strings.Split(filepath.ToSlash(rel), "/")
	if !p.includeHidden {
		for _, c := range comps {
			if strings.HasPrefix(c, ".") {
				return false
			}
		}
	}
	if !p.respectIgnore {
		return true
	}
	chain := p.base
	cur := p.root
	for i, c := range comps {
		cur = filepath.Join(cur, c)
		isDir := i < len(comps)-1
		if p.filter.ignoreMatch(cur, isDir, chain) == matchIgnore {
			return false
		}
		if isDir {
			chain = append(chain[:len(chain):len(chain)], p.dirIgnores(cur))
		}
	}
	return true
}
