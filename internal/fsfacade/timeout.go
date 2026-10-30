package fsfacade

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DirNoFollowOpener is an optional FS capability: open name as a directory
// without following a final symlink, then close it. The walker uses it to tell
// a searchable-but-unreadable directory (which may be crossed) from a component
// that truly cannot be examined.
type DirNoFollowOpener interface {
	OpenDirNoFollow(name string) error
}

// Realpather is an optional FS capability: the platform's canonical realpath.
// When an FS does not implement it, Realpath (the POSIX walker in this package)
// is used instead.
type Realpather interface {
	RealpathOS(name string) (string, error)
}

var (
	_ DirNoFollowOpener = OSFS{}
	_ Realpather        = OSFS{}
)

// OpenDirNoFollow implements DirNoFollowOpener (platform code in
// opendir_*.go).
func (OSFS) OpenDirNoFollow(name string) error { return openDirNoFollow(name) }

// RealpathOS implements Realpather over filepath.EvalSymlinks.
func (OSFS) RealpathOS(name string) (string, error) {
	r, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(r); err == nil {
		r = abs
	}
	return filepath.Clean(r), nil
}

// ErrProbeTimeout is wrapped by the error a TimeoutFS probe returns when the
// underlying filesystem call did not answer within the timeout (a stalled
// network or automount mount), or when an earlier probe at or above the same
// path is still hung. The walker treats it like any component that could not
// be examined: under the "unverified ancestry" policy the walk stops with the
// unverified sentinel, so the permission layer refuses the path instead of
// hanging.
var ErrProbeTimeout = errors.New("fsfacade: filesystem probe timed out (stalled mount?)")

// DefaultProbeTimeout bounds a single filesystem probe (lstat, readlink,
// opendir, realpath) made by the permission walker.
const DefaultProbeTimeout = 5 * time.Second

// DefaultWalkBudget bounds a whole permission walk (every probe together).
const DefaultWalkBudget = 15 * time.Second

// StallSet records the paths whose probes timed out and are still blocked in
// the kernel. While a probe of P is hung, any new probe of P or of a path
// beneath P fails immediately with ErrProbeTimeout instead of piling another
// blocked goroutine onto the same dead mount. An entry clears by itself when
// the hung call finally returns. It is safe for concurrent use.
type StallSet struct {
	mu      sync.Mutex
	pending map[string]int
}

// defaultStalls is shared by every TimeoutFS that does not name its own set:
// a stalled mount is a property of the machine, not of one session.
var defaultStalls = &StallSet{}

func (s *StallSet) add(p string) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]int{}
	}
	s.pending[p]++
	s.mu.Unlock()
}

func (s *StallSet) done(p string) {
	s.mu.Lock()
	if s.pending[p] <= 1 {
		delete(s.pending, p)
	} else {
		s.pending[p]--
	}
	s.mu.Unlock()
}

// Stalled reports whether p is, or lies beneath, a path with a hung probe.
func (s *StallSet) Stalled(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return false
	}
	p = toSlash(p)
	for k := range s.pending {
		k = toSlash(k)
		if p == k || strings.HasPrefix(p, strings.TrimSuffix(k, "/")+"/") {
			return true
		}
	}
	return false
}

// TimeoutFS wraps an FS so every probe runs under a deadline. A probe that
// does not return in time yields an error wrapping ErrProbeTimeout; the
// blocked call is left to finish in its own goroutine (it cannot be cancelled
// in the kernel) and is recorded in the StallSet meanwhile.
type TimeoutFS struct {
	// FS is the wrapped filesystem.
	FS FS
	// Timeout bounds each probe; zero means DefaultProbeTimeout.
	Timeout time.Duration
	// Stalls records hung probes; nil uses the package-wide set.
	Stalls *StallSet
}

var (
	_ FS                = TimeoutFS{}
	_ Cwder             = TimeoutFS{}
	_ DirNoFollowOpener = TimeoutFS{}
	_ Realpather        = TimeoutFS{}
)

func (t TimeoutFS) stalls() *StallSet {
	if t.Stalls != nil {
		return t.Stalls
	}
	return defaultStalls
}

func (t TimeoutFS) timeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return DefaultProbeTimeout
}

type probeResult[T any] struct {
	v   T
	err error
}

// probe runs f under t's deadline.
func probe[T any](t TimeoutFS, op, name string, f func() (T, error)) (T, error) {
	var zero T
	st := t.stalls()
	if st.Stalled(name) {
		return zero, &fs.PathError{Op: op, Path: name, Err: ErrProbeTimeout}
	}
	ch := make(chan probeResult[T], 1)
	go func() {
		v, err := f()
		ch <- probeResult[T]{v, err}
	}()
	timer := time.NewTimer(t.timeout())
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-timer.C:
		st.add(name)
		go func() {
			<-ch
			st.done(name)
		}()
		return zero, &fs.PathError{Op: op, Path: name, Err: ErrProbeTimeout}
	}
}

// Stat implements FS.
func (t TimeoutFS) Stat(name string) (fs.FileInfo, error) {
	return probe(t, "stat", name, func() (fs.FileInfo, error) { return t.FS.Stat(name) })
}

// Lstat implements FS.
func (t TimeoutFS) Lstat(name string) (fs.FileInfo, error) {
	return probe(t, "lstat", name, func() (fs.FileInfo, error) { return t.FS.Lstat(name) })
}

// Readlink implements FS.
func (t TimeoutFS) Readlink(name string) (string, error) {
	return probe(t, "readlink", name, func() (string, error) { return t.FS.Readlink(name) })
}

// ReadDir implements FS.
func (t TimeoutFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return probe(t, "readdir", name, func() ([]fs.DirEntry, error) { return t.FS.ReadDir(name) })
}

// Open implements FS.
func (t TimeoutFS) Open(name string) (fs.File, error) {
	return probe(t, "open", name, func() (fs.File, error) { return t.FS.Open(name) })
}

// ReadFile implements FS.
func (t TimeoutFS) ReadFile(name string) ([]byte, error) {
	return probe(t, "read", name, func() ([]byte, error) { return t.FS.ReadFile(name) })
}

// Getwd implements Cwder, delegating to the wrapped FS (or the process).
func (t TimeoutFS) Getwd() (string, error) {
	if c, ok := t.FS.(Cwder); ok {
		return c.Getwd()
	}
	return os.Getwd()
}

// OpenDirNoFollow implements DirNoFollowOpener; an FS without the capability
// reports an error (the component then counts as not examinable).
func (t TimeoutFS) OpenDirNoFollow(name string) error {
	o, ok := t.FS.(DirNoFollowOpener)
	if !ok {
		return &fs.PathError{Op: "opendir", Path: name, Err: errors.ErrUnsupported}
	}
	_, err := probe(t, "opendir", name, func() (struct{}, error) { return struct{}{}, o.OpenDirNoFollow(name) })
	return err
}

// RealpathOS implements Realpather: the wrapped FS's own realpath when it has
// one, else this package's POSIX walker over the wrapped FS — under the
// deadline either way.
func (t TimeoutFS) RealpathOS(name string) (string, error) {
	return probe(t, "realpath", name, func() (string, error) {
		if r, ok := t.FS.(Realpather); ok {
			return r.RealpathOS(name)
		}
		return Realpath(t.FS, name)
	})
}
