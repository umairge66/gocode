package toolrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/chromehost"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// A click asks for a screenshot after itself by default and the image comes
// back on the tool_result; with the setting off it is not asked for and the
// descriptions say how to ask.
func TestDispatch_ComputerScreenshotAfterAction(t *testing.T) {
	reply := json.RawMessage(`{"action":"left_click","ok":true,"delivery":"input","dataUrl":"data:image/jpeg;base64,aW1n","shot":{"width":800,"height":600}}`)
	run := func(opts ...Option) (*recordingBrowserClient, agent.ToolResult, *Dispatcher) {
		c := &recordingBrowserClient{fakeBrowserClient: fakeBrowserClient{byMethod: map[string]json.RawMessage{chromehost.MethodComputer: reply}}}
		d := New(toolreg.ModeBypassPermissions, nil, append([]Option{WithBrowserClient(c)}, opts...)...)
		res, err := d.Dispatch(context.Background(), agent.ToolUse{Name: string(protocol.ToolBrowserComputer),
			Input: json.RawMessage(`{"action":"left_click","coordinate":[10,20]}`)})
		if err != nil || res.IsError {
			t.Fatalf("dispatch: %v %+v", err, res)
		}
		return c, res, d
	}
	description := func(d *Dispatcher, tool protocol.Tool) string {
		for _, s := range d.Schemas() {
			if s.Name == string(tool) {
				return s.Description
			}
		}
		return ""
	}

	c, res, d := run()
	if !strings.Contains(string(c.params[0]), `"screenshot":true`) {
		t.Errorf("default: sent %s", c.params[0])
	}
	if !strings.HasPrefix(res.Content, "computer: left_click done. Screenshot after the action attached.") ||
		len(res.Images) != 1 || res.Images[0].MediaType != "image/jpeg" {
		t.Errorf("default result: %q images %+v", res.Content, res.Images)
	}
	if !strings.Contains(description(d, protocol.ToolBrowserComputer), "you do not need a separate screenshot call") ||
		!strings.Contains(description(d, protocol.ToolBrowserBatch), "only for the last step") {
		t.Errorf("default descriptions:\n%s\n%s", description(d, protocol.ToolBrowserComputer), description(d, protocol.ToolBrowserBatch))
	}

	c, _, d = run(WithBrowserScreenshotAfterActions(false))
	if strings.Contains(string(c.params[0]), "screenshot") {
		t.Errorf("off: sent %s", c.params[0])
	}
	if desc := description(d, protocol.ToolBrowserComputer); strings.Contains(desc, "separate screenshot call") || !strings.Contains(desc, "Pass screenshot:true") {
		t.Errorf("off description: %s", desc)
	}
	if strings.Contains(description(d, protocol.ToolBrowserBatch), "screenshot of the page after it") {
		t.Errorf("off batch description: %s", description(d, protocol.ToolBrowserBatch))
	}
}
