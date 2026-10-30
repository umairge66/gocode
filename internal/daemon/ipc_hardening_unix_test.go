//go:build !windows

package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A symlink planted where a runtime directory is expected must be refused, and
// the directory it points at must not be chmod-ed.
func TestVerifyOwnedDirsRefusesSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "victim")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "cc-daemon-x")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := verifyOwnedDirs(link); err == nil {
		t.Fatal("verifyOwnedDirs accepted a symlinked runtime dir")
	}
	fi, _ := os.Stat(target)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("symlink target mode changed to %o", fi.Mode().Perm())
	}
}

// A deep socket (…/cc-daemon-<uid>/<disc>/pty/<id>.sock) must vet the whole
// chain up to the per-user root, not just its two nearest directories.
func TestListenEndpointVetsPerUserRoot(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "gcipc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	root := filepath.Join(base, "cc-daemon-t")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "d", "pty", "a.sock")
	l, err := listenEndpoint(ControlEndpoint{Network: "unix", Address: sock})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l.Close()
	fi, _ := os.Stat(root)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("per-user root left at mode %o; want it vetted and tightened to 0700", fi.Mode().Perm())
	}
}

// The pty host must run the peer check before sending anything (hello, the
// scrollback ring) to an accepted connection.
func TestPTYHostAcceptRunsPeerCheck(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gcpty")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h := &ptyHost{sock: sock, ring: NewByteRing(1024), clients: map[*hostClient]struct{}{},
		checkPeer: func(net.Conn) error { return errors.New("foreign uid") }}
	go h.acceptLoop(l)
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if n != 0 || err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("rejected peer got n=%d err=%v; want immediate close with no data", n, err)
	}
}

// dialEndpoint must refuse an endpoint served by a different uid.
func TestDialEndpointRefusesForeignOwner(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gcdial")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	real := os.Getuid()
	currentUID = func() int { return real + 1 }
	defer func() { currentUID = os.Getuid }()
	c, err := dialEndpoint(context.Background(), ControlEndpoint{Network: "unix", Address: sock})
	if err == nil {
		c.Close()
		t.Fatal("dialEndpoint connected to a socket owned by another uid")
	}
}
