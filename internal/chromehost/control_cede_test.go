package chromehost

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// cedingListener is a stand-in for the Windows pipe listener: it records Cede.
type cedingListener struct {
	net.Listener
	ceded chan struct{}
}

func (c *cedingListener) Cede() { close(c.ceded) }

// plainListener does NOT support ceding (like the unix listener).
type plainListener struct{ net.Listener }

type recordingRelay struct{ methods chan string }

func (r recordingRelay) Send(_ context.Context, method string, _ any) (json.RawMessage, error) {
	r.methods <- method
	return json.RawMessage(`"relayed"`), nil
}

// roundTrip serves one request on an in-memory connection attributed to l
// and returns the reply.
func roundTrip(t *testing.T, s *ControlServer, l net.Listener, req ControlRequest) ControlResponse {
	t.Helper()
	cli, srv := net.Pipe()
	defer cli.Close()
	go s.serveConn(context.Background(), srv, l)
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeControlFrame(cli, req); err != nil {
		t.Fatal(err)
	}
	frame, err := readControlFrame(bufio.NewReader(cli))
	if err != nil {
		t.Fatal(err)
	}
	var resp ControlResponse
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// A takeover request makes a cede-capable listener cede (and is NOT relayed to
// the extension); the ack is sent after Cede.
func TestTakeoverRequestCedesListener(t *testing.T) {
	relay := recordingRelay{methods: make(chan string, 1)}
	s := &ControlServer{Relay: relay, CheckPeer: func(net.Conn) error { return nil }}
	l := &cedingListener{ceded: make(chan struct{})}
	resp := roundTrip(t, s, l, ControlRequest{Method: hostTakeoverMethod})
	if resp.Error != "" || string(resp.Result) != "true" {
		t.Fatalf("takeover reply = %+v", resp)
	}
	select {
	case <-l.ceded:
	default:
		t.Fatal("listener was not ceded before the ack")
	}
	select {
	case m := <-relay.methods:
		t.Fatalf("takeover was relayed to the extension as %q", m)
	default:
	}
}

// On a listener that cannot cede (unix), the reserved name is relayed like any
// other method: unix behaviour is unchanged.
func TestTakeoverRequestOnPlainListenerIsRelayed(t *testing.T) {
	relay := recordingRelay{methods: make(chan string, 1)}
	s := &ControlServer{Relay: relay, CheckPeer: func(net.Conn) error { return nil }}
	resp := roundTrip(t, s, plainListener{}, ControlRequest{Method: hostTakeoverMethod})
	if string(resp.Result) != `"relayed"` {
		t.Fatalf("reply = %+v, want the relayed result", resp)
	}
	if m := <-relay.methods; m != hostTakeoverMethod {
		t.Fatalf("relayed method = %q", m)
	}
}

// A takeover request from a peer that fails the peer check is rejected and
// does not cede.
func TestTakeoverRequiresPeerCheck(t *testing.T) {
	s := &ControlServer{
		Relay:     recordingRelay{methods: make(chan string, 1)},
		CheckPeer: func(net.Conn) error { return errTestPeer },
	}
	l := &cedingListener{ceded: make(chan struct{})}
	cli, srv := net.Pipe()
	defer cli.Close()
	done := make(chan struct{})
	go func() { s.serveConn(context.Background(), srv, l); close(done) }()
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
	frame, err := readControlFrame(bufio.NewReader(cli))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(frame), "permission denied") {
		t.Fatalf("reply = %s, want permission denied", frame)
	}
	<-done
	select {
	case <-l.ceded:
		t.Fatal("rejected peer made the listener cede")
	default:
	}
}

type testPeerError struct{}

func (testPeerError) Error() string { return "different user" }

var errTestPeer error = testPeerError{}

func TestControlPipeName(t *testing.T) {
	if got, want := ControlPipeName("1a2b3c4d"), `\\.\pipe\gocode-chrome-host-1a2b3c4d`; got != want {
		t.Fatalf("ControlPipeName = %q, want %q", got, want)
	}
}
