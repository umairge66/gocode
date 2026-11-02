package pathguard

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/pathclass"
)

// This file holds the session-specific parts of the file-write permission
// pipeline: the internal hard-deny / allowlist, the write-safety
// check for gocode settings files and sensitive files, and the
// working-directory containment test the acceptEdits fast path
// uses. They depend on session state (config dir, cwd, settings
// file locations, agent-memory roots, plan slug), which SessionFiles carries
// as plain fields instead of reaching through global singletons.
//
// Where they take effect: toolreg.Decider consults SessionFiles (its Files
// field) for every edit-tool call (Write, Edit, MultiEdit, NotebookEdit) in
// the order the file-edit tools' permission check applies them.
// RuleSetEngine is not on the live tool path.

// FileBehavior is the verdict of an internal file check
// (allow / deny / passthrough).
type FileBehavior string

const (
	FilePassthrough FileBehavior = "passthrough"
	FileAllow       FileBehavior = "allow"
	FileDeny        FileBehavior = "deny"
)

// FileVerdict is the internal file check's result. Message is the user-facing text (the deny
// message, or the allow decisionReason.reason).
type FileVerdict struct {
	Behavior FileBehavior
	Message  string
	// Reason is the decisionReason.reason of a deny (a short internal label).
	Reason string
	// CircuitBreaker names the breaker a deny carries, if any.
	CircuitBreaker string
}

// WriteSafety is the write-safety check's result.
type WriteSafety struct {
	Safe    bool
	Message string
	// ClassifierApprovable is informational only: there is no
	// classifier.
	ClassifierApprovable bool
	// CircuitBreaker is "suspiciousWindowsPath" or "gocodeSettingsFile" when
	// set; Also carries the secondary breakers.
	CircuitBreaker string
	Also           []string
}

// Circuit breakers named by the session-file checks. Neither is
// bypassImmune, so bypassPermissions mode still allows a write they would
// hold for approval.
const (
	CircuitBreakerSettingsFile = "gocodeSettingsFile"
)

// SessionFiles carries the session state the internal-write, write-safety and
// containment checks consult. A
// zero field disables the check that needs it (it never widens anything): an
// empty PlanSlug allows no plan file, an empty ConfigDir derives no
// agent-memory or seed-admin root, and so on.
type SessionFiles struct {
	// ConfigDir is the gocode root (internal/confighome: $GOCODE_CONFIG_DIR,
	// else $XDG_CONFIG_HOME/gocode, else ~/.config/gocode).
	ConfigDir string
	// HomeDir is the user's home directory.
	HomeDir string
	// OriginalCwd is the session's launch directory.
	OriginalCwd string
	// Cwd is the current working directory; defaults to
	// OriginalCwd.
	Cwd string
	// ProjectConfigRoot is the --project-config-root override, if any.
	ProjectConfigRoot string
	// AdditionalWorkingDirs are the /add-dir directories. Together with
	// OriginalCwd they are the working directories writes are confined to.
	AdditionalWorkingDirs []string

	// SettingsFiles are the paths of every settings source (user, project,
	// local, flag, policy).
	SettingsFiles []string
	// ManagedDropInDirs are the managed-settings.d directories.
	ManagedDropInDirs []string

	// PlansDir and PlanSlug locate the current session's plan file. Plan
	// slugs are not minted yet; empty disables the plan-file allowlist.
	PlansDir string
	PlanSlug string
	// ScratchpadDir is the session scratchpad.
	ScratchpadDir string
	// WorkflowScriptsDir is the session's workflow-script directory, with or
	// without a trailing separator.
	WorkflowScriptsDir string
	// AutoMemoryDir is the auto-memory directory; it must end in a
	// separator to count. When AutoMemoryDirOverridden is set, the
	// overridden directory gets no allowlist entry.
	AutoMemoryDir           string
	AutoMemoryDirOverridden bool
	// RemoteMemoryDir is $GOCODE_CODE_REMOTE_MEMORY_DIR.
	RemoteMemoryDir string
	// HostCredsFile is $GOCODE_CODE_HOST_CREDS_FILE.
	HostCredsFile string
	// BackgroundJobDir is $GOCODE_JOB_DIR when this is a background session
	// ($GOCODE_CODE_SESSION_KIND=bg); empty otherwise.
	BackgroundJobDir string

	// Restricted is the --restricted session flag.
	Restricted bool
	// BlockReadsOutsideWorkingDirectories is the
	// permissions.blockReadsOutsideWorkingDirectories setting: Read, Glob and
	// Grep are refused for a path with any spelling outside WorkingDirs, in
	// every permission mode (circuit breaker "outsideReadsBlocked").
	BlockReadsOutsideWorkingDirectories bool
	// FlagSettingsDir is the directory of the --settings file, the root of a
	// "/x" path rule from flagSettings. Empty uses the cwd.
	FlagSettingsDir string
	// Diskless reports a session with no config folder: every
	// write is denied.
	Diskless bool
	// Platform is the host platform ("windows", "wsl", "linux",
	// "darwin"); "" means a POSIX host.
	Platform string
	// TrustedNetworkDirectories are network-share roots under which a UNC
	// spelling is not suspicious.
	TrustedNetworkDirectories []string

	// FS is consulted for realpath/lstat; nil means the real filesystem.
	FS fsfacade.FS

	// ProbeTimeout bounds each filesystem probe of the permission walk (zero:
	// fsfacade.DefaultProbeTimeout), so a stalled network or automount mount
	// cannot hang a permission check; a timed-out probe leaves the path
	// unresolved, which refuses it.
	ProbeTimeout time.Duration
	// WalkBudget bounds a whole permission walk (zero:
	// fsfacade.DefaultWalkBudget).
	WalkBudget time.Duration
	// Stalls records probes still hung on a dead mount; nil uses the
	// process-wide set (fsfacade.StallSet).
	Stalls *fsfacade.StallSet
	// NoLaunchAncestry drops the launch-directory ancestry an EPERM/EACCES
	// ancestor may be crossed under (set in a background session,
	// gocode_CODE_SESSION_KIND=bg).
	NoLaunchAncestry bool
}

func (s *SessionFiles) fsys() fsfacade.FS {
	if s.FS != nil {
		return s.FS
	}
	return fsfacade.OSFS{}
}

func (s *SessionFiles) cwd() string {
	if s.Cwd != "" {
		return s.Cwd
	}
	return s.OriginalCwd
}

// resolveAgainst is path.resolve: a relative path is joined to the
// original cwd, then cleaned.
func (s *SessionFiles) resolve(p string) string {
	if p == "" {
		return p
	}
	if !filepath.IsAbs(p) && s.OriginalCwd != "" {
		p = filepath.Join(s.OriginalCwd, p)
	}
	return filepath.Clean(p)
}

// realpath returns the canonical path of p, or "" when it cannot be resolved
// (or the probe timed out on a stalled mount).
func (s *SessionFiles) realpath(p string) string {
	var name string
	if s.FS == nil {
		name = p
	} else {
		name = filepath.ToSlash(p)
	}
	r, err := fsfacade.RealpathVia(s.probeFS(), name)
	if err != nil {
		return ""
	}
	return filepath.Clean(filepath.FromSlash(r))
}

// spellingsOf is the spellings of the permission walk:
// the requested spelling, the targets of a leaf symlink, the landing (a
// missing leaf lands beneath its deepest existing ancestor) and the canonical
// landing — and the network spelling a link leads onto, which the walk reports
// without touching. See ResolvePath.
func (s *SessionFiles) spellingsOf(p string) []string {
	return s.ResolvePath(p).Spellings
}

// Spellings returns the spellings the write checks judge for a requested path:
// the cleaned absolute path first and the symlink-resolved landing last when
// it differs.
func (s *SessionFiles) Spellings(p string) []string { return s.spellingsOf(p) }

// --- folding helpers --------------------------------------------------------

// bidiAndZeroWidth is the class of bidi and zero-width characters
// stripped before a segment is compared.
var bidiAndZeroWidth = regexp.MustCompile("[\u200c-\u200f\u202a-\u202e\u206a-\u206f\ufeff]")

var trailingDotsSpaces = regexp.MustCompile(`[. ]+$`)

// foldSegment lower-cases (with the ı/ſ folds), drop
// zero-width and bidi characters, cut an NTFS alternate-data-stream suffix
// (":…") and trailing dots/spaces; an empty result falls back to the
// lower-cased input.
func foldSegment(e string) string {
	r := foldCase(e)
	t := bidiAndZeroWidth.ReplaceAllString(r, "")
	if i := strings.IndexByte(t, ':'); i >= 0 {
		t = t[:i]
	}
	t = trailingDotsSpaces.ReplaceAllString(t, "")
	if t == "" {
		return r
	}
	return t
}

const sep = string(filepath.Separator)

// foldPath normalizes, then folds every segment with foldSegment.
func foldPath(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(filepath.Clean(p), sep)
	for i, sg := range segs {
		segs[i] = foldSegment(sg)
	}
	return strings.Join(segs, sep)
}

// excludedSegments are directory/file names that disqualify a path from
// an allowlist when they appear beneath the allowlisted root.
var excludedSegments = map[string]bool{
	".git": true, "hooks": true, ".husky": true, ".githooks": true, "node_modules": true,
	".vscode": true, ".idea": true, "head": true, "config": true, "objects": true,
	"refs": true, ".gocode": true, "skills": true, "commands": true, "agents": true,
	".cargo": true, ".devcontainer": true, ".yarn": true, ".mvn": true,
}

// sensitiveFileNames are dotfiles and tool configs that execute code or
// steer tooling, with a lower-cased set alongside.
var sensitiveFileNames = []string{
	".gitconfig", ".gitmodules", ".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".profile",
	".zshenv", ".zlogin", ".zlogout", ".bash_login", ".bash_aliases", ".bash_logout", ".envrc",
	".ripgreprc", ".mcp.json", ".gocode.json", ".npmrc", ".yarnrc", ".yarnrc.yml", ".pnp.cjs",
	".pnp.loader.mjs", ".pnpmfile.cjs", "bunfig.toml", ".bunfig.toml", ".bazelrc", ".bazelversion",
	".bazeliskrc", ".pre-commit-config.yaml", "lefthook.yml", ".lefthook.yml", "lefthook.yaml",
	".lefthook.yaml", "gradle-wrapper.properties", "maven-wrapper.properties", ".devcontainer.json",
	"pyrightconfig.json",
}

var sensitiveFileSet = func() map[string]bool {
	m := make(map[string]bool, len(sensitiveFileNames))
	for _, n := range sensitiveFileNames {
		m[strings.ToLower(n)] = true
	}
	return m
}()

// sensitiveDirs are sensitive directory names; sensitiveDirRuns are
// sensitive multi-segment runs.
var (
	sensitiveDirs    = []string{".git", ".vscode", ".idea", ".gocode", ".husky", ".cargo", ".devcontainer", ".yarn", ".mvn"}
	sensitiveDirRuns = []string{".config/git"}
)

var segSplit = regexp.MustCompile(`[\\/]`)

// hasExcludedSegment reports whether any segment of e after the prefix
// n folds to an excluded name, or (when last is non-nil) the last segment is
// in last.
func hasExcludedSegment(e, prefix string, last map[string]bool) bool {
	if len(prefix) > len(e) {
		return false
	}
	segs := segSplit.Split(e[len(prefix):], -1)
	for i, sg := range segs {
		f := foldSegment(sg)
		if excludedSegments[f] {
			return true
		}
		if i == len(segs)-1 && last != nil && last[f] {
			return true
		}
	}
	return false
}

// EscapeControl escapes control characters (exported for the permission
// messages built outside this package).
func EscapeControl(e string) string { return escapeControl(e) }

// escapeControl: control and line-separator characters become HTML
// numeric references, so a path cannot forge message lines.
func escapeControl(e string) string {
	var b strings.Builder
	for _, r := range e {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			b.WriteString("&#" + strconv.Itoa(int(r)) + ";")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --- internal hard-deny and allowlist ---------------------------------------

// Verbatim internal-check verdict messages.
const (
	MsgDisklessWriteDenied   = "This session has no config folder to check the path against, so the write is denied. A retry is denied the same way."
	MsgHostCredsDenied       = "The host credentials file is managed by the host process; it cannot be written directly"
	MsgSeedAdminDenied       = "seed-admin in the gocode config directory holds the private git directories of cloud-session uploads and is managed by gocode; it cannot be written directly"
	MsgSettingsReviewDenied  = "Staged gocode settings changes are the owner\u2019s to review in /settings-review; the review store cannot be written directly"
	MsgAdoptJSONDenied       = "adopt.json is the bg-fork handoff carrier and is managed by the harness; it cannot be written directly"
	MsgPlanFileAllowed       = "Plan files for current session are allowed for writing"
	MsgPlanFileReadable      = "Plan files for current session are allowed for reading"
	MsgScratchpadReadable    = "Scratchpad files for current session are allowed for reading"
	MsgWorkflowScriptAllowed = "Workflow script files for current session are allowed for writing"
	MsgScratchpadAllowed     = "Scratchpad files for current session are allowed for writing"
	MsgJobTmpAllowed         = "Job tmp/ subtree for current bg session is allowed for writing"
	MsgAgentMemoryAllowed    = "Agent memory files are allowed for writing"
	MsgAutoMemoryAllowed     = "auto memory files are allowed for writing"
	MsgLaunchConfigAllowed   = "Preview launch config is allowed for writing"
)

var passthrough = FileVerdict{Behavior: FilePassthrough}

func denyVerdict(msg, reason, cb string) FileVerdict {
	return FileVerdict{Behavior: FileDeny, Message: msg, Reason: reason, CircuitBreaker: cb}
}

func allowVerdict(msg string) FileVerdict {
	return FileVerdict{Behavior: FileAllow, Message: msg}
}

var (
	verdictDiskless       = denyVerdict(MsgDisklessWriteDenied, "no config folder", "")
	verdictHostCreds      = denyVerdict(MsgHostCredsDenied, "host-creds file rewrite redirects the bearer token", "")
	verdictSeedAdmin      = denyVerdict(MsgSeedAdminDenied, "seed-admin git configuration is a code-execution surface for the upload", "")
	verdictSettingsReview = denyVerdict(MsgSettingsReviewDenied, "settings review store write substitutes a proposal the owner is about to accept", CircuitBreakerSettingsFile)
	verdictAdoptJSON      = denyVerdict(MsgAdoptJSONDenied, "adopt.json is a code-execution surface for the fork", "")
)

// msysDrivePath matches the MSYS/Cygwin drive spelling converted on Windows.
var msysDrivePath = regexp.MustCompile(`^/(?:cygdrive/)?[A-Za-z]/`)

// toWindowsSpelling: on Windows an MSYS "/c/x" or "/cygdrive/c/x"
// spelling becomes "C:\x"; elsewhere the path is unchanged.
func (s *SessionFiles) toWindowsSpelling(e string) string {
	if s.platform() != "windows" || !msysDrivePath.MatchString(e) {
		return e
	}
	rest := strings.TrimPrefix(e, "/cygdrive")
	drive := strings.ToUpper(rest[1:2])
	tail := rest[2:]
	if tail == "" {
		tail = "\\"
	}
	return drive + ":" + strings.ReplaceAll(tail, "/", "\\")
}

func (s *SessionFiles) platform() string {
	if s.Platform != "" {
		return s.Platform
	}
	return runtime.GOOS
}

// CheckInternalWrite runs the internal file checks over a spelling set:
// the hard denies are checked on every spelling first; then each spelling is
// judged alone and the call is allowed only when every spelling is allowed.
// Anything else passes through to the ordinary permission flow.
//
// planMode reports whether the session is in plan mode (it only widens the
// plan-file check to the workshop doc, which does not exist here).
func (s *SessionFiles) CheckInternalWrite(path string, spellings []string, planMode bool) FileVerdict {
	if s.Diskless {
		return verdictDiskless
	}
	if len(spellings) == 0 {
		return s.checkInternalWriteOne(s.resolve(path))
	}
	conv := make([]string, len(spellings))
	for i, sp := range spellings {
		conv[i] = s.toWindowsSpelling(sp)
	}
	for _, c := range conv {
		if s.isHostCredsFile(c) {
			return verdictHostCreds
		}
	}
	if v, ok := s.jobsFolderDeny(conv); ok {
		return v
	}
	for _, c := range conv {
		if s.isSeedAdmin(c) {
			return verdictSeedAdmin
		}
	}
	// There is no Gocode profile store (a hosted sign-in artifact), so no
	// profile-store check applies.
	for _, c := range conv {
		if s.isSettingsReviewStore(c) {
			return verdictSettingsReview
		}
	}
	// The first deny wins; a non-allow makes the whole call pass through.
	var first *FileVerdict
	for _, sp := range spellings {
		v := s.checkInternalWriteOne(sp)
		if v.Behavior == FileDeny {
			return v
		}
		if v.Behavior != FileAllow {
			return passthrough
		}
		if first == nil {
			vv := v
			first = &vv
		}
	}
	if first != nil {
		return *first
	}
	return passthrough
}

// checkInternalWriteOne is the internal check for a single path.
func (s *SessionFiles) checkInternalWriteOne(e string) FileVerdict {
	if s.Diskless {
		return verdictDiskless
	}
	h := filepath.Clean(e)
	y := filepath.Clean(s.toWindowsSpelling(e))
	if v, ok := s.jobsFolderDeny([]string{y}); ok {
		return v
	}
	if s.isPlanFile(h) {
		return allowVerdict(MsgPlanFileAllowed)
	}
	if !s.Restricted && s.isWorkflowScript(h) {
		return allowVerdict(MsgWorkflowScriptAllowed)
	}
	if s.isScratchpad(h) {
		return allowVerdict(MsgScratchpadAllowed)
	}
	if s.isJobTmp(h) {
		return allowVerdict(MsgJobTmpAllowed)
	}
	if !s.Restricted && strings.HasSuffix(h, ".md") && s.isAgentMemory(h) {
		return allowVerdict(MsgAgentMemoryAllowed)
	}
	// There is no memory-pause switch, so memory writes are never paused.
	if !s.Restricted && !s.AutoMemoryDirOverridden && strings.HasSuffix(h, ".md") && s.isAutoMemory(h) {
		return allowVerdict(MsgAutoMemoryAllowed)
	}
	if !s.Restricted && s.OriginalCwd != "" && h == filepath.Join(s.OriginalCwd, ".gocode", "launch.json") {
		return allowVerdict(MsgLaunchConfigAllowed)
	}
	if s.isSeedAdmin(y) {
		return verdictSeedAdmin
	}
	if s.isHostCredsFile(y) {
		return verdictHostCreds
	}
	if s.isSettingsReviewStore(y) {
		return verdictSettingsReview
	}
	return passthrough
}

// configRoots are the gocode root and the former per-user directory
// ~/.gocode, which stays protected while it still exists.
func (s *SessionFiles) configRoots() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		for _, o := range out {
			if o == p {
				return
			}
		}
		out = append(out, p)
	}
	add(s.ConfigDir)
	if s.HomeDir != "" {
		add(filepath.Join(s.HomeDir, ".gocode"))
	}
	return out
}

// projectRoots is the original cwd plus the project config root.
func (s *SessionFiles) projectRoots() []string {
	var out []string
	if s.OriginalCwd != "" {
		out = append(out, filepath.Clean(s.OriginalCwd))
	}
	if s.ProjectConfigRoot != "" {
		out = append(out, filepath.Clean(s.ProjectConfigRoot))
	}
	return out
}

// isHostCredsFile reports the host credentials file named by
// $GOCODE_CODE_HOST_CREDS_FILE, compared case-folded across its spellings.
func (s *SessionFiles) isHostCredsFile(e string) bool {
	n := s.HostCredsFile
	if n == "" {
		return false
	}
	trimmed := strings.TrimRight(n, "/")
	if s.platform() == "windows" {
		trimmed = strings.TrimRight(n, `\/`)
	}
	if trimmed == "" {
		trimmed = n
	}
	h := foldCase(filepath.Clean(e))
	for _, sp := range s.spellingsOf(trimmed) {
		if foldCase(filepath.Clean(sp)) == h {
			return true
		}
	}
	return false
}

// isSeedAdmin reports <config>/seed-admin (and the same under the config dir's
// real path) or anything beneath it.
func (s *SessionFiles) isSeedAdmin(e string) bool {
	if s.ConfigDir == "" {
		return false
	}
	n := foldPath(e)
	roots := []string{foldPath(filepath.Join(s.ConfigDir, "seed-admin"))}
	if r := s.realpath(s.ConfigDir); r != "" {
		roots = append(roots, foldPath(filepath.Join(r, "seed-admin")))
	}
	for _, r := range roots {
		if n == r || strings.HasPrefix(n, r+sep) {
			return true
		}
	}
	return false
}

// isSettingsReviewStore reports <root>/state or its settings-review.json (and
// the same under the former ~/.gocode), matched exactly across their
// spellings.
func (s *SessionFiles) isSettingsReviewStore(e string) bool {
	n := foldPath(e)
	for _, root := range s.configRoots() {
		r := filepath.Join(root, "state")
		for _, p := range []string{r, filepath.Join(r, "settings-review.json")} {
			for _, sp := range s.spellingsOf(p) {
				if foldPath(sp) == n {
					return true
				}
			}
		}
	}
	return false
}

// jobsFolderDeny is the unconditional jobs-folder deny: a write to
// <config>/jobs/<id>/adopt.json (the bg-fork handoff carrier) is denied. The
// broader jobs-folder rules (the whole jobs folder, daemon.json,
// shell-snapshots, session-env) are not applied.
func (s *SessionFiles) jobsFolderDeny(spellings []string) (FileVerdict, bool) {
	for _, root := range s.configRoots() {
		rootSegs := strings.Split(foldPath(filepath.Join(root, "jobs")), sep)
		for _, e := range spellings {
			c := strings.Split(foldPath(e), sep)
			if len(c) < len(rootSegs) {
				continue
			}
			match := true
			for i := range rootSegs {
				if c[i] != rootSegs[i] {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			rest := c[len(rootSegs):]
			if len(rest) > 0 && rest[len(rest)-1] == "" {
				rest = rest[:len(rest)-1]
			}
			if len(rest) == 2 && strings.HasPrefix(rest[1], "adopt.json") {
				return verdictAdoptJSON, true
			}
		}
	}
	return FileVerdict{}, false
}

// IsPlanFile reports whether path (absolute, or relative to the launch
// directory) is one of the current session's plan files, which the session
// may always read.
func (s *SessionFiles) IsPlanFile(path string) bool {
	if s == nil || path == "" {
		return false
	}
	return s.isPlanFile(s.resolve(path))
}

// isPlanFile reports <plans dir>/<slug>.md or <slug>-agent-*.md for the
// current session's plan slug.
func (s *SessionFiles) isPlanFile(h string) bool {
	if s.PlanSlug == "" || s.PlansDir == "" {
		return false
	}
	if filepath.Dir(h) != filepath.Clean(s.PlansDir) {
		return false
	}
	b := filepath.Base(h)
	return b == s.PlanSlug+".md" ||
		(strings.HasPrefix(b, s.PlanSlug+"-agent-") && strings.HasSuffix(b, ".md"))
}

// isWorkflowScript reports a .js file under the session's workflow-scripts dir.
func (s *SessionFiles) isWorkflowScript(h string) bool {
	if s.WorkflowScriptsDir == "" {
		return false
	}
	d := s.WorkflowScriptsDir
	if !strings.HasSuffix(d, sep) {
		d += sep
	}
	return strings.HasPrefix(h, d) && strings.HasSuffix(h, ".js")
}

// IsScratchpad reports whether path (absolute, or relative to the launch
// directory) lies in the session scratchpad, which the session may always
// read. Every spelling must: a link inside the scratchpad that leads out of
// it does not count.
func (s *SessionFiles) IsScratchpad(path string) bool {
	if s == nil || path == "" || s.ScratchpadDir == "" {
		return false
	}
	sp := s.spellingsOf(s.resolve(path))
	if len(sp) == 0 {
		return false
	}
	for _, h := range sp {
		if !s.isScratchpad(s.resolve(h)) {
			return false
		}
	}
	return true
}

// isScratchpad reports the scratchpad dir itself or beneath it, case-folded,
// with no excluded segment and no sensitive file name at the leaf.
func (s *SessionFiles) isScratchpad(h string) bool {
	if s.ScratchpadDir == "" {
		return false
	}
	cmp := foldCase(filepath.Clean(h))
	dir := foldCase(filepath.Clean(s.ScratchpadDir))
	prefix := dir + sep
	return cmp == dir || (strings.HasPrefix(cmp, prefix) && !hasExcludedSegment(cmp, prefix, sensitiveFileSet))
}

// isJobTmp reports, in a background session, the job directory's tmp/ subtree.
func (s *SessionFiles) isJobTmp(e string) bool {
	if s.BackgroundJobDir == "" || s.ConfigDir == "" {
		return false
	}
	jobs := filepath.Join(s.ConfigDir, "jobs") + sep
	n := filepath.Clean(s.BackgroundJobDir)
	if !strings.HasPrefix(n, jobs) {
		return false
	}
	h := n + sep + "tmp" + sep
	if !strings.HasPrefix(foldCase(e), foldCase(h)) {
		return false
	}
	return !hasExcludedSegment(e, h, sensitiveFileSet)
}

// agentMemoryRoot returns the agent-memory root a path lies under, or "".
func (s *SessionFiles) agentMemoryRoot(e string) string {
	if !s.Diskless {
		base := s.RemoteMemoryDir
		if base == "" {
			base = s.ConfigDir
		}
		if base != "" {
			h := filepath.Join(base, "agent-memory") + sep
			if strings.HasPrefix(e, h) {
				return h
			}
		}
	}
	cwd := s.cwd()
	if cwd != "" {
		r := filepath.Join(cwd, ".gocode", "agent-memory") + sep
		if strings.HasPrefix(e, r) {
			return r
		}
	}
	if s.RemoteMemoryDir != "" {
		h := filepath.Join(s.RemoteMemoryDir, "projects") + sep
		if strings.Contains(e, sep+"agent-memory-local"+sep) && strings.HasPrefix(e, h) {
			return h
		}
		return ""
	}
	if cwd != "" {
		r := filepath.Join(cwd, ".gocode", "agent-memory-local") + sep
		if strings.HasPrefix(e, r) {
			return r
		}
	}
	return ""
}

// isAgentMemory reports a path beneath an agent-memory root with no excluded segment.
func (s *SessionFiles) isAgentMemory(e string) bool {
	n := filepath.Clean(e)
	r := s.agentMemoryRoot(n)
	return r != "" && !hasExcludedSegment(n, r, nil)
}

// isAutoMemory reports a path beneath the auto-memory directory (which must end in a
// separator and not be a root-like path) with no excluded segment.
func (s *SessionFiles) isAutoMemory(e string) bool {
	n := s.AutoMemoryDir
	if n == "" || !strings.HasSuffix(n, sep) {
		return false
	}
	trimmed := strings.TrimRight(n, `/\`)
	if !filepath.IsAbs(trimmed) || len(trimmed) < 3 {
		return false
	}
	r := filepath.Clean(e)
	if !strings.HasPrefix(r, n) {
		return false
	}
	return !hasExcludedSegment(r, n, nil)
}

// --- settings-file and sensitive-file write safety --------------------------

// MsgWriteNotGranted and MsgSensitiveFile are the write-safety messages.
func MsgWriteNotGranted(path string) string {
	return "gocode requested permissions to write to " + escapeControl(path) + ", but you haven't granted it yet."
}

// MsgSensitiveFile is the sensitive-file message.
func MsgSensitiveFile(path string) string {
	return "gocode requested permissions to edit " + escapeControl(path) + " which is a sensitive file."
}

// MsgSuspiciousWindowsPathWrite is the suspicious-path message (control
// escaping, not display truncation).
func MsgSuspiciousWindowsPathWrite(path string) string {
	return "gocode requested permissions to write to " + escapeControl(path) +
		", which contains a suspicious Windows path pattern that requires manual approval."
}

// CheckWriteSafety: a write to a suspicious Windows spelling, a gocode
// settings file, a gocode command/agent/skill definition or a sensitive file
// is not safe and must be approved by the person.
func (s *SessionFiles) CheckWriteSafety(path string, spellings []string) WriteSafety {
	if s.Diskless {
		return WriteSafety{Safe: false, Message: MsgWriteNotGranted(path)}
	}
	w := spellings
	if len(w) == 0 {
		w = s.spellingsOf(path)
	}
	settings, linkFrom := s.settingsFileKind(w)
	for _, p := range w {
		if IsSuspiciousWindowsSpelling(p, s.Platform, s.TrustedNetworkDirectories) {
			ws := WriteSafety{Message: MsgSuspiciousWindowsPathWrite(path), CircuitBreaker: circuitBreakerSuspiciousWinPath}
			if settings {
				ws.Also = []string{CircuitBreakerSettingsFile}
			}
			return ws
		}
	}
	if settings {
		msg := MsgWriteNotGranted(path)
		if linkFrom != "" {
			msg += " The gocode settings file " + displayPath(linkFrom) + " leads here through a link."
		}
		return WriteSafety{Message: msg, ClassifierApprovable: linkFrom == "", CircuitBreaker: CircuitBreakerSettingsFile}
	}
	for _, p := range w {
		if s.isGocodeDefinitionPath(p) {
			return WriteSafety{Message: MsgWriteNotGranted(path), ClassifierApprovable: true}
		}
	}
	for _, p := range w {
		if s.isSensitiveFile(p) {
			return WriteSafety{Message: MsgSensitiveFile(path), ClassifierApprovable: true}
		}
	}
	return WriteSafety{Safe: true}
}

// settingsFileNames are settings.json, cowork_settings.json and
// settings.local.json; configRootNames are the config root directory names
// and projectSettingsNames the project settings file names.
var (
	configRootNames      = []string{"settings.json", "cowork_settings.json"}
	projectSettingsNames = []string{"settings.json", "settings.local.json"}
)

// allSettingsFiles returns every settings source path plus the settings
// files of each config root.
func (s *SessionFiles) allSettingsFiles() []string {
	out := append([]string(nil), s.SettingsFiles...)
	for _, root := range s.configRoots() {
		for _, n := range configRootNames {
			out = append(out, filepath.Join(root, n))
		}
	}
	return out
}

// settingsLinks maps, for every settings file that is itself a symlink,
// the folded spellings of its walk mapped back to the settings file.
func (s *SessionFiles) settingsLinks() map[string]string {
	m := map[string]string{}
	for _, f := range s.allSettingsFiles() {
		fi, err := s.fsys().Lstat(f)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		for _, sp := range s.spellingsOf(f) {
			m[foldPath(sp)] = f
		}
	}
	return m
}

// settingsFileKind reports whether any spelling is a gocode
// settings file or settings directory. linkFrom names a symlinked settings
// file that leads to one of the spellings when none of the spellings is that
// file itself (the "leads here through a link" sentence).
func (s *SessionFiles) settingsFileKind(w []string) (bool, string) {
	links := s.settingsLinks()
	found := false
	for _, p := range w {
		if s.isSettingsFile(p, links) || s.isSettingsDir(p) {
			found = true
			break
		}
	}
	if !found {
		for _, p := range w {
			n := s.resolve(p)
			if !isJSONName(foldPath(n)) {
				continue
			}
			for _, d := range s.ManagedDropInDirs {
				if foldPath(d) == filepath.Dir(foldPath(n)) {
					found = true
				}
			}
		}
	}
	if !found {
		return false, ""
	}
	folded := make([]string, len(w))
	for i, p := range w {
		folded[i] = foldPath(s.resolve(p))
	}
	for _, f := range folded {
		if src, ok := links[f]; ok {
			srcFolded := foldPath(src)
			for _, g := range folded {
				if g == srcFolded {
					return true, ""
				}
			}
			return true, src
		}
	}
	return true, ""
}

// isJSONName is the ".json" test on the folded path.
func isJSONName(folded string) bool { return strings.HasSuffix(folded, ".json") }

// isSettingsFile reports whether p is a settings file.
func (s *SessionFiles) isSettingsFile(e string, links map[string]string) bool {
	n := s.resolve(e)
	r := foldPath(n)
	if _, ok := links[r]; ok {
		return true
	}
	if strings.HasSuffix(r, sep+".gocode"+sep+"settings.json") || strings.HasSuffix(r, sep+".gocode"+sep+"settings.local.json") {
		return true
	}
	for _, f := range s.allSettingsFiles() {
		if foldPath(f) == r {
			return true
		}
	}
	if isJSONName(r) {
		for _, d := range s.ManagedDropInDirs {
			if foldPath(d) == filepath.Dir(r) {
				return true
			}
		}
	}
	base := filepath.Base(r)
	type group struct {
		dir   string
		names []string
	}
	var groups []group
	for _, root := range s.configRoots() {
		groups = append(groups, group{root, configRootNames})
	}
	for _, pr := range s.projectRoots() {
		groups = append(groups, group{filepath.Join(pr, ".gocode"), projectSettingsNames})
	}
	for _, g := range groups {
		ok := false
		for _, nm := range g.names {
			if foldSegment(nm) == base {
				ok = true
			}
		}
		if !ok {
			continue
		}
		if real := s.realpath(g.dir); real != "" && filepath.Join(foldPath(real), base) == r {
			return true
		}
	}
	return false
}

// isSettingsDir reports a ".gocode" directory, or one of the settings
// directories (config roots, project .gocode dirs, the managed drop-in dirs)
// or their real paths.
func (s *SessionFiles) isSettingsDir(e string) bool {
	n := strings.TrimRight(foldPath(s.resolve(e)), `\/`)
	if n == "" {
		return false
	}
	if filepath.Base(n) == foldSegment(".gocode") {
		return true
	}
	dirs := append([]string(nil), s.configRoots()...)
	for _, pr := range s.projectRoots() {
		dirs = append(dirs, filepath.Join(pr, ".gocode"))
	}
	dirs = append(dirs, s.ManagedDropInDirs...)
	for _, d := range dirs {
		if foldPath(d) == n {
			return true
		}
		if r := s.realpath(d); r != "" && foldPath(r) == n {
			return true
		}
	}
	return false
}

// isGocodeDefinitionPath reports a settings file, or anything beneath a
// project's .gocode/commands, .gocode/agents or .gocode/skills.
func (s *SessionFiles) isGocodeDefinitionPath(e string) bool {
	if s.isSettingsFile(e, s.settingsLinks()) {
		return true
	}
	abs := s.resolve(e)
	for _, pr := range s.projectRoots() {
		for _, sub := range []string{"commands", "agents", "skills"} {
			if pathWithin(abs, filepath.Join(pr, ".gocode", sub), true, false) {
				return true
			}
		}
	}
	return false
}

// workingDirPrefixLen returns the length of the working-directory prefix a
// split path shares, cut back to a ".gocode" segment inside the working
// directory itself (unless it is a worktree's).
func (s *SessionFiles) workingDirPrefixLen(h []string) int {
	n := 0
	if s.OriginalCwd == "" {
		return 0
	}
	for _, r := range s.spellingsOf(s.OriginalCwd) {
		sg := strings.Split(s.resolve(r), sep)
		if len(sg) > 1 && sg[len(sg)-1] == "" {
			sg = sg[:len(sg)-1]
		}
		k := 0
		for k < len(sg) && k < len(h) && (h[k] == sg[k] ||
			(k == 0 && len(h[k]) == 2 && h[k][1] == ':' && isASCIILetter(h[k][0]) && strings.EqualFold(h[k], sg[k]))) {
			k++
		}
		if k == len(sg) {
			y := k
			for w := 0; w < k; w++ {
				next := ""
				if w+1 < len(sg) {
					next = sg[w+1]
				}
				if foldSegment(sg[w]) == ".gocode" && foldSegment(next) != "worktrees" {
					y = w
					break
				}
			}
			if y > n {
				n = y
			}
		}
	}
	return n
}

// ntObjectPrefix is /^[\\/]\?\?[\\/]/.
var ntObjectPrefix = regexp.MustCompile(`^[\\/]\?\?[\\/]`)

// isSensitiveFile (there is no remote mode): UNC and automounter
// spellings outside the trusted network directories, any segment naming a
// tool-config directory (.git, .vscode, .gocode — except a worktree below the
// working directory — …), a ".config/git" run, or a sensitive dotfile name at
// the leaf.
//
// Not checked here: the credentials store, command producer directories and
// inline plugin roots — they depend on plugin and credential state kept
// elsewhere.
func (s *SessionFiles) isSensitiveFile(e string) bool {
	trusted := s.TrustedNetworkDirectories
	// An NT object path, or a UNC/device spelling that is neither a WSL
	// path nor under a trusted network directory.
	if ntObjectPrefix.MatchString(e) {
		return true
	}
	if pathclass.IsUNCOrDevicePath(e) && !pathclass.IsWSLPath(e) && !underTrustedNetworkDir(e, trusted) {
		return true
	}
	// An automounter / firmlink spelling not under a trusted directory.
	if pathclass.IsAutomountNetBareOrFirmlink(e) && !underTrustedNetworkDir(e, trusted) {
		return true
	}
	abs := s.resolve(e)
	h := strings.Split(abs, sep)
	limit := s.workingDirPrefixLen(h)
	seenWorktree := false
	for p := 0; p < len(h); p++ {
		a := foldSegment(h[p])
	dirs:
		for _, d := range sensitiveDirs {
			if a != foldCase(d) {
				continue
			}
			if d == ".gocode" {
				if seenWorktree {
					return true
				}
				next := ""
				if p+1 < len(h) {
					next = foldSegment(h[p+1])
				}
				if next == "worktrees" {
					if p >= limit {
						seenWorktree = true
					}
					break dirs
				}
			}
			return true
		}
	}
	for _, run := range sensitiveDirRuns {
		parts := strings.Split(run, "/")
		for a := 0; a+len(parts) <= len(h); a++ {
			all := true
			for b, part := range parts {
				if foldSegment(h[a+b]) != foldCase(part) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
	}
	if last := h[len(h)-1]; last != "" {
		if sensitiveFileSet[foldSegment(last)] {
			return true
		}
	}
	return false
}

// --- working-directory containment ------------------------------------------

// WorkingDirs is the original cwd plus the additional working directories.
func (s *SessionFiles) WorkingDirs() []string {
	var out []string
	if s.OriginalCwd != "" {
		out = append(out, s.OriginalCwd)
	}
	return append(out, s.AdditionalWorkingDirs...)
}

// WithinWorkingDirs reports whether every spelling lies within some spelling of some
// working directory, compared without case folding and with UNC-shape parity.
// With no working directory configured nothing is within.
func (s *SessionFiles) WithinWorkingDirs(spellings []string) bool {
	var roots []string
	for _, d := range s.WorkingDirs() {
		roots = append(roots, s.spellingsOf(d)...)
	}
	if len(roots) == 0 || len(spellings) == 0 {
		return false
	}
	for _, sp := range spellings {
		inside := false
		for _, r := range roots {
			if pathWithin(sp, r, false, true) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}
