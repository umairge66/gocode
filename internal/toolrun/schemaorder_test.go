package toolrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
)

// orderedExternal lists its tools in the order given.
type orderedExternal struct{ names []string }

func (o orderedExternal) Tools() []agent.ToolSchema {
	out := make([]agent.ToolSchema, len(o.names))
	for i, n := range o.names {
		out[i] = agent.ToolSchema{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)}
	}
	return out
}
func (o orderedExternal) Has(string) bool { return true }
func (o orderedExternal) Call(context.Context, string, json.RawMessage) (string, bool, error) {
	return "", false, nil
}

// External tools are advertised in name order whatever order their servers
// listed them in, so the tools array (the head of the cached prompt prefix)
// is the same from run to run.
func TestSchemasSortsExternalTools(t *testing.T) {
	external := func(names ...string) string {
		d := New(toolreg.ModeBypassPermissions, nil, WithExternalTools(orderedExternal{names: names}))
		var got []string
		for _, s := range d.Schemas() {
			if strings.HasPrefix(s.Name, "mcp__") {
				got = append(got, s.Name)
			}
		}
		return strings.Join(got, ",")
	}
	a := external("mcp__b__x", "mcp__a__y", "mcp__a__b")
	b := external("mcp__a__b", "mcp__b__x", "mcp__a__y")
	if want := "mcp__a__b,mcp__a__y,mcp__b__x"; a != want || b != want {
		t.Errorf("external order = %q / %q, want %q", a, b, want)
	}
}
