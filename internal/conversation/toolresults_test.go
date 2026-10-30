package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// dumpTool is an external tool that returns a large text result.
type dumpTool struct{ out string }

func (d dumpTool) Tools() []agent.ToolSchema {
	return []agent.ToolSchema{{Name: "mcp__srv__dump", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
func (d dumpTool) Has(name string) bool { return name == "mcp__srv__dump" }
func (d dumpTool) Call(context.Context, string, json.RawMessage) (string, bool, error) {
	return d.out, false, nil
}

// toolThenDoneModel asks for the dump tool on the first call, then ends the
// turn; it records every request.
type toolThenDoneModel struct{ reqs []agent.ModelRequest }

func (m *toolThenDoneModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.reqs = append(m.reqs, req)
	if len(m.reqs) == 1 {
		return agent.ModelResponse{
			Message:    agent.AssistantMessage(agent.ToolUseBlock("toolu_big", "mcp__srv__dump", json.RawMessage(`{}`))),
			StopReason: agent.StopToolUse,
		}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
}

// An oversized tool result never enters the conversation in full: the next
// request (and the history) carry the saved-output preview, and the complete
// text is in the session's results directory.
func TestOversizedToolResultIsSavedNotResent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tool-results")
	full := strings.Repeat("0123456789abcdef\n", 6000) // 102000 chars
	d := toolrun.New(toolreg.ModeBypassPermissions, nil,
		toolrun.WithExternalTools(dumpTool{out: full}),
		toolrun.WithToolResultsDir(dir))
	model := &toolThenDoneModel{}
	c := New(model, d)
	c.ToolSchemas = d.Schemas()

	if _, err := c.Submit(context.Background(), "dump it"); err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("model called %d times, want 2", len(model.reqs))
	}
	var sent string
	for _, m := range model.reqs[1].Messages {
		for _, b := range m.Content {
			if b.ToolUseID == "toolu_big" {
				sent = b.Content
			}
		}
	}
	if !strings.HasPrefix(sent, "<persisted-output>\nOutput too large (99.6KB). Full output saved to: "+filepath.Join(dir, "toolu_big.txt")) {
		t.Fatalf("second request carried %d chars: %q", len(sent), sent[:min(len(sent), 200)])
	}
	if len(sent) > 2500 {
		t.Fatalf("second request carried %d chars for the result", len(sent))
	}
	saved, err := os.ReadFile(filepath.Join(dir, "toolu_big.txt"))
	if err != nil || string(saved) != full {
		t.Fatalf("saved result: %d bytes, %v", len(saved), err)
	}
	for _, m := range c.History {
		for _, b := range m.Content {
			if len(b.Content) > 2500 {
				t.Fatalf("history kept a %d-char block", len(b.Content))
			}
		}
	}
}
