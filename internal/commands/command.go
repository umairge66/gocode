// Package commands implements the gocode slash-command subsystem: how
// `/command` inputs are parsed, how custom commands are loaded from markdown
// files, how argument placeholders are expanded, and how commands from
// different sources are matched and shadowed.
//
// The pure parsing / loading / matching / argument-expansion layer lives here,
// together with the `!`-shell-block execution (ExpandShellBlocks),
// which runs each block through an injectable ShellRunner and splices its stdout
// into the prompt. Every block is first permission-checked through the
// ShellExecOptions.Authorize gate (a Bash-tool permission check with the
// command's allowed-tools as temporary allow rules); the session wires
// that gate to the Bash tool's decider + shared rule store (cli/wiring.go,
// toolrun.Dispatcher.AuthorizeShellCommand). In auto mode the gate auto-allows
// blocks (the user's choice) after deny/ask rules; a block it does not approve
// but does not deny (e.g. an ask rule) is handed to the model as a "run this
// first" instruction instead of running (ErrShellHandOff,
// shellhandoff.go), so the model
// runs it through its Bash tool under the normal checks; a denied block still
// fails the command. When the disableSkillShellExecution setting is in effect
// no block runs and each is replaced by ShellDisabledPlaceholder
// (ShellExecOptions.ExecutionDisabled, ShellPolicyApplies,
// shellpolicy.go). Built-in command behaviour lives in internal/builtincmds and
// the Skill-tool bridge in internal/agenttool; the gap for prompt commands
// produced in code is tracked in internal/conversation (expandInput).
package commands

import "strings"

// Source identifies where a command came from. Each descriptor carries a
// `source` string; these are the possible values.
type Source string

const (
	// SourceBuiltin is a command compiled into the binary (e.g. /help, /clear,
	// /compact, /model, /doctor). Bundled skills carry
	// source:"bundled"; both are native and are treated as the lowest priority.
	SourceBuiltin Source = "builtin"
	// SourceBundled denotes a skill/command shipped inside the binary and
	// extracted to disk on use.
	SourceBundled Source = "bundled"
	// SourceUser is a command from the user config dir (<gocode root>/commands).
	SourceUser Source = "user"
	// SourceProject is a command from the project (.gocode/commands) — it takes
	// precedence over a user command of the same bare name.
	SourceProject Source = "project"
	// SourceLocal is a project-local (git-ignored) command scope.
	SourceLocal Source = "local"
	// SourcePlugin is a command contributed by an installed plugin.
	SourcePlugin Source = "plugin"
)

// sourcePriority orders sources for bare-name resolution. Higher wins.
var sourcePriority = map[Source]int{
	SourceProject: 5,
	SourceLocal:   4,
	SourceUser:    3,
	SourcePlugin:  2,
	SourceBundled: 1,
	SourceBuiltin: 0,
}

// Command is the "prompt"-type command descriptor.
//
// It unifies the fields set for bundled commands and by the markdown
// descriptor builder. Fields that only
// matter to the live renderer/session are omitted; the ones kept are those that
// drive parsing, matching, listing and expansion.
type Command struct {
	// Type is always "prompt" for slash commands.
	Type string
	// Name is the fully-qualified, namespaced name, e.g. "project:git:commit"
	// for .gocode/commands/git/commit.md, or "help" for a builtin.
	Name string
	// Description is the one-line summary shown in listings and to the model.
	Description string
	// HasUserSpecifiedDescription is true when the description came from the
	// frontmatter rather than being derived from the body's first heading.
	HasUserSpecifiedDescription bool
	// AllowedTools / DisallowedTools gate which tools the model may use while
	// this command's prompt is active.
	AllowedTools    []string
	DisallowedTools []string
	// ArgumentHint is placeholder text shown after the slash command name,
	// e.g. "[file]" or "<instruction>".
	ArgumentHint string
	// ArgNames are named positional placeholders declared via the @internal
	// `arguments` frontmatter key; used by the expander for `$name` placeholders.
	ArgNames []string
	// Model is a model override ("haiku"/"sonnet"/"opus"/"fable"/full id/
	// "inherit"). Empty means use the session default.
	Model string
	// Effort is the thinking-effort override ("low"/"medium"/"high"/"max"/int).
	Effort string
	// Shell selects the interpreter for `!`-command blocks: "bash" (default) or
	// "powershell".
	Shell string
	// ShellExplicit is true when the frontmatter set `shell:` (to a recognised
	// value). This distinguishes an explicit `shell: bash` (fails on a
	// Windows host without Git Bash) from no shell (falls back to PowerShell).
	ShellExplicit bool
	// Warnings are non-fatal load diagnostics (frontmatter parse failure,
	// invalid effort/shell values, duplicate files), each prefixed with the
	// file path. A host may log or surface them.
	Warnings []string
	// WhenToUse is model-facing guidance (frontmatter when_to_use).
	WhenToUse string
	// Version is @internal bookkeeping (frontmatter version).
	Version string
	// DisableModelInvocation hides the command from the Skill tool: only a user
	// may run it by typing `/name`.
	DisableModelInvocation bool
	// UserInvocable, when false, hides the slash command from users; only the
	// model may invoke it. Defaults to true. When false the command is Hidden.
	UserInvocable bool
	// Hidden is the isHidden flag (= !UserInvocable).
	Hidden bool
	// Aliases are alternate names the command answers to.
	Aliases []string
	// Subcommands maps a bare first-word to a canonical command name (the
	// `subcommands` map on bundled commands, e.g. {"ultra":"ultrareview"}).
	Subcommands map[string]string
	// SubcommandsBareOnly means the subcommand word is only recognised when it
	// is the entire argument string.
	SubcommandsBareOnly bool
	// Source is where this command was loaded from.
	Source Source
	// LoadedFrom is the finer-grained origin tag (e.g. "bundled", "plugin",
	// "skills"); empty for plain project/user markdown commands.
	LoadedFrom string
	// ProgressMessage is shown while the command runs ("running" by default,
	// "loading" for skills).
	ProgressMessage string
	// FilePath is the markdown file the command was loaded from (empty for
	// builtins). BaseDir is the commands root it was found under.
	FilePath string
	BaseDir  string
	// PluginRepository identifies the contributing plugin (source == plugin).
	PluginRepository string
	// Hooks is a plugin skill's validated frontmatter `hooks` object (nil when
	// absent, or when it could not be loaded — see pluginhooks.go).
	Hooks map[string]any
	// NpmResolution is the sanitized npm provenance of the contributing plugin
	// (pluginInfo.npmResolution), nil when it was not installed from npm.
	NpmResolution *NpmResolution
	// Body is the raw markdown body (after frontmatter) used as the prompt
	// template. For builtins this is empty; their prompt is produced in code.
	Body string
}

// ShellSetting returns the shell the command's frontmatter requested, or ""
// when it set none (the platform default applies). Pass it to
// WithCommandShell when expanding the command's `!`-blocks.
func (c *Command) ShellSetting() string {
	if c == nil || !c.ShellExplicit {
		return ""
	}
	return c.Shell
}

// IsNamespaced reports whether the command name carries a scope/namespace,
// i.e. contains a ':' (e.g. "project:foo", "ns:git:commit").
func (c *Command) IsNamespaced() bool {
	return strings.IndexByte(c.Name, ':') >= 0
}

// dedupKey identifies a command: a command is uniquely identified by
// its name together with its source (and plugin repository for plugin
// commands). Two descriptors with the same key are the same command.
func (c *Command) dedupKey() string {
	if c.Type != "" && c.Type != "prompt" {
		return c.Name + ":" + c.Type
	}
	if c.Source == SourcePlugin && c.PluginRepository != "" {
		return c.Name + ":" + string(c.Source) + ":" + c.PluginRepository
	}
	return c.Name + ":" + string(c.Source)
}

// Registry holds the merged set of commands and resolves user-typed names to a
// single command, applying source precedence (project over user, etc.).
type Registry struct {
	commands []*Command
	seen     map[string]struct{} // dedup keys
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{seen: map[string]struct{}{}}
}

// Add appends a command, skipping exact duplicates (same name+source+plugin).
func (r *Registry) Add(c *Command) {
	if c == nil {
		return
	}
	k := c.dedupKey()
	if _, ok := r.seen[k]; ok {
		return
	}
	r.seen[k] = struct{}{}
	r.commands = append(r.commands, c)
}

// All returns every registered command in insertion order.
func (r *Registry) All() []*Command { return r.commands }

// matchNames returns the lowercased set of tokens a command answers to: its
// name, its aliases, and (for a namespaced name) the bare last segment.
func matchNames(c *Command) []string {
	out := make([]string, 0, 4+len(c.Aliases))
	add := func(s string) {
		if s == "" {
			return
		}
		out = append(out, strings.ToLower(s))
	}
	add(c.Name)
	for _, a := range c.Aliases {
		add(a)
	}
	if i := strings.LastIndexByte(c.Name, ':'); i >= 0 && i+1 < len(c.Name) {
		add(c.Name[i+1:])
	}
	return out
}

// Resolve finds the command a user means when they type `name` (the token after
// the leading '/'). Matching is case-insensitive against each command's name,
// aliases and bare last segment. When several commands match (for instance a
// project and a user command that share a bare name), the one with the highest
// source priority wins; ties keep the earliest-registered command.
func (r *Registry) Resolve(name string) *Command {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return nil
	}
	var best *Command
	for _, c := range r.commands {
		matched := false
		for _, n := range matchNames(c) {
			if n == want {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if best == nil || sourcePriority[c.Source] > sourcePriority[best.Source] {
			best = c
		}
	}
	return best
}
