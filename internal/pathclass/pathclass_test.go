package pathclass

import (
	"testing"
)

func TestIsNTDevicePath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`\??\C:`, true},
		{`/??/foo`, true},
		{`\??/bar`, true},
		{`C:\Windows`, false},
		{`/usr/local`, false},
		{`has??inside`, false}, // "??" but no device prefix after normalise
		{`/??`, false},         // no trailing separator
	}
	for _, c := range cases {
		if got := IsNTDevicePath(c.in); got != c.want {
			t.Errorf("IsNTDevicePath(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestIsUNCOrDevicePath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`\\server\share`, true},
		{`//server/share`, true},
		{`\??\C:`, true},
		{`/single`, false},
		{`C:\x`, false},
		{`\\`, true},
	}
	for _, c := range cases {
		if got := IsUNCOrDevicePath(c.in); got != c.want {
			t.Errorf("IsUNCOrDevicePath(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestHasDotSegment(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"a/./b", true},
		{"a/../b", true},
		{"./x", true},
		{"a/. /b", true}, // "." then trailing space before sep
		{"a/.. /b", true},
		{"a/.foo/b", false},
		{"a/b", false},
		{"...", true}, // ".." + trailing "." matches the loose dot-segment regex
	}
	for _, c := range cases {
		if got := HasDotSegment(c.in); got != c.want {
			t.Errorf("HasDotSegment(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestHasDotSegmentForOS(t *testing.T) {
	// win32 treats backslash as a separator; linux does not.
	if HasDotSegmentForOS(`a\..\b`, "linux") {
		t.Error("linux should not see backslash dot-segments")
	}
	if !HasDotSegmentForOS(`a\..\b`, "win32") {
		t.Error("win32 should see backslash dot-segments")
	}
	if !HasDotSegmentPOSIX("a/../b") {
		t.Error("posix dot-segment")
	}
}

func TestIsDeviceNamespaceRoot(t *testing.T) {
	for _, s := range []string{`\\?\C:\`, `\\.\PhysicalDrive0`, `//?/x`, `//./x`} {
		if !IsDeviceNamespaceRoot(s) {
			t.Errorf("IsDeviceNamespaceRoot(%q) should be true", s)
		}
	}
	for _, s := range []string{`\\server\share`, `C:\x`, `/usr`} {
		if IsDeviceNamespaceRoot(s) {
			t.Errorf("IsDeviceNamespaceRoot(%q) should be false", s)
		}
	}
}

func TestUNCServerName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`\\Server\share`, "server"},
		{`//HOST/share`, "host"},
		{`\\?\UNC\srv\share`, "srv"},
		{`\\.\UNC\SRV\share`, "srv"},
		{`\\?\C:\x`, ""}, // device root, not UNC
		{`C:\x`, ""},
		{`/usr`, ""},
	}
	for _, c := range cases {
		if got := UNCServerName(c.in); got != c.want {
			t.Errorf("UNCServerName(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestCrossesUNCBoundary(t *testing.T) {
	cases := []struct {
		p, base string
		want    bool
	}{
		{`\\srv\a`, `\\srv\b`, false},   // same server
		{`\\srv\a`, `\\other\b`, true},  // different server
		{`\\?\C:`, `\\srv\b`, true},     // device root
		{`\\srv\..\x`, `\\srv\b`, true}, // dot segment
		{`C:\x`, `\\srv\b`, false},      // not UNC at all
	}
	for _, c := range cases {
		if got := CrossesUNCBoundary(c.p, c.base); got != c.want {
			t.Errorf("CrossesUNCBoundary(%q,%q)=%v want %v", c.p, c.base, got, c.want)
		}
	}
}

func TestIsMacFirmlinkPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/.vol/123/file", true},
		{"/.file/foo", true},
		{"/.nofollow/x", true},
		{"/.resolve/x", true},
		{"/usr/.vol/x", false}, // .vol not the first segment
		{"/.volume/x", false},  // not an exact firmlink segment
		{".vol/x", false},      // not absolute
		{"/.vol", true},        // ".vol" alone at end
	}
	for _, c := range cases {
		if got := IsMacFirmlinkPath(c.in); got != c.want {
			t.Errorf("IsMacFirmlinkPath(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestAutomountAnchorHostAndPrefix(t *testing.T) {
	cases := []struct {
		in        string
		host      string
		prefix    string
		isAuto    bool
		isNetwork bool
		isBareNet bool
	}{
		{"/net/host1/share", "host1", "/net/host1", true, false, false},
		{"/network/servers/HostX/d", "hostx", "/network/servers/HostX", true, true, false},
		{"/net", "", "", false, false, true},
		{"/network/x", "", "", false, true, false},
		{"/usr/local", "", "", false, false, false},
		{"relative/net/host", "", "", false, false, false},
		{"/net/./host/..", "host", "/net/host", true, false, true}, // resolves to single "net"
	}
	for _, c := range cases {
		if got := AutomountAnchorHost(c.in); got != c.host {
			t.Errorf("AutomountAnchorHost(%q)=%q want %q", c.in, got, c.host)
		}
		if got := automountAnchorPrefix(c.in); got != c.prefix {
			t.Errorf("automountAnchorPrefix(%q)=%q want %q", c.in, got, c.prefix)
		}
		if got := IsAutomountPath(c.in); got != c.isAuto {
			t.Errorf("IsAutomountPath(%q)=%v want %v", c.in, got, c.isAuto)
		}
		if got := IsNetworkRoot(c.in); got != c.isNetwork {
			t.Errorf("IsNetworkRoot(%q)=%v want %v", c.in, got, c.isNetwork)
		}
		if got := IsBareNetRoot(c.in); got != c.isBareNet {
			t.Errorf("IsBareNetRoot(%q)=%v want %v", c.in, got, c.isBareNet)
		}
	}
}

func TestIsNetworkRootBareNetwork(t *testing.T) {
	if !IsNetworkRoot("/network/h/x") {
		t.Error("/network path should be a network root")
	}
}

func TestWSL(t *testing.T) {
	cases := []struct {
		in    string
		isWSL bool
	}{
		{`\\wsl$\Ubuntu\home`, true},
		{`\\wsl.localhost\Debian\x`, true},
		{`\\wsl$\.\x`, false}, // degenerate distro -> not WSL
		{`\\server\share`, false},
		{`/usr/local`, false},
	}
	for _, c := range cases {
		if got := IsWSLPath(c.in); got != c.isWSL {
			t.Errorf("IsWSLPath(%q)=%v want %v", c.in, got, c.isWSL)
		}
	}
}

func TestStripMacDataVolume(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/System/Volumes/Data/Users/me", "/Users/me"},
		{"/System/Volumes/Data/usr/local/bin", "/usr/local/bin"},
		{"/System/Volumes/Data/Unknown/x", "/System/Volumes/Data/Unknown/x"}, // not a known root
		{"/Users/me", "/Users/me"},
	}
	for _, c := range cases {
		if got := StripMacDataVolume(c.in); got != c.want {
			t.Errorf("StripMacDataVolume(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestIsSuspectDarwinAutomount(t *testing.T) {
	// On darwin, network roots are suspect.
	if !isSuspectDarwinAutomount("/network/h/x", "darwin", true, "/home/me") {
		t.Error("network root should be suspect on darwin")
	}
	// A /home anchor different from current is suspect.
	if !isSuspectDarwinAutomount("/home/other", "darwin", true, "/home/me") {
		t.Error("foreign home anchor should be suspect")
	}
	if isSuspectDarwinAutomount("/home/me", "darwin", true, "/home/me") {
		t.Error("own home anchor should not be suspect")
	}
}

func TestIsGOPathDisabled(t *testing.T) {
	// Disabled predicate in this build: always false.
	if IsGOPath("/anything") {
		t.Error("IsGOPath should always be false in this build")
	}
}

func TestFoldHelpers(t *testing.T) {
	if lowerASCII("ABC-ÀÉ") != "abc-ÀÉ" {
		t.Errorf("lowerASCII should only touch ASCII: %q", lowerASCII("ABC-ÀÉ"))
	}
}
