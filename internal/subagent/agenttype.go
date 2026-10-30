package subagent

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/protocol"
)

// Spawn-wide constants.
const (
	// DefaultAgentType is the general-purpose agent, used when subagent_type is
	// omitted (and available). It may be disabled in some sessions.
	DefaultAgentType = "general-purpose"

	// ForkType spawns a copy of the current agent that inherits its full
	// context.
	ForkType = "fork"

	// ForkBuiltinID is the permission-rule identity of the fork agent.
	ForkBuiltinID = "agent:builtin:fork"

	// MaxResultSizeChars caps the size of the report the Agent tool returns to
	// its caller (maxResultSizeChars on the Agent tool).
	MaxResultSizeChars = 100000

	// DefaultMaxSpawnDepth bounds subagent nesting unless overridden by
	// GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH.
	DefaultMaxSpawnDepth = 3
)

// SpecialBuiltInTypes are the built-in agent types that are not the default
// general-purpose worker: the read-only Explore agent and the read-only Plan
// architect.
var SpecialBuiltInTypes = []string{"Explore", "Plan"}

// AgentType describes one agent definition: how the model is told to use it and
// which tools it may call. A nil/empty Tools with "*" means "all tools"; a
// DisallowedTools list subtracts from that.
type AgentType struct {
	// Name is the identifier matched against subagent_type.
	Name string
	// WhenToUse is the description shown to the spawning model.
	WhenToUse string
	// Tools is the allow-list; the single element "*" means every tool.
	Tools []string
	// DisallowedTools subtracts from Tools (for the read-only built-ins: the
	// Agent tool, the Artifact family, ExitPlanMode, Edit, Write and
	// NotebookEdit).
	DisallowedTools []string
	// Model is the model directive, typically "inherit" for the built-ins.
	Model string
	// Source is "built-in" for the shipped agents (vs. plugin/user definitions).
	Source string
	// BuiltIn marks a shipped definition.
	BuiltIn bool
	// OmitProjectMemory is the omitgocodeMd flag of the read-only built-ins.
	OmitProjectMemory bool
	// Memory is the definition's persistent agent-memory scope ("user",
	// "project" or "local"), "" when it has none.
	Memory string
	// RequiredMcpServers names MCP servers (case-insensitive substrings of a
	// server name) that must have tools before this agent can be spawned.
	RequiredMcpServers []string
	// Isolation is the definition's default isolation ("worktree" or ""); the
	// Agent tool's isolation input takes precedence.
	Isolation string
	// Background makes the agent always run in the background.
	Background bool
	// SystemPrompt is the fixed persona of a built-in agent that ships with
	// its own prompt; "" uses the default persona.
	SystemPrompt string
	// Unlisted keeps a built-in out of the Agent tool's agent listing and the
	// "Available agents" hints; it is still spawned when named exactly (the
	// statusline-setup agent, which /statusline names explicitly).
	Unlisted bool
}

// writeTools are the tools the read-only built-ins (Explore, Plan) exclude, in
// addition to the Agent tool itself. The exact set comes from the
// shipped agent-type roster: Explore/Plan expose "All tools except Agent,
// Artifact, ArtifactComments, ArtifactData, ArtifactCheck, ExitPlanMode, Edit,
// Write, NotebookEdit".
var readOnlyDisallowed = []string{
	string(protocol.ToolAgent),
	"Artifact", "ArtifactComments", "ArtifactData", "ArtifactCheck",
	"ExitPlanMode",
	"Edit",
	string(protocol.ToolWrite),
	string(protocol.ToolNotebookEdit),
}

// BuiltInAgentTypes returns the shipped agent definitions in roster order.
func BuiltInAgentTypes() []AgentType {
	return []AgentType{
		{
			Name: DefaultAgentType,
			WhenToUse: "General-purpose agent for researching complex questions, " +
				"searching for code, and executing multi-step tasks. When you are " +
				"searching for a keyword or file and are not confident that you will " +
				"find the right match in the first few tries use this agent to perform " +
				"the search for you.",
			Tools:   []string{"*"},
			Source:  "built-in",
			BuiltIn: true,
		},
		{
			Name: "Explore",
			WhenToUse: "Read-only search agent for broad fan-out searches — when " +
				"answering means sweeping many files, directories, or naming " +
				"conventions and you only need the conclusion, not the file dumps. It " +
				"reads excerpts rather than whole files, so it locates code; it doesn't " +
				"review or audit it. Specify search breadth: \"medium\" for moderate " +
				"exploration, or \"very thorough\" to search across multiple locations " +
				"and naming conventions.",
			Tools:             []string{"*"},
			DisallowedTools:   readOnlyDisallowed,
			Model:             "inherit",
			Source:            "built-in",
			BuiltIn:           true,
			OmitProjectMemory: true,
		},
		{
			Name: "Plan",
			WhenToUse: "Software architect agent for designing implementation plans. " +
				"Use this when you need to plan the implementation strategy for a task. " +
				"Returns step-by-step plans, identifies critical files, and considers " +
				"architectural trade-offs.",
			Tools:             []string{"*"},
			DisallowedTools:   readOnlyDisallowed,
			Model:             "inherit",
			Source:            "built-in",
			BuiltIn:           true,
			OmitProjectMemory: true,
		},
		{
			Name:         agentsdef.AgentStatuslineSetup,
			WhenToUse:    "Use this agent to configure the user's gocode status line setting.",
			Tools:        []string{string(protocol.ToolRead), string(protocol.ToolEdit)},
			Model:        "sonnet",
			Source:       "built-in",
			BuiltIn:      true,
			SystemPrompt: agentsdef.StatuslineSetupPrompt(),
			Unlisted:     true,
		},
	}
}

// ToolAllowed reports whether this agent type may call the named tool. A Tools
// list of ["*"] admits every tool; any explicit allow-list restricts to its
// members; a DisallowedTools entry always wins.
func (a AgentType) ToolAllowed(tool string) bool {
	for _, d := range a.DisallowedTools {
		if d == tool {
			return false
		}
	}
	if len(a.Tools) == 0 {
		return false
	}
	for _, t := range a.Tools {
		if t == "*" || t == tool {
			return true
		}
	}
	return false
}

// IsSpecialBuiltIn reports whether name is one of the non-default built-in
// types (Explore / Plan).
func IsSpecialBuiltIn(name string) bool {
	for _, s := range SpecialBuiltInTypes {
		if s == name {
			return true
		}
	}
	return false
}

// NormalizeAgentType reduces a fork identity to the bare "fork" type and trims
// surrounding whitespace. Other names pass through unchanged.
func NormalizeAgentType(name string) string {
	name = strings.TrimSpace(name)
	if name == ForkBuiltinID {
		return ForkType
	}
	return name
}

// IsFork reports whether name requests a fork (which inherits the parent's
// context, model and tool set rather than a restricted definition).
func IsFork(name string) bool {
	return NormalizeAgentType(name) == ForkType
}

// AgentTypeRegistry resolves subagent_type values against the available agent
// definitions. It holds the built-ins plus any registered custom types.
type AgentTypeRegistry struct {
	byName map[string]AgentType
	order  []string
}

// NewAgentTypeRegistry builds a registry seeded with the built-in agent types.
func NewAgentTypeRegistry() *AgentTypeRegistry {
	r := &AgentTypeRegistry{byName: make(map[string]AgentType)}
	for _, a := range BuiltInAgentTypes() {
		r.Register(a)
	}
	return r
}

// Lookup returns the registered agent type with exactly this name.
func (r *AgentTypeRegistry) Lookup(name string) (AgentType, bool) {
	a, ok := r.byName[name]
	return a, ok
}

// Register adds or replaces an agent definition.
func (r *AgentTypeRegistry) Register(a AgentType) {
	if _, ok := r.byName[a.Name]; !ok {
		r.order = append(r.order, a.Name)
	}
	r.byName[a.Name] = a
}

// Names lists the registered agent-type names in registration order.
func (r *AgentTypeRegistry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Resolve returns the agent definition for a requested subagent_type. An empty
// request resolves to the general-purpose default. A fork request resolves to a
// synthetic unrestricted definition (it inherits the parent's tools). Unknown
// names return (AgentType{}, false).
func (r *AgentTypeRegistry) Resolve(request string) (AgentType, bool) {
	name := NormalizeAgentType(request)
	if name == ForkType {
		return AgentType{
			Name:    ForkType,
			Tools:   []string{"*"},
			Model:   "inherit",
			Source:  "built-in",
			BuiltIn: true,
		}, true
	}
	if name == "" {
		name = DefaultAgentType
	}
	a, ok := r.byName[name]
	return a, ok
}

// Built-in, user/project and plugin agent definitions are loaded by
// internal/agentsdef (Load + conversation.LoadPlugins) and registered here by
// the Agent tool (agenttool.AgentTool.catalog / descriptorToType), which also
// builds each child's restricted tool set via its DispatcherFactory.
//
// Per-agent-type allow/deny rules are applied by the permission
// layer: toolrun matches "Agent(<type>)" rules against subagent_type
// (internal/toolrun/external.go).

// AgentTypeError is a model-facing refusal to resolve a subagent_type.
type AgentTypeError struct{ Msg string }

func (e *AgentTypeError) Error() string { return e.Msg }

// TypeKey is the fuzzy comparison key of an agent type name: NFKC-normalized,
// lower-cased, with every whitespace, dash-punctuation and underscore removed,
// so "code reviewer", "Code-Reviewer" and "code_reviewer" all compare equal.
func TypeKey(name string) string {
	s := strings.ToLower(norm.NFKC.String(name))
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) || r == '_' {
			return -1
		}
		return r
	}, s)
}

// AgentList renders a name list the way the errors do: ", "-joined, or
// "none" when empty.
func AgentList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Match resolves a requested subagent_type against the registered agents.
// available reports whether a registered agent may be spawned in this session
// (nil: every registered agent). An exact available name wins; otherwise the
// request is compared by TypeKey against EVERY registered agent: one match that
// is available resolves to it, several matches are reported as ambiguous
// (unavailable ones marked), and anything else is "not found".
func (r *AgentTypeRegistry) Match(request string, available func(string) bool) (AgentType, error) {
	if available == nil {
		available = func(string) bool { return true }
	}
	var avail []string
	for _, n := range r.order {
		if available(n) && !r.byName[n].Unlisted {
			avail = append(avail, n)
		}
	}
	if a, ok := r.byName[request]; ok && available(request) {
		return a, nil
	}
	key := TypeKey(request)
	var matches []AgentType
	if key != "" {
		for _, n := range r.order {
			if TypeKey(n) == key {
				matches = append(matches, r.byName[n])
			}
		}
	}
	if len(matches) > 1 {
		var labels, usable []string
		for _, m := range matches {
			if available(m.Name) {
				labels = append(labels, m.Name)
				usable = append(usable, m.Name)
			} else {
				labels = append(labels, m.Name+" (unavailable)")
			}
		}
		tail := "None of these are available. Available agents: " + AgentList(avail)
		if len(usable) > 0 {
			tail = "Use the exact name: " + strings.Join(usable, " or ")
		}
		return AgentType{}, &AgentTypeError{Msg: fmt.Sprintf("Agent type '%s' is ambiguous \u2014 matches %s. %s", request, strings.Join(labels, ", "), tail)}
	}
	if len(matches) == 1 && available(matches[0].Name) {
		return matches[0], nil
	}
	return AgentType{}, &AgentTypeError{Msg: fmt.Sprintf("Agent type '%s' not found. Available agents: %s", request, AgentList(avail))}
}

// DefaultAvailable reports whether the general-purpose default can serve an
// omitted subagent_type: the exact built-in when present, else a single agent
// whose TypeKey equals it.
func (r *AgentTypeRegistry) DefaultAvailable(available func(string) bool) bool {
	if available == nil {
		available = func(string) bool { return true }
	}
	if _, ok := r.byName[DefaultAgentType]; ok {
		return available(DefaultAgentType)
	}
	key := TypeKey(DefaultAgentType)
	var hits []string
	for _, n := range r.order {
		if TypeKey(n) == key {
			hits = append(hits, n)
		}
	}
	return len(hits) == 1 && available(hits[0])
}

// MCPServerOfTool returns the MCP server a tool belongs to ("mcp__<server>__…"),
// or "".
func MCPServerOfTool(tool string) string {
	if !strings.HasPrefix(tool, "mcp__") {
		return ""
	}
	parts := strings.Split(tool, "__")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// MissingMcpServers checks an agent's RequiredMcpServers against the tools
// currently available: it returns the required patterns no server-with-tools
// matches (case-insensitive substring), and the distinct servers that do have
// tools, in first-seen order.
func MissingMcpServers(required, toolNames []string) (missing, servers []string) {
	seen := map[string]bool{}
	for _, t := range toolNames {
		if s := MCPServerOfTool(t); s != "" && !seen[s] {
			seen[s] = true
			servers = append(servers, s)
		}
	}
	for _, req := range required {
		lr := strings.ToLower(req)
		found := false
		for _, s := range servers {
			if strings.Contains(strings.ToLower(s), lr) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, req)
		}
	}
	return missing, servers
}

// RequiredMcpServersMessage is the refusal when an agent's required MCP servers
// have no tools.
func RequiredMcpServersMessage(agentType string, missing, servers []string) string {
	have := "none"
	if len(servers) > 0 {
		have = strings.Join(servers, ", ")
	}
	return fmt.Sprintf("Agent '%s' requires MCP servers matching: %s. MCP servers with tools: %s. Use /mcp to configure and authenticate the required MCP servers.", agentType, strings.Join(missing, ", "), have)
}
