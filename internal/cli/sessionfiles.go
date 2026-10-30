package cli

import (
	"path/filepath"
	"runtime"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/sandbox"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// newSessionFileChecks builds the session-specific file-write checks the
// toolreg Decider applies to Write / Edit / MultiEdit / NotebookEdit from the
// session's own paths:
// the config home (agent-memory user root, seed-admin, the bg job folder),
// the former per-user directory ~/.gocode, the working
// directory and every settings source file.
//
// Only local paths are read; nothing leaves the machine.
//
// addDirs are the session's resolved --add-dir directories; they and the
// permissions.additionalDirectories setting become additional working
// directories.
func newSessionFileChecks(env *Env, addDirs ...string) *pathguard.SessionFiles {
	getenv := func(string) string { return "" }
	if env != nil && env.Getenv != nil {
		getenv = env.Getenv
	}
	ctx := settingsContext(env)
	f := &pathguard.SessionFiles{
		HomeDir:         ctx.HomeDir,
		OriginalCwd:     ctx.Cwd,
		Cwd:             ctx.Cwd,
		Diskless:        ctx.Diskless,
		Platform:        runtime.GOOS,
		HostCredsFile:   getenv("GOCODE_CODE_HOST_CREDS_FILE"),
		RemoteMemoryDir: getenv("GOCODE_CODE_REMOTE_MEMORY_DIR"),
	}
	if dir, ok := ctx.UserConfigDir(); ok {
		f.ConfigDir = dir
	}
	// The bg job folder: only a background session's own job directory.
	if getenv("GOCODE_CODE_SESSION_KIND") == "bg" {
		f.BackgroundJobDir = getenv("GOCODE_JOB_DIR")
		// A background session has no launch directory, so the permission walk
		// crosses no unreadable launch-directory ancestor unexamined.
		f.NoLaunchAncestry = true
	}
	for _, src := range settingsload.CanonicalSourceOrder {
		if p, ok := ctx.PathForSource(src); ok {
			f.SettingsFiles = append(f.SettingsFiles, p)
		}
	}
	f.ManagedDropInDirs = []string{ctx.ManagedDropInDir()}
	if p, ok := ctx.PathForSource(settingspolicy.SourceFlag); ok && p != "" {
		f.FlagSettingsDir = filepath.Dir(p)
	}
	// permissions.blockReadsOutsideWorkingDirectories: honoured when any
	// settings source turns it on (it only restricts; a restrictive policy
	// key, settingspolicy RestrictivePaths).
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if perms, ok := or.Result.Settings["permissions"].(map[string]any); ok && perms["blockReadsOutsideWorkingDirectories"] == true {
			f.BlockReadsOutsideWorkingDirectories = true
		}
	}
	f.AdditionalWorkingDirs = additionalWorkingDirs(env, addDirs)
	return f
}

// additionalWorkingDirs is the session's additional working directories: the
// --add-dir directories, then permissions.additionalDirectories from the
// settings sources (resolved as the sandbox resolves them), de-duplicated.
func additionalWorkingDirs(env *Env, addDirs []string) []string {
	ctx := settingsContext(env)
	configDir, _ := ctx.UserConfigDir()
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	s := sandbox.ParseSettings(sandboxLayers(ordered, ctx.Cwd, configDir), sandbox.PathContext{Cwd: ctx.Cwd, Home: ctx.HomeDir, ConfigDir: configDir})
	seen := map[string]bool{}
	var out []string
	for _, d := range append(append([]string{}, addDirs...), s.AdditionalDirectories...) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}
