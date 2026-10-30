package apiclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageUnmarshal(t *testing.T) {
	raw := `{
	  "role": "assistant",
	  "content": [
	    {"type":"text","text":"Hello"},
	    {"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"SF"}}
	  ]
	}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Role != "assistant" || len(m.Content) != 2 {
		t.Fatalf("message: %+v", m)
	}
	if m.Content[0].Type != BlockText || m.Content[0].Text != "Hello" {
		t.Errorf("text block: %+v", m.Content[0])
	}
	if m.Content[1].Type != BlockToolUse || m.Content[1].Name != "get_weather" ||
		!strings.Contains(string(m.Content[1].Input), "SF") {
		t.Errorf("tool_use block: %+v", m.Content[1])
	}
}

// TestToolResultBlockMarshal checks tool_result fields serialize and that the
// omitempty tags keep unrelated fields off the wire.
func TestToolResultBlockMarshal(t *testing.T) {
	b, err := json.Marshal(ContentBlock{Type: BlockToolResult, ToolUseID: "tu_1", Content: "72F", IsError: true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "72F", "is_error": true}
	if len(got) != len(want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestUsageUnmarshal(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"input_tokens":10,"output_tokens":25,"cache_read_input_tokens":3}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 10 || u.OutputTokens != 25 || u.CacheReadInputTokens != 3 {
		t.Errorf("usage: %+v", u)
	}
}
