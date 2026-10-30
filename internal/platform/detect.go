// Package platform provides small foundational helpers used across the CLI:
//
//   - OS / platform detection (this file). Its central
//     export CurrentPlatform resolves the running platform id used
//     across the CLI, including WSL discrimination from Linux.
//   - branded-string tagging helpers (see brand.go).
//
// Detection reads its host facts from an injectable Sources value so the
// logic can be unit-tested; the package-level functions use a lazily
// constructed default detector backed by the Go runtime and the host
// filesystem. The kernel release is a best-effort read.
package platform

import (
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Platform identifiers returned by CurrentPlatform.
const (
	PlatformMacOS   = "macos"
	PlatformWindows = "windows"
	PlatformWSL     = "wsl"
	PlatformLinux   = "linux"
	PlatformUnknown = "unknown"
)

// procVersionReadDisabled gates whether ReadProcVersionSync reads
// /proc/version. It is false by default, so proc-version reads are enabled.
var procVersionReadDisabled = false

// procVersionReadDisabledValue reports the flag.
func procVersionReadDisabledValue() bool { return procVersionReadDisabled }

// ReadProcVersionSync returns the lower-cased contents of /proc/version, used
// for WSL detection on Linux. The second result is false when the read is
// disabled or fails (the error itself is swallowed).
func ReadProcVersionSync() (string, bool) {
	if procVersionReadDisabledValue() {
		return "", false
	}
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return "", false
	}
	return strings.ToLower(string(b)), true
}

// isWSLKernel reports whether a (lower-cased) kernel string indicates WSL.
func isWSLKernel(kernel string) bool {
	return strings.Contains(kernel, "microsoft") || strings.Contains(kernel, "wsl")
}

// LinuxDistroInfo describes the running Linux distribution, assembled from the
// kernel release and /etc/os-release.
type LinuxDistroInfo struct {
	// LinuxKernel is the kernel release string.
	LinuxKernel string
	// LinuxDistroID is the os-release ID field.
	LinuxDistroID string
	// LinuxDistroVersion is the os-release VERSION_ID field.
	LinuxDistroVersion string
}

// Sources supplies the host facts the detector reads, injectable so detection
// is unit-testable.
type Sources struct {
	// Platform is the runtime.GOOS-style platform id ("darwin", "linux",
	// "windows", ...).
	Platform string
	// Env looks up an environment variable, returning "" when unset.
	Env func(key string) string
	// ReadProcVersionSync returns the lower-cased /proc/version contents,
	// ok=false on failure.
	ReadProcVersionSync func() (string, bool)
	// OSRelease returns the kernel release string.
	OSRelease func() string
	// ReadOSRelease returns the contents of /etc/os-release.
	ReadOSRelease func() (string, error)
}

// detector memoizes platform facts derived from its Sources. A mutex guards
// the caches because the package-level default detector is shared across the
// many importers of this package.

type detector struct {
	src Sources

	mu sync.Mutex

	primedProcVersion   string
	primedProcVersionOK bool

	fallbackProcVersion     string
	fallbackProcVersionOK   bool
	fallbackProcVersionRead bool

	platform       string
	platformCached bool

	wslVersion       string
	wslVersionOK     bool
	wslVersionCached bool

	macOSMajor       int
	macOSMajorOK     bool
	macOSMajorCached bool

	linuxDistro       *LinuxDistroInfo
	linuxDistroCached bool
}

// newDetector builds a detector over the given sources.
func newDetector(src Sources) *detector { return &detector{src: src} }

// kernelString returns the lower-cased kernel string, preferring the primed
// value and otherwise reading it once via the source (caching the result).
// Caller must hold d.mu.
func (d *detector) kernelString() (string, bool) {
	if d.primedProcVersionOK {
		return d.primedProcVersion, true
	}
	if !d.fallbackProcVersionRead {
		d.fallbackProcVersion, d.fallbackProcVersionOK = d.src.ReadProcVersionSync()
		d.fallbackProcVersionRead = true
	}
	return d.fallbackProcVersion, d.fallbackProcVersionOK
}

// getPlatform resolves and memoizes the platform id.
func (d *detector) getPlatform() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.platformCached {
		return d.platform
	}
	switch d.src.Platform {
	case "darwin":
		d.platform = PlatformMacOS
	case "windows": // runtime.GOOS
		d.platform = PlatformWindows
	case "linux":
		if d.src.Env("WSL_DISTRO_NAME") != "" || d.src.Env("WSL_INTEROP") != "" {
			d.platform = PlatformWSL
		} else if k, ok := d.kernelString(); ok && isWSLKernel(k) {
			d.platform = PlatformWSL
		} else {
			d.platform = PlatformLinux
		}
	default:
		d.platform = PlatformUnknown
	}
	d.platformCached = true
	return d.platform
}

// getWSLVersion resolves and memoizes the WSL major version ("1", "2", ...).
// ok=false means "not applicable / unknown".
func (d *detector) getWSLVersion() (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.wslVersionCached {
		return d.wslVersion, d.wslVersionOK
	}
	d.wslVersionCached = true
	if d.src.Platform != "linux" {
		return "", false
	}
	k, ok := d.kernelString()
	if !ok {
		return "", false
	}
	if m := wslVersionRE.FindStringSubmatch(k); m != nil && m[1] != "" {
		d.wslVersion, d.wslVersionOK = m[1], true
	} else if strings.Contains(k, "microsoft") {
		d.wslVersion, d.wslVersionOK = "1", true
	}
	return d.wslVersion, d.wslVersionOK
}

// getMacOSMajorVersion resolves and memoizes the macOS major version, derived
// from the Darwin kernel major (kernel major minus 9). ok=false when not
// applicable (not darwin) or unparseable.
func (d *detector) getMacOSMajorVersion() (int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.macOSMajorCached {
		return d.macOSMajor, d.macOSMajorOK
	}
	d.macOSMajorCached = true
	if d.src.Platform != "darwin" {
		return 0, false
	}
	m := darwinMajorRE.FindStringSubmatch(d.src.OSRelease())
	if m == nil || m[1] == "" {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	d.macOSMajor, d.macOSMajorOK = n-9, true
	return d.macOSMajor, d.macOSMajorOK
}

// getLinuxDistroInfo resolves and memoizes distro info, or nil when not Linux.
func (d *detector) getLinuxDistroInfo() *LinuxDistroInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.linuxDistroCached {
		return d.linuxDistro
	}
	d.linuxDistroCached = true
	if d.src.Platform != "linux" {
		return nil
	}
	info := &LinuxDistroInfo{LinuxKernel: d.src.OSRelease()}
	if contents, err := d.src.ReadOSRelease(); err == nil {
		for _, line := range strings.Split(contents, "\n") {
			m := osReleaseKVRE.FindStringSubmatch(line)
			if m == nil || m[1] == "" {
				continue
			}
			val := osReleaseQuoteRE.ReplaceAllString(m[2], "")
			switch m[1] {
			case "ID":
				info.LinuxDistroID = val
			case "VERSION_ID":
				info.LinuxDistroVersion = val
			}
		}
	}
	d.linuxDistro = info
	return info
}

// prime pre-reads /proc/version (Linux only) and resets the platform/WSL
// caches so the next query reflects the primed kernel string. The read is
// synchronous.
func (d *detector) prime() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.primedProcVersionOK || d.src.Platform != "linux" {
		return
	}
	// A read failure just leaves the primed value unset.
	v, ok := d.src.ReadProcVersionSync()
	if !ok {
		return
	}
	d.primedProcVersion, d.primedProcVersionOK = strings.ToLower(v), true
	d.platformCached = false
	d.wslVersionCached = false
}

var (
	wslVersionRE     = regexp.MustCompile(`wsl(\d+)`)
	darwinMajorRE    = regexp.MustCompile(`^(\d+)\.`)
	osReleaseKVRE    = regexp.MustCompile(`^(ID|VERSION_ID)=(.*)$`)
	osReleaseQuoteRE = regexp.MustCompile(`^"|"$`)
)

// --- package-level default detector and exports ---

var (
	defaultOnce     sync.Once
	defaultDetector *detector
)

// def returns the lazily built default detector backed by the Go runtime and
// the host filesystem.
func def() *detector {
	defaultOnce.Do(func() {
		defaultDetector = newDetector(Sources{
			Platform:            runtime.GOOS,
			Env:                 os.Getenv,
			ReadProcVersionSync: ReadProcVersionSync,
			OSRelease:           defaultOSRelease,
			ReadOSRelease:       func() (string, error) { b, err := os.ReadFile("/etc/os-release"); return string(b), err },
		})
	})
	return defaultDetector
}

// defaultOSRelease is a best-effort, stdlib-only kernel release reader. It
// reads /proc/sys/kernel/osrelease, which is present on Linux and WSL and returns ""
// elsewhere.
func defaultOSRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// CurrentPlatform returns the running platform id (one of the Platform*
// constants). settingspolicy.MergeDeps.CurrentPlatform is wired to this.
func CurrentPlatform() string { return def().getPlatform() }
