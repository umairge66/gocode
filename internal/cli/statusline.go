package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	envtrust "github.com/umairge66/gocode/internal/envtrust"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/git"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/statusline"
)

// This file reads the `theme` and `statusLine` settings for the interactive
// session and renders the configured status line. It reuses the same settings
// source precedence the permission rules use (internal/settingsload), so the
// values come entirely from the user's own settings files — there is nothing
// hardcoded and no telemetry.
//
// The status line is produced by internal/statusline's Renderer through an
// injectable Runner (ExecRunner in production, a fake in tests), so the whole
// path can be exercised without a real shell or TTY. internal/ui never builds
// or runs the status line itself; the host hands it the finished string via
// SetStatusLineMsg.

// cheapGitBranchTimeout bounds the best-effort `git symbolic-ref` probe used to
// fill the status-line context. The branch is a nicety, not worth blocking the
// session on, so the probe is given a short budget and any failure is silent.
const cheapGitBranchTimeout = 500 * time.Millisecond

// resolveInteractiveSettings reads the `theme` and `statusLine` settings from
// the effective settings sources (low→high precedence, last wins). It never
// fails: a missing or malformed settings file degrades to an empty theme (which
// the caller defaults to dark) and no status line.
//
// The status-line command is a hook in the disableAllHooks sense: a policy
// disableAllHooks runs none, and a disableAllHooks from any other source (or
// the policy's allowManagedHooksOnly) restricts it to the policy's own
// statusLine — so a user who sets disableAllHooks to keep a cloned
// repository's commands from running is covered for statusLine too.
func resolveInteractiveSettings(env *Env) (theme string, sl *statusline.Config) {
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	policyOff, managedOnly := false, false
	var policySL *statusline.Config
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		dis := or.Result.Settings["disableAllHooks"] == true
		if or.Source == settingspolicy.SourcePolicy {
			policyOff = policyOff || dis
			managedOnly = managedOnly || or.Result.Settings["allowManagedHooksOnly"] == true
		} else {
			managedOnly = managedOnly || dis
		}
		if t, ok := or.Result.Settings["theme"].(string); ok && t != "" {
			theme = t
		}
		if raw, ok := or.Result.Settings["statusLine"]; ok {
			if cfg := parseStatusLineConfig(raw); cfg != nil {
				sl = cfg
				if or.Source == settingspolicy.SourcePolicy {
					policySL = cfg
				}
			}
		}
	}
	switch {
	case policyOff:
		sl = nil
	case managedOnly:
		sl = policySL
	}
	return theme, sl
}

// parseStatusLineConfig decodes the `statusLine` setting (a JSON object) into a
// statusline.Config. It returns nil when the value cannot be decoded or is not
// a usable command config, so the caller treats it as "no status line".
func parseStatusLineConfig(raw any) *statusline.Config {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var cfg statusline.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	if !cfg.IsCommand() {
		return nil
	}
	return &cfg
}

// renderStatusLine runs the configured status-line command for a freshly-built
// context and returns the rendered line. It returns "" when no status line is
// configured, so the caller can decide whether to push an update at all.
func (s *interactiveSession) renderStatusLine(ctx context.Context, env *Env) string {
	if s.statusLine == nil || !s.statusLine.IsCommand() {
		return ""
	}
	r := &statusline.Renderer{Runner: s.statusRunner}
	return r.Render(ctx, s.statusLine, s.buildStatusContext(ctx, env))
}

// buildStatusContext assembles the statusline.Context handed to the command on
// stdin: the session id and /rename title, the active model, the cwd, the
// permission mode, and — when a git repository is cheaply reachable — the
// current branch. The Context has no plain top-level branch field, so the branch is
// carried on Workspace.GitWorktree (git_worktree); it is left empty when git is
// unavailable, detached, or slow.
func (s *interactiveSession) buildStatusContext(ctx context.Context, env *Env) *statusline.Context {
	cwd := currentDir(env)
	sc := &statusline.Context{
		SessionID:      s.sessionID,
		CWD:            cwd,
		PermissionMode: string(s.print.Mode),
		Model:          statusline.Model{ID: s.print.Model, DisplayName: s.print.Model},
		Workspace:      statusline.Workspace{CurrentDir: cwd, ProjectDir: cwd},
	}
	if s.sessionName != nil {
		sc.SessionName = s.sessionName()
	}
	if branch := cheapGitBranch(ctx, cwd); branch != "" {
		sc.Workspace.GitWorktree = branch
	}
	return sc
}

// currentDir resolves the working directory through the Env seam, returning ""
// when it is unavailable (which keeps tests free of a real cwd/git dependency).
func currentDir(env *Env) string {
	if env != nil && env.Cwd != nil {
		if cwd, err := env.Cwd(); err == nil {
			return cwd
		}
	}
	return ""
}

// cheapGitBranch returns the checked-out branch of the repository at dir, or ""
// when dir is empty, is not a repository, is in detached-HEAD state, or git is
// too slow. It is a best-effort read gated by a short timeout; every failure is
// silent so the status line never blocks on git.
func cheapGitBranch(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, cheapGitBranchTimeout)
	defer cancel()
	runner := git.NewRunner("", osEnvMap())
	res, err := runner.Run(ctx, dir, git.CurrentBranchArgs(), nil)
	if err != nil || res == nil {
		return ""
	}
	b := git.ParseBranch(res.Code, res.Stdout)
	if b.Detached {
		return ""
	}
	return b.Name
}

// osEnvMap snapshots the process environment as an envtrust.Env, used as the
// git runner's base environment (git needs PATH to be found). The runner layers
// its hardened/sanitized overrides on top.
func osEnvMap() envtrust.Env {
	e := envtrust.Env{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			e[kv[:i]] = kv[i+1:]
		}
	}
	return e
}
