//go:build unix

package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/tools/exec"
)

func newManager(t *testing.T) *bgtask.Manager {
	t.Helper()
	m := bgtask.NewManager(bgtask.WithOutputDir(t.TempDir()), bgtask.WithKillGrace(300*time.Millisecond))
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func startBackground(t *testing.T, m *bgtask.Manager, owner, command string) exec.BashResult {
	t.Helper()
	res, err := exec.BashTool{BackgroundTasks: m, Owner: owner}.Execute(context.Background(), exec.BashInput{Command: command, RunInBackground: true})
	if err != nil || res.BackgroundTaskID == "" {
		t.Fatalf("background start: %+v, %v", res, err)
	}
	return res
}

func call(t *testing.T, tool TaskStopTool, in any) (Output, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	text, err := tool.Execute(context.Background(), raw)
	if err != nil {
		return Output{}, err
	}
	var out Output
	if jerr := json.Unmarshal([]byte(text), &out); jerr != nil {
		t.Fatalf("result is not JSON: %q", text)
	}
	return out, nil
}

// TaskStop kills the whole process group of a long background command: the
// shell and the sleep it started in the background.
func TestTaskStopKillsProcessGroup(t *testing.T) {
	m := newManager(t)
	res := startBackground(t, m, "", "sleep 30 & echo $!; wait")
	id := res.BackgroundTaskID

	// The grandchild's pid is the first line of the output file.
	info, _ := m.Info(id)
	var child int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(info.OutputFile)
		if line, _, ok := strings.Cut(string(data), "\n"); ok {
			child, _ = strconv.Atoi(strings.TrimSpace(line))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child <= 0 {
		t.Fatal("background command did not report its child pid")
	}

	out, err := call(t, TaskStopTool{Manager: m}, Input{TaskID: id})
	if err != nil {
		t.Fatalf("TaskStop: %v", err)
	}
	if out.TaskID != id || out.TaskType != "local_bash" || !strings.HasPrefix(out.Message, "Successfully stopped task: "+id) {
		t.Fatalf("result = %+v", out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, err := m.Wait(ctx, id); err != nil || st != bgtask.StatusKilled {
		t.Fatalf("status after TaskStop = %q, %v", st, err)
	}
	gone := false
	for i := 0; i < 200 && !gone; i++ {
		gone = errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
		if !gone {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !gone {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("the background command's child %d survived TaskStop", child)
	}
	// The model asked for the stop, so no completion notification follows.
	if m.Notifications().HasPending() {
		t.Error("TaskStop queued a completion notification")
	}

	// Stopping it again reports that it is not running.
	if _, err := call(t, TaskStopTool{Manager: m}, Input{TaskID: id}); err == nil || !strings.Contains(err.Error(), "is not running (status: killed)") {
		t.Fatalf("second stop err = %v", err)
	}
}

func TestTaskStopInputErrors(t *testing.T) {
	m := newManager(t)
	tool := TaskStopTool{Manager: m}
	if _, err := call(t, tool, map[string]any{}); err == nil || err.Error() != "Missing required parameter: task_id" {
		t.Fatalf("missing id err = %v", err)
	}
	if _, err := call(t, tool, Input{TaskID: "local_bash_99"}); err == nil || !strings.Contains(err.Error(), "No task found with ID: local_bash_99. No background tasks are running.") {
		t.Fatalf("unknown id err = %v", err)
	}
	res := startBackground(t, m, "", "sleep 30")
	// The deprecated shell_id spelling still works, and the unknown-id error
	// lists the running tasks.
	if _, err := call(t, tool, Input{TaskID: "nope"}); err == nil || !strings.Contains(err.Error(), "Running tasks: "+res.BackgroundTaskID) {
		t.Fatalf("unknown id err = %v", err)
	}
	if _, err := call(t, tool, Input{ShellID: res.BackgroundTaskID}); err != nil {
		t.Fatalf("shell_id stop: %v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"task_id":`)); err == nil {
		t.Fatal("malformed input accepted")
	}
}

// A subagent may stop only the commands it started.
func TestTaskStopOwnership(t *testing.T) {
	m := newManager(t)
	mainTask := startBackground(t, m, "", "sleep 30")
	subTask := startBackground(t, m, "subagent", "sleep 30")
	sub := TaskStopTool{Manager: m, Owner: "subagent"}
	if _, err := call(t, sub, Input{TaskID: mainTask.BackgroundTaskID}); err == nil || !strings.Contains(err.Error(), "not started by this agent") {
		t.Fatalf("subagent stopping a main task: %v", err)
	}
	if _, err := call(t, sub, Input{TaskID: subTask.BackgroundTaskID}); err != nil {
		t.Fatalf("subagent stopping its own task: %v", err)
	}
	if _, err := call(t, TaskStopTool{Manager: m}, Input{TaskID: mainTask.BackgroundTaskID}); err != nil {
		t.Fatalf("main stopping its task: %v", err)
	}
}

func TestBackgroundStartedMessage(t *testing.T) {
	m := newManager(t)
	res := startBackground(t, m, "", "true")
	info, _ := m.Info(res.BackgroundTaskID)
	for _, want := range []string{"Command running in background with ID: " + res.BackgroundTaskID + ".", "Output is being written to: " + info.OutputFile + ".", "You will be notified when it completes"} {
		if !strings.Contains(res.BackgroundMessage, want) {
			t.Errorf("message missing %q: %s", want, res.BackgroundMessage)
		}
	}
	sub := startBackground(t, m, "subagent", "true")
	if !strings.Contains(sub.BackgroundMessage, "Nothing notifies you when the command finishes") || strings.Contains(sub.BackgroundMessage, "You will be notified") {
		t.Errorf("subagent message = %s", sub.BackgroundMessage)
	}
}
