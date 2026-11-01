package cli

import (
	"bytes"
	"context"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWorktreeDerivations(t *testing.T) {
	cases := []struct {
		name       string
		wantSlug   string
		wantBranch string
		wantPath   string
		wantSess   string // for repoRoot "/home/u/myrepo"
	}{
		{"feature", "feature", "worktree-feature", "/home/u/myrepo/.gocode/worktrees/feature", "myrepo_worktree-feature"},
		{"team/api", "team+api", "worktree-team+api", "/home/u/myrepo/.gocode/worktrees/team+api", "myrepo_worktree-team+api"},
		{"v1.2", "v1.2", "worktree-v1.2", "/home/u/myrepo/.gocode/worktrees/v1.2", "myrepo_worktree-v1_2"},
	}
	for _, c := range cases {
		if got := worktreeSlug(c.name); got != c.wantSlug {
			t.Errorf("worktreeSlug(%q)=%q want %q", c.name, got, c.wantSlug)
		}
		if got := worktreeBranchName(c.name); got != c.wantBranch {
			t.Errorf("worktreeBranchName(%q)=%q want %q", c.name, got, c.wantBranch)
		}
		if got := worktreePath("/home/u/myrepo", c.name); got != filepath.FromSlash(c.wantPath) {
			t.Errorf("worktreePath(%q)=%q want %q", c.name, got, c.wantPath)
		}
		if got := generateTmuxSessionName("/home/u/myrepo", worktreeBranchName(c.name)); got != c.wantSess {
			t.Errorf("generateTmuxSessionName(%q)=%q want %q", c.name, got, c.wantSess)
		}
	}
}

func TestValidateWorktreeName(t *testing.T) {
	ok := []string{"feature", "team/api", "v1.2", "a_b-c", "pr-42"}
	for _, n := range ok {
		if msg := validateWorktreeName(n); msg != "" {
			t.Errorf("validateWorktreeName(%q) unexpected error: %s", n, msg)
		}
	}
	bad := map[string]string{
		strings.Repeat("x", 65): "64 characters or fewer",
		"a/./b":                 `must not contain "." or ".."`,
		"a/../b":                `must not contain "." or ".."`,
		".git":                  "reserved git directory name",
		"GIT.":                  "", // ".git"? no -> "git." lowercased trimmed dots = "git" not ".git"; valid
		"bad name":              "only letters, digits",
		"weird$":                "only letters, digits",
	}
	for n, want := range bad {
		msg := validateWorktreeName(n)
		if want == "" {
			if msg != "" {
				t.Errorf("validateWorktreeName(%q) unexpected error: %s", n, msg)
			}
			continue
		}
		if !strings.Contains(msg, want) {
			t.Errorf("validateWorktreeName(%q)=%q want substring %q", n, msg, want)
		}
	}
	// A reserved .git spelling with trailing dots.
	if msg := validateWorktreeName(".git."); !strings.Contains(msg, "reserved git directory name") {
		t.Errorf("validateWorktreeName(.git.) = %q, want reserved", msg)
	}
}

func TestParsePRReference(t *testing.T) {
	cases := []struct {
		in     string
		num    int
		forge  string
		wantOK bool
	}{
		{"https://github.com/o/r/pull/123", 123, "github", true},
		{"https://github.com/o/r/pull/123#issue", 123, "github", true},
		{"https://gitlab.com/g/s/p/-/merge_requests/7", 7, "gitlab", true},
		{"#42", 42, "", true},
		{"feature", 0, "", false},
		{"https://example.com/x", 0, "", false},
	}
	for _, c := range cases {
		ref, ok := parsePRReference(c.in)
		if ok != c.wantOK {
			t.Errorf("parsePRReference(%q) ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (ref.Number != c.num || ref.Forge != c.forge) {
			t.Errorf("parsePRReference(%q)=%+v want {%d %q}", c.in, ref, c.num, c.forge)
		}
	}
}

func TestParseTmuxWorktreeArgs(t *testing.T) {
	cases := []struct {
		args    []string
		name    string
		classic bool
	}{
		{[]string{"--tmux", "--worktree", "feature"}, "feature", false},
		{[]string{"--tmux", "-w", "api"}, "api", false},
		{[]string{"--tmux=classic", "--worktree=team/api"}, "team/api", true},
		{[]string{"--tmux", "--worktree"}, "", false},                 // no value
		{[]string{"--tmux", "--worktree", "--model", "x"}, "", false}, // value is a flag
		{[]string{"--tmux", "--", "--worktree", "x"}, "", false},      // after terminator
	}
	for _, c := range cases {
		n, cl := parseTmuxWorktreeArgs(c.args)
		if n != c.name || cl != c.classic {
			t.Errorf("parseTmuxWorktreeArgs(%v)=(%q,%v) want (%q,%v)", c.args, n, cl, c.name, c.classic)
		}
	}
}

func TestTmuxSupportsEnvFlags(t *testing.T) {
	yes := []string{"tmux 3.2", "tmux 3.4", "tmux next-3.5", "tmux 4.0"}
	no := []string{"tmux 3.1a", "tmux 2.9", "tmux 3.0"}
	for _, v := range yes {
		if !tmuxSupportsEnvFlags(v) {
			t.Errorf("tmuxSupportsEnvFlags(%q)=false want true", v)
		}
	}
	for _, v := range no {
		if tmuxSupportsEnvFlags(v) {
			t.Errorf("tmuxSupportsEnvFlags(%q)=true want false", v)
		}
	}
}

func TestTmuxEnvFlags(t *testing.T) {
	got := tmuxEnvFlags([]kv{{"A", "1"}, {"B", "x;"}})
	want := []string{"-e", "A=1", "-e", `B=x\;`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tmuxEnvFlags = %v want %v", got, want)
	}
}

func TestBuildTmuxCommands(t *testing.T) {
	inv := tmuxInvocation{
		Session:      "repo_worktree-feature",
		WorktreePath: "/repo/.gocode/worktrees/feature",
		EnvFlags:     []string{"-e", "GOCODE_CODE_TMUX_SESSION=repo_worktree-feature"},
		Exec:         []string{"/bin/gocode"},
	}

	// Not inside tmux: single foreground new-session -A.
	inv.InsideTmux = false
	cmds := buildTmuxCommands(inv)
	if len(cmds) != 1 || !cmds[0].Foreground {
		t.Fatalf("outside-tmux plan = %+v", cmds)
	}
	want := []string{"new-session", "-A", "-s", "repo_worktree-feature",
		"-e", "GOCODE_CODE_TMUX_SESSION=repo_worktree-feature",
		"-c", "/repo/.gocode/worktrees/feature", "--", "/bin/gocode"}
	if !reflect.DeepEqual(cmds[0].Args, want) {
		t.Fatalf("outside-tmux argv = %v want %v", cmds[0].Args, want)
	}

	// Inside tmux, session exists: switch-client only.
	inv.InsideTmux = true
	inv.HasSession = true
	cmds = buildTmuxCommands(inv)
	if len(cmds) != 1 || !cmds[0].Foreground ||
		!reflect.DeepEqual(cmds[0].Args, []string{"switch-client", "-t", "repo_worktree-feature"}) {
		t.Fatalf("inside+has plan = %+v", cmds)
	}

	// Inside tmux, no session: detached new-session -d then switch-client.
	inv.HasSession = false
	cmds = buildTmuxCommands(inv)
	if len(cmds) != 2 || cmds[0].Foreground || !cmds[1].Foreground {
		t.Fatalf("inside+nosession plan = %+v", cmds)
	}
	if cmds[0].Args[0] != "new-session" || cmds[0].Args[1] != "-d" {
		t.Fatalf("inside+nosession detached argv = %v", cmds[0].Args)
	}
}

func TestRandomWorktreeNameDeterministic(t *testing.T) {
	n := randomWorktreeName(rand.New(rand.NewSource(1)))
	if validateWorktreeName(n) != "" {
		t.Fatalf("generated name %q failed validation", n)
	}
	parts := strings.Split(n, "-")
	if len(parts) != 3 {
		t.Fatalf("generated name %q not adj-noun-suffix", n)
	}
	// Same seed -> same name.
	if n2 := randomWorktreeName(rand.New(rand.NewSource(1))); n2 != n {
		t.Fatalf("non-deterministic: %q vs %q", n, n2)
	}
}

// --- exec-seam tests -------------------------------------------------------

type fakeRunner struct {
	lookErr    map[string]error
	results    map[string]cmdResult // keyed by first two argv tokens, e.g. "tmux -V"
	foreground [][]string
	captures   [][]string
}

func key(name string, args []string) string {
	k := name
	if len(args) > 0 {
		k += " " + args[0]
	}
	if len(args) > 1 {
		k += " " + args[1]
	}
	return k
}

func (f *fakeRunner) lookPath(name string) (string, error) {
	if f.lookErr != nil {
		if err, ok := f.lookErr[name]; ok {
			return "", err
		}
	}
	return "/usr/bin/" + name, nil
}

func (f *fakeRunner) capture(ctx context.Context, name string, args []string, dir string, env []string) (cmdResult, error) {
	f.captures = append(f.captures, append([]string{name}, args...))
	if f.results != nil {
		if r, ok := f.results[key(name, args)]; ok {
			return r, nil
		}
	}
	return cmdResult{Code: 0}, nil
}

func (f *fakeRunner) runForeground(ctx context.Context, name string, args []string, dir string, env []string) (int, error) {
	f.foreground = append(f.foreground, append([]string{name}, args...))
	return 0, nil
}

func newTmuxTestEnv() *Env {
	return &Env{
		Getenv: func(string) string { return "" },
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Cwd:    func() (string, error) { return "/home/u/myrepo", nil },
	}
}

func TestExecIntoTmuxWorktreeHappyPath(t *testing.T) {
	fr := &fakeRunner{results: map[string]cmdResult{
		"tmux -V":           {Code: 0, Stdout: "tmux 3.4"},
		"tmux show-options": {Code: 0, Stdout: "prefix C-a"},
		"tmux has-session":  {Code: 1}, // no existing session
	}}
	deps := tmuxDeps{
		runner:         fr,
		trusted:        func() bool { return true },
		gitRoot:        func(ctx context.Context, dir string) (string, bool) { return "/home/u/myrepo", true },
		createWorktree: func(ctx context.Context, repoRoot, path, branch string) error { return nil },
		rng:            rand.New(rand.NewSource(1)),
		insideTmux:     false,
		execPath:       "/bin/gocode",
		baseEnv:        nil,
	}
	res := execIntoTmuxWorktree(context.Background(), newTmuxTestEnv(),
		[]string{"--tmux", "--worktree", "feature"}, deps)
	if !res.handled || res.errMsg != "" {
		t.Fatalf("result = %+v", res)
	}
	if len(fr.foreground) != 1 {
		t.Fatalf("expected 1 foreground tmux call, got %v", fr.foreground)
	}
	fg := fr.foreground[0]
	wantPrefix := []string{"tmux", "new-session", "-A", "-s", "myrepo_worktree-feature"}
	if !reflect.DeepEqual(fg[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("foreground argv = %v want prefix %v", fg, wantPrefix)
	}
	// Env flags present (tmux 3.4), worktree path and exec after "--".
	joined := strings.Join(fg, " ")
	if !strings.Contains(joined, "-e GOCODE_CODE_TMUX_SESSION=myrepo_worktree-feature") {
		t.Errorf("missing session env flag: %v", fg)
	}
	if !strings.Contains(joined, "-c "+filepath.FromSlash("/home/u/myrepo/.gocode/worktrees/feature")+" -- /bin/gocode") {
		t.Errorf("missing -c/-- exec tail: %v", fg)
	}
}

func TestExecIntoTmuxWorktreeMissingTmux(t *testing.T) {
	fr := &fakeRunner{results: map[string]cmdResult{"tmux -V": {Code: 127}}}
	deps := tmuxDeps{
		runner:  fr,
		trusted: func() bool { return true },
		gitRoot: func(ctx context.Context, dir string) (string, bool) { return "/r", true },
		rng:     rand.New(rand.NewSource(1)),
	}
	res := execIntoTmuxWorktree(context.Background(), newTmuxTestEnv(),
		[]string{"--tmux", "--worktree", "feature"}, deps)
	if res.handled || !strings.Contains(res.errMsg, "tmux is not installed") {
		t.Fatalf("missing-tmux result = %+v", res)
	}
}

func TestExecIntoTmuxWorktreeWindows(t *testing.T) {
	deps := tmuxDeps{isWindows: true, runner: &fakeRunner{}, rng: rand.New(rand.NewSource(1))}
	res := execIntoTmuxWorktree(context.Background(), newTmuxTestEnv(),
		[]string{"--tmux", "--worktree", "x"}, deps)
	if res.handled || !strings.Contains(res.errMsg, "not natively available on Windows") {
		t.Fatalf("windows result = %+v", res)
	}
}

func TestExecIntoTmuxWorktreeNoGitRepo(t *testing.T) {
	fr := &fakeRunner{results: map[string]cmdResult{"tmux -V": {Code: 0, Stdout: "tmux 3.4"}}}
	deps := tmuxDeps{
		runner:  fr,
		trusted: func() bool { return true },
		gitRoot: func(ctx context.Context, dir string) (string, bool) { return "", false },
		rng:     rand.New(rand.NewSource(1)),
	}
	res := execIntoTmuxWorktree(context.Background(), newTmuxTestEnv(),
		[]string{"--tmux", "--worktree", "feature"}, deps)
	if res.handled || !strings.Contains(res.errMsg, "requires a git repository") {
		t.Fatalf("no-git result = %+v", res)
	}
}

func TestExecIntoTmuxWorktreeUntrusted(t *testing.T) {
	deps := tmuxDeps{
		runner:  &fakeRunner{},
		trusted: func() bool { return false },
		rng:     rand.New(rand.NewSource(1)),
	}
	res := execIntoTmuxWorktree(context.Background(), newTmuxTestEnv(),
		[]string{"--tmux", "--worktree", "x"}, deps)
	if res.handled || !strings.Contains(res.errMsg, "Workspace trust not yet accepted") {
		t.Fatalf("untrusted result = %+v", res)
	}
}
