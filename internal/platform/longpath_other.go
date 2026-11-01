//go:build !windows

package platform

// LongPath returns p unchanged: short names exist only on Windows.
func LongPath(p string) string { return p }
