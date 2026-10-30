package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the LOCAL-ONLY session persistence + resume feature into the
// cli session lifecycle, shared by the -p and interactive paths:
//
//   - wireSessionStore builds the per-cwd sessionstore.Store and, for a
//     persisting session, binds a fresh writable session onto the conversation
//     so each turn is appended as it happens (and /clear rotates to a new one).
//   - resumeInto loads a saved session's transcript into the live conversation
//     and binds the store so the continuation appends to that session (or forks).
//   - the -p and interactive entry points resolve the --continue / --resume /
//     --session-id intent and apply it.
//
// Nothing here touches the network: the only effect is reading and writing JSONL
// transcripts under <configHome>/projects (the layout is defined in
// internal/sessionstore). Options: -c/--continue, -r/--resume [value],
// --session-id, --fork-session, --no-session-persistence.

// resumeIntent is the parsed launch-time resume request.
type resumeIntent struct {
	continueLatest bool   // -c/--continue: resume the latest session, no picker
	resume         bool   // -r/--resume present
	value          string // the optional resume value (id / name / picker search term)
	sessionID      string // --session-id for a fresh session
	fork           bool   // --fork-session: resume into a new id
	noPersistence  bool   // --no-session-persistence
}

// resumeIntentFrom extracts the resume request from the dispatch defaults.
func resumeIntentFrom(d *DispatchDefaults) resumeIntent {
	if d == nil {
		return resumeIntent{}
	}
	return resumeIntent{
		continueLatest: d.Continue,
		resume:         d.Resume,
		value:          strings.TrimSpace(d.ResumeValue),
		sessionID:      strings.TrimSpace(d.SessionID),
		fork:           d.ForkSession,
		noPersistence:  d.NoSessionPersistence,
	}
}

// wants reports whether any resume/continue was requested at launch.
func (r resumeIntent) wants() bool { return r.continueLatest || r.resume }

// sessionConfigHome is the gocode root (internal/confighome) that holds the
// session transcript tree, file history, plans and tool results — the same
// directory as the user settings. "" when no root can be resolved (a diskless
// session → no persistence).
func sessionConfigHome(env *Env, paths sessionPaths) string {
	if paths.configDir != "" {
		return paths.configDir
	}
	dir, _ := confighome.Resolve(envGetenv(env), paths.home)
	return dir
}

// join mirrors filepath.Join (kept local so resume.go reads cleanly).
func join(parts ...string) string { return filepath.Join(parts...) }

// wireSessionStore builds the session store for this session and, when
// persistence is enabled (cfg.persistNew and a config home exists), binds a
// fresh writable session onto the conversation. It records the store + cwd on
// the runtime so the resume flows and the /resume picker can read it. A diskless
// session (no config home) or --no-session-persistence leaves everything nil, so
// the conversation simply never persists.
func (r *sessionRuntime) wireSessionStore(env *Env, paths sessionPaths, sessionID string, cfg sessionConfig) {
	r.sessionID = sessionID
	base := sessionConfigHome(env, paths)
	if cfg.noPersistence || base == "" {
		return
	}
	store := sessionstore.New(base)
	store.AppVersion = updateCurrentVersion()
	// Transcripts moved from ~/.gocode may name files under the former
	// location; Load points those paths at the root.
	if legacy := confighome.LegacyDir(paths.home); legacy != "" {
		store.MovedFrom = []string{legacy}
	}
	r.store = store
	r.storeCwd = paths.cwd

	// Age-based retention: prune transcripts older than cleanupPeriodDays
	// (default 30, 0 disables).
	if days := cfg.cleanupDays; days > 0 {
		store.Cleanup(paths.cwd, time.Now().Add(-time.Duration(days)*24*time.Hour))
	}

	if !cfg.persistNew {
		return
	}
	sess, err := store.Create(paths.cwd, sessionID)
	if err != nil {
		return
	}
	r.conv.Store = sess
	// /clear rotates to a fresh, lazily-created session (the prior one stays on
	// disk, resumable).
	r.conv.NewSessionStore = func() conversation.SessionRecorder {
		ns, err := store.Create(paths.cwd, ids.RandomUUID())
		if err != nil {
			return nil
		}
		return ns
	}
}

// resumeInto loads session id's transcript into the live conversation and binds
// the store so the continuation records to that session — or, with fork, to a
// brand-new session seeded with the loaded history (the original is left
// untouched). It returns the loaded session meta and ok=false when the session
// could not be read.
func (r *sessionRuntime) resumeInto(id string, fork bool) (sessionstore.Meta, bool) {
	if r.store == nil {
		return sessionstore.Meta{}, false
	}
	msgs, err := r.store.Load(r.storeCwd, id)
	if err != nil {
		return sessionstore.Meta{}, false
	}
	meta, _ := r.store.Find(r.storeCwd, id)
	meta.MessageCount = len(msgs)

	var rec conversation.SessionRecorder
	if fork {
		forkID := r.sessionID
		if forkID == "" || forkID == id {
			forkID = ids.RandomUUID()
		}
		if sess, err := r.store.Create(r.storeCwd, forkID); err == nil {
			// Seed the fork's file with the loaded history, then append new turns.
			if werr := sess.ReplaceHistory(msgs); werr == nil {
				rec = sess
			}
		}
	} else {
		if sess, err := r.store.Open(r.storeCwd, id, len(msgs)); err == nil {
			rec = sess
		}
	}
	r.conv.SeedHistory(msgs, rec)
	r.allowResumedToolResults(id)
	// The conversation now records to another session: the tabs gocode
	// opened in the previous session's tab group are closed.
	r.chromeTabs.switched(r.currentSessionID())
	return meta, true
}

// allowResumedToolResults lets the read-only file tools open the resumed
// session's saved tool results without a prompt, as they can for the current
// session's own: the loaded history refers to those files.
func (r *sessionRuntime) allowResumedToolResults(id string) {
	if r.store == nil || r.dispatcher == nil || r.dispatcher.Decider == nil || id == "" {
		return
	}
	dir := filepath.Clean(join(r.store.ProjectDir(r.storeCwd), id, "tool-results"))
	for _, have := range r.dispatcher.Decider.ReadableDirs {
		if have == dir {
			return
		}
	}
	r.dispatcher.Decider.ReadableDirs = append(r.dispatcher.Decider.ReadableDirs, dir)
}

// resumeLatest resumes the most-recent session for the cwd (the --continue /
// `-c` target). ok=false when there is none.
func (r *sessionRuntime) resumeLatest(fork bool) (sessionstore.Meta, bool) {
	if r.store == nil {
		return sessionstore.Meta{}, false
	}
	latest, ok := r.store.Latest(r.storeCwd)
	if !ok {
		return sessionstore.Meta{}, false
	}
	return r.resumeInto(latest.ID, fork)
}

// resumeByValue resumes the session named by value (an id, a unique id prefix,
// or a title substring). ok=false when nothing resolves.
func (r *sessionRuntime) resumeByValue(value string, fork bool) (sessionstore.Meta, bool) {
	if r.store == nil {
		return sessionstore.Meta{}, false
	}
	m, ok := r.store.Find(r.storeCwd, value)
	if !ok {
		return sessionstore.Meta{}, false
	}
	return r.resumeInto(m.ID, fork)
}

// currentSessionID is the id of the session this conversation records to: the
// resumed session after a --resume/--continue (or its fork), else the id the
// session was built with.
func (r *sessionRuntime) currentSessionID() string {
	if r.conv != nil {
		if sess, ok := r.conv.Store.(*sessionstore.Session); ok && sess != nil {
			return sess.ID()
		}
	}
	return r.sessionID
}

// defaultCleanupDays is the default retention window, in days.
const defaultCleanupDays = 30

// applyPrintResume applies a launch-time --continue/--resume to a -p session
// before its single turn. The interactive picker is unavailable headlessly, so a
// bare --resume (no id) reports guidance. It returns a user-facing notice ("" =
// nothing requested).
func (r *sessionRuntime) applyPrintResume(intent resumeIntent) string {
	switch {
	case intent.continueLatest:
		if m, ok := r.resumeLatest(intent.fork); ok {
			return fmt.Sprintf("Continuing conversation %s (%d messages).", shortID(m.ID), m.MessageCount)
		}
		return "No conversations found to resume."
	case intent.resume:
		if intent.value == "" {
			return "--resume needs a session id in print mode (the interactive picker is unavailable); use --continue for the most recent conversation."
		}
		if m, ok := r.resumeByValue(intent.value, intent.fork); ok {
			return fmt.Sprintf("Resumed conversation %s (%d messages).", shortID(m.ID), m.MessageCount)
		}
		return fmt.Sprintf("No conversation matching %q found to resume.", intent.value)
	}
	return ""
}

// applyInteractiveLaunchResume applies a launch-time --continue/--resume in the
// interactive REPL at startup: --continue resumes the most recent session
// directly; --resume with a value resumes it (or opens the picker filtered by
// it); bare --resume opens the picker. It runs on the startup goroutine (off the
// UI thread), so the picker's blocking wait is safe.
func applyInteractiveLaunchResume(d *interactiveDialogs, intent resumeIntent) {
	if d == nil || d.store == nil {
		if d != nil {
			d.sendMsg(ui.AppendSystemMsg{Text: "Session persistence is disabled; there is nothing to resume."})
		}
		return
	}
	switch {
	case intent.continueLatest:
		m, ok := d.store.Latest(d.resumeCwd)
		if !ok {
			d.sendMsg(ui.AppendSystemMsg{Text: "No conversations found to resume."})
			return
		}
		d.applyResume(m.ID)
	case intent.resume:
		d.resume(intent.value)
	}
}

// --- picker presentation ---------------------------------------------------

// resumeDetail renders the dimmed trailing subtitle for a session picker row:
// "<relative-modified> · N messages[ · <branch>]".
func resumeDetail(m sessionstore.Meta) string {
	parts := []string{humanizeSince(m.Modified)}
	if m.MessageCount == 1 {
		parts = append(parts, "1 message")
	} else {
		parts = append(parts, fmt.Sprintf("%d messages", m.MessageCount))
	}
	if m.GitBranch != "" {
		parts = append(parts, m.GitBranch)
	}
	return strings.Join(parts, " · ")
}

// humanizeSince renders a coarse "N <unit> ago" / "just now" relative time.
func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	default:
		return plural(int(d/(30*24*time.Hour)), "month")
	}
}

func plural(n int, unit string) string {
	if n <= 1 {
		n = max1(n)
		if n == 1 {
			return "1 " + unit + " ago"
		}
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// shortID returns the first 8 chars of a session id for a terse echo.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
