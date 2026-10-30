package mods

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/sandbox"
)

// fsScope is where a hooks module's $.fs calls may reach.
//
// Declaring a call in the hooks module (the scan's gate) is not the whole
// permission: rather than reaching any path the gocode process can, every
// $.fs call (read, write, list, exists, stat) is confined to
//
//   - the session's project: its working directory, $.session.root and the
//     additional working directories (--add-dir,
//     permissions.additionalDirectories);
//   - the plugin's own root folder;
//   - the plugin's data folder (${GOCODE_PLUGIN_DATA}).
//
// Paths are resolved fully before the check (absolute, cleaned, every
// existing component's symbolic links followed; a dangling link is refused),
// so neither "../" nor a link inside the project that points outside escapes.
// A write is further refused on the paths the Bash sandbox write-protects
// inside a project (sandbox.IsWriteProtectedInProject: .git hooks/config, the
// .gocode settings/skills/hooks/commands/agents, .mcp.json, shell rc files …)
// and anywhere inside a .git directory.
//
// RESIDUAL: the check and the open are separate steps, so a process that
// swaps a checked path component for a symbolic link in between could still
// redirect one call; the engine never follows links it did not resolve first.
type fsScope struct {
	roots    []scopeRoot // every allowed root, resolved
	projects []string    // the resolved project roots (write protection applies)
}

type scopeRoot struct {
	label    string // as shown in errors
	resolved string
}

// newFSScope builds the scope of one plugin's $.fs calls.
func newFSScope(projects []string, pluginRoot, dataDir string) fsScope {
	var sc fsScope
	seen := map[string]bool{}
	add := func(p, kind string) {
		project := kind == "project"
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		r, err := resolveFSPath(p)
		if err != nil {
			return
		}
		if project {
			sc.projects = append(sc.projects, r)
		}
		if seen[r] {
			return
		}
		seen[r] = true
		sc.roots = append(sc.roots, scopeRoot{label: filepath.Clean(p) + " (" + kind + ")", resolved: r})
	}
	for _, p := range projects {
		add(p, "project")
	}
	add(pluginRoot, "plugin")
	add(dataDir, "plugin data")
	return sc
}

// allowedList names the allowed roots for an error message.
func (sc fsScope) allowedList() string {
	if len(sc.roots) == 0 {
		return "(none)"
	}
	labels := make([]string, len(sc.roots))
	for i, r := range sc.roots {
		labels[i] = r.label
	}
	return strings.Join(labels, ", ")
}

// check resolves p (absolute) and returns the resolved path, or why the call
// is refused.
func (sc fsScope) check(p string, write bool) (string, string) {
	r, err := resolveFSPath(p)
	if err != nil {
		return "", fmt.Sprintf("the path cannot be resolved (%v); a hooks module may only reach %s", err, sc.allowedList())
	}
	inside := false
	for _, root := range sc.roots {
		if within(root.resolved, r) {
			inside = true
			break
		}
	}
	if !inside {
		return "", fmt.Sprintf("%s is outside the folders a hooks module may reach: %s", r, sc.allowedList())
	}
	if write {
		for _, proj := range sc.projects {
			if !within(proj, r) {
				continue
			}
			rel, _ := filepath.Rel(proj, r)
			if sandbox.IsWriteProtectedInProject(rel) || hasGitSegment(rel) {
				return "", fmt.Sprintf("%s is a protected file of the project (git, gocode settings and configuration, shell and editor configuration are never written by a hooks module)", r)
			}
		}
	}
	return r, ""
}

func hasGitSegment(rel string) bool {
	for _, s := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(s, ".git") {
			return true
		}
	}
	return false
}

// resolveFSPath makes p absolute and clean and follows every symbolic link
// in its existing part; the components that do not exist yet are appended
// as they are. A dangling link (it exists but its target does not) is an
// error, since writing through it would create its target wherever it
// points.
func resolveFSPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cur := abs
	var rest []string
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", fmt.Errorf("%s is a symbolic link whose target does not exist", cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}
