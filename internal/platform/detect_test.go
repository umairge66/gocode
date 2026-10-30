package platform

import (
	"reflect"
	"testing"
)

// testSources builds a Sources with sensible defaults for the fields a case
// does not care about.
func testSources(platform string, env map[string]string, kernel string, release string, osRelease string) Sources {
	return Sources{
		Platform: platform,
		Env: func(k string) string {
			if env == nil {
				return ""
			}
			return env[k]
		},
		ReadProcVersionSync: func() (string, bool) {
			if kernel == "" {
				return "", false
			}
			return kernel, true
		},
		OSRelease:     func() string { return release },
		ReadOSRelease: func() (string, error) { return osRelease, nil },
	}
}

func TestGetPlatform(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		env      map[string]string
		kernel   string
		want     string
	}{
		{"darwin->macos", "darwin", nil, "", PlatformMacOS},
		{"windows->windows", "windows", nil, "", PlatformWindows},
		{"linux plain", "linux", nil, "linux version 6.1.0-generic", PlatformLinux},
		{"linux no kernel", "linux", nil, "", PlatformLinux},
		{"linux WSL_DISTRO_NAME", "linux", map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}, "", PlatformWSL},
		{"linux WSL_INTEROP", "linux", map[string]string{"WSL_INTEROP": "/run/WSL/1_interop"}, "", PlatformWSL},
		{"linux kernel microsoft", "linux", nil, "linux version 5.15.0-microsoft-standard-wsl2", PlatformWSL},
		{"linux kernel wsl", "linux", nil, "linux version 5.15.0-wsl", PlatformWSL},
		{"unknown platform", "freebsd", nil, "", PlatformUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDetector(testSources(c.platform, c.env, c.kernel, "", ""))
			if got := d.getPlatform(); got != c.want {
				t.Fatalf("getPlatform() = %q, want %q", got, c.want)
			}
			// Second call must return the memoized value.
			if got := d.getPlatform(); got != c.want {
				t.Fatalf("getPlatform() memoized = %q, want %q", got, c.want)
			}
		})
	}
}

func TestGetWSLVersion(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		kernel   string
		want     string
		wantOK   bool
	}{
		{"not linux", "darwin", "", "", false},
		{"no kernel", "linux", "", "", false},
		{"wsl2", "linux", "linux version 5.15.0-microsoft-standard-wsl2", "2", true},
		{"microsoft only -> 1", "linux", "microsoft linux", "1", true},
		{"plain linux", "linux", "linux version 6.1.0-generic", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDetector(testSources(c.platform, nil, c.kernel, "", ""))
			got, ok := d.getWSLVersion()
			if got != c.want || ok != c.wantOK {
				t.Fatalf("getWSLVersion() = (%q,%v), want (%q,%v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestGetMacOSMajorVersion(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		release  string
		want     int
		wantOK   bool
	}{
		{"not darwin", "linux", "6.1.0", 0, false},
		{"sonoma darwin 23", "darwin", "23.1.0", 14, true},
		{"bigsur darwin 20", "darwin", "20.6.0", 11, true},
		{"unparseable", "darwin", "notaversion", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDetector(testSources(c.platform, nil, "", c.release, ""))
			got, ok := d.getMacOSMajorVersion()
			if got != c.want || ok != c.wantOK {
				t.Fatalf("getMacOSMajorVersion() = (%d,%v), want (%d,%v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestGetLinuxDistroInfo(t *testing.T) {
	t.Run("not linux returns nil", func(t *testing.T) {
		d := newDetector(testSources("darwin", nil, "", "23.1.0", ""))
		if info := d.getLinuxDistroInfo(); info != nil {
			t.Fatalf("expected nil, got %+v", info)
		}
	})
	t.Run("parses os-release", func(t *testing.T) {
		osRel := "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"22.04\"\nPRETTY_NAME=\"Ubuntu 22.04\"\n"
		d := newDetector(testSources("linux", nil, "", "6.1.0-generic", osRel))
		info := d.getLinuxDistroInfo()
		if info == nil {
			t.Fatal("expected info, got nil")
		}
		want := &LinuxDistroInfo{LinuxKernel: "6.1.0-generic", LinuxDistroID: "ubuntu", LinuxDistroVersion: "22.04"}
		if !reflect.DeepEqual(info, want) {
			t.Fatalf("got %+v, want %+v", info, want)
		}
	})
}

func TestPrimeResetsCachesWithKernel(t *testing.T) {
	// Before priming, a plain-linux kernel yields "linux".
	src := testSources("linux", nil, "linux version 5.15.0-microsoft-standard-wsl2", "", "")
	d := newDetector(src)
	// Prime reads proc version and resets caches.
	d.prime()
	if got := d.getPlatform(); got != PlatformWSL {
		t.Fatalf("after prime getPlatform() = %q, want %q", got, PlatformWSL)
	}
	if v, ok := d.getWSLVersion(); !ok || v != "2" {
		t.Fatalf("after prime getWSLVersion() = (%q,%v), want (2,true)", v, ok)
	}
}

func TestCurrentPlatformIsKnown(t *testing.T) {
	got := CurrentPlatform()
	valid := map[string]bool{
		PlatformMacOS: true, PlatformWindows: true, PlatformWSL: true,
		PlatformLinux: true, PlatformUnknown: true,
	}
	if !valid[got] {
		t.Fatalf("CurrentPlatform() = %q, not a known platform id", got)
	}
}
