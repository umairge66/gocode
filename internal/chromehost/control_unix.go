//go:build !windows

package chromehost

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/umairge66/gocode/internal/daemon"
)

// listenControl binds the unix-domain control socket at ep.Address. It creates
// the parent runtime directory 0700 (same mode the daemon uses) and unlinks any
// stale socket left by a previous host before binding.
//
// SINGLETON / TAKEOVER: removing the existing path and binding a fresh socket is
// how a newly spawned host (Chrome relaunched the native host when the extension
// reconnected) TAKES OVER ownership — new connections reach the newest listener,
// and the previous host's listener, if any, no longer receives them. This
// mirrors the daemon's listenEndpoint (mkdir 0700 + remove stale + Listen): at
// any moment exactly one host owns the socket path, and the latest one wins.
func listenControl(ep ControlEndpoint) (net.Listener, error) {
	if dir := filepath.Dir(ep.Address); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		// The runtime tree lives under the shared temp root: a directory
		// another user pre-created (or a symlink planted there) would let them
		// replace our socket and intercept browser commands. Refuse it.
		if err := vetControlDirs(filepath.Dir(dir), dir); err != nil {
			return nil, err
		}
	}
	// Replace a stale socket from a prior host; ignore if absent.
	if err := os.Remove(ep.Address); err != nil && !os.IsNotExist(err) {
		_ = err
	}
	l, err := net.Listen(ep.Network, ep.Address)
	if err != nil {
		return nil, err
	}
	ul, ok := l.(*net.UnixListener)
	if !ok {
		return l, nil
	}
	// Go unlinks a unix listener's path on Close. After a newer host took over,
	// that path is the NEWER host's socket, so the old host's shutdown would
	// delete it and leave the live host unreachable ("Chrome is not connected"
	// until the extension is reloaded). Remove the file only while it is still
	// ours.
	ul.SetUnlinkOnClose(false)
	if err := os.Chmod(ep.Address, 0o600); err != nil {
		_ = ul.Close()
		return nil, err
	}
	fi, err := os.Stat(ep.Address)
	if err != nil {
		_ = ul.Close()
		return nil, err
	}
	return &ownedListener{UnixListener: ul, path: ep.Address, file: fi}, nil
}

// ownedListener is a unix listener that, on Close, removes its socket file only
// if the path still names the socket this listener created.
type ownedListener struct {
	*net.UnixListener
	path string
	file os.FileInfo
}

// Close stops listening and removes the socket file if it is still ours.
func (l *ownedListener) Close() error {
	err := l.UnixListener.Close()
	if fi, statErr := os.Stat(l.path); statErr == nil && os.SameFile(fi, l.file) {
		_ = os.Remove(l.path)
	}
	return err
}

// socketMissing reports whether the listener's socket file no longer exists
// (deleted by something else). A file replaced by a newer host is not missing:
// the newest host owns the path.
func socketMissing(l net.Listener) bool {
	ol, ok := l.(*ownedListener)
	if !ok {
		return false
	}
	_, err := os.Stat(ol.path)
	return os.IsNotExist(err)
}

// dialControl opens a connection to the unix-domain control socket. A missing
// socket (no host listening) surfaces as a dial error the client maps to
// ErrChromeNotConnected.
//
// The socket path must not be a symlink and the listening endpoint must be
// owned by this user: browser commands (and the page content they return) are
// never exchanged with a socket another local user bound.
func dialControl(ctx context.Context, ep ControlEndpoint) (net.Conn, error) {
	if fi, err := os.Lstat(ep.Address); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("chromehost: refusing control socket %s: it is a symlink", ep.Address)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, ep.Network, ep.Address)
	if err != nil {
		return nil, err
	}
	uid, ok := daemon.ReadPeerUID(conn)
	if !ok || uid != controlUID() {
		_ = conn.Close()
		return nil, fmt.Errorf("chromehost: refusing control socket %s: endpoint is not owned by this user", ep.Address)
	}
	return conn, nil
}

// vetControlDirs requires each dir to be a real (non-symlink) directory owned
// by this user and tightens its mode to 0700.
func vetControlDirs(dirs ...string) error {
	uid := controlUID()
	for _, d := range dirs {
		fi, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("chromehost: control socket directory %s is not a real directory; refusing to use it", d)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			return fmt.Errorf("chromehost: control socket directory %s is not owned by this user; refusing to use it", d)
		}
		if fi.Mode().Perm() != 0o700 {
			if err := os.Chmod(d, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

// controlUID is os.Getuid, replaceable in tests.
var controlUID = os.Getuid
