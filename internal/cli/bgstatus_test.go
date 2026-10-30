package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// newTestJob seeds a background job dir and returns its reporter.
func newTestJob(t *testing.T, intent string) (*jobStatus, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "jobs", "abcd1234")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := daemon.NewJobState(daemon.SeedParams{Template: "bg", SessionID: "abcd1234-s", Cwd: dir, Intent: intent})
	if err := daemon.WriteJobState(dir, st); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GOCODE_CODE_SESSION_KIND": "bg", "GOCODE_JOB_DIR": dir}
	j := newJobStatus(func(k string) string { return env[k] }, "abcd1234-s", "test")
	if j == nil {
		t.Fatal("no reporter for a bg worker")
	}
	return j, dir
}

func readTestJob(t *testing.T, dir string) *daemon.JobState {
	t.Helper()
	st, err := daemon.ReadJobState(dir)
	if err != nil || st == nil {
		t.Fatalf("read state: %v", err)
	}
	return st
}

func TestJobStatusOnlyForBackgroundWorkers(t *testing.T) {
	if newJobStatus(func(string) string { return "" }, "s", "v") != nil {
		t.Fatal("reporter outside a bg worker")
	}
	var j *jobStatus // nil is a no-op
	j.turnStarted()
	j.setBlock("x")
	j.turnEnded("done")
}

func TestJobStatusTurnLifecycle(t *testing.T) {
	j, dir := newTestJob(t, "fix the bug")
	// The seed is working/active "starting…"; a finished turn settles it.
	j.notePrompt("fix the bug")
	j.turnStarted()
	j.turnEnded("Patched the parser.\nResult: fixed the off-by-one in the lexer")
	st := readTestJob(t, dir)
	if st.State != "done" || st.Tempo != "idle" || st.Detail != "fixed the off-by-one in the lexer" || st.FirstTerminalAt == nil {
		t.Fatalf("after turn: %+v", st)
	}
	if agentsDisplayState(st) != "done" {
		t.Fatalf("display = %s", agentsDisplayState(st))
	}
	var out map[string]string
	if json.Unmarshal(st.Output, &out) != nil || out["result"] != "fixed the off-by-one in the lexer" {
		t.Fatalf("output = %s", st.Output)
	}
	// A follow-up message makes it active again, with the message as detail.
	j.notePrompt("now add a test")
	j.turnStarted()
	st = readTestJob(t, dir)
	if st.Tempo != "active" || st.Detail != "now add a test" || agentsDisplayState(st) != "working" {
		t.Fatalf("after second start: %+v", st)
	}
	// An aborted turn goes idle.
	j.turnAborted()
	if st = readTestJob(t, dir); st.Tempo != "idle" {
		t.Fatalf("after abort: %+v", st)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "timeline.jsonl"))
	if strings.Count(string(b), "\n") < 2 {
		t.Fatalf("timeline = %q", b)
	}
}

func TestJobStatusMidTurnDebounced(t *testing.T) {
	j, dir := newTestJob(t, "x")
	now := time.Now()
	j.now = func() time.Time { return now }
	j.turnStarted()
	j.midTurn("", "Bash ls -la")
	if st := readTestJob(t, dir); st.State != "working" || st.Tempo != "active" || st.Detail != "Bash ls -la" {
		t.Fatalf("mid-turn: %+v", st)
	}
	j.midTurn("Reading the configuration loader now", "Read cfg.go")
	if st := readTestJob(t, dir); st.Detail != "Bash ls -la" {
		t.Fatalf("not debounced: %q", st.Detail)
	}
	now = now.Add(16 * time.Second)
	j.midTurn("Reading the configuration loader now", "Read cfg.go")
	if st := readTestJob(t, dir); st.Detail != "Reading the configuration loader now" {
		t.Fatalf("after debounce: %q", st.Detail)
	}
}

func TestJobStatusPermissionBlock(t *testing.T) {
	j, dir := newTestJob(t, "x")
	j.turnStarted()
	j.setBlock(permissionBlockText("Bash", json.RawMessage(`{"command":"rm -rf build"}`)))
	st := readTestJob(t, dir)
	if st.Tempo != "blocked" || st.Needs != "approve Bash: rm -rf build" || agentsDisplayState(st) != "blocked" {
		t.Fatalf("blocked: %+v", st)
	}
	j.clearBlock()
	if st = readTestJob(t, dir); st.Tempo != "active" || st.Needs != "" {
		t.Fatalf("after answer: %+v", st)
	}
}

func TestJobStatusAPIFailure(t *testing.T) {
	j, dir := newTestJob(t, "x")
	j.turnStarted()
	j.apiFailure(errNoCredentials)
	st := readTestJob(t, dir)
	if st.State != "blocked" || st.Tempo != "blocked" || !strings.HasPrefix(st.Needs, "login required") {
		t.Fatalf("api failure: %+v", st)
	}
	j2, dir2 := newTestJob(t, "y")
	j2.apiFailure(errors.New("boom"))
	if st := readTestJob(t, dir2); st.State != "blocked" || !strings.HasPrefix(st.Needs, "API error") {
		t.Fatalf("generic failure: %+v", st)
	}
}

func TestBlockTexts(t *testing.T) {
	if got := permissionBlockText("ExitPlanMode", nil); got != "approve plan" {
		t.Fatal(got)
	}
	if got := permissionBlockText("WebFetch", json.RawMessage(`{"url":"https://x.test"}`)); got != "approve WebFetch: https://x.test" {
		t.Fatal(got)
	}
	if got := permissionBlockText("Glob", json.RawMessage(`{}`)); got != "approve Glob" {
		t.Fatal(got)
	}
	if got := questionBlockText("Which db?", []string{"pg", "sqlite"}); got != "answer: Which db? (pg · sqlite)" {
		t.Fatal(got)
	}
}

// The real permission prompter marks the job blocked while its dialog is open.
func TestPrompterReportsBlock(t *testing.T) {
	j, dir := newTestJob(t, "x")
	j.turnStarted()
	p := newDialogPrompter(nil)
	p.status = j
	opened := make(chan struct{}, 1)
	p.bind(func(m tea.Msg) {
		if _, ok := m.(ui.OpenDialogMsg); ok {
			opened <- struct{}{}
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.Prompt(context.Background(), toolrun.PermissionRequest{ToolName: "Bash", Input: json.RawMessage(`{"command":"make deploy"}`)})
	}()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("dialog never opened")
	}
	if st := readTestJob(t, dir); agentsDisplayState(st) != "blocked" || st.Needs != "approve Bash: make deploy" {
		t.Fatalf("while prompting: %+v", st)
	}
	p.cancel()
	<-done
	if st := readTestJob(t, dir); st.Tempo == "blocked" {
		t.Fatalf("still blocked after the answer: %+v", st)
	}
}
