package chromehost

import (
	"encoding/json"
	"fmt"
)

// The native-messaging protocol is a small request/response RPC on top of the
// framed JSON codec. The HOST (this process) sends Commands and the EXTENSION
// replies with a matching Response, correlated by a string id the host assigns.
// The extension may also send unsolicited messages (a Hello handshake on
// connect, and later tab events); those carry a "method" and are classified as
// events rather than responses.

// Command is a host -> extension request. ID correlates the eventual Response;
// Method selects the action; Params carries action-specific arguments.
type Command struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is an extension -> host reply to a Command, echoing its ID. Exactly
// one of Result or Error is set.
type Response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is a structured, in-band error returned by the extension for a
// command it could not fulfil (e.g. no active tab, scripting blocked on a
// chrome:// page). It is distinct from a transport failure.
type RPCError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code != 0 {
		return fmt.Sprintf("chrome extension error %d: %s", e.Code, e.Message)
	}
	return "chrome extension error: " + e.Message
}

// Method names for the milestone-1 verbs. read_page and get_page_text are the
// same action under two names; the extension accepts both.
const (
	MethodHello       = "hello"
	MethodPing        = "ping"
	MethodNavigate    = "navigate"
	MethodReadPage    = "read_page"
	MethodTabsContext = "tabs_context"
	MethodTabsCreate  = "tabs_create"
	MethodTabsClose   = "tabs_close"
	MethodScreenshot  = "screenshot"

	// Milestone-3 interaction verbs. These drive chrome.debugger (CDP) in the
	// extension: the debugger is attached to the target tab per action/batch and
	// detached when idle so Chrome's "started debugging this browser" banner only
	// shows during active control. The host relay and control socket are
	// verb-agnostic, so these need no other host changes.
	MethodComputer     = "computer"
	MethodFind         = "find"
	MethodFormInput    = "form_input"
	MethodBrowserBatch = "browser_batch"

	// Verbs the --gocode-in-chrome-mcp server adds. javascript_tool
	// evaluates an expression in a gocode tab through chrome.debugger
	// (Runtime.evaluate); resize_window resizes the window of a gocode tab.
	MethodJavascript   = "javascript_tool"
	MethodResizeWindow = "resize_window"
)

// Hello is the handshake the extension sends immediately after connectNative.
// It identifies the extension build so the host can log what connected; it is
// an event (it carries Method == MethodHello), not a reply to any command.
type Hello struct {
	Method  string `json:"method"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Event is an extension-initiated message (not a response to a host command):
// the Hello handshake, or — in later milestones — a tab lifecycle event. It is
// distinguished from a Response purely by carrying a non-empty Method.
type Event struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	// Raw is the complete decoded frame, so an OnEvent handler can unmarshal
	// method-specific shapes such as Hello (which carries name/version at the
	// top level rather than inside Params).
	Raw json.RawMessage `json:"-"`
}

// envelope is the discriminating view used to classify an inbound frame: a
// frame with a non-empty Method is an extension-initiated Event; otherwise it
// is a Response to one of the host's pending Commands, correlated by ID.
type envelope struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
	Params json.RawMessage `json:"params"`
}
