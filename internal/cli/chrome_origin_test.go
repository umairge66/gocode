package cli

import (
	"bytes"
	"strings"
	"testing"
)

// testEnv builds an Env wired for a native-host dispatch test: captured
// stdout/stderr, an immediately-EOF stdin so the host serve loop returns, and
// the func fields Run dereferences (Getenv/Setenv/Cwd).
func testEnv(out, errb *bytes.Buffer) *Env {
	return &Env{
		Getenv: func(string) string { return "" },
		Setenv: func(string, string) {},
		Stdout: out,
		Stderr: errb,
		Stdin:  strings.NewReader(""),
		Cwd:    func() (string, error) { return "/", nil },
	}
}

// TestChromeExtensionOriginRoutesToHost verifies the native-messaging launch
// path: Chrome starts the binary named by the host manifest with the extension
// ORIGIN as the first argument (not our --chrome-native-host flag), so a first
// token of "chrome-extension://<id>/" must enter host mode — NOT fall through
// to the interactive session. With an empty stdin (immediate EOF) the host
// serve loop returns cleanly and nothing is written to stdout (stdout is the
// native-messaging protocol channel).
func TestChromeExtensionOriginRoutesToHost(t *testing.T) {
	var out, errb bytes.Buffer
	env := testEnv(&out, &errb)

	code := Run(env, []string{"gocode", "chrome-extension://enlbkekddkndgijidajppabineoekbfh/"})

	if code != 0 {
		t.Errorf("exit code = %d, want 0 (clean EOF from the native-messaging host)", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty (stdout is the protocol channel, must not carry logs/banners/TUI)", out.String())
	}
}

// TestChromeExtensionOriginWithParentWindowRoutesToHost covers the Windows
// launch argv: Chrome runs `<host.exe> chrome-extension://<id>/
// --parent-window=<hwnd>` (the window handle is appended on Windows only). The
// trailing flag must not derail routing into host mode (no help/usage, no
// interactive session, no "unknown option"), and stdout must stay empty.
func TestChromeExtensionOriginWithParentWindowRoutesToHost(t *testing.T) {
	for _, argv := range [][]string{
		{`C:\Program Files\gocode\gocode.exe`, "chrome-extension://enlbkekddkndgijidajppabineoekbfh/", "--parent-window=0"},
		{`C:\Program Files\gocode\gocode.exe`, "chrome-extension://enlbkekddkndgijidajppabineoekbfh/", "--parent-window=132456"},
	} {
		var out, errb bytes.Buffer
		env := testEnv(&out, &errb)
		if code := Run(env, argv); code != 0 {
			t.Errorf("%v: exit code = %d, want 0 (stderr=%q)", argv, code, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout = %q, want empty (protocol channel)", argv, out.String())
		}
	}
}

// TestChromeNativeHostFlagStillRoutes keeps the explicit flag working for
// manual testing alongside the origin-argument path.
func TestChromeNativeHostFlagStillRoutes(t *testing.T) {
	var out, errb bytes.Buffer
	env := testEnv(&out, &errb)
	if code := Run(env, []string{"gocode", "--chrome-native-host"}); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}
