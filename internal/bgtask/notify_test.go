package bgtask

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitDone(t *testing.T, m *Manager, id string) Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := m.Wait(ctx, id)
	if err != nil {
		t.Fatalf("Wait(%s): %v", id, err)
	}
	return st
}

// waitNotified waits for the completion bookkeeping that follows the
// terminal state (the notification queue and the callback).
func waitChanged(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("Changed did not fire")
	}
}

func TestSummaryAndNotificationText(t *testing.T) {
	info := TaskInfo{ID: "local_bash_3", Command: "make test", ToolUseID: "tu_9", OutputFile: "/tmp/x/local_bash_3.output", Status: StatusCompleted}
	if got, want := info.Summary(), `Background command "make test" completed (exit code 0)`; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	info.Description = "run <tests>"
	text := NotificationText(info, "ignored tail")
	want := "<task-notification>\n" +
		"<task-id>local_bash_3</task-id>\n" +
		"<tool-use-id>tu_9</tool-use-id>\n" +
		"<output-file>/tmp/x/local_bash_3.output</output-file>\n" +
		"<status>completed</status>\n" +
		"<summary>Background command \"run &lt;tests&gt;\" completed (exit code 0)</summary>\n" +
		"</task-notification>"
	if text != want {
		t.Errorf("NotificationText =\n%s\nwant\n%s", text, want)
	}

	failed := TaskInfo{ID: "local_bash_1", Command: "false", Status: StatusFailed, ExitCode: 1}
	if got := failed.Summary(); got != `Background command "false" failed with exit code 1` {
		t.Errorf("failed Summary = %q", got)
	}
	killed := TaskInfo{ID: "local_bash_2", Command: "sleep 9", Status: StatusKilled, ExitCode: 137}
	if got := killed.Summary(); got != `Background command "sleep 9" was stopped` {
		t.Errorf("killed Summary = %q", got)
	}
	// Without an output file the retained tail rides along.
	if got := NotificationText(failed, "boom\n"); !strings.Contains(got, "<output>boom</output>") || strings.Contains(got, "<output-file>") {
		t.Errorf("no-file notification = %s", got)
	}
}

func TestCompletionNotifiedOnce(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	var (
		mu    sync.Mutex
		calls []bool
	)
	m.SetOnComplete(func(info TaskInfo, notified bool) {
		mu.Lock()
		calls = append(calls, notified)
		mu.Unlock()
	})
	id, err := m.StartTask(context.Background(), StartOptions{Command: "x", Description: "job", ToolUseID: "tu_1"})
	if err != nil {
		t.Fatal(err)
	}
	ch := m.Changed()
	r.last().write("hello\n")
	r.last().finish(0)
	if st := waitDone(t, m, id); st != StatusCompleted {
		t.Fatalf("status = %s", st)
	}
	waitChanged(t, ch)

	q := m.Notifications()
	if !q.HasPending() {
		t.Fatal("no notification queued")
	}
	items := q.TakeMidTurn()
	if len(items) != 1 || !strings.Contains(items[0], "<task-id>"+id+"</task-id>") || !strings.Contains(items[0], "<output>hello</output>") {
		t.Fatalf("notifications = %q", items)
	}
	if q.HasPending() || len(q.TakePending()) != 0 {
		t.Fatal("notification handed out twice")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || !calls[0] {
		t.Fatalf("callback calls = %v, want one notified call", calls)
	}
}

func TestStopSuppressesNotification(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	id, err := m.Start(context.Background(), "sleep")
	if err != nil {
		t.Fatal(err)
	}
	ch := m.Changed()
	info, err := m.Stop(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusKilled {
		t.Fatalf("Stop status = %s", info.Status)
	}
	waitDone(t, m, id)
	waitChanged(t, ch)
	if m.Notifications().HasPending() {
		t.Fatal("a requested stop queued a notification")
	}
	if _, err := m.Stop(id); !errors.Is(err, ErrTaskNotRunning) {
		t.Fatalf("second Stop err = %v, want ErrTaskNotRunning", err)
	}
	if _, err := m.Stop("nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Stop(unknown) err = %v", err)
	}
}

func TestKillIsNotified(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	id, _ := m.Start(context.Background(), "sleep")
	ch := m.Changed()
	if err := m.Kill(id); err != nil {
		t.Fatal(err)
	}
	waitDone(t, m, id)
	waitChanged(t, ch)
	items := m.Notifications().TakePending()
	if len(items) != 1 || !strings.Contains(items[0], "<status>killed</status>") || !strings.Contains(items[0], "was stopped") {
		t.Fatalf("notifications = %q", items)
	}
}

func TestSubagentTaskNotNotified(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	id, _ := m.StartTask(context.Background(), StartOptions{Command: "x", Owner: "subagent"})
	ch := m.Changed()
	r.last().finish(0)
	waitDone(t, m, id)
	waitChanged(t, ch)
	if m.Notifications().HasPending() {
		t.Fatal("a subagent's task was announced to the main session")
	}
	if len(m.Running(true)) != 0 || len(m.Running(false)) != 0 {
		t.Fatal("finished task still listed as running")
	}
}

func TestShutdownDoesNotNotify(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(WithRunner(r), WithKillGrace(10*time.Millisecond))
	if _, err := m.Start(context.Background(), "sleep"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Notifications().HasPending() {
		t.Fatal("session shutdown queued a notification")
	}
}

func TestOutputFileWrittenAndBounded(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	m := NewManager(WithRunner(r), WithOutputDir(dir), WithOutputFileLimit(16))
	id, err := m.Start(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := m.Info(id)
	if info.OutputFile != filepath.Join(dir, id+".output") {
		t.Fatalf("OutputFile = %q", info.OutputFile)
	}
	r.last().write("0123456789")
	r.last().write("abcdefghij")
	r.last().finish(0)
	waitDone(t, m, id)
	data, err := os.ReadFile(info.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "0123456789abcdef\n[output truncated") {
		t.Fatalf("output file = %q", data)
	}
	// Windows has no Unix permission bits: the file is private through the
	// per-user ACL of the directory it is created in.
	if st, _ := os.Stat(info.OutputFile); runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Errorf("output file mode = %v, want owner-only", st.Mode().Perm())
	}
	// The tail keeps working past the file limit.
	tail, _ := m.Tail(id)
	if tail.Output != "0123456789abcdefghij" {
		t.Errorf("tail = %q", tail.Output)
	}
	// A directory the manager did not create is kept on shutdown.
	_ = m.Close()
	if _, err := os.Stat(info.OutputFile); err != nil {
		t.Errorf("output file removed from a caller-owned dir: %v", err)
	}
}

func TestTempOutputDirLifecycle(t *testing.T) {
	setTempDir(t, t.TempDir())
	r := &fakeRunner{}
	m := NewManager(WithRunner(r), WithTempOutputDir())
	dir := m.OutputDir()
	if dir == "" || !strings.HasPrefix(filepath.Base(dir), "gocode-tasks-") {
		t.Fatalf("OutputDir = %q", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("output dir created before any task: %v", err)
	}
	id, _ := m.Start(context.Background(), "x")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o700) {
		t.Fatalf("output dir after start: %v %v", st, err)
	}
	r.last().finish(0)
	waitDone(t, m, id)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("output dir not removed on shutdown: %v", err)
	}
}

func TestTempOutputDirNeverReusesExisting(t *testing.T) {
	setTempDir(t, t.TempDir())
	r := &fakeRunner{}
	m := NewManager(WithRunner(r), WithTempOutputDir())
	// Someone else created the directory first: no output file is written
	// there, and it is not removed.
	if err := os.Mkdir(m.OutputDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	id, err := m.Start(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := m.Info(id); info.OutputFile != "" {
		t.Fatalf("output file written into a directory the manager did not create: %s", info.OutputFile)
	}
	r.last().finish(0)
	waitDone(t, m, id)
	_ = m.Close()
	if _, err := os.Stat(m.OutputDir()); err != nil {
		t.Fatalf("foreign directory removed: %v", err)
	}
}

// setTempDir points os.TempDir at dir for the test: TMPDIR on Unix, TMP and
// TEMP on Windows.
func setTempDir(t *testing.T, dir string) {
	t.Helper()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
}
