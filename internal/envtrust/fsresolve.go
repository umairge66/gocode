package envtrust

import (
	"syscall"
)

// This file supplies the *filesystem half* that the pure trust predicates in
// this package were written to accept pre-extracted. Each pure function stays
// exactly as it was (SanitizeLine, ParseGitdirPointer, JoinUnlessAbsolute,
// IsWithin, IsToleratedPathError, the errno sets …); the helpers here read and
// canonicalize through an injectable fsfacade.FS and then hand the pure
// predicate the inputs it needs. Nothing here changes the public pure API.
//
// Not handled here: settings-tier resolution; repository / work-tree
// *vouching* (admin-dir realpath + "worktrees" parent check + commondir
// cross-check, and the second trusted root derived from a repo handle); and
// volume/device/junction classification around the realpath-then-containment
// core.

// errnoNames maps the syscall errnos this package classifies to their symbolic
// names, so a real OS error can be reduced to the same string the pure errno
// sets use. It is built in init rather than as a literal because some names
// collide on some platforms (e.g. ENOTSUP == EOPNOTSUPP on Linux), which a map
// literal would reject as a duplicate key.
var errnoNames = map[syscall.Errno]string{}

func init() {
	// Order matters only for colliding values; the surviving name is still a
	// member of the tolerated set, so either is correct for classification.
	for name, errno := range map[string]syscall.Errno{
		"EACCES":       syscall.EACCES,
		"EPERM":        syscall.EPERM,
		"EROFS":        syscall.EROFS,
		"EEXIST":       syscall.EEXIST,
		"ENOTDIR":      syscall.ENOTDIR,
		"ELOOP":        syscall.ELOOP,
		"ENAMETOOLONG": syscall.ENAMETOOLONG,
		"EOPNOTSUPP":   syscall.EOPNOTSUPP,
		"ENOTSUP":      syscall.ENOTSUP,
	} {
		errnoNames[errno] = name
	}
}
