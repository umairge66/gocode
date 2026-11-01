//go:build !windows

package settingsload

import "os"

// replaceFile renames tmp over path.
func replaceFile(tmp, path string) error { return os.Rename(tmp, path) }
