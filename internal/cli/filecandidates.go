package cli

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/ui"
)

// This file builds the "@" file-mention candidate list that feeds the composer
// file menu (ui.SetFilesMsg) and keeps it fresh.
//
// The list is a bounded, sorted set of file + directory paths (relative to the
// cwd, "/"-separated) chosen by the same walker as the Glob and Grep tools:
// nested .gitignore files, .ignore, .rgignore, .git/info/exclude, the global
// git excludes file and the search.includeHidden / respectGitignore settings
// all apply, plus a small set of always-skipped build/vendor directories.
//
// Refresh policy (fileIndex): the list is built at interactive startup off the
// UI goroutine, then rebuilt in the background when the user opens an "@"
// mention and the snapshot is older than fileIndexMaxAge, and (debounced)
// after tool calls that can create, delete or rename files. Typing never waits
// on a walk: the menu keeps the previous list until the new one arrives, and an
// identical rebuild is not re-sent. Every walk is bounded by
// maxFileCandidates and fileIndexWalkTimeout and is cancelled on shutdown.

// maxFileCandidates bounds the index so a huge tree never blows memory/time;
// once reached the walk stops. 10k paths is far more than any menu will
// window, but keeps the whole set available for fuzzy filtering.
const maxFileCandidates = 10000

// alwaysSkipDirs are directory names pruned from the walk regardless of the
// ignore files: the VCS metadata and the usual heavyweight build/vendor trees.
var alwaysSkipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	"dist":         {},
	"build":        {},
}

// buildFileCandidates walks root into a bounded, sorted []ui.FileCandidate for
// the "@" menu. Directories are included (IsDir true). Paths are relative to
// root with "/" separators and no trailing slash; root itself is not emitted.
// The walk is breadth-first, so under the cap the shallow entries are the ones
// kept. A missing or unreadable root, or a cancelled ctx, yields what was
// collected so far (possibly nothing).
func buildFileCandidates(ctx context.Context, root string, opt filetools.ListOptions) []ui.FileCandidate {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil
		}
		root = wd
	}
	opt.SkipDirs = alwaysSkipDirs
	opt.Limit = maxFileCandidates
	var out []ui.FileCandidate
	_, _ = filetools.ListPaths(ctx, root, opt, func(rel string, isDir bool) bool {
		out = append(out, ui.FileCandidate{Path: rel, IsDir: isDir})
		return true
	})
	sortFileCandidates(out)
	return out
}

// sortFileCandidates orders the list shallow-paths-first (fewer "/" segments),
// then directories before files at the same depth, then alphabetically. Shallow
// entries are the ones a user most often mentions, so they lead the empty-query
// menu.
func sortFileCandidates(cs []ui.FileCandidate) {
	depth := func(p string) int { return strings.Count(p, "/") }
	sort.SliceStable(cs, func(i, j int) bool {
		di, dj := depth(cs[i].Path), depth(cs[j].Path)
		if di != dj {
			return di < dj
		}
		if cs[i].IsDir != cs[j].IsDir {
			return cs[i].IsDir // dirs first
		}
		return cs[i].Path < cs[j].Path
	})
}

// Refresh tuning.
const (
	// fileIndexMaxAge is how old a snapshot may be before opening an "@"
	// mention rebuilds it.
	fileIndexMaxAge = 5 * time.Second
	// fileIndexDebounce coalesces the rebuilds requested by a burst of
	// file-changing tool calls.
	fileIndexDebounce = 300 * time.Millisecond
	// fileIndexWalkTimeout bounds one walk of a huge or slow tree.
	fileIndexWalkTimeout = 10 * time.Second
	// fileIndexSlowFactor stretches the "@" staleness window for a tree whose
	// walk is slow, so opening the menu repeatedly does not keep rescanning it.
	fileIndexSlowFactor = 10
)

// fileIndex owns the "@" candidate snapshot and its background rebuilds. All
// methods are safe for concurrent use and never block on a walk; a nil
// *fileIndex is a no-op.
type fileIndex struct {
	root    string
	options func() filetools.ListOptions // nil: hidden shown, ignore files respected
	send    func([]ui.FileCandidate)

	maxAge   time.Duration
	debounce time.Duration
	timeout  time.Duration

	mu       sync.Mutex
	ctx      context.Context
	stop     context.CancelFunc
	running  bool
	again    bool          // a rebuild was requested while one was running
	builtAt  time.Time     // when the current snapshot finished
	lastScan time.Duration // how long that walk took
	timer    *time.Timer
	last     []ui.FileCandidate
	sent     bool
	wg       sync.WaitGroup
}

// newFileIndex returns an index for root that delivers each changed snapshot
// through send (called from a background goroutine).
func newFileIndex(root string, options func() filetools.ListOptions, send func([]ui.FileCandidate)) *fileIndex {
	ctx, stop := context.WithCancel(context.Background())
	return &fileIndex{
		root:     root,
		options:  options,
		send:     send,
		maxAge:   fileIndexMaxAge,
		debounce: fileIndexDebounce,
		timeout:  fileIndexWalkTimeout,
		ctx:      ctx,
		stop:     stop,
	}
}

// Refresh starts a rebuild now (or right after the one in flight).
func (x *fileIndex) Refresh() {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.startLocked()
}

// RefreshIfStale rebuilds when the snapshot is older than the staleness window
// and no rebuild is already running or scheduled. It is called when an "@"
// mention opens.
func (x *fileIndex) RefreshIfStale() {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.running || x.timer != nil {
		return
	}
	age := max(x.maxAge, fileIndexSlowFactor*x.lastScan)
	if !x.builtAt.IsZero() && time.Since(x.builtAt) < age {
		return
	}
	x.startLocked()
}

// Invalidate schedules a debounced rebuild: the files on disk may have changed
// (a Write, an Edit, a shell command).
func (x *fileIndex) Invalidate() {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.ctx.Err() != nil {
		return
	}
	if x.timer != nil {
		x.timer.Reset(x.debounce)
		return
	}
	x.timer = time.AfterFunc(x.debounce, func() {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.timer = nil
		x.startLocked()
	})
}

// Close cancels any walk in flight and pending rebuilds, and waits for the
// background goroutine to finish.
func (x *fileIndex) Close() {
	if x == nil {
		return
	}
	x.mu.Lock()
	x.stop()
	if x.timer != nil {
		x.timer.Stop()
		x.timer = nil
	}
	x.mu.Unlock()
	x.wg.Wait()
}

// startLocked launches a walk, or marks one to follow the walk in flight.
func (x *fileIndex) startLocked() {
	if x.ctx.Err() != nil {
		return
	}
	if x.running {
		x.again = true
		return
	}
	x.running = true
	x.wg.Add(1)
	go x.run()
}

// run performs walks until no further rebuild was requested meanwhile.
func (x *fileIndex) run() {
	defer x.wg.Done()
	for {
		opt := filetools.ListOptions{IncludeHidden: true, RespectIgnore: true}
		if x.options != nil {
			opt = x.options()
		}
		ctx, cancel := context.WithTimeout(x.ctx, x.timeout)
		started := time.Now()
		files := buildFileCandidates(ctx, x.root, opt)
		cancel()

		x.mu.Lock()
		if x.ctx.Err() != nil {
			x.running = false
			x.mu.Unlock()
			return
		}
		x.builtAt = time.Now()
		x.lastScan = x.builtAt.Sub(started)
		changed := !x.sent || !slices.Equal(files, x.last)
		if changed {
			x.last, x.sent = files, true
		}
		x.mu.Unlock()

		// Deliver outside the lock: send may wait briefly on the UI queue.
		if changed && x.send != nil {
			x.send(files)
		}

		x.mu.Lock()
		if !x.again || x.ctx.Err() != nil {
			x.running = false
			x.mu.Unlock()
			return
		}
		x.again = false
		x.mu.Unlock()
	}
}

// fileChangingTools are the tools whose calls may create, delete or rename
// files, so the "@" list is rebuilt after them.
var fileChangingTools = map[string]bool{
	"Write":        true,
	"Edit":         true,
	"MultiEdit":    true,
	"NotebookEdit": true,
	"Bash":         true,
	"PowerShell":   true,
}

// toolChangesFiles reports whether a finished tool call warrants an "@" list
// rebuild. A failed shell command may still have written files; a failed edit
// has not.
func toolChangesFiles(name string, isError bool) bool {
	if !fileChangingTools[name] {
		return false
	}
	return !isError || name == "Bash" || name == "PowerShell"
}
