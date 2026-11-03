package mcphost

import (
	"bytes"
	"encoding/json"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/textutil"
)

// neutralizeTool escapes harness control tags and fake turn markers
// (textutil.NeutralizeHarnessText) in the text an MCP server supplies for a
// tool definition: its description, and the "description" / "title" strings
// anywhere in its input schema. The definition reaches the model verbatim, so
// a server must not be able to pose as a system reminder there. Schema values
// are re-encoded only when something matched.
func neutralizeTool(t mcp.Tool) mcp.Tool {
	t.Description, _ = textutil.NeutralizeHarnessText(t.Description)
	t.InputSchema.Properties = neutralizeRawMap(t.InputSchema.Properties, false)
	t.InputSchema.Extra = neutralizeRawMap(t.InputSchema.Extra, true)
	return t
}

// neutralizeRawMap neutralizes the values of a schema object's members
// (topLevel: the map holds the schema's own keywords, so a "description"
// member is text; otherwise each value is a property's schema). The map is
// copied only when a value changed.
func neutralizeRawMap(m map[string]json.RawMessage, topLevel bool) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	for k, raw := range m {
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			continue
		}
		var changed bool
		if s, ok := v.(string); ok && topLevel && schemaTextKey(k) {
			v, changed = neutralizeString(s)
		} else {
			changed = neutralizeSchemaText(v)
		}
		if !changed {
			continue
		}
		enc, err := marshalNoEscape(v)
		if err != nil {
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage, len(m))
			for k2, r2 := range m {
				out[k2] = r2
			}
		}
		out[k] = enc
	}
	if out == nil {
		return m
	}
	return out
}

// schemaTextKey reports whether a schema keyword holds model-facing text.
func schemaTextKey(k string) bool { return k == "description" || k == "title" }

// neutralizeSchemaText rewrites, in place, every "description" / "title"
// string inside a decoded schema value and reports whether any changed.
func neutralizeSchemaText(v any) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && schemaTextKey(k) {
				if ns, c := neutralizeString(s); c {
					x[k], changed = ns, true
				}
				continue
			}
			if neutralizeSchemaText(e) {
				changed = true
			}
		}
	case []any:
		for _, e := range x {
			if neutralizeSchemaText(e) {
				changed = true
			}
		}
	}
	return changed
}

func neutralizeString(s string) (string, bool) {
	out, hits := textutil.NeutralizeHarnessText(s)
	return out, len(hits) > 0
}

func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
