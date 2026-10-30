//go:build windows

package namedpipe

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// These tests exercise the real Win32 named-pipe transport. They only run on
// Windows.

func testPipeName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`\\.\pipe\gocode-namedpipe-test-%d-%x`, os.Getpid(), b)
}

func listen(t *testing.T, name string) *Listener {
	t.Helper()
	l, err := Listen(name, nil)
	if err != nil {
		t.Fatalf("Listen(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func dial(t *testing.T, name string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, name)
	if err != nil {
		t.Fatalf("Dial(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestListenDialRoundTrip(t *testing.T) {
	name := testPipeName(t)
	l := listen(t, name)

	errc := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer c.Close()
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			errc <- err
			return
		}
		_, err = c.Write([]byte("echo:" + line))
		errc <- err
	}()

	c := dial(t, name)
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo:hello\n" {
		t.Fatalf("reply = %q", got)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server: %v", err)
	}
	// The server closed its end: the next read is a clean EOF.
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after server close = %v, want io.EOF", err)
	}
	if c.LocalAddr().Network() != "pipe" || c.RemoteAddr().String() != name {
		t.Fatalf("addrs = %v / %v", c.LocalAddr(), c.RemoteAddr())
	}
}

func TestManySequentialClients(t *testing.T) {
	name := testPipeName(t)
	l := listen(t, name)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, io.LimitReader(c, 1))
			}()
		}
	}()
	for i := 0; i < 20; i++ {
		c := dial(t, name)
		if _, err := c.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("client %d write: %v", i, err)
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(c, b); err != nil || b[0] != byte(i) {
			t.Fatalf("client %d read = %v %v", i, b, err)
		}
		_ = c.Close()
	}
}

func TestReadDeadline(t *testing.T) {
	name := testPipeName(t)
	l := listen(t, name)
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	c := dial(t, name)
	srv := <-accepted
	defer srv.Close()

	if err := c.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	start := time.Now()
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read past deadline = %v, want a timeout net.Error", err)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read past deadline = %v, want os.ErrDeadlineExceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("deadline fired after %v", d)
	}
	// Clearing the deadline makes the conn usable again.
	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = srv.Write([]byte("x")) }()
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatalf("read after clearing deadline: %v", err)
	}
}

func TestCloseUnblocksAccept(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond) // let Accept park in ConnectNamedPipe
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}
	// The name is released: nothing is listening any more.
	if Exists(name) {
		t.Fatal("pipe still exists after Close")
	}
	if _, err := Listen(name, nil); err != nil {
		t.Fatalf("re-Listen after Close: %v", err)
	}
}

func TestCloseUnblocksConnRead(t *testing.T) {
	name := testPipeName(t)
	l := listen(t, name)
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(5 * time.Second)
		}
	}()
	c := dial(t, name)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	_ = c.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Read returned nil after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestFirstInstanceRefusesExistingName(t *testing.T) {
	name := testPipeName(t)
	listen(t, name)
	_, err := Listen(name, nil)
	if !errors.Is(err, ErrPipeInUse) {
		t.Fatalf("second Listen = %v, want ErrPipeInUse", err)
	}
	// Joining explicitly is allowed for the same user (our DACL grants it).
	l2, err := Listen(name, &ListenConfig{JoinExisting: true})
	if err != nil {
		t.Fatalf("JoinExisting Listen: %v", err)
	}
	_ = l2.Close()
}

func TestDialMissingPipeIsDialOpError(t *testing.T) {
	_, err := Dial(context.Background(), testPipeName(t))
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "dial" {
		t.Fatalf("Dial(missing) = %v, want a dial *net.OpError", err)
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("Dial(missing) = %v, want ERROR_FILE_NOT_FOUND", err)
	}
}

func TestDialBusyHonoursContext(t *testing.T) {
	name := testPipeName(t)
	// Never Accept, so the listener never pre-creates a second instance; the
	// first Dial takes the only idle instance.
	listen(t, name)
	dial(t, name)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, name)
	if err == nil {
		t.Fatal("Dial with every instance busy succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial(busy) = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("busy Dial ignored its context")
	}
}

func TestExists(t *testing.T) {
	name := testPipeName(t)
	if Exists(name) {
		t.Fatal("Exists before Listen")
	}
	l := listen(t, name)
	if !Exists(name) {
		t.Fatal("!Exists while listening")
	}
	_ = l.Close()
	if Exists(name) {
		t.Fatal("Exists after Close")
	}
}

// The peer check accepts our own process and rejects a different SID. A real
// second user is not available in CI, so the mismatch is exercised by
// comparing our own process against LocalSystem's SID.
func TestVerifyProcessUser(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyProcessUser(windows.GetCurrentProcessId(), me); err != nil {
		t.Fatalf("own process rejected: %v", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if me.Equals(system) {
		t.Skip("test is running as LocalSystem")
	}
	if err := verifyProcessUser(windows.GetCurrentProcessId(), system); !errors.Is(err, ErrPeerMismatch) {
		t.Fatalf("verify against another SID = %v, want ErrPeerMismatch", err)
	}
	// An unopenable pid fails closed.
	if err := verifyProcessUser(0xFFFFFFF0, me); !errors.Is(err, ErrPeerMismatch) {
		t.Fatalf("verify of a bogus pid = %v, want ErrPeerMismatch", err)
	}
}

// The pipe's DACL is the user-only descriptor.
func TestPipeSecurityDescriptor(t *testing.T) {
	name := testPipeName(t)
	listen(t, name)
	c := dial(t, name)
	f := c.(*pipeConn).f
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetSecurityInfo: %v", err)
	}
	me, _ := currentUserSID()
	// Generic rights are mapped at creation (GA -> FA), so compare structure:
	// a protected DACL with exactly the user and SYSTEM ACEs.
	got := sd.String()
	want := "D:P(A;;FA;;;" + me.String() + ")(A;;FA;;;SY)"
	if got != want {
		t.Fatalf("pipe DACL = %q, want %q", got, want)
	}
}
