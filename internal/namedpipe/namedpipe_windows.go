//go:build windows

package namedpipe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// -----------------------------------------------------------------------------
// Design
//
// LISTENER. A Listener keeps exactly one idle ("next") pipe instance waiting
// for a client at all times while it is open, so the pipe name never vanishes
// between two Accepts (a client would otherwise see ERROR_FILE_NOT_FOUND and
// think nothing is listening). Accept issues an OVERLAPPED ConnectNamedPipe on
// that instance, waits for it, pre-creates the following idle instance, then
// verifies the client's user SID and only then returns the connection.
//
// The first instance is created with FILE_FLAG_FIRST_PIPE_INSTANCE unless the
// caller explicitly asks to join an existing pipe (ListenConfig.JoinExisting):
// that turns a squatted or still-live name into ErrPipeInUse instead of
// silently sharing the name with whoever created it. Named pipes cannot go
// stale — the name disappears when the last handle is closed, including when
// the owning process dies — so there is nothing to "remove" before listening.
//
// CLOSE UNBLOCKS ACCEPT. Close marks the listener closed, closes the idle
// instance and CancelIoEx()es a ConnectNamedPipe in flight; the blocked
// Accept then completes with ERROR_OPERATION_ABORTED, closes its handle and
// returns an error wrapping net.ErrClosed. Close waits for that handle to be
// released so a re-Listen in the same process does not race its own leftovers.
//
// CONNECTIONS. An accepted or dialed handle is opened FILE_FLAG_OVERLAPPED and
// wrapped with os.NewFile. Since Go 1.25, os.NewFile detects overlapped handles
// and associates them with the runtime's I/O completion port, which gives:
// Read/Write that do not pin an OS thread, working SetDeadline/
// SetReadDeadline/SetWriteDeadline (timeouts surface as
// os.ErrDeadlineExceeded, a net.Error with Timeout()==true), and Close that
// CancelIoEx()es pending pipe I/O. pipeConn adapts that to net.Conn.
//
// Only Accept's ConnectNamedPipe uses a hand-rolled event-based OVERLAPPED
// (before the handle is handed to os.NewFile): it parks one goroutine/thread
// in GetOverlappedResult per listener, which is fine for these single-user
// local endpoints.
// -----------------------------------------------------------------------------

// pipeBufferSize is the advisory in/out buffer size of each pipe instance.
const pipeBufferSize = 64 << 10

// busyRetryInterval is how long Dial waits before retrying a pipe whose
// instances are all busy (ERROR_PIPE_BUSY).
const busyRetryInterval = 10 * time.Millisecond

// ListenConfig tunes Listen. The zero value (or a nil *ListenConfig) is the
// secure default: current-user + SYSTEM DACL, and this listener must CREATE the
// pipe name (FILE_FLAG_FIRST_PIPE_INSTANCE).
type ListenConfig struct {
	// JoinExisting adds instances to an already-existing pipe of the same name
	// instead of requiring this listener to create it. The existing pipe's
	// security descriptor (set by its creator) still decides whether that is
	// permitted: our own DACL only grants FILE_CREATE_PIPE_INSTANCE to the
	// current user. Only the chrome host's takeover path sets this, and only
	// after it has verified the existing server runs as the same user.
	JoinExisting bool
}

// Listener is a named-pipe net.Listener. See the package design note.
type Listener struct {
	name string
	path *uint16
	sa   *windows.SecurityAttributes
	user *windows.SID

	acceptMu sync.Mutex // serializes Accept

	mu          sync.Mutex
	closed      bool
	next        windows.Handle // idle instance waiting for ConnectNamedPipe
	pending     windows.Handle // instance with a ConnectNamedPipe in flight
	pendingDone chan struct{}  // closed when pending has been released
}

var _ net.Listener = (*Listener)(nil)

// Listen creates the pipe name with a current-user-only DACL and returns a
// listener. With a nil or zero cfg the name must not exist yet; if it does,
// the error wraps ErrPipeInUse.
func Listen(name string, cfg *ListenConfig) (*Listener, error) {
	if cfg == nil {
		cfg = &ListenConfig{}
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	user, err := currentUserSID()
	if err != nil {
		return nil, opErr("listen", name, err)
	}
	sddl, err := SecurityDescriptor(user.String(), true)
	if err != nil {
		return nil, opErr("listen", name, err)
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, opErr("listen", name, os.NewSyscallError("ConvertStringSecurityDescriptorToSecurityDescriptor", err))
	}
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, opErr("listen", name, err)
	}
	l := &Listener{
		name: name,
		path: path,
		sa: &windows.SecurityAttributes{
			Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
			SecurityDescriptor: sd,
			InheritHandle:      0,
		},
		user:    user,
		next:    windows.InvalidHandle,
		pending: windows.InvalidHandle,
	}
	h, err := l.createInstance(!cfg.JoinExisting)
	if err != nil {
		if !cfg.JoinExisting && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			// FILE_FLAG_FIRST_PIPE_INSTANCE fails with ACCESS_DENIED when the
			// name already exists.
			err = fmt.Errorf("%w: %w", ErrPipeInUse, err)
		}
		return nil, opErr("listen", name, err)
	}
	l.next = h
	return l, nil
}

// createInstance creates one server instance of the pipe. first requests
// FILE_FLAG_FIRST_PIPE_INSTANCE.
func (l *Listener) createInstance(first bool) (windows.Handle, error) {
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	mode := uint32(windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS)
	h, err := windows.CreateNamedPipe(l.path, flags, mode, windows.PIPE_UNLIMITED_INSTANCES,
		pipeBufferSize, pipeBufferSize, 0, l.sa)
	if err != nil {
		return windows.InvalidHandle, os.NewSyscallError("CreateNamedPipe", err)
	}
	return h, nil
}

// Addr implements net.Listener.
func (l *Listener) Addr() net.Addr { return Addr(l.name) }

// Accept waits for a client, verifies that it runs as the same Windows user,
// and returns the connection. A client that fails the check (or whose identity
// cannot be determined) is disconnected and Accept keeps waiting: one bad
// client never ends the serve loop. After Close, Accept returns an error that
// wraps net.ErrClosed.
func (l *Listener) Accept() (net.Conn, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	for {
		h, err := l.connectNext()
		if err != nil {
			return nil, err
		}
		if h == windows.InvalidHandle {
			continue // client vanished before we saw it; wait for the next one
		}
		if err := verifyClient(h, l.user); err != nil {
			// Fail closed: drop the client without sending it anything.
			_ = windows.DisconnectNamedPipe(h)
			_ = windows.CloseHandle(h)
			continue
		}
		return newConn(h, l.name), nil
	}
}

// connectNext waits for a client on the idle instance. It returns the
// connected handle, InvalidHandle (client already gone: retry), or an error
// (listener closed or a fatal Win32 failure).
func (l *Listener) connectNext() (windows.Handle, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return windows.InvalidHandle, opErr("accept", l.name, net.ErrClosed)
	}
	h := l.next
	l.next = windows.InvalidHandle
	if h == windows.InvalidHandle {
		var err error
		if h, err = l.createInstance(false); err != nil {
			l.mu.Unlock()
			return windows.InvalidHandle, opErr("accept", l.name, err)
		}
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		l.next = h
		l.mu.Unlock()
		return windows.InvalidHandle, opErr("accept", l.name, os.NewSyscallError("CreateEvent", err))
	}
	defer windows.CloseHandle(ev)
	ov := &windows.Overlapped{HEvent: ev}

	// Issue the connect while holding mu so a concurrent Close either sees
	// l.pending (and cancels it) or ran before us (and we saw l.closed).
	err = windows.ConnectNamedPipe(h, ov)
	switch {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
		// A client connected between CreateNamedPipe and ConnectNamedPipe.
		l.mu.Unlock()
	case errors.Is(err, windows.ERROR_IO_PENDING):
		l.pending = h
		l.pendingDone = make(chan struct{})
		l.mu.Unlock()
		var n uint32
		err = windows.GetOverlappedResult(h, ov, &n, true)
		l.mu.Lock()
		done := l.pendingDone
		l.pending = windows.InvalidHandle
		l.pendingDone = nil
		closed := l.closed
		l.mu.Unlock()
		if closed || errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			_ = windows.CloseHandle(h)
			close(done)
			return windows.InvalidHandle, opErr("accept", l.name, net.ErrClosed)
		}
		close(done)
		if err != nil {
			// ERROR_NO_DATA / ERROR_BROKEN_PIPE: the client closed before
			// the connect completed. Drop this instance and try again.
			_ = windows.CloseHandle(h)
			return windows.InvalidHandle, nil
		}
	case errors.Is(err, windows.ERROR_NO_DATA):
		// The client connected and already closed its end.
		l.mu.Unlock()
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, nil
	default:
		l.mu.Unlock()
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, opErr("accept", l.name, os.NewSyscallError("ConnectNamedPipe", err))
	}

	// Keep the name alive: pre-create the next idle instance before handing
	// this one out. A failure here is retried by the next Accept.
	l.mu.Lock()
	if !l.closed && l.next == windows.InvalidHandle {
		if nh, err := l.createInstance(false); err == nil {
			l.next = nh
		}
	}
	l.mu.Unlock()
	return h, nil
}

// Close stops listening: the idle instance is closed and a blocked Accept is
// cancelled (it returns net.ErrClosed). Connections already returned by Accept
// are unaffected. Close is idempotent.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	if l.next != windows.InvalidHandle {
		_ = windows.CloseHandle(l.next)
		l.next = windows.InvalidHandle
	}
	done := l.pendingDone
	if l.pending != windows.InvalidHandle {
		_ = windows.CancelIoEx(l.pending, nil)
	}
	l.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}

// Dial connects to the local pipe name, retrying while every instance is busy
// (until ctx ends), and verifies that the server process runs as the same
// Windows user before returning. A missing pipe returns immediately with a
// *net.OpError{Op:"dial"} wrapping ERROR_FILE_NOT_FOUND, which callers treat
// as "nothing is listening". A server owned by another user returns an error
// wrapping ErrPeerMismatch (NOT a dial OpError, so it is not mistaken for an
// absent server).
func Dial(ctx context.Context, name string) (net.Conn, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, opErr("dial", name, err)
	}
	user, err := currentUserSID()
	if err != nil {
		return nil, opErr("dial", name, err)
	}
	var h windows.Handle
	for {
		// SECURITY_SQOS_PRESENT|SECURITY_IDENTIFICATION: the server may learn
		// who we are but can never impersonate us.
		h, err = windows.CreateFile(path,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION,
			0)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, opErr("dial", name, os.NewSyscallError("CreateFile", err))
		}
		select {
		case <-ctx.Done():
			return nil, opErr("dial", name, ctx.Err())
		case <-time.After(busyRetryInterval):
		}
	}
	if err := verifyServer(h, user); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("namedpipe: dial %s: %w", name, err)
	}
	return newConn(h, name), nil
}

// Exists reports whether any instance of the pipe name exists, WITHOUT
// connecting to it (so it never consumes a server instance). It is how a
// listener that ceded the name notices the name has disappeared. Errors other
// than "not found" count as existing, so a transient failure never triggers a
// spurious re-bind.
func Exists(name string) bool {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	r, _, e := procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(path)), 1)
	if r != 0 {
		return true
	}
	return !errors.Is(e, windows.ERROR_FILE_NOT_FOUND)
}

// procWaitNamedPipeW is WaitNamedPipeW, which x/sys/windows does not wrap.
var procWaitNamedPipeW = windows.NewLazySystemDLL("kernel32.dll").NewProc("WaitNamedPipeW")

// verifyClient checks the user SID of the process on the client end of h.
func verifyClient(h windows.Handle, user *windows.SID) error {
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(h, &pid); err != nil {
		return os.NewSyscallError("GetNamedPipeClientProcessId", err)
	}
	return verifyProcessUser(pid, user)
}

// verifyServer checks the user SID of the process on the server end of h.
func verifyServer(h windows.Handle, user *windows.SID) error {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(h, &pid); err != nil {
		return os.NewSyscallError("GetNamedPipeServerProcessId", err)
	}
	return verifyProcessUser(pid, user)
}

// verifyProcessUser opens pid's primary token and requires its TokenUser SID to
// equal user. Any failure to determine the identity is an error (fail closed).
// This is the Windows counterpart of the unix SO_PEERCRED/LOCAL_PEERCRED uid
// comparison in internal/daemon (peercred_*.go).
func verifyProcessUser(pid uint32, user *windows.SID) error {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("%w: cannot open peer process %d: %w", ErrPeerMismatch, pid, err)
	}
	defer windows.CloseHandle(p)
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("%w: cannot open token of peer process %d: %w", ErrPeerMismatch, pid, err)
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("%w: cannot read user of peer process %d: %w", ErrPeerMismatch, pid, err)
	}
	if !windows.EqualSid(tu.User.Sid, user) {
		return fmt.Errorf("%w: peer process %d runs as %s, this process as %s",
			ErrPeerMismatch, pid, tu.User.Sid.String(), user.String())
	}
	return nil
}

// currentUserSID returns a copy of this process's user SID.
func currentUserSID() (*windows.SID, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, os.NewSyscallError("GetTokenInformation", err)
	}
	return tu.User.Sid.Copy()
}

func opErr(op, name string, err error) error {
	return &net.OpError{Op: op, Net: "pipe", Addr: Addr(name), Err: err}
}

// pipeConn is a net.Conn over one connected, overlapped pipe handle.
type pipeConn struct {
	f    *os.File
	addr Addr
}

var _ net.Conn = (*pipeConn)(nil)

func newConn(h windows.Handle, name string) *pipeConn {
	return &pipeConn{f: os.NewFile(uintptr(h), name), addr: Addr(name)}
}

// Read implements net.Conn. io.EOF (peer closed) is returned unwrapped.
func (c *pipeConn) Read(b []byte) (int, error) {
	n, err := c.f.Read(b)
	return n, c.mapErr("read", err)
}

// Write implements net.Conn.
func (c *pipeConn) Write(b []byte) (int, error) {
	n, err := c.f.Write(b)
	return n, c.mapErr("write", err)
}

// Close implements net.Conn. It cancels pending I/O on the handle.
func (c *pipeConn) Close() error {
	return c.mapErr("close", c.f.Close())
}

// LocalAddr implements net.Conn.
func (c *pipeConn) LocalAddr() net.Addr { return c.addr }

// RemoteAddr implements net.Conn.
func (c *pipeConn) RemoteAddr() net.Addr { return c.addr }

// SetDeadline implements net.Conn.
func (c *pipeConn) SetDeadline(t time.Time) error { return c.f.SetDeadline(t) }

// SetReadDeadline implements net.Conn.
func (c *pipeConn) SetReadDeadline(t time.Time) error { return c.f.SetReadDeadline(t) }

// SetWriteDeadline implements net.Conn.
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

// mapErr turns *os.File errors into net-style errors: io.EOF is passed
// through untouched (bufio and frame readers compare it with ==), a use after
// Close becomes net.ErrClosed, and everything else is wrapped in a
// *net.OpError (so os.ErrDeadlineExceeded keeps reporting Timeout()==true).
func (c *pipeConn) mapErr(op string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	if errors.Is(err, os.ErrClosed) {
		err = net.ErrClosed
	}
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) ||
		errors.Is(err, windows.ERROR_NO_DATA) {
		if op == "read" {
			return io.EOF
		}
		err = io.ErrClosedPipe
	}
	return &net.OpError{Op: op, Net: "pipe", Source: c.addr, Addr: c.addr, Err: err}
}
