package skills

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/projectmemory"
)

// Registry holds the resolved set of skills, one per canonical name, after
// applying scope precedence to collisions.
type Registry struct {
	byName map[string]Descriptor
	// Conflicts records names that lost a precedence collision, in the order
	// they were rejected (useful for the loader's warning output).
	Conflicts []Conflict
	// Denied, when set, hides a skill from the listing (the session's
	// Skill(<name>) deny rules): the model would be refused anyway.
	Denied func(name string) bool

	// mu guards activated: the conditional (`paths:`) skills a touched file
	// has matched, by name.
	mu        sync.Mutex
	activated map[string]bool
}

// Conflict describes a skill that was not loaded because a higher-precedence
// skill already claimed its name.
type Conflict struct {
	Name       string
	Rejected   Scope
	WonBy      Scope
	RejectedID string // unqualified identifier of the rejected skill
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Descriptor{}}
}

// Add inserts a descriptor, honoring scope precedence: a skill with a
// lower precedence rank (see Scope.precedence) wins a name collision. A
// losing insert is recorded in Conflicts and dropped.
func (r *Registry) Add(d Descriptor) {
	existing, ok := r.byName[d.Name]
	if !ok {
		r.byName[d.Name] = d
		return
	}
	if d.Scope.precedence() < existing.Scope.precedence() {
		// New skill wins; the previously registered one is now the conflict.
		r.Conflicts = append(r.Conflicts, Conflict{
			Name:       existing.Name,
			Rejected:   existing.Scope,
			WonBy:      d.Scope,
			RejectedID: existing.UnqualifiedName,
		})
		r.byName[d.Name] = d
		return
	}
	// Existing wins (higher or equal precedence).
	r.Conflicts = append(r.Conflicts, Conflict{
		Name:       d.Name,
		Rejected:   d.Scope,
		WonBy:      existing.Scope,
		RejectedID: d.UnqualifiedName,
	})
}

// AddAll inserts many descriptors in order.
func (r *Registry) AddAll(ds []Descriptor) {
	for _, d := range ds {
		r.Add(d)
	}
}

// Lookup returns the descriptor registered under name, trying the exact
// (possibly namespaced) name first and then the bare/unqualified name.
func (r *Registry) Lookup(name string) (Descriptor, bool) {
	if d, ok := r.byName[name]; ok {
		return d, true
	}
	for _, d := range r.byName {
		if d.UnqualifiedName == name {
			return d, true
		}
	}
	return Descriptor{}, false
}

// List returns all resolved descriptors sorted by precedence, then name.
func (r *Registry) List() []Descriptor {
	out := make([]Descriptor, 0, len(r.byName))
	for _, d := range r.byName {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope.precedence() < out[j].Scope.precedence()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// listableLine returns the "- name: description - when_to_use (scope)" line
// for the listing, or ("", false) when the skill must not appear (model
// invocation disabled).
func (d *Descriptor) listableLine() (string, bool) {
	if d.DisableModelInvocation {
		return "", false
	}
	desc := d.Description
	if w := strings.TrimSpace(d.WhenToUse); w != "" {
		if desc == "" {
			desc = w
		} else {
			desc += " - " + w
		}
	}
	return "- " + d.Name + ": " + desc + d.Scope.listingSuffix(), true
}

// Listing renders the "available skills" block the model sees: one
// "- name: description (scope)" line per model-invocable skill, newline-joined.
// A conditional (`paths:`) skill is listed only once ActivateForPath matched
// it, and a Denied skill not at all. The returned string is the content
// placed after the fixed header
// "The following skills are available for use with the Skill tool:".
func (r *Registry) Listing() string {
	var lines []string
	for _, d := range r.List() {
		if len(d.Paths) > 0 && !r.isActivated(d.Name) {
			continue
		}
		if r.Denied != nil && r.Denied(d.Name) {
			continue
		}
		if line, ok := d.listableLine(); ok {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func (r *Registry) isActivated(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activated[name]
}

// ActivateForPath activates the conditional skills whose `paths:` globs match
// target (an absolute path the agent read, edited or wrote) and returns the
// names newly activated. A project skill's globs are relative to the directory
// that holds its .gocode/.claude folder; any other skill's to cwd. Safe for
// concurrent use.
func (r *Registry) ActivateForPath(target, cwd string) []string {
	if r == nil || target == "" {
		return nil
	}
	target = path.Clean(strings.ReplaceAll(target, "\\", "/"))
	cwd = strings.ReplaceAll(cwd, "\\", "/")
	var out []string
	for _, d := range r.List() {
		if len(d.Paths) == 0 {
			continue
		}
		base := cwd
		if d.Scope == ScopeProject || d.Scope == ScopeClaudeProject {
			// <dir>/.gocode/skills/<name> → <dir>
			base = path.Dir(path.Dir(path.Dir(strings.ReplaceAll(d.BaseDir, "\\", "/"))))
		}
		if base == "" || !projectmemory.PathsMatch(d.Paths, base, target) {
			continue
		}
		r.mu.Lock()
		if !r.activated[d.Name] {
			if r.activated == nil {
				r.activated = map[string]bool{}
			}
			r.activated[d.Name] = true
			out = append(out, d.Name)
		}
		r.mu.Unlock()
	}
	return out
}

// projectSkillDirs lists the directories project skills are read from,
// nearest first: cwd, then each parent up to and including root when cwd is
// inside root; otherwise up to the filesystem root. The home directory is
// never a parent project (its ~/.claude/skills is the user tier), though a
// cwd that is home still reads its own skills.
func projectSkillDirs(cwd, root, home string) []string {
	if cwd == "" {
		return nil
	}
	cwd = filepath.Clean(cwd)
	if root != "" {
		root = filepath.Clean(root)
		if rel, err := filepath.Rel(root, cwd); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			root = ""
		}
	}
	if home != "" {
		home = filepath.Clean(home)
	}
	dirs := []string{cwd}
	if cwd == home {
		return dirs
	}
	for d := cwd; d != root; {
		parent := filepath.Dir(d)
		if parent == d || parent == home {
			break
		}
		dirs = append(dirs, parent)
		d = parent
	}
	return dirs
}

// ListingHeader is the fixed preamble the content is appended to.
const ListingHeader = "The following skills are available for use with the Skill tool:"

// LoadConfig describes where to look for skills across scopes.
type LoadConfig struct {
	// UserDir is the gocode root (internal/confighome); personal skills live
	// in <UserDir>/skills. Empty disables that scope.
	UserDir string
	// Home is the user's home directory, for the ~/.claude/skills scope.
	// Empty disables it.
	Home string
	// Cwd is the working directory (project scope). Empty disables it.
	Cwd string
	// ProjectRoot bounds the project-scope walk: project skills are read from
	// Cwd and each parent directory up to and including ProjectRoot (the git
	// root). When empty, or when Cwd is not inside it, the walk climbs to the
	// filesystem root, stopping below WalkStop (or Home when WalkStop is
	// empty). The nearest directory wins a name collision within a scope.
	ProjectRoot string
	// WalkStop is the directory the project walk never climbs into, normally
	// the home directory (its ~/.claude/skills is the user tier). Empty uses
	// Home.
	WalkStop string
	// PluginRoots maps a plugin ID to its root directory; each contributes
	// <root>/skills discovered under ScopePlugin, namespaced "<id>:<name>".
	PluginRoots map[string]string
	// ManagedRoot, when set, is the admin-managed skills directory
	// (<managed settings dir>/.gocode/skills), discovered under ScopeManaged
	// ahead of every other scope. Empty disables it.
	ManagedRoot string
	// BuiltinRoot, when set, is discovered under ScopeBuiltin.
	BuiltinRoot string
	// SyncedRoot, when set, is discovered under ScopeSynced.
	SyncedRoot string
}

// Load discovers skills across all configured scopes and returns a resolved
// registry. Discovery order is highest-precedence first so that the collision
// bookkeeping records the correct winner.
func Load(fsys fsfacade.FS, cfg LoadConfig) *Registry {
	reg := NewRegistry()

	// Managed (policy) skills come from the managed settings directory's
	// .gocode/skills.
	if cfg.ManagedRoot != "" {
		reg.AddAll(DiscoverScope(fsys, cfg.ManagedRoot, ScopeManaged, ""))
	}

	// Directory scopes, highest precedence first. Within a scope tier the
	// gocode-native .gocode/skills dir is discovered before the Claude Code
	// .claude/skills dir of the same tier, so that on a name collision the
	// gocode skill wins (dedup by scope rank in Registry.Add). Overall
	// directory precedence:
	//
	//	project .gocode > project .claude > user (gocode root) > user .claude
	//
	// The .claude/skills roots are for gocode<->Claude interop: they let
	// gocode discover skills that Claude Code

	// installed, loaded through the exact same DiscoverScope directory loader
	// and per-skill SKILL.md parsing as the native dirs. A missing .claude
	// dir is skipped like any other absent root (DiscoverScope returns nil).
	//
	// The project tier is read from cwd and its parents (projectSkillDirs),
	// nearest first, so a nearer skill wins over a same-named one further up
	// (equal scope rank: the first registered wins).
	stop := cfg.WalkStop
	if stop == "" {
		stop = cfg.Home
	}
	dirs := projectSkillDirs(cfg.Cwd, cfg.ProjectRoot, stop)
	for _, dir := range dirs {
		reg.AddAll(DiscoverScope(fsys, ProjectSkillsDir(dir), ScopeProject, ""))
	}
	for _, dir := range dirs {
		reg.AddAll(DiscoverScope(fsys, ClaudeProjectSkillsDir(dir), ScopeClaudeProject, ""))
	}
	if cfg.UserDir != "" {
		reg.AddAll(DiscoverScope(fsys, PersonalSkillsDir(cfg.UserDir), ScopePersonal, ""))
	}
	if cfg.Home != "" {
		reg.AddAll(DiscoverScope(fsys, ClaudePersonalSkillsDir(cfg.Home), ScopeClaudePersonal, ""))
	}
	if cfg.BuiltinRoot != "" {
		reg.AddAll(DiscoverScope(fsys, cfg.BuiltinRoot, ScopeBuiltin, ""))
	}
	// Plugin roots in deterministic ID order.
	ids := make([]string, 0, len(cfg.PluginRoots))
	for id := range cfg.PluginRoots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		root := cfg.PluginRoots[id]
		reg.AddAll(DiscoverScope(fsys, root+"/"+skillsDirName, ScopePlugin, id))
	}
	if cfg.SyncedRoot != "" {
		reg.AddAll(DiscoverScope(fsys, cfg.SyncedRoot, ScopeSynced, ""))
	}
	return reg
}
