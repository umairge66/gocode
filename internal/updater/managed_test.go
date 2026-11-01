package updater

import "testing"

func TestManagedInstall(t *testing.T) {
	none := func(string) bool { return false }
	dpkg := func(p string) bool { return p == "/var/lib/dpkg/info/gocode.list" }
	cases := []struct {
		exe, goos, appimage string
		exists              func(string) bool
		want                string
	}{
		{"/opt/homebrew/Cellar/gocode/0.1.2/bin/gocode", "darwin", "", none, "Homebrew"},
		{"/usr/local/Cellar/gocode/0.1.2/bin/gocode", "darwin", "", none, "Homebrew"},
		{"/home/linuxbrew/.linuxbrew/Cellar/gocode/0.1.2/bin/gocode", "linux", "", none, "Homebrew"},
		{`C:\Users\u\AppData\Local\Microsoft\WinGet\Packages\umairge66.gocode_x\gocode.exe`, "windows", "", none, "winget"},
		{`C:\Users\u\scoop\apps\gocode\current\gocode.exe`, "windows", "", none, "Scoop"},
		{"/nix/store/abc-gocode-0.1.2/bin/gocode", "linux", "", none, "Nix"},
		{"/usr/bin/gocode", "linux", "", dpkg, "a .deb package"},
		{"/usr/bin/gocode", "linux", "", none, "a system package"},
		{"/tmp/.mount_gocodeX/usr/bin/gocode", "linux", "/home/u/gocode.AppImage", none, "an AppImage"},
		{"/home/u/.local/bin/gocode", "linux", "", none, ""},
		{`C:\Users\u\AppData\Local\Programs\gocode\gocode.exe`, "windows", "", none, ""},
		{"/usr/local/bin/gocode", "darwin", "", none, ""},
	}
	for _, c := range cases {
		getenv := func(k string) string {
			if k == "APPIMAGE" {
				return c.appimage
			}
			return ""
		}
		m, ok := ManagedInstall(c.exe, c.goos, "acme/tool", getenv, c.exists)
		if ok != (c.want != "") || m.By != c.want {
			t.Errorf("ManagedInstall(%q, %s) = %+v, %v; want %q", c.exe, c.goos, m, ok, c.want)
		}
		if ok && m.Upgrade == "" {
			t.Errorf("%q: no upgrade advice", c.exe)
		}
	}
}
