// Package toolsearch holds the on-demand tool loading policy and the
// ToolSearch tool's search engine.
//
// Only the core tools' definitions ride on every model request. Every other
// tool is deferred: the model sees its name in a system-reminder and loads its
// full definition with the ToolSearch tool ("select:Name,Other" for exact
// names, or keywords). Which tools are deferred, the ENABLE_TOOL_SEARCH
// switch, and the query forms and ranking live here; the per-request tool
// selection and the reminders live in internal/agent (deferred.go).
package toolsearch

import (
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/protocol"
)

// Name is the ToolSearch tool's name.
const Name = string(protocol.ToolSearch)

// EnvVar is the switch for deferred tool loading.
//
//	unset, "true", "1" …   on (the default)
//	"false", "0" …         off: every tool definition is sent on every request
//	"auto"                 on only when the deferrable tools' definitions are
//	                       at least 10% of the model's context window
//	"auto:N"               the same with N% (auto:0 is on, auto:100 is off)
//	"force"                on, even when experimental betas are disabled
const EnvVar = "ENABLE_TOOL_SEARCH"

// DisableBetasEnvVar turns experimental API features off; it also turns
// deferred tool loading off unless ENABLE_TOOL_SEARCH is "force".
const DisableBetasEnvVar = "GOCODE_CODE_DISABLE_EXPERIMENTAL_BETAS"

// Mode is the resolved deferred-loading mode.
type Mode int

const (
	// ModeStandard sends every tool definition on every request.
	ModeStandard Mode = iota
	// ModeOn defers the deferrable tools.
	ModeOn
	// ModeAuto defers them only when their definitions are large relative to
	// the context window (see AutoThresholdChars).
	ModeAuto
)

func (m Mode) String() string {
	switch m {
	case ModeOn:
		return "tst"
	case ModeAuto:
		return "tst-auto"
	default:
		return "standard"
	}
}

// defaultAutoPercent is the auto mode threshold: deferrable definitions
// worth at least this share of the context window turn deferral on.
const defaultAutoPercent = 10

// charsPerToken converts the auto threshold from tokens to characters.
const charsPerToken = 2.5

// ResolveMode reads the mode from getenv (ENABLE_TOOL_SEARCH and the
// experimental-betas switch).
func ResolveMode(getenv func(string) string) Mode {
	if getenv == nil {
		return ModeOn
	}
	v := strings.TrimSpace(getenv(EnvVar))
	if v == "force" {
		return ModeOn
	}
	if envutil.EnvTruthy(getenv(DisableBetasEnvVar)) {
		return ModeStandard
	}
	if pct, ok := autoPercent(v); ok {
		switch pct {
		case 0:
			return ModeOn
		case 100:
			return ModeStandard
		}
	}
	if v == "auto" || strings.HasPrefix(v, "auto:") {
		return ModeAuto
	}
	if envutil.EnvTruthy(v) {
		return ModeOn
	}
	if envutil.EnvFalsy(v) {
		return ModeStandard
	}
	return ModeOn
}

// autoPercent parses "auto:N" (clamped to 0..100).
func autoPercent(v string) (int, bool) {
	if !strings.HasPrefix(v, "auto:") {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v[len("auto:"):]), 64)
	if err != nil {
		return 0, false
	}
	switch {
	case n < 0:
		n = 0
	case n > 100:
		n = 100
	}
	return int(n), true
}

// AutoThresholdChars is the auto mode threshold in characters for a model
// with the given context window (tokens): deferral turns on when the
// deferrable tools' name + description + schema characters reach it.
func AutoThresholdChars(getenv func(string) string, contextWindow int) int {
	pct := defaultAutoPercent
	if getenv != nil {
		if p, ok := autoPercent(strings.TrimSpace(getenv(EnvVar))); ok {
			pct = p
		}
	}
	tokens := contextWindow * pct / 100
	return int(float64(tokens) * charsPerToken)
}

// deferredBuiltins are the built-in tools whose definitions are withheld
// until loaded. The core tools (Bash, Read, Edit, Write, Glob, Grep, Agent,
// Skill, Workflow, ListAgents, AskUserQuestion, TodoWrite, ToolSearch) are
// always sent, except that Workflow and ListAgents are also deferred when
// compact tool texts are on (compactDeferredBuiltins). TodoWrite stays loaded
// because models that track their work with it use the finished checklist as
// the signal to stop; without it some keep re-verifying a completed task. The browser tools are deferred like
// every MCP tool.
var deferredBuiltins = map[string]bool{
	string(protocol.ToolNotebookEdit): true,
	string(protocol.ToolWebFetch):     true,
	string(protocol.ToolWebSearch):    true,
	string(protocol.ToolSendMessage):  true,
	string(protocol.ToolAmendAgent):   true,
	string(protocol.ToolMultiEdit):    true,
	"EnterPlanMode":                   true,
	"ExitPlanMode":                    true,
	"EnterWorktree":                   true,
	"ExitWorktree":                    true,
	"TaskStop":                        true,
	"Monitor":                         true,
	string(protocol.ToolLSP):          true,
	"CronCreate":                      true,
	"CronDelete":                      true,
	"CronList":                        true,
	"TaskCreate":                      true,
	"TaskGet":                         true,
	"TaskUpdate":                      true,
	"TaskList":                        true,
	"PushNotification":                true,
	"ListMcpResourcesTool":            true,
	"ReadMcpResourceTool":             true,

	string(protocol.ToolBrowserNavigate):    true,
	string(protocol.ToolBrowserReadPage):    true,
	string(protocol.ToolBrowserGetPageText): true,
	string(protocol.ToolBrowserTabsContext): true,
	string(protocol.ToolBrowserTabsCreate):  true,
	string(protocol.ToolBrowserTabsClose):   true,
	string(protocol.ToolBrowserScreenshot):  true,
	string(protocol.ToolBrowserComputer):    true,
	string(protocol.ToolBrowserFind):        true,
	string(protocol.ToolBrowserFormInput):   true,
	string(protocol.ToolBrowserBatch):       true,
}

// IsDeferred reports whether a tool's definition is withheld until loaded:
// every MCP tool, and the non-core built-ins. ToolSearch never is.
func IsDeferred(name string, mcp bool) bool {
	if name == Name {
		return false
	}
	if mcp || strings.HasPrefix(name, "mcp__") {
		return true
	}
	return deferredBuiltins[name]
}

// compactDeferredBuiltins are core tools deferred as well when compact tool
// texts are on: sessions rarely orchestrate workflow scripts or list local
// sessions, so their definitions load on demand.
var compactDeferredBuiltins = map[string]bool{
	string(protocol.ToolWorkflow):   true,
	string(protocol.ToolListAgents): true,
}

// IsDeferredCompact is IsDeferred with compact tool texts on, which also
// defers compactDeferredBuiltins.
func IsDeferredCompact(name string, mcp bool) bool {
	return IsDeferred(name, mcp) || compactDeferredBuiltins[name]
}

// unsupportedModels are model families that cannot read tool_reference
// blocks; a provider with native deferred loading sends them every tool.
var unsupportedModels = []string{
	"gocode-3-opus", "gocode-3-sonnet", "gocode-3-haiku",
	"gocode-3-5-sonnet", "gocode-3-5-haiku", "gocode-3-7-sonnet",
	"claude-3-opus", "claude-3-sonnet", "claude-3-haiku",
	"claude-3-5-sonnet", "claude-3-5-haiku", "claude-3-7-sonnet",
}

// ModelSupportsToolReference reports whether model can use native deferred
// loading (tool_reference blocks).
func ModelSupportsToolReference(model string) bool {
	m := strings.ToLower(model)
	for _, u := range unsupportedModels {
		if strings.Contains(m, u) {
			return false
		}
	}
	return true
}

// NativeBeta is the anthropic-beta value that enables deferred loading
// (defer_loading tools and tool_reference blocks) on the Messages API.
const NativeBeta = "advanced-tool-use-2025-11-20"

// NativeAllowed reports whether a Messages API endpoint gets native deferred
// loading: always on the default endpoint (base "" or defaultBase); on a
// custom base URL — a gateway or proxy that may not accept the request
// shape — only when ENABLE_TOOL_SEARCH is set to an explicit "on" value.
// Elsewhere deferred tools are loaded by adding their definitions to later
// requests, which every endpoint accepts.
func NativeAllowed(base, defaultBase string, getenv func(string) string) bool {
	if getenv != nil && ResolveMode(getenv) == ModeStandard {
		return false
	}
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" || b == strings.TrimRight(defaultBase, "/") {
		return true
	}
	if getenv == nil {
		return false
	}
	v := strings.TrimSpace(getenv(EnvVar))
	return v == "force" || envutil.EnvTruthy(v)
}
