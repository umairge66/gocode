package daemon

import (
	"errors"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/envsec"
)

// -----------------------------------------------------------------------------
// Dispatch descriptor: what the CLI sends in the `dispatch` op's `d` field to
// start a background session, and how the daemon turns it into the worker's
// argv and environment.

// Launch modes.
const (
	LaunchPrompt = "prompt"
	LaunchResume = "resume"
	LaunchExec   = "exec"
)

// Dispatch sources.
const (
	SourceShell   = "shell"
	SourceSpare   = "spare"
	SourceRespawn = "respawn"
)

// Launch describes how the worker starts.
type Launch struct {
	Mode string `json:"mode"`
	// prompt: the worker argv after the binary ("--session-id", id, prompt...).
	// exec: the shell argv (["-c", cmd] / ["/d","/s","/c",cmd]).
	Args []string `json:"args,omitempty"`
	// exec: the shell binary.
	Cmd string `json:"cmd,omitempty"`
	// resume
	SessionID          string   `json:"sessionId,omitempty"`
	TranscriptPath     string   `json:"transcriptPath,omitempty"`
	RestoresTranscript *bool    `json:"restoresTranscript,omitempty"`
	Fork               bool     `json:"fork,omitempty"`
	FlagArgs           []string `json:"flagArgs,omitempty"`
}

// Seed carries the display seed of the session.
type Seed struct {
	Intent string `json:"intent"`
	Name   string `json:"name,omitempty"`
}

// Dispatch is the `d` payload of the dispatch op.
type Dispatch struct {
	Proto        int               `json:"proto"`
	Short        string            `json:"short"`
	SessionID    string            `json:"sessionId"`
	CreatedAt    int64             `json:"createdAt"`
	Source       string            `json:"source"`
	Cwd          string            `json:"cwd"`
	Launch       Launch            `json:"launch"`
	RespawnFlags []string          `json:"respawnFlags"`
	Env          map[string]string `json:"env,omitempty"`
	ReattachEnv  map[string]string `json:"reattachEnv,omitempty"`
	Isolation    string            `json:"isolation,omitempty"`
	Agent        string            `json:"agent,omitempty"`
	Seed         *Seed             `json:"seed,omitempty"`
	Cols         int               `json:"cols,omitempty"`
	Rows         int               `json:"rows,omitempty"`
	Nonce        string            `json:"nonce,omitempty"`
}

// Validate checks the descriptor the daemon is about to act on.
func (d *Dispatch) Validate() error {
	if !ShortIDPattern.MatchString(d.Short) {
		return errors.New("malformed request: d.short must be 8 lowercase hex characters")
	}
	if d.SessionID == "" {
		return errors.New("malformed request: d.sessionId is required")
	}
	if d.Cwd == "" || !filepath.IsAbs(d.Cwd) {
		return errors.New("malformed request: d.cwd must be an absolute path")
	}
	switch d.Launch.Mode {
	case LaunchPrompt, LaunchResume:
	case LaunchExec:
		if d.Launch.Cmd == "" {
			return errors.New("malformed request: d.launch.cmd is required for exec")
		}
	default:
		return errors.New("malformed request: d.launch.mode must be prompt, resume or exec")
	}
	return nil
}

// ExecShell returns the shell invocation for an `--exec` command: $SHELL -c,
// %COMSPEC% /d /s /c on Windows, else /bin/sh -c.
func ExecShell(getenv func(string) string, command string) (string, []string) {
	if sh := getenv("SHELL"); sh != "" {
		return sh, []string{"-c", command}
	}
	if runtime.GOOS == "windows" {
		cs := getenv("COMSPEC")
		if cs == "" {
			cs = "cmd.exe"
		}
		return cs, []string{"/d", "/s", "/c", command}
	}
	return "/bin/sh", []string{"-c", command}
}

// WorkerArgs returns the worker's argv after the binary for attempt n.
// A respawn of a session whose transcript exists resumes it; a respawn that
// moved to a new session id restarts with that id.
func WorkerArgs(d *Dispatch, attempt int, hasTranscript bool, sessionID string, respawnFlags []string) []string {
	if d.Launch.Mode == LaunchExec {
		return append([]string(nil), d.Launch.Args...)
	}
	if attempt > 1 && hasTranscript {
		return append([]string{"--resume", sessionID}, respawnFlags...)
	}
	if attempt > 1 && sessionID != d.SessionID {
		return append([]string{"--session-id", sessionID}, respawnFlags...)
	}
	if d.Launch.Mode == LaunchResume {
		var out []string
		if d.Launch.Fork {
			out = append(out, "--session-id", d.SessionID, "--fork-session")
		}
		target := d.Launch.TranscriptPath
		if target == "" {
			target = d.Launch.SessionID
		}
		out = append(out, "--resume", target)
		return append(out, d.Launch.FlagArgs...)
	}
	return append([]string(nil), d.Launch.Args...)
}

// WorkerEnvParams carries the per-spawn inputs of WorkerEnv.
type WorkerEnvParams struct {
	JobDir    string
	ConfigDir string
	// PTYAuth is passed inline (Windows) unless TokensPath is set.
	PTYAuth    string
	TokensPath string
}

// WorkerEnv builds the pty host + worker environment from the daemon's own
// environment (base, KEY=VALUE form) and the dispatch.
func WorkerEnv(base []string, d *Dispatch, p WorkerEnvParams) []string {
	env := map[string]string{}
	pathKey := "PATH"
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if strings.EqualFold(k, "PATH") {
			pathKey = k
		}
		env[k] = v
	}
	basePath := env[pathKey]
	_, baseHasConfigDir := env["GOCODE_CONFIG_DIR"]
	for k, v := range d.Env {
		// A dispatch never carries credentials (the CLI sends none); one
		// that does — an older client — has them dropped, so a job's key
		// comes only from the daemon's own environment or auth.json.
		if envsec.IsProviderKeyEnvName(k) || envsec.IsSecretEnvName(k) {
			continue
		}
		env[k] = v
	}
	name := ""
	if d.Seed != nil {
		name = d.Seed.Name
		if name == "" {
			name = d.Seed.Intent
		}
	}
	if name == "" {
		name = d.Short
	}
	set := map[string]string{
		"GOCODE_CODE_SESSION_KIND": "bg",
		"GOCODE_BG_BACKEND":        "daemon",
		"GOCODE_BG_SOURCE":         d.Source,
		"GOCODE_JOB_DIR":           p.JobDir,
		"GOCODE_CODE_SESSION_NAME": sessionNameEnv(name),
		"FORCE_COLOR":              "3",
		"COLORTERM":                "truecolor",
		"BROWSER":                  "true",
	}
	for k, v := range set {
		env[k] = v
	}
	// PATH: the dispatch's, else the daemon's; one spelling only.
	path := d.Env["PATH"]
	if path == "" {
		path = basePath
	}
	for k := range env {
		if strings.EqualFold(k, "PATH") {
			delete(env, k)
		}
	}
	if path != "" {
		env[pathKey] = path
	}
	if exportConfigDir(p.ConfigDir, baseHasConfigDir) {
		env["GOCODE_CONFIG_DIR"] = p.ConfigDir
	}
	if d.Isolation == "worktree" {
		env["GOCODE_BG_ISOLATION"] = "worktree"
	}
	if p.TokensPath != "" {
		env["GOCODE_BG_SOCKET_TOKENS_PATH"] = p.TokensPath
		delete(env, "GOCODE_BG_PTY_AUTH")
	} else if p.PTYAuth != "" {
		env["GOCODE_BG_PTY_AUTH"] = p.PTYAuth
	}
	if d.Launch.Mode == LaunchExec {
		for k := range env {
			upper := strings.ToUpper(k)
			if strings.HasPrefix(upper, "GOCODE_") && upper != "GOCODE_JOB_DIR" && upper != "GOCODE_CONFIG_DIR" &&
				upper != "GOCODE_BG_PTY_AUTH" && upper != "GOCODE_BG_SOCKET_TOKENS_PATH" {
				delete(env, k)
			}
			if strings.HasPrefix(upper, "OTEL_") {
				delete(env, k)
			}
			// An exec job runs a plain command: it never needs gocode's own
			// provider keys.
			if envsec.IsProviderKeyEnvName(k) {
				delete(env, k)
			}
		}
		delete(env, "BROWSER")
		env["GOCODE_PTY_HOST_EXEC"] = "1"
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// exportConfigDir reports whether a worker must be told the daemon's gocode
// root through GOCODE_CONFIG_DIR: when it was set explicitly (inherited by the
// daemon) or the daemon serves a root other than the default one. Otherwise
// the worker resolves the same root itself.
func exportConfigDir(configDir string, inherited bool) bool {
	if configDir == "" {
		return false
	}
	if inherited {
		return true
	}
	return filepath.Clean(configDir) != filepath.Clean(DefaultConfigHome(nil))
}

// sessionNameEnv flattens a display name for an environment variable.
func sessionNameEnv(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == 0 {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}
