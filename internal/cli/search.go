package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	toolexec "github.com/umairge66/gocode/internal/tools/exec"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/ugrep"
)

// This file wires the search engine (bundled ugrep, a configured or system
// ugrep, or the built-in Go engine) from the user's settings:
//
//	"respectGitignore": bool                  (top level; default per tool)
//	"search": {
//	  "useUgrep": bool,          default true — false: built-in Go engine,
//	                             the Grep tool, and the system grep in Bash
//	  "ugrepPath": string,       explicit ugrep (GOCODE_UGREP_PATH overrides)
//	  "preferSystemUgrep": bool, default false — ugrep from PATH over bundled
//	  "includeHidden": bool      default true — search hidden files/dirs
//	}
//
// Settings from earlier releases are still accepted: "useRipgrep": false is
// read as "useUgrep": false when useUgrep is not set; "ripgrepPath" and
// "preferSystemRipgrep" are ignored. Either way the session starts with a
// one-line note naming the keys (legacySearchNote).
//
// With a usable ugrep, `grep` in the model's Bash commands (and in "!" shell
// mode and command "!"-blocks) runs ugrep and the Grep tool is not offered;
// see sessionUgrepShadow. Settings are read when the session starts; the
// /search dialog applies engine changes to the running session's Grep and
// Glob tools, while the Bash `grep` and the tool list follow from the next
// session. Nothing here touches the network.

// Search settings keys of earlier releases.
const (
	legacyUseRipgrep          = "useRipgrep"
	legacyRipgrepPath         = "ripgrepPath"
	legacyPreferSystemRipgrep = "preferSystemRipgrep"
)

// mergedEffectiveSettings merges the active settings sources (low→high
// precedence, last wins). The "search" object is merged key by key so a
// project file can override one search key without dropping the others.
func mergedEffectiveSettings(env *Env) map[string]any {
	settings := map[string]any{}
	search := map[string]any{}
	ctx := settingsContext(env)
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		for k, v := range or.Result.Settings {
			if k == "search" {
				if m, ok := v.(map[string]any); ok {
					for sk, sv := range m {
						search[sk] = sv
					}
				}
				continue
			}
			settings[k] = v
		}
	}
	if len(search) > 0 {
		settings["search"] = search
	}
	return settings
}

// searchSettingsFrom extracts the search settings from merged settings.
func searchSettingsFrom(settings map[string]any) filetools.SearchSettings {
	var s filetools.SearchSettings
	optBool := func(m map[string]any, k string) *bool {
		if b, ok := m[k].(bool); ok {
			return &b
		}
		return nil
	}
	s.RespectGitignore = optBool(settings, "respectGitignore")
	if m, ok := settings["search"].(map[string]any); ok {
		s.UseUgrep = optBool(m, "useUgrep")
		if s.UseUgrep == nil {
			// Turning the external engine off keeps its meaning.
			if old := optBool(m, legacyUseRipgrep); old != nil && !*old {
				s.UseUgrep = old
			}
		}
		s.IncludeHidden = optBool(m, "includeHidden")
		if p, ok := m["ugrepPath"].(string); ok {
			s.UgrepPath = p
		}
		if b := optBool(m, "preferSystemUgrep"); b != nil {
			s.PreferSystemUgrep = *b
		}
	}
	return s
}

// legacySearchNote is the startup note for search settings of earlier
// releases, or "" when none is set.
func legacySearchNote(settings map[string]any) string {
	m, ok := settings["search"].(map[string]any)
	if !ok {
		return ""
	}
	var keys []string
	for _, k := range []string{legacyUseRipgrep, legacyRipgrepPath, legacyPreferSystemRipgrep} {
		if _, set := m[k]; set {
			keys = append(keys, "search."+k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	note := "Search now runs on ugrep: " + strings.Join(keys, ", ") + " no longer apply"
	if v, ok := m[legacyUseRipgrep].(bool); ok && !v {
		if _, set := m["useUgrep"]; !set {
			note += " (useRipgrep: false is read as search.useUgrep: false)"
		}
	}
	return note + ". Use search.useUgrep, search.ugrepPath and search.preferSystemUgrep instead, or /search."
}

// newSessionSearchEngine builds the session's search engine from settings
// and the environment. Resolution (and bundled-ugrep extraction) happens on
// first use.
func newSessionSearchEngine(env *Env) *filetools.SearchEngine {
	e := filetools.NewSearchEngine(searchSettingsFrom(mergedEffectiveSettings(env)))
	getenv := envGet(env)
	e.Getenv = getenv
	e.Ugrep = ugrep.Config{
		Getenv:   getenv,
		LookPath: func(name string) (string, error) { return lookPathIn(getenv("PATH"), name) },
		CacheDir: func() (string, error) { return userCacheDir(getenv) },
	}
	e.Cwd = sessionCwd(env)
	return e
}

// sessionUgrepShadow is the ugrep binary the session's Bash `grep` runs, or
// "" to leave grep alone (and keep the Grep tool): no usable ugrep, a session
// started as a local agent, or a Windows host without Git Bash (commands run
// in PowerShell there).
func sessionUgrepShadow(env *Env, engine *filetools.SearchEngine) string {
	getenv := envGet(env)
	if getenv("GOCODE_CODE_ENTRYPOINT") == "local-agent" {
		return ""
	}
	if runtime.GOOS == "windows" && toolexec.FindGitBash(toolexec.DefaultShellEnv()) == "" {
		return ""
	}
	return engine.UgrepPath()
}

// searchEngineSummary is the "Search:" line for /status, /doctor and
// `--version --verbose`: "ugrep 7.8.5 (bundled)", "ugrep 7.8.4 (system
// /usr/bin/ugrep)" or "built-in".
func searchEngineSummary(env *Env) string {
	return newSessionSearchEngine(env).Describe()
}

// bundledUgrepSummary describes the ugrep embedded in this build.
func bundledUgrepSummary() string {
	if v := ugrep.BundledVersion(); v != "" {
		return "ugrep " + v + " (bundled)"
	}
	return "none bundled for this platform"
}

// lookPathIn finds an executable named name in the PATH list path.
func lookPathIn(path, name string) (string, error) {
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ""}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			p := filepath.Join(dir, name+ext)
			fi, err := os.Stat(p)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
				continue
			}
			return p, nil
		}
	}
	return "", errors.New(name + ": not found on PATH")
}

// userCacheDir is os.UserCacheDir read through getenv, so a session's
// environment (and a test's) decides where the bundled ugrep is extracted.
func userCacheDir(getenv func(string) string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		if d := getenv("LocalAppData"); d != "" {
			return d, nil
		}
		return "", errors.New("%LocalAppData% is not set")
	case "darwin", "ios":
		if h := getenv("HOME"); h != "" {
			return filepath.Join(h, "Library", "Caches"), nil
		}
		return "", errors.New("$HOME is not set")
	default:
		if d := getenv("XDG_CACHE_HOME"); d != "" && filepath.IsAbs(d) {
			return d, nil
		}
		if h := getenv("HOME"); h != "" {
			return filepath.Join(h, ".cache"), nil
		}
		return "", errors.New("neither $XDG_CACHE_HOME nor $HOME is set")
	}
}
