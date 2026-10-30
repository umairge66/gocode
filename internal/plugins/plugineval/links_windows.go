//go:build windows

package plugineval

import "os"

// hardLinks is 1 on Windows, where os.FileInfo carries no link count.
func hardLinks(os.FileInfo) uint64 { return 1 }
