package pathguard

import (
	"io/fs"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// stallFS is a MemFS whose probes of chosen paths fail with an errno or block
// until released (a stalled network mount), recording what was probed.
type stallFS struct {
	*fsfacade.MemFS
	mu     sync.Mutex
	probed []string
	fail   map[string]error
	block  map[string]chan struct{}
}

func newStallFS() *stallFS {
	return &stallFS{MemFS: fsfacade.NewMemFS(), fail: map[string]error{}, block: map[string]chan struct{}{}}
}

func (s *stallFS) hit(name string) error {
	s.mu.Lock()
	s.probed = append(s.probed, name)
	ch, err := s.block[name], s.fail[name]
	s.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return err
}

func (s *stallFS) Lstat(name string) (fs.FileInfo, error) {
	if err := s.hit(name); err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	return s.MemFS.Lstat(name)
}

func (s *stallFS) Readlink(name string) (string, error) {
	if err := s.hit(name); err != nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: err}
	}
	return s.MemFS.Readlink(name)
}

func (s *stallFS) touched(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.probed {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func netSession(fsys fsfacade.FS) *SessionFiles {
	return &SessionFiles{OriginalCwd: "/proj", Cwd: "/proj", Platform: "linux", FS: fsys, Stalls: &fsfacade.StallSet{}}
}

func TestResolvePathSurfacesAutomountLink(t *testing.T) {
	m := newStallFS()
	m.MkdirAll("/proj")
	m.Symlink("/net/fileserver/export", "/proj/share")
	s := netSession(m)
	res := s.ResolvePath("/proj/share/data.csv")
	if res.Unresolved {
		t.Fatalf("unresolved: %+v", res)
	}
	want := []string{"/proj/share/data.csv", "/net/fileserver/export/data.csv"}
	if strings.Join(res.Spellings, "|") != strings.Join(want, "|") || res.Landing != want[1] {
		t.Fatalf("got %+v", res)
	}
	if m.touched("/net") {
		t.Fatalf("probed the automount map: %v", m.probed)
	}
}

func TestResolvePathOrdinarySpellingsUnchanged(t *testing.T) {
	m := newStallFS()
	m.WriteFile("/real/dir/f.txt", nil)
	m.Symlink("/real/dir", "/proj/link")
	s := netSession(m)
	if got := s.ResolvePath("/proj/link/f.txt").Spellings; strings.Join(got, "|") != "/proj/link/f.txt|/real/dir/f.txt" {
		t.Fatalf("link: %v", got)
	}
	if got := s.ResolvePath("/proj/link/new.txt").Spellings; strings.Join(got, "|") != "/proj/link/new.txt|/real/dir/new.txt" {
		t.Fatalf("absent leaf: %v", got)
	}
	if got := s.ResolvePath("/real/dir/f.txt").Spellings; strings.Join(got, "|") != "/real/dir/f.txt" {
		t.Fatalf("plain: %v", got)
	}
}

// A stalled mount must not hang the permission check: the probe times out
// and the path is unresolved ("cut-short" when the walk budget ran out).
func TestResolvePathStalledMountTimesOut(t *testing.T) {
	m := newStallFS()
	m.MkdirAll("/mnt/nfs/dir")
	release := make(chan struct{})
	defer close(release)
	m.block["/mnt/nfs"] = release
	s := netSession(m)
	s.ProbeTimeout = 40 * time.Millisecond
	start := time.Now()
	res := s.ResolvePath("/mnt/nfs/dir/file")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("permission walk hung for %v", d)
	}
	if !res.Unresolved {
		t.Fatalf("stalled mount resolved: %+v", res)
	}
	// The next check fails fast instead of piling onto the dead mount.
	start = time.Now()
	if res := s.ResolvePath("/mnt/nfs/dir/other"); !res.Unresolved || time.Since(start) > 30*time.Millisecond {
		t.Fatalf("second check: %+v in %v", res, time.Since(start))
	}
	// A whole-walk budget that is already spent is "cut-short".
	s2 := netSession(newStallFS())
	s2.WalkBudget = time.Nanosecond
	if res := s2.ResolvePath("/a/b"); !res.Unresolved || res.Reason != "cut-short" {
		t.Fatalf("budget reason: %+v", res)
	}
}

func TestResolvePathUnreadableAncestry(t *testing.T) {
	m := newStallFS()
	m.WriteFile("/home/u/proj/f", nil)
	m.WriteFile("/home/u/other/link-target", nil)
	m.Symlink("/home/u/other/link-target", "/home/u/proj/l")
	m.fail["/home/u/other"] = syscall.EIO
	s := &SessionFiles{OriginalCwd: "/home/u/proj", Cwd: "/home/u/proj", FS: m, Stalls: &fsfacade.StallSet{}}
	if res := s.ResolvePath("/home/u/proj/l"); !res.Unresolved || res.Reason != "past-a-link" {
		t.Fatalf("EIO past a link: %+v", res)
	}
	// A plain EACCES with no link followed stands as requested.
	m2 := newStallFS()
	m2.fail["/srv"] = syscall.EACCES
	s2 := &SessionFiles{OriginalCwd: "/home/u", Cwd: "/home/u", FS: m2, Stalls: &fsfacade.StallSet{}}
	if res := s2.ResolvePath("/srv/x"); res.Unresolved || strings.Join(res.Spellings, "|") != "/srv/x" {
		t.Fatalf("EACCES no link: %+v", res)
	}
}

func TestLaunchAncestry(t *testing.T) {
	cases := []struct{ launch, cwd, want string }{
		{"/home/u/proj", "/home/u/proj/sub", "/home/u/proj"},
		{"/home/u/a", "/home/u/b", "/home/u"},
		{"/a", "/b", ""},
		{"", "/b", ""},
	}
	for _, c := range cases {
		if got := LaunchAncestry(c.launch, c.cwd); got != c.want {
			t.Errorf("LaunchAncestry(%q,%q)=%q want %q", c.launch, c.cwd, got, c.want)
		}
	}
	s := &SessionFiles{OriginalCwd: "/home/u/proj", Cwd: "/home/u/proj", NoLaunchAncestry: true}
	if s.launchAncestry() != "" {
		t.Error("background session kept a launch ancestry")
	}
}

func TestCheckReadNetworkSafety(t *testing.T) {
	s := &SessionFiles{Platform: "linux"}
	cases := []struct {
		req       string
		spellings []string
		reason    string
		msg       string
	}{
		{"/net/host/x", []string{"/net/host/x"}, ReasonAutomountHosts,
			"gocode requested permissions to read from /net/host/x, which is under the /net automount map and could trigger a DNS lookup and NFS mount to a remote host."},
		{"/net", []string{"/net"}, ReasonAutomountHosts, ""},
		{"/.vol/1/2", nil, ReasonKernelResolved,
			"gocode requested permissions to read from /.vol/1/2, which is under /.vol, /.file, /.nofollow or /.resolve (paths the macOS kernel redirects) and could reach a network mount, triggering a DNS lookup and mount to a remote host."},
		{"/proj/share/a", []string{"/proj/share/a", "/net/srv/export/a"}, ReasonAutomountHosts,
			"gocode requested permissions to read from /proj/share/a, which is under the /net automount map and could trigger a DNS lookup and NFS mount to a remote host."},
		{"/proj/s", []string{"/proj/s", `\\nas\team\s`}, ReasonUNCPath,
			"gocode requested permissions to read from /proj/s, which appears to be a UNC path that could access network resources."},
		{"/Network/Servers/h/x", []string{"/Network/Servers/h/x"}, ReasonAutomountHosts, ""},
	}
	for _, c := range cases {
		got := s.CheckReadNetworkSafety(c.req, c.spellings, "")
		if got.Safe || got.Reason != c.reason || !got.SafetyCheck || (c.msg != "" && got.Message != c.msg) {
			t.Errorf("%s: got %+v", c.req, got)
		}
	}
	if got := s.CheckReadNetworkSafety("/proj/a.go", []string{"/proj/a.go"}, ""); !got.Safe {
		t.Errorf("local: %+v", got)
	}
	if got := s.CheckReadNetworkSafety("/proj", []string{"/proj"}, "//srv/share/**"); got.Reason != ReasonUNCGlob ||
		got.Message != "gocode requested permissions to glob //srv/share/**, which appears to be a UNC pattern that could access network resources." {
		t.Errorf("glob: %+v", got)
	}
	// A suspicious Windows spelling asks as type "other".
	if got := s.CheckReadNetworkSafety("/proj/a...b/../x", []string{"/proj/.../x"}, ""); got.Safe || got.SafetyCheck || got.Reason != ReasonSuspiciousWindowsRead {
		t.Errorf("suspicious: %+v", got)
	}
}

// The trusted network directories exempt their own subtree only.
func TestCheckReadNetworkSafetyTrustedDirectories(t *testing.T) {
	s := &SessionFiles{Platform: "linux", TrustedNetworkDirectories: []string{"/net/fileserver/export", `\\nas\team`}}
	for _, sp := range []string{"/net/fileserver/export/a.csv", `\\nas\team\docs\b.txt`, `\\NAS\Team\c`} {
		if got := s.CheckReadNetworkSafety("/proj/link", []string{"/proj/link", sp}, ""); !got.Safe {
			t.Errorf("trusted %s: %+v", sp, got)
		}
	}
	for _, sp := range []string{"/net/otherhost/x", `\\nas\private\x`, "/net/fileserver/export/../../h/x"} {
		if got := s.CheckReadNetworkSafety("/proj/link", []string{"/proj/link", sp}, ""); got.Safe {
			t.Errorf("untrusted %s passed", sp)
		}
	}
}

func TestUnresolvedMessages(t *testing.T) {
	if got := MsgUnresolvedPath("read from", "/a/b"); got != "Refusing to read /a/b: where it leads on disk could not be determined (a link on the way could not be examined, or the links do not resolve)." {
		t.Errorf("read: %q", got)
	}
	if got := MsgUnresolvedPath("write to", "/a/b"); !strings.HasPrefix(got, "Refusing to write /a/b: ") {
		t.Errorf("write: %q", got)
	}
}
