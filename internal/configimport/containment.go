// This file holds the path-containment checks the importers apply before
// writing: target-path selection, parent-escape detection and the
// symlink-aware "stays inside root" resolvers.

package configimport

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// mcpTargetPath returns where an MCP entry would land: project → <cwd>/.mcp.json;
// user MCP is stored in the Gocode user config and has no single-file target
// here (MCPTargetScope conveys the destination).
func mcpTargetPath(scope Scope, cwd, _ string) string {
	if scope == ScopeProject {
		return filepath.Join(cwd, ".mcp.json")
	}
	return ""
}

// escapesParent reports whether rel escapes its base (absolute, "..", "../…").
func escapesParent(rel string) bool {
	return filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// containedJoin resolves p against base and returns the cleaned absolute path
// only if it stays strictly inside base; otherwise "". It is the pure path-shape
// check; the symlink-aware runtime checks are containedNoSymlink and
// containedReal below.
func containedJoin(base, p string) string {
	resolved := p
	if !filepath.IsAbs(p) {
		resolved = filepath.Join(base, p)
	}
	resolved = filepath.Clean(resolved)
	rel, err := filepath.Rel(base, resolved)
	if err != nil || rel == "" || rel == "." || escapesParent(rel) {
		return ""
	}
	return resolved
}

// containedNoSymlink is the project-scope write/read guard: p must be inside
// root (containedJoin) and no existing path component between root and p may
// be a symlink. A missing component ends the walk successfully (the rest will
// be created). Returns the resolved path, or "" when refused.
func containedNoSymlink(root, p string) string {
	r := containedJoin(root, p)
	if r == "" {
		return ""
	}
	rel, err := filepath.Rel(root, r)
	if err != nil {
		return ""
	}
	cur := root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		st, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return r
			}
			return ""
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return ""
		}
	}
	return r
}

// containedReal is the user-scope source guard: p must be inside root both
// lexically and after resolving symlinks on both sides (so a symlinked skill
// directory pointing outside the tool's config dir is refused). Both must
// exist. Returns the lexical resolved path, or "".
func containedReal(root, p string) string {
	r := containedJoin(root, p)
	if r == "" {
		return ""
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	realP, err := filepath.EvalSymlinks(r)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(realRoot, realP)
	if err != nil || rel == "." || rel == "" || escapesParent(rel) {
		return ""
	}
	return r
}

// isNetworkPath reports a UNC / network path (\\server\share, //server/share).
func isNetworkPath(p string) bool {
	return len(p) >= 2 && (p[0] == '\\' || p[0] == '/') && (p[1] == '\\' || p[1] == '/')
}

// resolveExisting resolves symlinks on the longest existing prefix of p and
// re-appends the missing tail. ok is false when resolution fails.
func resolveExisting(p string) (string, bool) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	var tail []string
	cur := p
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, tail...)...), true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, true
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

// pathWithin reports whether dir is base itself or inside it after symlink
// resolution (case-folded on Windows/macOS). Resolution failure counts as
// "within" — the guard fails closed.
func pathWithin(dir, base string) bool {
	if base == "" {
		return false
	}
	d, ok := resolveExisting(dir)
	if !ok {
		return true
	}
	b, ok := resolveExisting(base)
	if !ok {
		return true
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		d, b = strings.ToLower(d), strings.ToLower(b)
	}
	rel, err := filepath.Rel(b, d)
	if err != nil {
		return false
	}
	return rel == "." || !escapesParent(rel)
}
