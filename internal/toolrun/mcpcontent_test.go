package toolrun

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/toolreg"
)

// The MCP host returns rich (image-carrying, uncut) results.
var _ ContentExternalTools = (*mcphost.Host)(nil)

// richExternal implements ExternalTools plus every optional provider
// interface (ContentExternalTools, ReadOnlyExternalTools,
// AlwaysLoadExternalTools), the way mcphost.Host does.
type richExternal struct {
	mu         sync.Mutex
	schemas    []agent.ToolSchema
	result     agent.ToolResult
	maxChars   int
	readOnly   map[string]bool
	alwaysLoad map[string]bool
}

func (f *richExternal) Tools() []agent.ToolSchema {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.ToolSchema(nil), f.schemas...)
}

func (f *richExternal) Has(name string) bool {
	for _, s := range f.Tools() {
		if s.Name == name {
			return true
		}
	}
	return false
}

func (f *richExternal) Call(context.Context, string, json.RawMessage) (string, bool, error) {
	return "text-only seam", false, nil
}

func (f *richExternal) CallContent(context.Context, string, json.RawMessage) (agent.ToolResult, error) {
	return f.result, nil
}

func (f *richExternal) MaxResultChars() int         { return f.maxChars }
func (f *richExternal) ReadOnly(name string) bool   { return f.readOnly[name] }
func (f *richExternal) AlwaysLoad(name string) bool { return f.alwaysLoad[name] }

func oneTool(name string) []agent.ToolSchema {
	return []agent.ToolSchema{{Name: name, Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}

func pngBase64(t *testing.T, w, h int) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestExternalImagesReachTheModel(t *testing.T) {
	ext := &richExternal{schemas: oneTool("mcp__s__shot"), result: agent.ToolResult{
		Content: "a screenshot",
		Images: []agent.ToolResultImage{
			{Base64: pngBase64(t, 4, 4), MediaType: "image/png"},
			{Base64: "PHN2Zz48L3N2Zz4=", MediaType: "image/svg+xml"},
			{Base64: "%%%not base64", MediaType: "image/png"},
		},
	}}
	d := New(toolreg.ModeBypassPermissions, nil, WithExternalTools(ext))
	res := dispatch(t, d, "mcp__s__shot", map[string]any{})
	if res.IsError {
		t.Fatalf("error: %q", res.Content)
	}
	if len(res.Images) != 1 || res.Images[0].MediaType != "image/png" {
		t.Fatalf("images = %+v", res.Images)
	}
	if !strings.HasPrefix(res.Content, "a screenshot\n[Image: image/svg+xml - not shown: ") || strings.Count(res.Content, "not shown") != 2 {
		t.Fatalf("text = %q", res.Content)
	}
}

func TestExternalLargeImageIsFittedToTheReadBudget(t *testing.T) {
	ext := &richExternal{schemas: oneTool("mcp__s__shot"), result: agent.ToolResult{
		Images: []agent.ToolResultImage{{Base64: pngBase64(t, 3000, 200), MediaType: "image/png"}},
	}}
	d := New(toolreg.ModeBypassPermissions, nil, WithExternalTools(ext))
	res := dispatch(t, d, "mcp__s__shot", map[string]any{})
	if len(res.Images) != 1 {
		t.Fatalf("images = %d (%q)", len(res.Images), res.Content)
	}
	data, _ := base64.StdEncoding.DecodeString(res.Images[0].Base64)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > 1568 {
		t.Fatalf("image not resized: %+v %v", cfg, err)
	}
}

func TestOversizedExternalTextIsSavedInFull(t *testing.T) {
	dir := t.TempDir()
	full := strings.Repeat("0123456789", 300) // 3000 chars, over the 1000 budget
	ext := &richExternal{schemas: oneTool("mcp__s__dump"), maxChars: 1000, result: agent.ToolResult{
		Content: full,
		Images:  []agent.ToolResultImage{{Base64: pngBase64(t, 2, 2), MediaType: "image/png"}},
	}}
	d := New(toolreg.ModeBypassPermissions, nil, WithExternalTools(ext), WithToolResultsDir(dir))
	res := dispatch(t, d, "mcp__s__dump", map[string]any{})
	if !strings.HasPrefix(res.Content, persistedOutputOpen) {
		t.Fatalf("not saved: %q", res.Content)
	}
	if len(res.Images) != 1 {
		t.Fatalf("images dropped: %d", len(res.Images))
	}
	saved, err := os.ReadFile(dir + "/tu_mcp__s__dump.txt")
	if err != nil || string(saved) != full {
		t.Fatalf("saved file holds %d chars (%v), want the full %d", len(saved), err, len(full))
	}
}

func TestOversizedExternalTextIsCutOnlyWhenItCannotBeSaved(t *testing.T) {
	ext := &richExternal{schemas: oneTool("mcp__s__dump"), maxChars: 1000, result: agent.ToolResult{Content: strings.Repeat("y", 3000)}}
	d := New(toolreg.ModeBypassPermissions, nil, WithExternalTools(ext))
	res := dispatch(t, d, "mcp__s__dump", map[string]any{})
	if !strings.HasPrefix(res.Content, truncatedOutputOpen) {
		t.Fatalf("not cut: %q", res.Content[:60])
	}
	if n := utf8.RuneCountInString(res.Content); n > 1000 {
		t.Fatalf("cut result is %d chars, over the 1000 budget", n)
	}
	// Within budget: untouched.
	ext.result.Content = strings.Repeat("y", 900)
	if res := dispatch(t, d, "mcp__s__dump", map[string]any{}); res.Content != ext.result.Content {
		t.Fatal("result within budget was rewritten")
	}
}
