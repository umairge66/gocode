package filetools

import (
	"context"
	"os"
	"path/filepath"
)

// ListOptions steers ListPaths.
type ListOptions struct {
	// IncludeHidden lists entries whose name starts with "." (VCS
	// directories are always skipped).
	IncludeHidden bool
	// RespectIgnore honours .rgignore, .ignore, .gitignore (nested ones
	// included), .git/info/exclude and the global git excludes file.
	RespectIgnore bool
	// SkipDirs are directory names pruned wherever they appear, whatever the
	// ignore files say.
	SkipDirs map[string]struct{}
	// Limit stops the walk after this many entries (0: no limit).
	Limit int
}

// ListOptions returns the hidden / ignore switches a project file listing
// uses for this session: the search settings (search.includeHidden,
// respectGitignore), defaulting to hidden entries shown and ignore files
// respected.
func (e *SearchEngine) ListOptions() ListOptions {
	hidden, respect := e.grepOptions()
	return ListOptions{IncludeHidden: hidden, RespectIgnore: respect}
}

// ListPaths walks root breadth-first — every entry of a directory level
// before the next level, each directory's entries in lexical order — and
// calls visit with each kept file and directory as a "/"-separated path
// relative to root. Hidden entries, ignore files and VCS directories are
// handled exactly as for the Glob and Grep tools. Symlinks are not followed
// and unreadable directories are skipped. A pruned directory (ignored,
// hidden or in SkipDirs) hides its whole subtree.
//
// The walk stops early when visit returns false, when Limit entries were
// visited (complete is false then), or when ctx is done (the context's error
// is returned). complete reports whether every entry was visited.
func ListPaths(ctx context.Context, root string, opt ListOptions, visit func(rel string, isDir bool) bool) (complete bool, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, &os.PathError{Op: "list", Path: root, Err: errNotDir}
	}
	filter, chain := newWalkFilter(root, opt.IncludeHidden, opt.RespectIgnore, newOverrideSet(root, vcsExcludeGlobs()), nil)

	type pending struct {
		abs, rel string
		chain    []*dirIgnores
	}
	queue := []pending{{abs: root, chain: chain}}
	visited := 0
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		dir := queue[0]
		queue[0] = pending{}
		queue = queue[1:]
		entries, rerr := os.ReadDir(dir.abs)
		if rerr != nil {
			continue
		}
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			isDir := e.IsDir()
			if !isDir && !e.Type().IsRegular() {
				continue
			}
			name := e.Name()
			if isDir {
				if _, skip := opt.SkipDirs[name]; skip {
					continue
				}
			}
			abs := filepath.Join(dir.abs, name)
			if !filter.decide(abs, name, isDir, dir.chain) {
				continue
			}
			rel := name
			if dir.rel != "" {
				rel = dir.rel + "/" + name
			}
			if !visit(rel, isDir) {
				return false, nil
			}
			visited++
			if opt.Limit > 0 && visited >= opt.Limit {
				return false, nil
			}
			if isDir {
				next := dir.chain
				if filter.useIgnores {
					next = append(dir.chain[:len(dir.chain):len(dir.chain)], loadDirIgnores(abs))
				}
				queue = append(queue, pending{abs: abs, rel: rel, chain: next})
			}
		}
	}
	return true, nil
}
