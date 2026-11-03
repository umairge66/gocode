package mcp

import "encoding/json"

// MCP protocol version negotiation.
//
// LatestProtocolVersion is the version the client advertises in the initialize
// request; SupportedProtocolVersions are all versions the client will accept
// back from a server (the handshake rejects anything outside this set).
const LatestProtocolVersion = "2025-11-25"

// SupportedProtocolVersions lists the accepted versions, newest first.
var SupportedProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
	"2024-10-07",
}

// MCP method names used by this client.
const (
	MethodInitialize       = "initialize"
	MethodInitializedNotif = "notifications/initialized"
	MethodToolsList        = "tools/list"
	MethodToolsCall        = "tools/call"
	MethodResourcesList    = "resources/list"
	MethodPromptsList      = "prompts/list"
	MethodPing             = "ping"

	// MethodToolsListChangedNotif is the server notification that its tool
	// catalog changed (servers advertising tools.listChanged send it).
	MethodToolsListChangedNotif = "notifications/tools/list_changed"
)

// Implementation identifies a party (client or server) in the handshake.
type Implementation struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Version     string `json:"version"`
	WebsiteURL  string `json:"websiteUrl,omitempty"`
	Description string `json:"description,omitempty"`
	Icons       []Icon `json:"icons,omitempty"`
}

// Icon is a display icon reference carried by implementations, tools and
// resources.
type Icon struct {
	Src      string   `json:"src"`
	MimeType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"` // "light" | "dark"
}

// ClientCapabilities is what the client declares it supports in the initialize
// request. All fields are optional feature objects; presence (not value) is
// what matters, so they are kept as raw objects to preserve the wire shape
// exactly without pinning every nested sub-capability.
type ClientCapabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
	Sampling     json.RawMessage            `json:"sampling,omitempty"`
	Elicitation  json.RawMessage            `json:"elicitation,omitempty"`
	Roots        *RootsCapability           `json:"roots,omitempty"`
	Tasks        json.RawMessage            `json:"tasks,omitempty"`
	Extensions   map[string]json.RawMessage `json:"extensions,omitempty"`
}

// RootsCapability advertises filesystem-roots support.
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerCapabilities is what the server reports back in the initialize result.
type ServerCapabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
	Logging      json.RawMessage            `json:"logging,omitempty"`
	Completions  json.RawMessage            `json:"completions,omitempty"`
	Prompts      *ListChangedCapability     `json:"prompts,omitempty"`
	Resources    *ResourcesCapability       `json:"resources,omitempty"`
	Tools        *ListChangedCapability     `json:"tools,omitempty"`
	Tasks        json.RawMessage            `json:"tasks,omitempty"`
	Extensions   map[string]json.RawMessage `json:"extensions,omitempty"`
}

// ListChangedCapability is the common {listChanged?} sub-capability shared by
// prompts and tools.
type ListChangedCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability advertises resource support.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// InitializeParams are the params of the "initialize" request.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the result of the "initialize" request.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
	Meta            json.RawMessage    `json:"_meta,omitempty"`
}

// Tool is a single tool entry in a tools/list result.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  ToolSchema       `json:"inputSchema"`
	OutputSchema *ToolSchema      `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Icons        []Icon           `json:"icons,omitempty"`
	Meta         json.RawMessage  `json:"_meta,omitempty"`
}

// ToolSchema is the JSON-Schema object shape used for inputSchema/outputSchema.
// It is always type "object"; properties are preserved raw.
//
// The schema is a catchall object, so every other key a server sends
// ($defs, additionalProperties, $schema, oneOf, …) is kept verbatim in Extra
// and re-emitted by MarshalJSON: the full schema reaches the model unchanged.
type ToolSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties,omitempty"`
	Required   []string                   `json:"required,omitempty"`
	// Extra holds every other top-level schema key, raw (the catchall).
	Extra map[string]json.RawMessage `json:"-"`
}

// toolSchemaKnown is ToolSchema without its methods (avoids recursion).
type toolSchemaKnown struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties,omitempty"`
	Required   []string                   `json:"required,omitempty"`
}

// UnmarshalJSON decodes the known keys and keeps every other key in Extra.
func (s *ToolSchema) UnmarshalJSON(data []byte) error {
	var k toolSchemaKnown
	if err := json.Unmarshal(data, &k); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	delete(all, "type")
	delete(all, "properties")
	delete(all, "required")
	*s = ToolSchema{Type: k.Type, Properties: k.Properties, Required: k.Required}
	if len(all) > 0 {
		s.Extra = all
	}
	return nil
}

// MarshalJSON emits the known keys plus every Extra key.
func (s ToolSchema) MarshalJSON() ([]byte, error) {
	known, err := json.Marshal(toolSchemaKnown{Type: s.Type, Properties: s.Properties, Required: s.Required})
	if err != nil || len(s.Extra) == 0 {
		return known, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(known, &all); err != nil {
		return nil, err
	}
	for k, v := range s.Extra {
		if _, clash := all[k]; clash || len(v) == 0 {
			continue
		}
		all[k] = v
	}
	return json.Marshal(all)
}

// ToolAnnotations are the optional behavioral hints on a tool.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ListToolsParams are the (optional) params of tools/list.
type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult is the result of tools/list.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams are the params of tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

// CallToolResult is the result of tools/call.
type CallToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// ContentBlock is one element of a tool-call result's content array. The MCP
// content union is discriminated by "type" (text | image | audio |
// resource_link | resource); the common fields are modeled explicitly and the
// type-specific payload is preserved raw.
type ContentBlock struct {
	Type string `json:"type"`
	// Text is set for type "text".
	Text string `json:"text,omitempty"`
	// Data (base64) and MimeType are set for type "image" and "audio".
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	// Resource carries the embedded resource for type "resource".
	Resource json.RawMessage `json:"resource,omitempty"`
	// URI, Name and Description are set for type "resource_link".
	URI         string `json:"uri,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Meta is the optional per-block _meta.
	Meta json.RawMessage `json:"_meta,omitempty"`
}
