package pathguard

// Network-share and automount handling for the file tools' permission checks:
//
//   - ResolveForPermission is the "permission" spelling walk. It walks the
//     path with the network-aware walker (internal/fsfacade Walk) and never
//     steps onto a network share or automounter: a symlink whose target is a
//     UNC path, a /net or /Network/Servers automount path, the /Network browse
//     root or a bare /net yields that network spelling WITHOUT touching it, so
//     the checks below see it. A component that cannot be examined (including
//     a probe that timed out on a stalled mount) leaves the walk "unresolved".
//   - CheckReadNetworkSafety is the read check's defense-in-depth network
//     screen.
//   - MsgUnresolvedPath is the refusal for an unresolved walk.
//   - NetworkVerdict / CrossesOntoNetwork / UnverifiableOrNetwork classify a
//     walk's result.
//
// Detection is by spelling, not by filesystem type: there is
// no statfs / mount-table classification on this path. A file on an NFS or SMB
// mount reached through an ordinary local mount point (e.g. /home on NFS, a
// CIFS share mounted at /mnt/share) is NOT treated specially; only paths that
// name the automounter or a share (/net/<host>, /Network/..., \\host\share,
// /.vol etc.) — directly or through a symlink — are.

import (
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/pathclass"
)

// PathResolution is the result of the permission spelling walk.
type PathResolution struct {
	// Requested is the path as asked (made absolute).
	Requested string
	// Spellings are every spelling the checks must judge: the requested path,
	// the targets of a leaf symlink, the landing, the canonical landing — and,
	// when a link leads onto a network share or automounter, that network
	// spelling.
	Spellings []string
	// Landing is where the path resolves (resolved walks only).
	Landing string
	// Unresolved reports that where the path leads could not be determined.
	Unresolved bool
	// StoppedAt is the last path probed (unresolved walks).
	StoppedAt string
	// Reason is "past-a-link", "unexamined" or "cut-short" (unresolved only).
	Reason string
	// LeafIsSymlink reports that the final component was a symlink.
	LeafIsSymlink bool
}

// orderedSet is an insertion-ordered set with delete-then-add
// moving an entry to the end.
type orderedSet struct{ items []string }

func (o *orderedSet) has(s string) bool {
	for _, v := range o.items {
		if v == s {
			return true
		}
	}
	return false
}

func (o *orderedSet) add(s string) {
	if !o.has(s) {
		o.items = append(o.items, s)
	}
}

func (o *orderedSet) remove(s string) {
	for i, v := range o.items {
		if v == s {
			o.items = append(o.items[:i], o.items[i+1:]...)
			return
		}
	}
}

func (o *orderedSet) list() []string { return append([]string(nil), o.items...) }

// LaunchAncestry is the longest shared ancestry of the launch directory
// and the current working directory, or "" when they share no component (or
// there is no launch directory, as in a background session).
func LaunchAncestry(launch, cwd string) string {
	if launch == "" || cwd == "" {
		return ""
	}
	r, i := filepath.ToSlash(filepath.Clean(launch)), filepath.ToSlash(filepath.Clean(cwd))
	root := slashRoot(r)
	if root == "" || root != slashRoot(i) {
		return ""
	}
	o := splitNonEmpty(r[len(root):])
	a := splitNonEmpty(i[len(root):])
	u := 0
	for u < len(o) && u < len(a) && o[u] == a[u] {
		u++
	}
	if u == 0 {
		return ""
	}
	return filepath.FromSlash(path.Join(append([]string{root}, o[:u]...)...))
}

func slashRoot(p string) string {
	if len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && p[2] == '/' {
		return p[:3]
	}
	if strings.HasPrefix(p, "/") {
		return "/"
	}
	return ""
}

// ResolveForPermission runs the permission spelling walk over fsys.
// requested should be absolute. Wrap fsys in fsfacade.TimeoutFS and pass a
// deadline so a stalled mount cannot hang the caller: a probe that times out,
// or a walk that outlives the deadline, leaves the path unresolved
// ({unresolved:true, reason:"cut-short"}).
func ResolveForPermission(fsys fsfacade.FS, requested, launchAncestry string, deadline time.Time) PathResolution {
	r := requested
	set := &orderedSet{}
	set.add(r)
	resolved := func(landing string, leaf bool) PathResolution {
		return PathResolution{Requested: r, Spellings: set.list(), Landing: landing, LeafIsSymlink: leaf}
	}
	if (pathclass.IsUNCOrDevicePath(r) && !pathclass.IsWSLPath(r)) || pathclass.IsAutomountPath(r) || pathclass.IsGOPath(r) {
		return resolved(r, false)
	}
	var collapsed string
	leaf := false
	w := fsfacade.Walk(fsys, r, fsfacade.WalkOptions{
		UnverifiedAncestry: true,
		LaunchAncestry:     launchAncestry,
		OnCollapsedLanding: func(s string) { collapsed = s },
		OnHop: func(composed string, isLeaf bool, _ string) {
			if isLeaf {
				set.add(filepath.FromSlash(composed))
				leaf = true
			}
		},
		Deadline: deadline,
	})
	timedOut := !deadline.IsZero() && time.Now().After(deadline)
	// No link was followed and the last probe failed for permissions (or a
	// too-long name): the requested spelling itself stands.
	tolerated := !w.SawReadlink && (w.LastErrno == fsfacade.ErrnoNameTooLong || w.LastErrno == fsfacade.ErrnoAccess)
	unresolved := func() PathResolution {
		if tolerated {
			return resolved(r, false)
		}
		reason := "unexamined"
		if w.SawReadlink || w.OpendirLoop {
			reason = "past-a-link"
		}
		if timedOut {
			reason = "cut-short"
		}
		return PathResolution{Requested: r, Spellings: set.list(), Unresolved: true, StoppedAt: filepath.FromSlash(w.StoppedAt), Reason: reason, LeafIsSymlink: leaf}
	}
	switch w.Verdict {
	case fsfacade.WalkUnverified:
		if timedOut {
			tolerated = false
		}
		return unresolved()
	case fsfacade.WalkReached:
		if collapsed != "" {
			set.add(filepath.FromSlash(collapsed))
		}
		k := filepath.FromSlash(w.Reached)
		set.remove(k)
		set.add(k)
		return resolved(k, leaf)
	}
	out := w.Outcome
	if out.Kind == fsfacade.OutcomeNone {
		return unresolved()
	}
	if out.Kind == fsfacade.OutcomeAbsent && w.SawReadlink {
		for _, rem := range out.Remaining {
			if pathclass.HasDotSegmentPOSIX(rem) {
				return unresolved()
			}
		}
	}
	v, rest := out.Path, ""
	if out.Kind == fsfacade.OutcomeAbsent {
		v, rest = out.At, strings.Join(out.Remaining, "/")
	}
	j := v
	if rest != "" {
		j = path.Join(v, rest)
	}
	jOS := filepath.FromSlash(j)
	if w.SawReadlink || v != slashRoot(v) {
		set.add(jOS)
	}
	if real, err := fsfacade.RealpathVia(fsys, filepath.FromSlash(v)); err == nil {
		real = filepath.ToSlash(real)
		if real != v {
			t := real
			if rest != "" {
				t = path.Join(real, rest)
			}
			if tOS := filepath.FromSlash(t); tOS != jOS {
				set.remove(tOS)
				set.add(tOS)
			}
		}
	}
	return resolved(jOS, leaf)
}

// probeFS is the session's filesystem with every probe under a deadline.
func (s *SessionFiles) probeFS() fsfacade.FS {
	return fsfacade.TimeoutFS{FS: s.fsys(), Timeout: s.ProbeTimeout, Stalls: s.Stalls}
}

// walkDeadline is the whole-walk budget's deadline.
func (s *SessionFiles) walkDeadline() time.Time {
	b := s.WalkBudget
	if b <= 0 {
		b = fsfacade.DefaultWalkBudget
	}
	return time.Now().Add(b)
}

// launchAncestry is LaunchAncestry for this session.
func (s *SessionFiles) launchAncestry() string {
	if s.NoLaunchAncestry {
		return ""
	}
	return LaunchAncestry(s.OriginalCwd, s.cwd())
}

// ResolvePath runs the permission spelling walk for p (made absolute against
// the session's original cwd).
func (s *SessionFiles) ResolvePath(p string) PathResolution {
	abs := s.resolve(p)
	if abs == "" {
		return PathResolution{}
	}
	return ResolveForPermission(s.probeFS(), abs, s.launchAncestry(), s.walkDeadline())
}

// --- an unresolved walk is refused -------------------------------------------

// MsgUnresolvedPath is the unresolved-walk message; verb is "read from" or
// "write to".
func MsgUnresolvedPath(verb, p string) string {
	v := "read"
	if verb == "write to" {
		v = "write"
	}
	return "Refusing to " + v + " " + displayPath(p) +
		": where it leads on disk could not be determined (a link on the way could not be examined, or the links do not resolve)."
}

// --- the read check's network screen ----------------------------------------

// ReadSafety is CheckReadNetworkSafety's verdict.
type ReadSafety struct {
	// Safe is true when nothing was found.
	Safe bool
	// Message is the prompt text.
	Message string
	// Reason is the decisionReason text.
	Reason string
	// SafetyCheck is true for the network asks (type "safetyCheck",
	// classifierApprovable:false) and false for the suspicious-spelling ask
	// (type "other").
	SafetyCheck bool
}

// Verbatim reasons of the network screen.
const (
	ReasonAutomountHosts      = "Automount -hosts path detected (defense-in-depth check)"
	ReasonAutomountBrowse     = "Automount browse surface detected (defense-in-depth check)"
	ReasonKernelResolved      = "Kernel-resolved path prefix (/.vol etc.) detected (defense-in-depth check)"
	ReasonUNCPath             = "UNC path detected (defense-in-depth check)"
	ReasonUNCGlob             = "UNC glob pattern detected (defense-in-depth check)"
	ReasonAutomountHostsGlob  = "Automount -hosts glob pattern detected (defense-in-depth check)"
	ReasonAutomountBrowseGlob = "Automount browse surface glob pattern detected (defense-in-depth check)"
	ReasonKernelResolvedGlob  = "Kernel-resolved path prefix (/.vol etc.) glob pattern detected (defense-in-depth check)"
)

func readAsk(msg, reason string) ReadSafety {
	return ReadSafety{Message: msg, Reason: reason, SafetyCheck: true}
}

func msgReadAutomountHosts(h string) string {
	return "gocode requested permissions to read from " + escapeControl(h) +
		", which is under the /net automount map and could trigger a DNS lookup and NFS mount to a remote host."
}

func msgReadAutomountBrowse(h string) string {
	return "gocode requested permissions to read from " + escapeControl(h) +
		", which is under the /Network automount browse surface and could trigger a directory-service lookup and mount to a remote host."
}

func msgReadKernelResolved(h string) string {
	return "gocode requested permissions to read from " + escapeControl(h) +
		", which is under /.vol, /.file, /.nofollow or /.resolve (paths the macOS kernel redirects) and could reach a network mount, triggering a DNS lookup and mount to a remote host."
}

func msgReadUNC(h string) string {
	return "gocode requested permissions to read from " + escapeControl(h) +
		", which appears to be a UNC path that could access network resources."
}

// CheckReadNetworkSafety: a read of a path that is — or whose
// spellings reach — an automount map path, the /Network browse surface, a
// kernel-redirected macOS path or a UNC share, outside the trusted network
// directories, asks as a safety check; so does a Glob pattern of those shapes
// and any suspicious Windows spelling. requested is the path as the tool was
// given it; spellings are the walk's spellings (ResolvePath); globPattern is
// the Glob tool's pattern ("" for other tools).
func (s *SessionFiles) CheckReadNetworkSafety(requested string, spellings []string, globPattern string) ReadSafety {
	y := s.TrustedNetworkDirectories
	h := requested
	trusted := func(p string) bool { return underTrustedNetworkDir(p, y) }
	if (pathclass.IsAutomountPath(h) || pathclass.IsBareNetRoot(h)) && !trusted(h) {
		return readAsk(msgReadAutomountHosts(h), ReasonAutomountHosts)
	}
	if pathclass.IsGOPath(h) && !trusted(h) {
		return readAsk(msgReadAutomountBrowse(h), ReasonAutomountBrowse)
	}
	if pathclass.IsMacFirmlinkPath(h) && !trusted(h) {
		return readAsk(msgReadKernelResolved(h), ReasonKernelResolved)
	}
	for _, sp := range spellings {
		if pathclass.IsUNCOrDevicePath(sp) && !pathclass.IsWSLPath(sp) && !trusted(sp) {
			return readAsk(msgReadUNC(h), ReasonUNCPath)
		}
		if (pathclass.IsAutomountPath(sp) || pathclass.IsBareNetRoot(sp)) && !trusted(sp) {
			return readAsk(msgReadAutomountHosts(h), ReasonAutomountHosts)
		}
		if pathclass.IsGOPath(sp) && !trusted(sp) {
			return readAsk(msgReadAutomountBrowse(h), ReasonAutomountBrowse)
		}
		if pathclass.IsMacFirmlinkPath(sp) && !trusted(sp) {
			return readAsk(msgReadKernelResolved(h), ReasonKernelResolved)
		}
	}
	if g := globPattern; g != "" {
		if pathclass.IsUNCOrDevicePath(g) && !pathclass.IsWSLPath(g) && !trusted(g) {
			return readAsk("gocode requested permissions to glob "+g+", which appears to be a UNC pattern that could access network resources.", ReasonUNCGlob)
		}
		if (pathclass.IsAutomountPath(g) || pathclass.IsBareNetRoot(g)) && !trusted(g) {
			return readAsk("gocode requested permissions to glob "+g+", which is under the /net automount map and could trigger a DNS lookup and NFS mount to a remote host.", ReasonAutomountHostsGlob)
		}
		if pathclass.IsGOPath(g) && !trusted(g) {
			return readAsk("gocode requested permissions to glob "+g+", which is under the /Network automount browse surface and could trigger a directory-service lookup and mount to a remote host.", ReasonAutomountBrowseGlob)
		}
		if pathclass.IsMacFirmlinkPath(g) && !trusted(g) {
			return readAsk("gocode requested permissions to glob "+g+", which is under /.vol, /.file, /.nofollow or /.resolve (paths the macOS kernel redirects) and could reach a network mount, triggering a DNS lookup and mount to a remote host.", ReasonKernelResolvedGlob)
		}
	}
	for _, sp := range spellings {
		if IsSuspiciousWindowsSpelling(sp, s.platform(), y) {
			return ReadSafety{
				Message: "gocode requested permissions to read from " + escapeControl(h) +
					", which contains a suspicious Windows path pattern that requires manual approval.",
				Reason: ReasonSuspiciousWindowsRead,
			}
		}
	}
	return ReadSafety{Safe: true}
}
