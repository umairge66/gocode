//go:build windows

package daemon

import "os"

// verifyOwnedFile: the roster lives in the user's profile, whose ACL
// restricts it to the user.
func verifyOwnedFile(string, os.FileInfo) error { return nil }
