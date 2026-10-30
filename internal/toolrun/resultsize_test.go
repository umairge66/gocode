package toolrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/exec"
	"github.com/umairge66/gocode/internal/tools/fileedit"
)

// numberedLines returns "line 0\nline 1\n…" of at least n characters.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// fixedOutput registers tool as a fake returning out (with images, if any).
func fixedOutput(d *Dispatcher, tool protocol.Tool, out string, images []agent.ToolResultImage) {
	a := adapter{
		permission:  func(json.RawMessage) (string, string, pathguard.Operation, error) { return "", "", "", nil },
		inputSchema: json.RawMessage(`{"type":"object"}`),
	}
	if images != nil {
		a.runImages = func(context.Context, json.RawMessage) (string, []agent.ToolResultImage, error) {
			return out, images, nil
		}
	} else {
		a.run = func(context.Context, json.RawMessage) (string, error) { return out, nil }
	}
	d.Register(tool, a)
}

func TestOversizedBashResultIsSavedWithPreview(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tool-results")
	d := New(toolreg.ModeBypassPermissions, nil, WithToolResultsDir(dir))
	full := numberedLines(BashMaxResultSizeChars + 5000)
	fixedOutput(d, protocol.ToolBash, full, nil)

	res := dispatch(t, d, string(protocol.ToolBash), map[string]any{})
	path := filepath.Join(dir, "tu_Bash.txt")
	wantHead := "<persisted-output>\nOutput too large (" + formatSize(len(full)) + "). Full output saved to: " + path + "\n\nPreview (first 2KB):\nline 0\n"
	if !strings.HasPrefix(res.Content, wantHead) {
		t.Fatalf("head:\n%q\nwant prefix\n%q", res.Content[:200], wantHead)
	}
	if !strings.HasSuffix(res.Content, "\n...\n</persisted-output>") {
		t.Fatalf("tail: %q", res.Content[len(res.Content)-80:])
	}
	// The preview ends on a whole line, within the 2000-character window.
	body := strings.TrimSuffix(strings.SplitN(res.Content, "Preview (first 2KB):\n", 2)[1], "\n...\n</persisted-output>")
	if len(body) > ResultPreviewChars || !strings.HasPrefix(full, body+"\n") {
		t.Fatalf("preview (%d chars) is not a line-aligned prefix", len(body))
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != full {
		t.Fatalf("saved %d bytes, want the full %d", len(saved), len(full))
	}
	if len(res.Content) > 2600 {
		t.Fatalf("inline result is %d chars", len(res.Content))
	}
}

func TestResultLimitsPerTool(t *testing.T) {
	cases := []struct {
		tool  protocol.Tool
		size  int
		saved bool
	}{
		{protocol.ToolBash, BashMaxResultSizeChars, false},
		{protocol.ToolBash, BashMaxResultSizeChars + 1, true},
		{protocol.ToolGrep, GrepMaxResultSizeChars, false},
		{protocol.ToolGrep, GrepMaxResultSizeChars + 1, true},
		{protocol.ToolGlob, GrepMaxResultSizeChars + 1, false},
		{protocol.ToolGlob, MaxResultSizeChars + 1, true},
		{protocol.ToolWebFetch, MaxResultSizeChars + 1, true},
		{protocol.ToolRead, 3 * MaxResultSizeChars, false},
	}
	for _, c := range cases {
		dir := t.TempDir()
		d := New(toolreg.ModeBypassPermissions, nil, WithToolResultsDir(dir))
		// Lines of normal length, so only the size limit applies.
		out := strings.Repeat(strings.Repeat("x", 99)+"\n", c.size/100) + strings.Repeat("x", c.size%100)
		fixedOutput(d, c.tool, out, nil)
		res := dispatch(t, d, string(c.tool), map[string]any{})
		got := strings.HasPrefix(res.Content, persistedOutputOpen)
		if got != c.saved {
			t.Errorf("%s with %d chars: saved=%v, want %v", c.tool, c.size, got, c.saved)
		}
		if !got && res.Content != out {
			t.Errorf("%s with %d chars: content changed", c.tool, c.size)
		}
	}
}

func TestOversizedExternalResultIsSaved(t *testing.T) {
	dir := t.TempDir()
	ext := &bigExternal{name: "mcp__srv__dump", out: strings.Repeat("y", MaxResultSizeChars+10)}
	d := New(toolreg.ModeBypassPermissions, nil, WithExternalTools(ext), WithToolResultsDir(dir))
	res := dispatch(t, d, "mcp__srv__dump", map[string]any{})
	if !strings.HasPrefix(res.Content, persistedOutputOpen) {
		t.Fatalf("external result not saved: %q", res.Content[:80])
	}
}

type bigExternal struct{ name, out string }

func (f *bigExternal) Tools() []agent.ToolSchema {
	return []agent.ToolSchema{{Name: f.name, InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
func (f *bigExternal) Has(name string) bool { return name == f.name }
func (f *bigExternal) Call(context.Context, string, json.RawMessage) (string, bool, error) {
	return f.out, false, nil
}

func TestOversizedResultWithoutDirectoryIsCut(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	full := numberedLines(BashMaxResultSizeChars * 3)
	fixedOutput(d, protocol.ToolBash, full, nil)
	res := dispatch(t, d, string(protocol.ToolBash), map[string]any{})
	if !strings.HasPrefix(res.Content, "<truncated-output>\nOutput too large ("+formatSize(len(full))+"). It could not be saved, so only the first ") {
		t.Fatalf("head: %q", res.Content[:160])
	}
	if !strings.Contains(res.Content, "the rest was dropped. If the tool can page or filter its results, call it again for the part you need.\n\nline 0\n") {
		t.Fatalf("frame: %q", res.Content[:300])
	}
	if !strings.HasSuffix(res.Content, "\n</truncated-output>") {
		t.Fatalf("tail: %q", res.Content[len(res.Content)-40:])
	}
	if n := utf8.RuneCountInString(res.Content); n > BashMaxResultSizeChars {
		t.Fatalf("cut result is %d chars, over the %d limit", n, BashMaxResultSizeChars)
	}
	if n := utf8.RuneCountInString(res.Content); n < BashMaxResultSizeChars-500 {
		t.Fatalf("cut result is only %d chars", n)
	}
}

func TestResultsWithImagesAreNotRewritten(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil, WithToolResultsDir(t.TempDir()))
	out := strings.Repeat("z", MaxResultSizeChars+1)
	fixedOutput(d, protocol.ToolBrowserScreenshot, out, []agent.ToolResultImage{{MediaType: "image/png", Base64: "AAAA"}})
	res := dispatch(t, d, string(protocol.ToolBrowserScreenshot), map[string]any{})
	if res.Content != out || len(res.Images) != 1 {
		t.Fatalf("image result rewritten: %d chars, %d images", len(res.Content), len(res.Images))
	}
}

func TestPersistReusesExistingFileAndRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	p1, err := persistToolResult(dir, "id1", "first")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := persistToolResult(dir, "id1", "first")
	if err != nil || p2 != p1 {
		t.Fatalf("second save: %q %v", p2, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "id2.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := persistToolResult(dir, "id2", "x"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("symlinked result path accepted: %v", err)
	}
}

func TestToolResultFileName(t *testing.T) {
	for in, want := range map[string]string{
		"toolu_01AbC-x": "toolu_01AbC-x.txt",
		"a/b.c":         "a%2Fb.c.txt",
		".hidden":       "%2Ehidden.txt",
		"":              "%.txt",
		"call 1":        "call%201.txt",
	} {
		if got := toolResultFileName(in); got != want {
			t.Errorf("toolResultFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int]string{512: "512 bytes", 1023: "1023 bytes", 1024: "1KB", 1536: "1.5KB", 2000: "2KB", 108894: "106.3KB", 1 << 20: "1MB", 1 << 30: "1GB"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestResultPreviewCutsAtLineBreakPastMiddle(t *testing.T) {
	s := strings.Repeat("a", 1500) + "\n" + strings.Repeat("b", 1000)
	if p, more := resultPreview(s, 2000); p != strings.Repeat("a", 1500) || !more {
		t.Fatalf("preview = %d chars, more=%v", len(p), more)
	}
	// A break in the first half is ignored: the window is cut at 2000.
	s = strings.Repeat("a", 500) + "\n" + strings.Repeat("b", 3000)
	if p, _ := resultPreview(s, 2000); utf8.RuneCountInString(p) != 2000 {
		t.Fatalf("preview = %d chars, want 2000", len(p))
	}
	if p, more := resultPreview("short", 2000); p != "short" || more {
		t.Fatalf("short preview = %q %v", p, more)
	}
}

func TestSavedResultsAreReadableInDefaultMode(t *testing.T) {
	proj, outside, other := outsideFixture(t)
	results := filepath.Join(outside, "tool-results")
	if err := os.MkdirAll(results, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(results, "x.txt")
	if err := os.WriteFile(saved, []byte("saved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := recordPrompter(PermissionDenied, nil)
	d := New(toolreg.ModeDefault, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithPermissionPrompter(p), WithToolResultsDir(results))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": saved}); res.IsError {
		t.Fatalf("saved result read refused: %+v", res)
	}
	if len(p.requests) != 0 {
		t.Fatalf("saved result read prompted: %+v", p.requests)
	}
	// Only the results directory is opened up, not its parent.
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": other}); !res.IsError {
		t.Fatalf("read outside the results directory allowed: %+v", res)
	}
}

// --- whole-number inputs ----------------------------------------------------

func TestUnmarshalAcceptsWholeNumberFloats(t *testing.T) {
	for _, raw := range []string{
		`{"command":"sleep 1.0","timeout":120000.0}`,
		`{"command":"sleep 1.0","timeout":1.2e5}`,
		`{"command":"sleep 1.0","timeout":120000}`,
	} {
		var in exec.BashInput
		if err := unmarshal(json.RawMessage(raw), &in); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if in.Timeout != 120000 || in.Command != "sleep 1.0" {
			t.Fatalf("%s decoded to %+v", raw, in)
		}
	}
	var in exec.BashInput
	if err := unmarshal(json.RawMessage(`{"command":"x","timeout":1.5}`), &in); err == nil {
		t.Fatal("fractional timeout accepted")
	}
	if err := unmarshal(json.RawMessage(`{"command":"x","timeout":"soon"}`), &in); err == nil {
		t.Fatal("string timeout accepted")
	}
}

func TestReadAcceptsWholeNumberOffsetAndLimit(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(toolreg.ModeBypassPermissions, nil)
	res, err := d.Dispatch(context.Background(), agent.ToolUse{
		ID:    "r1",
		Name:  string(protocol.ToolRead),
		Input: json.RawMessage(`{"file_path":` + mustQuote(file) + `,"offset":2.0,"limit":1.0}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("Read with float offset/limit: %+v %v", res, err)
	}
	if res.Content != "2\ttwo" {
		t.Fatalf("content = %q", res.Content)
	}
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- disabled tools ---------------------------------------------------------

func TestDisabledToolsAreNotOfferedOrRun(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil, WithDisabledTools(string(protocol.ToolAskUserQuestion), string(protocol.ToolBrowserNavigate)))
	for _, s := range d.Schemas() {
		if s.Name == string(protocol.ToolAskUserQuestion) || s.Name == string(protocol.ToolBrowserNavigate) {
			t.Fatalf("disabled tool %s offered", s.Name)
		}
	}
	res := dispatch(t, d, string(protocol.ToolAskUserQuestion), map[string]any{})
	if !res.IsError || !strings.Contains(res.Content, "No such tool available: AskUserQuestion") {
		t.Fatalf("disabled tool ran: %+v", res)
	}
	// Restrict does not lift a disabled tool.
	d.Restrict(nil)
	if res := dispatch(t, d, string(protocol.ToolBrowserNavigate), map[string]any{}); !res.IsError {
		t.Fatalf("disabled tool ran after Restrict(nil): %+v", res)
	}
	if got := strings.Join(d.DisabledTools(), ","); got != "AskUserQuestion,navigate" {
		t.Fatalf("DisabledTools = %q", got)
	}
	// Still offered without the option.
	found := false
	for _, s := range New(toolreg.ModeBypassPermissions, nil).Schemas() {
		found = found || s.Name == string(protocol.ToolAskUserQuestion)
	}
	if !found {
		t.Fatal("AskUserQuestion missing from a default dispatcher")
	}
}

// The Read tool's own budget keeps whole-file reads small enough that the
// result layer never needs to touch them.
func TestReadIsNotRewrittenByResultLimit(t *testing.T) {
	if resultLimit(fileedit.ToolNameRead) != 0 {
		t.Fatal("Read has an inline result limit")
	}
}
