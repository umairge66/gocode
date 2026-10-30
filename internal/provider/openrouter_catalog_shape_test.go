package provider

import (
	"encoding/json"
	"testing"
)

// The catalog keeps every well-formed record when one record has a field in
// an unexpected shape, and accepts time-window bounds as numbers or strings.
func TestDecodeOpenRouterModelsTolerant(t *testing.T) {
	body := `{"data":[
	  {"id":"a/numeric-window","name":"A","context_length":1000,
	   "pricing":{"prompt":"0.000001","completion":"0.000002",
	     "overrides":[{"utc_start":1500,"utc_end":100,"prompt":"0.0000005"},
	                  {"min_prompt_tokens":272000,"prompt":"0.000002","completion":"0.000004"}]}},
	  {"id":"b/string-window","name":"B","context_length":2000,
	   "pricing":{"prompt":"0.000001","completion":"0.000002",
	     "overrides":[{"utc_start":"15:00","utc_end":"01:00","prompt":"0.0000005"}]}},
	  {"id":"c/broken","name":"C","context_length":"not-a-number","pricing":["unexpected"]},
	  {"id":"d/plain","name":"D","context_length":3000,"pricing":{"prompt":"0","completion":"0"}}
	]}`
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	got := decodeOpenRouterModels(parsed.Data)
	ids := map[string]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	for _, want := range []string{"a/numeric-window", "b/string-window", "d/plain"} {
		if !ids[want] {
			t.Errorf("%s missing from %v", want, ids)
		}
	}
	if ids["c/broken"] {
		t.Errorf("malformed record was kept")
	}
	for _, v := range []string{`1500`, `"15:00"`} {
		if !rawPresent(json.RawMessage(v)) {
			t.Errorf("rawPresent(%s) = false", v)
		}
	}
	for _, v := range []string{``, `null`, `""`} {
		if rawPresent(json.RawMessage(v)) {
			t.Errorf("rawPresent(%q) = true", v)
		}
	}
}
