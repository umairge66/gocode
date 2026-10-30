package exec

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// fakeHost is an injectable ShellEnv: a PATH table, a set of existing files,
// symlinks and environment variables.
type fakeHost struct {
	goos  string
	path  map[string]string
	files map[string]bool
	links map[string]string
	env   map[string]string
}

func (h fakeHost) shellEnv() *ShellEnv {
	return &ShellEnv{
		GOOS: h.goos,
		LookPath: func(f string) (string, error) {
			if p, ok := h.path[f]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		IsFile: func(p string) bool { return h.files[p] },
		Readlink: func(p string) (string, error) {
			if t, ok := h.links[p]; ok {
				return t, nil
			}
			return "", errors.New("not a link")
		},
		Getenv: func(k string) string { return h.env[k] },
	}
}

func TestFindPowerShellPrefersPwsh(t *testing.T) {
	h := fakeHost{goos: "windows", path: map[string]string{
		"pwsh":       `C:\Program Files\PowerShell\7\pwsh.exe`,
		"powershell": `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
	}}
	got, err := FindPowerShell(*h.shellEnv())
	if err != nil || got != `C:\Program Files\PowerShell\7\pwsh.exe` {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindPowerShellWindowsFallbackPaths(t *testing.T) {
	// Not on PATH: PowerShell 7 under %ProgramFiles%.
	h := fakeHost{goos: "windows", env: map[string]string{"ProgramFiles": `D:\PF`},
		files: map[string]bool{`D:\PF\PowerShell\7\pwsh.exe`: true}}
	if got, _ := FindPowerShell(*h.shellEnv()); got != `D:\PF\PowerShell\7\pwsh.exe` {
		t.Fatalf("ProgramFiles: got %q", got)
	}
	// Store install: the WindowsApps alias is followed.
	h = fakeHost{goos: "windows", env: map[string]string{"LOCALAPPDATA": `C:\U\AppData\Local`},
		links: map[string]string{`C:\U\AppData\Local\Microsoft\WindowsApps\pwsh.exe`: `C:\Store\pwsh.exe`},
		files: map[string]bool{`C:\Store\pwsh.exe`: true}}
	if got, _ := FindPowerShell(*h.shellEnv()); got != `C:\Store\pwsh.exe` {
		t.Fatalf("store alias: got %q", got)
	}
	// Only Windows PowerShell 5.1 on PATH.
	h = fakeHost{goos: "windows", path: map[string]string{"powershell": `C:\WPS\powershell.exe`}}
	if got, _ := FindPowerShell(*h.shellEnv()); got != `C:\WPS\powershell.exe` {
		t.Fatalf("powershell.exe fallback: got %q", got)
	}
	// Not on PATH: %SYSTEMROOT% default location.
	h = fakeHost{goos: "windows", files: map[string]bool{`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`: true}}
	if got, _ := FindPowerShell(*h.shellEnv()); got != `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe` {
		t.Fatalf("SYSTEMROOT fallback: got %q", got)
	}
	// Neither.
	h = fakeHost{goos: "windows"}
	if _, err := FindPowerShell(*h.shellEnv()); !errors.Is(err, ErrNoPowerShell) {
		t.Fatalf("err = %v, want ErrNoPowerShell", err)
	}
}

func TestFindPowerShellSnapWorkaround(t *testing.T) {
	h := fakeHost{goos: "linux", path: map[string]string{"pwsh": "/snap/bin/pwsh"},
		files: map[string]bool{"/opt/microsoft/powershell/7/pwsh": true}}
	if got, _ := FindPowerShell(*h.shellEnv()); got != "/opt/microsoft/powershell/7/pwsh" {
		t.Fatalf("got %q", got)
	}
}

func TestFindGitBash(t *testing.T) {
	h := fakeHost{goos: "windows", env: map[string]string{"GOCODE_CODE_GIT_BASH_PATH": `E:\git\bin\bash.exe`},
		files: map[string]bool{`E:\git\bin\bash.exe`: true}}
	if got := FindGitBash(*h.shellEnv()); got != `E:\git\bin\bash.exe` {
		t.Fatalf("override: %q", got)
	}
	h = fakeHost{goos: "windows", path: map[string]string{"git": `F:\Git\cmd\git.exe`},
		files: map[string]bool{`F:\Git\bin\bash.exe`: true}}
	if got := FindGitBash(*h.shellEnv()); got != `F:\Git\bin\bash.exe` {
		t.Fatalf("from git on PATH: %q", got)
	}
	if got := FindGitBash(*fakeHost{goos: "linux"}.shellEnv()); got != "" {
		t.Fatalf("linux: %q", got)
	}
}

func TestPowerShellArgv(t *testing.T) {
	env := *fakeHost{goos: "windows"}.shellEnv()
	args := PowerShellArgs("Get-ChildItem", env)
	want := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", powerShellPrelude + "Get-ChildItem" + powerShellEpilogue}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("argv = %q", args)
	}
	env = *fakeHost{goos: "windows", env: map[string]string{"GOCODE_CODE_POWERSHELL_RESPECT_EXECUTION_POLICY": "1"}}.shellEnv()
	if got := PowerShellFlags(env); !reflect.DeepEqual(got, []string{"-NoProfile", "-NonInteractive"}) {
		t.Fatalf("flags = %q", got)
	}
	// Scripts that must start bare (param/using/attribute) skip the prelude.
	if s := WrapPowerShellScript("# c\nparam($x)\n$x"); strings.HasPrefix(s, "try {") {
		t.Fatalf("param script got the prelude: %q", s)
	}
	if s := WrapPowerShellScript("[Console]::WriteLine(1)"); !strings.HasPrefix(s, "try {") {
		t.Fatalf("static call should keep the prelude: %q", s)
	}
}

func TestResolveShellWindows(t *testing.T) {
	withBash := fakeHost{goos: "windows", files: map[string]bool{`C:\Program Files\Git\bin\bash.exe`: true},
		path: map[string]string{"pwsh": `C:\PS7\pwsh.exe`}}
	inv, err := ResolveShell("", "ls", *withBash.shellEnv())
	if err != nil || inv.Path != `C:\Program Files\Git\bin\bash.exe` || !reflect.DeepEqual(inv.Args, []string{"-c", "-l", "ls"}) {
		t.Fatalf("default with Git Bash: %+v %v", inv, err)
	}
	inv, err = ResolveShell("PowerShell", "ls", *withBash.shellEnv())
	if err != nil || inv.Path != `C:\PS7\pwsh.exe` || !inv.PowerShell || inv.Args[len(inv.Args)-2] != "-Command" {
		t.Fatalf("shell: powershell: %+v %v", inv, err)
	}
	if inv.Env["PYTHONIOENCODING"] != "utf-8:surrogateescape" || inv.Env["NO_COLOR"] != "1" {
		t.Fatalf("env overrides: %v", inv.Env)
	}

	noBash := fakeHost{goos: "windows", path: map[string]string{"powershell": `C:\WPS\powershell.exe`}}
	inv, err = ResolveShell("", "dir", *noBash.shellEnv())
	if err != nil || inv.Path != `C:\WPS\powershell.exe` || inv.Notice != NoBashNotice {
		t.Fatalf("no Git Bash fallback: %+v %v", inv, err)
	}
	if _, err = ResolveShell("bash", "ls", *noBash.shellEnv()); err == nil || !strings.Contains(err.Error(), "Git Bash was not found") {
		t.Fatalf("explicit bash without Git Bash: %v", err)
	}
	if _, err = ResolveShell("", "ls", *fakeHost{goos: "windows"}.shellEnv()); err == nil || !strings.Contains(err.Error(), "requires either Git for Windows") {
		t.Fatalf("neither shell: %v", err)
	}
	if _, err = ResolveShell("powershell", "ls", *fakeHost{goos: "windows", files: map[string]bool{`C:\Program Files\Git\bin\bash.exe`: true}}.shellEnv()); !errors.Is(err, ErrNoPowerShell) {
		t.Fatalf("powershell requested but missing: %v", err)
	}
}

func TestResolveShellPOSIX(t *testing.T) {
	h := fakeHost{goos: "linux", path: map[string]string{"bash": "/usr/bin/bash", "zsh": "/usr/bin/zsh", "pwsh": "/usr/bin/pwsh"}}
	cases := map[string][]string{
		"":     {"/bin/sh", "-c", "x"},
		"bash": {"/usr/bin/bash", "-c", "x"},
		"zsh":  {"/usr/bin/zsh", "-c", "x"},
		// PowerShell off Windows needs GOCODE_CODE_USE_POWERSHELL_TOOL.
		"powershell": {"/bin/sh", "-c", "x"},
	}
	for kind, want := range cases {
		inv, err := ResolveShell(kind, "x", *h.shellEnv())
		if err != nil {
			t.Fatalf("%q: %v", kind, err)
		}
		if got := append([]string{inv.Path}, inv.Args...); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: argv = %q, want %q", kind, got, want)
		}
	}
	h.env = map[string]string{"GOCODE_CODE_USE_POWERSHELL_TOOL": "1"}
	if inv, _ := ResolveShell("pwsh", "x", *h.shellEnv()); inv.Path != "/usr/bin/pwsh" || !inv.PowerShell {
		t.Fatalf("enabled pwsh: %+v", inv)
	}
	if _, err := ResolveShell("zsh", "x", *fakeHost{goos: "linux"}.shellEnv()); err == nil || !strings.Contains(err.Error(), "requires zsh") {
		t.Fatalf("missing zsh: %v", err)
	}
	if _, err := ResolveShell("fish", "x", *h.shellEnv()); err == nil {
		t.Fatal("fish should be rejected")
	}
}
