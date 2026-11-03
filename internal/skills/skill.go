// Package skills is gocode "skills loader": how skills
// (invoked via the Skill tool, protocol.ToolSkill == "Skill") are discovered
// on disk, parsed from SKILL.md files, namespaced, de-duplicated by scope
// precedence, and presented to the model as the "available skills" listing.
//
// It covers the skill frontmatter schema, the descriptor builder, the SKILL.md
// discovery/dedup orchestration, the scope/source model, the name slugifier,
// the "available skills" listing render (the `${name}: ${description}` join
// with token budget), the collision/precedence error text, and the YAML
// frontmatter parser with its /^---\s*\n([\s\S]*?)---\s*\n?/ delimiter regex
// and untrusted-input guards (65536-byte size cap). Skill names in the
// "gocode-skills:" namespace are normalized by settingspolicy.NormalizeSkillName.
package skills

// Scope identifies where a skill was discovered. It determines both the
// listing suffix shown to the model and the precedence used to resolve
// name collisions.
type Scope int

const (
	// ScopeManaged is a skill forced by managed/policy settings (source
	// "policySettings"). Locked names take precedence over everything else.
	ScopeManaged Scope = iota
	// ScopeProject is a skill under <cwd>/.gocode/skills (declared via
	// projectSettings/localSettings).
	ScopeProject
	// ScopeClaudeProject is a skill under <cwd>/.claude/skills, installed by
	// Claude Code at the project level (gocode<->Claude interop: reading
	// Claude Code's skills directories). It behaves exactly like a project directory skill but
	// ranks just below ScopeProject so a gocode project skill of the same name
	// takes precedence over Claude's.
	ScopeClaudeProject
	// ScopePersonal is a skill under <gocode root>/skills (declared via
	// userSettings).
	ScopePersonal
	// ScopeClaudePersonal is a skill under ~/.claude/skills, installed by Claude
	// Code at the user level (gocode<->Claude interop); behaves like a
	// personal directory skill but ranks just below
	// ScopePersonal so a gocode user skill of the same name takes precedence.
	ScopeClaudePersonal
	// ScopeBuiltin is a skill bundled with the app (source
	// "builtin"/"bundled"; its listing shows no scope suffix).
	ScopeBuiltin
	// ScopePlugin is a skill contributed by an installed or session plugin.

	ScopePlugin
	// ScopeSynced is a skill synced from the user's gocode.ai account
	// (loadedFrom "syncedSkills"); its names live under the reserved
	// "gocode-skills:" namespace.
	ScopeSynced
)

// precedence returns the collision rank of a scope; lower wins.
func (s Scope) precedence() int { return int(s) }

// String returns the canonical scope token.
func (s Scope) String() string {
	switch s {
	case ScopeManaged:
		return "managed"
	case ScopeProject:
		return "project"
	case ScopeClaudeProject:
		return "claude-project"
	case ScopePersonal:
		return "personal"
	case ScopeClaudePersonal:
		return "claude-personal"
	case ScopeBuiltin:
		return "builtin"
	case ScopePlugin:
		return "plugin"
	case ScopeSynced:
		return "synced"
	default:
		return "unknown"
	}
}

// listingSuffix is the parenthetical appended to a skill's description in the
// "available skills" listing.
func (s Scope) listingSuffix() string {
	switch s {
	case ScopeBuiltin:
		return ""
	case ScopeProject:
		return " (project)"
	case ScopeClaudeProject:
		// gocode<->Claude interop: project skill installed by Claude Code.
		return " (claude project)"
	case ScopePersonal:
		return " (user)"
	case ScopeClaudePersonal:
		// gocode<->Claude interop: user skill installed by Claude Code.
		return " (claude user)"
	case ScopeManaged:
		return " (managed)"
	case ScopePlugin:
		return " (plugin)"
	case ScopeSynced:
		return " (gocode.ai account)"
	default:
		return ""
	}
}

// ExecutionContext selects how an invoked skill runs.
type ExecutionContext int

const (
	// ContextInline expands the skill body into the current conversation.
	ContextInline ExecutionContext = iota
	// ContextFork spawns a subagent that runs the skill and reports back.
	ContextFork
)

// Frontmatter is the parsed, normalized YAML frontmatter of a SKILL.md file.
// Only the fields the loader actually consumes are modeled; "@internal"
// bookkeeping keys (mcpServers, themes, author, homepage, keywords, ...) are
// collapsed into Metadata / DeclaredFields where needed.
type Frontmatter struct {
	// Name is the display name; defaults to the filename without extension.
	Name string
	// Description is the one-line summary shown in listings and the Skill tool.
	Description string
	// Model override: "haiku", "sonnet", "opus", "fable", a full ID, or
	// "inherit" to match the parent conversation.
	Model string
	// AllowedTools: tools available while this file is active. In frontmatter,
	// a comma-separated string or a YAML list; parsed to a slice here.
	AllowedTools []string
	// DisallowedTools: tools removed while this file is active (key
	// "disallowed-tools", canonical alias "disallowedTools").
	DisallowedTools []string
	// ArgumentHint is placeholder text shown after the slash command name.
	ArgumentHint string
	// DisableModelInvocation: if true, the model cannot invoke this via the
	// Skill tool; only users can type the slash command.
	DisableModelInvocation bool
	// UserInvocable: if false, hides the slash command from users; only the
	// model can invoke it via the Skill tool. Defaults to true.
	UserInvocable bool
	// Effort: thinking effort — "low","medium","high","max", or an integer.
	Effort string
	// Shell for !-command blocks: "bash" or "powershell". Defaults to bash.
	Shell string
	// Version: bookkeeping, not surfaced to users.
	Version string
	// WhenToUse: guidance for when the model should reach for this skill;
	// becomes part of the tool description.
	WhenToUse string
	// Paths: glob patterns this skill applies to. The skill only loads when
	// the model touches matching files (conditional/dynamic skills).
	Paths []string
	// Context: where the skill runs ("inline" or "fork"). Empty == inline.
	Context string
	// Agent: agent type to spawn when context == "fork".
	Agent string
	// Background: only for context == "fork". Forks run as background agents
	// that report back as a task notification instead of blocking the turn;
	// set false to keep the caller waiting for the result in-line. Absent
	// means true (see BackgroundSet).
	Background bool
	// BackgroundSet records whether `background` was present, so the
	// default (absent => background) can be
	// distinguished from an explicit false.
	BackgroundSet bool
	// Hooks is the frontmatter `hooks` object (event -> matcher groups, the
	// settings-file shape), registered for the session when the skill is
	// invoked. nil when absent or not an object.
	Hooks map[string]any
	// License: @internal license marker.
	License string
	// Metadata: free-form map for the skill author's own use (e.g. entitlement
	// or catalog fields). Preserved on the loaded skill.
	Metadata map[string]any
	// userInvocableSet records whether `user-invocable` was present, so the
	// default (true) can be distinguished from an explicit false.
	userInvocableSet bool
}

// Descriptor is the loaded, normalized skill as the registry presents it.
type Descriptor struct {
	// Name is the canonical, possibly namespaced identifier used to invoke the
	// skill (e.g. "code-review", "searchfit-seo:seo-audit",
	// "gocode-skills:<synced>"). Derived from the directory name, namespaced
	// by scope; see loader.BuildDescriptor.
	Name string
	// UnqualifiedName is Name with any "<plugin>:"/"gocode-skills:" prefix
	// stripped. Used for bare-name lookup.
	UnqualifiedName string
	// DisplayName is the optional frontmatter `name` (capped at 256 chars),
	// or "" when absent.
	DisplayName string
	// Description is the resolved description (frontmatter, else first body
	// line); the listing appends the scope suffix on top of this.
	Description string
	// HasUserSpecifiedDescription records whether Description came from
	// frontmatter rather than the body fallback.
	HasUserSpecifiedDescription bool
	// AllowedTools / DisallowedTools after parsing.
	AllowedTools    []string
	DisallowedTools []string
	// ArgumentHint, WhenToUse, Version, Model from frontmatter.
	ArgumentHint string
	WhenToUse    string
	Version      string
	Model        string
	// Paths are the frontmatter `paths:` globs: a skill with paths is
	// conditional and is only listed once a matching file was touched
	// (Registry.ActivateForPath). nil for an unconditional skill.
	Paths []string
	// DisableModelInvocation / UserInvocable gating.
	DisableModelInvocation bool
	UserInvocable          bool
	// ExecutionContext selects inline vs fork (subagent).
	ExecutionContext ExecutionContext
	// Agent is the agent type to spawn when ExecutionContext == ContextFork.
	Agent string
	// Background / BackgroundSet: fork runs as a background task notification
	// (absent => background; see RunsInBackground).
	Background    bool
	BackgroundSet bool
	// Hooks is the frontmatter `hooks` object, registered for the session when
	// the skill is invoked (see the invocation model below).
	Hooks map[string]any
	// Effort / Shell passthrough.
	Effort string
	Shell  string
	// Scope is the discovery scope (drives precedence and listing suffix).
	Scope Scope
	// Path is the absolute SKILL.md path; BaseDir is its containing directory
	// (the skill root, substituted for ${GOCODE_SKILL_DIR} in the body).
	Path    string
	BaseDir string
	// PluginRoot and PluginData are a plugin skill's ${…PLUGIN_ROOT} and
	// ${…PLUGIN_DATA} values, substituted in the body on invocation ("" when
	// not a plugin skill or unknown).
	PluginRoot string
	PluginData string
	// Metadata preserves the frontmatter `metadata` map.
	Metadata map[string]any
}

// RunsInSubagent reports whether invoking the skill spawns a subagent rather
// than expanding its body into the current conversation.
func (d *Descriptor) RunsInSubagent() bool { return d.ExecutionContext == ContextFork }

// RunsInBackground reports whether a forked skill asks to run as a background
// agent. The default is true when `background` is absent (the
// host still runs it in the foreground when no background path exists, e.g.
// in -p / non-interactive mode).
func (d *Descriptor) RunsInBackground() bool {
	if !d.BackgroundSet {
		return true
	}
	return d.Background
}

// Invocation model (executed by internal/agenttool):
//
// When the Skill tool invokes a descriptor, the loader reads its BaseDir's
// SKILL.md body (${GOCODE_SKILL_DIR} in the body is
// substituted with BaseDir), registers its frontmatter Hooks for the rest of
// the SESSION (hookhost.Host.AddSkillHooks,
// once per skill, policy-gated), and then either:
//   - ContextInline: expands the body into the current conversation and
//     activates AllowedTools (temporary allow rules), DisallowedTools
//     (temporary deny rules), Model ("inherit" = none) and Effort for the REST
//     OF THE CURRENT TURN. The Model is dropped in auto mode when the model
//     does not support it and, outside the availableModels
//     allowlist, the session model is kept; an integer Effort (a
//     thinking budget) runs at "high" (allowed_tools/model/effort permission
//     layers on the running query; disallowed-tools deny
//     rules reset when the next user message starts a turn). Everything is
//     undone when the turn ends (conversation.DeferTurnEnd): the rule layer is
//     removed (toolrun.ScopedRules — never added to the shared store, never
//     persisted) and the user's model/effort are restored. Subagents spawned
//     during the turn inherit the deny rules but not the allow rules.
//     Invoked BY A SUBAGENT, the same applies to that subagent — its
//     dispatcher and its own model caller — for the rest of its run (the
//     layers fold into the calling agent's context).
//   - ContextFork: spawns a subagent of type Agent (the frontmatter `agent`,
//     here Descriptor.Agent) seeded with the body, on the skill's model (over
//     the agent definition's) and effort, with AllowedTools / DisallowedTools
//     as temporary rules on the subagent's own dispatcher and bare disallowed
//     tool names removed from its tool roster. The agents
//     the fork spawns, and theirs, inherit those rules unchanged (a child's
//     permission state derives from the fork's). Unless
//     `background: false`, the fork runs as a background agent and reports
//     back as a task notification; with no
//     background path (-p), while a background fork of the same skill is still
//     live, or when its scoping record would not validate,
//     it runs in the foreground. The fork runs at the invoker's nesting depth
//     + 1, so the spawn-depth cap
//     (GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH) counts nested forks; its Model
//     outside availableModels falls back to the default subagent model or the
//     parent's; an integer Effort runs at "high"; the scoping record
//     freezes the INVOKING agent's command-scoped deny rules
//     (frozenCommandDenies).
//
// This package discovers, parses and presents skills; execution lives in the
// Skill tool (internal/agenttool/skill.go SkillTool.Invoke/Execute) with the
// session wiring in internal/cli/skillscope.go.
//
// A skill that declares PreToolUse/PermissionRequest outside `hooks` loads with
// no hooks and no allowed-tools. By
// design a background fork's scoping record is validated but not
// written to a sidecar: nothing about a background agent outlives
// the process.
//
// The availableModels / deniedModels check (with modelOverrides remapping, version-prefix
// entries and the policy-load-failure refusal) and the family step-down live in
// internal/cli/modelpolicy.go. Under allowManagedPermissionRulesOnly a non-managed skill's
// allowed-tools are not applied (internal/cli/toolgrants.go).
//
// TODO: the auto-mode check treats only auto (not plan-with-auto-classifier) as
// auto and has no fast-mode breaker; a fork from an agent already at
// the depth cap is refused rather than run in the foreground.
