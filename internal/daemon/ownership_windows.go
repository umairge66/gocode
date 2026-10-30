//go:build windows

package daemon

// verifyOwnedDirs is a no-op on Windows: the per-user named pipes carry their
// own DACL and the breadcrumb directories live under the user's config home.
func verifyOwnedDirs(...string) error { return nil }
