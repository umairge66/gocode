package pathguard

// permrules.go matches the content of a file-path
// permission rule ("Read(./secrets/**)", "Edit(//etc/**)", "Read(~/.ssh/**)")
// against a requested path. The rule text is never compared with the
// path as strings: the rule is split into a root and a gitignore-style
// pattern relative to that root, the pattern is normalized, the path is
// relativized to each root (path.relative, skipping roots the path is not
// under) and tested with gitignore semantics. A rule therefore
// covers a directory's descendants ("secrets" covers "secrets/key"), a pattern
// without an inner "/" matches at any depth, and "//" anchors at "/".

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// goos is runtime.GOOS; tests set it to exercise the Windows path rules on
// any platform.
var goos = runtime.GOOS

// Windows paths in path rules.
//
// Windows paths have a drive ("D:\work\x") and are not case-sensitive, while
// rules are written in POSIX form. An absolute rule ("//...") is matched
// against the path's POSIX spelling, the drive letter as the first segment in
// lower case: D:\work\x is /d/work/x, so "//d/work/**" covers it (the
// form Claude Code documents for Windows). "//D:/work/**" and
// "//D:\work\**" are read the same way. Every rule is matched without regard
// to case on Windows, so a different spelling of a name cannot step around a
// deny rule. A UNC path (\\server\share\...) has no drive and is not matched
// by an absolute rule.

// PathRuleRoots holds the directories a path rule is resolved against.
type PathRuleRoots struct {
	// Cwd is the session's working directory; relative rules and
	// "/x" rules from session / cliArg / command sources resolve here, as do
	// project, local and policy settings.
	Cwd string
	// Home is the user's home directory, the root of "~/x".
	Home string
	// UserConfigDir is the config home, the root of a "/x" rule
	// from userSettings.
	UserConfigDir string
	// FlagSettingsDir is the directory of the --settings file, the root of a
	// "/x" rule from flagSettings. Empty
	// falls back to Cwd.
	FlagSettingsDir string
}

// sourceRoot returns the root a "/x" rule from source resolves against.
func (r PathRuleRoots) sourceRoot(source string) string {
	switch source {
	case "userSettings":
		if r.UserConfigDir != "" {
			return r.UserConfigDir
		}
	case "flagSettings":
		if r.FlagSettingsDir != "" {
			return r.FlagSettingsDir
		}
	}
	return r.Cwd
}

// ResolvePathRule splits a path rule's content into the root it is relative
// to and the pattern:
//
//   - "//x"   → root "/",          pattern "/x"
//   - "~/x"   → root Home,         pattern "/x"
//   - "/x"    → root of the source, pattern "/x"
//   - "./x"   → root Cwd,          pattern "x"
//   - "x"     → root Cwd,          pattern "x"
func (r PathRuleRoots) ResolvePathRule(content, source string) (root, pattern string) {
	switch {
	case strings.HasPrefix(content, "//"):
		return "/", content[1:]
	case strings.HasPrefix(content, "~/"):
		return r.Home, content[1:]
	case strings.HasPrefix(content, "/"):
		return r.sourceRoot(source), content
	case strings.HasPrefix(content, "./"):
		return r.Cwd, content[2:]
	}
	return r.Cwd, content
}

// normalizeRulePattern collapses repeated slashes, then rewrites a
// trailing "/**". A deny / ask "dir/**" becomes "dir" (gitignore matching then
// covers the directory and everything beneath it; a single-segment "dir" floats
// to any depth); an allow "dir/**" with a single segment is anchored ("/dir").
func normalizeRulePattern(p string, allow bool) string {
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if strings.HasSuffix(p, "/**") {
		r := strings.TrimSuffix(p, "/**")
		if strings.Trim(r, "/") == "" {
			return "/**"
		}
		if strings.Contains(r, "/") || !allow || strings.HasPrefix(r, "!") || strings.HasPrefix(r, "#") {
			return r
		}
		return "/" + r
	}
	return p
}

var gitignoreCache sync.Map // pattern -> *regexp.Regexp (nil when unusable)

// compileGitignore converts one gitignore pattern (gitignore semantics,
// without negation) into a regexp anchored at the rule root. A pattern with a
// leading or inner "/" is anchored; any other pattern matches at any depth. A
// trailing "/" restricts the match to directories (a strict parent of the
// tested path here). "*" and "?" stay within one segment, "**" spans
// segments, "[...]" is a class and "\x" escapes x.
func compileGitignore(pat string) (re *regexp.Regexp, dirOnly bool) {
	// Windows paths are not case-sensitive, so neither is a rule there.
	fold := goos == "windows"
	key := pat
	if fold {
		key = "(?i)" + pat
	}
	if v, ok := gitignoreCache.Load(key); ok {
		c := v.(compiledIgnore)
		return c.re, c.dirOnly
	}
	defer func() { gitignoreCache.Store(key, compiledIgnore{re, dirOnly}) }()
	if pat == "" || strings.HasPrefix(pat, "#") || strings.HasPrefix(pat, "!") {
		return nil, false
	}
	if len(pat) > 1 && strings.HasSuffix(pat, "/") {
		dirOnly = true
		pat = strings.TrimSuffix(pat, "/")
	}
	anchored := strings.HasPrefix(pat, "/") || strings.Contains(pat, "/")
	pat = strings.TrimPrefix(pat, "/")
	if pat == "" {
		return nil, false
	}
	var b strings.Builder
	if fold {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	runes := []rune(pat)
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
				atStart := i == 0 || runes[i-1] == '/'
				j := i + 2
				switch {
				case atStart && j < len(runes) && runes[j] == '/':
					b.WriteString("(?:.*/)?")
					i = j
				case atStart && j == len(runes):
					b.WriteString(".*")
					i = j - 1
				default:
					b.WriteString("[^/]*")
					i++
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
				b.WriteString(`\[`)
				continue
			}
			body := runes[i+1 : j]
			b.WriteByte('[')
			k := 0
			if len(body) > 0 && (body[0] == '!' || body[0] == '^') {
				b.WriteByte('^')
				k = 1
			}
			for ; k < len(body); k++ {
				r := body[k]
				if r == '\\' || r == '[' || r == ']' || r == '^' {
					b.WriteByte('\\')
				}
				b.WriteRune(r)
			}
			b.WriteByte(']')
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	compiled, err := regexp.Compile(b.String())
	if err != nil {
		return nil, false
	}
	return compiled, dirOnly
}

type compiledIgnore struct {
	re      *regexp.Regexp
	dirOnly bool
}

// gitignoreMatches reports whether rel (a "/"-separated path relative to the
// rule root) is matched by pattern, gitignore style: the path itself or any
// of its parent directories matching the pattern ignores it.
func gitignoreMatches(pattern, rel string) bool {
	re, dirOnly := compileGitignore(pattern)
	if re == nil {
		return false
	}
	segs := strings.Split(rel, "/")
	for i := 1; i <= len(segs); i++ {
		if dirOnly && i == len(segs) {
			break
		}
		if re.MatchString(strings.Join(segs[:i], "/")) {
			return true
		}
	}
	return false
}

// relUnderRoot returns the path relative to root (path.relative plus a
// validity test), or
// false when p is root itself or not beneath it.
func relUnderRoot(root, p string) (string, bool) {
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

var rootTwinCache sync.Map // root -> canonical root ("" when same / unresolvable)

// physicalTwin returns the symlink-resolved spelling of a rule root when it
// differs from the root (a rule written against /tmp also covers
// /private/tmp), or "".
func physicalTwin(root string) string {
	if root == "" || root == "/" {
		return ""
	}
	if v, ok := rootTwinCache.Load(root); ok {
		return v.(string)
	}
	twin := ""
	if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
		twin = r
	}
	rootTwinCache.Store(root, twin)
	return twin
}

// PathRuleMatches reports whether the content of a path rule from source
// covers the absolute, cleaned path p. allow selects the allow-side pattern
// normalization. A deny / ask rule is also tried against the
// symlink-resolved spelling of its root.
func (r PathRuleRoots) PathRuleMatches(content, source, p string, allow bool) bool {
	// On Windows a rule written as a drive path ("C:\\Users\\me\\x",
	// "C:/Users/me/x") is an absolute rule.
	if goos == "windows" && len(content) >= 3 && isASCIILetter(content[0]) && content[1] == ':' && (content[2] == '\\' || content[2] == '/') {
		content = "//" + content
	}
	if r.pathRuleMatches(content, source, p, allow) {
		return true
	}
	// Extra strictness: a deny / ask "/x" rule (relative to its settings
	// root) is also read as the absolute path "//x", so a rule a user wrote
	// meaning the absolute path still refuses it. Allow rules keep the
	// settings-relative reading only.
	if !allow && strings.HasPrefix(content, "/") && !strings.HasPrefix(content, "//") {
		return r.pathRuleMatches("/"+content, source, p, allow)
	}
	return false
}

func (r PathRuleRoots) pathRuleMatches(content, source, p string, allow bool) bool {
	root, rawPattern := r.ResolvePathRule(content, source)
	if root == "/" && goos == "windows" {
		return windowsAbsoluteRuleMatches(rawPattern, p, allow)
	}
	pattern := normalizeRulePattern(rawPattern, allow)
	if pattern == "/**" || pattern == "**" {
		// Whole-root form: everything beneath the root (not the root
		// itself, which is skipped as an empty relative path).
		_, ok := relUnderRoot(root, p)
		if ok {
			return true
		}
		if !allow {
			if twin := physicalTwin(root); twin != "" {
				_, ok = relUnderRoot(twin, p)
			}
		}
		return ok
	}
	if rel, ok := relUnderRoot(root, p); ok && gitignoreMatches(pattern, rel) {
		return true
	}
	if !allow {
		if twin := physicalTwin(root); twin != "" {
			if rel, ok := relUnderRoot(twin, p); ok && gitignoreMatches(pattern, rel) {
				return true
			}
		}
	}
	return false
}

// windowsAbsoluteRuleMatches matches an absolute rule's pattern (the part
// after the first "/" of "//...") against a Windows path p. Both are brought
// to the POSIX drive form first (see "Windows paths in path rules").
func windowsAbsoluteRuleMatches(rawPattern, p string, allow bool) bool {
	posix, ok := windowsPOSIXPath(p)
	if !ok {
		return false
	}
	pattern := normalizeRulePattern(windowsRulePattern(rawPattern), allow)
	rel := strings.TrimPrefix(posix, "/")
	if rel == "" {
		return false
	}
	if pattern == "/**" || pattern == "**" {
		return true
	}
	return gitignoreMatches(pattern, rel)
}

// windowsPOSIXPath spells a Windows drive path in POSIX form: "D:\a\b"
// (or "D:/a/b") is "/d/a/b". ok is false for a path without a drive letter
// (a UNC or relative path).
func windowsPOSIXPath(p string) (string, bool) {
	if len(p) < 2 || p[1] != ':' || !isASCIILetter(p[0]) {
		return "", false
	}
	rest := strings.ReplaceAll(p[2:], "\\", "/")
	for strings.Contains(rest, "//") {
		rest = strings.ReplaceAll(rest, "//", "/")
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest != "" && !strings.HasPrefix(rest, "/") {
		return "", false // "D:x" is relative to the drive's current directory
	}
	return "/" + strings.ToLower(p[:1]) + rest, true
}

// windowsRulePattern brings an absolute rule's pattern to the POSIX drive
// form: "/D:/work/**" and "/D:\work\**" become "/d/work/**"; "/d/work/**"
// is unchanged. Backslashes are separators here (a Windows user writing
// "//C:\Users\..." means a path), not gitignore escapes.
func windowsRulePattern(pat string) string {
	if !strings.HasPrefix(pat, "/") {
		return pat
	}
	body := pat[1:]
	if len(body) >= 2 && isASCIILetter(body[0]) && body[1] == ':' {
		body = strings.ToLower(body[:1]) + body[2:]
		body = strings.ReplaceAll(body, "\\", "/")
	}
	return "/" + body
}

// IsMatchAllPathRule reports whether a rule content covers the whole
// filesystem ("//**", "//*", "//**/*"…), the shape whose deny / ask also
// reaches the sibling file tools.
func IsMatchAllPathRule(content string) bool {
	if !strings.HasPrefix(content, "//") {
		return false
	}
	c := content
	for strings.Contains(c, "//") {
		c = strings.ReplaceAll(c, "//", "/")
	}
	return matchAllRe.MatchString(c)
}

var matchAllRe = regexp.MustCompile(`^/(?:\*\*/)?\*+(?:/\*\*)?$`)

// PathSpellingsFor returns the spellings a path rule is judged on when no
// session walk is available: the cleaned absolute path (a leading "~/" is
// expanded against home, a relative path joined to cwd) and, when it
// differs, its symlink-resolved spelling (for a missing leaf, the resolved
// parent joined with the leaf).
func PathSpellingsFor(p, cwd, home string) []string {
	abs := AbsRequestedPath(p, cwd, home)
	if abs == "" {
		return nil
	}
	out := []string{abs}
	if r := resolveLanding(abs); r != "" && r != abs {
		out = append(out, r)
	}
	return out
}

// AbsRequestedPath normalizes a requested path the way the tools do:
// "~" and "~/x" expand against home, a relative path is joined to
// cwd, and the result is cleaned.
func AbsRequestedPath(p, cwd, home string) string {
	if p == "" {
		return ""
	}
	if home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// resolveLanding resolves the symlinks of abs; for a path that does not exist
// yet it resolves the deepest existing ancestor and re-attaches the rest.
func resolveLanding(abs string) string {
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, rest := filepath.Dir(abs), filepath.Base(abs)
	for dir != abs && dir != "" {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		abs, dir = dir, next
	}
	return ""
}

// RuleRoots returns the path-rule roots of this session: its cwd, home,
// config home and --settings directory.
func (s *SessionFiles) RuleRoots() PathRuleRoots {
	return PathRuleRoots{Cwd: s.cwd(), Home: s.HomeDir, UserConfigDir: s.ConfigDir, FlagSettingsDir: s.FlagSettingsDir}
}

// ReadsBlockedOutside applies permissions.blockReadsOutsideWorkingDirectories
// to a read's spellings through the RuleSetEngine (step 4 of EvaluateSpellings: a spelling
// outside the working directories is a safetyCheck refusal with the
// "outsideReadsBlocked" breaker; an unresolved walk is refused the same way).
// It returns nil when the setting is off or the read is inside.
func (s *SessionFiles) ReadsBlockedOutside(res PathResolution) *Decision {
	if s == nil || !s.BlockReadsOutsideWorkingDirectories {
		return nil
	}
	var dirs []string
	for _, d := range s.WorkingDirs() {
		dirs = append(dirs, s.spellingsOf(d)...)
	}
	eng := RuleSetEngine{
		Mode:                                ModeAllowAllExcept,
		WorkingDirs:                         dirs,
		BlockReadsOutsideWorkingDirectories: true,
		Restricted:                          s.Restricted,
		Platform:                            s.platform(),
		TrustedNetworkDirectories:           s.TrustedNetworkDirectories,
	}
	dec := eng.EvaluateSpellings(PathSpellings{Requested: res.Requested, Spellings: res.Spellings, Unresolved: res.Unresolved}, OpRead)
	if dec == nil || dec.Allowed {
		return nil
	}
	return dec
}
