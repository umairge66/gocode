package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// TestProgressHeartbeatOverStdio: a tools/call whose _meta carries a
// progressToken gets periodic "Running for <s>s" heartbeats, with strictly
// increasing progress, before the result arrives.
func TestProgressHeartbeatOverStdio(t *testing.T) {
	s := NewServer(DefaultServerInfo, WithProgressInterval(30*time.Millisecond))
	s.RegisterTool("slow", mcp.ToolSchema{Type: "object"}, func(ctx context.Context, _ json.RawMessage) (mcp.CallToolResult, error) {
		time.Sleep(110 * time.Millisecond)
		return mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "done"}}}, nil
	})
	fc, done := startServer(t, s)
	fc.write(t, newRequest(t, 1, mcp.MethodToolsCall, mcp.CallToolParams{
		Name: "slow",
		Meta: json.RawMessage(`{"progressToken":"tok-1"}`),
	}))

	var notes []progressParams
	for {
		msg := fc.readMsg(t)
		if msg.Notification != nil {
			if msg.Notification.Method != MethodProgressNotification {
				t.Fatalf("unexpected notification %q", msg.Notification.Method)
			}
			var p progressParams
			if err := json.Unmarshal(msg.Notification.Params, &p); err != nil {
				t.Fatalf("decode progress: %v", err)
			}
			notes = append(notes, p)
			continue
		}
		if msg.Response == nil {
			t.Fatalf("unexpected message %+v", msg)
		}
		break
	}
	if len(notes) < 1 {
		t.Fatalf("want at least one heartbeat, got %+v", notes)
	}
	if string(notes[0].ProgressToken) != `"tok-1"` {
		t.Errorf("first note = %+v, want the token", notes[0])
	}
	for i := 0; i < len(notes); i++ {
		if i > 0 && notes[i].Progress <= notes[i-1].Progress {
			t.Errorf("progress not increasing: %v then %v", notes[i-1].Progress, notes[i].Progress)
		}
		if !strings.HasPrefix(notes[i].Message, "Running for ") || !strings.HasSuffix(notes[i].Message, "s") {
			t.Errorf("heartbeat message = %q", notes[i].Message)
		}
	}
	if notes[0].Message != "Running for 0.03s" {
		t.Errorf("first heartbeat = %q, want \"Running for 0.03s\"", notes[0].Message)
	}
	fc.inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// TestNoProgressWithoutToken: no progressToken → no notifications at all.
func TestNoProgressWithoutToken(t *testing.T) {
	s := NewServer(DefaultServerInfo, WithProgressInterval(10*time.Millisecond))
	s.RegisterTool("slow", mcp.ToolSchema{Type: "object"}, func(ctx context.Context, _ json.RawMessage) (mcp.CallToolResult, error) {
		time.Sleep(50 * time.Millisecond)
		return mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok"}}}, nil
	})
	fc, done := startServer(t, s)
	fc.write(t, newRequest(t, 1, mcp.MethodToolsCall, mcp.CallToolParams{Name: "slow"}))
	msg := fc.readMsg(t)
	if msg.Response == nil {
		t.Fatalf("expected the response first (no progress), got %+v", msg)
	}
	fc.inW.Close()
	<-done
}

func TestProgressTokenFromMeta(t *testing.T) {
	cases := map[string]string{
		`{"progressToken":"a"}`:  `"a"`,
		`{"progressToken":7}`:    `7`,
		`{"progressToken":null}`: ``,
		`{"progressToken":{}}`:   ``,
		`{}`:                     ``,
		`not json`:               ``,
	}
	for in, want := range cases {
		if got := string(progressTokenFromMeta(json.RawMessage(in))); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}

// TestHeartbeatSendFailureIsIgnored: a failing send is reported through the
// error hook and never stops the heartbeat or the call.
func TestHeartbeatSendFailureIsIgnored(t *testing.T) {
	s := NewServer(DefaultServerInfo)
	var mu sync.Mutex
	var errs []error
	s.OnError(func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() })
	hb := s.startProgressHeartbeat(json.RawMessage(`1`), func(progressParams) error { return errors.New("pipe closed") }, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	hb.stop()
	hb.stop() // idempotent
	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "progress heartbeat not sent: pipe closed") {
		t.Fatalf("errs = %v", errs)
	}
}
