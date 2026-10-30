// Package agentsdef is gocode "custom agent-definition loader": how custom subagent types are DEFINED and LOADED from
// `.gocode/agents/*.md` files (and from an SDK `agents` JSON object), merged
// with the built-in agent types, and resolved by scope precedence into the set
// the Agent tool offers. This is the definition/loading side; the runtime that
// actually spawns a subagent from a resolved descriptor lives in another
// package (internal/agent).
//
// Overview:
//
//   - A `.gocode/agents/*.md` file becomes a descriptor from its frontmatter
//     plus body; the body, trimmed, becomes the system prompt. Plugin agents
//     use the same builder (name falls back to the filename; several keys are
//     ignored for plugin agents).
//   - The SDK/JSON `agents` object is parsed into descriptors at flagSettings.
//   - Frontmatter key aliases: allowedtools->tools, whentouse/when_to_use/
//     when-to-use->description.
//   - tools / skills accept a comma string or a list; "*" collapses to
//     "inherit all".
//   - Scope precedence: built-in < plugin < userSettings < projectSettings <
//     flagSettings < policySettings; last wins.
//
// The frontmatter parse mirrors the approach already taken in internal/skills
// and internal/commands, but is kept self-contained here (those packages'
// helpers are unexported).
package agentsdef

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Size and length caps.
const (
	// maxDefinitionBytes caps a single agent .md file.
	maxDefinitionBytes = 1048576
	// maxNameLen caps the agent name.
	maxNameLen = 256
	// gocodeDirName is the per-root project config directory.
	gocodeDirName = ".gocode"
	// agentsDirName is the per-root agents subdirectory.
	agentsDirName = "agents"
)

// Enum value sets.
var (
	// memoryScopes are the valid `memory` values.
	memoryScopes = []string{"user", "project", "local"}
	// isolationModes are the valid `isolation` values.
	isolationModes = []string{"worktree", "remote"}
	// effortLevels are the valid `effort` levels, or an integer (the error
	// text is "Valid options: <levels> or an integer").
	effortLevels = []string{"low", "medium", "high", "xhigh", "max"}
	// permissionModes are the valid `permissionMode` values.
	permissionModes = []string{"default", "acceptEdits", "bypassPermissions", "plan", "dontAsk"}
	// agentColors is the color enum a definition's `color` is checked against.
	agentColors = []string{"red", "blue", "green", "yellow", "purple", "orange", "pink", "cyan"}
)

// Source identifies the settings tier / origin that declared an agent. It
// drives precedence (see Scope).
type Source string

const (
	// SourceBuiltin is an agent bundled with the app.
	SourceBuiltin Source = "built-in"
	// SourcePlugin is an agent contributed by an installed/session plugin.
	SourcePlugin Source = "plugin"
	// SourceUser is an agent under <gocode root>/agents.
	SourceUser Source = "userSettings"
	// SourceProject is an agent under <dir>/.gocode/agents
	// ("projectSettings"); the project cwd or an added directory.
	SourceProject Source = "projectSettings"
	// SourcePolicy is an agent forced by managed/policy settings
	// ("policySettings"); it wins every collision.
	SourcePolicy Source = "policySettings"
	// SourceFlag is an agent supplied via the SDK `agents` object / an
	// `agents --json` style flag config.
	SourceFlag Source = "flagSettings"
)

// precedence returns the collision rank of a source; HIGHER wins. The merge
// builds a name->descriptor map in the order
// [built-in, plugin, userSettings, projectSettings, flagSettings,
// policySettings] and lets later entries overwrite earlier ones, so
// policySettings has the highest precedence and built-in the lowest.
func (s Source) precedence() int {
	switch s {
	case SourcePolicy:
		return 5
	case SourceFlag:
		return 4
	case SourceProject:
		return 3
	case SourceUser:
		return 2
	case SourcePlugin:
		return 1
	case SourceBuiltin:
		return 0
	default:
		return -1
	}
}

// Frontmatter is the parsed, normalized YAML frontmatter of an agent .md file.
// Only the keys the loader consumes are modeled; unknown keys are warned about
// but preserved verbatim in Extra.
type Frontmatter struct {
	// Name is the agent type. REQUIRED for file-defined agents (the
	// definition is rejected when absent); plugin agents fall back to the
	// filename.
	Name string
	// Description becomes WhenToUse. REQUIRED. Literal "\n" sequences are
	// expanded to real newlines.
	Description string
	// Tools is the allowlist. See Descriptor.Tools for the nil semantics.
	Tools []string
	// DisallowedTools removes tools while this agent is active.
	DisallowedTools []string
	// Skills restricts which skills the agent may use.
	Skills []string
	// Model: "haiku"/"sonnet"/"opus"/a full id, or "inherit".
	Model string
	// Color is the badge color; kept only if in the known color set.
	Color string
	// Effort: "low"/"medium"/"high"/"xhigh"/"max" or an integer (as string).
	Effort string
	// PermissionMode: one of permissionModes.
	PermissionMode string
	// Memory: "user"/"project"/"local".
	Memory string
	// Isolation: "worktree"/"remote".
	Isolation string
	// Background: run forks as background agents.
	Background bool
	// OmitGocodeMd: skip injecting the project gocode.md into the agent.
	OmitGocodeMd bool
	// InitialPrompt seeds the subagent's first turn.
	InitialPrompt string
	// Observer / ObserverMessage / ObserveSubagents: observation wiring.
	Observer         string
	ObserverMessage  string
	ObserveSubagents *bool
	// Extra holds frontmatter keys the loader does not recognize.
	Extra map[string]any
}

// Descriptor is a loaded, normalized agent definition as the Registry presents
// it. It is the DEFINITION; spawning is the runtime's concern.
type Descriptor struct {
	// AgentType is the canonical name used to select this agent (from
	// frontmatter `name`).
	AgentType string
	// WhenToUse is the description shown in the Agent tool / agent listing.
	WhenToUse string
	// Tools is the allowlist of tools the agent may call. A nil slice means
	// "inherit the default tool set" (the frontmatter key was absent, or was
	// "*"); a non-nil slice (possibly empty) is an explicit allowlist.
	Tools []string
	// DisallowedTools are tools removed while the agent is active.
	DisallowedTools []string
	// Skills restricts the agent's available skills.
	Skills []string
	// SystemPrompt is the agent's system prompt. For file/JSON agents it is the
	// markdown body trimmed;
	// for built-ins it is the built-in prompt text. When `memory` is set the
	// runtime appends primed-memory content after this.
	SystemPrompt string
	// Model, Color, Effort, PermissionMode, Memory, Isolation as declared;
	// "" means unset. Model "inherit" matches the parent conversation.
	Model          string
	Color          string
	Effort         string
	PermissionMode string
	Memory         string
	Isolation      string
	// Background / OmitGocodeMd flags.
	Background   bool
	OmitGocodeMd bool
	// InitialPrompt / Observer wiring (file/JSON agents only).
	InitialPrompt    string
	Observer         string
	ObserverMessage  string
	ObserveSubagents *bool
	// Source is the declaring tier; Scope is Source (precedence lives there).
	Source Source
	// FromAdditionalDirectory marks a project agent discovered under an added
	// directory rather than the main project.
	FromAdditionalDirectory bool
	// Filename is the .md basename without extension.
	Filename string
	// FilePath is the absolute path of the source file ("" for built-ins and
	// JSON agents).
	FilePath string
	// BaseDir is the trust/base directory the definition came from ("built-in"
	// for built-ins).
	BaseDir string
}

// IsBuiltin reports whether the descriptor is a built-in agent type.
func (d Descriptor) IsBuiltin() bool { return d.Source == SourceBuiltin }

// IsPlugin reports whether the descriptor came from a plugin.
func (d Descriptor) IsPlugin() bool { return d.Source == SourcePlugin }

// InheritsAllTools reports whether the agent inherits the default tool set
// rather than a restricted allowlist (Tools == nil).
func (d Descriptor) InheritsAllTools() bool { return d.Tools == nil }

// ValidateName applies the agent-name rules, returning a human
// message when the name is invalid (empty message == valid).
func ValidateName(name string) string {
	if name == "" {
		return `Missing required "name" field in frontmatter`
	}
	if strings.HasPrefix(name, "-") {
		return `Invalid "name": names must not start with "-"`
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return `Invalid "name": names must be at most 256 characters`
	}
	if strings.Contains(norm.NFKC.String(name), ":") {
		return `Invalid "name": names must not contain ":" (reserved for plugin namespacing)`
	}
	return ""
}

// parseToolsField parses the `tools`/`disallowedTools` fields: it returns (nil, false) when the field is absent or resolves to "all
// tools" (a "*" entry), and (list, true) otherwise — where the list is the
// trimmed, comma-or-sequence-split entries (possibly empty for an explicit
// empty list).
func parseToolsField(v any, present bool) (list []string, explicit bool) {
	if !present {
		return nil, false
	}
	items := coerceStringList(v)
	for _, it := range items {
		if it == "*" {
			return nil, false // "*" collapses to "inherit all"
		}
	}
	return items, true
}

// parseSkillsField parses the `skills` field: a list (never nil-as-all); an
// absent field yields an empty list.
func parseSkillsField(v any, present bool) []string {
	if !present {
		return nil
	}
	return coerceStringList(v)
}

// coerceStringList normalizes a frontmatter value into a trimmed []string,
// splitting a scalar string on commas.
func coerceStringList(v any) []string {
	var raw []string
	switch x := v.(type) {
	case string:
		raw = strings.Split(x, ",")
	case []string:
		raw = x
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				raw = append(raw, s)
			}
		}
	default:
		return []string{}
	}
	out := []string{}
	for _, s := range raw {
		s = strings.TrimSpace(trimQuotes(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func trimQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
