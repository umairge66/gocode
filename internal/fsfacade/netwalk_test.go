package fsfacade

import (
	"errors"
	"io/fs"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// probeFS wraps MemFS: it records every probed path (as a slash path, since
// the walker hands it OS-separated names), can fail chosen paths
// with an errno, can block chosen paths until released (a stalled mount), and
// can vouch for an unreadable directory through OpenDirNoFollow.
type probeFS struct {
	*MemFS
	mu       sync.Mutex
	probed   []string
	fail     map[string]error
	block    map[string]chan struct{}
	dirsOpen map[string]bool
}

func newProbeFS() *probeFS {
	return &probeFS{MemFS: NewMemFS(), fail: map[string]error{}, block: map[string]chan struct{}{}, dirsOpen: map[string]bool{}}
}

func (p *probeFS) hit(name string) error {
	name = toSlash(name)
	p.mu.Lock()
	p.probed = append(p.probed, name)
	ch := p.block[name]
	err := p.fail[name]
	p.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return err
}

func (p *probeFS) Lstat(name string) (fs.FileInfo, error) {
	if err := p.hit(name); err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	return p.MemFS.Lstat(name)
}

func (p *probeFS) Readlink(name string) (string, error) {
	if err := p.hit(name); err != nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: err}
	}
	return p.MemFS.Readlink(name)
}

func (p *probeFS) OpenDirNoFollow(name string) error {
	if p.dirsOpen[toSlash(name)] {
		return nil
	}
	return &fs.PathError{Op: "opendir", Path: name, Err: syscall.EACCES}
}

func (p *probeFS) touched(prefix string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.probed {
		if s == prefix || strings.HasPrefix(s, prefix+"/") {
			return true
		}
	}
	return false
}

// A link onto the /net automount map is reported, never probed.
func TestWalkStopsAtAutomountLinkTarget(t *testing.T) {
	m := newProbeFS()
	m.MkdirAll("/proj")
	m.Symlink("/net/fileserver/export", "/proj/share")
	w := Walk(m, "/proj/share/data.csv", WalkOptions{UnverifiedAncestry: true})
	if w.Verdict != WalkReached || w.Reached != "/net/fileserver/export/data.csv" {
		t.Fatalf("got %+v", w)
	}
	if m.touched("/net") {
		t.Fatalf("walker probed the automount map: %v", m.probed)
	}
}

// A relative link composing onto /net is surfaced raw.
func TestWalkSurfacesRelativeAutomountRaw(t *testing.T) {
	m := newProbeFS()
	m.MkdirAll("/a")
	m.Symlink("../net/host/x", "/a/l")
	w := Walk(m, "/a/l/f", WalkOptions{SurfaceNetworkRaw: true, UnverifiedAncestry: true})
	if w.Verdict != WalkReached || w.Reached != "/net/host/x/f" {
		t.Fatalf("got %+v", w)
	}
	if m.touched("/net") {
		t.Fatalf("probed /net: %v", m.probed)
	}
}

// A kernel-redirected macOS path (/.vol etc.) is unverified, unprobed.
func TestWalkKernelRedirectedIsUnverified(t *testing.T) {
	m := newProbeFS()
	w := Walk(m, "/.vol/1/2", WalkOptions{UnverifiedAncestry: true})
	if w.Verdict != WalkUnverified {
		t.Fatalf("got %+v", w)
	}
}

// A plain local path resolves clean with a resolved outcome.
func TestWalkLocalClean(t *testing.T) {
	m := newProbeFS()
	m.WriteFile("/real/f.txt", []byte("x"))
	m.Symlink("/real", "/alias")
	w := Walk(m, "/alias/f.txt", WalkOptions{UnverifiedAncestry: true})
	if w.Verdict != WalkClean || w.Outcome.Kind != OutcomeResolved || w.Outcome.Path != "/real/f.txt" {
		t.Fatalf("got %+v", w)
	}
	// An absent leaf reports where the walk stopped.
	w = Walk(m, "/real/new/g.txt", WalkOptions{UnverifiedAncestry: true})
	if w.Verdict != WalkClean || w.Outcome.Kind != OutcomeAbsent || w.Outcome.At != "/real" ||
		strings.Join(w.Outcome.Remaining, "/") != "new/g.txt" {
		t.Fatalf("absent: %+v", w)
	}
}

// An unexaminable component is unverified; an EACCES ancestor of the launch
// directory may be crossed.
func TestWalkUnreadableAncestry(t *testing.T) {
	m := newProbeFS()
	m.WriteFile("/home/u/proj/f", nil)
	m.fail["/home"] = syscall.EACCES
	if w := Walk(m, "/home/u/proj/f", WalkOptions{UnverifiedAncestry: true}); w.Verdict != WalkUnverified {
		t.Fatalf("no ancestry: %+v", w)
	}
	w := Walk(m, "/home/u/proj/f", WalkOptions{UnverifiedAncestry: true, LaunchAncestry: "/home/u/proj"})
	if w.Verdict != WalkClean || w.Outcome.Path != "/home/u/proj/f" {
		t.Fatalf("launch ancestry: %+v", w)
	}
	// ... but not followed by a dot segment.
	if w := Walk(m, "/home/../home/u/proj/f", WalkOptions{UnverifiedAncestry: true, LaunchAncestry: "/home/u/proj"}); w.Verdict == WalkReached {
		t.Fatalf("dot after crossing: %+v", w)
	}
	// A directory the opendir probe vouches for is crossed.
	m.dirsOpen["/home"] = true
	if w := Walk(m, "/home/u/proj/f", WalkOptions{UnverifiedAncestry: true}); w.Verdict != WalkClean {
		t.Fatalf("opendir vouched: %+v", w)
	}
}

// A probe that hangs on a stalled mount times out; later probes at or below
// the stalled path fail fast; the entry clears once the call returns.
func TestTimeoutFSStalledMount(t *testing.T) {
	m := newProbeFS()
	m.MkdirAll("/mnt/dead/sub")
	release := make(chan struct{})
	m.block["/mnt/dead"] = release
	stalls := &StallSet{}
	tfs := TimeoutFS{FS: m, Timeout: 30 * time.Millisecond, Stalls: stalls}

	start := time.Now()
	w := Walk(tfs, "/mnt/dead/sub/file", WalkOptions{UnverifiedAncestry: true})
	if w.Verdict != WalkUnverified {
		t.Fatalf("got %+v", w)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("walk took %v", d)
	}
	if !stalls.Stalled("/mnt/dead/sub") {
		t.Fatal("stall not recorded")
	}
	start = time.Now()
	if _, err := tfs.Lstat("/mnt/dead/sub"); !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("second probe: %v", err)
	}
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Fatalf("second probe did not fail fast: %v", d)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for stalls.Stalled("/mnt/dead") {
		if time.Now().After(deadline) {
			t.Fatal("stall never cleared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w := Walk(tfs, "/mnt/dead/sub", WalkOptions{UnverifiedAncestry: true}); w.Verdict != WalkClean {
		t.Fatalf("after release: %+v", w)
	}
}

// A passed deadline ends the walk unverified.
func TestWalkDeadline(t *testing.T) {
	m := newProbeFS()
	m.MkdirAll("/a/b")
	w := Walk(m, "/a/b", WalkOptions{UnverifiedAncestry: true, Deadline: time.Now().Add(-time.Second)})
	if w.Verdict != WalkUnverified {
		t.Fatalf("got %+v", w)
	}
}

// A symlink cycle ends the walk (clean); the hop limit
// ends it unverified.
func TestWalkCycleAndHopLimit(t *testing.T) {
	m := newProbeFS()
	m.Symlink("/b", "/a")
	m.Symlink("/a", "/b")
	if w := Walk(m, "/a/x", WalkOptions{UnverifiedAncestry: true}); w.Verdict != WalkClean || w.Outcome.Kind != OutcomeNone {
		t.Fatalf("cycle: %+v", w)
	}
	m2 := newProbeFS()
	m2.MkdirAll("/d")
	for i := 0; i < 70; i++ {
		m2.Symlink("/l"+itoa(i+1), "/l"+itoa(i))
	}
	m2.Symlink("/d", "/l70")
	if w := Walk(m2, "/l0", WalkOptions{UnverifiedAncestry: true}); w.Verdict != WalkUnverified {
		t.Fatalf("hop limit: %+v", w)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestClassifyErrno(t *testing.T) {
	cases := map[error]Errno{
		nil:                  ErrnoNone,
		fs.ErrNotExist:       ErrnoNoEnt,
		syscall.ENOTDIR:      ErrnoNotDir,
		syscall.EACCES:       ErrnoAccess,
		syscall.EPERM:        ErrnoAccess,
		syscall.ENAMETOOLONG: ErrnoNameTooLong,
		syscall.ELOOP:        ErrnoLoop,
		syscall.EIO:          ErrnoOther,
		ErrProbeTimeout:      ErrnoOther,
	}
	for err, want := range cases {
		if got := ClassifyErrno(err); got != want {
			t.Errorf("%v: got %v want %v", err, got, want)
		}
	}
}
