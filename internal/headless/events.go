package headless

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
)

// Writer writes one JSON record per line. It is safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer over w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Emit writes v as a single line of JSON. HTML characters are not escaped, so
// the text matches what the model produced byte for byte.
func (w *Writer) Emit(v any) error {
	b, err := Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(append(b, '\n'))
	return err
}

// Marshal encodes v as compact JSON without HTML escaping and without a
// trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MCPServerStatus is one entry of system/init's mcp_servers: status is
// mcp_status's vocabulary (connected, failed, needs-auth, pending), source
// where the server's definition came from (a config scope or "plugin").
type MCPServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Source string `json:"source,omitempty"`
}

// PluginInfo is one entry of system/init's plugins.
type PluginInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SystemInit is the first record of a stream-json run: the session's
// metadata, emitted ahead of every other record.
type SystemInit struct {
	Type           string            `json:"type"`
	Subtype        string            `json:"subtype"`
	Cwd            string            `json:"cwd"`
	SessionID      string            `json:"session_id"`
	Tools          []string          `json:"tools"`
	MCPServers     []MCPServerStatus `json:"mcp_servers"`
	Model          string            `json:"model"`
	PermissionMode string            `json:"permissionMode"`
	SlashCommands  []string          `json:"slash_commands"`
	APIKeySource   string            `json:"apiKeySource"`
	Version        string            `json:"gocode_code_version"`
	OutputStyle    string            `json:"output_style"`
	Agents         []string          `json:"agents"`
	Skills         []string          `json:"skills"`
	Plugins        []PluginInfo      `json:"plugins"`
	// Capabilities lists the control request subtypes the session accepts
	// on stream-json input (empty without it), so a host can feature-detect.
	Capabilities []string `json:"capabilities"`
	UUID         string   `json:"uuid"`
}

// NewSystemInit returns a system/init record with every list non-nil (the
// record always carries arrays, never null).
func NewSystemInit(sessionID, cwd, model, permissionMode string) SystemInit {
	return SystemInit{
		Type: "system", Subtype: "init", Cwd: cwd, SessionID: sessionID,
		Tools: []string{}, MCPServers: []MCPServerStatus{}, Model: model,
		PermissionMode: permissionMode, SlashCommands: []string{}, APIKeySource: "none",
		OutputStyle: "default", Agents: []string{}, Skills: []string{}, Plugins: []PluginInfo{},
		Capabilities: []string{}, UUID: newUUID(),
	}
}

// APIMessage is the model response carried by an assistant record.
type APIMessage struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []agent.ContentBlock `json:"content"`
	StopReason   *string              `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        Usage                `json:"usage"`
}

// AssistantMessage is a stream-json assistant record.
type AssistantMessage struct {
	Type            string     `json:"type"`
	Message         APIMessage `json:"message"`
	ParentToolUseID *string    `json:"parent_tool_use_id"`
	SessionID       string     `json:"session_id"`
	UUID            string     `json:"uuid"`
}

// NewAssistantMessage builds the assistant record for one model turn.
func NewAssistantMessage(sessionID, model, id string, m agent.Message, stopReason string, u agent.ModelUsage) AssistantMessage {
	content := m.Content
	if content == nil {
		content = []agent.ContentBlock{}
	}
	if id == "" {
		id = m.ID
	}
	if id == "" {
		id = "msg_" + newUUID()
	}
	return AssistantMessage{
		Type: "assistant",
		Message: APIMessage{
			ID: id, Type: "message", Role: agent.RoleAssistant, Model: model,
			Content: content, StopReason: optString(stopReason), Usage: UsageFromModel(u),
		},
		SessionID: sessionID,
		UUID:      newUUID(),
	}
}

// UserContent is the message carried by a user record.
type UserContent struct {
	Role    string               `json:"role"`
	Content []agent.ContentBlock `json:"content"`
}

// UserMessage is a stream-json user record (a round's tool results).
type UserMessage struct {
	Type            string      `json:"type"`
	Message         UserContent `json:"message"`
	ParentToolUseID *string     `json:"parent_tool_use_id"`
	SessionID       string      `json:"session_id"`
	UUID            string      `json:"uuid"`
}

// NewUserMessage builds the user record for a tool-result turn.
func NewUserMessage(sessionID string, m agent.Message) UserMessage {
	content := m.Content
	if content == nil {
		content = []agent.ContentBlock{}
	}
	return UserMessage{
		Type:      "user",
		Message:   UserContent{Role: agent.RoleUser, Content: content},
		SessionID: sessionID,
		UUID:      newUUID(),
	}
}

// HasToolResult reports whether m carries at least one tool_result block —
// the user turns a stream-json run reports.
func HasToolResult(m agent.Message) bool {
	for _, b := range m.Content {
		if b.Type == agent.BlockToolResult {
			return true
		}
	}
	return false
}

// StreamEvent is a partial-message record (--include-partial-messages): one
// raw streaming event of the assistant turn being produced.
type StreamEvent struct {
	Type            string  `json:"type"`
	Event           any     `json:"event"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	SessionID       string  `json:"session_id"`
	UUID            string  `json:"uuid"`
}

// NewStreamEvent wraps one streaming event.
//
// Partial events follow the Messages API stream of one model response:
// message_start, then per content block content_block_start, its
// content_block_delta events (text_delta, thinking_delta, input_json_delta)
// and content_block_stop, then message_delta and message_stop. Block
// indices count the blocks of that response from 0.
func NewStreamEvent(sessionID string, event any) StreamEvent {
	return StreamEvent{Type: "stream_event", Event: event, SessionID: sessionID, UUID: newUUID()}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
