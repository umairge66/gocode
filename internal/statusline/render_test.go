package statusline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeRunner is an injectable Runner for tests. It records the stdin it was
// given and returns a canned RunOutput/error.
type fakeRunner struct {
	out      RunOutput
	err      error
	gotCmd   string
	gotStdin string
	called   bool
}

func (f *fakeRunner) Run(_ context.Context, command, stdin string) (RunOutput, error) {
	f.called = true
	f.gotCmd = command
	f.gotStdin = stdin
	return f.out, f.err
}

func sampleContext() *Context {
	used := 42.5
	return &Context{
		SessionID:      "sess-123",
		TranscriptPath: "/tmp/transcript.jsonl",
		CWD:            "/home/user/project",
		PermissionMode: "default",
		Model:          Model{ID: "gocode-3-5-sonnet-20241022", DisplayName: "gocode 3.5 Sonnet"},
		Workspace: Workspace{
			CurrentDir: "/home/user/project",
			ProjectDir: "/home/user/project",
			AddedDirs:  []string{"/home/user/other"},
			Repo:       &Repo{Host: "github.com", Owner: "gocodes", Name: "gocode"},
		},
		Version:       "2.1.306",
		OutputStyle:   OutputStyle{Name: "default"},
		ContextWindow: &ContextWindow{ContextWindowSize: 200000, UsedPercentage: &used},
		Thinking:      &Thinking{Enabled: true},
	}
}

func TestBuildStdin_ShapeAndKeys(t *testing.T) {
	stdin, err := BuildStdin(sampleContext())
	if err != nil {
		t.Fatalf("BuildStdin: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(stdin), &m); err != nil {
		t.Fatalf("stdin is not valid JSON: %v", err)
	}

	// Required top-level keys use the snake_case names.
	for _, k := range []string{"session_id", "transcript_path", "cwd", "model", "workspace", "version", "output_style", "exceeds_200k_tokens"} {
		if _, ok := m[k]; !ok {
			t.Errorf("stdin missing required key %q", k)
		}
	}

	model, ok := m["model"].(map[string]any)
	if !ok {
		t.Fatalf("model is not an object: %T", m["model"])
	}
	if model["display_name"] != "gocode 3.5 Sonnet" {
		t.Errorf("model.display_name = %v", model["display_name"])
	}
	if model["id"] != "gocode-3-5-sonnet-20241022" {
		t.Errorf("model.id = %v", model["id"])
	}

	ws, ok := m["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("workspace is not an object: %T", m["workspace"])
	}
	if ws["current_dir"] != "/home/user/project" || ws["project_dir"] != "/home/user/project" {
		t.Errorf("workspace dirs = %v / %v", ws["current_dir"], ws["project_dir"])
	}
	repo, ok := ws["repo"].(map[string]any)
	if !ok {
		t.Fatalf("workspace.repo is not an object: %T", ws["repo"])
	}
	if repo["owner"] != "gocodes" || repo["name"] != "gocode" {
		t.Errorf("repo = %v", repo)
	}

	// Optional/absent objects must not be emitted (omitempty).
	for _, k := range []string{"session_name", "prompt_id", "vim", "agent", "pr", "worktree", "rate_limits", "effort", "cost"} {
		if _, ok := m[k]; ok {
			t.Errorf("absent optional key %q should be omitted", k)
		}
	}
	// exceeds_200k_tokens is always present even when false.
	if v, ok := m["exceeds_200k_tokens"].(bool); !ok || v {
		t.Errorf("exceeds_200k_tokens = %v (want false, present)", m["exceeds_200k_tokens"])
	}
}

func TestRender_FakeRunner_TrimmedMultiline(t *testing.T) {
	fr := &fakeRunner{out: RunOutput{Stdout: "  branch: main  \n\n   3 files   \n", ExitCode: 0}}
	r := &Renderer{Runner: fr, Default: "FALLBACK"}

	got := r.Render(context.Background(), &Config{Type: "command", Command: "my-statusline"}, sampleContext())
	want := "branch: main\n3 files"
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
	if !fr.called {
		t.Fatal("runner was not invoked")
	}
	if fr.gotCmd != "my-statusline" {
		t.Errorf("runner command = %q", fr.gotCmd)
	}
	// The runner receives the serialized JSON context on stdin.
	var m map[string]any
	if err := json.Unmarshal([]byte(fr.gotStdin), &m); err != nil {
		t.Fatalf("runner stdin is not JSON: %v", err)
	}
	if m["session_id"] != "sess-123" {
		t.Errorf("stdin session_id = %v", m["session_id"])
	}
}

func TestRender_SingleLinePreservesANSI(t *testing.T) {
	// ANSI escapes must survive the stdout->line mapping.
	ansi := "\x1b[32mgocode\x1b[0m"
	fr := &fakeRunner{out: RunOutput{Stdout: ansi + "\n", ExitCode: 0}}
	r := &Renderer{Runner: fr}
	if got := r.Render(context.Background(), &Config{Type: "command", Command: "c"}, sampleContext()); got != ansi {
		t.Errorf("Render = %q, want %q (ANSI preserved)", got, ansi)
	}
}

func TestRender_NoConfig_ReturnsDefault(t *testing.T) {
	r := &Renderer{Runner: &fakeRunner{}, Default: "DEF"}

	cases := map[string]*Config{
		"nil":        nil,
		"empty type": {Type: "", Command: "c"},
		"wrong type": {Type: "builtin", Command: "c"},
		"no command": {Type: "command", Command: ""},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			fr := &fakeRunner{}
			r.Runner = fr
			if got := r.Render(context.Background(), cfg, sampleContext()); got != "DEF" {
				t.Errorf("Render = %q, want default", got)
			}
			if fr.called {
				t.Error("runner should not be invoked when no command is configured")
			}
		})
	}
}

func TestRender_EmptyOutput_ReturnsDefault(t *testing.T) {
	fr := &fakeRunner{out: RunOutput{Stdout: "   \n\n  \n", ExitCode: 0}}
	r := &Renderer{Runner: fr, Default: "DEF"}
	if got := r.Render(context.Background(), &Config{Type: "command", Command: "c"}, sampleContext()); got != "DEF" {
		t.Errorf("Render = %q, want default for blank output", got)
	}
}

func TestRender_FailingCommand_GracefulFallback(t *testing.T) {
	// Non-zero exit with stdout content still falls back to default.
	fr := &fakeRunner{out: RunOutput{Stdout: "partial", Stderr: "boom", ExitCode: 2}}
	r := &Renderer{Runner: fr, Default: "DEF"}
	if got := r.Render(context.Background(), &Config{Type: "command", Command: "c"}, sampleContext()); got != "DEF" {
		t.Errorf("Render = %q, want default for non-zero exit", got)
	}
}

func TestRender_SpawnError_GracefulFallback(t *testing.T) {
	fr := &fakeRunner{err: errors.New("exec: not found")}
	r := &Renderer{Runner: fr, Default: "DEF"}
	if got := r.Render(context.Background(), &Config{Type: "command", Command: "c"}, sampleContext()); got != "DEF" {
		t.Errorf("Render = %q, want default on runner error", got)
	}
}

func TestExecRunner_EchoLine(t *testing.T) {
	// A harmless echo confirms the real exec path pipes stdin and captures
	// stdout. stdin is echoed back so we verify round-tripping.
	r := &Renderer{Runner: ExecRunner{Timeout: 5 * time.Second}}
	got := r.Render(context.Background(), &Config{Type: "command", Command: "echo hello-status"}, sampleContext())
	if got != "hello-status" {
		t.Errorf("Render via echo = %q, want %q", got, "hello-status")
	}
}

func TestExecRunner_ReadsStdin(t *testing.T) {
	// `cat` echoes the JSON context from stdin; the mapping keeps non-blank
	// lines, and compact JSON is a single line.
	r := &Renderer{Runner: ExecRunner{Timeout: 5 * time.Second}}
	got := r.Render(context.Background(), &Config{Type: "command", Command: "cat"}, sampleContext())
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("stdin was not round-tripped as JSON: %v (got %q)", err, got)
	}
	if m["session_id"] != "sess-123" {
		t.Errorf("round-tripped session_id = %v", m["session_id"])
	}
}
