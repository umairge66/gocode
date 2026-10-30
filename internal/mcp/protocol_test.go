package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestInitializeResultRoundTrip unmarshals an initialize result
// (as a server would send it) and checks the fields land correctly.
func TestInitializeResultRoundTrip(t *testing.T) {
	raw := `{
		"protocolVersion": "2025-11-25",
		"capabilities": {
			"tools": {"listChanged": true},
			"resources": {"subscribe": true, "listChanged": false},
			"logging": {}
		},
		"serverInfo": {"name": "example-server", "version": "0.1.0", "title": "Example"},
		"instructions": "Use the tools wisely."
	}`
	var res InitializeResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if res.ProtocolVersion != LatestProtocolVersion {
		t.Errorf("protocolVersion = %s", res.ProtocolVersion)
	}
	if res.Capabilities.Tools == nil || !res.Capabilities.Tools.ListChanged {
		t.Errorf("tools.listChanged not parsed")
	}
	if res.Capabilities.Resources == nil || !res.Capabilities.Resources.Subscribe {
		t.Errorf("resources.subscribe not parsed")
	}
	if res.ServerInfo.Name != "example-server" || res.ServerInfo.Version != "0.1.0" {
		t.Errorf("serverInfo = %+v", res.ServerInfo)
	}
	if res.Instructions != "Use the tools wisely." {
		t.Errorf("instructions = %q", res.Instructions)
	}

	// Round-trip back out and ensure it re-parses equivalently.
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var res2 InitializeResult
	if err := json.Unmarshal(out, &res2); err != nil {
		t.Fatal(err)
	}
	if res2.ProtocolVersion != res.ProtocolVersion {
		t.Errorf("round-trip lost protocolVersion")
	}
}

// TestListToolsResultRoundTrip checks the tools/list result shape, including
// the nested inputSchema and annotations.
func TestListToolsResultRoundTrip(t *testing.T) {
	raw := `{
		"tools": [
			{
				"name": "get_weather",
				"description": "Get the weather",
				"inputSchema": {
					"type": "object",
					"properties": {"city": {"type": "string"}},
					"required": ["city"]
				},
				"annotations": {"readOnlyHint": true, "title": "Weather"}
			}
		],
		"nextCursor": "page2"
	}`
	var res ListToolsResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("got %d tools", len(res.Tools))
	}
	tool := res.Tools[0]
	if tool.Name != "get_weather" {
		t.Errorf("name = %q", tool.Name)
	}
	if tool.InputSchema.Type != "object" {
		t.Errorf("inputSchema.type = %q", tool.InputSchema.Type)
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "city" {
		t.Errorf("required = %v", tool.InputSchema.Required)
	}
	if _, ok := tool.InputSchema.Properties["city"]; !ok {
		t.Errorf("properties missing city")
	}
	if tool.Annotations == nil || tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
		t.Errorf("annotations.readOnlyHint not parsed")
	}
	if res.NextCursor != "page2" {
		t.Errorf("nextCursor = %q", res.NextCursor)
	}
}

// TestCallToolResultContent checks the content union of a tools/call result.
func TestCallToolResultContent(t *testing.T) {
	raw := `{
		"content": [
			{"type": "text", "text": "hello"},
			{"type": "image", "data": "aGk=", "mimeType": "image/png"}
		],
		"isError": false
	}`
	var res CallToolResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 2 {
		t.Fatalf("got %d content blocks", len(res.Content))
	}
	if res.Content[0].Type != "text" || res.Content[0].Text != "hello" {
		t.Errorf("text block = %+v", res.Content[0])
	}
	if res.Content[1].Type != "image" || res.Content[1].MimeType != "image/png" || res.Content[1].Data != "aGk=" {
		t.Errorf("image block = %+v", res.Content[1])
	}
	if res.IsError {
		t.Errorf("isError should be false")
	}
}

// TestCallToolParamsMarshal verifies the tools/call params marshal to the wire
// shape {name, arguments}.
func TestCallToolParamsMarshal(t *testing.T) {
	p := CallToolParams{Name: "echo", Arguments: json.RawMessage(`{"msg":"hi"}`)}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["name"]) != `"echo"` {
		t.Errorf("name = %s", got["name"])
	}
	if string(got["arguments"]) != `{"msg":"hi"}` {
		t.Errorf("arguments = %s", got["arguments"])
	}
}

func TestSupportedProtocolVersions(t *testing.T) {
	if SupportedProtocolVersions[0] != LatestProtocolVersion {
		t.Errorf("latest %q is not first in supported set", LatestProtocolVersion)
	}
	for _, want := range []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"} {
		found := false
		for _, v := range SupportedProtocolVersions {
			if v == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing supported version %q", want)
		}
	}
}

func TestToolSchemaKeepsCatchallKeys(t *testing.T) {
	raw := `{"type":"object","properties":{"a":{"$ref":"#/$defs/x"}},"required":["a"],"$defs":{"x":{"type":"string"}},"additionalProperties":false}`
	var s ToolSchema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if s.Type != "object" || len(s.Required) != 1 || s.Extra["$defs"] == nil || s.Extra["additionalProperties"] == nil {
		t.Fatalf("decoded = %+v", s)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal([]byte(raw), &a)
	_ = json.Unmarshal(out, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("round trip lost keys:\n in  %s\n out %s", raw, out)
	}
}
