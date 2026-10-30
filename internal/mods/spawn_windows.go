//go:build windows

package mods

import "os"

// exitSignal is "" on Windows: a child there ends with an exit code.
func exitSignal(*os.ProcessState) string { return "" }
