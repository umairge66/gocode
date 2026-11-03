// Package settingsload is the filesystem settings loader that
// internal/settingspolicy deferred.
//
// It covers the per-source path resolver, the managed base-dir resolver, the
// settings-filename map, the read+parse+validate flow, the absent/unreadable
// result builders, managed-settings.json plus managed-settings.d drop-in
// scanning, the load-state folding and rawSettingsKeyPresence. Sources are
// ordered low→high: userSettings, projectSettings, localSettings,
// flagSettings, policySettings. The user config dir is the gocode root
// (internal/confighome): GOCODE_CONFIG_DIR / XDG_CONFIG_HOME/gocode /
// HOME/.config/gocode. The admin
// tiers are merged by settingspolicy.MergeAdminTiers.
//
// The policySettings source is not a single file: LoadSource returns the
// composition of the admin tiers (OS MDM tier, managed-settings.json plus its
// drop-ins, HKCU fallback) built by ComposePolicy (policy.go). Remote
// server-managed settings come from gocode's hosted service and are not
// supported by design.
package settingsload

import (
	"path/filepath"
	"runtime"

	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// Settings filenames.
const (
	userSettingsFile    = "settings.json"
	coworkSettingsFile  = "cowork_settings.json"
	projectDirName      = ".gocode"
	projectSettingsFile = "settings.json"
	localSettingsFile   = "settings.local.json"
	managedSettingsFile = "managed-settings.json"
	managedDropInDir    = "managed-settings.d"
)

// Context carries the per-session inputs the path resolver threads through
// . Zero values are
// valid; the Env hook defaults to os.Getenv and GOOS to runtime.GOOS when the
// resolver needs them (see UserConfigDir / ManagedBaseDir).
type Context struct {
	// HomeDir is the user's home directory. Used by
	// UserConfigDir's HOME fallback and by Env lookups' XDG defaults.
	HomeDir string
	// Cwd is the session working directory. Empty means "no
	// working directory", which disables the
	// project/local sources.
	Cwd string
	// ProjectConfigDir, when non-empty, overrides the directory the project
	// and local sources live under. When set it
	// also re-enables those sources even without a Cwd.
	ProjectConfigDir string
	// GitRoot is the resolved canonical git root for Cwd, if any. When
	// non-empty the local source is anchored at the git root rather than Cwd.
	// The git-root discovery itself lives in internal/git and is out of scope
	// for this resolver.
	GitRoot string
	// Diskless disables every on-disk source. Used by
	// sandboxed / in-memory sessions.
	Diskless bool
	// FlagPath is the explicit --settings file path, the
	// backing file for the flagSettings source.
	FlagPath string
	// Cowork selects the cowork user-settings filename (also selected by the
	// env GOCODE_CODE_USE_COWORK_PLUGINS).
	Cowork bool

	// Env resolves environment variables. nil uses the
	// process environment.
	Env func(string) string
	// GOOS overrides the OS used to pick the managed base directory. Empty
	// uses runtime.GOOS.
	GOOS string
	// ManagedBaseDirOverride, when non-empty, replaces the per-OS managed base
	// directory (like the env override GOCODE_CODE_MANAGED_SETTINGS_PATH).
	ManagedBaseDirOverride string

	// OSPolicyReader, when non-nil, replaces the platform reader of the
	// OS-managed policy (macOS managed preferences, the Windows registry
	// policy keys, the WSL bridge). Tests inject a fixed readout here; nil
	// uses the platform default (see mdm_reader_*.go).
	OSPolicyReader func() OSPolicyReadout
	// WSLWindowsManagedDirOverride, when non-empty, replaces the DrvFs view of
	// the Windows managed folder.
	WSLWindowsManagedDirOverride string

	// UntrustedProject withholds, in EffectiveSources, the projectSettings /
	// localSettings keys that run code or widen permissions (hooks,
	// statusLine, local mcpServers and .mcp.json approvals, allow/ask rules,
	// additionalDirectories, a loosening defaultMode, sandbox loosening,
	// enabledPlugins, model/provider/fallbackModel) because the user has not
	// trusted the folder. See trust.go.
	UntrustedProject bool
	// LocalSettingsTracked marks settings.local.json as committed to git: its
	// mcpServers are treated as project servers (approval needed) and its
	// .mcp.json approvals are ignored. See trust.go.
	LocalSettingsTracked bool

	// SourcesSet restricts the opt-in sources (user, project, local) to
	// Sources: the --setting-sources selection. A source left out loads as
	// absent everywhere (LoadSource). flagSettings and policySettings always
	// load. False means every source is active.
	SourcesSet bool
	Sources    []settingspolicy.Source
}

// SourceEnabled reports whether src may be read under the --setting-sources
// selection (always true for the flag and policy sources).
func (c *Context) SourceEnabled(src settingspolicy.Source) bool {
	if c == nil || !c.SourcesSet {
		return true
	}
	switch src {
	case settingspolicy.SourceFlag, settingspolicy.SourcePolicy, settingspolicy.SourceManaged:
		return true
	}
	for _, s := range c.Sources {
		if s == src {
			return true
		}
	}
	return false
}

func (c *Context) env(key string) string {
	if c.Env != nil {
		return c.Env(key)
	}
	return osGetenv(key)
}

// userSettingsFilename picks the user-settings filename.
func (c *Context) userSettingsFilename() string {
	if c.Cowork || c.env("GOCODE_CODE_USE_COWORK_PLUGINS") != "" {
		return coworkSettingsFile
	}
	return userSettingsFile
}

// UserConfigDir resolves the gocode root, the one directory that holds every
// user-level file (settings, auth.json, transcripts, file history, plans, the
// global state file, …). See internal/confighome for the rule:
//
//	GOCODE_CONFIG_DIR            -> that directory verbatim
//	XDG_CONFIG_HOME             -> <XDG_CONFIG_HOME>/gocode
//	HOME (Context.HomeDir)      -> <HOME>/.config/gocode
//
// ok is false only when neither an override nor a home directory is available.
func (c *Context) UserConfigDir() (dir string, ok bool) {
	return confighome.Resolve(c.env, c.HomeDir)
}

// ManagedBaseDir resolves the directory holding managed-settings.json and the
// managed-settings.d drop-in directory.
func (c *Context) ManagedBaseDir() string {
	if c.ManagedBaseDirOverride != "" {
		return c.ManagedBaseDirOverride
	}
	goos := c.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	// These literals are written
	// verbatim rather than via filepath.Join so the Windows path keeps its
	// backslashes regardless of the host the loader runs on.
	switch goos {
	case "darwin":
		return `/Library/Application Support/gocode`
	case "windows":
		return `C:\Program Files\gocode`
	default:
		return `/etc/gocode`
	}
}

// projectBaseDir is the directory the project source lives under: the
// ProjectConfigDir override, else Cwd.
func (c *Context) projectBaseDir() string {
	if c.ProjectConfigDir != "" {
		return c.ProjectConfigDir
	}
	return c.Cwd
}

// localBaseDir is the directory the local source lives under: the
// ProjectConfigDir override, else the canonical git root when known, else Cwd.
func (c *Context) localBaseDir() string {
	if c.ProjectConfigDir != "" {
		return c.ProjectConfigDir
	}
	if c.GitRoot != "" {
		return c.GitRoot
	}
	return c.Cwd
}

// projectSourcesEnabled reports whether the project/local sources resolve to a
// path at all: not in a diskless session, and not when there is neither a
// cwd nor a project config dir.
func (c *Context) projectSourcesEnabled() bool {
	if c.Diskless {
		return false
	}
	return c.Cwd != "" || c.ProjectConfigDir != ""
}

// PathForSource returns the on-disk file path backing src.
//
// ok is false when the source has no backing file in this context: a diskless
// session, a project/local source with no project directory, or a flagSettings
// source with no --settings path.
func (c *Context) PathForSource(src settingspolicy.Source) (path string, ok bool) {
	if c.Diskless {
		return "", false
	}
	switch src {
	case settingspolicy.SourceUser:
		dir, have := c.UserConfigDir()
		if !have {
			return "", false
		}
		return filepath.Join(dir, c.userSettingsFilename()), true
	case settingspolicy.SourceProject:
		if !c.projectSourcesEnabled() {
			return "", false
		}
		return filepath.Join(c.projectBaseDir(), projectDirName, projectSettingsFile), true
	case settingspolicy.SourceLocal:
		if !c.projectSourcesEnabled() {
			return "", false
		}
		return filepath.Join(c.localBaseDir(), projectDirName, localSettingsFile), true
	case settingspolicy.SourcePolicy, settingspolicy.SourceManaged:
		return filepath.Join(c.ManagedBaseDir(), managedSettingsFile), true
	case settingspolicy.SourceFlag:
		if c.FlagPath == "" {
			return "", false
		}
		return c.FlagPath, true
	default:
		return "", false
	}
}

// ManagedDropInDir returns the managed-settings.d drop-in directory path.
func (c *Context) ManagedDropInDir() string {
	return filepath.Join(c.ManagedBaseDir(), managedDropInDir)
}
