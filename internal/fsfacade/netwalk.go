package fsfacade

// This file implements the network-aware symlink-chain walker. Unlike Realpath (the plain canonicalizing
// walk), this walker never steps onto a network share or automounter: when a
// component or a symlink target is a UNC path, a /net (or /Network/Servers)
// automount path, a bare /net, the /Network browse root or a macOS
// kernel-redirected path (/.vol, /.file, /.nofollow, /.resolve) it stops and
// reports the network spelling it would reach, WITHOUT touching it — touching
// it is what triggers the DNS lookup and mount, and what hangs on a dead
// server. Under the "unverified ancestry" policy a component that cannot be
// examined ends the walk with WalkUnverified instead.

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/umairge66/gocode/internal/pathclass"
)

// WalkVerdict is the walker's return value.
type WalkVerdict int

const (
	// WalkClean means the walk ended without reaching
	// a network spelling (resolved, absent, or a symlink cycle).
	WalkClean WalkVerdict = iota
	// WalkUnverified is the unverified-ancestry sentinel: a component could not be examined, a link
	// chain was too long, a probe timed out, or the walk ran out of budget.
	WalkUnverified
	// WalkReached means the walk reached a network/automount spelling; it is in
	// WalkResult.Reached.
	WalkReached
)

// OutcomeKind is the kind of WalkOutcome.
type OutcomeKind int

const (
	// OutcomeNone: no outcome was reported.
	OutcomeNone OutcomeKind = iota
	// OutcomeResolved: every component resolved; Path is the landing.
	OutcomeResolved
	// OutcomeAbsent: a component is missing (ENOENT) or not a folder (ENOTDIR).
	OutcomeAbsent
)

// WalkOutcome is the walker's outcome report.
type WalkOutcome struct {
	Kind           OutcomeKind
	Path           string   // resolved landing
	At             string   // deepest existing prefix (absent)
	Remaining      []string // components not found (absent)
	FromLinkTarget bool     // the missing part came from a link's target
	NotAFolder     bool     // ENOTDIR
}

// Errno is the coarse error class the walker reasons about.
type Errno int

const (
	ErrnoNone Errno = iota
	ErrnoNoEnt
	ErrnoNotDir
	ErrnoNameTooLong
	ErrnoAccess // EACCES or EPERM
	ErrnoLoop   // ELOOP
	ErrnoOther  // anything else, including a probe timeout
)

// ClassifyErrno maps a filesystem error onto the walker's error classes.
func ClassifyErrno(err error) Errno {
	switch {
	case err == nil:
		return ErrnoNone
	case errors.Is(err, ErrProbeTimeout):
		return ErrnoOther
	case errors.Is(err, syscall.ENOTDIR):
		return ErrnoNotDir
	case errors.Is(err, syscall.ENAMETOOLONG):
		return ErrnoNameTooLong
	case errors.Is(err, syscall.ELOOP):
		return ErrnoLoop
	case errors.Is(err, fs.ErrNotExist):
		return ErrnoNoEnt
	case errors.Is(err, fs.ErrPermission):
		return ErrnoAccess
	}
	var nd errNotDirectory
	if errors.As(err, &nd) {
		return ErrnoNotDir
	}
	return ErrnoOther
}

// WalkOptions are the walker's options.
type WalkOptions struct {
	// Anchors are trusted network roots the walk may stay within.
	Anchors []string
	// SurfaceNetworkRaw reports a network spelling reached through a relative
	// link or a bare /net target as soon as it is composed, and makes a kernel-redirected component unverified.
	SurfaceNetworkRaw bool
	// UnverifiedAncestry makes a component that cannot be examined end the
	// walk with WalkUnverified rather than as clean.
	UnverifiedAncestry bool
	// LaunchAncestry is the launch directory's shared ancestry with the cwd:
	// an EPERM/EACCES ancestor of it may be
	// crossed without being examined, provided no "." / ".." follows.
	LaunchAncestry string
	// OnHop is called for each symlink hop.
	OnHop func(composed string, leaf bool, text string)
	// OnCollapsedLanding receives the landing a dot-segment collapse produced.
	OnCollapsedLanding func(string)
	// Deadline, when non-zero, ends the walk with WalkUnverified once passed.
	Deadline time.Time
}

// WalkResult is the walk's verdict plus the probe trace callers need.
type WalkResult struct {
	Verdict WalkVerdict
	// Reached is the network spelling reached (WalkReached only).
	Reached string
	// Outcome is the onOutcome payload, if any.
	Outcome WalkOutcome
	// SawReadlink reports that a readlink was issued.
	SawReadlink bool
	// ReadlinkFailed reports a failed readlink.
	ReadlinkFailed bool
	// LastErrno is the error class of the last lstat/opendir probe, reset by
	// each later non-readlink probe.
	LastErrno Errno
	// OpendirLoop reports an opendir probe that failed with ELOOP.
	OpendirLoop bool
	// StoppedAt is the last path probed.
	StoppedAt string
}

// maxWalkHops is the walker's symlink-hop limit.
const maxWalkHops = 64

var trailingDotSpace = regexp.MustCompile(`[. ]$`)

// isWindowsPaths selects win32 path semantics (drive roots, "\" separators,
// UNC joins); it is set on Windows.
var isWindowsPaths = runtime.GOOS == "windows"

// slash converts OS separators to "/" (a no-op on POSIX, where "\" is an
// ordinary filename character).
func slash(p string) string { return filepath.ToSlash(p) }

// pathRoot is the root of a slash path: "/" or "C:/" (or "C:" for a
// drive-relative spelling), "" for a relative one.
func pathRoot(p string) string {
	if isWindowsPaths && strings.HasPrefix(p, "//") {
		// win32 UNC root: "//server/share/" (e.g. a \\wsl$\distro path).
		parts := strings.SplitN(p[2:], "/", 3)
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return "//" + parts[0] + "/" + parts[1] + "/"
		}
		return "//"
	}
	if isWindowsPaths && len(p) >= 2 && isLetter(p[0]) && p[1] == ':' {
		if len(p) >= 3 && p[2] == '/' {
			return p[:3]
		}
		return p[:2]
	}
	if strings.HasPrefix(p, "/") {
		return "/"
	}
	return ""
}

// afterRoot is p without its root (empty when p is just the root).
func afterRoot(p, root string) string {
	if len(p) <= len(root) {
		return ""
	}
	return p[len(root):]
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// isAbsSlash reports whether a slash path is absolute.
func isAbsSlash(p string) bool {
	r := pathRoot(p)
	return r == "/" || strings.HasSuffix(r, "/")
}

// joinSlash joins slash paths; UNC spellings keep their leading pair on Windows.
func joinSlash(base string, elems ...string) string {
	if isWindowsPaths && strings.HasPrefix(base, "//") {
		return "//" + strings.TrimPrefix(path.Join(append([]string{base[2:]}, elems...)...), "/")
	}
	return path.Join(append([]string{base}, elems...)...)
}

// resolveSlash resolves elems against base for slash paths (an absolute
// element restarts the result).
func resolveSlash(base string, elems ...string) string {
	out := base
	for _, e := range elems {
		if isAbsSlash(e) {
			out = e
			continue
		}
		out = joinSlash(out, e)
	}
	return joinSlash(out)
}

// splitComponents splits p on "/" and drops empty components.
func splitComponents(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isDotSegmentLoose reports whether a component or path has a "." / ".."
// segment.
func isDotSegmentLoose(p string) bool { return pathclass.HasDotSegment(p) }

// --- the anchor tests -------------------------------------------------------

// isNetworkRoot reports whether the first segment is "Network".
func isNetworkRoot(e string) bool { return pathclass.IsNetworkRoot(e) }

// within reports whether e is n or lies beneath it.
func within(e, n string) bool {
	trim := func(s string) string {
		if len(s) > 1 {
			return strings.TrimRight(s, "/")
		}
		return s
	}
	i, s := trim(e), trim(n)
	if i == s {
		return true
	}
	if s == "/" {
		return strings.HasPrefix(i, "/")
	}
	return strings.HasPrefix(i, s+"/")
}

// leavesNetworkAnchor reports whether e stays within (or automounts under) a
// /Network anchor without crossing its automount boundary.
func leavesNetworkAnchor(e string, anchors []string) bool {
	for _, r := range anchors {
		if pathclass.IsNetworkRoot(r) && !pathclass.CrossesAutomountBoundaryDarwin(e, r) &&
			(within(e, r) || pathclass.IsAutomountPath(e)) {
			return true
		}
	}
	return false
}

// anchorBeneath reports whether some /Network anchor lies within e.
func anchorBeneath(e string, anchors []string) bool {
	for _, r := range anchors {
		if pathclass.IsNetworkRoot(r) && within(r, e) {
			return true
		}
	}
	return false
}

// crossesEveryAutomount reports whether e crosses the automount boundary of
// every anchor.
func crossesEveryAutomount(e string, anchors []string) bool {
	for _, r := range anchors {
		if !pathclass.CrossesAutomountBoundaryDarwin(e, r) {
			return false
		}
	}
	return true
}

// crossesEveryUNC reports whether e crosses the UNC boundary of every anchor.
func crossesEveryUNC(e string, anchors []string) bool {
	for _, r := range anchors {
		if !pathclass.CrossesUNCBoundary(e, r) {
			return false
		}
	}
	return true
}

// isUNC reports a UNC/device spelling that is not a WSL path.
func isUNC(e string) bool { return pathclass.IsUNCOrDevicePath(e) && !pathclass.IsWSLPath(e) }

// --- collapse helpers -------------------------------------------------------

// truncateAtDotSegment returns the prefix before the first "." / ".."
// segment, without trailing separators (the input itself when that is empty).
func truncateAtDotSegment(e string) string {
	var n strings.Builder
	for _, tok := range splitKeepSeparators(e) {
		if tok == "." || tok == ".." {
			break
		}
		n.WriteString(tok)
	}
	r := trailingSeps.ReplaceAllString(n.String(), "$1")
	if r == "" {
		return e
	}
	return r
}

var (
	sepRun       = regexp.MustCompile(`[\\/]+`)
	trailingSeps = regexp.MustCompile(`([^\\/])[\\/]+$`)
	leadingSlash = regexp.MustCompile(`^/+`)
)

// splitKeepSeparators splits e into parts interleaved with the
// separator runs between them.
func splitKeepSeparators(e string) []string {
	var out []string
	last := 0
	for _, loc := range sepRun.FindAllStringIndex(e, -1) {
		out = append(out, e[last:loc[0]], e[loc[0]:loc[1]])
		last = loc[1]
	}
	return append(out, e[last:])
}

// collapseLanding collapses a dot-segment landing to its automount anchor or
// to the prefix before the first dot segment.
func collapseLanding(e string) string {
	if strings.HasPrefix(e, `\\`) {
		return truncateAtDotSegment(e)
	}
	n := leadingSlash.ReplaceAllString(e, "/")
	if r := pathclass.AutomountAnchor(n); r != "" {
		return r
	}
	return truncateAtDotSegment(e)
}

// landOn composes the landing for e plus the remaining components, collapsing
// dot segments and reporting a collapsed landing to cb.
func landOn(e string, rest []string, cb func(string)) string {
	dotted := isDotSegmentLoose(e)
	hasDot := false
	for _, s := range rest {
		if s == "." || s == ".." {
			hasDot = true
			break
		}
	}
	if len(rest) == 0 || dotted || hasDot {
		if cb != nil && (len(rest) > 0 || dotted) {
			cb(joinSlash(e, rest...))
		}
		if dotted {
			return collapseLanding(e)
		}
		return e
	}
	return joinSlash(e, rest...)
}

// automountComposed returns the automount spelling e reaches when it crosses
// every anchor's automount boundary, or "".
func automountComposed(e string, anchors []string, cb func(string)) string {
	if !pathclass.IsAutomountPath(e) {
		return ""
	}
	i := pathclass.AutomountAnchor(e)
	if i == "" || !crossesEveryAutomount(i, anchors) {
		return ""
	}
	s := joinSlash(e)
	if pathclass.IsAutomountPath(s) && pathclass.AutomountAnchorHost(s) == pathclass.AutomountAnchorHost(i) {
		return s
	}
	return landOn(e, nil, cb)
}

// isStrictAncestor reports whether e is a proper ancestor of n.
func isStrictAncestor(e, n string) bool {
	i, s := resolveSlash("/", e), resolveSlash("/", n)
	if pathRoot(i) != pathRoot(s) {
		return false
	}
	o, a := splitComponents(afterRoot(i, pathRoot(i))), splitComponents(afterRoot(s, pathRoot(s)))
	if len(o) >= len(a) {
		return false
	}
	for k := range o {
		if o[k] != a[k] {
			return false
		}
	}
	return true
}

// --- the walker -------------------------------------------------------------

// Walk runs the network-aware walker over fsys. p is resolved
// against fsys's working directory when relative. Every probe goes through
// fsys; wrap it in a TimeoutFS so a stalled mount cannot hang the walk.
func Walk(fsys FS, p string, o WalkOptions) WalkResult {
	var res WalkResult
	reached := func(s string) WalkResult {
		res.Verdict, res.Reached = WalkReached, s
		return res
	}
	unverified := func() WalkResult {
		res.Verdict = WalkUnverified
		return res
	}
	clean := func() WalkResult {
		res.Verdict = WalkClean
		return res
	}
	// An error ends the walk; under the unverified policy it is
	// unverified unless it merely says "absent".
	ended := func(cls Errno, hopped, anyDot, trailing bool) WalkResult {
		if !o.UnverifiedAncestry {
			return clean()
		}
		if cls == ErrnoNoEnt || cls == ErrnoNotDir {
			return clean()
		}
		if cls == ErrnoNameTooLong && !hopped && !anyDot && !trailing {
			return clean()
		}
		return unverified()
	}
	expired := func() bool { return !o.Deadline.IsZero() && time.Now().After(o.Deadline) }

	r := o.Anchors
	e := slash(p)
	if isNetworkRoot(e) && !leavesNetworkAnchor(e, r) {
		return reached(e)
	}
	if (isUNC(e) && crossesEveryUNC(e, r)) || (pathclass.IsAutomountPath(e) && crossesEveryAutomount(e, r)) {
		return clean()
	}
	i := e
	if !isAbsSlash(i) {
		cwd := "/"
		if c, ok := fsys.(Cwder); ok {
			if wd, err := c.Getwd(); err == nil && wd != "" {
				cwd = slash(wd)
			}
		}
		i = resolveSlash(cwd, i)
	} else {
		i = joinSlash(i)
	}
	root := pathRoot(i)
	cur := root
	a := splitComponents(afterRoot(i, root))
	hops := 0
	minRest := len(a)
	crossedUnreadable := false
	examined := false
	visited := map[string]bool{}
	anyDot := func() bool {
		for _, s := range a {
			if isDotSegmentLoose(s) {
				return true
			}
		}
		return false
	}
	trailing := func() bool { return len(a) > 0 && trailingDotSpace.MatchString(a[0]) }

	for len(a) > 0 && hops < maxWalkHops {
		if expired() {
			return unverified()
		}
		if crossedUnreadable && isDotSegmentLoose(a[0]) {
			return unverified()
		}
		f := joinSlash(cur, a[0])
		if (pathclass.IsAutomountPath(f) && crossesEveryAutomount(f, r)) ||
			(isNetworkRoot(f) && !(leavesNetworkAnchor(f, r) || anchorBeneath(f, r))) {
			return reached(landOn(f, a[1:], o.OnCollapsedLanding))
		}
		if pathclass.IsMacFirmlinkPath(f) {
			if o.SurfaceNetworkRaw || o.UnverifiedAncestry {
				return unverified()
			}
			return reached(landOn(f, a[1:], o.OnCollapsedLanding))
		}
		res.StoppedAt = f
		res.LastErrno = ErrnoNone
		fi, err := fsys.Lstat(fromSlash(f))
		if err != nil {
			cls := ClassifyErrno(err)
			res.LastErrno = cls
			if cls != ErrnoNoEnt && cls != ErrnoNotDir && cls != ErrnoNameTooLong {
				if expired() {
					return unverified()
				}
				oerr := openDirNoFollowVia(fsys, fromSlash(f))
				if oerr == nil {
					res.LastErrno = ErrnoNone
					examined = true
					a = a[1:]
					cur = f
					continue
				}
				ocls := ClassifyErrno(oerr)
				if !errors.Is(oerr, errors.ErrUnsupported) {
					// LastErrno is the last probe's errno; a platform without
					// the opendir probe keeps the lstat's.
					res.LastErrno = ocls
				}
				if ocls == ErrnoLoop {
					res.OpendirLoop = true
				}
			}
			if o.LaunchAncestry != "" && !examined && cls == ErrnoAccess && isStrictAncestor(f, slash(o.LaunchAncestry)) {
				crossedUnreadable = true
				a = a[1:]
				cur = f
				continue
			}
			if cls == ErrnoNoEnt || cls == ErrnoNotDir {
				res.Outcome = WalkOutcome{
					Kind:           OutcomeAbsent,
					At:             cur,
					Remaining:      append([]string(nil), a...),
					FromLinkTarget: len(a) > minRest,
					NotAFolder:     cls == ErrnoNotDir,
				}
			}
			return ended(cls, hops > 0, anyDot(), trailing())
		}
		examined = true
		if fi.Mode()&fs.ModeSymlink == 0 {
			a = a[1:]
			cur = f
			continue
		}
		hops++
		key := f + "\x00" + strings.Join(a, "\x00")
		if visited[key] {
			return clean()
		}
		visited[key] = true
		if expired() {
			return unverified()
		}
		res.SawReadlink = true
		target, err := fsys.Readlink(fromSlash(f))
		if err != nil {
			res.ReadlinkFailed = true
			return ended(ClassifyErrno(err), hops > 0, anyDot(), trailing())
		}
		c := slash(target)
		if !isAbsSlash(c) && !isUNC(c) && !strings.HasPrefix(c, "//") {
			t := splitComponents(c)
			if o.SurfaceNetworkRaw {
				composed := strings.TrimSuffix(cur, "/") + "/" + strings.Join(append(append([]string(nil), t...), a[1:]...), "/")
				if s := automountComposed(composed, r, o.OnCollapsedLanding); s != "" {
					return reached(s)
				}
			}
			a = a[1:]
			if o.OnHop != nil {
				o.OnHop(resolveSlash(cur, append([]string{c}, a...)...), len(a) == 0, c)
			}
			if len(a) < minRest {
				minRest = len(a)
			}
			a = append(t, a...)
			continue
		}
		w := c
		if o.SurfaceNetworkRaw && len(a) > 1 && pathclass.IsBareNetRoot(w) {
			if s := automountComposed(joinSlash(w, a[1:]...), r, o.OnCollapsedLanding); s != "" {
				return reached(s)
			}
		}
		if (isUNC(w) && crossesEveryUNC(w, r)) ||
			(pathclass.IsAutomountPath(w) && crossesEveryAutomount(w, r)) ||
			(isNetworkRoot(w) && !(leavesNetworkAnchor(w, r) || (len(a) > 1 && anchorBeneath(w, r)))) {
			a = a[1:]
			return reached(landOn(w, a, o.OnCollapsedLanding))
		}
		if o.SurfaceNetworkRaw && isDotSegmentLoose(w) &&
			(pathclass.IsUNCOrDevicePath(w) || pathclass.IsAutomountPath(w) || isNetworkRoot(w) || pathclass.IsBareNetRoot(w)) {
			a = a[1:]
			if len(a) == 0 {
				return reached(w)
			}
			return reached(w + "/" + strings.Join(a, "/"))
		}
		a = a[1:]
		d := pathRoot(w)
		if d == "" {
			d = "/"
		}
		v := pathRoot(f)
		rootMismatch := d == "/" && v != "" && v != "/"
		if rootMismatch && (o.SurfaceNetworkRaw || o.UnverifiedAncestry) {
			return unverified()
		}
		if o.OnHop != nil {
			composed := w
			if len(a) > 0 {
				composed = resolveSlash(w, a...)
			}
			o.OnHop(composed, len(a) == 0, c)
		}
		if rootMismatch {
			cur = v
		} else {
			cur = d
		}
		examined = false
		if len(a) < minRest {
			minRest = len(a)
		}
		rest := w
		if strings.HasPrefix(rest, d) {
			rest = rest[len(d):]
		}
		a = append(splitComponents(rest), a...)
	}
	if len(a) > 0 {
		return unverified()
	}
	if hops > 0 && pathclass.IsBareNetRoot(cur) {
		return reached(cur)
	}
	if isNetworkRoot(cur) && !leavesNetworkAnchor(cur, r) {
		return reached(cur)
	}
	res.Outcome = WalkOutcome{Kind: OutcomeResolved, Path: cur}
	return clean()
}

// fromSlash converts back to OS separators for the fs call.
func fromSlash(p string) string { return filepath.FromSlash(p) }

// openDirNoFollowVia runs fsys's DirNoFollowOpener, or fails when absent.
func openDirNoFollowVia(fsys FS, name string) error {
	if o, ok := fsys.(DirNoFollowOpener); ok {
		return o.OpenDirNoFollow(name)
	}
	return &fs.PathError{Op: "opendir", Path: name, Err: errors.ErrUnsupported}
}

// RealpathVia is the walker's realpath operation: fsys's own realpath when
// it has one, else this package's POSIX Realpath.
func RealpathVia(fsys FS, name string) (string, error) {
	if r, ok := fsys.(Realpather); ok {
		return r.RealpathOS(name)
	}
	return Realpath(fsys, name)
}
