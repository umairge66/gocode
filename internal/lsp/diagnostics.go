package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Diagnostics reporting after edits.
//
// Before the model edits a file a language server handles, the file's current
// diagnostics are taken as its baseline (the file is opened on the server
// first when needed, waiting up to BaselineWait for them). After the edit the
// new content is sent (didChange + didSave) and the server gets up to Settle
// to publish diagnostics for it. Just before the next model request the
// diagnostics not present in the baseline are reported once, in a
// <new-diagnostics> reminder, and become the new baseline. Diagnostics that
// arrive after the settle window are reported at a later request.
const (
	// DefaultSettle is how long a server is given after an edit to publish
	// diagnostics for it.
	DefaultSettle = 2 * time.Second
	// DefaultBaselineWait is how long the pre-edit baseline is waited for
	// when the file was not yet open on its server.
	DefaultBaselineWait = 1500 * time.Millisecond
	// MaxDiagnosticsPerFile and MaxDiagnosticsTotal bound one report; the
	// most severe issues are kept.
	MaxDiagnosticsPerFile = 10
	MaxDiagnosticsTotal   = 30
	// maxReportChars bounds the reminder body.
	maxReportChars = 4000
	// maxSyncedFileSize is the largest file synchronized to a server.
	maxSyncedFileSize = 10_000_000
	// syncTimeout bounds one background synchronization (it may include a
	// server start).
	syncTimeout = 2 * time.Minute
)

// trackedFile is the diagnostics state of one edited file.
type trackedFile struct {
	path          string
	baseline      []Diagnostic
	baselineKnown bool
	// openVersion is the document version the baseline was requested for
	// (0 when the file was already open).
	openVersion int
	comparedSeq uint64
	// An edit not yet reported: pending is set by the edit; synced once the
	// new content went out (changeVersion / changeSeq then name the
	// publishes that answer it); deadline ends the settle window.
	pending       bool
	synced        bool
	changeVersion int
	changeSeq     uint64
	deadline      time.Time
}

// Tracker reports the new diagnostics language servers publish for the files
// the model edits. It is the dispatcher's edit observer (BeforeFileEdit /
// AfterFileEdit) and a conversation context feed (TakePending / TakeMidTurn).
type Tracker struct {
	m *Manager
	// Settle and BaselineWait override DefaultSettle / DefaultBaselineWait.
	Settle       time.Duration
	BaselineWait time.Duration
	// OnReport, when set, is told of each report (for the transcript).
	OnReport func(files []FileDiagnostics)

	mu    sync.Mutex
	files map[string]*trackedFile
	// syncs counts the edits synchronized per server.
	syncs map[string]int

	queue *syncQueue
}

// NewTracker returns a Tracker over m. Close stops its background syncing.
func NewTracker(m *Manager) *Tracker {
	t := &Tracker{m: m, files: map[string]*trackedFile{}, syncs: map[string]int{}, queue: newSyncQueue()}
	m.OnDiagnostics(t.lateBaseline)
	return t
}

func (t *Tracker) settle() time.Duration {
	if t.Settle > 0 {
		return t.Settle
	}
	return DefaultSettle
}

func (t *Tracker) baselineWait() time.Duration {
	if t.BaselineWait > 0 {
		return t.BaselineWait
	}
	return DefaultBaselineWait
}

// Close stops background synchronization.
func (t *Tracker) Close() {
	if t != nil {
		t.queue.close()
	}
}

// reports reports whether diagnostics for path are tracked: a server
// handles it and its diagnostics are on.
func (t *Tracker) reports(path string) bool {
	s := t.m.ServerFor(path)
	return s != nil && s.Config.DiagnosticsEnabled()
}

// readSynced reads a file for synchronization; ok is false when it is missing,
// not a regular file, or too large.
func readSynced(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxSyncedFileSize {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// BeforeFileEdit captures the baseline of a file about to be edited.
func (t *Tracker) BeforeFileEdit(path string) {
	if t == nil || !t.reports(path) {
		return
	}
	key := diagKey(path)
	t.mu.Lock()
	tf := t.files[key]
	if tf != nil && tf.pending {
		// An earlier edit is not reported yet: keep its baseline so the report
		// covers both edits.
		t.mu.Unlock()
		return
	}
	if tf == nil {
		tf = &trackedFile{path: key}
		t.files[key] = tf
	}
	t.mu.Unlock()

	text, exists := readSynced(path)
	if !exists {
		if _, err := os.Stat(path); err != nil {
			// A new file has no diagnostics yet.
			t.setBaseline(tf, nil, t.m.Seq())
		}
		return
	}
	if t.m.IsFileOpen(path) {
		// Open and synchronized: what the server last published is current
		// (nothing published means no issues).
		if fd, ok := t.m.Diagnostics(path); ok {
			t.setBaseline(tf, fd.Diagnostics, fd.Seq)
		} else {
			t.setBaseline(tf, nil, 0)
		}
		return
	}
	before := t.m.Seq()
	done := make(chan struct{})
	t.queue.push(func(ctx context.Context) {
		defer close(done)
		if !t.m.IsFileOpen(path) {
			if err := t.m.OpenFile(ctx, path, text); err != nil {
				return
			}
		}
		if v, ok := t.m.DocumentVersion(PathToURI(path)); ok {
			t.mu.Lock()
			if !tf.baselineKnown {
				tf.openVersion = v
			}
			t.mu.Unlock()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), t.baselineWait())
	defer cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return
	}
	if t.m.WaitDiagnostics(ctx, path, before) {
		if fd, ok := t.m.Diagnostics(path); ok {
			t.setBaseline(tf, fd.Diagnostics, fd.Seq)
		}
	}
}

func (t *Tracker) setBaseline(tf *trackedFile, diags []Diagnostic, seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tf.pending {
		return
	}
	tf.baseline = append([]Diagnostic(nil), diags...)
	tf.baselineKnown = true
	tf.comparedSeq = seq
}

// lateBaseline takes a publish for the pre-edit content that arrived after
// BaselineWait as the baseline it was waiting for.
func (t *Tracker) lateBaseline(fd FileDiagnostics) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tf := t.files[diagKey(fd.Path)]
	if tf == nil || tf.baselineKnown || tf.openVersion == 0 || fd.Version == nil || *fd.Version != tf.openVersion {
		return
	}
	tf.baseline = append([]Diagnostic(nil), fd.Diagnostics...)
	tf.baselineKnown = true
	if !fd.Stale {
		tf.comparedSeq = fd.Seq
	}
}

// AfterFileEdit sends a successfully edited file's new content to its server
// and starts the settle window.
func (t *Tracker) AfterFileEdit(path string) { t.afterEdit(path, true) }

// SyncOnly returns an edit observer that keeps the servers' copy of edited
// files current without reporting their diagnostics (for subagents, whose
// edits are not reported to the main conversation).
func (t *Tracker) SyncOnly() *SyncOnly { return &SyncOnly{t: t} }

// SyncOnly is the observer SyncOnly returns.
type SyncOnly struct{ t *Tracker }

// BeforeFileEdit does nothing.
func (s *SyncOnly) BeforeFileEdit(string) {}

// AfterFileEdit sends the new content to the file's server.
func (s *SyncOnly) AfterFileEdit(path string) { s.t.afterEdit(path, false) }

func (t *Tracker) afterEdit(path string, track bool) {
	if t == nil || t.m.ServerFor(path) == nil {
		return
	}
	text, ok := readSynced(path)
	if !ok {
		return
	}
	var tf *trackedFile
	if track && t.reports(path) {
		key := diagKey(path)
		t.mu.Lock()
		tf = t.files[key]
		if tf == nil {
			tf = &trackedFile{path: key}
			t.files[key] = tf
		}
		tf.pending = true
		tf.synced = false
		tf.deadline = time.Now().Add(t.settle())
		t.mu.Unlock()
	}
	t.queue.push(func(ctx context.Context) {
		seq := t.m.Seq()
		if err := t.m.ChangeFile(ctx, path, text); err != nil {
			return
		}
		_ = t.m.SaveFile(path)
		if s := t.m.ServerFor(path); s != nil {
			t.mu.Lock()
			t.syncs[s.Name]++
			t.mu.Unlock()
		}
		if tf == nil {
			return
		}
		v, _ := t.m.DocumentVersion(PathToURI(path))
		t.mu.Lock()
		tf.synced = true
		tf.changeVersion = v
		tf.changeSeq = seq
		t.mu.Unlock()
	})
}

// silentEdits is how many synchronized edits without any diagnostics
// published mark a server as one that does not publish them.
const silentEdits = 2

// silent reports whether a server has had silentEdits edits synchronized
// without ever publishing diagnostics. Called with t.mu held.
func (t *Tracker) silent(server string) bool {
	return t.syncs[server] >= silentEdits && !t.m.HasPublished(server)
}

// fresh reports whether fd answers the file's last synced edit. Called with
// t.mu held.
func (tf *trackedFile) fresh(fd FileDiagnostics) bool {
	if !tf.synced || fd.Seq <= tf.changeSeq {
		return false
	}
	return fd.Version == nil || *fd.Version >= tf.changeVersion
}

// HasPending reports nothing: diagnostics never start a turn on their own.
func (t *Tracker) HasPending() bool { return false }

// TakeMidTurn returns the new-diagnostics reminder for the request that
// follows a tool round.
func (t *Tracker) TakeMidTurn() []string { return t.take(false) }

// TakePending returns the new-diagnostics reminder for a new user turn and
// forgets the files with nothing left to report.
func (t *Tracker) TakePending() []string { return t.take(true) }

func (t *Tracker) take(reset bool) []string {
	if t == nil {
		return nil
	}
	t.waitSettle()
	var report []FileDiagnostics
	t.mu.Lock()
	keys := sortedKeys(t.files)
	for _, key := range keys {
		tf := t.files[key]
		fd, ok := t.m.Diagnostics(tf.path)
		if !ok || fd.Seq <= tf.comparedSeq {
			if reset && (!tf.pending || time.Since(tf.deadline) > syncTimeout) {
				delete(t.files, key)
			}
			continue
		}
		if tf.pending && !tf.fresh(fd) {
			continue
		}
		var baseline map[string]bool
		if tf.baselineKnown {
			baseline = make(map[string]bool, len(tf.baseline))
			for _, d := range tf.baseline {
				baseline[diagnosticKey(d)] = true
			}
		}
		var added []Diagnostic
		seen := map[string]bool{}
		for _, d := range fd.Diagnostics {
			k := diagnosticKey(d)
			if baseline[k] || seen[k] {
				continue
			}
			seen[k] = true
			added = append(added, d)
		}
		tf.baseline = append([]Diagnostic(nil), fd.Diagnostics...)
		tf.baselineKnown = true
		tf.comparedSeq = fd.Seq
		tf.pending = false
		if len(added) > 0 {
			report = append(report, FileDiagnostics{Path: fd.Path, Diagnostics: added, Server: fd.Server, Version: fd.Version, Seq: fd.Seq})
		}
		if reset {
			delete(t.files, key)
		}
	}
	t.mu.Unlock()
	report = limitDiagnostics(report)
	if len(report) == 0 {
		return nil
	}
	if t.OnReport != nil {
		t.OnReport(report)
	}
	return []string{"<system-reminder>\n" + FormatNewDiagnostics(report) + "\n</system-reminder>"}
}

// waitSettle waits, up to each file's settle deadline, for the servers to
// answer the pending edits.
func (t *Tracker) waitSettle() {
	for {
		t.mu.Lock()
		var waitPath string
		var waitSeq uint64
		var deadline time.Time
		now := time.Now()
		for _, key := range sortedKeys(t.files) {
			tf := t.files[key]
			if !tf.pending || !now.Before(tf.deadline) {
				continue
			}
			if s := t.m.ServerFor(tf.path); s == nil || (tf.synced && t.silent(s.Name)) {
				// A server that never publishes diagnostics is not waited for.
				continue
			}
			if fd, ok := t.m.Diagnostics(tf.path); ok && tf.fresh(fd) {
				continue
			}
			waitPath, deadline = tf.path, tf.deadline
			waitSeq = tf.changeSeq
			if fd, ok := t.m.Diagnostics(tf.path); ok && fd.Seq > waitSeq {
				waitSeq = fd.Seq
			}
			break
		}
		t.mu.Unlock()
		if waitPath == "" {
			return
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		t.m.WaitDiagnostics(ctx, waitPath, waitSeq)
		cancel()
	}
}

// diagnosticKey identifies a diagnostic for the baseline comparison.
func diagnosticKey(d Diagnostic) string {
	b, _ := json.Marshal([]any{d.Message, d.Severity, d.Source, d.Code,
		d.Range.Start.Line, d.Range.Start.Character, d.Range.End.Line, d.Range.End.Character})
	return string(b)
}

// severityRank orders severities for the volume limit (errors first).
func severityRank(s string) int {
	switch s {
	case "Error":
		return 1
	case "Warning":
		return 2
	case "Info":
		return 3
	default:
		return 4
	}
}

// limitDiagnostics keeps the most severe MaxDiagnosticsPerFile issues of each
// file and MaxDiagnosticsTotal overall.
func limitDiagnostics(files []FileDiagnostics) []FileDiagnostics {
	var out []FileDiagnostics
	total := 0
	for _, f := range files {
		ds := append([]Diagnostic(nil), f.Diagnostics...)
		sort.SliceStable(ds, func(i, j int) bool { return severityRank(ds[i].Severity) < severityRank(ds[j].Severity) })
		if len(ds) > MaxDiagnosticsPerFile {
			ds = ds[:MaxDiagnosticsPerFile]
		}
		if room := MaxDiagnosticsTotal - total; len(ds) > room {
			ds = ds[:room]
		}
		total += len(ds)
		if len(ds) > 0 {
			f.Diagnostics = ds
			out = append(out, f)
		}
	}
	return out
}

// SeverityIcon is the glyph shown for a severity.
func SeverityIcon(severity string) string {
	switch severity {
	case "Error":
		return "✘"
	case "Warning":
		return "⚠"
	case "Info":
		return "ℹ"
	case "Hint":
		return "★"
	default:
		return "●"
	}
}

// formatDiagnosticLine renders one diagnostic:
// "<icon> [Line L:C] message [code] (source)".
func formatDiagnosticLine(d Diagnostic) string {
	line := fmt.Sprintf("%s [Line %d:%d] %s", SeverityIcon(d.Severity), d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
	if d.Code != "" {
		line += " [" + d.Code + "]"
	}
	if d.Source != "" {
		line += " (" + d.Source + ")"
	}
	return line
}

// FormatNewDiagnostics renders the <new-diagnostics> block the model
// receives: each file's base name, then one line per diagnostic. The body is
// cut at maxReportChars.
func FormatNewDiagnostics(files []FileDiagnostics) string {
	parts := make([]string, 0, len(files))
	for _, f := range files {
		lines := make([]string, 0, len(f.Diagnostics))
		for _, d := range f.Diagnostics {
			lines = append(lines, "  "+formatDiagnosticLine(d))
		}
		name := f.Path[strings.LastIndexAny(f.Path, `/\`)+1:]
		if name == "" {
			name = f.Path
		}
		parts = append(parts, name+":\n"+strings.Join(lines, "\n"))
	}
	body := strings.Join(parts, "\n\n")
	if r := []rune(body); len(r) > maxReportChars {
		body = string(r[:maxReportChars-12]) + "\u2026[truncated]"
	}
	return "<new-diagnostics>The following new diagnostic issues were detected:\n\n" + body + "</new-diagnostics>"
}

// SummaryText renders a report for the transcript: a count line, then the
// issues (at most maxLines of them) with their severity icons.
func SummaryText(files []FileDiagnostics, cwd string, maxLines int) string {
	n := 0
	for _, f := range files {
		n += len(f.Diagnostics)
	}
	issues, fileWord := "issues", "files"
	if n == 1 {
		issues = "issue"
	}
	if len(files) == 1 {
		fileWord = "file"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d new diagnostic %s in %d %s", n, issues, len(files), fileWord)
	shown := 0
	for _, f := range files {
		for _, d := range f.Diagnostics {
			if shown == maxLines {
				fmt.Fprintf(&b, "\n  … %d more", n-shown)
				return b.String()
			}
			fmt.Fprintf(&b, "\n  %s %s:%d:%d %s", SeverityIcon(d.Severity), DisplayPath(f.Path, cwd), d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
			shown++
		}
	}
	return b.String()
}

// syncQueue runs document synchronization in order on one goroutine, so the
// content a server sees always follows the order of the edits.
type syncQueue struct {
	mu     sync.Mutex
	ops    []func(context.Context)
	wake   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
}

func newSyncQueue() *syncQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &syncQueue{wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	go q.run()
	return q
}

func (q *syncQueue) push(op func(context.Context)) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.ops = append(q.ops, op)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *syncQueue) run() {
	for {
		q.mu.Lock()
		if len(q.ops) == 0 {
			closed := q.closed
			q.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-q.wake:
			case <-q.ctx.Done():
			}
			continue
		}
		op := q.ops[0]
		q.ops = q.ops[1:]
		q.mu.Unlock()
		ctx, cancel := context.WithTimeout(q.ctx, syncTimeout)
		op(ctx)
		cancel()
	}
}

func (q *syncQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.ops = nil
	q.mu.Unlock()
	q.cancel()
}
