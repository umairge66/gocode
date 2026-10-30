package projectmemory

import (
	"io/fs"
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/frontmatter"
)

// Rules directories.
//
// A rules directory holds *.md files (searched recursively) that are loaded
// like gocode.md memory. A rule without a `paths:` frontmatter is
// unconditional and joins the startup memory block; a rule with one is
// conditional and is only injected once a file matching its globs is touched
// (see Nested).
const (
	rulesSubdir        = "rules"
	projectRulesSubdir = ".gocode/rules"
	ruleFileSuffix     = ".md"
)

// managedRulesDir is <ManagedDir>/.gocode/rules.
func managedRulesDir(managedDir string) string { return join(managedDir, projectRulesSubdir) }

// userRulesDir is <ConfigDir>/rules.
func userRulesDir(configDir string) string { return join(configDir, rulesSubdir) }

// projectRulesDir is <dir>/.gocode/rules.
func projectRulesDir(dir string) string { return join(dir, projectRulesSubdir) }

// loadRulesDir walks a rules directory and returns the entries of every *.md
// file in it (subdirectories included, in name order), each followed by its
// @imports. With conditional false only entries without globs are returned;
// with conditional true only entries with globs. Files are still marked
// processed either way (the processed set is shared).
//
// Symlinks: a linked directory or file is followed and loaded from its
// resolved path, and a visited set over both the link and its target breaks
// directory cycles. Outside the User tier, a link that leads out of the
// working directory is not followed, as there is no external-include approval
// flow.
func (l *loader) loadRulesDir(dir string, kind Kind, conditional bool) []Entry {
	return l.walkRules(normalizePath(dir), kind, conditional, map[string]bool{})
}

func (l *loader) walkRules(dir string, kind Kind, conditional bool, visited map[string]bool) []Entry {
	if dir == "" || visited[dir] {
		return nil
	}
	resolved, isLink, ok := l.resolveLink(dir)
	if !ok {
		return nil
	}
	if isLink && !l.mayFollow(kind, resolved) {
		return nil
	}
	visited[dir] = true
	visited[resolved] = true

	ents, err := l.fs.ReadDir(resolved)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, de := range ents {
		p := join(dir, de.Name())
		target := p
		isDir, isFile := de.IsDir(), de.Type().IsRegular()
		if de.Type()&fs.ModeSymlink != 0 {
			r, _, ok := l.resolveLink(p)
			if !ok || !l.mayFollow(kind, r) {
				continue
			}
			fi, err := l.fs.Stat(r)
			if err != nil {
				continue // dangling link
			}
			target, isDir, isFile = r, fi.IsDir(), fi.Mode().IsRegular()
		}
		switch {
		case isDir:
			out = append(out, l.walkRules(target, kind, conditional, visited)...)
		case isFile && strings.HasSuffix(de.Name(), ruleFileSuffix):
			for _, e := range l.collect(target, kind, "", 0, false) {
				if (len(e.Globs) > 0) == conditional {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// resolveLink reports where p leads. ok is false when p does not exist or its
// link cannot be resolved.
func (l *loader) resolveLink(p string) (resolved string, isLink, ok bool) {
	fi, err := l.fs.Lstat(p)
	if err != nil {
		return "", false, false
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return p, false, true
	}
	r, err := realpath(l.fs, p)
	if err != nil {
		return "", true, false
	}
	return normalizePath(r), true, true
}

// mayFollow reports whether a link resolving to target may be followed for a
// source of the given kind.
func (l *loader) mayFollow(kind Kind, target string) bool {
	if kind == KindUser {
		return true
	}
	return within(target, l.cwd)
}

// within reports whether p is base or below it.
func within(p, base string) bool {
	if p == "" || base == "" {
		return false
	}
	if p == base {
		return true
	}
	prefix := strings.TrimSuffix(base, "/") + "/"
	return strings.HasPrefix(p, prefix)
}

// splitMemoryFrontmatter strips a leading frontmatter block from a memory
// file and returns the body plus its `paths:` globs (nil when absent, empty,
// or all "**", which makes the file unconditional).
func splitMemoryFrontmatter(raw string) (content string, globs []string) {
	doc := frontmatter.ParseDocument(raw)
	if !doc.HasFrontmatter {
		return raw, nil
	}
	pats := splitPathPatterns(doc.Data["paths"])
	var out []string
	allStars := true
	for _, p := range pats {
		p = strings.TrimSuffix(p, "/**")
		if p == "" {
			continue
		}
		if p != "**" {
			allStars = false
		}
		out = append(out, p)
	}
	if len(out) == 0 || allStars {
		return doc.Body, nil
	}
	return doc.Body, out
}

// Brace-expansion budget.
const (
	braceMaxResults = 1000
	braceMaxBytes   = 4194304
)

// splitPathPatterns turns a `paths:` value (a string or a list of strings)
// into patterns: each string is split on commas outside braces, trimmed, and
// brace-expanded. Other value types yield nothing.
func splitPathPatterns(v any) []string {
	budget := braceBudget{results: braceMaxResults, bytes: braceMaxBytes}
	return splitPathValue(v, &budget)
}

func splitPathValue(v any, b *braceBudget) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, splitPathValue(e, b)...)
		}
		return out
	case []string:
		var out []string
		for _, e := range t {
			out = append(out, splitPathValue(e, b)...)
		}
		return out
	case string:
		var parts []string
		var cur strings.Builder
		depth := 0
		for _, r := range t {
			switch {
			case r == '{':
				depth++
				cur.WriteRune(r)
			case r == '}':
				depth--
				cur.WriteRune(r)
			case r == ',' && depth == 0:
				if p := strings.TrimSpace(cur.String()); p != "" {
					parts = append(parts, p)
				}
				cur.Reset()
			default:
				cur.WriteRune(r)
			}
		}
		if p := strings.TrimSpace(cur.String()); p != "" {
			parts = append(parts, p)
		}
		var out []string
		for _, p := range parts {
			out = append(out, expandBraces(p, b)...)
		}
		return out
	}
	return nil
}

type braceBudget struct{ results, bytes int }

// braceRe matches the first innermost-free {a,b} group.
var braceRe = regexp.MustCompile(`^([^{]*)\{([^}]+)\}(.*)$`)

// expandBraces expands {a,b} alternations. When the expansion would exceed the
// shared budget, the pattern is used unexpanded.
func expandBraces(p string, b *braceBudget) []string {
	if !strings.Contains(p, "{") {
		return []string{p}
	}
	var done []string
	stack := []string{p}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		m := braceRe.FindStringSubmatch(h)
		if m == nil {
			done = append(done, h)
			continue
		}
		alts := strings.Split(m[2], ",")
		b.bytes -= len(h)
		total := len(done) + len(stack) + len(alts)
		if b.bytes < 0 || total > b.results || total*len(p) > b.bytes {
			return []string{p}
		}
		for i := len(alts) - 1; i >= 0; i-- {
			stack = append(stack, m[1]+strings.TrimSpace(alts[i])+m[3])
		}
	}
	b.results -= len(done)
	b.bytes -= len(done) * len(p)
	return done
}

// globsMatch reports whether rel (a slash path relative to the rule's base
// directory) is matched by the gitignore-style globs: a pattern without a
// slash matches at any depth, a leading or inner slash anchors it to the base,
// a trailing slash matches directories only, "!" negates, the last matching
// pattern wins, a match on a parent directory covers everything below it, and
// matching ignores case. Invalid patterns are dropped.
func globsMatch(globs []string, rel string) bool {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
		return false
	}
	var rules []globRule
	for _, g := range globs {
		if r, ok := compileGlobRule(g); ok {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return false
	}
	segs := strings.Split(rel, "/")
	// A parent directory that is matched covers the file (each parent is
	// checked before the path itself).
	for i := 1; i < len(segs); i++ {
		if evalGlobRules(rules, strings.Join(segs[:i], "/"), true) {
			return true
		}
	}
	return evalGlobRules(rules, rel, false)
}

type globRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

func evalGlobRules(rules []globRule, p string, isDir bool) bool {
	matched := false
	for _, r := range rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(p) {
			matched = !r.negate
		}
	}
	return matched
}

// compileGlobRule converts one gitignore-style pattern to a rule.
func compileGlobRule(pat string) (globRule, bool) {
	pat = strings.TrimRight(pat, " ")
	if pat == "" || strings.HasPrefix(pat, "#") {
		return globRule{}, false
	}
	var r globRule
	if strings.HasPrefix(pat, "!") {
		r.negate = true
		pat = pat[1:]
	} else if strings.HasPrefix(pat, `\!`) || strings.HasPrefix(pat, `\#`) {
		pat = pat[1:]
	}
	if strings.HasSuffix(pat, "/") {
		r.dirOnly = true
		pat = strings.TrimRight(pat, "/")
	}
	anchored := strings.Contains(pat, "/")
	pat = strings.TrimPrefix(pat, "/")
	if pat == "" {
		return globRule{}, false
	}
	body, ok := globToRegexp(pat)
	if !ok {
		return globRule{}, false
	}
	prefix := "^"
	if !anchored {
		prefix = "^(?:.*/)?"
	}
	re, err := regexp.Compile("(?i)" + prefix + body + "$")
	if err != nil {
		return globRule{}, false
	}
	r.re = re
	return r, true
}

// globToRegexp converts a glob body: "**" spans segments ("**/" may match
// nothing, a trailing "/**" matches everything inside), "*" and "?" stay
// within a segment, "[...]" / "[!...]" are classes and "\x" escapes x.
func globToRegexp(pat string) (string, bool) {
	var b strings.Builder
	rs := []rune(pat)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch c {
		case '\\':
			if i+1 < len(rs) {
				i++
				b.WriteString(regexp.QuoteMeta(string(rs[i])))
			}
		case '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				atStart := i == 0 || rs[i-1] == '/'
				i++
				switch {
				case atStart && i+1 < len(rs) && rs[i+1] == '/':
					i++
					b.WriteString("(?:.*/)?")
				case atStart && i+1 == len(rs):
					b.WriteString(".*")
				default:
					b.WriteString("[^/]*")
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := -1
			for j := i + 1; j < len(rs); j++ {
				if rs[j] == ']' && j > i+1 {
					end = j
					break
				}
			}
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := string(rs[i+1 : end])
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String(), true
}

// relUnder returns target relative to base, or ok=false when target is not
// below base.
func relUnder(base, target string) (string, bool) {
	base, target = normalizePath(base), normalizePath(target)
	if base == "" || target == "" || target == base || !within(target, base) {
		return "", false
	}
	return strings.TrimPrefix(target, strings.TrimSuffix(base, "/")+"/"), true
}
