//go:build unix

package exec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/ugrep"
)

// testUgrep returns a ugrep this host can run — GOCODE_TEST_UGREP, ugrep on
// PATH, or the bundled binary — or skips.
func testUgrep(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("GOCODE_TEST_UGREP"); p != "" {
		return p
	}
	if p, err := exec.LookPath("ugrep"); err == nil {
		return p
	}
	if b := ugrep.Bundled(); b != nil {
		if p, err := ugrep.Extract(b, t.TempDir()); err == nil {
			if _, err := ugrep.ProbeVersion(p); err == nil {
				return p
			}
		}
	}
	t.Skip("no ugrep on this host (set GOCODE_TEST_UGREP to a ugrep binary)")
	return ""
}

// gnuGrepDir returns a PATH directory whose grep is GNU grep, or skips.
func gnuGrepDir(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		out, err := exec.Command(filepath.Join(dir, "grep"), "--version").Output()
		if err == nil && bytes.Contains(out, []byte("GNU grep")) {
			return dir
		}
	}
	t.Skip("GNU grep not found")
	return ""
}

// shellRun runs script with shell in dir, stdin from stdin, and a PATH that
// holds only pathDir and the basic system directories.
func shellRun(t *testing.T, shell, dir, pathDir, script, stdin string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(shell, "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + pathDir + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "LC_ALL=C"}
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("%s: %v", script, err)
	}
	return o.String(), e.String(), code
}

func shells(t *testing.T) []string {
	var out []string
	for _, s := range []string{"sh", "bash", "zsh", "dash"} {
		if p, err := exec.LookPath(s); err == nil {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Skip("no POSIX shell")
	}
	return out
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// normalizeGrepOutput makes the outputs of the two greps comparable where
// they legitimately differ: ugrep searches files in parallel, so lines of
// different files come in no fixed order (sorted here), and it prints a path
// reached through "." without the leading "./".
func normalizeGrepOutput(s string, sortLines bool) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "./")
	}
	if sortLines {
		sort.Strings(lines)
	}
	return strings.Join(lines, "\n")
}

// TestGrepShadowMatchesGNUGrep runs common grep command lines through the
// system GNU grep and through the shadowing function (ugrep) in every
// available shell, and requires the same output and exit status. The only
// differences allowed are the ones normalizeGrepOutput removes: the order of
// lines from different files and a leading "./". Error messages differ in
// wording ("ugrep: warning: x: No such file or directory"), so stderr is only
// compared for being empty or not.
func TestGrepShadowMatchesGNUGrep(t *testing.T) {
	ug := testUgrep(t)
	gnu := gnuGrepDir(t)
	root := writeTree(t, map[string]string{
		"a.txt":     "needle one\nhay\nNeedle two\nneedles\n",
		"b.txt":     "nothing\n",
		"sub/c.txt": "x needle y\nfoo-bar\nneedle\n",
		"sub/d.log": "needle log\n",
		"dash.txt":  "-v flag\nplain\n",
	})
	cases := []struct {
		cmd   string
		stdin string
		sort  bool
	}{
		{cmd: "grep -r needle .", sort: true},
		{cmd: "grep -R needle sub", sort: true},
		{cmd: "grep -r needle", sort: true},
		{cmd: "grep -rn needle sub", sort: true},
		{cmd: "grep -rl needle .", sort: true},
		{cmd: "grep -rc needle sub", sort: true},
		{cmd: "grep -E 'ne+dle|hay' a.txt"},
		{cmd: "grep -F 'foo-bar' sub/c.txt"},
		{cmd: "grep -F '.' a.txt"},
		{cmd: "grep -i needle a.txt"},
		{cmd: "grep -l needle a.txt b.txt sub/c.txt", sort: true},
		{cmd: "grep -L needle a.txt b.txt", sort: true},
		{cmd: "grep -c needle a.txt b.txt", sort: true},
		{cmd: "grep -o 'need[a-z]*' a.txt"},
		{cmd: "grep -n needle a.txt"},
		{cmd: "grep -w needle a.txt"},
		{cmd: "grep -v needle a.txt"},
		{cmd: "grep -x needle sub/c.txt"},
		{cmd: "grep -A1 hay a.txt"},
		{cmd: "grep -B1 hay a.txt"},
		{cmd: "grep -C1 two a.txt"},
		{cmd: "grep -r --include='*.log' needle .", sort: true},
		{cmd: "grep -r --exclude='*.log' needle sub", sort: true},
		{cmd: "grep -r --exclude-dir=sub needle .", sort: true},
		{cmd: "grep -e hay -e two a.txt"},
		{cmd: "grep -h needle a.txt sub/c.txt", sort: true},
		{cmd: "grep -H needle a.txt"},
		{cmd: "grep -m1 needle a.txt"},
		{cmd: "grep -q needle a.txt"},
		{cmd: "grep -n 'ne\\(e\\)dle' a.txt"},
		{cmd: "grep 'a\\|b' b.txt"},
		{cmd: "grep '^n' a.txt"},
		{cmd: "grep -e -v dash.txt"},
		{cmd: "grep -- -v dash.txt"},
		{cmd: "printf 'a\\nx\\n' | grep x"},
		{cmd: "grep x", stdin: "a\nx\ny x\n"},
		{cmd: "grep -c x", stdin: "x\nx\n"},
		{cmd: "grep zzz a.txt"},        // exit 1
		{cmd: "grep needle nofile"},    // exit 2
		{cmd: "grep -s needle nofile"}, // exit 2, no message
		{cmd: "grep -E '(' a.txt"},     // exit 2, bad pattern
	}
	for _, sh := range shells(t) {
		prefix := GrepShadowPrefix(sh, ug)
		for _, tc := range cases {
			t.Run(filepath.Base(sh)+"/"+tc.cmd, func(t *testing.T) {
				wantOut, wantErr, wantCode := shellRun(t, sh, root, gnu, tc.cmd, tc.stdin)
				gotOut, gotErr, gotCode := shellRun(t, sh, root, gnu, prefix+tc.cmd, tc.stdin)
				if gotCode != wantCode {
					t.Fatalf("exit %d, GNU grep exits %d\nugrep: %q %q\ngnu:   %q %q", gotCode, wantCode, gotOut, gotErr, wantOut, wantErr)
				}
				if g, w := normalizeGrepOutput(gotOut, tc.sort), normalizeGrepOutput(wantOut, tc.sort); g != w {
					t.Fatalf("output differs\nugrep: %q\ngnu:   %q", g, w)
				}
				if (gotErr == "") != (wantErr == "") {
					t.Fatalf("stderr differs\nugrep: %q\ngnu:   %q", gotErr, wantErr)
				}
			})
		}
	}
}

// TestGrepShadowDefaults: unlike plain grep, the shadow skips .gitignore'd
// files, VCS metadata and binary files, and searches hidden files.
func TestGrepShadowDefaults(t *testing.T) {
	ug := testUgrep(t)
	gnu := gnuGrepDir(t)
	root := writeTree(t, map[string]string{
		".gitignore":        "ignored.txt\nbuild/\n",
		"ignored.txt":       "needle\n",
		"build/out.txt":     "needle\n",
		".git/config":       "needle\n",
		".hg/store":         "needle\n",
		"bin.dat":           "needle\x00binary\n",
		".hidden/found.txt": "needle\n",
		"kept.txt":          "needle\n",
	})
	sh := shells(t)[0]
	out, _, code := shellRun(t, sh, root, gnu, GrepShadowPrefix(sh, ug)+"grep -rl needle .", "")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := normalizeGrepOutput(out, true)
	if want := ".hidden/found.txt\nkept.txt"; got != want {
		t.Fatalf("grep -rl = %q, want %q", got, want)
	}
	// Plain GNU grep lists them all.
	plain, _, _ := shellRun(t, sh, root, gnu, "grep -rl needle .", "")
	if n := len(strings.Fields(plain)); n != 7 {
		t.Fatalf("GNU grep found %d files: %q", n, plain)
	}
}

// markerUgrep is a stand-in ugrep that records that it ran.
func markerUgrep(t *testing.T) (bin, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "ran")
	bin = filepath.Join(dir, "ugrep")
	script := "#!/bin/sh\necho \"$@\" > '" + marker + "'\necho ugrep-ran\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

// TestGrepShadowPassesThroughUnsafeOptions: the options that start pagers,
// viewers, filters or a TUI, touch config files, or mean something else in
// ugrep go to the system grep; GNU grep then either runs them with its own
// meaning (-Z, --null, -z) or rejects them with a clear message.
func TestGrepShadowPassesThroughUnsafeOptions(t *testing.T) {
	gnu := gnuGrepDir(t)
	root := writeTree(t, map[string]string{"a.txt": "needle\n"})
	for _, sh := range shells(t) {
		bin, marker := markerUgrep(t)
		prefix := GrepShadowPrefix(sh, bin)
		for _, args := range []string{
			"--pager=less needle a.txt", "--pager needle a.txt", "--view=vi needle a.txt",
			"--filter='sh:cat' needle a.txt", "--format-open='%f' needle a.txt",
			"--config=/tmp/x needle a.txt", "---x needle a.txt", "--save-config needle a.txt",
			"-@ needle a.txt", "-Z -l needle a.txt", "-lZ needle a.txt", "-z needle a.txt",
			"-rZl needle .", "--null -l needle a.txt", "--null-data needle a.txt",
		} {
			os.Remove(marker)
			out, errOut, code := shellRun(t, sh, root, gnu, prefix+"grep "+args, "")
			if _, err := os.Stat(marker); err == nil {
				t.Errorf("%s: grep %s reached ugrep", filepath.Base(sh), args)
			}
			if strings.Contains(out, "ugrep-ran") {
				t.Errorf("%s: grep %s ran ugrep", filepath.Base(sh), args)
			}
			switch {
			case strings.Contains(args, "Z") || strings.Contains(args, "--null") || strings.HasPrefix(args, "-z"):
				// GNU grep options: they must work as GNU grep's.
				if code > 1 {
					t.Errorf("%s: grep %s: exit %d %q", filepath.Base(sh), args, code, errOut)
				}
			default:
				if code != 2 || !strings.Contains(errOut, "grep:") {
					t.Errorf("%s: grep %s: want GNU grep's usage error, got exit %d %q", filepath.Base(sh), args, code, errOut)
				}
			}
		}
		// An ordinary command reaches ugrep with the default options first.
		os.Remove(marker)
		out, _, _ := shellRun(t, sh, root, gnu, prefix+"grep -n needle a.txt", "")
		got, err := os.ReadFile(marker)
		if err != nil || !strings.Contains(out, "ugrep-ran") {
			t.Fatalf("%s: grep did not run ugrep: %v %q", filepath.Base(sh), err, out)
		}
		if want := strings.Join(GrepShadowArgs, " ") + " -n needle a.txt\n"; string(got) != want {
			t.Fatalf("%s: ugrep argv = %q, want %q", filepath.Base(sh), got, want)
		}
	}
}

// TestGrepShadowFallsBackWhenUgrepMissing: without the binary, grep is the
// system grep, silently.
func TestGrepShadowFallsBackWhenUgrepMissing(t *testing.T) {
	gnu := gnuGrepDir(t)
	root := writeTree(t, map[string]string{"a.txt": "needle\nhay\n"})
	for _, sh := range shells(t) {
		prefix := GrepShadowPrefix(sh, filepath.Join(t.TempDir(), "missing", "ugrep"))
		out, errOut, code := shellRun(t, sh, root, gnu, prefix+"grep -n needle a.txt", "")
		if code != 0 || out != "1:needle\n" || errOut != "" {
			t.Fatalf("%s: fallback = %d %q %q", filepath.Base(sh), code, out, errOut)
		}
	}
}

// TestGrepShadowKeepsLineNumbers: the definition shares the command's first
// line, so shell errors still name the command's own line numbers.
func TestGrepShadowKeepsLineNumbers(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	root := t.TempDir()
	cmd := "true\nno_such_command_xyz"
	_, plain, _ := shellRun(t, bash, root, "/usr/bin", cmd, "")
	_, shadowed, _ := shellRun(t, bash, root, "/usr/bin", GrepShadowPrefix(bash, "/x/ugrep")+cmd, "")
	// bash 3.2 (macOS) numbers -c scripts differently; what matters is that
	// the shadow does not change the number.
	if plain != shadowed || !strings.Contains(plain, "no_such_command_xyz") {
		t.Fatalf("stderr changed: %q vs %q", plain, shadowed)
	}
}

// TestGrepShadowSurvivesAlias: an alias named grep in an expanding bash does
// not break the definition.
func TestGrepShadowSurvivesAlias(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	bin, marker := markerUgrep(t)
	root := writeTree(t, map[string]string{"a.txt": "needle\n"})
	script := "shopt -s expand_aliases\nalias grep='grep --color=auto'\n" + GrepShadowPrefix(bash, bin) + "grep needle a.txt"
	out, errOut, code := shellRun(t, bash, root, "/usr/bin", script, "")
	if code != 0 || !strings.Contains(out, "ugrep-ran") {
		t.Fatalf("alias broke the shadow: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
}

// TestBashToolShadowsGrep: the Bash tool runs `grep` as the configured ugrep,
// in the foreground and with no shadow when none is configured.
func TestBashToolShadowsGrep(t *testing.T) {
	bin, marker := markerUgrep(t)
	dir := writeTree(t, map[string]string{"a.txt": "needle\n"})
	res, err := BashTool{Dir: dir, UgrepPath: bin}.Execute(context.Background(), BashInput{Command: "grep -c needle a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "ugrep-ran" {
		t.Fatalf("Bash grep did not run ugrep: %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	os.Remove(marker)
	res, err = BashTool{Dir: dir}.Execute(context.Background(), BashInput{Command: "grep -c needle a.txt"})
	if err != nil || strings.TrimSpace(res.Stdout) != "1" {
		t.Fatalf("without ugrep, grep is the system grep: %v %+v", err, res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ugrep ran although none is configured")
	}
}
