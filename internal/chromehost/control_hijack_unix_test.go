//go:build !windows

package chromehost

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A runtime dir under the shared temp root that another local user
// pre-created must not be used: they could unlink our socket and bind their
// own, intercepting every browser command (and its page content results).
func TestListenControlRefusesForeignOwnedDir(t *testing.T) {
	old := controlUID
	controlUID = func() int { return os.Getuid() + 1 } // dirs look foreign
	defer func() { controlUID = old }()
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(t.TempDir(), "inst", ChromeHostSocketName)}
	if l, err := listenControl(ep); err == nil {
		l.Close()
		t.Fatal("listenControl bound a socket in a directory owned by another user")
	}
}

func TestListenControlRefusesSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(real, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "inst")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(link, ChromeHostSocketName)}
	if l, err := listenControl(ep); err == nil {
		l.Close()
		t.Fatal("listenControl followed a symlinked runtime directory")
	}
}

func TestListenControlSocketModes(t *testing.T) {
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(t.TempDir(), "inst", ChromeHostSocketName)}
	if err := os.MkdirAll(filepath.Dir(ep.Address), 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Dir(ep.Address), 0o777)
	l, err := listenControl(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, _ := os.Stat(filepath.Dir(ep.Address))
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("runtime dir mode %o, want 0700", fi.Mode().Perm())
	}
	si, _ := os.Stat(ep.Address)
	if si.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket mode %o, want 0600", si.Mode().Perm())
	}
}

// The session must not send browser commands to (or trust replies from) a
// socket bound by another user.
func TestDialControlRefusesForeignEndpoint(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	old := controlUID
	controlUID = func() int { return os.Getuid() + 1 }
	defer func() { controlUID = old }()
	if c, err := dialControl(context.Background(), ControlEndpoint{Network: "unix", Address: sock}); err == nil {
		c.Close()
		t.Fatal("dialControl accepted an endpoint owned by another uid")
	}
}

func TestDialControlRefusesSymlinkSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "real.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if c, err := dialControl(context.Background(), ControlEndpoint{Network: "unix", Address: link}); err == nil {
		c.Close()
		t.Fatal("dialControl followed a symlinked socket path")
	}
}
