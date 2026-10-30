package updater

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// version.go — semver compare table + validity.
// -----------------------------------------------------------------------------

func TestCompareTable(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"2.1.306", "2.1.306", 0},
		{"2.1.307", "2.1.306", 1},
		{"2.1.306", "2.1.307", -1},
		{"2.2.0", "2.1.999", 1},
		{"3.0.0", "2.9.9", 1},
		{"v2.1.306", "2.1.306", 0},    // leading v tolerated
		{"1.0.0", "1.0.0+build.5", 0}, // build metadata ignored
		{"1.0.0-alpha", "1.0.0", -1},  // prerelease < release
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha", 1}, // more identifiers wins
		{"1.0.0-1", "1.0.0-alpha", -1},      // numeric < alphanumeric
		{"1.0.0-2", "1.0.0-10", -1},         // numeric compared numerically
	}
	for _, c := range cases {
		got, err := Compare(c.a, c.b)
		if err != nil {
			t.Fatalf("Compare(%q,%q) unexpected error: %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
		// Predicate cross-checks.
		if (got == 1) != GreaterThan(c.a, c.b) {
			t.Errorf("GreaterThan(%q,%q) inconsistent with Compare=%d", c.a, c.b, got)
		}
		if (got >= 0) != AtLeast(c.a, c.b) {
			t.Errorf("AtLeast(%q,%q) inconsistent with Compare=%d", c.a, c.b, got)
		}
		if (got == -1) != LessThan(c.a, c.b) {
			t.Errorf("LessThan(%q,%q) inconsistent with Compare=%d", c.a, c.b, got)
		}
	}
}

func TestIsValid(t *testing.T) {
	valid := []string{"1.0.0", "2.1.306", "v2.1.306", "1.0.0-rc.1", "1.0.0+build"}
	for _, s := range valid {
		if !IsValid(s) {
			t.Errorf("IsValid(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "1", "1.0", "1.0.0.0", "1.0.x", "latest", "01.0.0", "1.0.0-"}
	for _, s := range invalid {
		if IsValid(s) {
			t.Errorf("IsValid(%q) = true, want false", s)
		}
	}
	if _, err := Compare("1.0.0", "garbage"); err == nil {
		t.Error("Compare with invalid operand should error")
	}
}

// -----------------------------------------------------------------------------
// update.go — "update available" decision.
// -----------------------------------------------------------------------------

func TestDecideUpdate(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		target    string
		opts      DecideOptions
		wantAvail bool
	}{
		{"upgrade", "2.1.306", "2.1.307", DecideOptions{}, true},
		{"downgrade explicit", "2.1.306", "2.1.300", DecideOptions{AllowDowngrade: true}, true},
		{"downgrade not allowed", "2.1.306", "2.1.300", DecideOptions{}, false},
		{"same not installed", "2.1.306", "2.1.306", DecideOptions{}, true},
		{"same already installed", "2.1.306", "2.1.306", DecideOptions{AlreadyInstalled: true}, false},
		{"same installed but forced", "2.1.306", "2.1.306", DecideOptions{AlreadyInstalled: true, Force: true}, true},
		{"max version caps and skips", "2.1.306", "2.2.0", DecideOptions{MaxVersion: "2.1.306", AlreadyInstalled: true}, false},
		{"max version caps to allowed", "2.1.300", "2.2.0", DecideOptions{MaxVersion: "2.1.306"}, true},
		{"below minimum blocked", "2.1.306", "2.1.100", DecideOptions{MinimumVersion: "2.1.200"}, false},
		{"at minimum allowed", "2.1.306", "2.1.300", DecideOptions{MinimumVersion: "2.1.200", AllowDowngrade: true}, true},
		{"newer is upgrade", "1.2.3", "1.3.0-rc.1", DecideOptions{AlreadyInstalled: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := DecideUpdate(c.current, c.target, c.opts)
			if got != c.wantAvail {
				t.Errorf("DecideUpdate(%q,%q,%+v) = %v (%s), want %v",
					c.current, c.target, c.opts, got, reason, c.wantAvail)
			}
		})
	}
}

func TestAtomicReplaceStagedChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("real bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "bin", "gocode")
	// Expected checksum deliberately wrong so the staged re-verify fails.
	err := AtomicReplace(src, target, strings.Repeat("f", 64))
	var mism *StagedChecksumMismatchError
	if !errors.As(err, &mism) {
		t.Fatalf("expected StagedChecksumMismatchError, got %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Errorf("target should not have been created, stat err = %v", statErr)
	}
}

// -----------------------------------------------------------------------------
// restart.go — self-restart via the injected exec seam (no real exec).
// -----------------------------------------------------------------------------

func TestRestarterReexecsInstalledBinary(t *testing.T) {
	var gotArgv0 string
	var gotArgv, gotEnv []string
	called := false

	r := &Restarter{
		Exec: func(argv0 string, argv []string, env []string) error {
			called = true
			gotArgv0, gotArgv, gotEnv = argv0, argv, env
			return nil // a real exec would not return; the seam does
		},
		Args: []string{"/old/path/gocode", "update", "--now"},
		Env:  []string{"GOCODE_TEST=1"},
	}

	const installed = "/new/path/bin/gocode"
	if err := r.Restart(installed); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !called {
		t.Fatal("exec seam was not called")
	}
	if gotArgv0 != installed {
		t.Errorf("argv0 = %q, want %q", gotArgv0, installed)
	}
	// argv[0] replaced with the installed path; the rest of the command line
	// carried over verbatim.
	wantArgv := []string{installed, "update", "--now"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Errorf("argv = %v, want %v", gotArgv, wantArgv)
	}
	if !slices.Equal(gotEnv, []string{"GOCODE_TEST=1"}) {
		t.Errorf("env = %v, want [GOCODE_TEST=1]", gotEnv)
	}
}

// TestRestarterWindowsSpawnAndExit exercises the Windows handoff shape without a
// real Windows build: the injected exec seam stands in for restart_windows.go's
// defaultExec by capturing the spawn argv/env and returning errHandoffExit (the
// "spawn succeeded, now exit" signal), and the injected Exit seam observes that
// Restart exits the current process rather than returning. This runs on any
// host, so it is not build-guarded.
func TestRestarterWindowsSpawnAndExit(t *testing.T) {
	var gotArgv0 string
	var gotArgv, gotEnv []string
	spawned := false
	exited := false
	exitCode := -1

	r := &Restarter{
		Exec: func(argv0 string, argv []string, env []string) error {
			spawned = true
			gotArgv0, gotArgv, gotEnv = argv0, argv, env
			// Mirror the Windows defaultExec: a detached child was spawned, so
			// signal the caller to exit instead of replacing the process image.
			return errHandoffExit
		},
		Exit: func(code int) {
			exited = true
			exitCode = code
		},
		Args: []string{`C:\old\gocode.exe`, "update", "--now"},
		Env:  []string{"GOCODE_TEST=1"},
	}

	const installed = `C:\Program Files\gocode\bin\gocode.exe`
	if err := r.Restart(installed); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !spawned {
		t.Fatal("spawn seam was not invoked")
	}
	if gotArgv0 != installed {
		t.Errorf("spawn argv0 = %q, want installed binary %q", gotArgv0, installed)
	}
	// argv[0] is replaced with the installed path; the rest of the command line
	// carries over verbatim.
	wantArgv := []string{installed, "update", "--now"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Errorf("spawn argv = %v, want %v", gotArgv, wantArgv)
	}
	if !slices.Equal(gotEnv, []string{"GOCODE_TEST=1"}) {
		t.Errorf("spawn env = %v, want [GOCODE_TEST=1]", gotEnv)
	}
	// The "then exit" signal: after a detached spawn the current process must
	// exit so the new one takes over, rather than Restart returning normally.
	if !exited {
		t.Fatal("exit seam was not called — Restart must exit after a detached-spawn handoff")
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
}
