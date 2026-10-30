//go:build !linux && !darwin && !windows

package settingsload

// readPlatformOSPolicy: no OS-managed policy source on this platform
// .
func readPlatformOSPolicy() OSPolicyReadout { return OSPolicyReadout{} }
