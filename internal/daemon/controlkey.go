package daemon

import (
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Control key: the second authorisation layer for the authed control ops
// (dispatch / reply / attach / permission-response). The first layer is the
// same-uid peer check (peercred.go); the key proves the client can read the
// user's own <config>/daemon/control.key.

const maxKeyFileBytes = 4096

// EnsureControlKey returns the daemon's control key, creating it when missing
// or invalid.
func EnsureControlKey(configDir string) (string, error) {
	p := ControlKeyPath(configDir)
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode().IsRegular() && fi.Size() <= maxKeyFileBytes {
			if b, err := os.ReadFile(p); err == nil {
				if k := strings.TrimSpace(string(b)); k != "" {
					return k, nil
				}
			}
		} else {
			_ = os.RemoveAll(p)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	k, err := randomHex(16)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(daemonConfigDir(configDir), 0o700); err != nil {
		return "", err
	}
	if err := writeFileAtomic(p, []byte(k), 0o600); err != nil {
		return "", err
	}
	return k, nil
}

// ReadControlKey returns the control key for a client, or "" when unreadable.
func ReadControlKey(configDir string) string {
	p := ControlKeyPath(configDir)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxKeyFileBytes {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// KeysMatch is a constant-time token comparison; an empty presented or
// expected token never matches.
func KeysMatch(presented, expected string) bool {
	if presented == "" || expected == "" || len(presented) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// -----------------------------------------------------------------------------
// Daemon log: "[<ISO>] [<component>] <msg>\n" appended to <config>/daemon.log,
// rotated to daemon.log.1 past 10 MiB.

// maxDaemonLogBytes is the rotation threshold.
const maxDaemonLogBytes = 10 << 20

// DaemonLog is a concurrency-safe appender for daemon.log.
type DaemonLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	// Echo, when non-nil, also receives every line (e.g. a TTY stdout).
	Echo func(string)
}

// OpenDaemonLog opens (creating) the log at path.
func OpenDaemonLog(path string) (*DaemonLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &DaemonLog{path: path}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxDaemonLogBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err == nil {
		l.size = fi.Size()
	}
	l.f = f
	return l, nil
}

// Write appends one line for component.
func (l *DaemonLog) Write(component, msg string) {
	if l == nil {
		return
	}
	line := fmt.Sprintf("[%s] [%s] %s\n", isoTime(time.Now()), component, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Echo != nil {
		l.Echo(line)
	}
	if l.f == nil {
		return
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
	if l.size > maxDaemonLogBytes {
		_ = l.f.Close()
		_ = os.Rename(l.path, l.path+".1")
		if f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			l.f = f
		} else {
			l.f = nil
		}
		l.size = 0
	}
}

// Close closes the log file.
func (l *DaemonLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
