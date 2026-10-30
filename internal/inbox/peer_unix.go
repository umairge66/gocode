//go:build !windows

package inbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
)

// maxSocketPathBytes is the sun_path budget (103 bytes).
const maxSocketPathBytes = 103

// DefaultSocketPath is $XDG_RUNTIME_DIR (else gocode_CODE_TMPDIR, else the
// temp dir)/cc-socks/<pid>.sock, or the per-uid /tmp/cc-socks-<uid>/<pid>.sock
// fallback when that path is too long for a unix socket.
func DefaultSocketPath() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.Getenv("gocode_CODE_TMPDIR")
	}
	if base == "" {
		base = os.TempDir()
	}
	p, err := filepath.Abs(filepath.Join(base, "cc-socks", strconv.Itoa(os.Getpid())+".sock"))
	if err == nil && len(p) <= maxSocketPathBytes {
		return p
	}
	return fallbackSocketPath()
}

// fallbackSocketPath is /tmp/cc-socks-<uid>/<pid>.sock.
func fallbackSocketPath() string {
	return filepath.Join("/tmp", "cc-socks-"+strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getpid())+".sock")
}

// sockDirError carries the socketsDirVetKind.
type sockDirError struct {
	kind string
	msg  string
}

func (e *sockDirError) Error() string { return e.msg }

// vetSocketDir (simplified): every existing ancestor of dir must be a
// directory owned by us or root that is not group/world-writable unless
// sticky; missing components are created 0700; dir itself must be a real
// (non-symlink) directory owned by us, and is forced to mode 0700.
func vetSocketDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return &sockDirError{"internal", "sockets directory must be absolute here"}
	}
	uid := os.Getuid()
	var missing []string
	for p := filepath.Dir(dir); ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			missing = append([]string{p}, missing...)
		} else {
			if !fi.IsDir() {
				return &sockDirError{"not_directory", "a sockets-directory component exists but is not a directory — refusing to use it"}
			}
			st, _ := fi.Sys().(*syscall.Stat_t)
			if st != nil {
				mode := fi.Mode()
				owner := int(st.Uid)
				sticky := mode&fs.ModeSticky != 0
				ok := owner == uid || owner == 0
				if ok && !sticky && mode.Perm()&0o002 != 0 {
					ok = false
				}
				if ok && !sticky && mode.Perm()&0o020 != 0 && !(owner == uid && int(st.Gid) == os.Getgid() && os.Getgid() == uid) {
					ok = false
				}
				if !ok {
					return &sockDirError{"directory_rule", "a sockets-directory component is not a private-or-sticky directory owned by us or root — refusing to use it"}
				}
			}
			break
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	for _, p := range missing {
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return &sockDirError{"leaf_shape", "sockets directory is a symlink — refusing to use it"}
	}
	if !fi.IsDir() {
		return &sockDirError{"leaf_shape", "sockets directory exists but is not a directory"}
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return &sockDirError{"foreign_owner", "sockets directory is owned by another user — refusing to use it"}
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// probeLive reports "live" when something accepts on path within 250ms.
func probeLive(path string) bool {
	c, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// listenLocal binds the inbox socket. The default path's directory is vetted
// (falling back to the per-uid /tmp directory when refused); a stale socket at
// the path is replaced; a live one (another session in a sibling pid
// namespace) makes us bind a moved-aside sibling "<pid>-<8hex>.sock". The
// socket is chmod 0600.
func listenLocal(path string, explicit bool) (net.Listener, string, error) {
	if !explicit {
		dir := filepath.Dir(path)
		if err := vetSocketDir(dir); err != nil {
			var se *sockDirError
			fb := fallbackSocketPath()
			if !errors.As(err, &se) || filepath.Dir(fb) == dir {
				return nil, "", fmt.Errorf("failed to set up sockets directory %s (refusing to bind — cross-session messaging is OFF for this session): %w", dir, err)
			}
			if err2 := vetSocketDir(filepath.Dir(fb)); err2 != nil {
				return nil, "", fmt.Errorf("failed to set up sockets directory %s (refusing to bind — cross-session messaging is OFF for this session): %w", filepath.Dir(fb), err2)
			}
			path = fb
		}
	} else {
		fi, err := os.Lstat(filepath.Dir(path))
		if err != nil {
			return nil, "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
			return nil, "", fmt.Errorf("--messaging-socket-path directory %s is not private (mode %o); the socket directory must be mode 0700 so no other user or group can reach or replace the socket", filepath.Dir(path), fi.Mode().Perm())
		}
		if probeLive(path) {
			return nil, "", fmt.Errorf("--messaging-socket-path points to a live socket: %s. Another process is listening there. Remove it or choose a different path.", path)
		}
		_ = os.Remove(path)
	}
	try := func(p string) (net.Listener, error) {
		ln, err := net.Listen("unix", p)
		if err == nil {
			if ul, ok := ln.(*net.UnixListener); ok {
				ul.SetUnlinkOnClose(true)
			}
		}
		return ln, err
	}
	ln, err := try(path)
	if err != nil && !explicit && errors.Is(err, syscall.EADDRINUSE) {
		if !probeLive(path) {
			_ = os.Remove(path)
			ln, err = try(path)
		}
		for i := 0; err != nil && i < 3; i++ {
			alt := movedAsidePath(path)
			ln, err = try(alt)
			if err == nil {
				path = alt
			}
		}
	}
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, "", err
	}
	return ln, path, nil
}

// movedAsidePath is "<base>-<8hex>.sock" beside path.
func movedAsidePath(path string) string {
	return path[:len(path)-len(".sock")] + "-" + randomHex(4) + ".sock"
}

// dialLocal connects to a peer inbox: it refuses a symlinked socket path,
// connects within the timeout, and refuses an endpoint not owned by this user.
func dialLocal(ctx context.Context, target string, timeout time.Duration) (net.Conn, error) {
	fi, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, &PeerSendError{Class: errClassRefused, Kind: "unvettable", Msg: "Refusing to send: cannot vet reply target"}
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, &PeerSendError{Class: errClassRefused, Kind: "symlink", Msg: "Refusing to send: reply target is a symlink"}
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", target)
	if err != nil {
		return nil, err
	}
	uid, ok := daemon.ReadPeerUID(conn)
	if !ok {
		_ = conn.Close()
		return nil, &PeerSendError{Class: errClassRefused, Kind: "endpoint-unverifiable", Msg: "Refusing to send: connected endpoint owner could not be read"}
	}
	if uid != os.Getuid() {
		_ = conn.Close()
		return nil, &PeerSendError{Class: errClassRefused, Kind: "wrong-endpoint", Msg: "Refusing to send: connected endpoint is not owned by this user"}
	}
	return conn, nil
}

// checkInboundPeer enforces same-user delivery: the connecting process must
// run as our uid. This FAILS CLOSED (an unreadable uid
// is refused); the 0700 sockets directory is the first layer.
func checkInboundPeer(conn net.Conn) error {
	return checkPeerUID(conn, daemon.ReadPeerUID)
}

// checkPeerUID applies the same-user policy with an injected uid read.
func checkPeerUID(conn net.Conn, read func(net.Conn) (int, bool)) error {
	uid, ok := read(conn)
	if !ok {
		return errors.New("permission denied: connecting peer uid could not be read")
	}
	if me := os.Getuid(); uid != me {
		return fmt.Errorf("permission denied: connecting uid %d != inbox uid %d", uid, me)
	}
	return nil
}

// canonicalEndpoint, for unix paths, is the resolved absolute path ("" for a
// path containing a ".." segment).
func canonicalEndpoint(p string) string {
	if hasDotDot(p) {
		return ""
	}
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// isGoneError is ENOENT/ECONNREFUSED; isBusyError is EBUSY/EAGAIN.
func isGoneError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func isBusyError(err error) bool {
	return errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EAGAIN)
}
