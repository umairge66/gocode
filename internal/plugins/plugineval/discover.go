package plugineval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// LoadError is a case file that could not be loaded.
type LoadError struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// Filter narrows the cases run.
type Filter struct {
	CaseGlob string
	Tags     []string
}

func (f Filter) matches(name string, tags []string) bool {
	if f.CaseGlob != "" && !globToRegexp(f.CaseGlob).MatchString(name) {
		return false
	}
	if len(f.Tags) > 0 {
		for _, want := range f.Tags {
			for _, t := range tags {
				if t == want {
					return true
				}
			}
		}
		return false
	}
	return true
}

// globToRegexp: "*" any run, "?" one character, anchored.
func globToRegexp(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range g {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// skippedDirNames are never scanned for cases (".git" is skipped too).
var skippedDirNames = map[string]bool{"node_modules": true, ".gocode": true, "results": true, "mocks": true}

const (
	maxScanDepth = 16
	maxScanDirs  = 100000
	// maxPluginClimb bounds the nearest-plugin search.
	maxPluginClimb = 32
)

func isCaseFileName(n string) bool { return n == "case.yaml" || n == "prompt.md" }

// DiscoverOptions configures Discover.
type DiscoverOptions struct {
	// Target is the path named on the command line (absolute or relative to
	// Cwd): a directory to scan, or a case.yaml / prompt.md.
	Target string
	Cwd    string
	// EvalDir is the eval directory, as path segments below the plugin
	// (default ["evals"]).
	EvalDir []string
	// FrameRoot is the plugin enclosing the target ("" for none): plugins a
	// case names must lie below it, and a case with no plugin of its own runs
	// against it.
	FrameRoot string
	Filter    Filter
}

// Discovery is the result of Discover.
type Discovery struct {
	Cases  []*Case
	Errors []LoadError
	// Root is the containment root (the plugin, or the target directory).
	Root string
}

// Discover finds and loads the cases under the target. A case is a directory
// holding case.yaml or prompt.md inside the eval directory; cases nest no
// deeper than 16 levels below the target.
func Discover(o DiscoverOptions) Discovery {
	evalDir := o.EvalDir
	if len(evalDir) == 0 {
		evalDir = []string{DefaultEvalDir}
	}
	target := o.Target
	if !filepath.IsAbs(target) {
		target = filepath.Join(o.Cwd, target)
	}
	target = filepath.Clean(target)
	st, err := os.Stat(target)
	if err != nil {
		msg := "no such file or directory (or not readable)"
		if !errors.Is(err, os.ErrNotExist) {
			msg = fmt.Sprintf("cannot be read (%v)", err)
		}
		return Discovery{Root: target, Errors: []LoadError{{File: target, Error: msg}}}
	}
	real, err := filepath.EvalSymlinks(target)
	if err == nil {
		target = real
	}
	var root string
	var caseDirs []string
	var errs []LoadError
	if !st.IsDir() && isCaseFileName(filepath.Base(target)) {
		dir := filepath.Dir(target)
		root = dir
		if o.FrameRoot != "" && within(dir, o.FrameRoot) {
			root = o.FrameRoot
		}
		caseDirs = []string{dir}
	} else {
		root = target
		if o.FrameRoot != "" && target != o.FrameRoot && within(target, o.FrameRoot) {
			root = o.FrameRoot
		}
		if !st.IsDir() {
			return Discovery{Root: root, Errors: []LoadError{{File: target, Error: "is a file, not a directory or a case file (case.yaml / prompt.md) — nothing to scan"}}}
		}
		w := &walker{evalDir: evalDir, base: o.FrameRoot, target: target, budget: maxScanDirs, found: map[string]bool{}}
		inSuite := w.suffixMatch(splitPath(target))
		w.walk(target, 0, inSuite)
		caseDirs = w.cases
		errs = append(errs, w.skipped...)
	}
	sort.Strings(caseDirs)
	var cases []*Case
	for _, dir := range caseDirs {
		c, err := loadCaseDir(dir, root, o.FrameRoot, o.Filter)
		if err != nil {
			file := dir
			if _, serr := os.Stat(filepath.Join(dir, "case.yaml")); serr == nil {
				var ce *caseError
				if errors.As(err, &ce) {
					file = filepath.Join(dir, "case.yaml")
				}
			}
			errs = append(errs, LoadError{File: file, Error: err.Error()})
			continue
		}
		if c != nil {
			cases = append(cases, c)
		}
	}
	return Discovery{Cases: cases, Errors: errs, Root: root}
}

// loadCaseDir loads one case directory; (nil, nil) when the filter drops it.
func loadCaseDir(dir, root, frameRoot string, f Filter) (*Case, error) {
	caseFile := filepath.Join(dir, "case.yaml")
	yamlDoc, err := loadCaseYAML(caseFile)
	if err != nil {
		return nil, err
	}
	prose, err := loadProseCase(dir)
	if err != nil {
		return nil, err
	}
	doc := yamlDoc
	source := "case_yaml"
	switch {
	case prose != nil && yamlDoc != nil:
		doc, source = mergeProse(yamlDoc, prose, dir), "mixed"
	case prose != nil:
		doc, source = mergeProse(nil, prose, dir), "prose"
	}
	name := filepath.Base(dir)
	var tags []string
	if doc != nil {
		if n, ok := doc["name"].(string); ok && n != "" {
			name = n
		}
		if tl, ok := doc["tags"].([]any); ok {
			for _, t := range tl {
				if s, ok := t.(string); ok {
					tags = append(tags, s)
				}
			}
		}
	}
	if !f.matches(name, tags) {
		return nil, nil
	}
	if doc == nil {
		return nil, errors.New("no case definition: case.yaml and prompt.md are empty or missing here (write the user prompt to test in prompt.md, or define the case in case.yaml)")
	}
	c, err := ParseCase(doc)
	if err != nil {
		return nil, err
	}
	c.CaseFile, c.CaseDir, c.CaseSource = caseFile, dir, source
	if err := resolveCasePlugins(c, root, frameRoot); err != nil {
		return nil, err
	}
	return c, nil
}

// resolveCasePlugins: the case's `plugins:` entries
// (relative to the case directory, inside the containment root), else the
// nearest plugin manifest between the case and the root, else the frame root.
func resolveCasePlugins(c *Case, root, frameRoot string) error {
	if c.Plugins != nil && len(c.Plugins) > 0 {
		for _, p := range c.Plugins {
			abs := p
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(c.CaseDir, p)
			}
			real, err := filepath.EvalSymlinks(abs)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if lst, lerr := os.Lstat(abs); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
						return fmt.Errorf("case %q: plugins entry %q is a symbolic link whose target does not exist", c.Name, p)
					}
					return fmt.Errorf("case %q: plugins entry %q does not exist", c.Name, p)
				}
				return fmt.Errorf("case %q: plugins entry %q is unreadable (%v)", c.Name, p, err)
			}
			if !within(real, root) && !(frameRoot != "" && within(real, frameRoot)) {
				return fmt.Errorf("case %q: plugins entry %q resolves to %s, outside the containment root %s (the enclosing plugin for a target inside one you control, else the directory you ran 'gocode plugin eval' against). Only plugins under it can be loaded from case.yaml.", c.Name, p, real, root)
			}
			c.PluginDirs = append(c.PluginDirs, real)
		}
		return nil
	}
	if p := NearestPlugin(c.CaseDir, root); p != "" {
		c.PluginDirs = []string{p}
	} else if frameRoot != "" {
		c.PluginDirs = []string{frameRoot}
	}
	return nil
}

// NearestPlugin returns the closest directory from dir up to (and
// including) stop holding plugin.json, .gocode-plugin/plugin.json or the
// Claude-compatible .claude-plugin/plugin.json. "" when none.
func NearestPlugin(dir, stop string) string {
	h := dir
	for i := 0; i < maxPluginClimb; i++ {
		if HasPluginManifest(h) {
			return h
		}
		if h == stop {
			return ""
		}
		parent := filepath.Dir(h)
		if parent == h {
			return ""
		}
		h = parent
	}
	return ""
}

// HasPluginManifest reports whether dir is a plugin root (plugin.json or
// .gocode-plugin/plugin.json; .claude-plugin/ keeps Claude marketplace
// plugins working).
func HasPluginManifest(dir string) bool {
	for _, rel := range []string{"plugin.json", filepath.Join(".gocode-plugin", "plugin.json"), filepath.Join(".claude-plugin", "plugin.json")} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			return true
		}
	}
	return false
}

// walker is the bounded scan for case directories.
type walker struct {
	evalDir []string
	base    string
	target  string
	budget  int
	found   map[string]bool
	cases   []string
	skipped []LoadError
}

// routeIndependent is true for the default eval dir: any "evals" directory
// anywhere below the target opens a suite.
func (w *walker) routeIndependent() bool {
	return len(w.evalDir) == 1 && w.evalDir[0] == DefaultEvalDir
}

// suffixMatch: the path's last segments equal the eval dir (letter case
// folded on macOS and Windows).
func (w *walker) suffixMatch(segs []string) bool {
	n := len(w.evalDir)
	if n == 0 || len(segs) < n {
		return false
	}
	fold := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	tail := segs[len(segs)-n:]
	for i := range tail {
		if fold && !strings.EqualFold(tail[i], w.evalDir[i]) || !fold && tail[i] != w.evalDir[i] {
			return false
		}
	}
	return true
}

// relSegs are the path segments of p below the plugin (or the target).
func (w *walker) relSegs(p string) []string {
	base := w.base
	if base == "" || !within(p, base) {
		base = w.target
	}
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." {
		return nil
	}
	return splitRel(rel)
}

// opensEvalDir reports whether directory p is the eval directory.
func (w *walker) opensEvalDir(p string) bool {
	if w.routeIndependent() {
		return w.suffixMatch(splitPath(p))
	}
	rel := w.relSegs(p)
	if len(rel) != len(w.evalDir) {
		return false
	}
	for i := range rel {
		if rel[i] != w.evalDir[i] {
			return false
		}
	}
	return true
}

// mayLeadToEvalDir: a custom eval dir is only reached along its own path.
func (w *walker) mayLeadToEvalDir(p string) bool {
	if w.routeIndependent() {
		return true
	}
	rel := w.relSegs(p)
	if len(rel) > len(w.evalDir) {
		return false
	}
	for i := range rel {
		if rel[i] != w.evalDir[i] {
			return false
		}
	}
	return true
}

func (w *walker) walk(dir string, depth int, inSuite bool) {
	if depth > maxScanDepth {
		if inSuite {
			w.skipped = append(w.skipped, LoadError{File: dir, Error: fmt.Sprintf("nested more than %d directories below the target by this route — not scanned from here (move the suite higher, or target it directly)", maxScanDepth)})
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if depth == 0 || inSuite {
			w.skipped = append(w.skipped, LoadError{File: dir, Error: fmt.Sprintf("cannot be read (%v) — not scanned", err)})
		}
		return
	}
	if inSuite {
		for _, e := range entries {
			if !e.IsDir() && isCaseFileName(e.Name()) {
				if !w.found[dir] {
					w.found[dir] = true
					w.cases = append(w.cases, dir)
				}
				return
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		if strings.EqualFold(name, ".git") || skippedDirNames[name] {
			continue
		}
		p := filepath.Join(dir, name)
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			// A symlinked directory is followed only when it stays inside the
			// target and does not loop back above itself.
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				if inSuite {
					w.skipped = append(w.skipped, LoadError{File: p, Error: fmt.Sprintf("%s is a symbolic link whose target does not exist — skipped", name)})
				}
				continue
			}
			st, err := os.Stat(real)
			if err != nil || !st.IsDir() {
				continue
			}
			if real == dir || within(dir, real) || !within(real, w.target) {
				if inSuite {
					w.skipped = append(w.skipped, LoadError{File: p, Error: fmt.Sprintf("%s is a symbolic link to the directory under test, one above it, or outside it — not followed", name)})
				}
				continue
			}
			isDir = true
		}
		if !isDir {
			continue
		}
		childInSuite := inSuite || w.opensEvalDir(p)
		if !childInSuite && !w.mayLeadToEvalDir(p) {
			continue
		}
		if w.budget <= 0 {
			w.skipped = append(w.skipped, LoadError{File: p, Error: fmt.Sprintf("the tree under the target has more than %d directories to walk (links can inflate it) — the rest was not scanned", maxScanDirs)})
			return
		}
		w.budget--
		w.walk(p, depth+1, childInSuite)
	}
}

// within reports whether p is base or below it.
func within(p, base string) bool {
	if p == base {
		return true
	}
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func splitPath(p string) []string {
	return splitRel(filepath.ToSlash(filepath.Clean(p)))
}

func splitRel(p string) []string {
	var out []string
	for _, s := range strings.Split(filepath.ToSlash(p), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// EvalDirFromFlag validates --eval-dir: a relative path of at most 8
// segments and 200 characters that does not climb.
func EvalDirFromFlag(v string) ([]string, error) {
	n := strings.TrimSpace(v)
	if n == "" {
		return nil, errors.New("--eval-dir must not be empty")
	}
	r := strings.ReplaceAll(n, `\`, "/")
	if filepath.IsAbs(n) || strings.HasPrefix(r, "/") || regexp.MustCompile(`^[A-Za-z]:`).MatchString(n) {
		return nil, errors.New("--eval-dir must be a relative path (below the plugin)")
	}
	var segs []string
	for _, s := range strings.Split(r, "/") {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			return nil, errors.New("--eval-dir must not contain \"..\"")
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return nil, errors.New("--eval-dir must name a directory below the plugin")
	}
	if len(n) > 200 || len(segs) > 8 {
		return nil, errors.New("--eval-dir is too long (at most 8 segments, 200 characters)")
	}
	return segs, nil
}

// EvalDirFromManifest reads the manifest's experimental.evals (a string, or a
// list whose first entry is used); ok=false when it sets none. A top-level
// "evals" key is ignored with a warning.
func EvalDirFromManifest(manifest map[string]any) (segs []string, warning string, ok bool) {
	exp, _ := manifest["experimental"].(map[string]any)
	v, has := exp["evals"]
	if !has {
		if _, misplaced := manifest["evals"]; misplaced {
			return nil, `ignoring the top-level "evals" key (it belongs under "experimental"); using evals/`, false
		}
		return nil, "", false
	}
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case []any:
		if len(t) == 0 {
			return nil, "", false
		}
		s, _ = t[0].(string)
		if len(t) > 1 {
			warning = fmt.Sprintf(`plugin.json "evals" lists %d entries; using the first (%s) as the case directory`, len(t), s)
		}
	}
	segs, err := EvalDirFromFlag(s)
	if err != nil {
		return nil, fmt.Sprintf("ignoring experimental.evals %q (%s); using evals/", s, strings.TrimPrefix(err.Error(), "--eval-dir ")), false
	}
	return segs, warning, true
}
