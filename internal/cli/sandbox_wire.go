package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/sandbox"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// newSessionSandbox builds the session's OS sandbox (internal/sandbox.Manager)
// from the loaded settings sources. Nothing starts until Start / the first
// sandboxed command; the session closes it.
//
// Only local state is touched: the proxy it may start listens on 127.0.0.1 and
// forwards only what a sandboxed command itself requests, to allowlisted hosts.
//
// addDirs are the session's resolved --add-dir directories: writable like the
// working directory (and protected like it).
func newSessionSandbox(env *Env, ordered []settingsload.OrderedResult, interactive bool, addDirs ...string) *sandbox.Manager {
	ctx := settingsContext(env)
	if ordered == nil {
		ordered = settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	}
	configDir, _ := ctx.UserConfigDir()
	paths := sandbox.SessionPaths{
		Cwd:       ctx.Cwd,
		Home:      ctx.HomeDir,
		ConfigDir: configDir,
		AddDirs:   addDirs,
	}
	for _, src := range settingsload.CanonicalSourceOrder {
		if p, ok := ctx.PathForSource(src); ok {
			paths.SettingsFiles = append(paths.SettingsFiles, p)
		}
	}
	if base := ctx.ManagedBaseDir(); base != "" {
		paths.ManagedDirs = append(paths.ManagedDirs, filepath.Join(base, "managed-settings.json"), ctx.ManagedDropInDir())
	}
	getenv := envGet(env)
	paths.TempDir = sandbox.TempRoot(getenv)
	return sandbox.NewManager(sandbox.ManagerOptions{
		Layers:      sandboxLayers(ordered, ctx.Cwd, configDir),
		Paths:       paths,
		Getenv:      getenv,
		Interactive: interactive,
	})
}

// sandboxLayers turns the loaded settings sources into sandbox layers (lowest
// precedence first), each with the directory its relative sandbox paths
// resolve against: the project root for project/local settings, the config
// home for user settings.
func sandboxLayers(ordered []settingsload.OrderedResult, cwd, configDir string) []sandbox.Layer {
	var out []sandbox.Layer
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded || or.Result.Settings == nil {
			continue
		}
		base := cwd
		if or.Source == settingspolicy.SourceUser && configDir != "" {
			base = configDir
		}
		out = append(out, sandbox.Layer{Source: string(or.Source), Settings: or.Result.Settings, BaseDir: base})
	}
	return out
}

// sandboxStartupCheck reports the sandbox state at session start: a required
// sandbox that cannot run refuses to start the session (exit 1), an enabled
// one that cannot run warns that commands run unsandboxed. It then starts the
// sandbox, so a start failure is known before the first command.
func sandboxStartupCheck(env *Env, sb *sandbox.Manager) (exitCode int, stop bool) {
	if sb == nil {
		return 0, false
	}
	stderr := env.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	fatal, warn := sb.StartupNotice()
	if fatal != "" {
		fmt.Fprintf(stderr, "\n%s\n\n", fatal)
		return 1, true
	}
	if warn != "" {
		fmt.Fprintf(stderr, "\n%s\n\n", warn)
		return 0, false
	}
	if err := sb.Start(); err != nil {
		if sb.IsSandboxRequired() {
			fmt.Fprintf(stderr, "\nError: sandbox required but unavailable: %v\n  sandbox.failIfUnavailable is set — refusing to start without a working sandbox.\n\n", err)
			return 1, true
		}
		fmt.Fprintf(stderr, "\n⚠ Sandbox disabled: %v\n  Commands will run WITHOUT sandboxing. Network and filesystem restrictions will NOT be enforced.\n\n", err)
	}
	return 0, false
}

// resolveAddDirs validates the --add-dir values: each becomes an absolute,
// cleaned directory ("~" expanded, relative to the cwd). A missing path and
// one already inside the working directory are skipped silently; anything
// else that is not a directory is skipped with a warning.
func resolveAddDirs(cwd, home string, raw []string) (dirs, warnings []string) {
	seen := map[string]bool{}
	for _, d := range raw {
		d = strings.TrimSpace(d)
		if d == "" || strings.ContainsRune(d, 0) {
			continue
		}
		switch {
		case d == "~":
			d = home
		case strings.HasPrefix(d, "~/"):
			d = filepath.Join(home, d[2:])
		case !filepath.IsAbs(d):
			d = filepath.Join(cwd, d)
		}
		abs := filepath.Clean(d)
		fi, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			warnings = append(warnings, fmt.Sprintf("%s is not a directory. Did you mean to add the parent directory %s?", abs, filepath.Dir(abs)))
			continue
		}
		if cwd != "" && (abs == filepath.Clean(cwd) || strings.HasPrefix(abs, filepath.Clean(cwd)+string(filepath.Separator))) {
			continue
		}
		if !seen[abs] {
			seen[abs] = true
			dirs = append(dirs, abs)
		}
	}
	return dirs, warnings
}
