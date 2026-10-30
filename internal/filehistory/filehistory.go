// Package filehistory is the file checkpointing behind /rewind: before the
// agent modifies a file (Write / Edit / MultiEdit / NotebookEdit) the file's
// prior content — or its absence — is backed up, and at every user prompt a
// snapshot records the backup that matches each tracked file at that moment.
// Rewinding to a prompt writes those backups back (deleting the files the
// agent created after it).
//
// Storage is LOCAL-ONLY and nothing here transmits anything:
//
//	<configHome>/file-history/<sessionId>/<sha256(path)[:16]>@v<version>   backups
//	<configHome>/file-history/<sessionId>/snapshots.json                    state
//
// The snapshot list is kept in snapshots.json beside the backups.
//
// A snapshot is keyed by the user message it precedes. Messages carry no ids,
// so a Key is the message's index in the conversation history plus its prompt
// text: both survive /resume unchanged, and a rewound or compacted history
// whose prompts moved simply stops matching the old keys.
package filehistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxSnapshots is how many snapshots a session keeps; older ones are evicted
// (with any backup only they referenced, except a file's version-1 backup,
// which the rewind fallback still needs).
const MaxSnapshots = 100

// MaxFileSize caps the files that are backed up: a larger file is not tracked
// (its edits cannot be rewound).
const MaxFileSize = 10 << 20

// stateFile is the snapshot list persisted beside the backups.
const stateFile = "snapshots.json"

// backupNamePattern matches a backup file name (sha256 prefix + version).
var backupNamePattern = regexp.MustCompile(`^[0-9a-f]{16}(?:[0-9a-f]{48})?@v\d+$`)

// sessionIDPattern restricts the session directory name (no separators, no
// dot segments), so a crafted id can never point outside the root.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Key identifies the user message a snapshot precedes: its index in the
// conversation history and its prompt text.
type Key struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// Backup is one file's recorded state. Absent means the file did not exist (a
// rewind deletes it); otherwise Name is the backup file in the session dir.
type Backup struct {
	Name    string    `json:"backupFileName,omitempty"`
	Absent  bool      `json:"absent,omitempty"`
	Version int       `json:"version"`
	Time    time.Time `json:"backupTime"`
	// RealParentDir is the symlink-resolved parent directory at backup time. A
	// restore refuses to write when the parent now resolves elsewhere.
	RealParentDir string `json:"realParentDir,omitempty"`
}

// Snapshot is the state of every tracked file just before a user message.
type Snapshot struct {
	Key     Key               `json:"key"`
	Backups map[string]Backup `json:"trackedFileBackups"`
	Time    time.Time         `json:"timestamp"`
}

// DiffStats summarizes what a rewind would change (or what a turn changed).
type DiffStats struct {
	// FilesChanged are absolute paths.
	FilesChanged []string
	Insertions   int
	Deletions    int
}

// RewindResult reports a completed rewind.
type RewindResult struct {
	// Changed are the absolute paths restored or deleted.
	Changed []string
	// Failed are the absolute paths whose backup was missing or that could not
	// be written.
	Failed []string
	// SkippedLinks counts paths refused for link safety (a symlink or
	// hard-linked destination, or a parent directory that moved).
	SkippedLinks int
}

// ErrNoFilesRestored is returned (wrapped) when every differing file failed.
var ErrNoFilesRestored = errors.New("no files were restored")

// ErrSnapshotNotFound is returned when no snapshot matches the key.
var ErrSnapshotNotFound = errors.New("The selected snapshot was not found")

// state is the persisted form of a session's history.
type state struct {
	Snapshots []Snapshot `json:"snapshots"`
	Tracked   []string   `json:"trackedFiles"`
}

// Store is one session's file history. It is safe for concurrent use.
type Store struct {
	mu        sync.Mutex
	root      string // <configHome>/file-history
	sessionID string
	cwd       string
	snapshots []Snapshot
	tracked   []string
	isTracked map[string]bool
}

// New opens the file history of sessionID under root (normally
// <configHome>/file-history), loading any state already on disk. cwd is the
// project directory tracked paths are recorded relative to.
func New(root, sessionID, cwd string) *Store {
	s := &Store{root: root, cwd: cwd}
	s.load(sessionID)
	return s
}

// SessionID is the session whose directory the store writes to.
func (s *Store) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// HasSession reports whether sessionID has a saved file history on disk.
func (s *Store) HasSession(sessionID string) bool {
	if !sessionIDPattern.MatchString(sessionID) {
		return false
	}
	_, err := os.Stat(filepath.Join(s.root, sessionID, stateFile))
	return err == nil
}

// Rebind points the store at another session. With carry, the current
// snapshots move along (their backups are hard-linked, or copied, into the new
// session's directory — a forked or rotated conversation keeps its
// checkpoints); without it the new session's own saved history is loaded (a
// resumed session), or the store starts empty.
func (s *Store) Rebind(sessionID string, carry bool) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("FileHistory: invalid session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID == s.sessionID {
		return nil
	}
	if !carry {
		s.load(sessionID)
		return nil
	}
	old := s.sessionDir()
	s.sessionID = sessionID
	dst := s.sessionDir()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	var firstErr error
	for name := range s.referencedLocked() {
		if err := linkOrCopy(filepath.Join(old, name), filepath.Join(dst, name)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := s.saveLocked(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// load replaces the in-memory state with sessionID's saved one (or empty).
// Callers hold mu (or own s exclusively).
func (s *Store) load(sessionID string) {
	s.sessionID = sessionID
	s.snapshots = nil
	s.tracked = nil
	s.isTracked = map[string]bool{}
	if !sessionIDPattern.MatchString(sessionID) {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.sessionDir(), stateFile))
	if err != nil {
		return
	}
	var st state
	if json.Unmarshal(data, &st) != nil {
		return
	}
	for _, snap := range st.Snapshots {
		clean := Snapshot{Key: snap.Key, Time: snap.Time, Backups: map[string]Backup{}}
		for p, b := range snap.Backups {
			// Only well-formed backup names are honoured: a tampered state file
			// can never redirect a restore to read outside the session dir.
			if !b.Absent && !backupNamePattern.MatchString(b.Name) {
				continue
			}
			clean.Backups[p] = b
		}
		s.snapshots = append(s.snapshots, clean)
	}
	for _, p := range st.Tracked {
		if !s.isTracked[p] {
			s.isTracked[p] = true
			s.tracked = append(s.tracked, p)
		}
	}
}

// sessionDir is <root>/<sessionId>.
func (s *Store) sessionDir() string { return filepath.Join(s.root, s.sessionID) }

// saveLocked persists the snapshot list (write to a temp file, then rename).
func (s *Store) saveLocked() error {
	if !sessionIDPattern.MatchString(s.sessionID) {
		return fmt.Errorf("FileHistory: invalid session id")
	}
	dir := s.sessionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state{Snapshots: s.snapshots, Tracked: s.tracked})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, stateFile+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, stateFile)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// trackingPath is the recorded form of path: relative to the project dir when
// inside it, else absolute.
func (s *Store) trackingPath(path string) string {
	abs := s.absPath(path)
	if s.cwd != "" {
		if rel, err := filepath.Rel(s.cwd, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel
		}
	}
	return abs
}

// absPath resolves a tracked path back to an absolute one.
func (s *Store) absPath(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(s.cwd, p)
}

// backupName is sha256(path)[:16]@v<version>.
func backupName(absPath string, version int) string {
	sum := sha256.Sum256([]byte(absPath))
	return hex.EncodeToString(sum[:])[:16] + "@v" + strconv.Itoa(version)
}

// backupPath resolves a backup name inside the session dir, refusing a
// malformed name.
func (s *Store) backupPath(name string) (string, error) {
	if !backupNamePattern.MatchString(name) {
		return "", fmt.Errorf("FileHistory: invalid backup file name")
	}
	return filepath.Join(s.sessionDir(), name), nil
}

// createBackup copies the file at absPath into the session dir as the given
// version, or records it as absent. A non-regular file (symlink, directory,
// device) or one over MaxFileSize is refused.
func (s *Store) createBackup(absPath string, version int) (Backup, error) {
	now := time.Now()
	fi, err := os.Lstat(absPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Backup{Absent: true, Version: version, Time: now, RealParentDir: realParentDir(absPath)}, nil
	}
	if err != nil {
		return Backup{}, err
	}
	if !fi.Mode().IsRegular() {
		return Backup{}, fmt.Errorf("FileHistory: tracked path is not a regular file")
	}
	if fi.Size() > MaxFileSize {
		return Backup{}, fmt.Errorf("FileHistory: %s is larger than %d bytes", absPath, MaxFileSize)
	}
	name := backupName(absPath, version)
	dst, err := s.backupPath(name)
	if err != nil {
		return Backup{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return Backup{}, err
	}
	_ = os.Remove(dst)
	if err := copyRegular(absPath, dst, fi.Mode().Perm()); err != nil {
		return Backup{}, err
	}
	return Backup{Name: name, Version: version, Time: now, RealParentDir: realParentDir(absPath)}, nil
}

// TrackEdit backs up path's current content (version 1) before the agent's
// first modification of it in this session, attaching the backup to the most
// recent snapshot. A path already tracked, or an edit with no snapshot yet
// (no user prompt recorded), is a no-op. Errors leave the file untracked.
func (s *Store) TrackEdit(path string) error {
	if path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snapshots) == 0 {
		return nil
	}
	tp := s.trackingPath(path)
	for _, snap := range s.snapshots {
		if _, ok := snap.Backups[tp]; ok {
			return nil
		}
	}
	b, err := s.createBackup(s.absPath(tp), 1)
	if err != nil {
		return err
	}
	last := &s.snapshots[len(s.snapshots)-1]
	if last.Backups == nil {
		last.Backups = map[string]Backup{}
	}
	last.Backups[tp] = b
	if !s.isTracked[tp] {
		s.isTracked[tp] = true
		s.tracked = append(s.tracked, tp)
	}
	return s.saveLocked()
}

// MakeSnapshot records the state of every tracked file just before the user
// message k: an unchanged file reuses its latest backup, a changed one gets a
// new version, a missing one is recorded absent. The oldest snapshots past
// MaxSnapshots are evicted.
func (s *Store) MakeSnapshot(k Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backups := map[string]Backup{}
	var latest map[string]Backup
	if n := len(s.snapshots); n > 0 {
		latest = s.snapshots[n-1].Backups
	}
	for _, tp := range s.tracked {
		abs := s.absPath(tp)
		prev, hasPrev := latest[tp]
		next := 0
		for _, snap := range s.snapshots {
			if b, ok := snap.Backups[tp]; ok && b.Version > next {
				next = b.Version
			}
		}
		next++
		fi, err := os.Lstat(abs)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if hasPrev && prev.Absent {
				backups[tp] = prev
			} else {
				backups[tp] = Backup{Absent: true, Version: next, Time: time.Now(), RealParentDir: realParentDir(abs)}
			}
			continue
		case err != nil:
			// Keep the previous record.
			if hasPrev {
				backups[tp] = prev
			}
			continue
		}
		if hasPrev && !prev.Absent {
			if bp, perr := s.backupPath(prev.Name); perr == nil && !differs(abs, fi, bp) {
				backups[tp] = prev
				continue
			}
		}
		b, berr := s.createBackup(abs, next)
		if berr != nil {
			if hasPrev {
				backups[tp] = prev
			}
			continue
		}
		backups[tp] = b
	}
	s.snapshots = append(s.snapshots, Snapshot{Key: k, Backups: backups, Time: time.Now()})
	if len(s.snapshots) > MaxSnapshots {
		evicted := s.snapshots[:len(s.snapshots)-MaxSnapshots]
		s.snapshots = append([]Snapshot(nil), s.snapshots[len(s.snapshots)-MaxSnapshots:]...)
		s.evictLocked(evicted)
	}
	return s.saveLocked()
}

// referencedLocked is the set of backup names the live snapshots reference.
func (s *Store) referencedLocked() map[string]bool {
	keep := map[string]bool{}
	for _, snap := range s.snapshots {
		for _, b := range snap.Backups {
			if !b.Absent && b.Name != "" {
				keep[b.Name] = true
			}
		}
	}
	return keep
}

// evictLocked deletes the backups only the evicted snapshots referenced,
// keeping every version-1 backup (the rewind fallback).
func (s *Store) evictLocked(evicted []Snapshot) {
	keep := s.referencedLocked()
	for _, snap := range evicted {
		for tp, b := range snap.Backups {
			if b.Absent || keep[b.Name] {
				continue
			}
			if b.Version == 1 {
				// The fallback needs it: carry it into the oldest kept snapshot
				// when that one does not record the file itself.
				if _, ok := s.snapshots[0].Backups[tp]; !ok {
					s.snapshots[0].Backups[tp] = b
				}
				keep[b.Name] = true
				continue
			}
			if p, err := s.backupPath(b.Name); err == nil {
				_ = os.Remove(p)
			}
		}
	}
}

// CanRestore reports whether a snapshot exists for k.
func (s *Store) CanRestore(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(k) >= 0
}

// Latest is the key of the most recent snapshot (ok=false when there is none).
func (s *Store) Latest() (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snapshots) == 0 {
		return Key{}, false
	}
	return s.snapshots[len(s.snapshots)-1].Key, true
}

// findLocked is the index of the LAST snapshot for k, or -1.
func (s *Store) findLocked(k Key) int {
	for i := len(s.snapshots) - 1; i >= 0; i-- {
		if s.snapshots[i].Key == k {
			return i
		}
	}
	return -1
}

// targetLocked is the backup a rewind to snapshot i applies to tp: the
// snapshot's own record, else the file's version-1 backup (it was first
// edited after that snapshot, so its pre-edit state is the state then).
func (s *Store) targetLocked(i int, tp string) (Backup, bool) {
	if b, ok := s.snapshots[i].Backups[tp]; ok {
		return b, true
	}
	for _, snap := range s.snapshots {
		if b, ok := snap.Backups[tp]; ok && b.Version == 1 {
			return b, true
		}
	}
	return Backup{}, false
}

// readBackup returns a backup's content (nil, true for an absent file).
func (s *Store) readBackup(b Backup) ([]byte, bool, error) {
	if b.Absent {
		return nil, false, nil
	}
	p, err := s.backupPath(b.Name)
	if err != nil {
		return nil, false, err
	}
	data, err := readRegular(p)
	return data, true, err
}

// DiffStats reports what rewinding to k would change on disk now: the files
// whose content differs from the snapshot, with the line insertions/deletions
// the restore applies. ok=false when there is no snapshot for k.
func (s *Store) DiffStats(k Key) (DiffStats, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findLocked(k)
	if i < 0 {
		return DiffStats{}, false
	}
	var st DiffStats
	for _, tp := range s.tracked {
		b, ok := s.targetLocked(i, tp)
		if !ok {
			continue
		}
		abs := s.absPath(tp)
		want, wantExists, err := s.readBackup(b)
		if err != nil {
			continue
		}
		have, haveExists := readCurrent(abs)
		s.addDiff(&st, abs, have, haveExists, want, wantExists)
	}
	return st, true
}

// TurnStats reports what changed between the snapshot for k and the snapshot
// for next (or the files on disk now when next is nil): the "N files changed"
// line of the rewind picker. ok=false when there is no snapshot for k.
func (s *Store) TurnStats(k Key, next *Key) (DiffStats, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findLocked(k)
	if i < 0 {
		return DiffStats{}, false
	}
	j := -1
	if next != nil {
		j = s.findLocked(*next)
	}
	var st DiffStats
	for _, tp := range s.tracked {
		abs := s.absPath(tp)
		var before, after []byte
		var beforeExists, afterExists bool
		b, ok := s.targetLocked(i, tp)
		if !ok {
			continue
		}
		var err error
		if before, beforeExists, err = s.readBackup(b); err != nil {
			continue
		}
		if j >= 0 {
			a, ok := s.targetLocked(j, tp)
			if !ok {
				continue
			}
			if after, afterExists, err = s.readBackup(a); err != nil {
				continue
			}
		} else {
			after, afterExists = readCurrent(abs)
		}
		// Forward direction: what the turn added and removed.
		s.addDiff(&st, abs, before, beforeExists, after, afterExists)
	}
	return st, true
}

// addDiff accumulates the change from (from) to (to) for one file.
func (s *Store) addDiff(st *DiffStats, abs string, from []byte, fromExists bool, to []byte, toExists bool) {
	if !fromExists && !toExists {
		return
	}
	ins, del := lineDiffCounts(string(from), string(to))
	if ins == 0 && del == 0 && fromExists == toExists {
		return
	}
	st.FilesChanged = append(st.FilesChanged, abs)
	st.Insertions += ins
	st.Deletions += del
}

// Rewind restores every tracked file to its state in the snapshot for k: a
// file that differs from its backup is rewritten, a file absent then is
// deleted. Paths that fail the link-safety checks are skipped and counted.
// When every differing file failed the error wraps ErrNoFilesRestored.
func (s *Store) Rewind(k Key) (RewindResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findLocked(k)
	if i < 0 {
		return RewindResult{}, ErrSnapshotNotFound
	}
	var res RewindResult
	for _, tp := range s.tracked {
		abs := s.absPath(tp)
		b, ok := s.targetLocked(i, tp)
		if !ok {
			res.Failed = append(res.Failed, abs)
			continue
		}
		if reason := checkDestination(abs, b.RealParentDir); reason != "" {
			res.SkippedLinks++
			continue
		}
		if b.Absent {
			fi, err := os.Lstat(abs)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil || !fi.Mode().IsRegular() {
				res.SkippedLinks++
				continue
			}
			if err := os.Remove(abs); err != nil {
				res.Failed = append(res.Failed, abs)
				continue
			}
			res.Changed = append(res.Changed, abs)
			continue
		}
		bp, err := s.backupPath(b.Name)
		if err != nil {
			res.Failed = append(res.Failed, abs)
			continue
		}
		bfi, err := os.Lstat(bp)
		if err != nil || !bfi.Mode().IsRegular() {
			res.Failed = append(res.Failed, abs)
			continue
		}
		cfi, cerr := os.Lstat(abs)
		if cerr == nil && !differs(abs, cfi, bp) {
			continue
		}
		switch restoreFile(abs, bp, bfi.Mode().Perm(), b.RealParentDir) {
		case restored:
			res.Changed = append(res.Changed, abs)
		case refused:
			res.SkippedLinks++
		default:
			res.Failed = append(res.Failed, abs)
		}
	}
	if len(res.Failed) > 0 && len(res.Changed) == 0 {
		msg := fmt.Sprintf("%d %s failed (backup missing, or the file could not be updated)", len(res.Failed), plural(len(res.Failed), "file", "files"))
		if res.SkippedLinks > 0 {
			msg += fmt.Sprintf(", and %d %s skipped for link safety", res.SkippedLinks, plural(res.SkippedLinks, "path was", "paths were"))
		}
		return res, fmt.Errorf("%w: %s", ErrNoFilesRestored, msg)
	}
	return res, nil
}

// TrackedFiles returns the absolute paths of the tracked files.
func (s *Store) TrackedFiles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.tracked))
	for _, tp := range s.tracked {
		out = append(out, s.absPath(tp))
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// --- filesystem helpers -----------------------------------------------------

// realParentDir resolves path's parent through symlinks; for a parent that
// does not exist yet it resolves the nearest existing ancestor and re-appends
// the rest.
func realParentDir(path string) string {
	parent := filepath.Dir(path)
	if r, err := filepath.EvalSymlinks(parent); err == nil {
		return r
	}
	cur := parent
	for {
		up := filepath.Dir(cur)
		if up == cur {
			return ""
		}
		cur = up
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			rel, rerr := filepath.Rel(cur, parent)
			if rerr != nil {
				return ""
			}
			return filepath.Join(r, rel)
		}
	}
}

// checkDestination is the pre-restore safety check: the destination must not
// be a symlink, a non-regular file or hard-linked, and its parent must still
// resolve to the directory recorded at backup time. It returns the refusal
// reason, or "" when safe.
func checkDestination(abs, wantParent string) string {
	fi, err := os.Lstat(abs)
	switch {
	case err == nil:
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "destination is a symlink"
		}
		if !fi.Mode().IsRegular() {
			return "destination is not a regular file"
		}
		if n := linkCount(fi); n > 1 {
			return fmt.Sprintf("destination is hard-linked (nlink=%d)", n)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "destination path does not resolve"
	}
	if wantParent != "" {
		if got := realParentDir(abs); got != wantParent {
			return fmt.Sprintf("parent directory moved (%s != %s)", got, wantParent)
		}
		if pfi, err := os.Stat(filepath.Dir(abs)); err == nil && !pfi.IsDir() {
			return "parent path is not a directory"
		}
	}
	return ""
}

type restoreOutcome int

const (
	restored restoreOutcome = iota
	refused
	failed
)

// restoreFile writes the backup at src over dst without following a symlink
// at dst, re-checking the destination's identity through the open handle.
func restoreFile(dst, src string, mode fs.FileMode, wantParent string) restoreOutcome {
	in, err := os.OpenFile(src, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return failed
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return refused
	}
	flags := os.O_WRONLY | os.O_CREATE | noFollow
	out, err := os.OpenFile(dst, flags, mode)
	if errors.Is(err, fs.ErrNotExist) {
		if wantParent != "" && realParentDir(dst) != wantParent {
			return refused
		}
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil {
			return failed
		}
		out, err = os.OpenFile(dst, flags, mode)
	}
	if err != nil {
		if isLinkErr(err) {
			return refused
		}
		return failed
	}
	defer out.Close()
	ofi, err := out.Stat()
	if err != nil || !ofi.Mode().IsRegular() || linkCount(ofi) > 1 {
		return refused
	}
	if wantParent != "" {
		if got, err := filepath.EvalSymlinks(filepath.Dir(dst)); err != nil || got != wantParent {
			return refused
		}
	}
	if pfi, err := os.Lstat(dst); err != nil || !os.SameFile(pfi, ofi) {
		return refused
	}
	if err := out.Truncate(0); err != nil {
		return failed
	}
	if _, err := io.Copy(out, in); err != nil {
		return failed
	}
	_ = out.Chmod(mode)
	return restored
}

// differs reports whether the file at abs (described by fi) differs from the
// backup at bp: existence, type, size, then content.
func differs(abs string, fi fs.FileInfo, bp string) bool {
	bfi, err := os.Lstat(bp)
	if err != nil {
		return true
	}
	if !fi.Mode().IsRegular() || !bfi.Mode().IsRegular() {
		return true
	}
	if fi.Size() != bfi.Size() || fi.Mode().Perm() != bfi.Mode().Perm() {
		return true
	}
	a, err := readRegular(abs)
	if err != nil {
		return true
	}
	b, err := readRegular(bp)
	if err != nil {
		return true
	}
	return string(a) != string(b)
}

// readCurrent reads a tracked file's current content (exists=false when it is
// missing or not a regular file).
func readCurrent(abs string) ([]byte, bool) {
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	data, err := readRegular(abs)
	if err != nil {
		return nil, false
	}
	return data, true
}

// readRegular reads a regular file without following a final symlink.
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("FileHistory: %s is not a regular file", path)
	}
	return io.ReadAll(io.LimitReader(f, MaxFileSize+1))
}

// copyRegular copies src to a new file dst with perm.
func copyRegular(src, dst string, perm fs.FileMode) error {
	in, err := os.OpenFile(src, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Chmod(dst, perm)
}

// linkOrCopy hard-links src to dst, falling back to a copy; an existing dst
// is kept.
func linkOrCopy(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("FileHistory: backup source is not a regular file")
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyRegular(src, dst, fi.Mode().Perm())
}
