package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/confighome"
	envtrust "github.com/umairge66/gocode/internal/envtrust"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/git"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/skills"
)

// Workspace trust (a "light" trust dialog).
//
// A folder is asked about only when it carries something that would run code
// or widen permissions before the user agrees: project/local hooks, a
// statusLine command, local MCP servers or .mcp.json approvals, allow/ask
// rules, additionalDirectories, a loosening defaultMode, sandbox loosening,
// enabledPlugins, a model/provider override (settingsload/trust.go), project
// skills/commands with `!` shell blocks, allowed-tools or hooks, and project
// agents with a loosening permissionMode. A plain folder — code, maybe a
// gocode.md — is never asked about.
//
// Until the folder is trusted those items are ignored (settingsContext sets
// settingsload.Context.UntrustedProject; the skill/command/agent parts are
// gated in buildSession). The answer is remembered in the user's global
// config, never in the repository: <gocode root>/.gocode.json (by default
// ~/.config/gocode/.gocode.json) projects[<path>].hasTrustDialogAccepted.

// defaultTrustCheck is the trust lookup used when an Env carries no override.
// Production reads the user's trust store; the package's tests replace it in
// TestMain so the many tests that put project settings in temp dirs keep
// running as trusted, and the trust tests opt back in per Env (trustCheck).
var defaultTrustCheck = folderTrustedInStore

// workspaceTrusted reports whether the session's working folder is trusted.
// A session with no working directory has no project sources to gate.
func workspaceTrusted(env *Env) bool {
	cwd := sessionCwd(env)
	if cwd == "" {
		return true
	}
	if env.trustSession {
		return true
	}
	if env.trustCheck != nil {
		return env.trustCheck(cwd)
	}
	return defaultTrustCheck(env, cwd)
}

// gocodeGlobalConfigPath is the user's global state file, <gocode
// root>/.gocode.json (internal/confighome). "" when no root resolves.
func gocodeGlobalConfigPath(env *Env) string {
	root, ok := confighome.Resolve(envGetenv(env), trustHome(env))
	if !ok {
		return ""
	}
	return confighome.StatePath(root)
}

func trustHome(env *Env) string {
	get := envGetenv(env)
	if h := get("HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		return get("USERPROFILE")
	}
	return ""
}

// canonicalDir resolves dir to a clean absolute path with symlinks evaluated
// (falling back to the cleaned path when it cannot be resolved).
func canonicalDir(dir string) string {
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir)
}

// findGitRoot returns the nearest ancestor of dir (inclusive) holding a .git
// entry, or "".
func findGitRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			return ""
		}
		d = p
	}
}

// isHomeOrAncestor reports whether dir is the home directory or one of its
// ancestors (including the filesystem root). Such a folder is trusted only for
// itself, never for the folders inside it, and accepting it in the dialog is
// remembered for the session only.
func isHomeOrAncestor(dir, home string) bool {
	if dir == "" {
		return false
	}
	if filepath.Dir(dir) == dir {
		return true // a filesystem root
	}
	if home == "" {
		return false
	}
	if samePath(dir, home) {
		return true
	}
	rel, err := filepath.Rel(dir, home)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// trustKey is the projects[...] key a decision for dir is stored under: the
// enclosing git root, else dir itself. A git root that is the
// home directory or above it (a dotfiles repository) is not used as the key.
func trustKey(dir, home string) (key, root string) {
	dir = canonicalDir(dir)
	root = findGitRoot(dir)
	if root != "" && isHomeOrAncestor(root, home) {
		root = ""
	}
	if root != "" {
		return root, root
	}
	return dir, ""
}

// readTrustProjects reads the projects map of the global config file.
func readTrustProjects(path string) map[string]any {
	if path == "" {
		return nil
	}
	obj := readJSONObject(path)
	p, _ := obj["projects"].(map[string]any)
	return p
}

func projectAccepted(projects map[string]any, key string) bool {
	if key == "" {
		return false
	}
	e, _ := projects[key].(map[string]any)
	b, _ := e["hasTrustDialogAccepted"].(bool)
	if b {
		return true
	}
	if runtime.GOOS == "windows" {
		for k, v := range projects {
			if strings.EqualFold(k, key) {
				e, _ := v.(map[string]any)
				if b, _ := e["hasTrustDialogAccepted"].(bool); b {
					return true
				}
			}
		}
	}
	return false
}

// folderTrustedInStore is the real trust lookup: dir is trusted when it, or an
// ancestor up to its repository root (any ancestor outside a repository), was
// accepted. The home directory and its ancestors only ever count for
// themselves.
func folderTrustedInStore(env *Env, dir string) (trusted bool) {
	projects := readTrustProjects(gocodeGlobalConfigPath(env))
	if len(projects) == 0 {
		return false
	}
	home := canonicalDir(trustHome(env))
	raw := filepath.Clean(dir)
	dir = canonicalDir(dir)
	if projectAccepted(projects, dir) || projectAccepted(projects, raw) {
		return true
	}
	_, root := trustKey(dir, home)
	for s := filepath.Dir(dir); ; {
		if root != "" && !within(s, root) {
			return false
		}
		if !isHomeOrAncestor(s, home) && projectAccepted(projects, s) {
			return true
		}
		if root != "" && samePath(s, root) {
			return false
		}
		p := filepath.Dir(s)
		if p == s {
			return false
		}
		s = p
	}
}

// within reports whether p is root or inside it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || !strings.HasPrefix(rel, ".."))
}

// setFolderTrust records (accept) or clears (revoke) the trust decision for
// dir's key in the global config file, returning the key used.
func setFolderTrust(env *Env, dir string, accept bool) (string, error) {
	path := gocodeGlobalConfigPath(env)
	if path == "" {
		return "", errors.New("cannot locate the gocode state file (set HOME, XDG_CONFIG_HOME or GOCODE_CONFIG_DIR)")
	}
	key, _ := trustKey(dir, canonicalDir(trustHome(env)))
	err := settingsload.UpdateFile(path, func(obj map[string]any) (bool, error) {
		projects, _ := obj["projects"].(map[string]any)
		if projects == nil {
			if !accept {
				return false, nil
			}
			projects = map[string]any{}
			obj["projects"] = projects
		}
		entry, _ := projects[key].(map[string]any)
		if accept {
			if b, _ := entry["hasTrustDialogAccepted"].(bool); b {
				return false, nil
			}
			if entry == nil {
				entry = map[string]any{}
			}
			entry["hasTrustDialogAccepted"] = true
			projects[key] = entry
			return true, nil
		}
		if _, has := entry["hasTrustDialogAccepted"]; !has {
			return false, nil
		}
		delete(entry, "hasTrustDialogAccepted")
		return true, nil
	})
	return key, err
}

// --- what a folder would apply -------------------------------------------

// trustReport is the trust state of the session's folder.
type trustReport struct {
	cwd     string
	key     string
	trusted bool
	// items lists, one per line, what is withheld until the folder is trusted
	// ("hooks: SessionStart (.gocode/settings.json)").
	items []string
	// kinds are the distinct item kinds, for the one-line notice.
	kinds []string
	// sessionOnly marks a folder (the home directory or above) whose acceptance
	// is not remembered.
	sessionOnly bool
}

// needsPrompt reports whether the folder must be asked about.
func (t trustReport) needsPrompt() bool { return !t.trusted && len(t.items) > 0 }

// assessWorkspaceTrust inspects the session folder.
func assessWorkspaceTrust(env *Env) trustReport {
	cwd := sessionCwd(env)
	rep := trustReport{cwd: cwd, trusted: workspaceTrusted(env)}
	if cwd == "" {
		return rep
	}
	home := canonicalDir(trustHome(env))
	rep.key, _ = trustKey(cwd, home)
	rep.sessionOnly = isHomeOrAncestor(rep.key, home)
	seen := map[string]bool{}
	add := func(kind, line string) {
		rep.items = append(rep.items, line)
		if !seen[kind] {
			seen[kind] = true
			rep.kinds = append(rep.kinds, kind)
		}
	}
	for _, it := range projectSettingsRisks(env) {
		add(it.kind, it.line)
	}
	if !isHomeOrAncestor(canonicalDir(cwd), home) {
		// In the home directory the "project" skill/command/agent folders are
		// the user's own, so only settings are inspected there.
		for _, it := range projectExtensionRisks(cwd) {
			add(it.kind, it.line)
		}
	}
	return rep
}

type trustItem struct{ kind, line string }

// projectSettingsRisks scans the raw project and local settings files.
func projectSettingsRisks(env *Env) []trustItem {
	ctx := settingsContext(env)
	cwd := ctx.Cwd
	var out []trustItem
	for _, src := range []settingspolicy.Source{settingspolicy.SourceProject, settingspolicy.SourceLocal} {
		path, ok := ctx.PathForSource(src)
		if !ok {
			continue
		}
		res := settingsload.LoadFile(fsfacade.OSFS{}, path)
		if res.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		shown := path
		if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
			shown = filepath.ToSlash(rel)
		}
		for _, it := range settingsload.ScanUntrusted(src, res.Settings) {
			out = append(out, trustItem{it.Kind, it.String() + " (" + shown + ")"})
		}
	}
	return out
}

// projectExtensionRisks scans the project's skills, slash commands and agents.
func projectExtensionRisks(cwd string) []trustItem {
	var shell, grants, hooks, agents []string
	fsys := fsfacade.OSFS{}
	for _, sc := range []struct {
		root  string
		scope skills.Scope
	}{
		{skills.ProjectSkillsDir(cwd), skills.ScopeProject},
		{skills.ClaudeProjectSkillsDir(cwd), skills.ScopeClaudeProject},
	} {
		for _, d := range skills.DiscoverScope(fsys, sc.root, sc.scope, "") {
			if len(d.Hooks) > 0 {
				hooks = append(hooks, d.Name)
			}
			if len(d.AllowedTools) > 0 {
				grants = append(grants, d.Name+" ("+strings.Join(clipNames(d.AllowedTools, 3), ", ")+")")
			}
			if data, err := os.ReadFile(d.Path); err == nil && hasShellBlocks(string(data)) {
				shell = append(shell, d.Name)
			}
		}
	}
	for _, cd := range customCommandDirs("", "", cwd) {
		if cd.src != commands.SourceProject && cd.src != commands.SourceLocal {
			continue
		}
		cmds, _ := commands.LoadDir(cd.dir, cd.src)
		for _, c := range cmds {
			if len(c.AllowedTools) > 0 {
				grants = append(grants, "/"+c.Name+" ("+strings.Join(clipNames(c.AllowedTools, 3), ", ")+")")
			}
			if hasShellBlocks(c.Body) {
				shell = append(shell, "/"+c.Name)
			}
		}
	}
	root := filepath.Join(cwd, ".gocode", "agents")
	for _, d := range agentsdef.DiscoverScope(fsys, root, agentsdef.SourceProject, root, false) {
		if settingsload.IsLoosePermissionMode(d.PermissionMode) {
			agents = append(agents, d.AgentType+" ("+d.PermissionMode+")")
		}
	}
	var out []trustItem
	if len(shell) > 0 {
		out = append(out, trustItem{"skill/command shell blocks", "skills/commands with ! shell blocks: " + strings.Join(clipNames(shell, 5), ", ")})
	}
	if len(grants) > 0 {
		out = append(out, trustItem{"skill/command allowed-tools", "skills/commands with allowed-tools: " + strings.Join(clipNames(grants, 4), ", ")})
	}
	if len(hooks) > 0 {
		out = append(out, trustItem{"skill hooks", "skills with hooks: " + strings.Join(clipNames(hooks, 5), ", ")})
	}
	if len(agents) > 0 {
		out = append(out, trustItem{"agent permission modes", "agents with permissionMode: " + strings.Join(clipNames(agents, 4), ", ")})
	}
	return out
}

// hasShellBlocks reports whether a skill/command body holds a `!` block.
func hasShellBlocks(body string) bool {
	if !strings.Contains(body, "!") {
		return false
	}
	return commands.ReplaceShellBlocks(body, "\x00") != body
}

func clipNames(l []string, max int) []string {
	l = append([]string(nil), l...)
	sort.Strings(l)
	if len(l) > max {
		return append(l[:max:max], fmt.Sprintf("+%d more", len(l)-max))
	}
	return l
}

// untrustedNotice is the one line shown when an untrusted folder's settings
// were ignored.
func (t trustReport) untrustedNotice() string {
	return "This folder is not trusted, so its settings that run code or widen permissions were ignored for this session (" +
		strings.Join(t.kinds, ", ") + "). To trust it, run `gocode trust` in this folder and start a new session."
}

// --- git-tracked settings.local.json --------------------------------------

var localTrackedCache sync.Map // settings.local.json path → bool

// localSettingsTracked reports whether ctx's settings.local.json is committed
// to git (`git ls-files --error-unmatch -- ":(icase).gocode/
// settings.local.json"`, hooks disabled via core.hooksPath=/dev/null and the
// hardened git environment). The probe runs once per file per process and only
// when the file exists.
func localSettingsTracked(ctx *settingsload.Context) bool {
	path, ok := ctx.PathForSource(settingspolicy.SourceLocal)
	if !ok {
		return false
	}
	if _, err := os.Lstat(path); err != nil {
		return false
	}
	if v, ok := localTrackedCache.Load(path); ok {
		return v.(bool)
	}
	tracked := gitTracksLocalSettings(filepath.Dir(filepath.Dir(path)))
	localTrackedCache.Store(path, tracked)
	return tracked
}

func gitTracksLocalSettings(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := git.NewRunner("", osEnvMap())
	res, err := r.Run(ctx, dir, []string{"-C", dir, "ls-files", "--error-unmatch", "--", ":(icase).gocode/settings.local.json"},
		envtrust.Env{"GIT_LITERAL_PATHSPECS": "0", "LC_ALL": "C", "GIT_TRACE2": "0", "GIT_TRACE2_PERF": "0", "GIT_TRACE2_EVENT": "0"})
	if err != nil || res == nil {
		return false
	}
	return res.Code == 0
}

// trackedLocalWarning is shown when settings.local.json is committed.
const trackedLocalWarning = "Warning: .gocode/settings.local.json is tracked by git, so it is treated as project settings: its MCP servers need approval and its MCP approvals are ignored. Untrack it (git rm --cached .gocode/settings.local.json) and add it to .gitignore."

// --- interactive dialog text ----------------------------------------------

const (
	trustChoiceYes = "trust"
	trustChoiceNo  = "ignore"
)

// trustDialogBody is the dialog text: the trust question plus the list of
// what the folder would apply.
func (t trustReport) dialogBody() []string {
	body := []string{
		"Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.",
		"",
		"This folder's settings would run code or widen permissions:",
	}
	for _, it := range t.items {
		body = append(body, "  • "+it)
	}
	body = append(body, "", "If you choose No, gocode still works here but ignores these settings for this session.")
	if t.sessionOnly {
		body = append(body, "This is your home directory (or above it): a Yes applies to this session only.")
	}
	return body
}

// untrustedSkillHooks drops the frontmatter hooks of an untrusted folder's
// project skills; every other skill's hooks register as usual.
type untrustedSkillHooks struct {
	inner agenttool.SkillHooksRegistrar
	gate  *allowedToolsGate
}

func (u untrustedSkillHooks) AddSkillHooks(skill, skillRoot string, raw map[string]any, managed bool) (int, error) {
	if u.gate.projectSkill(skill) {
		return 0, nil
	}
	return u.inner.AddSkillHooks(skill, skillRoot, raw, managed)
}
