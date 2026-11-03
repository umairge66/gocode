package pathguard

import "testing"

// withGOOS runs f with goos set and restores it afterwards (compiled
// patterns are cached per case mode, so no cache reset is needed).
func withGOOS(t *testing.T, os string, f func()) {
	t.Helper()
	old := goos
	goos = os
	t.Cleanup(func() { goos = old })
	f()
}

// Absolute rules on Windows match drive-letter paths, in the POSIX drive form
// and the drive-letter forms, without regard to case.
func TestWindowsAbsoluteRules(t *testing.T) {
	withGOOS(t, "windows", func() {
		var r PathRuleRoots
		for _, c := range []struct {
			rule, path  string
			allow, want bool
		}{
			{"//d/secret/**", `D:\secret\creds.txt`, false, true},
			{"//D:/secret/**", `D:\secret\creds.txt`, false, true},
			{`//D:\secret\**`, `D:\secret\creds.txt`, false, true},
			{"//d/secret/**", `d:\Secret\CREDS.txt`, false, true},
			{"//d/secret/**", `D:\secret`, false, true},
			{"//d/secret/**", `D:\secretive\x`, false, false},
			{"//c/secret/**", `D:\secret\x`, false, false},
			{"//**", `D:\anything\at\all`, false, true},
			{"//d/**/*.pem", `D:\work\keys\server.PEM`, false, true},
			{"//d/work/**", `D:\work\src\main.go`, true, true},
			{"//d/work/**", `E:\work\src\main.go`, true, false},
			{"//d/secret/**", `\\server\share\secret\x`, false, false},
			{"//d/secret/**", `D:secret\x`, false, false},
			{`D:\secret\creds.txt`, `D:\secret\creds.txt`, false, true},
			{`D:/secret/**`, `d:\SECRET\a`, false, true},
		} {
			if got := r.PathRuleMatches(c.rule, "projectSettings", c.path, c.allow); got != c.want {
				t.Errorf("%s vs %s (allow=%v): got %v, want %v", c.rule, c.path, c.allow, got, c.want)
			}
		}
	})
}

// Relative rules are matched without regard to case on Windows, so a deny
// rule cannot be stepped around by spelling a name differently.
func TestWindowsRulesIgnoreCase(t *testing.T) {
	r := PathRuleRoots{Cwd: "/proj"}
	withGOOS(t, "windows", func() {
		if !r.PathRuleMatches("./secret/**", "projectSettings", "/proj/Secret/key", false) {
			t.Error("Secret/key escaped a secret/** deny rule on Windows")
		}
		if !r.PathRuleMatches("*.env", "projectSettings", "/proj/APP.ENV", false) {
			t.Error("APP.ENV escaped a *.env deny rule on Windows")
		}
	})
	withGOOS(t, "linux", func() {
		if r.PathRuleMatches("./secret/**", "projectSettings", "/proj/Secret/key", false) {
			t.Error("rules must stay case-sensitive outside Windows")
		}
	})
}

func TestWindowsPOSIXPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\a`:    "/c/Users/a",
		`c:/Users/a/`:   "/c/Users/a",
		`D:\`:           "/d",
		`E:\\x\\\\y`:    "/e/x/y",
		`\\srv\share\x`: "",
		`C:rel\x`:       "",
		`x`:             "",
	} {
		got, ok := windowsPOSIXPath(in)
		if want == "" && ok || got != want {
			t.Errorf("%s: got %q ok=%v, want %q", in, got, ok, want)
		}
	}
}
