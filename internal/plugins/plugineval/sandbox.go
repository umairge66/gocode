package plugineval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Sandbox is one run's throwaway tree:
//
//	<root>/config      the child's config dir (GOCODE_CONFIG_DIR)
//	<root>/home        the child's HOME (a git repository, with a .gitconfig)
//	<root>/home/cwd    the run's working directory
//	<root>/out         the harness's own files (trace.jsonl)
//	<root>/tmp         TMPDIR (0700)
type Sandbox struct {
	Root      string
	Cwd       string
	ConfigDir string
	Home      string
	OutDir    string
	TmpDir    string
}

// gitRepoConfig: the home directory's .git/config.
const gitRepoConfig = "[core]\n\trepositoryformatversion = 0\n\tbare = false\n"

func newSandbox() (*Sandbox, error) {
	base := os.TempDir()
	prefix := "gocode-eval-"
	if runtime.GOOS == "darwin" {
		base, prefix = "/tmp", "e-"
	}
	root, err := os.MkdirTemp(base, prefix)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	sb := &Sandbox{
		Root:      root,
		ConfigDir: filepath.Join(root, "config"),
		Home:      filepath.Join(root, "home"),
		OutDir:    filepath.Join(root, "out"),
		TmpDir:    filepath.Join(root, "tmp"),
	}
	sb.Cwd = filepath.Join(sb.Home, "cwd")
	steps := []func() error{
		func() error { return os.MkdirAll(sb.Cwd, 0o755) },
		func() error { return os.MkdirAll(sb.ConfigDir, 0o755) },
		func() error { return os.MkdirAll(sb.OutDir, 0o755) },
		func() error { return os.MkdirAll(sb.TmpDir, 0o700) },
		func() error {
			return os.WriteFile(filepath.Join(sb.ConfigDir, ".gocode.json"), []byte(`{"hasCompletedOnboarding":true,"autoUpdates":false,"bypassPermissionsModeAccepted":false}`), 0o644)
		},
		func() error {
			return os.WriteFile(filepath.Join(sb.Home, ".gitconfig"), []byte("[user]\n\tname = Plugin Eval\n\temail = eval@example.invalid\n"), 0o644)
		},
		func() error { return os.MkdirAll(filepath.Join(sb.Home, ".git", "objects"), 0o755) },
		func() error { return os.MkdirAll(filepath.Join(sb.Home, ".git", "refs", "heads"), 0o755) },
		func() error { return os.MkdirAll(filepath.Join(sb.Home, ".git", "hooks"), 0o755) },
		func() error {
			return os.WriteFile(filepath.Join(sb.Home, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
		},
		func() error {
			return os.WriteFile(filepath.Join(sb.Home, ".git", "config"), []byte(gitRepoConfig), 0o644)
		},
		func() error { return os.WriteFile(filepath.Join(sb.Home, ".git", "commondir"), []byte(".\n"), 0o644) },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			os.RemoveAll(root)
			return nil, err
		}
	}
	return sb, nil
}

// Cleanup removes the sandbox.
func (sb *Sandbox) Cleanup() error {
	_ = filepath.Walk(sb.Root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() && fi.Mode().Perm()&0o700 != 0o700 {
			_ = os.Chmod(p, fi.Mode().Perm()|0o700)
		}
		return nil
	})
	return os.RemoveAll(sb.Root)
}

// seal: the agent-written home/ and tmp/ are moved into
// <root>/sealed and closed (mode 000), and the kept root made read-only, so
// nothing in it is run or loaded by accident.
func (sb *Sandbox) seal() (string, error) {
	if runtime.GOOS == "windows" {
		return "", errors.New("directory modes do not restrict access on Windows")
	}
	if os.Getuid() == 0 {
		return "", errors.New("the harness is running as root, which a directory mode does not bind")
	}
	sealed := filepath.Join(sb.Root, "sealed")
	if err := os.Mkdir(sealed, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(sb.Home, filepath.Join(sealed, "home")); err != nil {
		return "", err
	}
	if err := os.Rename(sb.TmpDir, filepath.Join(sealed, "tmp")); err != nil {
		return "", err
	}
	if err := os.Chmod(sealed, 0); err != nil {
		return "", err
	}
	if err := os.Chmod(sb.Root, 0o500); err != nil {
		return "", err
	}
	return sealed, nil
}

func (sb *Sandbox) sealedNote(sealed string) string {
	return fmt.Sprintf("home/ and tmp/ in it were written by the plugin under test and are sealed in %s (mode 000; the kept directory is read-only) — open them with `chmod 700 %s %s` to inspect, and do not run git (or anything that loads configuration from its working directory) anywhere inside the kept directory", sealed, sb.Root, sealed)
}

func (sb *Sandbox) removeHint() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("Remove-Item -Recurse -Force -LiteralPath '%s'", strings.ReplaceAll(sb.Root, "'", "''"))
	}
	return fmt.Sprintf("chmod -R u+rwX %s && rm -rf %s", shellQuote(sb.Root), shellQuote(sb.Root))
}

func shellQuote(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_./-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// evalEnvKey: the only keys a case may set in execution.env.
var evalEnvKey = regexp.MustCompile(`(?i)^EVAL_`)

// strippedEnvKeys are the parent variables a run never inherits: the sandbox
// replaces them, or they steer the harness itself.
var strippedEnvKeys = setOf(
	"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	"TMPDIR", "TMP", "TEMP", "GOCODE_CONFIG_DIR", "GOCODE_PROFILE", "GOCODE_CODE_TMPDIR",
	"GOCODE_CODE_MANAGED_SETTINGS_PATH", "GOCODE_CODE_MAX_TURNS", "GOCODE_CODE_EVAL_CONFINED",
	"GOCODE_CODE_EVAL_ALLOW_ARTIFACT_PUBLISH", "GOCODE_CODE_EVAL_ALLOW_FLAG_OVERRIDES",
	"GOCODE_CODE_EVAL_ARTIFACT_STUB_DIR", "GOCODE_INTERNAL_FC_OVERRIDES",
	"HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM",
	"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_NO_LAZY_FETCH",
)

// gitHardening: git in the run uses no pager, no editor and no
// credential helper.
var gitHardening = [][2]string{{"core.pager", "cat"}, {"core.editor", "true"}, {"credential.helper", ""}, {"core.hooksPath", "/dev/null"}, {"core.fsmonitor", "false"}}

// runEnv: the parent environment minus what the sandbox
// replaces, the operator's own EVAL_* variables, the case's execution.env
// (EVAL_* keys only), the sandbox paths, and the extra variables the host
// supplies (the provider selection and key).
func runEnv(parent []string, c *Case, sb *Sandbox, extra []string) ([]string, error) {
	for k := range c.Env {
		if !evalEnvKey.MatchString(k) {
			return nil, fmt.Errorf("case %q execution.env key %q is not allowed — only EVAL_* keys can be set from case.yaml. Anything else must come from the operator's shell.", c.Name, k)
		}
	}
	env := map[string]string{}
	var order []string
	set := func(k, v string) {
		for _, existing := range order {
			if strings.EqualFold(existing, k) && existing != k {
				delete(env, existing)
			}
		}
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	for _, kv := range parent {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if strippedEnvKeys[strings.ToUpper(k)] || strings.HasPrefix(strings.ToUpper(k), "GIT_CONFIG_KEY_") || strings.HasPrefix(strings.ToUpper(k), "GIT_CONFIG_VALUE_") {
			continue
		}
		set(k, v)
	}
	for _, k := range sortedStringKeys(c.Env) {
		set(k, c.Env[k])
	}
	paths := map[string]string{
		"GOCODE_CONFIG_DIR":                 sb.ConfigDir,
		"HOME":                              sb.Home,
		"USERPROFILE":                       sb.Home,
		"XDG_CONFIG_HOME":                   filepath.Join(sb.Home, ".config"),
		"XDG_DATA_HOME":                     filepath.Join(sb.Home, ".local", "share"),
		"XDG_CACHE_HOME":                    filepath.Join(sb.Home, ".cache"),
		"XDG_STATE_HOME":                    filepath.Join(sb.Home, ".local", "state"),
		"TMPDIR":                            sb.TmpDir,
		"TMP":                               sb.TmpDir,
		"TEMP":                              sb.TmpDir,
		"GOCODE_CODE_TMPDIR":                sb.TmpDir,
		"GOCODE_CODE_MANAGED_SETTINGS_PATH": sb.ConfigDir,
		"GOCODE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DISABLE_AUTOUPDATER":                      "1",
		"GIT_CONFIG_NOSYSTEM":                      "1",
		"GIT_PROXY_COMMAND":                        "",
		"GOCODE_CODE_EVAL_CONFINED":                "1",
	}
	if runtime.GOOS == "windows" {
		drive := ""
		if len(sb.Home) >= 2 && sb.Home[1] == ':' {
			drive = sb.Home[:2]
		}
		paths["HOMEDRIVE"] = drive
		paths["HOMEPATH"] = sb.Home[len(drive):]
		paths["APPDATA"] = filepath.Join(sb.Home, "AppData", "Roaming")
		paths["LOCALAPPDATA"] = filepath.Join(sb.Home, "AppData", "Local")
	}
	for _, k := range sortedStringKeys(paths) {
		set(k, paths[k])
	}
	set("GIT_CONFIG_COUNT", fmt.Sprint(len(gitHardening)))
	for i, kv := range gitHardening {
		set(fmt.Sprintf("GIT_CONFIG_KEY_%d", i), kv[0])
		set(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), kv[1])
	}
	for _, kv := range extra {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out, nil
}

func sortedStringKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// scaffoldTimeout is the scaffold's 120s kill timer.
const scaffoldTimeout = 120 * time.Second

// runScaffold: `bash <script>` in the run directory with a minimal
// environment, stdout discarded, stderr kept (16 KB).
func runScaffold(ctx context.Context, script string, sb *Sandbox) (int, string) {
	ctx, cancel := context.WithTimeout(ctx, scaffoldTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Dir = sb.Cwd
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + sb.Home, "USERPROFILE=" + sb.Home,
		"TMPDIR=" + sb.TmpDir, "TMP=" + sb.TmpDir, "TEMP=" + sb.TmpDir,
		"TERM=dumb", "GIT_CONFIG_NOSYSTEM=1", "USER_TYPE=external", "NODE_ENV=production",
	}
	var stderr capped
	stderr.max = 16 << 10
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return ee.ExitCode(), stderr.String()
	}
	return 127, err.Error()
}

// capped is a bounded output buffer.
type capped struct {
	b   strings.Builder
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

func (c *capped) String() string { return c.b.String() }

// resolveCasePath resolves a case-relative path that must stay inside the
// case directory or the plugin under test.
func resolveCasePath(c *Case, rel, what string) (string, error) {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.CaseDir, filepath.FromSlash(rel))
	}
	p = filepath.Clean(p)
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("case %q: %s %q does not exist", c.Name, what, rel)
	}
	ok := within(real, c.CaseDir)
	for _, d := range c.PluginDirs {
		ok = ok || within(real, d)
	}
	if !ok {
		return "", fmt.Errorf("case %q: %s %q resolves outside the case directory and the plugin under test", c.Name, what, rel)
	}
	return real, nil
}

const (
	// maxWalkDepth / maxWalkEntries bound the run-directory listing.
	maxWalkDepth   = 32
	maxWalkEntries = 200000
)

var errWalk = errors.New("the run directory (or a directory inside it) is no longer the one the harness listed, or cannot be listed — moved, removed, replaced by a link, made unreadable, or too deep or too large to walk — so its file evidence cannot be trusted")

// listFiles lists every non-directory below dir, as "/"-joined relative
// paths; links are not followed.
func listFiles(dir string) (map[string]bool, error) {
	out := map[string]bool{}
	count := 0
	var walk func(rel string, depth int) error
	walk = func(rel string, depth int) error {
		if depth > maxWalkDepth {
			return errWalk
		}
		entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return errWalk
		}
		count += len(entries)
		if count > maxWalkEntries {
			return errWalk
		}
		for _, e := range entries {
			p := e.Name()
			if rel != "" {
				p = rel + "/" + e.Name()
			}
			if e.IsDir() {
				if err := walk(p, depth+1); err != nil {
					return err
				}
				continue
			}
			out[p] = true
		}
		return nil
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return nil, errWalk
	}
	if err := walk("", 0); err != nil {
		return nil, err
	}
	return out, nil
}

// newFiles: the files present after the run but not before, sorted.
func newFiles(before, after map[string]bool) string {
	var out []string
	for f := range after {
		if !before[f] {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}
