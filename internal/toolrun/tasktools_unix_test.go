//go:build unix

package toolrun

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

var startedRE = regexp.MustCompile(`with ID: (\S+)\. Output is being written to: (.+?\.output)\.`)

func taskManager(t *testing.T) *bgtask.Manager {
	t.Helper()
	m := bgtask.NewManager(bgtask.WithOutputDir(t.TempDir()), bgtask.WithKillGrace(300*time.Millisecond))
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// allowBash lets Bash run without a prompt while every other tool keeps the
// default-mode checks.
var allowBash = toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Bash"}}

func startViaBash(t *testing.T, d *Dispatcher, command string) (id, path string) {
	t.Helper()
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "tu_bg", Name: "Bash", Input: mustJSON(t, map[string]any{
		"command": command, "run_in_background": true, "description": "bg job",
	})})
	if err != nil || res.IsError {
		t.Fatalf("Bash background: %+v, %v", res, err)
	}
	m := startedRE.FindStringSubmatch(res.Content)
	if m == nil {
		t.Fatalf("background result names no id and output file: %q", res.Content)
	}
	return m[1], m[2]
}

// A background command started through the Bash tool can be followed with
// the Read tool on its output file (no prompt in default mode): running, then
// completed with its output, announced by one notification carrying the exit
// code.
func TestBackgroundCommandReadAndNotify(t *testing.T) {
	m := taskManager(t)
	d := New(toolreg.ModeDefault, allowBash, WithBackgroundTasks(m))
	id, path := startViaBash(t, d, "sh -c 'sleep 0.2; echo done'")

	if st, _ := m.Status(id); st != bgtask.StatusRunning {
		t.Fatalf("status right after start = %q, want running", st)
	}
	res := dispatch(t, d, "Read", map[string]any{"file_path": path})
	if res.IsError {
		t.Fatalf("Read of the running task's output file was refused: %q", res.Content)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, err := m.Wait(ctx, id); err != nil || st != bgtask.StatusCompleted {
		t.Fatalf("final status = %q, %v", st, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !m.Notifications().HasPending() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	notes := m.Notifications().TakePending()
	if len(notes) != 1 {
		t.Fatalf("notifications = %q, want exactly one", notes)
	}
	for _, want := range []string{"<task-id>" + id + "</task-id>", "<tool-use-id>tu_bg</tool-use-id>", "<output-file>" + path + "</output-file>", "<status>completed</status>", `"bg job" completed (exit code 0)`} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("notification missing %q:\n%s", want, notes[0])
		}
	}
	// The file tracker saw the earlier read; read the finished output.
	res = dispatch(t, d, "Read", map[string]any{"file_path": path})
	if res.IsError || !strings.Contains(res.Content, "done") {
		t.Fatalf("Read of the finished output = %+v", res)
	}
}

// TaskStop self-allows in default mode and stops the task; a deny rule
// refuses it.
func TestTaskStopThroughDispatcher(t *testing.T) {
	m := taskManager(t)
	d := New(toolreg.ModeDefault, allowBash, WithBackgroundTasks(m))
	id, _ := startViaBash(t, d, "sleep 30")
	res := dispatch(t, d, "TaskStop", map[string]any{"task_id": id})
	if res.IsError {
		t.Fatalf("TaskStop refused: %q", res.Content)
	}
	if !json.Valid([]byte(res.Content)) {
		t.Fatalf("TaskStop result is not JSON: %q", res.Content)
	}
	if !strings.Contains(res.Content, `"task_id":"`+id+`"`) || !strings.Contains(res.Content, `"task_type":"local_bash"`) {
		t.Fatalf("TaskStop result = %q", res.Content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, _ := m.Wait(ctx, id); st != bgtask.StatusKilled {
		t.Fatalf("status after TaskStop = %q", st)
	}

	denied := New(toolreg.ModeDefault, append(toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "TaskStop"}}, allowBash...), WithBackgroundTasks(m))
	id2, _ := startViaBash(t, denied, "sleep 30")
	if res := dispatch(t, denied, "TaskStop", map[string]any{"task_id": id2}); !res.IsError {
		t.Fatalf("deny rule ignored: %q", res.Content)
	}
	if st, _ := m.Status(id2); st != bgtask.StatusRunning {
		t.Fatalf("denied TaskStop still stopped the task: %q", st)
	}
	// Plan mode does not block it either.
	plan := New(toolreg.ModePlan, nil, WithBackgroundTasks(m))
	if res := dispatch(t, plan, "TaskStop", map[string]any{"task_id": id2}); res.IsError {
		t.Fatalf("TaskStop refused in plan mode: %q", res.Content)
	}
}

// TaskStop is offered only with a task manager, and is a deferred tool that
// ToolSearch loads.
func TestTaskStopDeferredAndLoadable(t *testing.T) {
	if _, ok := schemaFlags(New(toolreg.ModeDefault, nil))["TaskStop"]; ok {
		t.Fatal("TaskStop offered without a background task manager")
	}
	m := taskManager(t)
	d := New(toolreg.ModeDefault, nil, WithBackgroundTasks(m), WithToolSearch(toolsearch.ModeOn, 0))
	if deferred, ok := schemaFlags(d)["TaskStop"]; !ok || !deferred {
		t.Fatalf("TaskStop present=%v deferred=%v, want a deferred tool", ok, deferred)
	}
	for _, q := range []string{"select:TaskStop", "kill background task"} {
		in, _ := json.Marshal(map[string]any{"query": q})
		res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t", Name: toolsearch.Name, Input: in})
		if err != nil || res.IsError || len(res.LoadedTools) == 0 || res.LoadedTools[0] != "TaskStop" {
			t.Fatalf("ToolSearch %q = %+v, %v", q, res, err)
		}
	}
	// Without deferred loading it is always offered.
	if deferred, ok := schemaFlags(New(toolreg.ModeDefault, nil, WithBackgroundTasks(m)))["TaskStop"]; !ok || deferred {
		t.Fatal("TaskStop not offered in full with deferred loading off")
	}
	// SetBackgroundTasks registers and unregisters it.
	late := New(toolreg.ModeDefault, nil)
	late.SetBackgroundTasks(m)
	if _, ok := schemaFlags(late)["TaskStop"]; !ok {
		t.Fatal("SetBackgroundTasks did not register TaskStop")
	}
	late.SetBackgroundTasks(nil)
	if _, ok := schemaFlags(late)["TaskStop"]; ok {
		t.Fatal("SetBackgroundTasks(nil) left TaskStop registered")
	}
	if desc, ok := late.Registry.Lookup(protocol.ToolTaskStop); !ok || desc.ReadOnly || !desc.AutoAllow {
		t.Fatalf("TaskStop descriptor = %+v", desc)
	}
}

// A subagent's background commands are not announced; it is told to read
// the output file instead, which it may do without a prompt.
func TestSubagentBackgroundCommand(t *testing.T) {
	m := taskManager(t)
	d := New(toolreg.ModeDefault, allowBash, WithBackgroundTasks(m), WithSubagent())
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "tu_sub", Name: "Bash", Input: mustJSON(t, map[string]any{
		"command": "echo sub", "run_in_background": true,
	})})
	if err != nil || res.IsError {
		t.Fatalf("Bash: %+v %v", res, err)
	}
	if !strings.Contains(res.Content, "Nothing notifies you") || strings.Contains(res.Content, "You will be notified") {
		t.Fatalf("subagent background result = %q", res.Content)
	}
	mm := startedRE.FindStringSubmatch(res.Content)
	if mm == nil {
		t.Fatalf("no id/path in %q", res.Content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = m.Wait(ctx, mm[1])
	time.Sleep(20 * time.Millisecond)
	if m.Notifications().HasPending() {
		t.Fatal("a subagent's background command was announced")
	}
	if info, _ := m.Info(mm[1]); info.Owner == "" {
		t.Fatal("subagent task recorded as the main session's")
	}
	if r := dispatch(t, d, "Read", map[string]any{"file_path": mm[2]}); r.IsError || !strings.Contains(r.Content, "sub") {
		t.Fatalf("subagent Read of its output = %+v", r)
	}
}
