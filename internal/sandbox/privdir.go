package sandbox

// EnsurePrivateDir creates dir (0700) if needed. On Unix it also verifies the
// directory is a real directory (not a symlink) owned by this user and
// tightens its mode to 0700, so a name pre-created by another user under a
// shared temp directory is refused instead of used.
func EnsurePrivateDir(dir string) error { return ensurePrivateDir(dir) }
