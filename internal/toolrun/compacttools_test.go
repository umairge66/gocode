package toolrun

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// stripDescriptions removes every "description" string from a JSON schema,
// leaving its structure (properties, types, enums, bounds, required).
func stripDescriptions(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("schema does not parse: %v\n%s", err, raw)
	}
	var walk func(any) any
	walk = func(x any) any {
		switch n := x.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, val := range n {
				if _, isText := val.(string); k == "description" && isText {
					continue
				}
				out[k] = walk(val)
			}
			return out
		case []any:
			out := make([]any, len(n))
			for i, val := range n {
				out[i] = walk(val)
			}
			return out
		}
		return x
	}
	return walk(v)
}

func schemasByName(d *Dispatcher) map[string]struct {
	desc   string
	schema json.RawMessage
} {
	out := map[string]struct {
		desc   string
		schema json.RawMessage
	}{}
	for _, s := range d.Schemas() {
		out[s.Name] = struct {
			desc   string
			schema json.RawMessage
		}{s.Description, s.InputSchema}
	}
	return out
}

func TestHeadlessCompactTextsKeepSchemas(t *testing.T) {
	full := schemasByName(New(toolreg.ModeDefault, nil))
	compact := schemasByName(New(toolreg.ModeDefault, nil, WithCompactTools(true)))
	limits := map[protocol.Tool]int{
		protocol.ToolTodoWrite: 2500,
		protocol.ToolGrep:      1800,
		protocol.ToolRead:      1600,
	}
	for name, limit := range limits {
		f, c := full[string(name)], compact[string(name)]
		if f.desc == "" || c.desc == "" {
			t.Fatalf("%s not advertised (full %q, compact %q)", name, f.desc, c.desc)
		}
		if c.desc == f.desc {
			t.Errorf("%s: headless description is the full one", name)
		}
		if !reflect.DeepEqual(stripDescriptions(t, f.schema), stripDescriptions(t, c.schema)) {
			t.Errorf("%s: headless schema structure differs\nfull:    %s\ncompact: %s", name, f.schema, c.schema)
		}
		if size := len(name) + len(c.desc) + len(c.schema); size > limit {
			t.Errorf("%s: headless definition is %d chars, want <= %d", name, size, limit)
		}
	}
	// Every other tool is advertised exactly as in an interactive session.
	for name, f := range full {
		if _, ok := limits[protocol.Tool(name)]; ok {
			continue
		}
		if c := compact[name]; c.desc != f.desc || string(c.schema) != string(f.schema) {
			t.Errorf("%s changed in a headless session", name)
		}
	}
}

func TestHeadlessDispatcherDefersHeadlessSet(t *testing.T) {
	for _, headless := range []bool{false, true} {
		d := New(toolreg.ModeDefault, nil, WithToolSearch(toolsearch.ModeOn, 0), WithCompactTools(headless))
		if d.CompactTools() != headless {
			t.Fatalf("Headless() = %v, want %v", d.CompactTools(), headless)
		}
		for _, n := range []string{"Workflow", "ListAgents"} {
			if got := d.isDeferred(n, false); got != headless {
				t.Errorf("headless=%v: %s deferred = %v", headless, n, got)
			}
		}
		for _, n := range []string{"Agent", "TodoWrite", "Read", "Grep"} {
			if d.isDeferred(n, false) {
				t.Errorf("headless=%v: %s should stay loaded", headless, n)
			}
		}
	}
}
