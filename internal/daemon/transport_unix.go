//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/umairge66/gocode/internal/errutil"
)

// listenEndpoint binds a unix-domain stream socket at ep.Address. It creates
// the parent runtime directory 0700 and
// unlinks any stale socket left by a previous daemon before binding.
func listenEndpoint(ep ControlEndpoint) (net.Listener, error) {
	if dir := filepath.Dir(ep.Address); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := verifyOwnedDirs(ownedDirChain(dir)...); err != nil {
			return nil, err
		}
	}
	// Remove a stale socket from a prior instance; ignore if absent.
	if err := os.Remove(ep.Address); err != nil && !os.IsNotExist(err) {
		// A non-"not exist" error (e.g. it is a live socket owned by another
		// daemon) is surfaced by the subsequent Listen's EADDRINUSE.
		_ = err
	}
	return net.Listen(ep.Network, ep.Address)
}

// dialEndpoint opens a unix-domain connection to ep.Address.
func dialEndpoint(ctx context.Context, ep ControlEndpoint) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, ep.Network, ep.Address)
	if err != nil {
		return nil, err
	}
	// Refuse an endpoint another user bound (a squatted socket would receive
	// this user's keystrokes / control requests).
	if uid, ok := readPeerUID(conn); ok && uid != currentUID() {
		_ = conn.Close()
		return nil, fmt.Errorf("refusing to connect: %s is served by uid %d, not uid %d", ep.Address, uid, currentUID())
	}
	return conn, nil
}

// ownedDirChain returns the directories the ownership guard checks for a
// socket directory: dir and its parent, extended upward to (and including)
// the per-user "cc-daemon-<uid>" root when dir lives beneath one, so that a
// deeper socket (runtime/<disc>/pty/<id>.sock) cannot sit under a root
// another user created or opened up.
func ownedDirChain(dir string) []string {
	chain := []string{dir}
	for p := filepath.Dir(dir); p != filepath.Dir(p); p = filepath.Dir(p) {
		if strings.HasPrefix(filepath.Base(p), "cc-daemon-") {
			return append(chain, p)
		}
		chain = append(chain, p)
	}
	return []string{filepath.Dir(dir), dir}
}

// currentUID is os.Getuid, replaceable in tests.
var currentUID = os.Getuid

// verifyOwnedDirs is the ownership guard run before binding: every directory
// must be owned by the current uid (else ENOTOWNED, so a directory another user
// pre-created under the shared temp root is never used), and one whose
// permission bits are not exactly 0700 is tightened to 0700. There is no check
// for a user namespace without a uid mapping.
func verifyOwnedDirs(dirs ...string) error {
	uid := currentUID()
	for _, d := range dirs {
		fi, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// A symlink planted in the shared temp root would redirect the
			// socket (and the 0700 chmod below) to a directory of the
			// planter's choosing.
			return fmt.Errorf("refusing to use %s: runtime directory is a symlink", d)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
			return &errutil.NotOwnedError{Path: d, UID: int(st.Uid)}
		}
		if fi.Mode().Perm() != 0o700 {
			if err := os.Chmod(d, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}
