package updater

import (
	"strings"
)

// Managed describes an installation that another tool owns: replacing its
// binary in place would leave that tool's records out of step (or fail on a
// read-only mount), so the update is left to it.
type Managed struct {
	// By names the owner, e.g. "Homebrew".
	By string
	// Upgrade is what to run, or do, to update instead.
	Upgrade string
}

// ManagedInstall reports whether the binary at exe (symlinks resolved) was
// installed by a package manager or runs from an AppImage. goos is the
// target system, getenv reads the environment and exists reports whether a
// file exists; repo ("owner/name") is where new releases are published.
func ManagedInstall(exe, goos, repo string, getenv func(string) string, exists func(string) bool) (Managed, bool) {
	releases := "https://github.com/" + repo + "/releases/latest"
	if getenv != nil && getenv("APPIMAGE") != "" {
		return Managed{By: "an AppImage", Upgrade: "download the new AppImage from " + releases}, true
	}
	p := strings.ToLower(strings.ReplaceAll(exe, `\`, "/"))
	switch {
	case strings.Contains(p, "/cellar/") || strings.Contains(p, "/homebrew/") || strings.Contains(p, "/.linuxbrew/"):
		return Managed{By: "Homebrew", Upgrade: "brew upgrade gocode"}, true
	case strings.Contains(p, "/nix/store/"):
		return Managed{By: "Nix", Upgrade: "update it through Nix (nix profile upgrade, or your configuration)"}, true
	}
	if goos == "windows" {
		switch {
		case strings.Contains(p, "/microsoft/winget/packages/") || strings.Contains(p, "/winget/links/"):
			return Managed{By: "winget", Upgrade: "winget upgrade gocode"}, true
		case strings.Contains(p, "/scoop/apps/") || strings.Contains(p, "/scoop/shims/"):
			return Managed{By: "Scoop", Upgrade: "scoop update gocode"}, true
		}
	}
	if goos == "linux" && (p == "/usr/bin/gocode" || p == "/bin/gocode") {
		by := "a system package"
		if exists != nil && exists("/var/lib/dpkg/info/gocode.list") {
			by = "a .deb package"
		} else if exists != nil && (exists("/var/lib/rpm") || exists("/usr/lib/sysimage/rpm")) {
			by = "an .rpm package"
		}
		return Managed{By: by, Upgrade: "install the new package from " + releases}, true
	}
	return Managed{}, false
}
