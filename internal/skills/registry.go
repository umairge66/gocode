package skills

import (
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// Registry holds the resolved set of skills, one per canonical name, after
// applying scope precedence to collisions.
type Registry struct {
	byName map[string]Descriptor
	// Conflicts records names that lost a precedence collision, in the order
	// they were rejected (useful for the loader's warning output).
	Conflicts []Conflict
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

// listableLine returns the "- name: description (scope)" line for the listing,
// or ("", false) when the skill must not appear (model invocation disabled).
func (d *Descriptor) listableLine() (string, bool) {
	if d.DisableModelInvocation {
		return "", false
	}
	return "- " + d.Name + ": " + d.Description + d.Scope.listingSuffix(), true
}

// Listing renders the "available skills" block the model sees: one
// "- name: description (scope)" line per model-invocable skill, newline-joined.
// The returned string is the content placed after the fixed header
// "The following skills are available for use with the Skill tool:".
func (r *Registry) Listing() string {
	var lines []string
	for _, d := range r.List() {
		if line, ok := d.listableLine(); ok {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
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
	// Cwd is the project root (project scope). Empty disables it.
	Cwd string
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
	if cfg.Cwd != "" {
		reg.AddAll(DiscoverScope(fsys, ProjectSkillsDir(cfg.Cwd), ScopeProject, ""))
		reg.AddAll(DiscoverScope(fsys, ClaudeProjectSkillsDir(cfg.Cwd), ScopeClaudeProject, ""))
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
