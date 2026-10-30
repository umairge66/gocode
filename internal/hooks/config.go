package hooks

import "encoding/json"

// Event is a lifecycle event name a hook can be registered against. It appears
// as a string literal in the stdin payload field hook_event_name and as the
// keys of the user's hooks config.
type Event string

// The supported lifecycle event set. Only the general-purpose lifecycle events
// are modelled; further events (PostToolBatch, PermissionRequest,
// PostToolUseFailure, Elicitation, Pre/PostModelSwitch, WorktreeCreate/Remove,
// CwdChanged, ConfigChange, Setup, ...) are not.
const (
	EventPreToolUse       Event = "PreToolUse"
	EventPostToolUse      Event = "PostToolUse"
	EventUserPromptSubmit Event = "UserPromptSubmit"
	EventNotification     Event = "Notification"
	EventStop             Event = "Stop"
	EventSubagentStop     Event = "SubagentStop"
	EventSubagentStart    Event = "SubagentStart"
	EventSessionStart     Event = "SessionStart"
	EventSessionEnd       Event = "SessionEnd"
	EventPreCompact       Event = "PreCompact"
)

// HookType discriminates the kinds of hook. The config schema validates a hook
// only as an opaque object; the type discrimination happens at dispatch time,
// which switches on hook.type over "command", "prompt", "http", "mcp_tool" and
// "mcp_prompt" (internal "agent"/"script"/"callback"/"function" kinds are not
// user-configurable and are not supported).
//
// command, prompt, http and mcp_* are implemented (see runner.go,
// httphook.go, mcphook.go, prompthook.go). They share one decision shape: each
// type produces a body/stdout that is parsed by ParseStdout and mapped by
// Interpret exactly like a command hook's stdout.
type HookType string

const (
	HookTypeCommand   HookType = "command"
	HookTypePrompt    HookType = "prompt"
	HookTypeHTTP      HookType = "http"
	HookTypeMCPTool   HookType = "mcp_tool"
	HookTypeMCPPrompt HookType = "mcp_prompt"
)

// OnFailure selects what happens when a command hook fails (exits non-zero in a
// way that is not the dedicated blocking code 2). The schema default is "block".
type OnFailure string

const (
	OnFailureBlock    OnFailure = "block"
	OnFailureContinue OnFailure = "continue"
)

// CommandHook is one configured hook. Despite the name (kept for API
// compatibility), it models every user-configurable hook
// type: its Type field selects the dispatch path, and the type-specific fields
// below are populated only for that type. Hook is an alias for callers that
// want the clearer spelling.
type CommandHook struct {
	Type    HookType `json:"type"`
	Command string   `json:"command,omitempty"`

	// Args are extra argv entries appended after Command when the hook is not
	// run through a shell. Optional.
	Args []string `json:"args,omitempty"`

	// If is an optional guard expression gating whether the hook runs. It uses
	// the permission-rule grammar ("ToolName" or "ToolName(content)"); see
	// guard.go. A hook whose If does not match the current tool call is skipped.
	If string `json:"if,omitempty"`

	// Shell overrides the shell used to interpret Command. Empty means the
	// platform default resolved by the runner.
	Shell string `json:"shell,omitempty"`

	// TimeoutSeconds caps the hook's wall-clock time. Zero/absent means the
	// runner's default. The unit is seconds: the dispatcher computes the
	// deadline as timeout*1000 ms (or the default) for every hook type
	// (command, http, mcp_tool and prompt executors).
	TimeoutSeconds int `json:"timeout,omitempty"`

	// OnFailure governs non-blocking failures. Defaults to OnFailureBlock when
	// empty (see NormalizeOnFailure).
	OnFailure OnFailure `json:"on_failure,omitempty"`

	// StatusMessage is shown to the user while the hook runs.
	StatusMessage string `json:"status_message,omitempty"`

	// Once, when true, runs the hook at most once per session (see
	// OnceTracker and hookhost's dedupe).
	Once bool `json:"once,omitempty"`

	// Async, when true, detaches the hook so the agent does not wait on it; its
	// result is discarded.
	Async bool `json:"async,omitempty"`

	// AsyncRewake detaches the hook like Async, but its late result IS
	// delivered: when the background run finishes, its decision is fed back
	// into the (possibly stopped) session — a "rewake" — carrying RewakeMessage
	// and RewakeSummary. See runner.go (RewakeSink) and hookhost.
	AsyncRewake   bool   `json:"async_rewake,omitempty"`
	RewakeMessage string `json:"rewake_message,omitempty"`
	RewakeSummary string `json:"rewake_summary,omitempty"`

	// URL is the endpoint an http hook POSTs its payload to.
	URL string `json:"url,omitempty"`
	// Headers are extra request headers for an http hook. Values may reference
	// ${VAR} entries listed in AllowedEnvVars.
	Headers map[string]string `json:"headers,omitempty"`
	// AllowedEnvVars whitelists env-var names an http hook's header values may
	// interpolate (intersected with the settings' allowedEnvVars and each
	// server's httpHookAllowedEnvVars).
	AllowedEnvVars []string `json:"allowedEnvVars,omitempty"`

	// Prompt is the text a prompt hook sends to the model to judge.
	Prompt string `json:"prompt,omitempty"`
	// Model optionally overrides the model a prompt hook queries.
	Model string `json:"model,omitempty"`

	// Server and Tool name the MCP server and tool an mcp_tool hook invokes.
	// PromptName names the prompt an mcp_prompt hook requests. Input is the
	// (optionally templated) argument object.
	Server     string          `json:"server,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	PromptName string          `json:"prompt_name,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
}

// NormalizeType returns the hook's effective type, defaulting an empty Type to
// HookTypeCommand (a missing type means a command hook).
func (h CommandHook) NormalizeType() HookType {
	if h.Type == "" {
		return HookTypeCommand
	}
	return h.Type
}

// NormalizeOnFailure applies the schema default ("block").
func (h CommandHook) NormalizeOnFailure() OnFailure {
	if h.OnFailure == OnFailureContinue {
		return OnFailureContinue
	}
	return OnFailureBlock
}

// MatcherGroup binds a matcher string to a list of hooks. In the normalized
// form this is one element of a per-event matchers array.
//
// An empty or absent Matcher matches every tool/event (see Matcher.Match).
type MatcherGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []CommandHook `json:"hooks"`
}

// Config is the user's hooks configuration: each event maps to a list of
// matcher groups. This is the settings-file representation (hooks keyed by
// event name); there is also a normalized array-of-{event,matchers} form,
// which Normalize/FromEntries convert between.
type Config map[Event][]MatcherGroup

// Groups returns the matcher groups registered for event, or nil.
func (c Config) Groups(event Event) []MatcherGroup {
	return c[event]
}
