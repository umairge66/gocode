package pathguard

import "testing"

// TestSuspiciousWindowsSpelling checks SuspiciousWindowsSpelling kind by kind, including its
// platform gates and check order.
func TestSuspiciousWindowsSpelling(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		platform string
		trusted  []string
		want     string
	}{
		// 8.3 short names: every platform (the tilde-digit test has no OS gate).
		{"8.3 dir", `C:\PROGRA~1\app\x.exe`, "windows", nil, WinSpellingTildeDigit},
		{"8.3 file", `C:\work\SECRET~1.TXT`, "windows", nil, WinSpellingTildeDigit},
		{"8.3 on linux too", `/work/PROGRA~1/x`, "linux", nil, WinSpellingTildeDigit},
		// Alternate data streams: colon past the drive position (windows/wsl).
		{"ADS ::$DATA", `C:\work\file.txt::$DATA`, "windows", nil, WinSpellingColonPastDrive},
		{"ADS named stream", `C:\work\file.txt:hidden`, "windows", nil, WinSpellingColonPastDrive},
		{"ADS on wsl", `/mnt/c/work/file.txt:hidden`, "wsl", nil, WinSpellingColonPastDrive},
		{"colon ignored on linux", `/work/file.txt:hidden`, "linux", nil, ""},
		{"drive colon itself is fine", `C:\work\file.txt`, "windows", nil, ""},
		// Trailing dots / spaces per segment (Unicode \s).
		{"trailing dot", `C:\work\secret.txt.`, "windows", nil, WinSpellingTrailingDotOrSpace},
		{"trailing dots", `C:\work\secret.txt...`, "windows", nil, WinSpellingTrailingDotOrSpace},
		{"trailing space", `C:\work\secret.txt `, "windows", nil, WinSpellingTrailingDotOrSpace},
		{"dir dot-space", `C:\work\dir. \x`, "windows", nil, WinSpellingTrailingDotOrSpace},
		{"trailing nbsp", "C:\\work\\secret.txt\u00a0", "windows", nil, WinSpellingTrailingDotOrSpace},
		{"trailing ideographic space", "/work/secret.txt\u3000", "linux", nil, WinSpellingTrailingDotOrSpace},
		{"dot segments are skipped", `C:\work\.\a\..\b`, "windows", nil, ""},
		// Device / long-path prefixes.
		{"\\\\?\\ drive (colon checked first)", `\\?\C:\work\x`, "windows", nil, WinSpellingColonPastDrive},
		{"\\\\?\\ drive off windows", `\\?\C:\work\x`, "linux", nil, WinSpellingDevicePathPrefix},
		{"\\\\.\\ device", `\\.\pipe\x`, "windows", nil, WinSpellingDevicePathPrefix},
		{"//?/ prefix", `//?/UNC/host/x`, "windows", nil, WinSpellingDevicePathPrefix},
		{"//./ prefix", `//./PhysicalDrive0`, "windows", nil, WinSpellingDevicePathPrefix},
		{"\\??\\ NT namespace", `\??\C:\work\x`, "windows", nil, WinSpellingNTDeviceNamespace},
		{"/??/ NT namespace", `/??/C:/work/x`, "linux", nil, WinSpellingNTDeviceNamespace},
		// DOS device suffix and dot runs.
		{"DOS device suffix", `C:\work\out.nul`, "windows", nil, WinSpellingDOSDeviceSuffix},
		{"DOS device suffix COM1", `C:\work\x.COM1`, "windows", nil, WinSpellingDOSDeviceSuffix},
		// A "..." segment already ends in dots, so the per-segment trailing-dot test
		// claims it before the dot-run test (which stays as defense in depth).
		{"dot run", `C:\work\...\x`, "windows", nil, WinSpellingTrailingDotOrSpace},
		// UNC / WebDAV (windows only), with the WSL and trusted-dir exemptions.
		{"UNC", `\\host\share\x`, "windows", nil, WinSpellingUNCOrWebDAV},
		{"UNC forward", `//host/share/x`, "windows", nil, WinSpellingUNCOrWebDAV},
		{"UNC mixed", `/\host\share\x`, "windows", nil, WinSpellingUNCOrWebDAV},
		{"UNC embedded", `C:\work\\host\share`, "windows", nil, WinSpellingUNCOrWebDAV},
		{"WebDAV", `C:\x\DavWWWRoot\y`, "windows", nil, WinSpellingUNCOrWebDAV},
		{"UNC not flagged off windows", `\\host\share\x`, "linux", nil, ""},
		{"WSL share exempt", `\\wsl$\Ubuntu\home\x`, "windows", nil, ""},
		{"trusted share exempt", `\\host\share\x`, "windows", []string{`\\host\share`}, ""},
		{"trusted share case-folded", `\\HOST\Share\x`, "windows", []string{`\\host\share`}, ""},
		{"trusted share needs same UNC shape", `\\host\share\x`, "windows", []string{`/host/share`}, WinSpellingUNCOrWebDAV},
		{"trusted share refuses dot segment", `\\host\share\..\x`, "windows", []string{`\\host\share`}, WinSpellingUNCOrWebDAV},
		// Ordinary.
		{"ordinary windows", `C:\work\src\main.go`, "windows", nil, ""},
		{"ordinary posix", `/home/me/src/main.go`, "linux", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SuspiciousWindowsSpelling(c.in, c.platform, c.trusted); got != c.want {
				t.Errorf("SuspiciousWindowsSpelling(%q,%q)=%q want %q", c.in, c.platform, got, c.want)
			}
			if got := IsSuspiciousWindowsSpelling(c.in, c.platform, c.trusted); got != (c.want != "") {
				t.Errorf("IsSuspiciousWindowsSpelling(%q)=%v", c.in, got)
			}
		})
	}
}

func TestIHPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\me`:    `/c/Users/me`,
		`c:/Users\me`:    `/c/Users/me`,
		`\\host\share\x`: `//host/share/x`,
		`a\b/c`:          `a/b/c`,
		`/work/x`:        `/work/x`,
		`C:`:             `C:`,
	} {
		if got := foldWindowsSpelling(in); got != want {
			t.Errorf("foldWindowsSpelling(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPathWithinUNCShape(t *testing.T) {
	cases := []struct {
		e, n             string
		fold, parity, ok bool
	}{
		{`//host/share/x`, `//host/share`, false, true, true},
		// Without parity a lexical clean would put "//work/x" inside "/work".
		{`//work/x`, `/work`, false, false, true},
		{`//work/x`, `/work`, false, true, false},
		{`/work/x`, `//work`, false, true, false},
		{`C:\Work\x`, `c:\Work`, false, true, true}, // foldWindowsSpelling lower-cases the drive
		{`C:\work\x`, `C:\Work`, false, true, false},
		{`C:\work\x`, `C:\Work`, true, true, true},
		{`/private/var/folders/x`, `/var/folders`, false, true, true},
		{`/private/tmp`, `/tmp`, false, true, true},
		{`/PRIVATE/TMP/x`, `/tmp`, true, true, true},
		{`/work/../etc`, `/work`, false, true, false},
	}
	for _, c := range cases {
		if got := pathWithin(c.e, c.n, c.fold, c.parity); got != c.ok {
			t.Errorf("pathWithin(%q,%q,fold=%v,parity=%v)=%v want %v", c.e, c.n, c.fold, c.parity, got, c.ok)
		}
	}
}
