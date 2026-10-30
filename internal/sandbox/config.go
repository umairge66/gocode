package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SessionPaths are the session directories the runtime configuration is
// derived from.
type SessionPaths struct {
	// Cwd is the project working directory (writable).
	Cwd string
	// Home is the user's home directory.
	Home string
	// ConfigDir is the gocode root (internal/confighome).
	ConfigDir string
	// TempDir is the per-user writable temp directory (TempRoot()).
	TempDir string
	// SettingsFiles are every settings-source file of the session (user,
	// project, local, managed, --settings); all are write-protected.
	SettingsFiles []string
	// ManagedDirs are the managed-settings directories (drop-ins).
	ManagedDirs []string
	// AddDirs are the --add-dir directories (writable like the cwd).
	AddDirs []string
}

// RuntimeConfig is the resolved per-session confinement: the path lists and
// network lists every wrapped command is rendered from.
type RuntimeConfig struct {
	DenyRead   []string
	AllowRead  []string
	AllowWrite []string
	DenyWrite  []string
	// FilesystemDisabled leaves the filesystem unconfined.
	FilesystemDisabled bool

	AllowedDomains  []string
	DeniedDomains   []string
	StrictAllowlist bool

	AllowUnixSockets             []string
	AllowAllUnixSockets          bool
	AllowLocalBinding            bool
	AllowMachLookup              []string
	HTTPProxyPort                int
	SOCKSProxyPort               int
	EnableWeakerNestedSandbox    bool
	EnableWeakerNetworkIsolation bool
	AllowAppleEvents             bool
	AllowNestedUserNamespaces    bool
	IgnoreViolations             map[string][]string
	GitSafeDirectories           []string
	// UnsetEnv are the sandbox.credentials.envVars denied to the command.
	UnsetEnv  []string
	BwrapPath string
	SocatPath string
}

// configHomeProtected are the config-home entries a sandboxed command may
// never write (they configure code that later runs outside the sandbox, or
// hold the session's own state).
var configHomeProtected = []string{
	"shell-snapshots", "session-env", "plugins", "hooks", "skills", "workflows",
	"commands", "agents", "routines", "rules", "output-styles", "scheduled_tasks.json",
	"launch.json", "gocode.md", "projects", "daemon.json", "policy-limits.json", "backups",
	"local", "jobs", "seed-admin", "daemon", "loop.md", "cowork_plugins",
	"mcp-skill-archives", "mcp-discovery-cache", "shares",
	"todos", "statsig", "logs", "access-audit",
	"remote-settings-helper-consent", "remote-settings-consent.json", "remote-settings.json",
	"state", "settings.json", "settings.local.json",
}

// configHomeSecrets are config-home entries holding credentials: denied for
// reads as well as writes. "ide" holds IDE lock files with their auth tokens;
// auth.json / .credentials.json are the API-key and OAuth stores, protected
// the same way as the host credentials file (gocode_CODE_HOST_CREDS_FILE).
var configHomeSecrets = []string{"ide", "auth.json", ".credentials.json"}

// projectProtected are the repository paths that configure gocode or run code
// outside the sandbox later (settings, hooks, skills, commands, agents,
// workflows, MCP servers).
var projectProtected = []string{
	".gocode/settings.json", ".gocode/settings.local.json",
	".gocode/skills", ".gocode/hooks", ".gocode/commands", ".gocode/agents",
	".gocode/launch.json", ".gocode/workflows", ".gocode/routines", ".gocode/output-styles",
	".gocode/scheduled_tasks.json", ".gocode/loop.md", ".mcp.json",
}

// dangerousFiles / dangerousDirs are the shell, git and editor configuration
// names the sandbox runtime always write-protects in the working directory and
// below it.
var (
	dangerousFiles = []string{".gitconfig", ".gitmodules", ".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".profile", ".ripgreprc", ".mcp.json",
		// The other startup files bash/zsh source.
		".zshenv", ".zlogin", ".zlogout", ".bash_login", ".bash_logout"}
	dangerousDirs = []string{".vscode", ".idea", ".gocode/commands", ".gocode/agents"}
)

// homeProtected are the home-directory files that configure a login shell,
// git or ssh: write-protected whenever the home directory (or the part of it
// holding them) is a writable root, existing or not.
var homeProtected = []string{
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile",
	".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout",
	".gitconfig", ".config/git", ".ssh", ".config/fish", ".inputrc",
	".mcp.json", ".ripgreprc",
}

// mandatoryDenySearchDepth bounds the scan for nested dangerous names.
const mandatoryDenySearchDepth = 8

// maxDenyScanEntries bounds the nested scan's work across all writable roots.
const maxDenyScanEntries = 50000

// BuildRuntimeConfig derives the confinement from the settings and the session
// paths.
//
// Writes: the working directory, the per-user temp dir, --add-dir and
// permissions.additionalDirectories, and sandbox.filesystem.allowWrite with
// Edit(...) allow rules — minus the write-protected paths (every settings
// file, the repository's .gocode configuration, git hooks/config, shell rc
// files, the config home's state). Reads: everything except
// sandbox.filesystem.denyRead with Read(...) deny rules and the credential
// stores, re-opened by allowRead.
func BuildRuntimeConfig(s Settings, p SessionPaths) RuntimeConfig {
	rc := RuntimeConfig{
		FilesystemDisabled:           s.Filesystem.Disabled,
		AllowedDomains:               s.Network.AllowedDomains,
		DeniedDomains:                s.Network.DeniedDomains,
		StrictAllowlist:              s.Network.StrictAllowlist,
		AllowUnixSockets:             s.Network.AllowUnixSockets,
		AllowAllUnixSockets:          s.Network.AllowAllUnixSockets,
		AllowLocalBinding:            s.Network.AllowLocalBinding,
		AllowMachLookup:              s.Network.AllowMachLookup,
		HTTPProxyPort:                s.Network.HTTPProxyPort,
		SOCKSProxyPort:               s.Network.SOCKSProxyPort,
		EnableWeakerNestedSandbox:    s.EnableWeakerNestedSandbox,
		EnableWeakerNetworkIsolation: s.EnableWeakerNetworkIsolation,
		AllowAppleEvents:             s.AllowAppleEvents,
		AllowNestedUserNamespaces:    s.AllowNestedUserNamespaces,
		IgnoreViolations:             s.IgnoreViolations,
		BwrapPath:                    s.BwrapPath,
		SocatPath:                    s.SocatPath,
	}
	if rc.AllowedDomains == nil {
		rc.AllowedDomains = []string{}
	}

	// Writable roots.
	write := []string{p.Cwd}
	if p.TempDir != "" {
		write = append(write, p.TempDir)
	}
	write = append(write, p.AddDirs...)
	write = append(write, s.AdditionalDirectories...)
	write = append(write, s.Filesystem.AllowWrite...)

	// Write-protected paths.
	var deny []string
	deny = append(deny, p.SettingsFiles...)
	deny = append(deny, p.ManagedDirs...)
	if p.ConfigDir != "" {
		// The whole config home is protected (it configures code that later
		// runs outside the sandbox); the named entries stay listed so a
		// symlinked entry is protected at its target too.
		deny = append(deny, p.ConfigDir)
		for _, name := range configHomeProtected {
			deny = append(deny, filepath.Join(p.ConfigDir, name))
		}
		for _, name := range configHomeSecrets {
			deny = append(deny, filepath.Join(p.ConfigDir, name))
		}
	}
	if p.Home != "" {
		for _, name := range homeProtected {
			deny = append(deny, filepath.Join(p.Home, filepath.FromSlash(name)))
		}
	}
	// Every directory a command may write gets the repository and shell
	// protections, not only the working directory: an --add-dir /
	// additionalDirectories / allowWrite root is a project too.
	roots := []string{p.Cwd}
	roots = append(roots, p.AddDirs...)
	roots = append(roots, s.AdditionalDirectories...)
	roots = append(roots, s.Filesystem.AllowWrite...)
	budget := maxDenyScanEntries
	var safeDirs []string
	for i, root := range dedupe(cleanAll(roots)) {
		if root == "" || isGlob(root) || root == "/" {
			continue
		}
		for _, rel := range projectProtected {
			deny = append(deny, filepath.Join(root, rel))
		}
		gitDenies, gitWrites, safe := gitProtection(root)
		deny = append(deny, gitDenies...)
		write = append(write, gitWrites...)
		if i == 0 || len(safe) > 0 {
			safeDirs = append(safeDirs, safe...)
		}
		deny = append(deny, mandatoryDenies(root, &budget)...)
	}
	rc.GitSafeDirectories = dedupe(safeDirs)
	deny = append(deny, s.Filesystem.DenyWrite...)

	// Read denies.
	var readDeny []string
	if p.ConfigDir != "" {
		for _, name := range configHomeSecrets {
			readDeny = append(readDeny, filepath.Join(p.ConfigDir, name))
		}
	}
	readDeny = append(readDeny, s.Filesystem.DenyRead...)
	readDeny = append(readDeny, s.CredentialFileDeny...)
	rc.UnsetEnv = s.CredentialEnvDeny

	rc.AllowWrite = sortedDedupe(cleanAll(write))
	rc.DenyWrite = sortedDedupe(cleanAll(deny))
	rc.DenyRead = sortedDedupe(cleanAll(readDeny))
	rc.AllowRead = sortedDedupe(cleanAll(s.Filesystem.AllowRead))
	return rc
}

func cleanAll(in []string) []string {
	var out []string
	for _, p := range in {
		if p == "" {
			continue
		}
		if isGlob(p) {
			out = append(out, p)
			continue
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// gitProtection write-protects the repository's git hooks and configuration
// (a sandboxed command must not plant a hook or core.fsmonitor that later runs
// unsandboxed), and grants writes to a linked worktree's common git directory
// so commits from a worktree still work.
func gitProtection(cwd string) (deny, write, safe []string) {
	gitPath := filepath.Join(cwd, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		// No repository here yet: a command must not create one whose hooks
		// or config later run outside the sandbox when the user runs git in
		// this directory (git uses the nearest .git). Protecting the
		// not-yet-existing hooks/config makes the Linux backend block
		// creating .git itself for the command's duration.
		return []string{filepath.Join(gitPath, "hooks"), filepath.Join(gitPath, "config")}, nil, nil
	}
	safe = append(safe, cwd)
	protectGitDir := func(dir string) { deny = append(deny, gitDirDenies(dir)...) }
	if fi.IsDir() {
		protectGitDir(gitPath)
		return deny, nil, safe
	}
	// A worktree: `.git` is a "gitdir: <common>/.git/worktrees/<name>" file.
	deny = append(deny, gitPath)
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return deny, nil, safe
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir:") {
		return deny, nil, safe
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(cwd, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
		return deny, nil, safe
	}
	common := filepath.Dir(filepath.Dir(gitdir))
	if filepath.Base(common) != ".git" {
		return deny, nil, safe
	}
	write = append(write, common)
	protectGitDir(common)
	safe = append(safe, filepath.Dir(common))
	return deny, write, safe
}

// gitDirDenies are the write-protected entries of one git directory: hooks,
// config (and its lock / worktree variants), commondir, alternates, every
// linked worktree's config, and every submodule repository's hooks/config.
func gitDirDenies(dir string) []string {
	var deny []string
	deny = append(deny,
		filepath.Join(dir, "hooks"),
		filepath.Join(dir, "config"),
		filepath.Join(dir, "config.lock"),
		filepath.Join(dir, "config.worktree"),
		filepath.Join(dir, "commondir"),
		filepath.Join(dir, "objects", "info", "alternates"),
		filepath.Join(dir, "objects", "info", "http-alternates"),
	)
	// Submodule repositories keep their own hooks and config under
	// <git>/modules/<path…>/.
	modules := filepath.Join(dir, "modules")
	n := 0
	_ = filepath.WalkDir(modules, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > 5000 {
			return fs.SkipAll
		}
		if d.IsDir() && (d.Name() == "objects" || d.Name() == "refs" || d.Name() == "logs") {
			return fs.SkipDir
		}
		switch d.Name() {
		case "hooks":
			if d.IsDir() {
				deny = append(deny, path)
				return fs.SkipDir
			}
		case "config":
			deny = append(deny, path)
		}
		return nil
	})
	if entries, err := os.ReadDir(filepath.Join(dir, "worktrees")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				deny = append(deny,
					filepath.Join(dir, "worktrees", e.Name(), "config.worktree"),
					filepath.Join(dir, "worktrees", e.Name(), "commondir"))
			}
		}
	}
	return deny
}

// mandatoryDenies lists the dangerous names in the working directory and the
// ones already present up to mandatoryDenySearchDepth levels below it (a
// nested `.git/hooks`, `.vscode`, `.bashrc`, …).
func mandatoryDenies(cwd string, budget *int) []string {
	var out []string
	for _, n := range dangerousFiles {
		out = append(out, filepath.Join(cwd, n))
	}
	for _, n := range dangerousDirs {
		out = append(out, filepath.Join(cwd, filepath.FromSlash(n)))
	}
	if fi, err := os.Lstat(filepath.Join(cwd, ".git")); err == nil && fi.IsDir() {
		out = append(out, filepath.Join(cwd, ".git", "hooks"), filepath.Join(cwd, ".git", "config"))
	}
	fileSet := map[string]bool{}
	for _, n := range dangerousFiles {
		fileSet[n] = true
	}
	dirSet := map[string]bool{".vscode": true, ".idea": true}
	_ = filepath.WalkDir(cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if *budget--; *budget < 0 {
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(cwd, path)
		if rerr != nil || rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		name := d.Name()
		if d.IsDir() {
			if name == "node_modules" {
				return fs.SkipDir
			}
			if depth > mandatoryDenySearchDepth {
				return fs.SkipDir
			}
			switch {
			case dirSet[name]:
				out = append(out, path)
				return fs.SkipDir
			case name == "commands" || name == "agents":
				if filepath.Base(filepath.Dir(path)) == ".gocode" {
					out = append(out, path)
					return fs.SkipDir
				}
			case name == ".git":
				out = append(out, gitDirDenies(path)...)
				return fs.SkipDir
			}
			return nil
		}
		if name == ".git" {
			// A nested worktree / submodule pointer file (gitdir: …).
			out = append(out, path)
			return nil
		}
		if depth <= mandatoryDenySearchDepth+1 && fileSet[name] {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// TempRoot is the per-user temp directory sandboxed commands write to and see
// as $TMPDIR: <gocode_CODE_TMPDIR or the OS temp dir>/gocode-<uid>, with
// symlinks resolved.
func TempRoot(getenv func(string) string) string {
	base := ""
	if getenv != nil {
		base = getenv("GOCODE_CODE_TMPDIR")
		if base == "" {
			base = getenv("gocode_CODE_TMPDIR")
		}
	}
	if base == "" {
		base = os.TempDir()
	}
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	return filepath.Join(base, "gocode-"+uidString())
}

// uidString is the numeric user id ("0" where there is none, e.g. Windows).
func uidString() string {
	uid := os.Getuid()
	if uid < 0 {
		uid = 0
	}
	return strconv.Itoa(uid)
}
