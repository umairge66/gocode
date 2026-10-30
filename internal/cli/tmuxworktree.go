package cli

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// -----------------------------------------------------------------------------
// `gocode --tmux [--worktree [name]]` — create/resolve a git worktree and launch
// (or attach) a tmux session rooted in it running gocode.
//
//	worktree slug    name with "/" replaced by "+"         (worktreeSlug)
//	worktree branch  "worktree-<slug>"                      (worktreeBranchName)
//	worktree path    <root>/.gocode/worktrees/<slug>        (worktreePath)
//	session name     "<basename(root)>_<branch>", "/" and "." -> "_"
//	name check       validateWorktreeName (at most 64 characters)
//	PR references    parsePRReference
//
// The full git-worktree creation (PR/MR ref fetch, base-branch resolution,
// sparse-checkout, symlink-containment verification, the WorktreeCreate hook)
// and the workspace-trust gate are not implemented; they sit behind the
// injectable tmuxDeps seam so the derivation + argv assembly stays unit
// testable without spawning git or tmux. The default seam runs the core
// `git worktree add --no-track -B <branch> <path> HEAD^{commit}` argv
// (git/worktree.go WorktreeAddArgs) and treats every workspace as trusted.
//
// tmux is spawned with inherited stdio and the handler WAITS for it; the
// process is NOT replaced with exec(2). Foreground commands run and wait,
// then the handler returns.
// -----------------------------------------------------------------------------

// tmuxWorktreeResult is the {handled, error} result of execIntoTmuxWorktree.
type tmuxWorktreeResult struct {
	handled bool
	// errMsg is a user-facing message (already prefixed where needed, e.g.
	// "Error: ..."); empty when there is no error.
	errMsg string
}

// runTmuxWorktree is the dispatch leaf for `--tmux [--worktree]`. It runs the
// flow with the real exec/git seam and maps the result to an exit
// code: a message is printed to stderr and the process exits 1; a handled run
// (tmux attached and returned) exits 0.
func runTmuxWorktree(env *Env, t []string) int {
	res := execIntoTmuxWorktree(context.Background(), env, t, defaultTmuxDeps(env))
	if res.errMsg != "" {
		fmt.Fprintln(env.Stderr, res.errMsg)
		return 1
	}
	return 0
}

// -----------------------------------------------------------------------------
// Pure derivations (unit-testable without any process spawn).
// -----------------------------------------------------------------------------

// worktreeSlug maps a worktree name to its on-disk slug: "/" becomes "+".
func worktreeSlug(name string) string { return strings.ReplaceAll(name, "/", "+") }

// worktreeBranchName is the branch the worktree checks out: "worktree-"+slug.
func worktreeBranchName(name string) string { return "worktree-" + worktreeSlug(name) }

// worktreePath derives the managed worktree directory for a name under a repo
// root: <root>/.gocode/worktrees/<slug>.
func worktreePath(repoRoot, name string) string {
	return filepath.Join(repoRoot, ".gocode", "worktrees", worktreeSlug(name))
}

// tmuxSessionNameRe collapses "/" and "." in a derived session name to "_".
var tmuxSessionNameRe = regexp.MustCompile(`[/.]`)

// generateTmuxSessionName builds the tmux session name from the repo basename
// and the worktree branch: `${basename(root)}_${branch}` with "/" and "."
// replaced by "_".
func generateTmuxSessionName(repoRoot, branch string) string {
	return tmuxSessionNameRe.ReplaceAllString(filepath.Base(repoRoot)+"_"+branch, "_")
}

// worktreeNameMaxLen / worktreeSegmentRe are validateWorktreeName's limits.
const worktreeNameMaxLen = 64

var worktreeSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validateWorktreeName returns a user-facing error string
// (empty when the name is valid). A name must be <=64 chars, and each
// "/"-separated segment must not be "." / ".." / a reserved ".git" spelling and
// must match [a-zA-Z0-9._-]+.
func validateWorktreeName(name string) string {
	if utf8.RuneCountInString(name) > worktreeNameMaxLen {
		return fmt.Sprintf("Invalid worktree name: must be %d characters or fewer (got %d)",
			worktreeNameMaxLen, utf8.RuneCountInString(name))
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return fmt.Sprintf("Invalid worktree name %q: must not contain \".\" or \"..\" path segments", name)
		}
		if strings.TrimRight(strings.ToLower(seg), ".") == ".git" {
			return fmt.Sprintf("Invalid worktree name %q: %q is a reserved git directory name", name, seg)
		}
		if !worktreeSegmentRe.MatchString(seg) {
			return fmt.Sprintf("Invalid worktree name %q: each \"/\"-separated segment must be non-empty and contain only letters, digits, dots, underscores, and dashes", name)
		}
	}
	return ""
}

// prReference is a parsed PR/MR reference.
type prReference struct {
	Number int
	Forge  string // "github", "gitlab", or "" for a bare #<n>
}

var (
	prGitHubURLRe = regexp.MustCompile(`(?i)^https?://[^/]+/[^/]+/[^/]+/pull/(\d+)/?([?#].*)?$`)
	prGitLabURLRe = regexp.MustCompile(`(?i)^https?://[^/]+/[^?#]+/-/merge_requests/(\d+)/?([?#].*)?$`)
	prHashRe      = regexp.MustCompile(`^#(\d+)$`)
)

// parsePRReference parses a GitHub pull URL, a GitLab merge-request
// URL, or a bare "#<n>". It returns (ref, true) on a match.
func parsePRReference(s string) (prReference, bool) {
	if m := prGitHubURLRe.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return prReference{Number: n, Forge: "github"}, true
		}
	}
	if m := prGitLabURLRe.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return prReference{Number: n, Forge: "gitlab"}, true
		}
	}
	if m := prHashRe.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return prReference{Number: n, Forge: ""}, true
		}
	}
	return prReference{}, false
}

// parseTmuxWorktreeArgs scans the argv for the worktree name (from
// `-w`/`--worktree <name>` or `--worktree=<name>`) and whether `--tmux=classic`
// was requested, stopping at a "--" terminator. The name is empty when
// `--worktree` was given with no value.
func parseTmuxWorktreeArgs(args []string) (name string, classic bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" {
			continue
		}
		if a == "--" {
			break
		}
		switch {
		case a == "-w" || a == "--worktree":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				name = args[i+1]
			}
		case strings.HasPrefix(a, "--worktree="):
			name = strings.TrimPrefix(a, "--worktree=")
		case a == "--tmux=classic":
			classic = true
		}
	}
	return name, classic
}

// tmuxNameAdjectives / tmuxNameNouns are the word lists for a generated name.
var (
	tmuxNameAdjectives = []string{"swift", "bright", "calm", "keen", "bold"}
	tmuxNameNouns      = []string{"fox", "owl", "elm", "oak", "ray"}
)

// randomWorktreeName builds `${adj}-${noun}-${suffix}` where suffix is 4 base36
// characters. rng is injected so the derivation is deterministic in tests.
func randomWorktreeName(rng *rand.Rand) string {
	adj := tmuxNameAdjectives[rng.Intn(len(tmuxNameAdjectives))]
	noun := tmuxNameNouns[rng.Intn(len(tmuxNameNouns))]
	return adj + "-" + noun + "-" + base36Suffix(rng, 4)
}

const base36Alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

func base36Suffix(rng *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(base36Alphabet[rng.Intn(len(base36Alphabet))])
	}
	return b.String()
}

// tmuxPrefixConflicts is the set of prefix bindings that collide with gocode's
// own key handling.
var tmuxPrefixConflicts = map[string]struct{}{
	"C-b": {}, "C-c": {}, "C-d": {}, "C-t": {}, "C-o": {}, "C-r": {}, "C-s": {}, "C-g": {}, "C-e": {},
}

// tmuxVersionRe extracts major.minor from `tmux -V` output.
var tmuxVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// tmuxSupportsEnvFlags reports whether the tmux version string supports
// per-session `-e NAME=VALUE` flags on new-session (tmux >= 3.2).
func tmuxSupportsEnvFlags(versionOutput string) bool {
	m := tmuxVersionRe.FindStringSubmatch(versionOutput)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 3 || (major == 3 && minor >= 2)
}

// tmuxPrefixFromShowOptions extracts the prefix binding from `show-options -g
// prefix` output, defaulting to "C-b".
var tmuxPrefixRe = regexp.MustCompile(`prefix\s+(\S+)`)

func tmuxPrefixFromShowOptions(out string) string {
	if m := tmuxPrefixRe.FindStringSubmatch(out); m != nil && m[1] != "" {
		return m[1]
	}
	return "C-b"
}

// tmuxEnvFlags renders ordered env entries as `-e NAME=VALUE` argv pairs. A
// trailing ";" in a value is escaped to "\;" (tmux treats a bare ";" as a
// command separator).
func tmuxEnvFlags(entries []kv) []string {
	out := make([]string, 0, len(entries)*2)
	for _, e := range entries {
		v := e.Value
		if strings.HasSuffix(v, ";") {
			v = v[:len(v)-1] + `\;`
		}
		out = append(out, "-e", e.Key+"="+v)
	}
	return out
}

// kv is an ordered key/value pair (Go maps do not preserve order, and the
// env flags are emitted in insertion order).
type kv struct {
	Key   string
	Value string
}

// tmuxCommand is one tmux invocation in the launch plan. Foreground commands
// inherit the parent stdio and are awaited (the attach/switch step); the rest
// are captured.
type tmuxCommand struct {
	Args       []string
	Foreground bool
}

// tmuxInvocation holds the resolved inputs for the final tmux command plan.
type tmuxInvocation struct {
	Session      string
	WorktreePath string
	EnvFlags     []string // pre-rendered "-e NAME=VALUE" pairs (empty on tmux <3.2)
	Exec         []string // exec prefix + pass-through args, placed after "--"
	ControlMode  bool     // iTerm2 -CC control mode (new window as a tab)
	InsideTmux   bool     // already inside a tmux client ($TMUX set)
	HasSession   bool     // a session with this name already exists
}

// buildTmuxCommands builds the launch plan for each case:
//
//   - not inside tmux: a single `[-CC] new-session -A -s <s> [-e ...] -c <path>
//     -- <exec>` run in the foreground (attach-or-create);
//   - inside tmux, session exists: `switch-client -t <s>`;
//   - inside tmux, no session: a detached `new-session -d -s <s> [-e ...] -c
//     <path> -- <exec>` then `switch-client -t <s>`.
func buildTmuxCommands(inv tmuxInvocation) []tmuxCommand {
	newSession := func(detached bool, control bool) []string {
		var args []string
		if control {
			args = append(args, "-CC")
		}
		args = append(args, "new-session")
		if detached {
			args = append(args, "-d")
		} else {
			args = append(args, "-A")
		}
		args = append(args, "-s", inv.Session)
		args = append(args, inv.EnvFlags...)
		args = append(args, "-c", inv.WorktreePath, "--")
		args = append(args, inv.Exec...)
		return args
	}

	if inv.InsideTmux {
		if inv.HasSession {
			return []tmuxCommand{{Args: []string{"switch-client", "-t", inv.Session}, Foreground: true}}
		}
		return []tmuxCommand{
			{Args: newSession(true, false), Foreground: false},
			{Args: []string{"switch-client", "-t", inv.Session}, Foreground: true},
		}
	}
	return []tmuxCommand{{Args: newSession(false, inv.ControlMode), Foreground: true}}
}

// passThroughArgs returns the user's argv with the tmux/worktree-selecting
// flags removed, so the inner gocode inherits the rest. Everything after a
// "--" is passed through verbatim; flag values are not re-quoted.
func passThroughArgs(args []string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		switch {
		case a == "--tmux" || a == "--tmux=classic":
			continue
		case a == "-w" || a == "--worktree":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		case strings.HasPrefix(a, "--worktree="):
			continue
		}
		out = append(out, a)
	}
	return out
}

// -----------------------------------------------------------------------------
// Exec / git seam.
// -----------------------------------------------------------------------------

// cmdResult is the {code, stdout, stderr} runner result.
type cmdResult struct {
	Code   int
	Stdout string
	Stderr string
}

// commandRunner is the injectable process seam. capture runs a command and
// collects its output; runForeground inherits the parent stdio and waits.
type commandRunner interface {
	lookPath(name string) (string, error)
	capture(ctx context.Context, name string, args []string, dir string, env []string) (cmdResult, error)
	runForeground(ctx context.Context, name string, args []string, dir string, env []string) (int, error)
}

// execRunner is the real os/exec-backed runner.
type execRunner struct{}

func (execRunner) lookPath(name string) (string, error) { return exec.LookPath(name) }

func (execRunner) capture(ctx context.Context, name string, args []string, dir string, env []string) (cmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := cmdResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.Code = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

func (execRunner) runForeground(ctx context.Context, name string, args []string, dir string, env []string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// tmuxDeps bundles the injectable behaviour of execIntoTmuxWorktree.
type tmuxDeps struct {
	runner commandRunner
	// isWindows forces the Windows clear-error branch (defaults to the real GOOS).
	isWindows bool
	// trusted reports whether the workspace trust dialog has been accepted.
	// The default seam treats every workspace as trusted: the
	// light trust check (trust.go) only asks about folders whose settings run
	// code, so a plain folder is never "accepted" and gating --worktree on it
	// would refuse every plain repository.
	trusted func() bool
	// gitRoot resolves the git worktree root for dir, or ("", false) when dir is
	// not in a git repository.
	gitRoot func(ctx context.Context, dir string) (string, bool)
	// createWorktree materialises the worktree at path on branch. The default
	// seam runs the core `git worktree add` argv; PR fetch, base resolution,
	// sparse checkout, containment checks and hooks are out of scope.
	createWorktree func(ctx context.Context, repoRoot, path, branch string) error
	// rng seeds the random worktree-name generator.
	rng *rand.Rand
	// insideTmux reports whether $TMUX is set.
	insideTmux bool
	// execPath is the gocode binary the tmux session runs.
	execPath string
	// extraEnv are additional process env entries the spawned tmux inherits.
	baseEnv []string
}

// defaultTmuxDeps wires tmuxDeps to the real process.
func defaultTmuxDeps(env *Env) tmuxDeps {
	rnr := execRunner{}
	exe, _ := os.Executable()
	if exe == "" {
		exe = "gocode"
	}
	return tmuxDeps{
		runner:    rnr,
		isWindows: runtime.GOOS == "windows",
		trusted:   func() bool { return true },
		gitRoot:   gitRootViaRunner(rnr),
		createWorktree: func(ctx context.Context, repoRoot, path, branch string) error {
			return gitCreateWorktree(ctx, rnr, repoRoot, path, branch)
		},
		rng:        rand.New(rand.NewSource(seedFromEnv(env))),
		insideTmux: env.Getenv("TMUX") != "",
		execPath:   exe,
		baseEnv:    os.Environ(),
	}
}

// execIntoTmuxWorktree is the orchestrator. It is pure of global
// state: everything it touches comes through deps, so tests drive it with a
// fake runner and never spawn tmux or git.
func execIntoTmuxWorktree(ctx context.Context, env *Env, args []string, deps tmuxDeps) tmuxWorktreeResult {
	if deps.isWindows {
		// tmux is unix-only.
		return tmuxWorktreeResult{errMsg: "Error: tmux is not natively available on Windows. Consider using WSL or Cygwin."}
	}

	// (a) Workspace-trust gate.
	if deps.trusted != nil && !deps.trusted() {
		return tmuxWorktreeResult{errMsg: "Workspace trust not yet accepted. Run `gocode` once in this directory and accept the trust dialog, then retry with --worktree."}
	}

	// (b) tmux must be installed.
	ver, err := deps.runner.capture(ctx, "tmux", []string{"-V"}, "", deps.baseEnv)
	if err != nil || ver.Code != 0 {
		return tmuxWorktreeResult{errMsg: "Error: tmux is not installed. Install tmux with: sudo apt install tmux"}
	}

	// Parse the worktree name + classic mode from the argv.
	name, classic := parseTmuxWorktreeArgs(args)

	// A PR/MR reference collapses to a pr-<n> name.
	if name != "" {
		if pr, ok := parsePRReference(name); ok {
			name = "pr-" + strconv.Itoa(pr.Number)
		}
	}
	// No name -> a generated adjective-noun-suffix name.
	if name == "" {
		name = randomWorktreeName(deps.rng)
	}

	// Validate the slug.
	if msg := validateWorktreeName(name); msg != "" {
		return tmuxWorktreeResult{errMsg: "Error: " + msg}
	}

	// Resolve the cwd and git root.
	cwd := ""
	if env.Cwd != nil {
		if c, cerr := env.Cwd(); cerr == nil {
			cwd = c
		}
	}
	root, ok := deps.gitRoot(ctx, cwd)
	if !ok || root == "" {
		return tmuxWorktreeResult{errMsg: "Error: --worktree requires a git repository"}
	}

	// (a) Resolve/create the worktree.
	wtPath := worktreePath(root, name)
	branch := worktreeBranchName(name)
	if deps.createWorktree != nil {
		if cerr := deps.createWorktree(ctx, root, wtPath, branch); cerr != nil {
			return tmuxWorktreeResult{errMsg: "Error: " + cerr.Error()}
		}
	}

	// (b) Derive the session name and launch inputs.
	session := generateTmuxSessionName(root, branch)

	execCmd := append([]string{deps.execPath}, passThroughArgs(args)...)

	// tmux prefix detection + conflict flag.
	prefix := "C-b"
	if po, perr := deps.runner.capture(ctx, "tmux", []string{"show-options", "-g", "prefix"}, "", deps.baseEnv); perr == nil && po.Code == 0 {
		prefix = tmuxPrefixFromShowOptions(po.Stdout)
	}
	_, conflicts := tmuxPrefixConflicts[prefix]
	conflictFlag := ""
	if conflicts {
		conflictFlag = "1"
	}

	// Session env, in emission order.
	envEntries := []kv{
		{"GOCODE_CODE_TMUX_SESSION", session},
		{"GOCODE_CODE_TMUX_PREFIX", prefix},
		{"GOCODE_CODE_TMUX_PREFIX_CONFLICTS", conflictFlag},
	}

	spawnEnv := deps.baseEnv
	var envFlags []string
	if tmuxSupportsEnvFlags(ver.Stdout) {
		envFlags = tmuxEnvFlags(envEntries)
	} else {
		// tmux <3.2: carry the vars in the spawned process env instead.
		for _, e := range envEntries {
			spawnEnv = append(spawnEnv, e.Key+"="+e.Value)
		}
	}

	// Does a session with this name already exist?
	hasSession := false
	if hs, herr := deps.runner.capture(ctx, "tmux", []string{"has-session", "-t", session}, "", deps.baseEnv); herr == nil && hs.Code == 0 {
		hasSession = true
	}

	inv := tmuxInvocation{
		Session:      session,
		WorktreePath: wtPath,
		EnvFlags:     envFlags,
		Exec:         execCmd,
		ControlMode:  false, // no iTerm2 control mode detection; classic forces off.
		InsideTmux:   deps.insideTmux,
		HasSession:   hasSession,
	}
	_ = classic // classic only affects iTerm2 -CC selection, which is not detected here.

	// (c) Run the plan: captured setup commands, then the foreground attach.
	for _, c := range buildTmuxCommands(inv) {
		if c.Foreground {
			if _, ferr := deps.runner.runForeground(ctx, "tmux", c.Args, wtPath, spawnEnv); ferr != nil {
				return tmuxWorktreeResult{errMsg: "Error: " + ferr.Error()}
			}
			continue
		}
		if _, cerr := deps.runner.capture(ctx, "tmux", c.Args, wtPath, spawnEnv); cerr != nil {
			return tmuxWorktreeResult{errMsg: "Error: " + cerr.Error()}
		}
	}
	return tmuxWorktreeResult{handled: true}
}

// gitRootViaRunner resolves the repo root with `git rev-parse --show-toplevel`.
func gitRootViaRunner(r commandRunner) func(ctx context.Context, dir string) (string, bool) {
	return func(ctx context.Context, dir string) (string, bool) {
		res, err := r.capture(ctx, "git", []string{"rev-parse", "--show-toplevel"}, dir, os.Environ())
		if err != nil || res.Code != 0 {
			return "", false
		}
		root := strings.TrimSpace(res.Stdout)
		return root, root != ""
	}
}

// gitCreateWorktree runs the core `git worktree add` argv when the
// worktree directory does not already exist. The PR/MR fetch, base-branch
// resolution, sparse checkout, containment verification and WorktreeCreate
// hook are out of scope (documented seam).
func gitCreateWorktree(ctx context.Context, r commandRunner, repoRoot, path, branch string) error {
	if _, err := os.Stat(path); err == nil {
		// Already present: reuse it.
		return nil
	}
	args := []string{"worktree", "add", "--no-track", "-B", branch, path, "HEAD^{commit}"}
	res, err := r.capture(ctx, "git", args, repoRoot, os.Environ())
	if err != nil {
		return fmt.Errorf("git worktree add failed: %v", err)
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("git worktree add exited %d", res.Code)
		}
		return fmt.Errorf("Failed to create worktree: %s", msg)
	}
	return nil
}

// seedFromEnv derives a non-deterministic-ish seed from the clock/pid so the
// generated worktree name differs run to run. It is only used by the default
// deps; tests inject their own rng.
func seedFromEnv(env *Env) int64 {
	return int64(os.Getpid()) ^ time.Now().UnixNano()
}
