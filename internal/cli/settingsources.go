package cli

import (
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// --setting-sources (user, project, local) selects which opt-in scopes a
// session reads. The settings files are gated where they load
// (settingsload.Context.SourceEnabled, wired by settingsContext); this file
// gates the other per-scope inputs the same way:
//
//   - user:    <gocode root>/gocode.md and rules, user skills (gocode root and
//     ~/.claude/skills), user agents and user commands;
//   - project: gocode.md / .gocode/gocode.md and .gocode/rules on the project
//     walk, project skills, agents and commands;
//   - local:   gocode.local.md (and settings.local.json).
//
// Managed settings, managed memory and skills, --settings, --agents and plugins
// are not gated.

// settingSourceEnabled reports whether the session reads src.
func settingSourceEnabled(env *Env, src settingspolicy.Source) bool {
	if env == nil || !env.SettingSourcesSet {
		return true
	}
	for _, s := range env.SettingSources {
		if s == src {
			return true
		}
	}
	return false
}

// scopedPaths are the session paths with the scopes --setting-sources leaves
// out blanked: configDir / home for user, cwd for project.
type scopedPaths struct {
	// configDir is the gocode root for user skills, agents, commands and memory
	// ("" when the user scope is off).
	configDir string
	// home is the home directory for the ~/.claude user scopes ("" when off).
	home string
	// projectCwd is the project root for project skills, agents and commands
	// ("" when the project scope is off).
	projectCwd string
}

// sourceScopedPaths applies the --setting-sources selection to paths.
func sourceScopedPaths(env *Env, paths sessionPaths) scopedPaths {
	sp := scopedPaths{configDir: paths.configDir, home: paths.home, projectCwd: paths.cwd}
	if !settingSourceEnabled(env, settingspolicy.SourceUser) {
		sp.configDir, sp.home = "", ""
	}
	if !settingSourceEnabled(env, settingspolicy.SourceProject) {
		sp.projectCwd = ""
	}
	return sp
}

// commandsCwd is the project root custom commands load from: like
// customCommandDirs, the home directory is never a project.
func (sp scopedPaths) commandsCwd(paths sessionPaths) string {
	if sp.projectCwd == paths.home {
		return ""
	}
	return sp.projectCwd
}
