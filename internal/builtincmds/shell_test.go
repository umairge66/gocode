package builtincmds

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	toolexec "github.com/umairge66/gocode/internal/tools/exec"
)

func fakeEnv(goos string, path map[string]string, env map[string]string) *toolexec.ShellEnv {
	return &toolexec.ShellEnv{
		GOOS: goos,
		LookPath: func(f string) (string, error) {
			if p, ok := path[f]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		IsFile:   func(string) bool { return false },
		Readlink: func(string) (string, error) { return "", errors.New("no") },
		Getenv:   func(k string) string { return env[k] },
	}
}

func TestShellInvocationPowerShellArgv(t *testing.T) {
	env := fakeEnv("windows", map[string]string{"pwsh": `C:\PS7\pwsh.exe`, "powershell": `C:\WPS\powershell.exe`}, nil)
	inv, err := shellInvocation("powershell", "Get-Date", env)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}, toolexec.WrapPowerShellScript("Get-Date"))
	if inv.Path != `C:\PS7\pwsh.exe` || !reflect.DeepEqual(inv.Args, want) {
		t.Fatalf("inv = %s %q", inv.Path, inv.Args)
	}
	// pwsh missing: Windows PowerShell 5.1.
	env = fakeEnv("windows", map[string]string{"powershell": `C:\WPS\powershell.exe`}, nil)
	if inv, _ = shellInvocation("pwsh", "x", env); inv.Path != `C:\WPS\powershell.exe` {
		t.Fatalf("fallback = %q", inv.Path)
	}
	// Neither: a clear error is spliced as the block's output.
	res := RunShellCommand(context.Background(), "x", RunOptions{Shell: "powershell", ShellEnv: fakeEnv("windows", nil, nil)})
	if res.ExitCode != -1 || !strings.Contains(res.Stderr, "no PowerShell executable") {
		t.Fatalf("res = %+v", res)
	}
}

func TestShellInvocationPOSIXDefaults(t *testing.T) {
	env := fakeEnv("linux", map[string]string{"bash": "/bin/bash", "pwsh": "/usr/bin/pwsh"}, nil)
	for _, shell := range []string{"", "bash", "fish", "powershell"} {
		inv, err := shellInvocation(shell, "x", env)
		if err != nil || inv.Path != "/bin/bash" || !reflect.DeepEqual(inv.Args, []string{"-c", "x"}) {
			t.Errorf("%q -> %s %q %v", shell, inv.Path, inv.Args, err)
		}
	}
}

func TestRunShellCommandHonorsZshAndSh(t *testing.T) {
	for _, sh := range []string{"zsh", "sh"} {
		if _, err := exec.LookPath(sh); err != nil {
			t.Logf("skip %s: not installed", sh)
			continue
		}
		res := RunShellCommand(context.Background(), `echo "$0"`, RunOptions{Shell: sh})
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, sh) {
			t.Errorf("%s: %+v", sh, res)
		}
	}
}
