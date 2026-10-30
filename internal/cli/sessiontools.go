package cli

import (
	"strings"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/tools/web"
)

// Tools a session does not offer, and where it saves oversized tool results.
//
// Every tool definition is sent with every model request, so a tool the
// session cannot use only costs context:
//
//   - AskUserQuestion needs someone to answer; a non-interactive session (-p)
//     has nobody, so the tool is not offered there.
//   - EnterPlanMode and ExitPlanMode need someone to approve the plan, so a
//     non-interactive session does not offer them either (plan mode itself
//     still applies there).
//   - AmendAgent redirects a subagent when the user adds a requirement while
//     it works; a non-interactive session gets no such follow-up, so it is
//     not offered there either.
//   - The browser tools drive Chrome through the gocode extension. In an
//     interactive session they are offered unless --no-chrome is given; a
//     non-interactive session offers them only with --chrome. The
//     GOCODE_CODE_ENABLE_CFC variable forces them on (truthy) or off (falsy)
//     when no flag was given.
//   - WebSearch needs a search backend (BRAVE_SEARCH_API_KEY); without one
//     every call fails, so the tool is not offered — in the main session, the
//     deferred-tool list, or any subagent.

// browserTools are the Chrome-extension tools.
var browserTools = []protocol.Tool{
	protocol.ToolBrowserNavigate,
	protocol.ToolBrowserReadPage,
	protocol.ToolBrowserGetPageText,
	protocol.ToolBrowserTabsContext,
	protocol.ToolBrowserTabsCreate,
	protocol.ToolBrowserTabsClose,
	protocol.ToolBrowserScreenshot,
	protocol.ToolBrowserComputer,
	protocol.ToolBrowserFind,
	protocol.ToolBrowserFormInput,
	protocol.ToolBrowserBatch,
}

// browserToolsEnabled decides whether the session offers the browser tools.
func browserToolsEnabled(env *Env, s *printSession, interactive bool) bool {
	if s != nil {
		switch s.Chrome {
		case "on":
			return true
		case "off":
			return false
		}
	}
	v := envGetenv(env)("GOCODE_CODE_ENABLE_CFC")
	switch {
	case envutil.EnvTruthy(v):
		return true
	case envutil.EnvFalsy(v):
		return false
	}
	return interactive
}

// unofferedTools lists the built-in tools this session leaves out.
func unofferedTools(env *Env, s *printSession, interactive bool) []string {
	var out []string
	if !interactive {
		out = append(out, string(protocol.ToolAskUserQuestion), string(protocol.ToolAmendAgent),
			string(protocol.ToolEnterPlanMode), string(protocol.ToolExitPlanMode))
	}
	if !browserToolsEnabled(env, s, interactive) {
		for _, t := range browserTools {
			out = append(out, string(t))
		}
	}
	if !webSearchConfigured(env) {
		out = append(out, string(protocol.ToolWebSearch))
	}
	return out
}

// webSearchConfigured reports whether a WebSearch backend is available: the
// Brave key is set in the session environment or the process environment.
func webSearchConfigured(env *Env) bool {
	if strings.TrimSpace(envGetenv(env)(web.BraveAPIKeyEnv)) != "" {
		return true
	}
	return web.NewBraveBackendFromEnv() != nil
}

// toolResultsDir is where this session saves tool results too large to send
// inline: <config home>/projects/<escaped cwd>/<session id>/tool-results.
// "" when there is no config home.
func toolResultsDir(env *Env, paths sessionPaths, sessionID string) string {
	base := sessionConfigHome(env, paths)
	if base == "" || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	return join(sessionstore.New(base).ProjectDir(paths.cwd), sessionID, "tool-results")
}

// fullToolDescriptionsEnv restores the full tool descriptions and the smaller
// deferred set when truthy.
const fullToolDescriptionsEnv = "GOCODE_FULL_TOOL_DESCRIPTIONS"

// compactToolsEnabled reports whether the session advertises the compact tool
// texts: on unless GOCODE_FULL_TOOL_DESCRIPTIONS is truthy.
func compactToolsEnabled(env *Env) bool {
	return !envutil.EnvTruthy(envGetenv(env)(fullToolDescriptionsEnv))
}
