package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// `gocode agents [--json] [--setting-sources user,project]`: list the agent
// definitions a session started here would have — built-in, plugin, user
// (<gocode root>/agents) and project (.gocode/agents) — after the name-collision
// precedence, and which definitions are shadowed.
//
// The dispatch flags (--permission-mode, --model, --effort, --agent,
// --add-dir, --mcp-config, …) would configure sessions dispatched from an
// interactive view; with no view they are accepted and ignored.

// agentSourceLabel returns the display label of an agent source.
func agentSourceLabel(s agentsdef.Source) string {
	switch s {
	case agentsdef.SourceUser:
		return "User"
	case agentsdef.SourceProject:
		return "Project"
	case agentsdef.SourceFlag:
		return "Flag"
	case agentsdef.SourcePolicy:
		return "Managed"
	case agentsdef.SourcePlugin:
		return "Plugin"
	case agentsdef.SourceBuiltin:
		return "Built-in"
	}
	return string(s)
}

// agentListEntry is one --json row.
type agentListEntry struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Source       string   `json:"source"`
	Model        string   `json:"model,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	Path         string   `json:"path,omitempty"`
	Active       bool     `json:"active"`
	OverriddenBy string   `json:"overriddenBy,omitempty"`
}

// agentListing is the resolved listing: active definitions and the shadowed
// ones.
type agentListing struct {
	active   []agentsdef.Descriptor
	shadowed []agentsdef.Descriptor
}

// loadAgentListing discovers the definitions in precedence order: built-in,
// plugin, user, project.
func loadAgentListing(env *Env, sources map[string]bool) agentListing {
	fsys := fsfacade.OSFS{}
	paths := resolveSessionPaths(env)
	var all []agentsdef.Descriptor
	all = append(all, agentsdef.BuiltinAgents()...)

	ordered := settingsload.EffectiveSources(fsys, settingsContext(env), settingsload.CanonicalSourceOrder)
	if reg, _, _ := discoverPlugins(env, &printSession{}, paths, ordered); reg != nil {
		for _, p := range reg.Enabled() {
			if dir := p.Contributions(fsys).AgentsDir; dir != "" {
				all = append(all, agentsdef.DiscoverScope(fsys, dir, agentsdef.SourcePlugin, dir, false)...)
			}
		}
	}
	if sources["user"] {
		if dir := userAgentsDir(paths.configDir); dir != "" {
			all = append(all, agentsdef.DiscoverScope(fsys, dir, agentsdef.SourceUser, dir, false)...)
		}
	}
	if sources["project"] && paths.cwd != "" {
		dir := filepath.Join(paths.cwd, ".gocode", "agents")
		all = append(all, agentsdef.DiscoverScope(fsys, dir, agentsdef.SourceProject, dir, false)...)
	}

	reg := agentsdef.NewRegistry()
	reg.AddAll(all)
	l := agentListing{active: reg.List()}
	winner := map[string]agentsdef.Descriptor{}
	for _, d := range l.active {
		winner[d.AgentType] = d
	}
	for _, d := range all {
		w := winner[d.AgentType]
		if w.Source == d.Source && w.FilePath == d.FilePath {
			continue
		}
		l.shadowed = append(l.shadowed, d)
	}
	return l
}

// parseSettingSources reads --setting-sources ("user,project,local"); unset
// means all.
func parseSettingSources(v string, set bool) (map[string]bool, error) {
	out := map[string]bool{"user": true, "project": true, "local": true}
	if !set {
		return out, nil
	}
	srcs, err := settingsload.ParseSettingSources(v)
	if err != nil {
		return nil, err
	}
	out = map[string]bool{}
	for _, s := range srcs {
		switch s {
		case settingspolicy.SourceUser:
			out["user"] = true
		case settingspolicy.SourceProject:
			out["project"] = true
		case settingspolicy.SourceLocal:
			out["local"] = true
		}
	}
	return out, nil
}

// runAgentsCmd is the command action.
func runAgentsCmd(env *Env, t []string) int {
	// `agents pending` / `agents respond`: the prompts background sessions
	// wait on (bgrespond.go).
	if rest := argsAfter(t, "agents"); len(rest) > 0 {
		switch rest[0] {
		case "pending":
			return runAgentsPending(env, rest[1:])
		case "respond":
			return runAgentsRespond(env, rest[1:])
		}
	}
	o, err := scanOpts(argsAfter(t, "agents"),
		map[string]string{"--json": "json", "--all": "all", "--definitions": "definitions", "--strict-mcp-config": "-", "--restricted": "-", "--dangerously-skip-permissions": "-", "--allow-dangerously-skip-permissions": "-"},
		map[string]string{"--setting-sources": "settingSources", "--cwd": "cwd", "--add-dir": "-", "--plugin-dir": "-", "--plugin-dir-no-mcp": "-", "--settings": "-", "--managed-settings": "-", "--mcp-config": "-", "--permission-mode": "-", "--inherit-permission-mode": "-", "--model": "-", "--effort": "-", "--agent": "-"})
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if len(o.pos) > 0 {
		fmt.Fprintf(env.Stderr, "error: too many arguments for 'agents'. Expected 0 arguments but got %d.\n", len(o.pos))
		return 1
	}
	if len(o.unknown) > 0 {
		fmt.Fprintf(env.Stderr, "error: unknown option '%s'\n", o.unknown[0])
		return 1
	}
	// Background sessions are what `gocode agents` lists (the --bg hint:
	// "gocode agents   list sessions"), printed as a table. Agent definitions
	// are listed with --definitions.
	if !o.has("definitions") {
		cwd, _ := o.value("cwd")
		if o.has("json") {
			return runAgentsJSON(env, cwd, o.has("all"))
		}
		return runAgentsTable(env, cwd, o.has("all"))
	}
	v, set := o.value("settingSources")
	sources, err := parseSettingSources(v, set)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	l := loadAgentListing(env, sources)
	if o.has("json") {
		rows := []agentListEntry{}
		for _, d := range l.active {
			rows = append(rows, agentEntry(d, true, ""))
		}
		won := map[string]agentsdef.Source{}
		for _, d := range l.active {
			won[d.AgentType] = d.Source
		}
		for _, d := range l.shadowed {
			rows = append(rows, agentEntry(d, false, string(won[d.AgentType])))
		}
		data, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintf(env.Stdout, "%s\n", data)
		return 0
	}
	fmt.Fprint(env.Stdout, renderAgentListing(l))
	return 0
}

func agentEntry(d agentsdef.Descriptor, active bool, by string) agentListEntry {
	return agentListEntry{Name: d.AgentType, Description: d.WhenToUse, Source: string(d.Source), Model: d.Model, Tools: d.Tools, Path: d.FilePath, Active: active, OverriddenBy: by}
}

// renderAgentListing prints the definitions grouped by source, highest
// precedence first.
func renderAgentListing(l agentListing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d active %s\n", len(l.active), pluralize(len(l.active), "agent", "agents"))
	order := []agentsdef.Source{agentsdef.SourcePolicy, agentsdef.SourceFlag, agentsdef.SourceProject, agentsdef.SourceUser, agentsdef.SourcePlugin, agentsdef.SourceBuiltin}
	for _, src := range order {
		var group []agentsdef.Descriptor
		for _, d := range l.active {
			if d.Source == src {
				group = append(group, d)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s agents:\n", agentSourceLabel(src))
		for _, d := range group {
			model := d.Model
			if model == "" {
				model = "inherit"
			}
			fmt.Fprintf(&b, "  %s · %s\n", d.AgentType, model)
			if d.FilePath != "" {
				fmt.Fprintf(&b, "    %s\n", d.FilePath)
			}
		}
	}
	if len(l.shadowed) > 0 {
		won := map[string]agentsdef.Source{}
		for _, d := range l.active {
			won[d.AgentType] = d.Source
		}
		b.WriteString("\nShadowed (a higher-precedence definition with the same name wins):\n")
		for _, d := range l.shadowed {
			where := agentSourceLabel(d.Source)
			if d.FilePath != "" {
				where += ", " + d.FilePath
			}
			fmt.Fprintf(&b, "  %s (%s) — overridden by %s\n", d.AgentType, where, strings.ToLower(agentSourceLabel(won[d.AgentType])))
		}
	}
	return b.String()
}
