//go:build linux

package settingsload

import "github.com/umairge66/gocode/internal/platform"

// readPlatformOSPolicy reads the OS policy on Linux: only WSL has one, the
// Windows registry policy reached through reg.exe.
func readPlatformOSPolicy() OSPolicyReadout {
	if platform.CurrentPlatform() != platform.PlatformWSL {
		return OSPolicyReadout{}
	}
	return readWSLOSPolicy()
}
