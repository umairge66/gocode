package headless

import "encoding/json"

// The control protocol of `--input-format stream-json`: besides user
// messages, the host writes control_request records on stdin and gocode
// answers each with a control_response on stdout; gocode sends its own
// control_request records (can_use_tool) the same way and reads the host's
// control_response from stdin. A request and its response share the
// request_id.
//
//	→ {"type":"control_request","request_id":"r1","request":{"subtype":"interrupt"}}
//	← {"type":"control_response","response":{"subtype":"success","request_id":"r1","response":{}}}
//	← {"type":"control_response","response":{"subtype":"error","request_id":"r1","error":"…"}}

// Record types of the control protocol.
const (
	TypeControlRequest       = "control_request"
	TypeControlResponse      = "control_response"
	TypeControlCancelRequest = "control_cancel_request"
)

// Control request subtypes.
const (
	ControlInitialize        = "initialize"
	ControlInterrupt         = "interrupt"
	ControlSetModel          = "set_model"
	ControlSetPermissionMode = "set_permission_mode"
	ControlMCPStatus         = "mcp_status"
	ControlGetContextUsage   = "get_context_usage"
	// ControlCanUseTool is the request gocode sends the host to decide a
	// tool call the permission rules leave to the user.
	ControlCanUseTool = "can_use_tool"
)

// ControlCapabilities are the control request subtypes a stream-json input
// session accepts from the host, in the order system/init lists them
// (its capabilities field). Hosts feature-detect on it.
var ControlCapabilities = []string{
	ControlInitialize, ControlInterrupt, ControlSetModel, ControlSetPermissionMode,
	ControlMCPStatus, ControlGetContextUsage,
}

// Control response subtypes.
const (
	ControlSuccess = "success"
	ControlError   = "error"
)

// ControlRequest is a control_request record gocode writes (can_use_tool).
type ControlRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   any    `json:"request"`
}

// NewControlRequest wraps request (a map or struct with a subtype).
func NewControlRequest(id string, request any) ControlRequest {
	return ControlRequest{Type: TypeControlRequest, RequestID: id, Request: request}
}

// ControlCancelRequest tells the host a request gocode sent is no longer
// wanted (its turn ended first).
type ControlCancelRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
}

// NewControlCancelRequest builds the cancel record for request id.
func NewControlCancelRequest(id string) ControlCancelRequest {
	return ControlCancelRequest{Type: TypeControlCancelRequest, RequestID: id}
}

// ControlResponseBody is the response field of a control_response.
type ControlResponseBody struct {
	Subtype   string `json:"subtype"`
	RequestID string `json:"request_id"`
	Response  any    `json:"response,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ControlResponse is a control_response record.
type ControlResponse struct {
	Type     string              `json:"type"`
	Response ControlResponseBody `json:"response"`
}

// ControlOK answers request id with a success carrying response (nil
// sends {}).
func ControlOK(id string, response any) ControlResponse {
	if response == nil {
		response = map[string]any{}
	}
	return ControlResponse{Type: TypeControlResponse, Response: ControlResponseBody{Subtype: ControlSuccess, RequestID: id, Response: response}}
}

// ControlFail answers request id with an error.
func ControlFail(id, msg string) ControlResponse {
	return ControlResponse{Type: TypeControlResponse, Response: ControlResponseBody{Subtype: ControlError, RequestID: id, Error: msg}}
}

// InputRecord is one record read from stream-json input: a user message, a
// control_request from the host, or a control_response answering one of
// gocode's requests.
type InputRecord struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	// Request is a control_request's body (its subtype and fields).
	Request json.RawMessage `json:"request"`
	// Response is a control_response's body.
	Response *InputControlResponse `json:"response"`
	// Message is a user record's message.
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// InputControlResponse is the response field of a control_response the
// host sent.
type InputControlResponse struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response"`
	Error     string          `json:"error"`
}

// PermissionUpdate is one entry of can_use_tool's permission_suggestions
// and of the host's updatedPermissions (the SDK's PermissionUpdate):
// addRules with toolName / ruleContent pairs, or setMode.
type PermissionUpdate struct {
	Type        string           `json:"type"`
	Rules       []PermissionRule `json:"rules,omitempty"`
	Behavior    string           `json:"behavior,omitempty"`
	Destination string           `json:"destination,omitempty"`
	Mode        string           `json:"mode,omitempty"`
}

// PermissionRule is one rule of an addRules permission update.
type PermissionRule struct {
	ToolName    string `json:"toolName"`
	RuleContent string `json:"ruleContent,omitempty"`
}

// CanUseToolRequest is the body of the can_use_tool control request.
type CanUseToolRequest struct {
	Subtype               string             `json:"subtype"`
	ToolName              string             `json:"tool_name"`
	Input                 json.RawMessage    `json:"input"`
	PermissionSuggestions []PermissionUpdate `json:"permission_suggestions"`
	ToolUseID             string             `json:"tool_use_id,omitempty"`
	// DecisionReason is why the rules left the call to the user.
	DecisionReason string `json:"decision_reason,omitempty"`
	// BlockedPath is the path outside the working directories a read
	// asks about.
	BlockedPath string `json:"blocked_path,omitempty"`
}

// PermissionResult is the host's answer to can_use_tool (the SDK's
// PermissionResult): behavior "allow" with an optional updatedInput and
// updatedPermissions, or "deny" with a message (interrupt also ends the
// turn).
type PermissionResult struct {
	Behavior           string             `json:"behavior"`
	UpdatedInput       json.RawMessage    `json:"updatedInput"`
	UpdatedPermissions []PermissionUpdate `json:"updatedPermissions"`
	Message            string             `json:"message"`
	Interrupt          bool               `json:"interrupt"`
}

// CompactBoundary is the system/compact_boundary record: a compaction
// rebuilt the history.
type CompactBoundary struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	CompactMetadata CompactMetadata `json:"compact_metadata"`
	SessionID       string          `json:"session_id"`
	UUID            string          `json:"uuid"`
}

// CompactMetadata is a compact boundary's trigger ("auto" / "manual") and
// the context size before it.
type CompactMetadata struct {
	Trigger   string `json:"trigger"`
	PreTokens int    `json:"pre_tokens"`
}

// NewCompactBoundary builds the compact_boundary record.
func NewCompactBoundary(sessionID, trigger string, preTokens int) CompactBoundary {
	return CompactBoundary{Type: "system", Subtype: "compact_boundary",
		CompactMetadata: CompactMetadata{Trigger: trigger, PreTokens: preTokens}, SessionID: sessionID, UUID: newUUID()}
}

// APIRetry is the system/api_retry record: a model request failed with a
// transient error and is retried after a backoff.
type APIRetry struct {
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	Attempt      int    `json:"attempt"`
	MaxRetries   int    `json:"max_retries"`
	RetryDelayMS int64  `json:"retry_delay_ms"`
	ErrorStatus  *int   `json:"error_status"`
	Error        string `json:"error"`
	SessionID    string `json:"session_id"`
	UUID         string `json:"uuid"`
}

// NewAPIRetry builds the api_retry record; status 0 is sent as null.
func NewAPIRetry(sessionID string, attempt, max int, delayMS int64, status int, errText string) APIRetry {
	r := APIRetry{Type: "system", Subtype: "api_retry", Attempt: attempt, MaxRetries: max, RetryDelayMS: delayMS,
		Error: errText, SessionID: sessionID, UUID: newUUID()}
	if status > 0 {
		r.ErrorStatus = &status
	}
	return r
}

// Task types of task_started.
const (
	TaskTypeLocalBash  = "local_bash"
	TaskTypeLocalAgent = "local_agent"
)

// TaskStarted is the system/task_started record: a background command or a
// subagent began.
type TaskStarted struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	TaskID         string `json:"task_id"`
	ToolUseID      string `json:"tool_use_id,omitempty"`
	Description    string `json:"description"`
	TaskType       string `json:"task_type"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	SpawnDepth     int    `json:"spawn_depth,omitempty"`
	ParentTaskID   string `json:"parent_task_id,omitempty"`
	SessionID      string `json:"session_id"`
	UUID           string `json:"uuid"`
}

// NewTaskStarted builds a task_started record.
func NewTaskStarted(sessionID, taskID, taskType, description string) TaskStarted {
	return TaskStarted{Type: "system", Subtype: "task_started", TaskID: taskID, TaskType: taskType,
		Description: description, SessionID: sessionID, UUID: newUUID()}
}

// Task notification statuses.
const (
	TaskCompleted = "completed"
	TaskFailed    = "failed"
	TaskStopped   = "stopped"
)

// TaskNotification is the system/task_notification record: a task
// completed, failed or was stopped.
type TaskNotification struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	TaskID     string `json:"task_id"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	Status     string `json:"status"`
	OutputFile string `json:"output_file"`
	Summary    string `json:"summary"`
	SessionID  string `json:"session_id"`
	UUID       string `json:"uuid"`
}

// NewTaskNotification builds a task_notification record.
func NewTaskNotification(sessionID, taskID, status, outputFile, summary string) TaskNotification {
	return TaskNotification{Type: "system", Subtype: "task_notification", TaskID: taskID, Status: status,
		OutputFile: outputFile, Summary: summary, SessionID: sessionID, UUID: newUUID()}
}
