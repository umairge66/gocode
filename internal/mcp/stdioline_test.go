package mcp

import (
	"io"
	"strings"
	"testing"
	"time"
)

// A line over maxStdioLine is dropped, its request gets an error response,
// and the messages after it still arrive on the same connection.
func TestReadPumpSurvivesOversizedLine(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"` + strings.Repeat("x", maxStdioLine+10) + `"}]}}` + "\n"
	next := `{"jsonrpc":"2.0","id":8,"result":{"ok":true}}` + "\n"
	tr := &StdioTransport{msgs: make(chan Message, 4)}
	go tr.readPump(strings.NewReader(big + next))

	got := recv(t, tr)
	if got.Response == nil || string(got.Response.ID) != "7" || got.Response.Error == nil ||
		!strings.Contains(got.Response.Error.Message, "was not read") {
		t.Fatalf("oversized line: got %+v", got.Response)
	}
	got = recv(t, tr)
	if got.Response == nil || string(got.Response.ID) != "8" || string(got.Response.Result) != `{"ok":true}` {
		t.Fatalf("next message: got %+v", got.Response)
	}
}

// Messages decode correctly while the line buffer is reused between them.
func TestReadPumpReusesLineBuffer(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":"a","result":{"v":"` + strings.Repeat("1", 200000) + `"}}` + "\r\n" +
		"not json\n\n" +
		`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`
	tr := &StdioTransport{msgs: make(chan Message, 4)}
	go tr.readPump(strings.NewReader(in))

	first := recv(t, tr)
	second := recv(t, tr)
	third := recv(t, tr)
	if first.Response == nil || string(first.Response.ID) != `"a"` || len(first.Response.Result) != 200000+8 {
		t.Fatalf("first = %+v", first.Response)
	}
	if second.Notification == nil || second.Notification.Method != "notifications/tools/list_changed" {
		t.Fatalf("second = %+v", second)
	}
	if third.Request == nil || third.Request.Method != "ping" || string(third.Request.ID) != "2" {
		t.Fatalf("third (no trailing newline) = %+v", third)
	}
}

func TestOversizedResponseAttribution(t *testing.T) {
	for _, c := range []struct {
		head string
		ok   bool
		id   string
	}{
		{`{"jsonrpc":"2.0","id":12,"result":{"content":[`, true, "12"},
		{`{"id":"req-1","jsonrpc":"2.0","result":`, true, `"req-1"`},
		{`{"jsonrpc":"2.0","result":{"x":{"id":3`, false, ""},
		{`{"jsonrpc":"2.0","id":4,"method":"sampling/createMessage","params":`, false, ""},
		{`{"jsonrpc":"2.0","result":`, false, ""},
	} {
		m, ok := oversizedResponse([]byte(c.head))
		if ok != c.ok || (ok && string(m.Response.ID) != c.id) {
			t.Errorf("%s: ok=%v msg=%+v", c.head, ok, m.Response)
		}
	}
}

func TestDecodeMessageShapes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"jsonrpc":"2.0","id":1,"method":"x","params":{"a":1}}`, "request"},
		{`{"jsonrpc":"2.0","id":null,"method":"x"}`, "notification"},
		{`{"jsonrpc":"2.0","method":"x"}`, "notification"},
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, "response"},
		{`{"jsonrpc":"2.0","id":1,"result":null}`, "response"},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`, "error"},
		{`{"jsonrpc":"2.0","id":1}`, "bad"},
		{`not json`, "bad"},
	} {
		m, err := DecodeMessage([]byte(c.in))
		got := "bad"
		switch {
		case err != nil:
		case m.Request != nil:
			got = "request"
		case m.Notification != nil:
			got = "notification"
		case m.Response != nil && m.Response.Error != nil:
			got = "error"
		case m.Response != nil:
			got = "response"
		}
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.in, got, c.want)
		}
	}
}

func TestDrainStderrKeepsTail(t *testing.T) {
	tr := &StdioTransport{}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { tr.drainStderr(r); close(done) }()
	chunk := strings.Repeat("a", 64*1024)
	for i := 0; i < 3*maxStderrTail/len(chunk); i++ {
		_, _ = io.WriteString(w, chunk)
	}
	_, _ = io.WriteString(w, "LAST ERROR LINE\n")
	w.Close()
	<-done
	s := tr.Stderr()
	if len(s) > 2*maxStderrTail || !strings.HasSuffix(s, "LAST ERROR LINE\n") {
		t.Fatalf("stderr kept %d bytes, suffix %q", len(s), s[max(0, len(s)-20):])
	}
}

func recv(t *testing.T, tr *StdioTransport) Message {
	t.Helper()
	select {
	case m, ok := <-tr.msgs:
		if !ok {
			t.Fatal("message channel closed")
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a message")
	}
	return Message{}
}
