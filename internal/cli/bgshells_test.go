package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// bgScriptCaller is a fake model: its first reply starts a background Bash
// command, it then ends the turn ("started"), and once a task-notification
// reaches it, it answers "final: done". It records every request and reads
// the output file the notification names while it still exists.
type bgScriptCaller struct {
	command string

	mu         sync.Mutex
	requests   [][]agent.Message
	notified   string // the notification text, once seen
	outputSeen string // the output file's content when the notification came
}

var outputFileRE = regexp.MustCompile(`<output-file>([^<]+)</output-file>`)

func (c *bgScriptCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, append([]agent.Message(nil), req.Messages...))
	for _, m := range req.Messages {
		if m.Role != agent.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == agent.BlockText && strings.Contains(b.Text, "<task-notification>") && c.notified == "" {
				c.notified = b.Text
				if mm := outputFileRE.FindStringSubmatch(b.Text); mm != nil {
					data, _ := os.ReadFile(mm[1])
					c.outputSeen = string(data)
				}
			}
		}
	}
	if c.notified != "" {
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("final: done")), StopReason: agent.StopEndTurn}, nil
	}
	if len(c.requests) == 1 {
		in, _ := json.Marshal(map[string]any{"command": c.command, "run_in_background": true, "description": "bg job"})
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("tu_bg", "Bash", in)), StopReason: agent.StopToolUse}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("started")), StopReason: agent.StopEndTurn}, nil
}

// notificationBlocks counts the text blocks carrying a task-notification in
// the last request the model saw.
func (c *bgScriptCaller) notificationBlocks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return 0
	}
	n := 0
	for _, m := range c.requests[len(c.requests)-1] {
		for _, b := range m.Content {
			if b.Type == agent.BlockText && strings.Contains(b.Text, "<task-notification>") {
				n++
			}
		}
	}
	return n
}

// bgPrintEnv is an isolated -p environment: HOME and the temp directory are
// test directories, and the extra variables are visible to the session.
func bgPrintEnv(t *testing.T, extra map[string]string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	env, out, errb := newTestEnv()
	env.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return extra[k]
	}
	env.Cwd = func() (string, error) { return cwd, nil }
	return env, out, errb
}

// A -p run waits for the background command it started: the completion
// notification reaches the model exactly once, in a continuation turn, and
// that turn's reply is what -p prints.
func TestPrintAwaitsBackgroundCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	env, out, errb := bgPrintEnv(t, nil)
	caller := &bgScriptCaller{command: "sleep 0.2; echo bg-finished"}
	s := &printSession{
		Mode:      toolreg.ModeBypassPermissions,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	if code := runPrintSession(env, s, "run it in the background"); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "final: done" {
		t.Fatalf("stdout = %q, want the continuation turn's reply", got)
	}
	caller.mu.Lock()
	note, seen := caller.notified, caller.outputSeen
	caller.mu.Unlock()
	for _, want := range []string{"<task-id>local_bash_1</task-id>", "<tool-use-id>tu_bg</tool-use-id>", "<status>completed</status>", `Background command "bg job" completed (exit code 0)`, "<output-file>"} {
		if !strings.Contains(note, want) {
			t.Errorf("notification missing %q:\n%s", want, note)
		}
	}
	if !strings.Contains(seen, "bg-finished") {
		t.Errorf("output file content = %q, want the command's output", seen)
	}
	if n := caller.notificationBlocks(); n != 1 {
		t.Errorf("the notification appears %d times in the conversation, want exactly 1", n)
	}
	// The output directory is gone once the session ended.
	if m := outputFileRE.FindStringSubmatch(note); m != nil {
		if _, err := os.Stat(filepath.Dir(m[1])); !os.IsNotExist(err) {
			t.Errorf("task output directory still exists after the session: %v", err)
		}
	}
}

// A -p run stops waiting at the ceiling: the still-running command is
// reported on stderr and stopped with the session.
func TestPrintBackgroundWaitCeiling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	env, out, errb := bgPrintEnv(t, map[string]string{printBgWaitCeilingEnv: "200"})
	caller := &bgScriptCaller{command: "sleep 30"}
	s := &printSession{
		Mode:      toolreg.ModeBypassPermissions,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	start := time.Now()
	if code := runPrintSession(env, s, "start a server"); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errb.String())
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("-p run took %v; the ceiling did not end the wait", el)
	}
	if got := strings.TrimSpace(out.String()); got != "started" {
		t.Fatalf("stdout = %q, want the last turn's reply", got)
	}
	if !strings.Contains(errb.String(), "Background tasks still running") || !strings.Contains(errb.String(), printBgWaitCeilingEnv+"=0") {
		t.Fatalf("stderr does not report the ceiling: %s", errb.String())
	}
}

func TestPrintBgWaitCeilingParse(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"":     defaultPrintBgWaitCeiling,
		"0":    0,
		"1500": 1500 * time.Millisecond,
		"-3":   defaultPrintBgWaitCeiling,
		"abc":  defaultPrintBgWaitCeiling,
	} {
		env := &Env{Getenv: func(k string) string {
			if k == printBgWaitCeilingEnv {
				return v
			}
			return ""
		}}
		if got := printBgWaitCeiling(env); got != want {
			t.Errorf("ceiling(%q) = %v, want %v", v, got, want)
		}
	}
}

// /tasks lists the background commands and stops one by id.
func TestTasksCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	m := bgtask.NewManager(bgtask.WithOutputDir(t.TempDir()), bgtask.WithKillGrace(200*time.Millisecond))
	defer func() { _ = m.Close() }()
	if got := tasksCommand(m, ""); got != "No background tasks." {
		t.Fatalf("empty list = %q", got)
	}
	id, err := m.StartTask(context.Background(), bgtask.StartOptions{Command: "sleep 30", Description: "long sleep"})
	if err != nil {
		t.Fatal(err)
	}
	list := tasksCommand(m, "list")
	for _, want := range []string{"1 running, 1 total", id, "running", "long sleep", "output: ", "/tasks stop <id>"} {
		if !strings.Contains(list, want) {
			t.Errorf("list missing %q:\n%s", want, list)
		}
	}
	if got := tasksCommand(m, "stop"); !strings.HasPrefix(got, "Usage") {
		t.Errorf("stop without id = %q", got)
	}
	if got := tasksCommand(m, "stop nope"); !strings.Contains(got, "No background task with ID nope") {
		t.Errorf("stop unknown = %q", got)
	}
	if got := tasksCommand(m, "stop "+id); got != "Stopped "+id+" (long sleep)." {
		t.Errorf("stop = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, err := m.Wait(ctx, id); err != nil || st != bgtask.StatusKilled {
		t.Fatalf("after /tasks stop: status %q, err %v", st, err)
	}
	if got := tasksCommand(m, "stop "+id); !strings.Contains(got, "is not running") {
		t.Errorf("stop finished = %q", got)
	}
	if m.Notifications().HasPending() {
		t.Error("a stop the user asked for queued a notification")
	}
	if got := tasksCommand(m, "frobnicate"); !strings.HasPrefix(got, "Usage") {
		t.Errorf("bad subcommand = %q", got)
	}
}

// In an interactive session a finished background command wakes the idle
// session: its summary is shown, a continuation turn delivers the
// notification to the model, and the next user turn does not repeat it.
func TestInteractiveBackgroundCompletionWakesSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	caller := &bgRecordingCaller{}
	conv := conversation.New(caller, fakeToolDispatcher{})
	m := bgtask.NewManager(bgtask.WithOutputDir(t.TempDir()))
	defer func() { _ = m.Close() }()
	conv.BackgroundTasks = m
	conv.AddContextFeed(m.Notifications())

	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}
	rt := &sessionRuntime{conv: conv}
	rt.setNotify(func(msg string) { prog.Send(ui.AppendSystemMsg{Text: msg}) })
	rt.setResume(drv.continueBackground)
	rt.wireBackgroundShells()

	if _, err := m.StartTask(context.Background(), bgtask.StartOptions{Command: "echo hi", Description: "say hi"}); err != nil {
		t.Fatal(err)
	}
	if !sawSystemMsg(prog, `Background command "say hi" completed (exit code 0)`) {
		t.Fatal("the completion was not shown to the user")
	}
	if !waitCount(caller, 1, 5*time.Second) {
		t.Fatal("the completion did not start a continuation turn")
	}
	if got := caller.request(0); strings.Count(got, "<task-notification>") != 1 || !strings.Contains(got, "<status>completed</status>") {
		t.Fatalf("continuation request = %q", got)
	}
	// The next user turn carries the notification only as history.
	time.Sleep(50 * time.Millisecond)
	drv.submit("next")
	if !waitCount(caller, 2, 5*time.Second) {
		t.Fatal("the user turn did not reach the model")
	}
	if got := caller.request(1); strings.Count(got, "<task-notification>") != 1 {
		t.Fatalf("the notification was delivered again: %q", got)
	}
}

func TestShellsPillRefresh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	conv := conversation.New(&bgRecordingCaller{}, fakeToolDispatcher{})
	m := bgtask.NewManager(bgtask.WithKillGrace(200 * time.Millisecond))
	defer func() { _ = m.Close() }()
	conv.BackgroundTasks = m
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}
	id, err := m.Start(context.Background(), "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	drv.refreshShells()
	if !prog.waitFor(func(msg tea.Msg) bool {
		s, ok := msg.(ui.SetBackgroundShellsMsg)
		return ok && s.Count == 1
	}, 2*time.Second) {
		t.Fatal("the footer was not told about the running shell")
	}
	if _, err := m.Stop(id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = m.Wait(ctx, id)
	drv.refreshShells()
	if !prog.waitFor(func(msg tea.Msg) bool {
		s, ok := msg.(ui.SetBackgroundShellsMsg)
		return ok && s.Count == 0
	}, 2*time.Second) {
		t.Fatal("the footer pill was not cleared")
	}
}

// The json output format also waits: the final result record is the
// continuation turn's.
func TestPrintJSONAwaitsBackgroundCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	env, out, errb := bgPrintEnv(t, nil)
	caller := &bgScriptCaller{command: "sleep 0.2; echo bg-finished"}
	s := &printSession{
		Mode:      toolreg.ModeBypassPermissions,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
	}
	o := printOutputOptions{outputFormat: "json"}
	if code := runHeadlessPrint(env, s, o, nil, singlePrompt("go")); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errb.String())
	}
	var res struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not one JSON result: %q", out.String())
	}
	if res.Result != "final: done" {
		t.Fatalf("result = %q, want the continuation turn's reply", res.Result)
	}
	if n := caller.notificationBlocks(); n != 1 {
		t.Errorf("notification delivered %d times, want 1", n)
	}
}
