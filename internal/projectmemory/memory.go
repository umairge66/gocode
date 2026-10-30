package projectmemory

import (
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// Kind labels where a memory source was discovered.
type Kind string

const (
	KindManaged Kind = "Managed" // <ManagedDir>/gocode.md (enterprise/managed)
	KindUser    Kind = "User"    // <ConfigDir>/gocode.md
	KindProject Kind = "Project" // <dir>/gocode.md and <dir>/.gocode/gocode.md
	KindLocal   Kind = "Local"   // <dir>/gocode.local.md (gitignored override)
	KindImport  Kind = "Import"  // pulled in via an @path import
)

// Defaults.
const (
	// DefaultMaxImportDepth is the recursion cap on @imports.
	DefaultMaxImportDepth = 5

	// DefaultMaxFileBytes is the per-file size cap. A file larger than this is
	// skipped with a note.
	DefaultMaxFileBytes int64 = 4 * 1024 * 1024

	// DefaultMaxTotalBytes bounds the total rendered block, a safety bound on
	// top of the per-file cap. Set Config.MaxTotalBytes negative to disable.
	DefaultMaxTotalBytes int64 = 256 * 1024
)

// Project memory filenames, relative to each walked directory.
const (
	projectFileName = "gocode.md"
	nestedFileName  = ".gocode/gocode.md"
	localFileName   = "gocode.local.md"

	// userFileName is the basename joined onto the resolved config home for the
	// user memory.
	userFileName = "gocode.md"

	// managedFileName is the basename joined onto an optional managed base dir.
	managedFileName = "gocode.md"
)

// Config is the input to Load. The caller resolves the directories; the loader
// itself is pure and does no environment lookups.
type Config struct {
	// Cwd is the working directory. The project walk starts here and climbs to
	// the filesystem root, so memory is discovered outermost-ancestor-first and
	// the current directory's files are rendered last (most specific wins by
	// appearing last). An empty Cwd disables the project walk.
	Cwd string

	// Home is the user's home directory, used to expand "~/" and "~" in @imports.
	// An empty Home leaves such imports unresolved (skipped with a note).
	Home string

	// ConfigDir is the resolved user config home (GOCODE_CONFIG_DIR /
	// XDG_CONFIG_HOME/gocode / HOME/.config/gocode — see
	// settingsload.Context.UserConfigDir). The user memory is
	// <ConfigDir>/gocode.md. An empty ConfigDir disables the user tier.
	ConfigDir string

	// ManagedDir is an optional managed/enterprise base directory. When set, its
	// <ManagedDir>/gocode.md is loaded first, as the Managed tier. Empty skips it.
	ManagedDir string

	// FS is the injected filesystem. When nil, fsfacade.OSFS{} is used.
	FS fsfacade.FS

	// MaxImportDepth overrides DefaultMaxImportDepth when > 0.
	MaxImportDepth int
	// MaxFileBytes overrides DefaultMaxFileBytes when > 0.
	MaxFileBytes int64
	// MaxTotalBytes overrides DefaultMaxTotalBytes when non-zero; a negative
	// value disables the total cap entirely.
	MaxTotalBytes int64
}

// Entry is one discovered memory source after reading and import resolution.
type Entry struct {
	Path    string // normalized absolute path (slash form)
	Kind    Kind   // where it was discovered
	Parent  string // importing file for an @import, "" for a directly-discovered file
	Content string // file body after frontmatter (never trimmed; empty files are not emitted)
	// Globs are the file's `paths:` frontmatter patterns, nil when it has none.
	// A rule file with globs is a conditional rule: it is not part of the
	// startup block and is only injected once a matching file is touched (see
	// Nested).
	Globs []string
}

// Result is the output of Load.
type Result struct {
	// Text is the rendered block ready for the system prompt, or "" when no
	// memory was found. Each source is wrapped as "Contents of <path>:\n\n<body>"
	// and blocks are separated by a blank line, in discovery order.
	Text string

	// Sources lists the files actually read, in render order, for diagnostics
	// and a /memory-style introspection view.
	Sources []string

	// Entries is the structured form of the rendered sources (same order).
	Entries []Entry

	// Notes records non-fatal events: a missing or oversize import, a cap hit,
	// a total-size truncation. They are diagnostics, not part of Text.
	Notes []string

	// Truncated is true when the total-size cap shortened Text.
	Truncated bool
}

// resolved config, with defaults applied.
type loader struct {
	fs           fsfacade.FS
	home         string
	cwd          string
	maxDepth     int
	maxFileBytes int64
	processed    map[string]bool // canonical paths already read (cycle + dup guard)
	seenContent  map[string]bool // trimmed contents already emitted (dedup)
	entries      []Entry
	notes        []string
}

// newLoader applies the Config defaults.
func newLoader(cfg Config) *loader {
	l := &loader{
		fs:           cfg.FS,
		home:         normalizePath(cfg.Home),
		cwd:          normalizePath(cfg.Cwd),
		maxDepth:     cfg.MaxImportDepth,
		maxFileBytes: cfg.MaxFileBytes,
		processed:    map[string]bool{},
		seenContent:  map[string]bool{},
	}
	if l.fs == nil {
		l.fs = fsfacade.OSFS{}
	}
	if l.maxDepth <= 0 {
		l.maxDepth = DefaultMaxImportDepth
	}
	if l.maxFileBytes <= 0 {
		l.maxFileBytes = DefaultMaxFileBytes
	}
	return l
}

// Load discovers and renders the gocode.md memory described by cfg.
//
// Discovery order:
//
//  1. Managed: <ManagedDir>/gocode.md, then the unconditional rules under
//     <ManagedDir>/.gocode/rules (when ManagedDir set).
//  2. User:    <ConfigDir>/gocode.md, then the unconditional rules under
//     <ConfigDir>/rules (when ConfigDir set).
//  3. Project: for each directory from the outermost ancestor of Cwd down to
//     Cwd itself: <dir>/gocode.md, <dir>/.gocode/gocode.md (Project), the
//     unconditional rules under <dir>/.gocode/rules (Project), then
//     <dir>/gocode.local.md (Local).
//
// Each file's @imports are resolved recursively and inserted immediately after
// the importing file. Files are deduped by path and by trimmed content; empty
// files are dropped. Conditional rules (a `paths:` frontmatter) are skipped
// here; Nested injects them when a matching file is touched.
func Load(cfg Config) Result {
	l := newLoader(cfg)

	// 1. Managed.
	if cfg.ManagedDir != "" {
		l.loadTop(join(cfg.ManagedDir, managedFileName), KindManaged)
		l.keep(l.loadRulesDir(managedRulesDir(cfg.ManagedDir), KindManaged, false))
	}

	// 2. User.
	if cfg.ConfigDir != "" {
		l.loadTop(join(cfg.ConfigDir, userFileName), KindUser)
		l.keep(l.loadRulesDir(userRulesDir(cfg.ConfigDir), KindUser, false))
	}

	// 3. Project walk: outermost ancestor first, cwd last.
	for _, dir := range ancestorsOutermostFirst(cfg.Cwd) {
		l.loadTop(join(dir, projectFileName), KindProject)
		l.loadTop(join(dir, nestedFileName), KindProject)
		l.keep(l.loadRulesDir(projectRulesDir(dir), KindProject, false))
		l.loadTop(join(dir, localFileName), KindLocal)
	}

	maxTotal := cfg.MaxTotalBytes
	if maxTotal == 0 {
		maxTotal = DefaultMaxTotalBytes
	}
	return l.render(maxTotal)
}

// loadTop loads a directly-discovered file (depth 0, no parent) and keeps it.
// A missing top-level file is silent (it simply does not exist); only imports
// note misses.
func (l *loader) loadTop(p string, kind Kind) {
	l.keep(l.collect(p, kind, "", 0, false))
}

// keep appends collected entries, dropping repeats by trimmed content.
func (l *loader) keep(es []Entry) {
	for _, e := range es {
		trimmed := strings.TrimSpace(e.Content)
		if l.seenContent[trimmed] {
			continue
		}
		l.seenContent[trimmed] = true
		l.entries = append(l.entries, e)
	}
}

// collect reads p and returns it followed by its resolved @imports (depth
// first), without keeping them.
func (l *loader) collect(p string, kind Kind, parent string, depth int, fromImport bool) []Entry {
	canon := normalizePath(p)
	if canon == "" {
		return nil
	}
	// Depth cap and cycle/dup guard.
	if depth >= l.maxDepth {
		if fromImport {
			l.notes = append(l.notes, "import skipped (max depth "+itoa(l.maxDepth)+" reached): "+canon)
		}
		return nil
	}
	if l.processed[canon] {
		return nil
	}
	if !l.mayRead(canon, kind, depth) {
		return nil
	}

	data, err := l.fs.ReadFile(canon)
	if err != nil {
		if fromImport {
			l.notes = append(l.notes, "import not found, skipped: "+canon)
		}
		return nil
	}
	// Per-file size cap.
	if int64(len(data)) > l.maxFileBytes {
		l.processed[canon] = true
		l.notes = append(l.notes, "file skipped (exceeds "+itoa64(l.maxFileBytes)+" byte limit): "+canon)
		return nil
	}
	l.processed[canon] = true

	content, globs := splitMemoryFrontmatter(string(data))
	// Empty files are not emitted.
	if strings.TrimSpace(content) == "" {
		return nil
	}

	// The importing file keeps its discovery kind for its parent; imported
	// files are labeled KindImport.
	entryKind := kind
	if fromImport {
		entryKind = KindImport
	}
	out := []Entry{{Path: canon, Kind: entryKind, Parent: parent, Content: content, Globs: globs}}

	// Recurse into @imports, resolved against this file's directory.
	for _, imp := range parseImports(content, dirOf(canon), l.home) {
		out = append(out, l.collect(imp, kind, canon, depth+1, true)...)
	}
	return out
}

// ancestorsOutermostFirst returns cwd and each of its ancestor directories up to
// (and including) the filesystem root, ordered outermost (root) first and cwd
// last. An empty cwd yields nil.
func ancestorsOutermostFirst(cwd string) []string {
	cwd = normalizePath(cwd)
	if cwd == "" {
		return nil
	}
	var chain []string
	for {
		chain = append([]string{cwd}, chain...) // unshift
		parent := dirOf(cwd)
		if parent == cwd || parent == "." {
			break // reached root (a drive root such as "C:" has the parent ".")
		}
		cwd = parent
	}
	return chain
}

// --- small path helpers (slash form, OS-independent for cross-compilation) ---

// normalizePath converts backslashes to slashes, trailing
// slashes stripped (but a bare root kept), then path.Clean.
func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean(p)
	return p
}

// dirOf returns the parent directory of a normalized path.
func dirOf(p string) string {
	return path.Dir(normalizePath(p))
}

// join joins a base directory and a relative sub-path in slash form.
func join(base, rel string) string {
	return normalizePath(base + "/" + rel)
}

// realpath resolves p through fsys and returns it in slash form. On Windows a
// filesystem that resolves paths natively is used directly, since drive-letter
// paths are not slash-rooted and cannot be walked component by component.
func realpath(fsys fsfacade.FS, p string) (string, error) {
	if runtime.GOOS == "windows" {
		if rp, ok := fsys.(fsfacade.Realpather); ok {
			r, err := rp.RealpathOS(filepath.FromSlash(p))
			if err != nil {
				return "", err
			}
			return normalizePath(r), nil
		}
	}
	r, err := fsfacade.Realpath(fsys, p)
	if err != nil {
		return "", err
	}
	return normalizePath(r), nil
}

func itoa(n int) string { return itoa64(int64(n)) }
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// mayRead applies the external-include and text-only gates before a memory
// file is read. A repository-controlled (Project/Local) memory file may not
// pull in anything outside the working directory: an @import whose path — or
// whose symlink-resolved target — leaves cwd is skipped (there is no
// approval flow for external includes), and an @import of a file with a
// non-text extension is skipped. Otherwise `@~/.ssh/id_rsa` or
// `@../../.aws/credentials` in a cloned repo's gocode.md would be read into
// the system prompt. User and Managed memory are the user's/admin's own and
// keep following external imports.
//
// Additionally, a top-level Project/Local file
// discovered in or below cwd that is a symlink resolving outside cwd is also
// skipped (git preserves symlinks, so `gocode.md -> ../../.ssh/id_rsa` is
// clonable).
func (l *loader) mayRead(canon string, kind Kind, depth int) bool {
	if kind == KindUser || kind == KindManaged {
		return true
	}
	if depth > 0 {
		if ext := strings.ToLower(path.Ext(canon)); ext != "" && !textImportExts[ext] {
			l.notes = append(l.notes, "import skipped (not a text file): "+canon)
			return false
		}
		if !within(canon, l.cwd) {
			l.notes = append(l.notes, "import skipped (outside the working directory): "+canon)
			return false
		}
	} else if !within(dirOf(canon), l.cwd) {
		// An ancestor directory's own file: not repository-placed.
		return true
	}
	if r, err := realpath(l.fs, canon); err == nil {
		real := normalizePath(r)
		cwdReal := l.cwd
		if cr, err := realpath(l.fs, l.cwd); err == nil {
			cwdReal = normalizePath(cr)
		}
		if real != canon && !within(real, cwdReal) && !within(real, l.cwd) {
			l.notes = append(l.notes, "memory file skipped (links outside the working directory): "+canon)
			return false
		}
	}
	return true
}

// textImportExts is the allowlist of @include extensions. Matching is on the lower-cased extension; an extensionless file is
// allowed.
var textImportExts = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(`.md .txt .text .json .yaml .yml .toml .xml .csv .html .htm .css .scss .sass .less .js .ts .tsx .jsx .mjs .cjs .mts .cts .py .pyi .pyw .rb .erb .rake .go .rs .java .kt .kts .scala .c .cpp .cc .cxx .h .hpp .hxx .cs .swift .sh .bash .zsh .fish .ps1 .bat .cmd .env .ini .cfg .conf .config .properties .sql .graphql .gql .proto .vue .svelte .astro .ejs .hbs .pug .jade .php .pl .pm .lua .r .dart .ex .exs .erl .hrl .clj .cljs .cljc .edn .hs .lhs .elm .ml .mli .f .f90 .f95 .for .cmake .make .makefile .gradle .sbt .rst .adoc .asciidoc .org .tex .latex .lock .log .diff .patch`) {
		m[e] = true
	}
	return m
}()
