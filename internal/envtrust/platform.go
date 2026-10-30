package envtrust

import "runtime"

// Platform names the host OS family, which the trust logic switches on to
// decide case sensitivity, drive letters and env-lookup rules.
type Platform string

const (
	// Linux is any non-macOS, non-Windows POSIX host.
	Linux Platform = "linux"
	// MacOS is Darwin.
	MacOS Platform = "macos"
	// Windows is win32.
	Windows Platform = "windows"
)

// CurrentPlatform maps the Go runtime to a Platform. Anything that is neither
// darwin nor windows is treated as Linux.
func CurrentPlatform() Platform {
	switch runtime.GOOS {
	case "windows":
		return Windows
	case "darwin":
		return MacOS
	default:
		return Linux
	}
}
