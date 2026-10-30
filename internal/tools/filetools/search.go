package filetools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/ugrep"
)

// SearchSettings are the user settings that steer the Grep and Glob engines
// (settings.json: "search": {...} and the top-level "respectGitignore").
// Pointer fields are tri-state: nil means "not set, use the tool's default".
type SearchSettings struct {
	// UseUgrep (search.useUgrep, default true) — false always uses the
	// built-in Go engine (and keeps the model's shell `grep` unshadowed).
	UseUgrep *bool
	// UgrepPath (search.ugrepPath) — explicit ugrep binary; wins over the
	// bundled one. GOCODE_UGREP_PATH overrides it.
	UgrepPath string
	// PreferSystemUgrep (search.preferSystemUgrep, default false) — use
	// ugrep from PATH instead of the bundled one.
	PreferSystemUgrep bool
	// IncludeHidden (search.includeHidden) — search hidden files and
	// directories. Default true for both tools (Glob also honours
	// GOCODE_CODE_GLOB_HIDDEN when the setting is unset).
	// VCS directories (.git, .svn, .hg, .bzr, .jj, .sl) are always excluded.
	IncludeHidden *bool
	// RespectGitignore (respectGitignore) — honor .gitignore/.ignore/.rgignore,
	// .git/info/exclude and the global git excludes file.
	// Default: true for Grep; for Glob the default is to NOT respect them
	// (GOCODE_CODE_GLOB_NO_IGNORE defaults to true).
	RespectGitignore *bool
}

// Environment knobs for the Glob tool and the search timeout.
const (
	EnvGlobNoIgnore       = "GOCODE_CODE_GLOB_NO_IGNORE"
	EnvGlobHidden         = "GOCODE_CODE_GLOB_HIDDEN"
	EnvGlobTimeoutSeconds = "GOCODE_CODE_GLOB_TIMEOUT_SECONDS"
)

// ugrepMaxOutputBytes caps collected ugrep stdout.
const ugrepMaxOutputBytes = 20_000_000

// SearchEngine runs the Grep and Glob tools for a session. Files are chosen
// by gocode's walker (ignore files, hidden entries, globs, file types); the
// Grep tool's content matching runs on ugrep (bundled, configured or system)
// when one resolves, and every match is rendered by the built-in Go engine,
// which also does the whole search when ugrep is off, missing or fails.
//
// A nil *SearchEngine is valid and means "built-in engine, default settings".
// Resolution happens lazily on first use and is cached; if the chosen ugrep
// then fails to start, the engine switches to the built-in one for the rest
// of the session and reports a one-line notice once.
type SearchEngine struct {
	Settings SearchSettings
	// Ugrep carries injectable resolution hooks (tests); its UseUgrep,
	// UgrepPath and PreferSystem are filled from Settings.
	Ugrep ugrep.Config
	// Getenv reads the environment (nil: os.Getenv).
	Getenv func(string) string
	// Cwd is the directory result paths are shown relative to (""=os.Getwd).
	Cwd string

	mu         sync.Mutex // guards everything below and Settings after Apply
	resolved   bool
	resolution ugrep.Resolution
	version    string
	failed     bool   // ugrep failed to start: built-in engine from now on
	notice     string // pending one-line notice
	noticeSent bool
}

// Apply replaces the engine's settings live (the /search dialog) and forgets
// the resolved engine, so the next search re-resolves ugrep vs built-in with
// the new settings. Safe to call while searches are running: in-flight searches
// finish with the settings they started with.
func (e *SearchEngine) Apply(s SearchSettings) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Settings = s
	e.resolved = false
	e.resolution = ugrep.Resolution{}
	e.version = ""
	e.failed = false
	e.notice = ""
	e.noticeSent = false
}

// settings returns a snapshot of the current settings.
func (e *SearchEngine) settings() SearchSettings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Settings
}

// NewSearchEngine returns an engine for the given settings.
func NewSearchEngine(s SearchSettings) *SearchEngine {
	return &SearchEngine{Settings: s}
}

func (e *SearchEngine) getenv(k string) string {
	if e != nil && e.Getenv != nil {
		return e.Getenv(k)
	}
	return os.Getenv(k)
}

func (e *SearchEngine) resolve() ugrep.Resolution {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.resolved {
		cfg := e.Ugrep
		cfg.UseUgrep = e.Settings.UseUgrep
		cfg.UgrepPath = e.Settings.UgrepPath
		cfg.PreferSystem = e.Settings.PreferSystemUgrep
		if cfg.Getenv == nil {
			cfg.Getenv = e.Getenv
		}
		e.resolution = ugrep.ResolveDetailed(cfg)
		e.version = e.resolution.Version
		if len(e.resolution.Notes) > 0 {
			e.notice = strings.Join(e.resolution.Notes, "; ")
		}
		e.resolved = true
	}
	return e.resolution
}

// UgrepPath returns the ugrep binary searches run on, or "" when the built-in
// engine does the whole search (ugrep off, not found, or failed to start).
// The first call resolves (and, for the bundled binary, extracts) ugrep.
func (e *SearchEngine) UgrepPath() string {
	if e == nil {
		return ""
	}
	r := e.resolve()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failed || r.Source == ugrep.SourceBuiltin {
		return ""
	}
	return r.Path
}

// markFailed records that ugrep could not be started; later searches use the
// built-in engine.
func (e *SearchEngine) markFailed(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.failed {
		e.failed = true
		e.notice = fmt.Sprintf("ugrep (%s) could not be started (%v); using the built-in search engine for this session.", e.resolution.Path, err)
		e.noticeSent = false
	}
}

// takeNotice returns a pending notice once.
func (e *SearchEngine) takeNotice() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.notice == "" || e.noticeSent {
		return ""
	}
	e.noticeSent = true
	return e.notice
}

// Describe renders the engine for /status, /doctor and --version --verbose:
// "ugrep 7.8.5 (bundled)", "ugrep 7.8.4 (system /usr/bin/ugrep)",
// "ugrep 7.8.4 (configured /opt/ugrep)" or "built-in".
func (e *SearchEngine) Describe() string {
	if e == nil {
		return "built-in"
	}
	r := e.resolve()
	e.mu.Lock()
	failed := e.failed
	v := e.version
	e.mu.Unlock()
	if failed || r.Source == ugrep.SourceBuiltin {
		if s := e.settings(); !failed && s.UseUgrep != nil && !*s.UseUgrep {
			return "built-in (ugrep disabled by search.useUgrep)"
		}
		return "built-in"
	}
	if v == "" {
		if pv, err := ugrep.ProbeVersion(r.Path); err == nil {
			v = pv
			e.mu.Lock()
			e.version = pv
			e.mu.Unlock()
		}
	}
	name := "ugrep"
	if v != "" {
		name += " " + v
	}
	if r.Source == ugrep.SourceBundled {
		return name + " (bundled)"
	}
	return fmt.Sprintf("%s (%s %s)", name, r.Source, r.Path)
}

// cwd returns the display base directory.
func (e *SearchEngine) cwd() string {
	if e != nil && e.Cwd != "" {
		return e.Cwd
	}
	wd, _ := os.Getwd()
	return wd
}

// grepOptions resolves the hidden / ignore switches for Grep.
func (e *SearchEngine) grepOptions() (hidden, respectIgnore bool) {
	hidden, respectIgnore = true, true
	if e == nil {
		return
	}
	s := e.settings()
	if s.IncludeHidden != nil {
		hidden = *s.IncludeHidden
	}
	if s.RespectGitignore != nil {
		respectIgnore = *s.RespectGitignore
	}
	return
}

// globOptions resolves the hidden / ignore switches for Glob: settings win,
// else the env knobs (both default "true").
func (e *SearchEngine) globOptions() (hidden, respectIgnore bool) {
	envBool := func(k string) bool {
		v := strings.TrimSpace(e.getenv(k))
		if v == "" {
			v = "true"
		}
		return envutil.EnvTruthy(v)
	}
	hidden = envBool(EnvGlobHidden)
	respectIgnore = !envBool(EnvGlobNoIgnore)
	if e == nil {
		return
	}
	s := e.settings()
	if s.IncludeHidden != nil {
		hidden = *s.IncludeHidden
	}
	if s.RespectGitignore != nil {
		respectIgnore = *s.RespectGitignore
	}
	return
}

// timeout is the ugrep run timeout.
func (e *SearchEngine) timeout() time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(e.getenv(EnvGlobTimeoutSeconds))); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if isWSL() {
		return 60 * time.Second
	}
	return 20 * time.Second
}

func (e *SearchEngine) timeoutSeconds() int { return int(e.timeout() / time.Second) }

func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && bytes.Contains(bytes.ToLower(b), []byte("microsoft"))
}

// errUgrepStart means ugrep could not be started at all (missing, not
// executable, wrong format); the caller falls back to the built-in engine.
type errUgrepStart struct{ err error }

func (e errUgrepStart) Error() string { return e.err.Error() }
func (e errUgrepStart) Unwrap() error { return e.err }

// errUgrepTimeout means ugrep did not finish within the search timeout.
var errUgrepTimeout = errors.New("ugrep timed out")

// capBuffer collects up to max bytes and reports overflow.
type capBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
	onFull   func()
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.overflow {
		return len(p), nil
	}
	room := c.max - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:room])
		c.overflow = true
		if c.onFull != nil {
			c.onFull()
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// ugrepRun is the outcome of one ugrep invocation.
type ugrepRun struct {
	out      []byte
	code     int
	overflow bool
}

// runUgrep runs ugrep (argv only, never a shell) with stdin, the search
// timeout and the output cap. A start failure is errUgrepStart; a timeout is
// errUgrepTimeout.
func (e *SearchEngine) runUgrep(path string, args []string, stdin []byte) (ugrepRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = ugrepEnv(os.Environ())
	cmd.Stdin = bytes.NewReader(stdin)
	out := &capBuffer{max: ugrepMaxOutputBytes, onFull: cancel}
	cmd.Stdout = out
	cmd.Stderr = &capBuffer{max: 64 << 10}
	if err := cmd.Start(); err != nil {
		return ugrepRun{}, errUgrepStart{err}
	}
	werr := cmd.Wait()
	if !out.overflow && ctx.Err() == context.DeadlineExceeded {
		return ugrepRun{}, errUgrepTimeout
	}
	code := 0
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return ugrepRun{}, werr
		}
		code = ee.ExitCode()
	}
	return ugrepRun{out: out.buf.Bytes(), code: code, overflow: out.overflow}, nil
}

// ugrepEnv drops the variables that change how ugrep reads its options or
// formats output, so a user's shell setup cannot alter the tool's results.
func ugrepEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "GREP_OPTIONS", "GREP_COLOR", "GREP_COLORS", "GREP_PATH":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isStartFailure reports whether err means ugrep could not be executed.
func isStartFailure(err error) bool {
	var se errUgrepStart
	if errors.As(err, &se) {
		return true
	}
	return errors.Is(err, syscall.ENOEXEC) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}
