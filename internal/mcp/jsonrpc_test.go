package mcp

import (
	"encoding/json"
	"testing"
)

// TestDecodeMessageClassification checks that DecodeMessage classifies each
// JSON-RPC message kind correctly.
func TestDecodeMessageClassification(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, "request"},
		{"request-string-id", `{"jsonrpc":"2.0","id":"abc","method":"initialize"}`, "request"},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "notification"},
		{"response-result", `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`, "response"},
		{"response-error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`, "response"},
		// method + id:null is a notification-ish edge that is not a request.
		{"null-id-method", `{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}`, "notification"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := DecodeMessage([]byte(tc.raw))
			if err != nil {
				t.Fatalf("DecodeMessage: %v", err)
			}
			var got string
			switch {
			case msg.Request != nil:
				got = "request"
			case msg.Notification != nil:
				got = "notification"
			case msg.Response != nil:
				got = "response"
			}
			if got != tc.want {
				t.Fatalf("classified as %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequestRoundTrip verifies the JSON-RPC request envelope marshals to the
// expected wire shape and back.
func TestRequestRoundTrip(t *testing.T) {
	req, err := newRequest(7, "tools/call", CallToolParams{Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", got["jsonrpc"])
	}
	if got["id"] != float64(7) {
		t.Errorf("id = %v, want 7", got["id"])
	}
	if got["method"] != "tools/call" {
		t.Errorf("method = %v, want tools/call", got["method"])
	}

	msg, err := DecodeMessage(data)
	if err != nil || msg.Request == nil {
		t.Fatalf("re-decode as request failed: %v", err)
	}
	if string(msg.Request.ID) != "7" {
		t.Errorf("decoded id = %s, want 7", msg.Request.ID)
	}
}

// TestRPCErrorEnvelope verifies the error response shape and the McpError
// message format.
func TestRPCErrorEnvelope(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"Invalid params","data":{"x":1}}}`
	msg, err := DecodeMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Response == nil || msg.Response.Error == nil {
		t.Fatal("expected an error response")
	}
	e := msg.Response.Error
	if e.Code != ErrInvalidParams {
		t.Errorf("code = %d, want %d", e.Code, ErrInvalidParams)
	}
	if got, want := e.Error(), "MCP error -32602: Invalid params"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
