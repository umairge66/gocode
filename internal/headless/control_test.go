package headless

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestControlRecordShapes(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{ControlOK("r1", nil), `{"type":"control_response","response":{"subtype":"success","request_id":"r1","response":{}}}`},
		{ControlFail("r2", "nope"), `{"type":"control_response","response":{"subtype":"error","request_id":"r2","error":"nope"}}`},
		{NewControlCancelRequest("g1"), `{"type":"control_cancel_request","request_id":"g1"}`},
		{NewControlRequest("g2", CanUseToolRequest{Subtype: ControlCanUseTool, ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`), PermissionSuggestions: []PermissionUpdate{}}),
			`{"type":"control_request","request_id":"g2","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"permission_suggestions":[]}}`},
	} {
		b, err := Marshal(c.v)
		if err != nil || string(b) != c.want {
			t.Errorf("got %s (%v), want %s", b, err, c.want)
		}
	}
}

func TestSystemRecords(t *testing.T) {
	b, _ := Marshal(NewCompactBoundary("s", "auto", 1234))
	if !strings.Contains(string(b), `"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":1234},"session_id":"s","uuid":"`) {
		t.Errorf("compact_boundary = %s", b)
	}
	b, _ = Marshal(NewAPIRetry("s", 2, 10, 1500, 0, "connection reset"))
	if !strings.Contains(string(b), `"subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1500,"error_status":null,"error":"connection reset"`) {
		t.Errorf("api_retry = %s", b)
	}
	b, _ = Marshal(NewTaskNotification("s", "local_bash_1", TaskCompleted, "/tmp/o", "done"))
	if !strings.Contains(string(b), `"subtype":"task_notification","task_id":"local_bash_1","status":"completed","output_file":"/tmp/o","summary":"done"`) {
		t.Errorf("task_notification = %s", b)
	}
	b, _ = Marshal(NewSystemInit("s", "/w", "m", "default"))
	if !strings.Contains(string(b), `"capabilities":[]`) {
		t.Errorf("init = %s", b)
	}
}
