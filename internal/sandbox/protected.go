package sandbox

import (
	"path/filepath"
	"strings"
)

// IsWriteProtectedInProject reports whether rel — a path relative to a project
// root (the cwd or an additional working directory), with either separator —
// names a path the sandbox write-protects there: the repository's gocode
// configuration (projectProtected), a .git directory's hooks or config (or a
// .git pointer file) at any depth, the shell/git/editor configuration files
// (dangerousFiles) at any depth, and the .vscode / .idea / .gocode/commands /
// .gocode/agents directories at any depth. Names compare case-insensitively
// (a case-insensitive file system would reach the same file).
//
// It is the same list BuildRuntimeConfig turns into mount denies, for callers
// that write files themselves (the hooks-module engine's $.fs.write).
func IsWriteProtectedInProject(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" {
		return false
	}
	segs := strings.Split(rel, "/")
	under := func(prefix string) bool {
		ps := strings.Split(prefix, "/")
		if len(segs) < len(ps) {
			return false
		}
		for i, p := range ps {
			if !strings.EqualFold(segs[i], p) {
				return false
			}
		}
		return true
	}
	for _, p := range projectProtected {
		if under(p) {
			return true
		}
	}
	for i, s := range segs {
		switch {
		case strings.EqualFold(s, ".git"):
			if i == len(segs)-1 {
				return true // a worktree / submodule pointer file, or .git itself
			}
			if next := segs[i+1]; strings.EqualFold(next, "hooks") || strings.EqualFold(next, "config") {
				return true
			}
		case strings.EqualFold(s, ".vscode"), strings.EqualFold(s, ".idea"):
			return true
		case strings.EqualFold(s, ".gocode") && i+1 < len(segs):
			if next := segs[i+1]; strings.EqualFold(next, "commands") || strings.EqualFold(next, "agents") {
				return true
			}
		}
	}
	base := segs[len(segs)-1]
	for _, n := range dangerousFiles {
		if strings.EqualFold(base, n) {
			return true
		}
	}
	return false
}
