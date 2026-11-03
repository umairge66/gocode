package toolrun

import (
	"context"
	"encoding/json"
	"math"
	"sync/atomic"

	"github.com/umairge66/gocode/internal/agent"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// WithToolSearch turns deferred tool loading on for the dispatcher: the
// non-core tools' schemas are marked Deferred (see toolsearch.IsDeferred) and
// the ToolSearch tool that loads them is registered. In auto mode the tools
// are deferred only while their definitions reach autoThresholdChars
// characters. ModeStandard (the zero value) leaves every schema undeferred
// and registers no ToolSearch.
func WithToolSearch(mode toolsearch.Mode, autoThresholdChars int) Option {
	return func(d *Dispatcher) {
		d.toolSearch = mode
		d.toolSearchAuto = autoThresholdChars
	}
}

// WithBrowserToolsPreloaded sends the browser tools' full definitions from
// the first request instead of deferring them behind ToolSearch. A session
// started with --chrome is there to use the browser, and live runs spent one
// to four model turns loading these tools before doing anything.
func WithBrowserToolsPreloaded(on bool) Option {
	return func(d *Dispatcher) { d.browserPreloaded = on }
}

// BrowserToolsPreloaded reports WithBrowserToolsPreloaded (for building a
// subagent's dispatcher the same way).
func (d *Dispatcher) BrowserToolsPreloaded() bool { return d.browserPreloaded }

// preloadableBrowserTools are the tools WithBrowserToolsPreloaded keeps loaded.
var preloadableBrowserTools = map[string]bool{
	string(protocol.ToolBrowserNavigate):    true,
	string(protocol.ToolBrowserReadPage):    true,
	string(protocol.ToolBrowserGetPageText): true,
	string(protocol.ToolBrowserTabsContext): true,
	string(protocol.ToolBrowserTabsCreate):  true,
	string(protocol.ToolBrowserTabsClose):   true,
	string(protocol.ToolBrowserScreenshot):  true,
	string(protocol.ToolBrowserComputer):    true,
	string(protocol.ToolBrowserFind):        true,
	string(protocol.ToolBrowserFormInput):   true,
	string(protocol.ToolBrowserBatch):       true,
}

// ToolSearchMode reports the dispatcher's deferred-loading mode and auto
// threshold (for building a subagent's dispatcher the same way).
func (d *Dispatcher) ToolSearchMode() (toolsearch.Mode, int) {
	return d.toolSearch, d.toolSearchAuto
}

// markDeferred sets Deferred on the schemas of deferrable tools when
// deferred loading is on, and drops the ToolSearch schema when it is off
// (auto mode below its threshold, or nothing to defer).
func (d *Dispatcher) markDeferred(schemas []agent.ToolSchema, external map[string]bool) []agent.ToolSchema {
	if d.toolSearch == toolsearch.ModeStandard {
		return schemas
	}
	if d.toolSearchDenied() {
		// ToolSearch is disallowed: nothing could load a deferred tool, so
		// every definition is sent in full.
		return d.undefer(schemas)
	}
	alwaysLoad, _ := d.External.(AlwaysLoadExternalTools)
	size := 0
	found := false
	for i := range schemas {
		name := schemas[i].Name
		if !d.isDeferred(name, external[name]) {
			continue
		}
		if external[name] && alwaysLoad != nil && alwaysLoad.AlwaysLoad(name) {
			// The server opted out of deferred loading ("alwaysLoad").
			continue
		}
		schemas[i].Deferred = true
		found = true
		size += len(name) + len(schemas[i].Description) + len(schemas[i].InputSchema)
	}
	if !found {
		return d.undefer(schemas)
	}
	if d.toolSearch != toolsearch.ModeAuto {
		return schemas
	}
	// Auto mode: once the deferred definitions reached the threshold, keep
	// deferring for the rest of the session (a stable tool list keeps the
	// prompt cache warm, and a tool table that shrinks a little does not
	// flip every definition back in).
	if d.toolSearchTripped.Load() || size >= d.toolSearchAuto {
		d.toolSearchTripped.Store(true)
		return schemas
	}
	return d.undefer(schemas)
}

// deferLatch is the sticky "auto mode has deferred tools" flag.
type deferLatch struct{ atomic.Bool }

// isDeferred applies the deferred set of this dispatcher's session kind.
func (d *Dispatcher) isDeferred(name string, mcp bool) bool {
	if d.browserPreloaded && !mcp && preloadableBrowserTools[name] {
		return false
	}
	if d.compact {
		return toolsearch.IsDeferredCompact(name, mcp)
	}
	return toolsearch.IsDeferred(name, mcp)
}

// undefer clears every Deferred mark and drops the ToolSearch schema.
func (d *Dispatcher) undefer(schemas []agent.ToolSchema) []agent.ToolSchema {
	out := schemas[:0]
	for _, s := range schemas {
		if s.Name == toolsearch.Name {
			continue
		}
		s.Deferred = false
		out = append(out, s)
	}
	return out
}

// toolSearchAdapter adapts the ToolSearch tool: it searches this
// dispatcher's own tool catalog, so a subagent finds only its own tools.
func (d *Dispatcher) toolSearchAdapter() adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in toolsearch.Input
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return "", "", "", nil
		},
		runLoads: func(ctx context.Context, input json.RawMessage) (string, []string, error) {
			var in toolsearch.Input
			if err := unmarshal(input, &in); err != nil {
				return "", nil, err
			}
			max := toolsearch.DefaultMaxMatch
			if in.MaxResults != nil && *in.MaxResults >= 1 && !math.IsInf(*in.MaxResults, 0) {
				max = int(math.Min(*in.MaxResults, 1000))
			}
			search := func() []string {
				var tools []toolsearch.Tool
				for _, s := range d.Schemas() {
					if s.Name == toolsearch.Name {
						continue
					}
					tools = append(tools, toolsearch.Tool{Name: s.Name, Description: s.Description, Deferred: s.Deferred})
				}
				return toolsearch.Search(in.Query, tools, max)
			}
			names := search()
			// MCP servers still connecting in the background may hold the
			// tool asked for: wait for them (bounded by the connect timeout)
			// before reporting a miss, so even a one-turn -p run finds it.
			if cw, ok := d.External.(ConnectingExternalTools); ok && toolsearch.Incomplete(in.Query, names) && cw.WaitConnecting(ctx) {
				names = search()
			}
			return toolsearch.ResultText(names), names, nil
		},
		description: toolsearch.Description,
		inputSchema: toolsearch.InputSchema,
	}
}

// registerToolSearch registers ToolSearch when deferred loading is on.
func (d *Dispatcher) registerToolSearch() {
	if d.toolSearch == toolsearch.ModeStandard {
		return
	}
	d.Register(protocol.ToolSearch, d.toolSearchAdapter())
}

// toolSearchDenied reports whether a deny rule covers every ToolSearch call.
func (d *Dispatcher) toolSearchDenied() bool {
	for _, r := range d.SnapshotRules() {
		if r.Behavior == toolreg.RuleDeny && r.ToolName == toolsearch.Name && (r.Content == "" || r.Content == "*") {
			return true
		}
	}
	return false
}
