package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/fileedit"
	"github.com/umairge66/gocode/internal/tools/web"
)

// TestReadEditRoundTrip: Edit without a prior Read is refused; after Read the
// Edit applies and a second Edit needs no re-read.
func TestReadEditRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(toolreg.ModeBypassPermissions, nil)

	edit := map[string]any{"file_path": file, "old_string": "beta", "new_string": "gamma"}
	res := dispatch(t, d, string(protocol.ToolEdit), edit)
	if !res.IsError || !strings.Contains(res.Content, fileedit.MsgFileNotRead) {
		t.Fatalf("Edit without Read = %+v, want is_error %q", res, fileedit.MsgFileNotRead)
	}
	if b, _ := os.ReadFile(file); string(b) != "alpha\nbeta\n" {
		t.Fatalf("refused Edit modified the file: %q", b)
	}

	res = dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file})
	if res.IsError || !strings.Contains(res.Content, "alpha") || !strings.Contains(res.Content, "beta") {
		t.Fatalf("Read = %+v", res)
	}

	res = dispatch(t, d, string(protocol.ToolEdit), edit)
	if res.IsError || !strings.Contains(res.Content, "updated successfully") {
		t.Fatalf("Edit after Read = %+v", res)
	}
	res = dispatch(t, d, string(protocol.ToolEdit), map[string]any{"file_path": file, "old_string": "alpha", "new_string": "omega"})
	if res.IsError {
		t.Fatalf("second Edit demanded a re-read: %+v", res)
	}
	if b, _ := os.ReadFile(file); string(b) != "omega\ngamma\n" {
		t.Fatalf("file = %q", b)
	}

	// An external change after the read is detected.
	if err := os.WriteFile(file, []byte("changed externally\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = dispatch(t, d, string(protocol.ToolEdit), map[string]any{"file_path": file, "old_string": "changed", "new_string": "x"})
	if !res.IsError {
		t.Fatalf("Edit after external modification should be refused: %+v", res)
	}
}

// TestWriteRequiresReadAndRefreshesTracker: Write of an existing unread file is
// refused (the shared tracker replaced AllowAllReads); a Write that creates a
// file records it so a follow-up Edit works without a re-read.
func TestWriteRequiresReadAndRefreshesTracker(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := fileedit.NewFileStateTracker()
	d := New(toolreg.ModeBypassPermissions, nil, WithFileState(tracker))
	if d.FileState != tracker {
		t.Fatal("WithFileState not honoured")
	}

	res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": existing, "content": "y"})
	if !res.IsError {
		t.Fatalf("Write over an unread file should be refused: %+v", res)
	}

	file := filepath.Join(dir, "new.txt")
	res = dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": file, "content": "one two\n"})
	if res.IsError {
		t.Fatalf("Write = %+v", res)
	}
	if !tracker.WasRead(file) {
		t.Fatal("Write did not refresh the shared tracker")
	}
	res = dispatch(t, d, string(protocol.ToolEdit), map[string]any{"file_path": file, "old_string": "two", "new_string": "three"})
	if res.IsError {
		t.Fatalf("Edit after Write demanded a re-read: %+v", res)
	}
	// And Write again (overwrite) after the Edit works too.
	res = dispatch(t, d, string(protocol.ToolWrite), map[string]any{"file_path": file, "content": "final\n"})
	if res.IsError {
		t.Fatalf("Write after Edit = %+v", res)
	}
	if b, _ := os.ReadFile(file); string(b) != "final\n" {
		t.Fatalf("file = %q", b)
	}
}

func TestMultiEdit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "m.txt")
	if err := os.WriteFile(file, []byte("a b c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(toolreg.ModeBypassPermissions, nil)
	in := map[string]any{"file_path": file, "edits": []map[string]any{
		{"old_string": "a", "new_string": "A"},
		{"old_string": "c", "new_string": "C"},
	}}
	if res := dispatch(t, d, string(protocol.ToolMultiEdit), in); !res.IsError {
		t.Fatalf("MultiEdit without Read should be refused: %+v", res)
	}
	dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file})
	res := dispatch(t, d, string(protocol.ToolMultiEdit), in)
	if res.IsError || !strings.Contains(res.Content, "Applied 2 edits") {
		t.Fatalf("MultiEdit = %+v", res)
	}
	if b, _ := os.ReadFile(file); string(b) != "A b C\n" {
		t.Fatalf("file = %q", b)
	}
}

// TestReadImageNote: an image Read returns a text note (tool_result content is
// a string, so no image block can be attached).
// TestReadImageStructured: an image Read maps fileedit.ReadResult.Images onto
// the structured ToolResult (image blocks carried alongside the text caption),
// not flattened to a text note.
func TestReadImageStructured(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "p.png")
	if err := os.WriteFile(file, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(toolreg.ModeBypassPermissions, nil)
	res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file})
	if res.IsError || !strings.Contains(res.Content, "Image file read") {
		t.Fatalf("image Read = %+v", res)
	}
	if len(res.Images) != 1 {
		t.Fatalf("image Read Images = %d, want 1 (%+v)", len(res.Images), res)
	}
	if res.Images[0].MediaType != "image/png" || res.Images[0].Base64 == "" {
		t.Fatalf("image block = %+v, want image/png with base64 data", res.Images[0])
	}
}

// TestReadTextNoImages: a normal text Read carries no image blocks (the string
// result path is unchanged).
func TestReadTextNoImages(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(toolreg.ModeBypassPermissions, nil)
	res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file})
	if res.IsError || len(res.Images) != 0 || !strings.Contains(res.Content, "alpha") {
		t.Fatalf("text Read = %+v, want no images", res)
	}
}

// TestEditPermissionsPathAndAcceptEdits: Edit is keyed by file_path and
// auto-allowed in acceptEdits; plan mode refuses it; a deny rule on the path
// wins.
func TestEditPermissionsPathAndAcceptEdits(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "p.txt")
	os.WriteFile(file, []byte("q\n"), 0o644)

	d := New(toolreg.ModeAcceptEdits, toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Read"}})
	dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file})
	if res := dispatch(t, d, string(protocol.ToolEdit), map[string]any{"file_path": file, "old_string": "q", "new_string": "r"}); res.IsError {
		t.Fatalf("acceptEdits should allow Edit: %+v", res)
	}

	deny := New(toolreg.ModeBypassPermissions, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Edit", Content: file}})
	if res := dispatch(t, deny, string(protocol.ToolEdit), map[string]any{"file_path": file, "old_string": "r", "new_string": "s"}); !res.IsError || !strings.Contains(res.Content, "denied") {
		t.Fatalf("deny rule on file_path not applied: %+v", res)
	}

	plan := New(toolreg.ModePlan, nil)
	if res := dispatch(t, plan, string(protocol.ToolMultiEdit), map[string]any{"file_path": file, "edits": []map[string]any{{"old_string": "r", "new_string": "s"}}}); !res.IsError {
		t.Fatalf("plan mode should refuse MultiEdit: %+v", res)
	}
}

// fakeWebDoer serves a fixed HTML page and records the requested URL.
type fakeWebDoer struct{ got string }

func (f *fakeWebDoer) Do(req *http.Request) (*http.Response, error) {
	f.got = req.URL.String()
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("<html><body><h1>Title</h1><p>Hello web.</p></body></html>")),
		Request:    req,
	}, nil
}

func TestWebFetchWithSummarizerAndDomainRule(t *testing.T) {
	var gotContent, gotPrompt string
	sum := web.SummarizerFunc(func(_ context.Context, content, prompt string) (string, error) {
		gotContent, gotPrompt = content, prompt
		return "summary: " + prompt, nil
	})
	rules := toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "WebFetch", Content: "domain:example.com"}}
	d := New(toolreg.ModeDefault, rules, WithWebFetchSummarizer(sum))
	if d.WebFetchSummarizer == nil {
		t.Fatal("WithWebFetchSummarizer not honoured")
	}
	doer := &fakeWebDoer{}
	tool := web.NewWebFetchTool(d.WebFetchSummarizer)
	tool.Doer = doer
	tool.Guard = func(*url.URL) error { return nil }
	d.Register(protocol.ToolWebFetch, WebFetchAdapter(tool))

	res := dispatch(t, d, string(protocol.ToolWebFetch), map[string]any{"url": "https://Example.com/page", "prompt": "what is it?"})
	if res.IsError || res.Content != "summary: what is it?" {
		t.Fatalf("WebFetch = %+v", res)
	}
	if !strings.Contains(gotContent, "Hello web.") || gotPrompt != "what is it?" {
		t.Fatalf("summarizer saw content=%q prompt=%q", gotContent, gotPrompt)
	}
	if !strings.HasPrefix(doer.got, "https://example.com/page") && !strings.HasPrefix(doer.got, "https://Example.com/page") {
		t.Fatalf("doer requested %q", doer.got)
	}

	// Another host is not covered by the allow rule: default mode asks -> refused.
	res = dispatch(t, d, string(protocol.ToolWebFetch), map[string]any{"url": "https://other.org/", "prompt": "x"})
	if !res.IsError {
		t.Fatalf("WebFetch to an un-allowed domain should not run: %+v", res)
	}

	// Summarizer failure surfaces as an is_error result.
	failing := web.NewWebFetchTool(web.SummarizerFunc(func(context.Context, string, string) (string, error) {
		return "", errors.New("model down")
	}))
	failing.Doer, failing.Guard = &fakeWebDoer{}, func(*url.URL) error { return nil }
	d.Register(protocol.ToolWebFetch, WebFetchAdapter(failing))
	res = dispatch(t, d, string(protocol.ToolWebFetch), map[string]any{"url": "https://example.com/x", "prompt": "p"})
	if !res.IsError || !strings.Contains(res.Content, "model down") {
		t.Fatalf("summarizer error = %+v", res)
	}
}

func TestWebFetchRuleContent(t *testing.T) {
	if got := WebFetchRuleContent("https://Docs.Example.com:8443/a?b"); got != "domain:docs.example.com" {
		t.Errorf("got %q", got)
	}
	if got := WebFetchRuleContent("::bad"); !strings.HasPrefix(got, "input:") {
		t.Errorf("got %q, want input: fallback", got)
	}
}

type fakeBackend struct{ query string }

func (f *fakeBackend) Search(_ context.Context, q string, _, _ []string) ([]web.Result, error) {
	f.query = q
	return []web.Result{{Title: "Go", URL: "https://go.dev/"}}, nil
}

func TestWebSearchBackendAndNotConfigured(t *testing.T) {
	be := &fakeBackend{}
	d := New(toolreg.ModeBypassPermissions, nil, WithSearchBackend(be))
	res := dispatch(t, d, string(protocol.ToolWebSearch), map[string]any{"query": "golang"})
	if res.IsError || be.query != "golang" || !strings.Contains(res.Content, `"url":"https://go.dev/"`) || !strings.Contains(res.Content, web.SearchSourcesReminder) {
		t.Fatalf("WebSearch = %+v (query %q)", res, be.query)
	}

	t.Setenv(web.BraveAPIKeyEnv, "")
	unconf := New(toolreg.ModeBypassPermissions, nil)
	res = dispatch(t, unconf, string(protocol.ToolWebSearch), map[string]any{"query": "golang"})
	if !res.IsError || !strings.Contains(res.Content, web.ErrMsgNotConfigured) {
		t.Fatalf("unconfigured WebSearch = %+v", res)
	}
	// Validation errors are not double-prefixed.
	res = dispatch(t, d, string(protocol.ToolWebSearch), map[string]any{"query": ""})
	if !res.IsError || strings.Contains(res.Content, "Error: Error:") {
		t.Fatalf("validation error = %+v", res)
	}
}

// TestSchemasCoverAllDispatchableTools: Schemas() lists every registered
// adapter, each with a non-empty description and a valid JSON object schema,
// in a stable order; Diff/Mods (not callable) are absent.
func TestSchemasCoverAllDispatchableTools(t *testing.T) {
	d := New(toolreg.ModeDefault, nil)
	schemas := d.Schemas()
	if len(schemas) != len(d.adapters) {
		t.Fatalf("Schemas() = %d entries, adapters = %d", len(schemas), len(d.adapters))
	}
	got := map[string]bool{}
	for _, s := range schemas {
		got[s.Name] = true
		if s.Description == "" {
			t.Errorf("%s: empty description", s.Name)
		}
		var obj map[string]any
		if err := json.Unmarshal(s.InputSchema, &obj); err != nil || obj["type"] != "object" {
			t.Errorf("%s: bad input schema (%v): %s", s.Name, err, s.InputSchema)
		}
	}
	for _, tool := range []protocol.Tool{
		protocol.ToolBash, protocol.ToolGlob, protocol.ToolGrep, protocol.ToolWrite, protocol.ToolNotebookEdit,
		protocol.ToolTodoWrite, protocol.ToolAskUserQuestion,
		protocol.ToolRead, protocol.ToolEdit, protocol.ToolMultiEdit, protocol.ToolWebFetch, protocol.ToolWebSearch,
	} {
		if !got[string(tool)] {
			t.Errorf("Schemas() missing %s", tool)
		}
	}
	if got["Diff"] || got["Mods"] {
		t.Error("Diff/Mods must not be advertised")
	}
	// Stable order across calls.
	again := d.Schemas()
	for i := range schemas {
		if schemas[i].Name != again[i].Name {
			t.Fatalf("order not stable at %d: %s vs %s", i, schemas[i].Name, again[i].Name)
		}
	}
}
