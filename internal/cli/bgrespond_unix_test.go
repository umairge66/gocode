//go:build !windows

package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/errutil"
)

// promptDaemon is a fake daemon for `agents pending` / `agents respond`: list
// returns its records, permission-response is recorded and answered with
// fail (OK when empty).
type promptDaemon struct {
	mu   sync.Mutex
	recs []daemon.JobRecord
	got  []daemon.Request
	fail errutil.WorkerCode
}

func (d *promptDaemon) requests() []daemon.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]daemon.Request(nil), d.got...)
}

// newPromptFixture serves a promptDaemon to the commands, with a job
// directory for each record (resolveJob looks the job up there) and a control
// key.
func newPromptFixture(t *testing.T, recs ...daemon.JobRecord) (*subcmdFixture, *promptDaemon, string) {
	t.Helper()
	f := newSubcmdFixture(t)
	cfg := filepath.Join(f.home, "cfg")
	f.vars["GOCODE_CONFIG_DIR"] = cfg
	key, err := daemon.EnsureControlKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		st := daemon.NewJobState(daemon.SeedParams{Template: "bg", SessionID: r.Short + "-s", Cwd: f.cwd, Intent: r.Intent})
		dir := daemon.JobDir(cfg, r.Short)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := daemon.WriteJobState(dir, st); err != nil {
			t.Fatal(err)
		}
	}
	d := &promptDaemon{recs: recs}
	sock := filepath.Join(shortSockDir(t), "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := &daemon.Server{Handler: daemon.HandlerFunc(func(_ context.Context, req daemon.Request, rw daemon.ReplyWriter) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch req.Op {
		case daemon.OpList:
			jobs, _ := json.Marshal(d.recs)
			_ = rw.Send(daemon.Reply{OK: true, Op: req.Op, Jobs: jobs})
		case daemon.OpPermissionResponse:
			d.got = append(d.got, req)
			if d.fail != "" {
				_ = rw.Send(daemon.FailReply(d.fail, "refused"))
				return
			}
			_ = rw.Send(daemon.OKReply(req.Op))
		default:
			_ = rw.Send(daemon.Reply{OK: true, Op: req.Op})
		}
	})}
	go func() { _ = srv.Serve(ctx, l) }()
	prev := bgControlClient
	bgControlClient = func(string) *daemon.Client {
		return &daemon.Client{Timeout: 5 * time.Second, Dial: func(ctx context.Context) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		}}
	}
	t.Cleanup(func() { bgControlClient = prev })
	return f, d, key
}

// The three kinds of open prompt.
func promptRecords() []daemon.JobRecord {
	return []daemon.JobRecord{
		{Short: "aaaa0001", Intent: "deploy", Permission: &daemon.PendingPermission{RequestID: "perm_1", Kind: daemon.PromptPermission,
			ToolName: "Bash", Input: json.RawMessage(`{"command":"make deploy"}`), Reason: "not in the allow list",
			Suggestions: []string{"Bash(make:*)"}, At: "2026-11-03T10:00:00.000Z"}},
		{Short: "bbbb0002", Intent: "schema", Permission: &daemon.PendingPermission{RequestID: "perm_2", Kind: daemon.PromptQuestion,
			ToolName: "AskUserQuestion", At: "2026-11-03T10:01:00.000Z", Input: json.RawMessage(
				`{"questions":[{"question":"Which database?","header":"DB","options":[{"label":"Postgres"},{"label":"SQLite"}],"multiSelect":false}]}`)}},
		{Short: "cccc0003", Intent: "refactor", Permission: &daemon.PendingPermission{RequestID: "perm_3", Kind: daemon.PromptPlan,
			ToolName: "ExitPlanMode", At: "2026-11-03T10:02:00.000Z", Input: json.RawMessage(
				`{"plan":"# Split the parser\n\n1. …","planFilePath":"/p.md","modes":["acceptEdits","default"]}`)}},
		{Short: "dddd0004", Intent: "busy"},
	}
}

// `agents pending` lists the jobs waiting on a prompt, oldest first, as text
// and as JSON with each prompt's input.
func TestAgentsPendingListsPrompts(t *testing.T) {
	f, _, _ := newPromptFixture(t, promptRecords()...)
	code, out, errb := f.run(t, "agents", "pending")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{
		"aaaa0001 · deploy  permission  Bash", "make deploy", "reason: not in the allow list", "--always adds: Bash(make:*)", "request: perm_1",
		"bbbb0002 · schema  question  AskUserQuestion", "Which database? (Postgres · SQLite)",
		"cccc0003 · refactor  plan  ExitPlanMode", "plan: Split the parser",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dddd0004") {
		t.Errorf("a job with no prompt is listed:\n%s", out)
	}
	code, out, _ = f.run(t, "agents", "pending", "--json")
	var rows []pendingPromptEntry
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 3 {
		t.Fatalf("--json exit %d: %s", code, out)
	}
	if rows[0].Job != "aaaa0001" || rows[0].RequestID != "perm_1" || !jsonEqual(rows[0].Input, json.RawMessage(`{"command":"make deploy"}`)) || rows[2].Kind != "plan" {
		t.Fatalf("rows = %+v", rows)
	}
}

// Without a daemon nothing is waiting.
func TestAgentsPendingNoDaemon(t *testing.T) {
	f := newSubcmdFixture(t)
	f.vars["GOCODE_CONFIG_DIR"] = filepath.Join(f.home, "cfg")
	prev := bgControlClient
	bgControlClient = func(string) *daemon.Client {
		return &daemon.Client{Timeout: time.Second, Dial: func(ctx context.Context) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: errNoSocket{}}
		}}
	}
	t.Cleanup(func() { bgControlClient = prev })
	if code, out, errb := f.run(t, "agents", "pending", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("exit %d: %q %q", code, out, errb)
	}
}

type errNoSocket struct{}

func (errNoSocket) Error() string { return "no socket" }

// `agents respond` sends permission-response with the control key, the job's
// open request id and the decision each kind of prompt expects.
func TestAgentsRespondSendsDecision(t *testing.T) {
	f, d, key := newPromptFixture(t, promptRecords()...)
	cases := []struct {
		args []string
		out  string
		want string
	}{
		{[]string{"aaaa0001", "allow", "--always"}, "allowed Bash and added Bash(make:*)",
			`{"behavior":"allow","updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"make:*"}],"behavior":"allow","destination":"session"}]}`},
		{[]string{"aaaa", "deny", "--message", "use the staging target"}, "denied Bash",
			`{"behavior":"deny","message":"use the staging target"}`},
		{[]string{"bbbb0002", "--answer", "sqlite"}, "answered the question",
			`{"behavior":"allow","updatedInput":{"answers":{"Which database?":"SQLite"}}}`},
		{[]string{"cccc0003", "allow", "--mode", "acceptEdits"}, "approved the plan",
			`{"behavior":"allow","updatedPermissions":[{"type":"setMode","destination":"session","mode":"acceptEdits"}]}`},
		{[]string{"cccc0003", "deny", "-m", "keep the old API"}, "kept planning",
			`{"behavior":"deny","message":"keep the old API"}`},
	}
	for i, c := range cases {
		code, out, errb := f.run(t, append([]string{"agents", "respond"}, c.args...)...)
		if code != 0 || !strings.Contains(out, c.out) {
			t.Fatalf("%v: exit %d out %q err %q", c.args, code, out, errb)
		}
		got := d.requests()
		if len(got) != i+1 {
			t.Fatalf("%v: %d requests", c.args, len(got))
		}
		req := got[i]
		if req.Auth != key || req.Short == "" || !strings.HasPrefix(req.RequestID, "perm_") || string(req.Decision) != c.want {
			t.Fatalf("%v: request = auth %t short %s id %s decision %s", c.args, req.Auth == key, req.Short, req.RequestID, req.Decision)
		}
	}
}

// Arguments that do not fit the prompt are refused before anything is sent.
func TestAgentsRespondRejectsMismatchedArgs(t *testing.T) {
	recs := promptRecords()
	recs[0].Permission.Suggestions = nil
	f, d, _ := newPromptFixture(t, recs...)
	cases := []struct {
		args []string
		err  string
	}{
		{[]string{"aaaa0001", "allow", "--always"}, "offers no rule"},
		{[]string{"aaaa0001", "maybe"}, "'allow' or 'deny'"},
		{[]string{"aaaa0001"}, "say allow or deny"},
		{[]string{"aaaa0001", "--answer", "x"}, "do not apply to a permission prompt"},
		{[]string{"bbbb0002", "--answer", "MySQL"}, `the options are: Postgres, SQLite`},
		{[]string{"bbbb0002", "allow"}, "one --answer per question"},
		{[]string{"cccc0003", "allow", "--mode", "bypassPermissions"}, "can continue in: acceptEdits, default"},
		{[]string{"dddd0004", "allow"}, "not waiting for an answer"},
		{[]string{"aaaa0001", "allow", "--request-id", "perm_9"}, "now perm_1, not perm_9"},
	}
	for _, c := range cases {
		code, _, errb := f.run(t, append([]string{"agents", "respond"}, c.args...)...)
		if code != 1 || !strings.Contains(errb, c.err) {
			t.Errorf("%v: exit %d err %q, want %q", c.args, code, errb, c.err)
		}
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

// The daemon's refusals are explained.
func TestAgentsRespondReportsRefusals(t *testing.T) {
	f, d, _ := newPromptFixture(t, promptRecords()...)
	for code, want := range map[errutil.WorkerCode]string{
		errutil.WorkerENoReply: "no longer open",
		errutil.WorkerEStale:   "replaced by another",
		errutil.WorkerEProto:   "did not accept the decision (refused)",
		errutil.WorkerEAuth:    "control key",
	} {
		d.mu.Lock()
		d.fail = code
		d.mu.Unlock()
		exit, _, errb := f.run(t, "agents", "respond", "aaaa0001", "allow")
		if exit != 1 || !strings.Contains(errb, "couldn't answer aaaa0001: ") || !strings.Contains(errb, want) {
			t.Errorf("%s: exit %d err %q", code, exit, errb)
		}
	}
}
