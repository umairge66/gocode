// Package confighome resolves the one per-user directory that holds all of
// gocode's user-level data, and moves data found in the former home-directory
// locations (~/.gocode and ~/.gocode.json) into it once (see Migrate).
//
// Everything user-level lives under this root: settings.json, auth.json, the
// user memory file gocode.md, saved conversations (projects/), file-edit
// history (file-history/), plan files (plans/), user agents, skills, commands
// and output styles, the plugin cache, background-session and daemon state,
// and the global state file.
//
// The root is resolved the same way on every OS:
//
//	GOCODE_CONFIG_DIR set   -> that directory verbatim
//	XDG_CONFIG_HOME set     -> <XDG_CONFIG_HOME>/gocode
//	otherwise               -> <home>/.config/gocode
//
// The global state file (trusted folders and per-project state) is
// <root>/.gocode.json. It keeps the file name it had in the home directory, so
// tools and setups that look for ".gocode.json" under GOCODE_CONFIG_DIR keep
// working unchanged.
package confighome

import (
	"path/filepath"
	"strings"
)

const (
	// EnvOverride names the environment variable that replaces the root.
	EnvOverride = "GOCODE_CONFIG_DIR"
	// AppDirName is the root's directory name under the XDG config home.
	AppDirName = "gocode"
	// StateFileName is the global state file's name inside the root.
	StateFileName = ".gocode.json"

	// legacyDirName and legacyStateName are the former per-user locations in
	// the home directory, read only by Migrate.
	legacyDirName   = ".gocode"
	legacyStateName = ".gocode.json"
)

// Resolve returns the root for the given environment lookup and home
// directory. ok is false when neither an override, XDG_CONFIG_HOME nor a home
// directory is available (a diskless environment).
func Resolve(getenv func(string) string, home string) (dir string, ok bool) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if v := strings.TrimSpace(getenv(EnvOverride)); v != "" {
		return v, true
	}
	if v := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); v != "" {
		return filepath.Join(v, AppDirName), true
	}
	if home != "" {
		return filepath.Join(home, ".config", AppDirName), true
	}
	return "", false
}

// ResolveDefault is Resolve for callers that always need a directory (control
// socket addresses, background-session state): the home directory comes from
// homeDir when getenv has no HOME, and with neither it falls back to the
// relative ".config/gocode".
func ResolveDefault(getenv func(string) string, homeDir func() (string, error)) string {
	home := ""
	if getenv != nil {
		home = strings.TrimSpace(getenv("HOME"))
	}
	if home == "" && homeDir != nil {
		if h, err := homeDir(); err == nil {
			home = h
		}
	}
	if dir, ok := Resolve(getenv, home); ok {
		return dir
	}
	return filepath.Join(".config", AppDirName)
}

// Overridden reports whether GOCODE_CONFIG_DIR selects the root.
func Overridden(getenv func(string) string) bool {
	return getenv != nil && strings.TrimSpace(getenv(EnvOverride)) != ""
}

// StatePath is the global state file inside root ("" when root is "").
func StatePath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, StateFileName)
}

// LegacyDir is the former per-user data directory, <home>/.gocode. It is
// consulted only to move its contents into the root, and by write guards that
// keep protecting it while it still exists.
func LegacyDir(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, legacyDirName)
}

// LegacyStatePath is the former global state file, <home>/.gocode.json.
func LegacyStatePath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, legacyStateName)
}
