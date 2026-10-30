// Package configimport implements gocode `import` subcommand: the
// logic that brings settings, MCP servers, slash commands, rules/memory and
// skills in from OTHER AI coding tools (OpenAI Codex, Google Gemini CLI and
// Cursor) and maps them into Gocode's own on-disk shapes.
//
// Structure:
//
//   - three per-source importers (codex, gemini, cursor), each with an id,
//     display name, Detect and Scan, collected in a registry;
//   - Scan returns the mappable items (each with id/kind/scope/label/
//     description/fingerprint/warning and an apply plan) and the unmappable
//     things the importer refuses to map automatically, each {scope, label,
//     reason};
//   - RunImport resolves scope, picks sources (an explicit `from` id or
//     everything detected), scans each, and filters to user-scope when the
//     launch's project config root is redirected. No analytics are emitted
//     (gocode collects no telemetry);
//   - Detect returns the importers whose detection succeeds; Get looks one up
//     by id;
//   - the "import-to-gocode" fallback SKILL.md lists the unmappable items for
//     manual follow-up (see fallback.go);
//   - untrusted labels are truncated to 120/500 display chars.
//
// LAYERS. `Import` (scan) is read-only: it returns the mapped ImportResult with
// each item's TargetPath / rendered Content / MCP / Setting payload. Writing is
// a separate, explicit step — Apply (apply.go) performs each item's write:
// the disk-level dedup ("already exists" skips, the
// `<!-- imported-from: <id> -->` marker that makes memory appends idempotent,
// MCP "already exists in … config" checks), the project-scope symlink guards,
// and the dry-run messages. WriteFallbackSkill (fallback.go) writes the
// fallback skill. The
// in-scan conflict handling (colliding target names) decides what maps vs.
// what becomes unmappable.
//
// Nothing here executes anything taken from a foreign config: MCP server
// commands are only written into config files (never spawned), skill
// directories are copied as files, and shell-exec markers are refused or
// escaped (shellexec.go).
package configimport

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/mcp"
)

// Source identifies a foreign AI coding tool that can be imported from. These
// are the `id` strings of the three registered importers.
type Source string

const (
	SourceCodex  Source = "codex"
	SourceGemini Source = "gemini"
	SourceCursor Source = "cursor"
)

// Scope is the config tier an imported item belongs to.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// ItemKind is the kind of Gocode artifact a mappable item becomes.
type ItemKind string

const (
	KindMCP          ItemKind = "mcp"          // → an mcpServers entry
	KindSetting      ItemKind = "setting"      // → a settings.json patch (e.g. permissions.defaultMode)
	KindSubagent     ItemKind = "subagent"     // → <gocode root>/agents/<name>.md
	KindSkill        ItemKind = "skill"        // → <gocode root>/skills/<name>/SKILL.md (+ dir copy)
	KindInstructions ItemKind = "instructions" // → appended to gocode.md (memory file)
	KindCommand      ItemKind = "command"      // → <gocode root>/commands/<name>.md
)

// SettingPatch is the Gocode settings fragment a KindSetting item merges into
// the user (<ConfigDir>/settings.json) or project (.gocode/settings.json)
// settings file. Only the fields the importer actually produces are modeled;
// Apply deep-merges them, preserving every other key.
type SettingPatch struct {
	// PermissionsDefaultMode is permissions.defaultMode: one of "default",
	// "acceptEdits", "auto". Empty if the patch does not touch it.
	PermissionsDefaultMode string
}

// ImportItem is one mappable thing found in a foreign config, already
// mapped to its Gocode shape. The mapped payload is exposed (MCP / Setting /
// Content + TargetPath) and the unexported apply* fields carry what Apply
// needs to perform the write.
type ImportItem struct {
	// ID is the stable item identifier, "<source>:<scope>:<kind>:<name>".
	ID string
	// Kind is what this becomes in Gocode.
	Kind ItemKind
	// Scope is user or project.
	Scope Scope
	// Label is a human-readable description of the source item (untrusted —
	// copied from the foreign config).
	Label string
	// Description is an optional short detail (source url/command, target path…).
	Description string
	// Fingerprint identifies the item's content for dedup across re-runs: two
	// items with the same fingerprint import to the same result.
	Fingerprint string
	// Warning, when non-empty, is a caution shown before applying (such items
	// are unchecked by default in the interactive picker).
	Warning string

	// TargetPath is where this would be written in the Gocode layout. For
	// KindInstructions it is the memory file the body is appended to.
	TargetPath string
	// Content is the fully-rendered file body (with frontmatter) for command /
	// subagent / skill(SKILL.md) / scoped-rule items, or the block appended for
	// instructions. Empty for MCP and Setting items.
	Content string
	// SourcePath is the originating file or directory on disk.
	SourcePath string

	// MCP is the mapped server config for KindMCP items.
	MCP *mcp.ServerConfig
	// MCPTargetScope is "user" or "project": user MCP goes to the Gocode user
	// MCP config, project MCP to <cwd>/.mcp.json.
	MCPTargetScope Scope
	// Setting is the mapped settings patch for KindSetting items.
	Setting *SettingPatch

	// AppendInstruction is true for KindInstructions items that append to an
	// existing memory file rather than creating a new file.
	AppendInstruction bool

	// --- apply plan (unexported; see apply.go) ---

	// applyKind selects which apply strategy this item uses.
	applyKind applyKind
	// containIn is the project root for project-scope items: every write (and
	// the source read for appends) must stay inside it without crossing a
	// symlink. Empty for user scope.
	containIn string
	// existsPath is the path whose existence means "already imported" when it
	// differs from TargetPath.
	existsPath string
	// msgName is the name used in skip messages (the sanitized item name, or
	// the source basename for copied commands).
	msgName string
	// msgSuffix is appended to a successful apply message (Cursor MCP env-var
	// notes, escaped shell-exec notes, dropped frontmatter keys).
	msgSuffix string
	// marker is the `<!-- imported-from: … -->` id for memory appends.
	marker string
	// skillRoot / skillPath locate a skill directory to copy: the directory is
	// skillPath resolved against skillRoot, contained.
	skillRoot, skillPath string
	// skillDir is the destination skill directory for directory copies.
	skillDir string
	// prepareSkill renders the destination SKILL.md from the source file read
	// at apply time, or returns a skip reason.
	prepareSkill func(raw string) (out string, skip string)
}

// UnmappableItem is something the importer deliberately did not map: it records
// why, so the fallback SKILL.md (and the picker UI) can tell the user what to
// do by hand.
type UnmappableItem struct {
	Scope  Scope
	Label  string
	Reason string
}

// ImportResult is one source's scan output.
type ImportResult struct {
	SourceID    Source
	DisplayName string
	Items       []ImportItem
	Unmappable  []UnmappableItem
}

// Options parametrizes detection and import.
type Options struct {
	// Cwd is the project root. Empty → os.Getwd().
	Cwd string
	// HomeDir overrides the SOURCE tool's config directory (e.g. a test's
	// .codex / .cursor / .gemini). Empty → <UserHome>/.<tool>.
	HomeDir string
	// UserHome overrides the OS home directory. Empty → os.UserHomeDir().
	UserHome string
	// GocodeDir is the write root for user-scope skills, commands, subagents
	// and rules — where the skills / agents / commands loaders read them.
	// Empty → ConfigDir.
	GocodeDir string
	// ConfigDir is the gocode root (internal/confighome: GOCODE_CONFIG_DIR /
	// XDG_CONFIG_HOME/gocode / ~/.config/gocode): user settings.json
	// (permission mode, user-scope mcpServers) and the user gocode.md memory
	// live here, which is where the settings loader and internal/projectmemory
	// read them. Empty → <UserHome>/.config/gocode.
	ConfigDir string
	// ConfigDirOverridden reports that GOCODE_CONFIG_DIR is set. With it set, a
	// config dir that is a network (UNC) path or resolves inside the project makes the
	// user-scope scan and the fallback-skill write refuse.
	ConfigDirOverridden bool
	// RealHome resolves the account's home directory from the user database
	// for the redirected-home guard ($HOME differs from the real home → user
	// scope is skipped). nil: when UserHome is empty the os/user lookup is
	// used; when UserHome is set explicitly the caller vouches for it and the guard is off.
	RealHome func() (string, bool)
	// ProjectConfigRedirected forces user scope only (the launch replaced the
	// project config root, so project-scope items are filtered out).
	ProjectConfigRedirected bool
	// Scope restricts the scan to one tier. Empty → both user and project.
	Scope Scope
}

func (o Options) cwd() string {
	if o.Cwd != "" {
		return o.Cwd
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func (o Options) userHome() string {
	if o.UserHome != "" {
		return o.UserHome
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func (o Options) gocodeDir() string {
	if o.GocodeDir != "" {
		return o.GocodeDir
	}
	return o.configDir()
}

func (o Options) configDir() string {
	if o.ConfigDir != "" {
		return o.ConfigDir
	}
	dir, _ := confighome.Resolve(nil, o.userHome())
	return dir
}

// homeRedirected reports true when the real account home cannot be resolved
// or differs from the home directory in use ($HOME redirected).
func (o Options) homeRedirected() bool {
	real := o.RealHome
	if real == nil {
		if o.UserHome != "" {
			return false
		}
		real = accountHomeDir
	}
	h, ok := real()
	if !ok || h == "" {
		return true
	}
	home := o.userHome()
	if home == "" {
		return true
	}
	return filepath.Clean(home) != filepath.Clean(h)
}

// accountHomeDir looks the current account's home up in the user database.
func accountHomeDir() (string, bool) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return "", false
	}
	return u.HomeDir, true
}

// detectRedirected is the detect-time short-circuit: with the default source
// home and a redirected $HOME every importer reports "detected", so the scan
// can tell the user why its user scope was skipped.
func (o Options) detectRedirected() bool {
	return o.HomeDir == "" && o.homeRedirected()
}

// userScopeRedirected is the scan-time guard: the user-scope read or write
// root has been redirected (a redirected $HOME, or a GOCODE_CONFIG_DIR that is
// a network path or resolves inside the project), so the user-scope scan is
// skipped for safety.
func (o Options) userScopeRedirected() bool {
	if o.detectRedirected() {
		return true
	}
	return o.ConfigDirOverridden && (isNetworkPath(o.configDir()) || pathWithin(o.configDir(), o.cwd()))
}

// redirectedReason is the unmappable reason recorded when userScopeRedirected.
const redirectedReason = "The user-scope read or write root has been redirected (resolves inside this project, to a network path, or away from the real home directory) — skipping user-scope scan for safety."

// sourceHome returns the SOURCE tool's config directory: opts.HomeDir if set,
// else <UserHome>/<dir>.
func (o Options) sourceHome(dir string) string {
	if o.HomeDir != "" {
		return o.HomeDir
	}
	return filepath.Join(o.userHome(), dir)
}

// scopeAllows reports whether the requested scope lets this tier run.
func (o Options) scopeAllows(s Scope) bool {
	return o.Scope == "" || o.Scope == s
}

// Importer is one foreign-tool importer.
type Importer interface {
	ID() Source
	DisplayName() string
	// Detect reports whether this tool's config is present for opts.
	Detect(opts Options) (bool, error)
	// Import scans the tool's config and maps it to Gocode shapes. It is pure:
	// it reads but never writes.
	Import(opts Options) (ImportResult, error)
}

// registry is the ordered set of importers: codex, gemini, cursor.
var registry = []Importer{codexImporter{}, geminiImporter{}, cursorImporter{}}

// Get returns the importer with the given id, or nil.
func Get(id Source) Importer {
	for _, im := range registry {
		if im.ID() == id {
			return im
		}
	}
	return nil
}

// Detect returns every importer whose config is present for opts.
func Detect(opts Options) ([]Importer, error) {
	var found []Importer
	for _, im := range registry {
		ok, err := im.Detect(opts)
		if err != nil {
			return found, err
		}
		if ok {
			found = append(found, im)
		}
	}
	return found, nil
}

// RunImport is the top-level orchestration: pick sources (an explicit
// `from` id, else everything Detect finds), scan each, and collect the results
// that produced any items or unmappable notes.
//
// A scan error becomes a "Couldn't read … Skipping." warning, not a hard
// failure. When ProjectConfigRedirected is set the scope is forced to user and
// project items are filtered out. No analytics event is emitted: no telemetry.
func RunImport(from Source, opts Options) (RunResult, error) {
	if opts.ProjectConfigRedirected {
		opts.Scope = ScopeUser
	}
	var sources []Importer
	if from != "" {
		if im := Get(from); im != nil {
			sources = []Importer{im}
		}
		if len(sources) == 0 {
			ids := make([]string, len(registry))
			for i, im := range registry {
				ids[i] = string(im.ID())
			}
			return RunResult{}, &UnknownSourceError{From: from, Known: ids}
		}
	} else {
		var err error
		sources, err = Detect(opts)
		if err != nil {
			return RunResult{}, err
		}
	}

	var res RunResult
	if len(sources) == 0 {
		names := make([]string, len(registry))
		for i, im := range registry {
			names[i] = im.DisplayName()
		}
		res.NoneDetected = true
		res.DetectedNames = names
		return res, nil
	}

	for _, im := range sources {
		r, err := im.Import(opts)
		if err != nil {
			res.Warnings = append(res.Warnings,
				"Couldn't read "+im.DisplayName()+" config — it may be malformed. Skipping.")
			continue
		}
		if opts.ProjectConfigRedirected || opts.Scope == ScopeUser {
			r.Items = filterScope(r.Items, ScopeUser)
		}
		if len(r.Items) > 0 || len(r.Unmappable) > 0 {
			res.Scans = append(res.Scans, r)
		}
	}
	return res, nil
}

// RunResult is RunImport's output.
type RunResult struct {
	Scans    []ImportResult
	Warnings []string
	// NoneDetected is set when no `from` was given and nothing was detected.
	NoneDetected  bool
	DetectedNames []string
}

// UnknownSourceError is returned when `from` names no registered importer.
type UnknownSourceError struct {
	From  Source
	Known []string
}

func (e *UnknownSourceError) Error() string {
	return "Unknown source \"" + string(e.From) + "\". Known: " + strings.Join(e.Known, ", ")
}

func filterScope(items []ImportItem, s Scope) []ImportItem {
	out := items[:0:0]
	for _, it := range items {
		if it.Scope == s {
			out = append(out, it)
		}
	}
	return out
}

// --- shared mapping helpers -------------------------------------------------

// SkillMaxBytes is the size ceiling Gocode loads for a skill / rule / command
// file; the importer refuses anything larger.
const SkillMaxBytes = 1_000_000

// ImportMaxFileBytes caps a single file the importer will read.
const ImportMaxFileBytes = 10 * 1024 * 1024

var (
	reNonSafe        = regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	reTripleHyphen   = regexp.MustCompile(`-{3,}`)
	reLeadingHyphens = regexp.MustCompile(`^-+`)
	reReservedDevice = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9\x{00B9}\x{00B2}\x{00B3}]|lpt[0-9\x{00B9}\x{00B2}\x{00B3}])$`)
	reControl        = regexp.MustCompile(`[\p{C}\x{2028}\x{2029}]`)
)

// safeName turns an arbitrary name into a filesystem-safe identifier.
func safeName(n string) string {
	e := reNonSafe.ReplaceAllString(n, "_")
	e = reTripleHyphen.ReplaceAllStringFunc(e, func(m string) string {
		return strings.Repeat("_", len(m))
	})
	e = reLeadingHyphens.ReplaceAllString(e, "_")
	if e == "" {
		e = "_"
	}
	if isReservedDeviceName(e) {
		return "_" + e
	}
	return e
}

// isReservedDeviceName reports whether the first dot-segment of n (trailing
// spaces trimmed) is a Windows reserved device name.
func isReservedDeviceName(n string) bool {
	seg := n
	if i := strings.IndexByte(n, '.'); i != -1 {
		seg = n[:i]
	}
	seg = strings.TrimRight(seg, " ")
	return reReservedDevice.MatchString(seg)
}

// normalizeDirName is the NFKC case/encoding fold used to compare directory
// names (e.g. to spot "skill.md" variants, plugin-shaped subdirectories, or the
// reserved skills-sync root).
func normalizeDirName(n string) string {
	n = strings.ToLower(norm.NFKC.String(n))
	return strings.TrimRight(n, ". ")
}

// reservedSkillsSyncRoot is the directory name Gocode reserves for synced
// skills; a skill imported under this name would never load.
const reservedSkillsSyncRoot = "synced"

func isReservedSkillsDir(name string) bool {
	return normalizeDirName(name) == reservedSkillsSyncRoot
}

// emDash replaces any run of 3+ hyphens with an em dash, so an imported
// value can't accidentally open a YAML frontmatter fence.
func emDash(n string) string {
	return reTripleHyphen.ReplaceAllString(n, "—")
}

// truncateLabel / truncateDesc clamp an untrusted label for display, collapsing
// control characters to spaces first.
func truncateLabel(n string) string { return truncate(n, 120, 117) }
func truncateDesc(n string) string  { return truncate(n, 500, 497) }

func truncate(n string, max, keep int) string {
	e := strings.TrimSpace(reControl.ReplaceAllString(n, " "))
	r := []rune(e)
	if len(r) > max {
		return string(r[:keep]) + "…"
	}
	return e
}

// TruncateLabel and TruncateDesc expose the label clamps for callers that format scan
// results.
func TruncateLabel(n string) string { return truncateLabel(n) }
func TruncateDesc(n string) string  { return truncateDesc(n) }

// fileExists reports whether path exists (via lstat; symlinks count).
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// readFileLimited reads a UTF-8 file, refusing anything over ImportMaxFileBytes.
func readFileLimited(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Size() > ImportMaxFileBytes {
		return "", errFileTooLarge
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

var errFileTooLarge = &fileTooLargeError{}

type fileTooLargeError struct{}

func (*fileTooLargeError) Error() string {
	return "file exceeds IMPORT_MAX_FILE_BYTES; refusing to load"
}

// collisionSet tracks target paths already claimed within one scan, so two
// rules/skills/commands mapping to the same file become a conflict instead of
// silently overwriting one another.
type collisionSet map[string]struct{}

func newCollisionSet() collisionSet { return collisionSet{} }

func (c collisionSet) claim(target string) bool {
	if _, ok := c[target]; ok {
		return false
	}
	c[target] = struct{}{}
	return true
}

// collisionReason is the unmappable reason used when a safe name collides.
const collisionReason = "Its filesystem-safe name collides with another imported rule, skill or command that targets the same path. Rename one of them and re-run the import."
