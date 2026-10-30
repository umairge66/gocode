package exec

import (
	"strings"
	"testing"
)

func TestShadowGrepOnlyPOSIXShells(t *testing.T) {
	cmd := "grep -n x f"
	for _, tc := range []struct {
		inv     ShellInvocation
		want    bool
		keyword bool
	}{
		{ShellInvocation{Path: "/bin/sh", Args: []string{"-c", cmd}}, true, false},
		{ShellInvocation{Path: "/usr/bin/dash", Args: []string{"-c", cmd}}, true, false},
		{ShellInvocation{Path: "/bin/bash", Args: []string{"-c", cmd}}, true, true},
		{ShellInvocation{Path: "/usr/bin/zsh", Args: []string{"-c", cmd}}, true, true},
		{ShellInvocation{Path: `C:\Program Files\Git\bin\bash.exe`, Args: []string{"-c", "-l", cmd}}, true, true},
		{ShellInvocation{Path: "pwsh", Args: []string{"-Command", cmd}, PowerShell: true}, false, false},
		{ShellInvocation{Path: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, Args: []string{"-Command", cmd}}, false, false},
		{ShellInvocation{Path: "cmd.exe", Args: []string{"/c", cmd}}, false, false},
	} {
		got := ShadowGrep(tc.inv, "/opt/ugrep")
		last := got.Args[len(got.Args)-1]
		if shadowed := last != cmd; shadowed != tc.want {
			t.Errorf("%s: shadowed=%v, want %v", tc.inv.Path, shadowed, tc.want)
			continue
		}
		if !tc.want {
			continue
		}
		if !strings.HasSuffix(last, "; "+cmd) || strings.Contains(last, "\n") {
			t.Errorf("%s: the definition must share the command's first line: %q", tc.inv.Path, last)
		}
		if strings.Contains(last, "function grep {") != tc.keyword {
			t.Errorf("%s: keyword form = %v, want %v", tc.inv.Path, !tc.keyword, tc.keyword)
		}
		if tc.inv.Args[len(tc.inv.Args)-1] != cmd {
			t.Errorf("%s: the caller's argv was modified", tc.inv.Path)
		}
	}
	if inv := ShadowGrep(ShellInvocation{Path: "/bin/sh", Args: []string{"-c", cmd}}, ""); inv.Args[1] != cmd {
		t.Error("no ugrep path must leave the command alone")
	}
}

func TestGrepShadowWindowsPath(t *testing.T) {
	got := GrepShadowPrefix(`C:\Program Files\Git\bin\bash.exe`, `C:\Users\Me\AppData\Local\gocode\ugrep\7.8.5\ugrep.exe`)
	if !strings.Contains(got, "'/c/Users/Me/AppData/Local/gocode/ugrep/7.8.5/ugrep.exe' -G --ignore-files") {
		t.Fatalf("Git Bash needs the /c/... form: %s", got)
	}
	if p := msysPath(`\\host\share\ugrep.exe`); p != "//host/share/ugrep.exe" {
		t.Fatalf("UNC path = %q", p)
	}
}

func TestGrepShadowQuotesPath(t *testing.T) {
	got := GrepShadowFunction("/tmp/it's here/ugrep", false)
	if !strings.Contains(got, `'/tmp/it'\''s here/ugrep' -G`) {
		t.Fatalf("path not quoted: %s", got)
	}
	for _, p := range GrepShadowPassThrough {
		if !strings.Contains(got, p) {
			t.Fatalf("pass-through pattern %s missing", p)
		}
	}
}
