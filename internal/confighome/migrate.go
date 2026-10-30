package confighome

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Migration bookkeeping files inside the root.
const (
	// migratedMarker records that the home-directory data was moved (or that
	// whatever could not be moved was reported). Its presence makes Migrate a
	// single stat on every later start.
	migratedMarker = ".home-data-migrated"
	// migrateLock is held while one process moves the data, so two gocode
	// processes starting at once never both move it.
	migrateLock = ".home-data-migrate.lock"
	// staleLockAge is how old a lock file must be before it is treated as left
	// behind by a crashed process and taken over.
	staleLockAge = 2 * time.Minute
)

// keptInLegacyDir lists entries of ~/.gocode that stay where they are: they are
// the home directory's own project-level configuration (what a session started
// in the home directory reads from <cwd>/.gocode), not user-level data.
var keptInLegacyDir = map[string]bool{
	"settings.json":       true,
	"settings.local.json": true,
	"gocode.md":           true,
	"rules":               true,
	"worktrees":           true,
	"launch.json":         true,
	"agent-memory":        true,
	"agent-memory-local":  true,
}

// MigrateOptions configures Migrate.
type MigrateOptions struct {
	// Root is the resolved root (Resolve). Required.
	Root string
	// Home is the home directory holding the former locations. Required.
	Home string
	// Overridden reports that GOCODE_CONFIG_DIR selected Root. The data in the
	// home directory belongs to the default root then, so nothing is moved.
	Overridden bool
	// Rename replaces os.Rename (tests simulate a cross-device failure).
	Rename func(oldpath, newpath string) error
	// LockWait bounds how long a second process waits for a migration running
	// in another process to finish. Zero means 10 seconds.
	LockWait time.Duration
}

// MigrateResult reports what Migrate did.
type MigrateResult struct {
	// Moved lists the top-level items moved into the root (by name).
	Moved []string
	// Kept lists source paths left in place because the root already had an
	// item of that name, or because moving them failed.
	Kept []string
	// Notice is the one-line message for the user ("" when nothing happened).
	Notice string
}

// Migrate moves the contents of ~/.gocode and the ~/.gocode.json state file
// into the root, once.
//
// Rules:
//   - Nothing happens when the marker file exists in the root, when neither
//     former location exists, when GOCODE_CONFIG_DIR is set, or when the root
//     and ~/.gocode are the same directory or one contains the other.
//   - Each entry of ~/.gocode is moved into the root, except the home
//     directory's project-level configuration (keptInLegacyDir). Directories
//     present on both sides are merged file by file. An existing file in the
//     root is never overwritten: the source file stays in ~/.gocode and is
//     listed once in the notice.
//   - A move uses rename; when that fails (for example across devices) the
//     item is copied with its permission bits and modification time, the
//     copy is verified by size and SHA-256, and only then is the source
//     removed.
//   - ~/.gocode.json becomes <root>/.gocode.json. When both exist the two are
//     merged: every project entry from both is kept, a folder trusted in
//     either stays trusted, and for any other key the root's value wins. A
//     file that is not a JSON object is left in place and reported.
//   - Empty directories left behind, including ~/.gocode itself, are removed.
//   - A lock file in the root serialises concurrent starts; the marker is
//     written when the run completes.
func Migrate(opts MigrateOptions) MigrateResult {
	var res MigrateResult
	if opts.Root == "" || opts.Home == "" || opts.Overridden {
		return res
	}
	root := filepath.Clean(opts.Root)
	marker := filepath.Join(root, migratedMarker)
	if _, err := os.Lstat(marker); err == nil {
		return res
	}
	legacyDir := LegacyDir(opts.Home)
	legacyState := LegacyStatePath(opts.Home)
	if !exists(legacyDir) && !exists(legacyState) {
		return res
	}
	if overlaps(root, legacyDir) {
		return res
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return res
	}
	unlock, ok := acquireLock(filepath.Join(root, migrateLock), opts.LockWait)
	if !ok {
		return res
	}
	defer unlock()
	// Another process may have finished while this one waited for the lock.
	if _, err := os.Lstat(marker); err == nil {
		return res
	}

	m := &mover{rename: opts.Rename}
	if m.rename == nil {
		m.rename = os.Rename
	}

	if info, err := os.Lstat(legacyDir); err == nil {
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			// A symlinked ~/.gocode points somewhere the user chose; it is
			// left alone rather than emptied through the link.
			m.kept = append(m.kept, legacyDir)
		case info.IsDir():
			entries, _ := os.ReadDir(legacyDir)
			for _, e := range entries {
				if keptInLegacyDir[e.Name()] {
					continue
				}
				before := m.moved
				m.move(filepath.Join(legacyDir, e.Name()), filepath.Join(root, e.Name()))
				if m.moved > before {
					res.Moved = append(res.Moved, e.Name())
				}
			}
			// Removes ~/.gocode only when it is now empty.
			_ = os.Remove(legacyDir)
		}
	}

	if exists(legacyState) {
		if m.moveState(legacyState, StatePath(root)) {
			res.Moved = append(res.Moved, legacyStateName)
		}
	}

	res.Kept = m.kept
	if len(res.Moved) > 0 || len(res.Kept) > 0 {
		res.Notice = notice(opts.Home, root, res)
	}
	writeMarker(marker, legacyDir)
	return res
}

// overlaps reports whether a and b are the same directory or one contains the
// other (compared after resolving symlinks where possible).
func overlaps(a, b string) bool {
	a, b = realOrClean(a), realOrClean(b)
	return within(a, b) || within(b, a)
}

func realOrClean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	} else if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(r, filepath.Base(p))
	}
	return filepath.Clean(p)
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func exists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}

// acquireLock creates the lock file exclusively, waiting up to wait for a
// concurrent holder and taking over a lock older than staleLockAge.
func acquireLock(path string, wait time.Duration) (func(), bool) {
	if wait <= 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
			_ = f.Close()
			return func() { _ = os.Remove(path) }, true
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, false
		}
		if info, serr := os.Stat(path); serr == nil && time.Since(info.ModTime()) > staleLockAge {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeMarker(path, legacyDir string) {
	body := fmt.Sprintf("moved from %s at %s\n", legacyDir, time.Now().UTC().Format(time.RFC3339))
	_ = os.WriteFile(path, []byte(body), 0o600)
}

// notice renders the one-line user message.
func notice(home, root string, res MigrateResult) string {
	from := filepath.Join("~", legacyDirName)
	if len(res.Moved) == 1 && res.Moved[0] == legacyStateName {
		from = filepath.Join("~", legacyStateName)
	}
	to := tildePath(home, root)
	if len(res.Moved) == 0 {
		return fmt.Sprintf("gocode: data in %s was not moved to %s because the same names already exist there: %s", from, to, keptList(home, res.Kept))
	}
	msg := fmt.Sprintf("gocode: moved data from %s to %s", from, to)
	if len(res.Kept) > 0 {
		msg += fmt.Sprintf(" (left in place, already present there: %s)", keptList(home, res.Kept))
	}
	return msg
}

func keptList(home string, kept []string) string {
	const max = 3
	names := make([]string, 0, max)
	for i, k := range kept {
		if i == max {
			break
		}
		names = append(names, tildePath(home, k))
	}
	s := strings.Join(names, ", ")
	if len(kept) > max {
		s += fmt.Sprintf(" and %d more", len(kept)-max)
	}
	return s
}

func tildePath(home, p string) string {
	if home == "" {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return p
}

// mover moves files and directory trees without overwriting anything.
type mover struct {
	rename func(oldpath, newpath string) error
	kept   []string
	// moved counts files, symlinks and whole trees moved so far.
	moved int
}

// move moves src to dst. dst is never overwritten: when it exists, directories
// are merged entry by entry and any other clash leaves src in place.
func (m *mover) move(src, dst string) {
	sinfo, err := os.Lstat(src)
	if err != nil {
		return
	}
	dinfo, derr := os.Lstat(dst)
	if derr == nil {
		if sinfo.IsDir() && dinfo.IsDir() {
			entries, err := os.ReadDir(src)
			if err != nil {
				m.kept = append(m.kept, src)
				return
			}
			for _, e := range entries {
				m.move(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			}
			_ = os.Remove(src) // only succeeds when everything moved
			return
		}
		m.kept = append(m.kept, src)
		return
	}
	if !errors.Is(derr, fs.ErrNotExist) {
		m.kept = append(m.kept, src)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		m.kept = append(m.kept, src)
		return
	}
	if err := m.rename(src, dst); err == nil {
		m.moved++
		return
	}
	// Rename failed (another device, or a platform that refuses it): copy,
	// verify, then remove the source. A destination that appeared meanwhile
	// is a clash, not something to overwrite.
	if exists(dst) {
		m.kept = append(m.kept, src)
		return
	}
	if err := m.copyMove(src, dst, sinfo); err != nil {
		m.kept = append(m.kept, src)
	}
}

// copyMove copies src to the absent dst, verifies it, and removes src.
func (m *mover) copyMove(src, dst string, info fs.FileInfo) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		return m.dropSource(src, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		before := len(m.kept)
		for _, e := range entries {
			m.move(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
		}
		_ = os.Chmod(dst, info.Mode().Perm())
		_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
		if len(m.kept) > before {
			// The children that could not move were recorded already.
			return nil
		}
		_ = os.Remove(src)
		return nil
	case info.Mode().IsRegular():
		if err := copyFileVerified(src, dst, info); err != nil {
			_ = os.Remove(dst)
			return err
		}
		return m.dropSource(src, dst)
	default:
		return fmt.Errorf("%s: not a regular file, directory or symlink", src)
	}
}

// dropSource removes src after it was copied to dst. When src cannot be
// removed (a file held open on Windows, say) the copy is removed instead, so
// the item exists in exactly one place.
func (m *mover) dropSource(src, dst string) error {
	if err := os.Remove(src); err != nil {
		_ = os.Remove(dst)
		return err
	}
	m.moved++
	return nil
}

// copyFileVerified copies a regular file to the absent dst with the source's
// permission bits and modification time, then checks size and SHA-256.
func copyFileVerified(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != info.Size() {
		return fmt.Errorf("%s: copied %d of %d bytes", src, n, info.Size())
	}
	sum, size, err := hashFile(dst)
	if err != nil {
		return err
	}
	if size != n || !bytes.Equal(sum, h.Sum(nil)) {
		return fmt.Errorf("%s: copy does not match the source", src)
	}
	_ = os.Chmod(dst, info.Mode().Perm())
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

func hashFile(p string) ([]byte, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return h.Sum(nil), n, err
}

// moveState moves or merges the former state file into dst. It reports
// whether the source's content now lives in dst (and the source is gone).
func (m *mover) moveState(src, dst string) bool {
	if !exists(dst) {
		before := m.moved
		m.move(src, dst)
		return m.moved > before
	}
	oldObj, err1 := readObject(src)
	newObj, err2 := readObject(dst)
	if err1 != nil || err2 != nil {
		m.kept = append(m.kept, src)
		return false
	}
	merged := MergeState(newObj, oldObj)
	info, err := os.Stat(dst)
	if err != nil {
		m.kept = append(m.kept, src)
		return false
	}
	if err := writeObjectAtomic(dst, merged, info.Mode().Perm()); err != nil {
		m.kept = append(m.kept, src)
		return false
	}
	if err := os.Remove(src); err != nil {
		m.kept = append(m.kept, src)
		return false
	}
	return true
}

// MergeState merges the former state object into the current one and returns
// the result (current is modified in place). Keys only in old are added; for a
// key in both, current wins — except "projects", which is merged per project:
// every project entry from both is kept, fields only in the old entry are
// added, and hasTrustDialogAccepted is true when either side has it true.
func MergeState(current, old map[string]any) map[string]any {
	if current == nil {
		current = map[string]any{}
	}
	for k, v := range old {
		cur, have := current[k]
		if !have {
			current[k] = v
			continue
		}
		if k != "projects" {
			continue
		}
		curProjects, ok1 := cur.(map[string]any)
		oldProjects, ok2 := v.(map[string]any)
		if !ok1 || !ok2 {
			continue
		}
		for key, ov := range oldProjects {
			cv, have := curProjects[key]
			if !have {
				curProjects[key] = ov
				continue
			}
			ce, ok1 := cv.(map[string]any)
			oe, ok2 := ov.(map[string]any)
			if !ok1 || !ok2 {
				continue
			}
			for f, fv := range oe {
				if _, have := ce[f]; !have {
					ce[f] = fv
				}
			}
			if b, _ := oe["hasTrustDialogAccepted"].(bool); b {
				ce["hasTrustDialogAccepted"] = true
			}
		}
	}
	return current
}

func readObject(p string) (map[string]any, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

func writeObjectAtomic(path string, obj map[string]any, perm fs.FileMode) error {
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gocode-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		return errors.Join(werr, cerr)
	}
	_ = os.Chmod(name, perm)
	if err := replaceFile(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// replaceFile renames tmp over path. Windows refuses to rename onto an
// existing file, so there the old file is moved aside first and restored if
// the rename still fails.
func replaceFile(tmp, path string) error {
	if err := os.Rename(tmp, path); err == nil {
		return nil
	}
	aside := path + ".old"
	_ = os.Remove(aside)
	if err := os.Rename(path, aside); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Rename(aside, path)
		return err
	}
	_ = os.Remove(aside)
	return nil
}

// LegacyLeftovers lists the entries of ~/.gocode that hold user-level data
// still outside the root (everything Migrate would move). It is empty when
// the directory is gone or holds only the home folder's project settings.
func LegacyLeftovers(home string) []string {
	dir := LegacyDir(home)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !keptInLegacyDir[e.Name()] {
			out = append(out, e.Name())
		}
	}
	return out
}
