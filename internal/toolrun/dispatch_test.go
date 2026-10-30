package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/interact"
)

// mustJSON marshals v to a json.RawMessage for a tool_use input.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return b
}

// dispatch is a small helper that fails the test on a loop-fatal error.
func dispatch(t *testing.T, d *Dispatcher, name string, input any) agent.ToolResult {
	t.Helper()
	res, err := d.Dispatch(context.Background(), agent.ToolUse{
		ID:    "tu_" + name,
		Name:  name,
		Input: mustJSON(t, input),
	})
	if err != nil {
		t.Fatalf("Dispatch(%s) returned loop-fatal error: %v", name, err)
	}
	return res
}

// TestRoundTrip runs a real Write then Glob then Grep through the dispatcher in
// an allowed mode, against a temp-dir sandbox.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d := New(toolreg.ModeBypassPermissions, nil)

	file := filepath.Join(dir, "hello.txt")
	const body = "hello world\nfoo bar\nbaz\n"

	// Write.
	res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{
		"file_path": file,
		"content":   body,
	})
	if res.IsError {
		t.Fatalf("Write reported is_error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "created successfully") {
		t.Errorf("Write content = %q, want a creation confirmation", res.Content)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("Write did not create the file: %v", err)
	}
	if string(got) != body {
		t.Errorf("file contents = %q, want %q", got, body)
	}

	// Glob.
	res = dispatch(t, d, string(protocol.ToolGlob), map[string]any{
		"pattern": "*.txt",
		"path":    dir,
	})
	if res.IsError {
		t.Fatalf("Glob reported is_error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "hello.txt") {
		t.Errorf("Glob content = %q, want it to list hello.txt", res.Content)
	}

	// Grep (content mode) for a line in the written file.
	res = dispatch(t, d, string(protocol.ToolGrep), map[string]any{
		"pattern":     "foo",
		"path":        dir,
		"output_mode": "content",
	})
	if res.IsError {
		t.Fatalf("Grep reported is_error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "foo bar") {
		t.Errorf("Grep content = %q, want it to contain the matching line", res.Content)
	}
}

// TestDeniedNoSideEffects verifies a denied call returns an is_error result,
// wrapped in <tool_use_error>, and does not execute the tool.
func TestDeniedNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	rules := toolreg.RuleSet{
		{Behavior: toolreg.RuleDeny, ToolName: "Write", Content: "*"},
	}
	d := New(toolreg.ModeDefault, rules)

	file := filepath.Join(dir, "should-not-exist.txt")
	res := dispatch(t, d, string(protocol.ToolWrite), map[string]any{
		"file_path": file,
		"content":   "nope",
	})
	if !res.IsError {
		t.Fatalf("denied Write should be is_error, got %q", res.Content)
	}
	if !strings.HasPrefix(res.Content, toolUseErrorOpen) || !strings.HasSuffix(res.Content, toolUseErrorClose) {
		t.Errorf("denied content not wrapped in <tool_use_error>: %q", res.Content)
	}
	if !strings.Contains(res.Content, "denied") {
		t.Errorf("denied content = %q, want a deny message", res.Content)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("denied Write created the file (side effect): stat err = %v", err)
	}
}

// TestUnknownTool verifies an unregistered tool returns the
// "No such tool available" is_error.
func TestUnknownTool(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	res := dispatch(t, d, "Bogus", map[string]any{})
	if !res.IsError {
		t.Fatalf("unknown tool should be is_error, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "No such tool available: Bogus") {
		t.Errorf("unknown-tool content = %q, want the no-such-tool message", res.Content)
	}
	if !strings.HasPrefix(res.Content, toolUseErrorOpen) {
		t.Errorf("unknown-tool content not wrapped: %q", res.Content)
	}
}

// TestBashEcho runs a harmless echo command through the dispatcher.
func TestBashEcho(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	res := dispatch(t, d, string(protocol.ToolBash), map[string]any{
		"command": "echo hello-dispatcher",
	})
	if res.IsError {
		t.Fatalf("Bash echo reported is_error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "hello-dispatcher") {
		t.Errorf("Bash content = %q, want the echoed text", res.Content)
	}
}

// TestBashBackgroundRegistersWithManager verifies that a Dispatcher given a
// bgtask.Manager runs a harmless echo Bash call with run_in_background and the
// resulting task is observable through that Manager (List/Wait/Tail). It
// exercises both injection seams: New's WithBackgroundTasks option and the
// post-construction SetBackgroundTasks setter.
func TestBashBackgroundRegistersWithManager(t *testing.T) {
	run := func(t *testing.T, newDispatcher func(*bgtask.Manager) *Dispatcher) {
		mgr := bgtask.NewManager()
		d := newDispatcher(mgr)

		if len(mgr.List()) != 0 {
			t.Fatalf("manager should start with no tasks, got %d", len(mgr.List()))
		}

		res := dispatch(t, d, string(protocol.ToolBash), map[string]any{
			"command":           "echo hello-background",
			"run_in_background": true,
		})
		if res.IsError {
			t.Fatalf("background Bash reported is_error: %q", res.Content)
		}

		// The task is now observable via the Manager.
		tasks := mgr.List()
		if len(tasks) != 1 {
			t.Fatalf("manager should hold exactly one task after a background Bash call, got %d", len(tasks))
		}
		id := tasks[0].ID
		if id == "" {
			t.Fatalf("registered task has no id")
		}
		if tasks[0].Command != "echo hello-background" {
			t.Errorf("task command = %q, want the dispatched command", tasks[0].Command)
		}

		// Let it finish and confirm the echoed output flowed through the manager.
		status, err := mgr.Wait(context.Background(), id)
		if err != nil {
			t.Fatalf("Wait(%s): %v", id, err)
		}
		if status != bgtask.StatusCompleted {
			t.Errorf("task status = %q, want completed", status)
		}
		out, err := mgr.Tail(id)
		if err != nil {
			t.Fatalf("Tail(%s): %v", id, err)
		}
		if !strings.Contains(out.Output, "hello-background") {
			t.Errorf("task output = %q, want the echoed text", out.Output)
		}
	}

	t.Run("WithBackgroundTasks option", func(t *testing.T) {
		run(t, func(m *bgtask.Manager) *Dispatcher {
			return New(toolreg.ModeBypassPermissions, nil, WithBackgroundTasks(m))
		})
	})

	t.Run("SetBackgroundTasks setter", func(t *testing.T) {
		run(t, func(m *bgtask.Manager) *Dispatcher {
			d := New(toolreg.ModeBypassPermissions, nil)
			d.SetBackgroundTasks(m)
			return d
		})
	})
}

// TestBashBackgroundNilManagerFallback verifies that without a Manager a
// run_in_background Bash call still succeeds via the detached-PID fallback
// (today's behavior is preserved).
func TestBashBackgroundNilManagerFallback(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	res := dispatch(t, d, string(protocol.ToolBash), map[string]any{
		"command":           "echo hello-fallback",
		"run_in_background": true,
	})
	if res.IsError {
		t.Fatalf("background Bash (no manager) reported is_error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "background") {
		t.Errorf("fallback content = %q, want a background notice", res.Content)
	}
}

// TestConcurrencyClassification checks the dispatcher reports per-tool
// concurrency safety from the descriptor: read-only tools safe, mutating not.
func TestConcurrencyClassification(t *testing.T) {
	d := New(toolreg.ModeDefault, nil)
	cases := map[protocol.Tool]bool{
		protocol.ToolGlob:         true,
		protocol.ToolGrep:         true,
		protocol.ToolBash:         false,
		protocol.ToolWrite:        false,
		protocol.ToolNotebookEdit: false,
	}
	for tool, want := range cases {
		got := d.IsConcurrencySafe(agent.ToolUse{Name: string(tool)})
		if got != want {
			t.Errorf("IsConcurrencySafe(%s) = %v, want %v", tool, got, want)
		}
	}
	// An unregistered tool is conservatively not safe (Wl default).
	if d.IsConcurrencySafe(agent.ToolUse{Name: "Bogus"}) {
		t.Errorf("unknown tool should not be concurrency-safe")
	}
}

// TestTodoWriteWritesThroughInjectedStore verifies a TodoWrite call is
// dispatched to the interact tool, writes the task list through the injected
// TodoStore, and returns the confirmation message. Both injection
// seams (WithTodoStore option, SetTodoStore setter) are exercised.
func TestTodoWriteWritesThroughInjectedStore(t *testing.T) {
	run := func(t *testing.T, newDispatcher func(interact.TodoStore) *Dispatcher) {
		store := interact.NewMemoryTodoStore()
		d := newDispatcher(store)

		res := dispatch(t, d, string(protocol.ToolTodoWrite), map[string]any{
			"todos": []map[string]any{
				{"content": "Run tests", "status": "in_progress", "activeForm": "Running tests"},
				{"content": "Ship it", "status": "pending", "activeForm": "Shipping it"},
			},
		})
		if res.IsError {
			t.Fatalf("TodoWrite reported is_error: %q", res.Content)
		}
		if res.Content != interact.TodoResultMessage {
			t.Errorf("TodoWrite content = %q, want the fixed confirmation message", res.Content)
		}

		// The list round-tripped through the injected store (agent id "").
		got := store.Get("")
		if len(got) != 2 || got[0].Content != "Run tests" || got[1].Status != "pending" {
			t.Errorf("store not updated as expected: %+v", got)
		}
	}

	t.Run("WithTodoStore option", func(t *testing.T) {
		run(t, func(s interact.TodoStore) *Dispatcher {
			return New(toolreg.ModeBypassPermissions, nil, WithTodoStore(s))
		})
	})

	t.Run("SetTodoStore setter", func(t *testing.T) {
		run(t, func(s interact.TodoStore) *Dispatcher {
			d := New(toolreg.ModeBypassPermissions, nil)
			d.SetTodoStore(s)
			return d
		})
	})
}

// TestTodoWriteDefaultStore verifies a TodoWrite call works with no injected
// store (registerDefaults installs an in-memory default).
func TestTodoWriteDefaultStore(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	res := dispatch(t, d, string(protocol.ToolTodoWrite), map[string]any{
		"todos": []map[string]any{
			{"content": "Do thing", "status": "pending", "activeForm": "Doing thing"},
		},
	})
	if res.IsError {
		t.Fatalf("TodoWrite with default store reported is_error: %q", res.Content)
	}
	if res.Content != interact.TodoResultMessage {
		t.Errorf("TodoWrite content = %q, want the fixed confirmation message", res.Content)
	}
}

// TestAskUserQuestionReturnsStructuredRequest verifies the AskUserQuestion
// adapter validates and returns the structured host request (with the TODO
// note) as a successful result.
func TestAskUserQuestionReturnsStructuredRequest(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	res := dispatch(t, d, string(protocol.ToolAskUserQuestion), map[string]any{
		"questions": []map[string]any{
			{
				"question": "Which database?",
				"header":   "Database",
				"options": []map[string]any{
					{"label": "Postgres"},
					{"label": "SQLite"},
				},
			},
		},
	})
	if res.IsError {
		t.Fatalf("AskUserQuestion reported is_error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Which database?") {
		t.Errorf("content = %q, want the structured request", res.Content)
	}
	if !strings.Contains(res.Content, "no interactive user to answer") {
		t.Errorf("content = %q, want the non-blocking no-user note", res.Content)
	}
}

// fakeAsker is a toolrun.Asker that records the request it was handed and
// returns canned answers (or a canned error), so the AskUserQuestion adapter can
// be exercised without a real UI.
type fakeAsker struct {
	gotReq interact.AskRequest
	answer interact.AskUserQuestionResult
	err    error
}

func (f *fakeAsker) Ask(_ context.Context, req interact.AskRequest) (interact.AskUserQuestionResult, error) {
	f.gotReq = req
	return f.answer, f.err
}

// TestAskUserQuestionWithAskerReturnsAnswers verifies that when an Asker is
// injected, the AskUserQuestion adapter presents the request to it and returns
// the user's answers (the AskUserQuestionResult shape) as the
// tool_result, not the structured-request stub. Both injection seams (WithAsker
// option, SetAsker setter) are exercised.
func TestAskUserQuestionWithAskerReturnsAnswers(t *testing.T) {
	run := func(t *testing.T, newDispatcher func(a Asker) *Dispatcher) {
		asker := &fakeAsker{answer: interact.AskUserQuestionResult{
			Answers: map[string]interact.StringOrList{
				"Which database?": {Values: []string{"Postgres"}},
			},
		}}
		d := newDispatcher(asker)

		res := dispatch(t, d, string(protocol.ToolAskUserQuestion), map[string]any{
			"questions": []map[string]any{
				{
					"question": "Which database?",
					"header":   "Database",
					"options": []map[string]any{
						{"label": "Postgres"},
						{"label": "SQLite"},
					},
				},
			},
		})
		if res.IsError {
			t.Fatalf("AskUserQuestion with asker reported is_error: %q", res.Content)
		}
		// The adapter returned the answers (not the TODO stub).
		if strings.Contains(res.Content, "TODO") {
			t.Errorf("content = %q, want the answers, not the stub", res.Content)
		}
		if !strings.Contains(res.Content, `"Which database?": "Postgres"`) {
			t.Errorf("content = %q, want the answer keyed by its question", res.Content)
		}
		// The Asker was handed the validated structured request.
		if len(asker.gotReq.Questions) != 1 || asker.gotReq.Questions[0].Question != "Which database?" {
			t.Errorf("asker request = %+v, want the structured question", asker.gotReq)
		}
	}

	t.Run("WithAsker option", func(t *testing.T) {
		run(t, func(a Asker) *Dispatcher {
			return New(toolreg.ModeBypassPermissions, nil, WithAsker(a))
		})
	})
	t.Run("SetAsker setter", func(t *testing.T) {
		run(t, func(a Asker) *Dispatcher {
			d := New(toolreg.ModeBypassPermissions, nil)
			d.SetAsker(a)
			return d
		})
	})
}

// TestAskUserQuestionAskerCancelled verifies that a cancelled / dismissed ask is
// reported as an is_error tool_result wrapped in <tool_use_error>.
func TestAskUserQuestionAskerCancelled(t *testing.T) {
	asker := &fakeAsker{err: errors.New("no answer (the dialog was dismissed)")}
	d := New(toolreg.ModeBypassPermissions, nil, WithAsker(asker))
	res := dispatch(t, d, string(protocol.ToolAskUserQuestion), map[string]any{
		"questions": []map[string]any{
			{"question": "Pick?", "header": "Pick", "options": []map[string]any{{"label": "A"}, {"label": "B"}}},
		},
	})
	if !res.IsError {
		t.Fatalf("cancelled ask should be is_error, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "no answer") {
		t.Errorf("content = %q, want the cancellation message", res.Content)
	}
	if !strings.HasPrefix(res.Content, toolUseErrorOpen) {
		t.Errorf("cancelled content not wrapped: %q", res.Content)
	}
}

// TestDiffModsNotCallable verifies that Diff and Mods are NOT registered as
// callable tools: dispatching them yields the "No such tool
// available" error even though they carry permission descriptors.
func TestDiffModsNotCallable(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	for _, tool := range []protocol.Tool{protocol.ToolDiff, protocol.ToolMods} {
		res := dispatch(t, d, string(tool), map[string]any{})
		if !res.IsError {
			t.Errorf("%q should not be callable, got %q", tool, res.Content)
		}
		if !strings.Contains(res.Content, "No such tool available: "+string(tool)) {
			t.Errorf("%q content = %q, want the no-such-tool error", tool, res.Content)
		}
	}
}

// TestInputValidationError verifies malformed JSON input is reported as an
// is_error result without a loop-fatal error.
func TestInputValidationError(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	res, err := d.Dispatch(context.Background(), agent.ToolUse{
		ID:    "tu_bad",
		Name:  string(protocol.ToolBash),
		Input: json.RawMessage(`{"command": 42}`), // command should be a string
	})
	if err != nil {
		t.Fatalf("malformed input should not be loop-fatal: %v", err)
	}
	if !res.IsError {
		t.Fatalf("malformed input should be is_error, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "invalid input for Bash") {
		t.Errorf("content = %q, want an input-validation message", res.Content)
	}
}
