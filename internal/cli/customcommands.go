package cli

import (
	"path/filepath"

	"github.com/umairge66/gocode/internal/commands"
)

// customCommandDirs lists the directories custom slash commands are loaded from,
// in registration order: user scope (<gocode root>/commands, then
// ~/.claude/commands for Claude Code interop), then project scope (<cwd>/.gocode/commands, then
// <cwd>/.claude/commands). The registry resolves name clashes by source priority
// (project over user); within a scope the .gocode copy is registered first and
// so wins a tie — the same .gocode-over-.claude rule skills use.
func customCommandDirs(configDir, home, cwd string) []struct {
	dir string
	src commands.Source
} {
	var out []struct {
		dir string
		src commands.Source
	}
	add := func(dir string, src commands.Source) {
		out = append(out, struct {
			dir string
			src commands.Source
		}{dir, src})
	}
	if configDir != "" {
		add(filepath.Join(configDir, "commands"), commands.SourceUser)
	}
	if home != "" {
		add(filepath.Join(home, ".claude", "commands"), commands.SourceUser)
	}
	if cwd != "" && cwd != home {
		add(filepath.Join(cwd, ".gocode", "commands"), commands.SourceProject)
		add(filepath.Join(cwd, ".claude", "commands"), commands.SourceProject)
	}
	return out
}

// loadCustomCommands loads the user's and project's markdown slash commands
// into reg (created when nil) and returns it. A file reachable through two of
// the directories (symlink / hard link) is loaded once. Missing directories are
// simply skipped.
func loadCustomCommands(reg *commands.Registry, configDir, home, cwd string) *commands.Registry {
	var all []*commands.Command
	for _, d := range customCommandDirs(configDir, home, cwd) {
		cmds, err := commands.LoadDir(d.dir, d.src)
		if err != nil {
			continue
		}
		all = append(all, cmds...)
	}
	all = commands.DedupeByFile(all)
	if len(all) == 0 {
		return reg
	}
	if reg == nil {
		reg = commands.NewRegistry()
	}
	for _, c := range all {
		reg.Add(c)
	}
	return reg
}
