package agentsdef

import (
	"path"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// Registry is the resolved set of agent definitions, one per agent type, after
// applying scope precedence to name collisions. It merges built-ins with
// discovered (file/JSON/plugin) definitions.
type Registry struct {
	byType map[string]Descriptor
	// order preserves insertion order for stable collision reporting.
	order []string
	// Conflicts records types that lost a precedence collision.
	Conflicts []Conflict
}

// Conflict describes an agent that did not become active because a
// higher-precedence agent already claimed its type.
type Conflict struct {
	AgentType string
	Rejected  Source
	WonBy     Source
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byType: map[string]Descriptor{}}
}

// Add inserts a descriptor, honoring source precedence: a descriptor whose
// source has precedence >= the incumbent's wins (equal precedence => the new
// one wins, as in a map-overwrite where later entries replace earlier ones).
// The loser is recorded in Conflicts.
//
// To get the documented precedence, callers add descriptors in the source order
// [built-in, plugin, userSettings, projectSettings(additional then main),
// flagSettings, policySettings]; BuildRegistry does this.
func (r *Registry) Add(d Descriptor) {
	incumbent, ok := r.byType[d.AgentType]
	if !ok {
		r.byType[d.AgentType] = d
		r.order = append(r.order, d.AgentType)
		return
	}
	if d.Source.precedence() >= incumbent.Source.precedence() {
		r.Conflicts = append(r.Conflicts, Conflict{
			AgentType: incumbent.AgentType,
			Rejected:  incumbent.Source,
			WonBy:     d.Source,
		})
		r.byType[d.AgentType] = d
		return
	}
	r.Conflicts = append(r.Conflicts, Conflict{
		AgentType: d.AgentType,
		Rejected:  d.Source,
		WonBy:     incumbent.Source,
	})
}

// AddAll inserts many descriptors in order.
func (r *Registry) AddAll(ds []Descriptor) {
	for _, d := range ds {
		r.Add(d)
	}
}

// Lookup returns the active descriptor for an agent type.
func (r *Registry) Lookup(agentType string) (Descriptor, bool) {
	d, ok := r.byType[agentType]
	return d, ok
}

// List returns the active descriptors sorted by agent type.
func (r *Registry) List() []Descriptor {
	out := make([]Descriptor, 0, len(r.byType))
	for _, t := range r.order {
		out = append(out, r.byType[t])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentType < out[j].AgentType })
	return out
}

// Listing renders the "{name}: {description}" projection the agent listing uses.
func (r *Registry) Listing() string {
	var b strings.Builder
	for i, d := range r.List() {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(d.AgentType)
		b.WriteString(": ")
		b.WriteString(d.WhenToUse)
	}
	return b.String()
}

// LoadOptions describes where to discover file-defined agents. Each directory
// is a *root* under which the loader looks for a `.gocode/agents` subtree,
// except UserAgentsDir which is already the user's agents directory.
type LoadOptions struct {
	// UserAgentsDir is the user's agents directory.
	UserAgentsDir string
	// ProjectDir is the project root; its <ProjectDir>/.gocode/agents is
	// searched at SourceProject.
	ProjectDir string
	// AdditionalDirs are extra roots; each <dir>/.gocode/agents is searched at
	// SourceProject with FromAdditionalDirectory set.
	AdditionalDirs []string
	// ManagedDir is the managed/policy root; its <ManagedDir>/.gocode/agents is
	// searched at SourcePolicy.
	ManagedDir string
	// AgentsJSON, when non-nil, is an SDK `agents` object parsed at SourceFlag.
	AgentsJSON []byte
	// IncludeBuiltins controls whether BuiltinAgents() are merged in.
	IncludeBuiltins bool
	// IgnoreProjectPermissionMode clears a loosening permissionMode
	// (acceptEdits, bypassPermissions, auto) from the project agents: the
	// folder has not been trusted (cli workspace trust).
	IgnoreProjectPermissionMode bool
}

// loosePermissionModes are the agent permission modes an untrusted project may
// not grant.
var loosePermissionModes = map[string]bool{"acceptEdits": true, "bypassPermissions": true, "auto": true}

// projectScope discovers a project agents root, honouring
// IgnoreProjectPermissionMode.
func (o LoadOptions) projectScope(fsys fsfacade.FS, root string, fromAdditional bool) []Descriptor {
	ds := DiscoverScope(fsys, root, SourceProject, root, fromAdditional)
	if o.IgnoreProjectPermissionMode {
		for i := range ds {
			if loosePermissionModes[ds[i].PermissionMode] {
				ds[i].PermissionMode = ""
			}
		}
	}
	return ds
}

// Load discovers all file/JSON/built-in agent definitions and resolves them
// into a Registry. Plugin agents are sourced separately at runtime and are not
// discovered here.
//
// Plugin agent sourcing
// is done by conversation.LoadPlugins, which discovers each enabled plugin's
// agents directory (DiscoverScope at SourcePlugin) and merges it via
// Registry.AddAll.
func Load(fsys fsfacade.FS, opts LoadOptions) *Registry {
	reg := NewRegistry()

	// Order: built-in, plugin(—), userSettings, projectSettings
	// (additional dirs first, then the main project), flagSettings,
	// policySettings — later additions overwrite on equal/higher precedence.
	if opts.IncludeBuiltins {
		reg.AddAll(BuiltinAgents())
	}

	// userSettings
	if opts.UserAgentsDir != "" {
		reg.AddAll(DiscoverScope(fsys, opts.UserAgentsDir, SourceUser, opts.UserAgentsDir, false))
	}

	// projectSettings — added directories first (lower effective precedence on
	// equal-name collisions, since the main project is added afterward).
	for _, dir := range opts.AdditionalDirs {
		root := path.Join(dir, gocodeDirName, agentsDirName)
		reg.AddAll(opts.projectScope(fsys, root, true))
	}
	if opts.ProjectDir != "" {
		root := path.Join(opts.ProjectDir, gocodeDirName, agentsDirName)
		reg.AddAll(opts.projectScope(fsys, root, false))
	}

	// flagSettings (SDK agents object)
	if opts.AgentsJSON != nil {
		descs, _ := ParseAgentsJSON(opts.AgentsJSON)
		reg.AddAll(descs)
	}

	// policySettings (managed)
	if opts.ManagedDir != "" {
		root := path.Join(opts.ManagedDir, gocodeDirName, agentsDirName)
		reg.AddAll(DiscoverScope(fsys, root, SourcePolicy, root, false))
	}

	return reg
}

// DiscoverScope walks an agents root directory recursively, parsing every
// `.md` file into a descriptor at the given source. Files that fail name or
// description validation are skipped. baseDir is recorded on each descriptor as
// the trust/base directory.
func DiscoverScope(fsys fsfacade.FS, root string, source Source, baseDir string, fromAdditional bool) []Descriptor {
	var out []Descriptor
	visited := map[string]bool{}

	var walk func(dir string)
	walk = func(dir string) {
		if visited[dir] {
			return // circular symlink guard
		}
		visited[dir] = true
		entries, err := fsys.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := path.Join(dir, e.Name())
			if e.IsDir() {
				walk(full)
				continue
			}
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := fsys.ReadFile(full)
			if err != nil || len(data) > maxDefinitionBytes {
				continue
			}
			pr := ParseDefinition(data, full)
			d, msg := BuildFileDescriptor(pr, full, baseDir, source)
			if msg != "" {
				continue
			}
			d.FromAdditionalDirectory = fromAdditional
			out = append(out, d)
		}
	}
	walk(root)

	// Stable order within a scope.
	sort.Slice(out, func(i, j int) bool { return out[i].AgentType < out[j].AgentType })
	return out
}
