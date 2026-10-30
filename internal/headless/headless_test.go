package headless

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func TestInvalidChoice(t *testing.T) {
	got := InvalidChoice(OutputFormatSpec, "xml", OutputFormats)
	want := "error: option '--output-format <format>' argument 'xml' is invalid. Allowed choices are text, json, stream-json."
	if got != want {
		t.Fatalf("got %q", got)
	}
	if !ValidOutputFormat("stream-json") || ValidOutputFormat("yaml") || ValidInputFormat("json") {
		t.Fatal("choice checks wrong")
	}
}

func TestResultMarshal(t *testing.T) {
	r := NewResult("sid")
	r.SetSuccess("done <ok> & fine")
	b, err := Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"result":"done <ok> & fine"`) || strings.Contains(s, `"errors"`) || !strings.Contains(s, `"stop_reason":null`) ||
		!strings.Contains(s, `"permission_denials":[]`) || !strings.Contains(s, `"modelUsage":{}`) {
		t.Fatalf("success = %s", s)
	}
	r.SetError(SubtypeErrorDuringExecution)
	b, _ = Marshal(r)
	s = string(b)
	if strings.Contains(s, `"result":`) || !strings.Contains(s, `"errors":[]`) || !strings.Contains(s, `"is_error":true`) {
		t.Fatalf("error = %s", s)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["subtype"] != SubtypeErrorDuringExecution {
		t.Fatalf("decode: %v %v", err, m)
	}
}

func TestWriterAndMessages(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	a := NewAssistantMessage("s", "m", "", agent.Message{Role: agent.RoleAssistant, Content: []agent.ContentBlock{agent.TextBlock("hi")}}, "end_turn", agent.ModelUsage{InputTokens: 3, OutputTokens: 4, CachedInputTokens: 1})
	_ = w.Emit(a)
	_ = w.Emit(NewUserMessage("s", agent.Message{Role: agent.RoleUser}))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", buf.String())
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &m)
	msg := m["message"].(map[string]any)
	if m["type"] != "assistant" || m["parent_tool_use_id"] != nil || msg["stop_reason"] != "end_turn" ||
		msg["usage"].(map[string]any)["cache_read_input_tokens"].(float64) != 1 || !strings.HasPrefix(msg["id"].(string), "msg_") {
		t.Fatalf("assistant = %v", m)
	}
	_ = json.Unmarshal([]byte(lines[1]), &m)
	if m["message"].(map[string]any)["content"] == nil {
		t.Fatalf("user content must be an array: %v", m)
	}
	if HasToolResult(agent.Message{Content: []agent.ContentBlock{agent.TextBlock("x")}}) {
		t.Fatal("text is not a tool result")
	}
}

func TestParseJSONSchema(t *testing.T) {
	if _, msg := ParseJSONSchema(`{"type":"object"}`); msg != "" {
		t.Fatal(msg)
	}
	if _, msg := ParseJSONSchema(`{`); !strings.HasPrefix(msg, "Error: --json-schema is not valid JSON: ") {
		t.Fatal(msg)
	}
	if _, msg := ParseJSONSchema(`"str"`); msg != ErrJSONSchemaNotObject {
		t.Fatal(msg)
	}
}

func TestValidateSchema(t *testing.T) {
	schema, _ := ParseJSONSchema(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","minLength":2},
			"age":{"type":"integer","minimum":0},
			"tags":{"type":"array","items":{"enum":["a","b"]},"maxItems":2},
			"kind":{"$ref":"#/$defs/kind"}
		},
		"required":["name"],
		"additionalProperties":false,
		"$defs":{"kind":{"anyOf":[{"const":"x"},{"type":"null"}]}}
	}`)
	ok := map[string]any{"name": "Ada", "age": 36, "tags": []any{"a"}, "kind": "x"}
	if msg := ValidateSchema(schema, ok); msg != "" {
		t.Fatalf("valid rejected: %s", msg)
	}
	cases := map[string]any{
		"root: must have required property 'name'":     map[string]any{},
		"/age: must be integer":                        map[string]any{"name": "Ada", "age": 1.5},
		"/name: must NOT have fewer than 2 characters": map[string]any{"name": "A"},
		"root: must NOT have additional properties":    map[string]any{"name": "Ada", "x": 1},
		"/tags/0: must be equal to one of the allowed": map[string]any{"name": "Ada", "tags": []any{"z"}},
		"/tags: must NOT have more than 2 items":       map[string]any{"name": "Ada", "tags": []any{"a", "b", "a"}},
		"/kind: must match a schema in anyOf":          map[string]any{"name": "Ada", "kind": "y"},
		"root: must be object":                         []any{},
		"/age: must be >= 0":                           map[string]any{"name": "Ada", "age": -1},
	}
	for want, v := range cases {
		if got := ValidateSchema(schema, v); !strings.Contains(got, want) {
			t.Errorf("%v: got %q, want %q", v, got, want)
		}
	}
}
