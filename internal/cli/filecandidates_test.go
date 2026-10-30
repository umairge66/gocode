package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/ui"
)

// writeTree creates files/dirs under root from a path→content map ("" content
// means an empty file; a trailing "/" key means a directory).
func writeTree(t *testing.T, root string, tree map[string]string) {
	t.Helper()
	for p, content := range tree {
		full := filepath.Join(root, filepath.FromSlash(p))
		if filepath.ToSlash(p)[len(p)-1] == '/' {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", full, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

// isolateGitConfig points HOME and XDG_CONFIG_HOME at empty directories so the
// user's global git excludes file never influences a walk.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

var defaultListOptions = filetools.ListOptions{IncludeHidden: true, RespectIgnore: true}

func candidateSetWith(root string, opt filetools.ListOptions) map[string]bool {
	set := map[string]bool{}
	for _, c := range buildFileCandidates(context.Background(), root, opt) {
		set[c.Path] = true
	}
	return set
}

func candidateSet(root string) map[string]bool { return candidateSetWith(root, defaultListOptions) }

func TestBuildFileCandidatesRespectsGitignore(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/":                   "",
		".gitignore":              "ignored.txt\n*.log\nbuilddir/\n",
		"README.md":               "readme",
		"ignored.txt":             "nope",
		"app.log":                 "nope",
		"src/main.go":             "pkg",
		"src/keep.txt":            "ok",
		"builddir/out.bin":        "nope",
		"node_modules/x/index.js": "nope",
	})

	set := candidateSet(root)
	for _, want := range []string{"README.md", "src", "src/main.go", "src/keep.txt"} {
		if !set[want] {
			t.Errorf("expected %q in candidates, missing (have %v)", want, set)
		}
	}
	var sawDir bool
	for _, c := range buildFileCandidates(context.Background(), root, defaultListOptions) {
		if c.Path == "src" {
			if !c.IsDir {
				t.Errorf("src should be marked IsDir")
			}
			sawDir = true
		}
	}
	if !sawDir {
		t.Errorf("directory 'src' not emitted")
	}
	for _, bad := range []string{"ignored.txt", "app.log", "builddir", "builddir/out.bin", "node_modules", "node_modules/x/index.js", ".git"} {
		if set[bad] {
			t.Errorf("expected %q excluded, but it is present", bad)
		}
	}
}

func TestBuildFileCandidatesNegation(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/":       "",
		".gitignore":  "*.secret\n!keep.secret\n",
		"a.secret":    "x",
		"keep.secret": "x",
		"notes.txt":   "x",
	})
	set := candidateSet(root)
	if set["a.secret"] {
		t.Errorf("a.secret should be ignored")
	}
	if !set["keep.secret"] {
		t.Errorf("keep.secret should be re-included by the negation rule")
	}
	if !set["notes.txt"] {
		t.Errorf("notes.txt should be present")
	}
}

// TestBuildFileCandidatesAllIgnoreSources covers nested .gitignore files,
// .ignore, .rgignore and .git/info/exclude, and the respectGitignore=false
// switch.
func TestBuildFileCandidatesAllIgnoreSources(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/info/exclude":       "excluded.txt\n",
		".ignore":                 "*.tmp\n",
		".rgignore":               "rg-only/\n",
		"excluded.txt":            "x",
		"scratch.tmp":             "x",
		"rg-only/file.go":         "x",
		"pkg/.gitignore":          "generated/\nlocal.txt\n",
		"pkg/api.go":              "x",
		"pkg/local.txt":           "x",
		"pkg/generated/gen.go":    "x",
		"pkg/deep/.ignore":        "secret.go\n",
		"pkg/deep/secret.go":      "x",
		"pkg/deep/visible.go":     "x",
		"other/local.txt":         "x",
		"other/generated/keep.go": "x",
	})
	set := candidateSet(root)
	for _, bad := range []string{"excluded.txt", "scratch.tmp", "rg-only", "rg-only/file.go", "pkg/local.txt", "pkg/generated", "pkg/generated/gen.go", "pkg/deep/secret.go"} {
		if set[bad] {
			t.Errorf("%q should be ignored", bad)
		}
	}
	// A nested .gitignore only covers its own subtree.
	for _, want := range []string{"pkg/api.go", "pkg/deep/visible.go", "other/local.txt", "other/generated/keep.go"} {
		if !set[want] {
			t.Errorf("%q should be listed", want)
		}
	}

	all := candidateSetWith(root, filetools.ListOptions{IncludeHidden: true, RespectIgnore: false})
	for _, want := range []string{"excluded.txt", "scratch.tmp", "pkg/generated/gen.go", "pkg/deep/secret.go"} {
		if !all[want] {
			t.Errorf("with ignore files off, %q should be listed", want)
		}
	}
	if all[".git"] || all[".git/info/exclude"] {
		t.Error(".git must be skipped even with ignore files off")
	}
}

func TestBuildFileCandidatesHiddenSetting(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".env":             "x",
		".github/ci.yml":   "x",
		"src/.hidden.go":   "x",
		"src/visible.go":   "x",
		".git/HEAD":        "x",
		"src/.git/objects": "x",
	})
	shown := candidateSetWith(root, filetools.ListOptions{IncludeHidden: true, RespectIgnore: true})
	for _, want := range []string{".env", ".github", ".github/ci.yml", "src/.hidden.go", "src/visible.go"} {
		if !shown[want] {
			t.Errorf("hidden shown: %q missing", want)
		}
	}
	hidden := candidateSetWith(root, filetools.ListOptions{IncludeHidden: false, RespectIgnore: true})
	for _, bad := range []string{".env", ".github", ".github/ci.yml", "src/.hidden.go"} {
		if hidden[bad] {
			t.Errorf("hidden excluded: %q present", bad)
		}
	}
	if !hidden["src/visible.go"] {
		t.Error("src/visible.go missing")
	}
	for _, set := range []map[string]bool{shown, hidden} {
		if set[".git"] || set[".git/HEAD"] || set["src/.git"] {
			t.Error("VCS directories must always be skipped")
		}
	}
}

func TestBuildFileCandidatesSortedShallowFirst(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"z.txt":           "x",
		"a/deep/file.txt": "x",
		"b.txt":           "x",
	})
	cs := buildFileCandidates(context.Background(), root, defaultListOptions)
	if len(cs) == 0 {
		t.Fatal("no candidates built")
	}
	if first := cs[0]; first.Path != "a" || !first.IsDir {
		t.Fatalf("first candidate = %+v, want the shallow dir 'a' first", first)
	}
}

func TestBuildFileCandidatesEmptyRootSafe(t *testing.T) {
	if cs := buildFileCandidates(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), defaultListOptions); len(cs) != 0 {
		t.Fatalf("missing root should yield no candidates, got %v", cs)
	}
}

// writeWideTree creates dirs×files empty files (d000/f000.txt …) plus one
// top-level file, returning the total entry count (files + dirs).
func writeWideTree(t *testing.T, root string, dirs, files int) int {
	t.Helper()
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < files; f++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "top.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return dirs*files + dirs + 1
}

// TestBuildFileCandidatesCapAndCancel: a tree larger than the cap is cut at
// maxFileCandidates with every shallow entry kept (breadth-first), and a
// cancelled walk stops early.
func TestBuildFileCandidatesCapAndCancel(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	total := writeWideTree(t, root, 120, 90)
	if total <= maxFileCandidates {
		t.Fatalf("fixture too small: %d", total)
	}
	cs := buildFileCandidates(context.Background(), root, defaultListOptions)
	if len(cs) != maxFileCandidates {
		t.Fatalf("got %d candidates, want the cap %d", len(cs), maxFileCandidates)
	}
	set := map[string]bool{}
	for _, c := range cs {
		set[c.Path] = true
	}
	if !set["top.txt"] || !set["d000"] || !set["d119"] {
		t.Error("shallow entries must survive the cap")
	}

	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	complete, err := filetools.ListPaths(ctx, root, defaultListOptions, func(string, bool) bool {
		seen++
		if seen == 50 {
			cancel()
		}
		return true
	})
	if complete || err != context.Canceled {
		t.Fatalf("cancelled walk: complete=%v err=%v", complete, err)
	}
	// The walk notices cancellation before reading the next directory.
	if seen >= total || seen > 50+90+121 {
		t.Fatalf("cancelled walk visited %d of %d entries", seen, total)
	}

	pre, stop := context.WithCancel(context.Background())
	stop()
	if cs := buildFileCandidates(pre, root, defaultListOptions); len(cs) != 0 {
		t.Fatalf("pre-cancelled walk returned %d entries", len(cs))
	}
}

// filesRecorder collects the snapshots a fileIndex delivers.
type filesRecorder struct {
	ch chan []ui.FileCandidate
}

func newFilesRecorder() *filesRecorder { return &filesRecorder{ch: make(chan []ui.FileCandidate, 16)} }

func (r *filesRecorder) send(files []ui.FileCandidate) { r.ch <- files }

func (r *filesRecorder) waitFor(t *testing.T, path string) []ui.FileCandidate {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case files := <-r.ch:
			for _, c := range files {
				if c.Path == path {
					return files
				}
			}
		case <-deadline:
			t.Fatalf("no snapshot with %q arrived", path)
			return nil
		}
	}
}

func hasPath(files []ui.FileCandidate, path string) bool {
	for _, c := range files {
		if c.Path == path {
			return true
		}
	}
	return false
}

// TestFileIndexRefreshOnMention: a file created after the first snapshot
// appears once an "@" mention opens on a stale snapshot; a fresh snapshot is
// not rebuilt.
func TestFileIndexRefreshOnMention(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "x"})
	rec := newFilesRecorder()
	idx := newFileIndex(root, nil, rec.send)
	defer idx.Close()

	idx.Refresh()
	first := rec.waitFor(t, "a.go")
	if hasPath(first, "later.go") {
		t.Fatal("later.go present before it was created")
	}

	writeTree(t, root, map[string]string{"later.go": "x"})
	// Fresh snapshot: opening "@" does not rescan.
	idx.RefreshIfStale()
	select {
	case files := <-rec.ch:
		t.Fatalf("fresh snapshot was rebuilt: %v", files)
	case <-time.After(200 * time.Millisecond):
	}

	idx.mu.Lock()
	idx.maxAge = 0
	idx.builtAt = time.Now().Add(-time.Minute)
	idx.mu.Unlock()
	idx.RefreshIfStale()
	rec.waitFor(t, "later.go")
}

// TestFileIndexNonBlocking: requesting a rebuild returns at once even while the
// walk is held up; the list arrives asynchronously afterwards.
func TestFileIndexNonBlocking(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "x"})
	release := make(chan struct{})
	var once sync.Once
	opts := func() filetools.ListOptions {
		once.Do(func() { <-release })
		return defaultListOptions
	}
	rec := newFilesRecorder()
	idx := newFileIndex(root, opts, rec.send)
	defer idx.Close()

	start := time.Now()
	idx.Refresh()
	idx.RefreshIfStale()
	idx.Invalidate()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("refresh requests blocked for %v", d)
	}
	select {
	case <-rec.ch:
		t.Fatal("a snapshot arrived before the walk was released")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	rec.waitFor(t, "a.go")
}

// TestFileIndexCloseCancels: Close stops a walk in flight and later requests
// are ignored.
func TestFileIndexCloseCancels(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeWideTree(t, root, 40, 50)
	rec := newFilesRecorder()
	idx := newFileIndex(root, nil, rec.send)
	idx.Refresh()
	idx.Close()
	idx.Refresh()
	idx.Invalidate()
	time.Sleep(50 * time.Millisecond)
	idx.mu.Lock()
	running := idx.running
	idx.mu.Unlock()
	if running {
		t.Fatal("index still running after Close")
	}
}

func TestToolChangesFiles(t *testing.T) {
	cases := []struct {
		name    string
		isError bool
		want    bool
	}{
		{"Write", false, true},
		{"Edit", false, true},
		{"Edit", true, false},
		{"Bash", false, true},
		{"Bash", true, true},
		{"Read", false, false},
		{"Grep", false, false},
	}
	for _, c := range cases {
		if got := toolChangesFiles(c.name, c.isError); got != c.want {
			t.Errorf("toolChangesFiles(%q, %v) = %v, want %v", c.name, c.isError, got, c.want)
		}
	}
}

// writeCaller emits one Write tool_use for path, then ends the turn.
type writeCaller struct {
	mu    sync.Mutex
	calls int
	input json.RawMessage
}

func (c *writeCaller) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n == 1 {
		return agent.ModelResponse{
			Message:    agent.AssistantMessage(agent.ToolUseBlock("w-1", "Write", c.input)),
			StopReason: agent.StopToolUse,
		}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
}

// writingDispatcher performs the Write call on disk.
type writingDispatcher struct{}

func (writingDispatcher) Dispatch(_ context.Context, use agent.ToolUse) (agent.ToolResult, error) {
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(use.Input, &in); err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	if err := os.WriteFile(in.FilePath, []byte(in.Content), 0o644); err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: "written"}, nil
}

// TestFileIndexRefreshAfterWriteTool: a Write tool call in a turn makes the
// newly created file show up in a SetFilesMsg pushed to the UI.
func TestFileIndexRefreshAfterWriteTool(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "x"})
	target := filepath.Join(root, "created_by_tool.go")
	input, _ := json.Marshal(map[string]string{"file_path": target, "content": "package x\n"})

	conv := conversation.New(&writeCaller{input: input}, writingDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}
	drv.files = newFileIndex(root, nil, func(files []ui.FileCandidate) { prog.Send(ui.SetFilesMsg{Files: files}) })
	defer drv.files.Close()

	drv.files.Refresh()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetFilesMsg)
		return ok && hasPath(s.Files, "a.go")
	}, 5*time.Second) {
		t.Fatal("initial snapshot not delivered")
	}
	drv.submit("go")
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetFilesMsg)
		return ok && hasPath(s.Files, "created_by_tool.go")
	}, 5*time.Second) {
		t.Fatal("file created by the Write tool did not reach the @ list")
	}
}
