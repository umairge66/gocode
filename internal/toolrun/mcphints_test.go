package toolrun

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// The MCP host reports read-only tools and alwaysLoad servers.
var (
	_ ReadOnlyExternalTools   = (*mcphost.Host)(nil)
	_ AlwaysLoadExternalTools = (*mcphost.Host)(nil)
)

func TestReadOnlyExternalToolsAreConcurrencySafe(t *testing.T) {
	ext := &richExternal{
		schemas:  append(oneTool("mcp__s__read"), oneTool("mcp__s__write")...),
		readOnly: map[string]bool{"mcp__s__read": true},
	}
	d := New(toolreg.ModeDefault, nil, WithExternalTools(ext))
	if !d.IsConcurrencySafe(call("mcp__s__read", `{}`)) {
		t.Error("read-only MCP tool should be concurrency-safe")
	}
	if d.IsConcurrencySafe(call("mcp__s__write", `{}`)) {
		t.Error("MCP tool without readOnlyHint should not be concurrency-safe")
	}
	// Without the optional interface nothing external is concurrency-safe.
	if New(toolreg.ModeDefault, nil, WithExternalTools(&fakeExternal{})).IsConcurrencySafe(call("mcp__srv__echo", `{}`)) {
		t.Error("plain provider tool should not be concurrency-safe")
	}
	// A tool the dispatcher is restricted from is not.
	d.Restrict([]string{"Read"})
	if d.IsConcurrencySafe(call("mcp__s__read", `{}`)) {
		t.Error("restricted tool reported concurrency-safe")
	}
}

func TestAlwaysLoadServerToolsAreNotDeferred(t *testing.T) {
	ext := &richExternal{
		schemas:    append(oneTool("mcp__keep__t"), oneTool("mcp__lazy__t")...),
		alwaysLoad: map[string]bool{"mcp__keep__t": true},
	}
	d := New(toolreg.ModeDefault, nil, WithExternalTools(ext), WithToolSearch(toolsearch.ModeOn, 0))
	flags := schemaFlags(d)
	if flags["mcp__keep__t"] {
		t.Error("alwaysLoad tool was deferred")
	}
	if !flags["mcp__lazy__t"] {
		t.Error("other MCP tool should be deferred")
	}
}

// deferredSize sums the definitions marked Deferred, as markDeferred does.
func deferredSize(schemas []agent.ToolSchema) int {
	n := 0
	for _, s := range schemas {
		if s.Deferred {
			n += len(s.Name) + len(s.Description) + len(s.InputSchema)
		}
	}
	return n
}

func TestToolSearchAutoStaysOnOnceTripped(t *testing.T) {
	small := oneTool("mcp__s__t")
	base := deferredSize(New(toolreg.ModeDefault, nil, WithExternalTools(&richExternal{schemas: small}), WithToolSearch(toolsearch.ModeOn, 0)).Schemas())
	threshold := base + 500

	ext := &richExternal{schemas: small}
	d := New(toolreg.ModeDefault, nil, WithExternalTools(ext), WithToolSearch(toolsearch.ModeAuto, threshold))
	if schemaFlags(d)["mcp__s__t"] {
		t.Fatal("deferred below the threshold")
	}
	big := oneTool("mcp__s__t")
	big[0].Description = strings.Repeat("x", 1000)
	ext.mu.Lock()
	ext.schemas = big
	ext.mu.Unlock()
	if !schemaFlags(d)["mcp__s__t"] {
		t.Fatal("not deferred above the threshold")
	}
	ext.mu.Lock()
	ext.schemas = small
	ext.mu.Unlock()
	flags := schemaFlags(d)
	if !flags["mcp__s__t"] {
		t.Error("deferral switched off again after the catalog shrank")
	}
	if _, ok := flags[toolsearch.Name]; !ok {
		t.Error("ToolSearch dropped after deferral latched")
	}
}
