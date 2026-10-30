package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/skills"
)

// This file decides whether a skill's or slash command's `allowed-tools`
// frontmatter may pre-approve tools. Under the admin policy's
// allowManagedPermissionRulesOnly only managed rules count: allowed-tools of a
// skill or command that is not itself managed are not applied as temporary
// allow rules (their tool calls are checked like any other call, and their
// `!`-blocks run only if the session's own rules already allow them). Their
// `disallowed-tools` still apply — this only ever removes allows.

// allowedToolsGate applies the rule above and remembers what it withheld.
type allowedToolsGate struct {
	mu       sync.Mutex
	withheld []string
	// stderr, when set (non-interactive text output), receives an error line
	// once per command.
	stderr io.Writer
	// skills resolves a skill's scope by name (the session skill registry).
	skills *skills.Registry
	// untrustedProject withholds the allowed-tools of project skills and
	// commands, and their `!`-blocks, because the folder is not trusted
	// (trust.go).
	untrustedProject bool
}

// projectSkill reports whether the skill named name is a project skill of an
// untrusted folder (its grants, `!`-blocks and hooks are withheld).
func (g *allowedToolsGate) projectSkill(name string) bool {
	if g == nil || !g.untrustedProject || g.skills == nil {
		return false
	}
	d, ok := g.skills.Lookup(strings.TrimPrefix(strings.TrimSpace(name), "/"))
	return ok && (d.Scope == skills.ScopeProject || d.Scope == skills.ScopeClaudeProject)
}

// projectCommand reports whether a command from src belongs to an untrusted
// folder.
func (g *allowedToolsGate) projectCommand(src commands.Source) bool {
	return g != nil && g.untrustedProject && (src == commands.SourceProject || src == commands.SourceLocal)
}

// grant returns the allowed-tools a command named name (from source, a label
// such as "user" or "project") may apply: all of them, unless managedOnly is
// set and the command is not managed, in which case none (with the notice).
func (g *allowedToolsGate) grant(managedOnly bool, name, source string, managed bool, tools []string) []string {
	if len(tools) == 0 || !managedOnly || managed {
		return tools
	}
	if g == nil {
		return nil
	}
	label := "/" + name + " (" + source + ")"
	g.mu.Lock()
	seen := contains(g.withheld, label)
	if !seen {
		g.withheld = append(g.withheld, label)
	}
	w := g.stderr
	g.mu.Unlock()
	if !seen && w != nil {
		fmt.Fprintf(w, "allowed-tools from %s ignored: your organization restricts permission rules to managed settings (allowManagedPermissionRulesOnly). Ask an admin to add the rules it needs to managed settings.\n", label)
	}
	return nil
}

// Withheld lists the commands whose allowed-tools were ignored this session
// , for /status.
func (g *allowedToolsGate) Withheld() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.withheld...)
}

// grantSkill is grant for the skill named name, looked up in the session
// skill registry for its scope (a skill that cannot be found is not managed).
func (g *allowedToolsGate) grantSkill(managedOnly bool, name string, tools []string) []string {
	if len(tools) == 0 || !managedOnly {
		return tools
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	var d skills.Descriptor
	found := false
	if g != nil && g.skills != nil {
		d, found = g.skills.Lookup(name)
	}
	if !found {
		return g.grant(managedOnly, name, "unknown", false, tools)
	}
	return g.grant(managedOnly, name, skillSourceLabel(d), skillScopeManaged(d.Scope), tools)
}

// grantCommand is grant for a custom slash command's `!`-block allowed-tools.
func (g *allowedToolsGate) grantCommand(managedOnly bool, inv commands.ShellInvocation) []string {
	if len(inv.AllowedTools) == 0 {
		return inv.AllowedTools
	}
	if (inv.Source == "" && g.projectSkill(inv.Name)) || g.projectCommand(inv.Source) {
		return nil // untrusted folder: no grants from its skills/commands
	}
	if !managedOnly {
		return inv.AllowedTools
	}
	if inv.Source == "" {
		// A skill's blocks carry no command source: look the skill up.
		return g.grantSkill(managedOnly, inv.Name, inv.AllowedTools)
	}
	managed := inv.Source == commands.SourceBuiltin || inv.Source == commands.SourceBundled
	return g.grant(managedOnly, inv.Name, commandSourceLabel(inv.Source), managed, inv.AllowedTools)
}

// skillScopeManaged reports the managed skill scopes: managed-settings and
// bundled skills.
func skillScopeManaged(s skills.Scope) bool {
	return s == skills.ScopeManaged || s == skills.ScopeBuiltin
}

// skillSourceLabel is the source label for a skill's scope.
func skillSourceLabel(d skills.Descriptor) string {
	switch d.Scope {
	case skills.ScopeManaged:
		return "managed"
	case skills.ScopeProject, skills.ScopeClaudeProject:
		return "project"
	case skills.ScopePersonal, skills.ScopeClaudePersonal:
		return "user"
	case skills.ScopeBuiltin:
		return "bundled"
	case skills.ScopePlugin:
		return "plugin"
	}
	return d.Scope.String()
}

// commandSourceLabel is the source label for a command source.
func commandSourceLabel(s commands.Source) string {
	switch s {
	case commands.SourceUser:
		return "user"
	case commands.SourceProject:
		return "project"
	case commands.SourceLocal:
		return "project, gitignored"
	}
	return string(s)
}

// managedRulesStatusLines is the /status "Permission rules" entry ("Managed
// settings only (allowManagedPermissionRulesOnly)" plus the allowed-tools
// ignored this session).
func managedRulesStatusLines(managedOnly bool, g *allowedToolsGate) []string {
	if !managedOnly {
		return nil
	}
	out := []string{"Managed settings only (allowManagedPermissionRulesOnly)"}
	if w := g.Withheld(); len(w) > 0 {
		out = append(out, "allowed-tools ignored this session: "+strings.Join(w, ", "))
	}
	return out
}
