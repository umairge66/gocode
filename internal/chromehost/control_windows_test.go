//go:build windows

package chromehost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/namedpipe"
)

// These tests exercise the real named-pipe control socket and the host
// takeover handshake. They only run on Windows.

// nameRelay answers every call with its own name, so a test can tell which
// host served a request.
type nameRelay string

func (r nameRelay) Send(context.Context, string, any) (json.RawMessage, error) {
	return json.Marshal(string(r))
}

// serveHost binds the control pipe for cfg (taking it over if needed) and
// serves it with a relay answering name. stop shuts the host down.
func serveHost(t *testing.T, cfg, name string) (stop func()) {
	t.Helper()
	ep, err := ControlSocketEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := listenControl(ep)
	if err != nil {
		t.Fatalf("host %s: listenControl: %v", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := &ControlServer{Relay: nameRelay(name)}
	go func() { _ = s.Serve(ctx, l); close(done) }()
	return func() {
		cancel()
		<-done
	}
}

func whoServes(t *testing.T, cfg string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := NewControlClient(cfg).Call(ctx, "who", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPipeEndpointName(t *testing.T) {
	ep, err := ControlSocketEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ep.Network != "pipe" || namedpipe.ValidateName(ep.Address) != nil {
		t.Fatalf("endpoint = %+v", ep)
	}
}

func TestPipeRoundTrip(t *testing.T) {
	cfg := t.TempDir()
	stop := serveHost(t, cfg, "A")
	defer stop()
	if got := whoServes(t, cfg); got != "A" {
		t.Fatalf("served by %q, want A", got)
	}
}

// No host: the client retries for connectRetryWindow and then reports
// ErrChromeNotConnected.
func TestPipeNoHostIsNotConnected(t *testing.T) {
	old := connectRetryWindow
	connectRetryWindow = 300 * time.Millisecond
	defer func() { connectRetryWindow = old }()
	_, err := NewControlClient(t.TempDir()).Call(context.Background(), "who", nil)
	if !errors.Is(err, ErrChromeNotConnected) {
		t.Fatalf("Call with no host = %v, want ErrChromeNotConnected", err)
	}
}

// A host that starts within the retry window is reached by a call that was
// made before it existed.
func TestPipeClientRetriesUntilHostStarts(t *testing.T) {
	cfg := t.TempDir()
	stopc := make(chan func(), 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		stopc <- serveHost(t, cfg, "late")
	}()
	if got := whoServes(t, cfg); got != "late" {
		t.Fatalf("served by %q, want late", got)
	}
	(<-stopc)()
}

// The newest host takes the endpoint over, and the old host's shutdown does
// not break it.
func TestPipeNewestHostWinsAndOldShutdownIsHarmless(t *testing.T) {
	cfg := t.TempDir()
	stopA := serveHost(t, cfg, "A")
	if got := whoServes(t, cfg); got != "A" {
		t.Fatalf("before takeover: %q", got)
	}
	stopB := serveHost(t, cfg, "B")
	defer stopB()
	for i := 0; i < 5; i++ {
		if got := whoServes(t, cfg); got != "B" {
			t.Fatalf("after takeover call %d served by %q, want B", i, got)
		}
	}
	stopA()
	if got := whoServes(t, cfg); got != "B" {
		t.Fatalf("after the old host stopped: %q, want B", got)
	}
}

// A request in flight on the old host when it is taken over still completes,
// and the takeover does not wait for it beyond takeoverSettle.
func TestPipeTakeoverWithInFlightRequest(t *testing.T) {
	cfg := t.TempDir()
	ep, _ := ControlSocketEndpoint(cfg)
	l, err := listenControl(ep)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sA := &ControlServer{Relay: blockingRelay{release: release}}
	go func() { _ = sA.Serve(ctx, l) }()

	slow := make(chan error, 1)
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := NewControlClient(cfg).Call(c, "slow", nil)
		slow <- err
	}()
	time.Sleep(200 * time.Millisecond) // the slow call is now in flight on A

	stopB := serveHost(t, cfg, "B") // must not block on A's in-flight call
	defer stopB()
	if got := whoServes(t, cfg); got != "B" {
		t.Fatalf("new calls served by %q, want B", got)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("in-flight call on the old host failed: %v", err)
	}
}

type blockingRelay struct{ release chan struct{} }

func (r blockingRelay) Send(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	select {
	case <-r.release:
		return json.RawMessage(`"A"`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A host that ceded the endpoint re-binds it once the newer host is gone,
// like the unix host re-binding a deleted socket file.
func TestPipeCededHostRebindsWhenNewerHostExits(t *testing.T) {
	old := socketCheckInterval
	socketCheckInterval = 50 * time.Millisecond
	defer func() { socketCheckInterval = old }()

	cfg := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	sA := &ControlServer{Relay: nameRelay("A")}
	go func() { done <- sA.ListenAndServe(ctx, cfg) }()
	if got := whoServes(t, cfg); got != "A" {
		t.Fatalf("first: %q", got)
	}

	stopB := serveHost(t, cfg, "B")
	if got := whoServes(t, cfg); got != "B" {
		t.Fatalf("after takeover: %q", got)
	}
	stopB()

	if got := whoServes(t, cfg); got != "A" { // client retries while A re-binds
		t.Fatalf("after the newer host exited: %q, want A", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	ep, _ := ControlSocketEndpoint(cfg)
	if namedpipe.Exists(ep.Address) {
		t.Fatal("pipe left behind after shutdown")
	}
}

// Close unblocks a ceded listener's Accept.
func TestHostListenerCloseAfterCede(t *testing.T) {
	ep, _ := ControlSocketEndpoint(t.TempDir())
	l, err := listenControl(ep)
	if err != nil {
		t.Fatal(err)
	}
	hl := l.(*hostListener)
	errc := make(chan error, 1)
	go func() {
		_, err := hl.Accept()
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	hl.Cede()
	select {
	case err := <-errc:
		t.Fatalf("Accept returned after Cede (%v); it must block until Close", err)
	case <-time.After(200 * time.Millisecond):
	}
	if !socketMissing(hl) {
		t.Fatal("ceded listener with no pipe left should report missing")
	}
	_ = hl.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock a ceded Accept")
	}
}
