//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
)

// permission-response reaches the job's open prompt: list shows the prompt,
// a decision for it is recorded for the worker, and a missing key, a stale
// request id, a malformed decision or no open prompt are refused.
func TestPermissionResponseRoutesToOpenPrompt(t *testing.T) {
	f := newBGFixture(t)
	// A short directory: t.TempDir() on macOS is longer than a unix socket
	// path may be (bind: invalid argument).
	sockDir, err := os.MkdirTemp("/tmp", "gcperm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	r := &Router{Version: "test", Ready: func() bool { return true }}
	f.m.Register(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = (&Server{Handler: r}).Serve(ctx, l) }()
	client := &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}, Timeout: 10 * time.Second}

	const short = "cccc0001"
	f.dispatch(short, Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	dir := JobDir(f.cfg, short)
	allow := json.RawMessage(`{"behavior":"allow","updatedInput":{"command":"make test"}}`)
	ask := func(req Request) Reply {
		req.Op, req.Short = OpPermissionResponse, short
		return client.Request(ctx, req)
	}

	if rep := ask(Request{Decision: allow}); rep.OK || rep.Code != errutil.WorkerEAuth {
		t.Fatalf("unauthenticated: %+v", rep)
	}
	if rep := ask(Request{Auth: f.key, Decision: allow}); rep.OK || rep.Code != errutil.WorkerENoReply {
		t.Fatalf("no open prompt: %+v", rep)
	}
	if err := WritePendingPermission(dir, PendingPermission{RequestID: "perm_1", ToolName: "Bash", Input: json.RawMessage(`{"command":"make"}`)}); err != nil {
		t.Fatal(err)
	}
	if rep := client.Request(ctx, Request{Op: OpList}); !rep.OK || !strings.Contains(string(rep.Jobs), `"permission":{"requestId":"perm_1","toolName":"Bash"`) {
		t.Fatalf("list: %s", rep.Jobs)
	}
	if rep := ask(Request{Auth: f.key, RequestID: "perm_0", Decision: allow}); rep.OK || rep.Code != errutil.WorkerEStale {
		t.Fatalf("stale id: %+v", rep)
	}
	if rep := ask(Request{Auth: f.key, Decision: json.RawMessage(`{"behavior":"maybe"}`)}); rep.OK || rep.Code != errutil.WorkerEProto {
		t.Fatalf("bad decision: %+v", rep)
	}
	if rep := ask(Request{Auth: f.key, RequestID: "perm_1", Decision: allow}); !rep.OK {
		t.Fatalf("answer: %+v", rep)
	}
	if _, ok := TakePermissionAnswer(dir, "perm_2"); ok {
		t.Fatal("an answer for another request was taken")
	}
	// The mismatched take dropped it as stale; answer again and take it.
	_ = ask(Request{Auth: f.key, Decision: allow})
	got, ok := TakePermissionAnswer(dir, "perm_1")
	if !ok || !strings.Contains(string(got), "make test") {
		t.Fatalf("answer = %s %v", got, ok)
	}
	ClearPendingPermission(dir, "perm_1")
	if p, _ := ReadPendingPermission(dir); p != nil {
		t.Fatalf("prompt still open: %+v", p)
	}
}
