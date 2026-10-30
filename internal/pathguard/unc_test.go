package pathguard

import "testing"

// TestIsUNCPathSpelling covers every detector branch, in both its plain
// and argument forms.
func TestIsUNCPathSpelling(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		plain   bool // IsUNCPathSpelling(in, false)
		argForm bool // IsUNCPathSpelling(in, true)
	}{
		// Leading pairs.
		{"leading backslash pair", `\\host\share\x`, true, true},
		{"leading slash pair", `//host/share/x`, true, true},
		{"leading mixed pair arg-only", `/\host`, false, true}, // leading pair: /^[\\/]{2}/
		{"nt object prefix", `\??\C:\x`, true, true},
		{"nt object prefix fwd", `/??/C:/x`, true, true},
		// Embedded spellings.
		{"embedded backslash host", `C:\a\\host\share`, true, true},
		{"embedded after NAME=", `NAME=\\host\share`, true, true},
		{"embedded slash host after NAME=", `NAME=//host/share`, true, true},
		{"embedded slash host mid-path", `C:/a//host/share`, true, true},
		{"url is not a share", `https://example.com/x`, false, false},
		{"nt object prefix after =", `x=\??\C:\x`, true, true},
		{"nt object prefix after word char is not Zf", `a\??\b`, false, false},
		// @SSL / @port / WebDAV.
		{"host@SSL", `\\host@SSL\share`, true, true},
		{"host@port", `\\host@8080\share`, true, true},
		{"slash host@ssl", `//host@ssl/x`, true, true},
		{"@SSL@port anywhere", `file@SSL@443`, true, true},
		{"@port@SSL anywhere", `file@443@ssl`, true, true},
		{"DavWWWRoot", `C:\x\davwwwroot\y`, true, true},
		// IP literals.
		{"ipv4 backslash", `\\192.168.0.1\c$`, true, true},
		{"ipv6 slash", `//[fe80::1]/x`, true, true},
		// Mixed slash runs.
		{"slash + 2 backslashes", `x/\\host`, true, true},
		// The argument form swaps in the strict patterns, which need a
		// non-word lookbehind and a trailing separator.
		{"2 backslashes + slash", `x\\/host`, true, false},
		{"slash backslash host sep (strict)", ` /\host\x`, false, true},
		{"backslash slash host sep (strict)", ` \/host\x`, false, true},
		{"strict needs non-word lookbehind", `a/\host\x`, false, false},
		{"strict needs trailing separator", ` /\host`, false, false},
		// -flag / --name= prefixes (argument form only).
		{"-flag cluster", `-o/\host\share\x`, false, true},
		{"-flag multi cluster", `-a-b\/host`, false, true},
		{"--name= prefix", `--include=\/host`, false, true},
		{"-n= prefix chain", `-I=--x=\/host`, false, true},
		{"-flag with embedded share", `-I\\host\share`, true, true},
		// Ordinary paths.
		{"drive path", `C:\Users\me\file.txt`, false, false},
		{"posix path", `/home/me/file.txt`, false, false},
		{"relative", `src/main.go`, false, false},
		{"empty", ``, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsUNCPathSpelling(c.in, false); got != c.plain {
				t.Errorf("IsUNCPathSpelling(%q,false)=%v want %v", c.in, got, c.plain)
			}
			if got := IsUNCPathSpelling(c.in, true); got != c.argForm {
				t.Errorf("IsUNCPathSpelling(%q,true)=%v want %v", c.in, got, c.argForm)
			}
		})
	}
}
