package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/filehistory"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires /rewind (aliases /checkpoint, /undo): file checkpoints taken
// before the agent's Write / Edit / MultiEdit / NotebookEdit calls (see
// internal/filehistory), a snapshot at every user prompt, and the interactive
// picker that restores the code and/or the conversation to the point before an
// earlier prompt. Everything is LOCAL-ONLY: the backups live under
// <configHome>/file-history/<sessionId>/ and nothing is transmitted.

// Wording of the rewind picker.
const (
	rewindTitle             = "Rewind"
	rewindSubtitle          = "Restore the code and/or conversation to the point before…"
	rewindSubtitleNoCode    = "Restore and fork the conversation to the point before…"
	rewindNothing           = "Nothing to rewind to yet."
	rewindManualEditsNote   = "Rewinding does not affect files edited manually or via bash."
	rewindSkippedReason     = "the tracked path is (or became) a link or other non-regular file, its directory changed since the checkpoint, or its backup could not be safely read"
	rewindOptBoth           = "both"
	rewindOptConversation   = "conversation"
	rewindOptCode           = "code"
	rewindOptNevermind      = "nevermind"
	rewindLabelBoth         = "Restore code and conversation"
	rewindLabelConversation = "Restore conversation"
	rewindLabelCode         = "Restore code"
	rewindLabelNevermind    = "Never mind"
)

// fileHistories maps a live conversation to its file history, so the /rewind
// dialog (which holds the conversation) finds the store the session wired.
var fileHistories sync.Map // *conversation.Conversation -> *filehistory.Store

// fileHistoryFor returns conv's file history (nil when checkpointing is off).
func fileHistoryFor(conv *conversation.Conversation) *filehistory.Store {
	if v, ok := fileHistories.Load(conv); ok {
		return v.(*filehistory.Store)
	}
	return nil
}

// fileCheckpointingEnabled is the checkpointing gate: on by default in an
// interactive session, off with fileCheckpointingEnabled:false in settings or
// GOCODE_CODE_DISABLE_FILE_CHECKPOINTING.
func fileCheckpointingEnabled(env *Env) bool {
	if envutil.EnvTruthy(envGetenv(env)("GOCODE_CODE_DISABLE_FILE_CHECKPOINTING")) {
		return false
	}
	settings, _ := cmdInfo{env: env}.effectiveSettings()
	if v, present := settingBool(settings, "fileCheckpointingEnabled"); present && !v {
		return false
	}
	return true
}

// wireFileHistory installs file checkpointing on an interactive session: the
// dispatcher backs up each file before the agent first modifies it, and every
// user prompt takes a snapshot just before its agent loop runs.
func (r *sessionRuntime) wireFileHistory(env *Env, paths sessionPaths, sessionID string, cfg sessionConfig) {
	if !cfg.interactive || r.conv == nil || r.dispatcher == nil || !fileCheckpointingEnabled(env) {
		return
	}
	base := sessionConfigHome(env, paths)
	if base == "" {
		return
	}
	conv := r.conv
	fh := filehistory.New(filepath.Join(base, "file-history"), sessionID, paths.cwd)
	fileHistories.Store(conv, fh)
	r.dispatcher.SetFileHistory(fh)

	inner := conv.RunLoop
	conv.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		snapshotPrompt(conv, fh, initial)
		if inner != nil {
			return inner(ctx, l, initial)
		}
		if conv.HookHost != nil {
			return conv.HookHost.RunLoop(ctx, l, initial)
		}
		return l.Run(ctx, initial)
	}
}

// snapshotPrompt takes the snapshot for the user prompt that ends initial (a
// re-run of the same turn — a fallback retry — does not snapshot again).
func snapshotPrompt(conv *conversation.Conversation, fh *filehistory.Store, initial []agent.Message) {
	n := len(initial)
	if n == 0 {
		return
	}
	text, ok := conversation.PromptText(initial[n-1])
	if !ok {
		return
	}
	syncFileHistory(conv, fh, false)
	k := filehistory.Key{Index: n - 1, Text: text}
	if last, ok := fh.Latest(); ok && last == k {
		return
	}
	_ = fh.MakeSnapshot(k)
}

// sessionIDOf is the id of the on-disk session the conversation records to
// ("" when persistence is off).
func sessionIDOf(conv *conversation.Conversation) string {
	if s, ok := conv.Store.(interface{ ID() string }); ok && s != nil {
		return s.ID()
	}
	return ""
}

// syncFileHistory follows the conversation onto another on-disk session: a
// resumed session's own saved history is loaded, a /clear starts empty, and a
// rotated session that still holds this history (or, with carry, any rotation)
// keeps the current checkpoints.
func syncFileHistory(conv *conversation.Conversation, fh *filehistory.Store, carry bool) {
	id := sessionIDOf(conv)
	if id == "" || id == fh.SessionID() {
		return
	}
	switch {
	case carry:
		_ = fh.Rebind(id, true)
	case fh.HasSession(id) || len(conv.History) == 0:
		_ = fh.Rebind(id, false)
	default:
		_ = fh.Rebind(id, true)
	}
}

// --- the /rewind picker ------------------------------------------------------

// rewind runs the /rewind flow: pick an earlier user prompt (newest first, with
// what each turn changed), then choose what to restore.
func (d *interactiveDialogs) rewind() {
	conv := d.conv
	fh := fileHistoryFor(conv)
	if fh != nil {
		syncFileHistory(conv, fh, false)
	}
	points := conversation.RewindPoints(conv.History)
	if len(points) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: rewindNothing})
		return
	}
	items := make([]ui.ListItem, 0, len(points))
	for i := len(points) - 1; i >= 0; i-- {
		p := points[i]
		items = append(items, ui.ListItem{
			Label:  rewindPromptLabel(p.Text),
			Value:  strconv.Itoa(p.Index),
			Detail: rewindTurnDetail(fh, points, i),
		})
	}
	subtitle := rewindSubtitleNoCode
	if fh != nil {
		subtitle = rewindSubtitle
	}
	r, ok := d.openAndWait(context.Background(), ui.ListDialog(rewindTitle, items, 10, subtitle))
	if !ok || r.cancelled {
		return
	}
	idx, err := strconv.Atoi(firstChosen(r.values))
	if err != nil {
		return
	}
	var point conversation.RewindPoint
	for _, p := range points {
		if p.Index == idx {
			point = p
		}
	}
	if point.Text == "" {
		d.sendMsg(ui.AppendErrorMsg{Text: "Message not found."})
		return
	}
	d.confirmRewind(fh, point)
}

// confirmRewind asks what to restore for the chosen prompt and applies it.
func (d *interactiveDialogs) confirmRewind(fh *filehistory.Store, p conversation.RewindPoint) {
	key := filehistory.Key{Index: p.Index, Text: p.Text}
	var stats filehistory.DiffStats
	canRestoreCode := false
	if fh != nil {
		if st, ok := fh.DiffStats(key); ok {
			stats = st
			canRestoreCode = len(st.FilesChanged) > 0
		}
	}

	var items []ui.ListItem
	if canRestoreCode {
		items = append(items,
			ui.ListItem{Label: rewindLabelBoth, Value: rewindOptBoth, Detail: "The conversation will be forked."},
			ui.ListItem{Label: rewindLabelConversation, Value: rewindOptConversation, Detail: "The conversation will be forked."},
			ui.ListItem{Label: rewindLabelCode, Value: rewindOptCode, Detail: "The conversation will be unchanged."},
		)
	} else {
		items = append(items, ui.ListItem{Label: rewindLabelConversation, Value: rewindOptConversation, Detail: "The conversation will be forked."})
	}
	items = append(items, ui.ListItem{Label: rewindLabelNevermind, Value: rewindOptNevermind, Detail: "The conversation will be unchanged."})

	lead := "Confirm you want to restore the conversation to the point before you sent this message:"
	if canRestoreCode {
		lead = "Confirm you want to restore to the point before you sent this message:"
	}
	body := []string{lead, "│ " + rewindPromptLabel(p.Text)}
	switch {
	case canRestoreCode:
		body = append(body, rewindCodeSummary(stats), "⚠ "+rewindManualEditsNote)
	case fh != nil:
		if fh.CanRestore(key) {
			body = append(body, "The code has not changed (nothing will be restored).")
		} else {
			body = append(body, "The code will be unchanged.")
		}
	}

	r, ok := d.openAndWait(context.Background(), ui.ListDialog(rewindTitle, items, 0, body...))
	if !ok || r.cancelled {
		return
	}
	choice := firstChosen(r.values)
	if choice == "" || choice == rewindOptNevermind {
		return
	}
	d.applyRewind(fh, p, choice)
}

// applyRewind restores the code (code / both) and then the conversation
// (conversation / both), reporting any failures.
func (d *interactiveDialogs) applyRewind(fh *filehistory.Store, p conversation.RewindPoint, choice string) {
	var codeErr, convErr error
	skipped := 0
	restoredFiles := 0
	if (choice == rewindOptCode || choice == rewindOptBoth) && fh != nil {
		res, err := fh.Rewind(filehistory.Key{Index: p.Index, Text: p.Text})
		codeErr = err
		skipped = res.SkippedLinks
		restoredFiles = len(res.Changed)
	}
	var promptText string
	if choice == rewindOptConversation || choice == rewindOptBoth {
		promptText, convErr = d.conv.RewindTo(p.Index)
		if convErr == nil && fh != nil {
			// The rewound conversation is forked onto a fresh session: its
			// checkpoints move along.
			syncFileHistory(d.conv, fh, true)
		}
	}

	switch {
	case convErr != nil && codeErr != nil:
		d.sendMsg(ui.AppendErrorMsg{Text: fmt.Sprintf("Failed to restore the conversation and code:\n%v\n%v", convErr, codeErr)})
		return
	case codeErr != nil:
		d.sendMsg(ui.AppendErrorMsg{Text: fmt.Sprintf("Failed to restore the code:\n%v", rewindErrText(codeErr))})
		if convErr == nil && promptText != "" {
			d.restorePrompt(promptText)
		}
		return
	case convErr != nil:
		d.sendMsg(ui.AppendErrorMsg{Text: fmt.Sprintf("Failed to restore the conversation:\n%v", convErr)})
	}
	if skipped > 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Restored the code, but skipped %d %s: %s. Skipped files were left untouched.", skipped, pluralWord(skipped, "file", "files"), rewindSkippedReason)})
	}
	if convErr != nil {
		return
	}

	if choice == rewindOptConversation || choice == rewindOptBoth {
		// Redraw the kept conversation in place of the rewound one.
		d.sendMsg(ui.ResetTranscriptMsg{})
		replayHistory(d.sendMsg, d.conv.History)
	}
	switch choice {
	case rewindOptBoth:
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Rewound the code (%d %s) and the conversation to before: %s", restoredFiles, pluralWord(restoredFiles, "file", "files"), rewindPromptLabel(p.Text))})
	case rewindOptConversation:
		d.sendMsg(ui.AppendSystemMsg{Text: "Rewound the conversation to before: " + rewindPromptLabel(p.Text)})
	case rewindOptCode:
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Restored the code (%d %s) to before: %s", restoredFiles, pluralWord(restoredFiles, "file", "files"), rewindPromptLabel(p.Text))})
	}
	if promptText != "" {
		d.restorePrompt(promptText)
		d.contextChanged()
	}
}

// restorePrompt puts the rewound message's text back into the prompt input,
// ready to edit and send again.
func (d *interactiveDialogs) restorePrompt(text string) {
	d.sendMsg(ui.SetComposerTextMsg{Text: text})
}

// rewindErrText unwraps the "no files were restored" error to its user-facing
// wording.
func rewindErrText(err error) string {
	if errors.Is(err, filehistory.ErrNoFilesRestored) {
		return "No files were restored: " + strings.TrimPrefix(err.Error(), filehistory.ErrNoFilesRestored.Error()+": ")
	}
	return err.Error()
}

// rewindPromptLabel is a one-line form of a prompt for the picker.
func rewindPromptLabel(text string) string {
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i]) + " …"
	}
	if r := []rune(line); len(r) > 100 {
		line = string(r[:99]) + "…"
	}
	return line
}

// rewindTurnDetail is a picker row's second line: what that turn changed.
func rewindTurnDetail(fh *filehistory.Store, points []conversation.RewindPoint, i int) string {
	if fh == nil {
		return ""
	}
	p := points[i]
	key := filehistory.Key{Index: p.Index, Text: p.Text}
	if !fh.CanRestore(key) {
		return "⚠ No code restore"
	}
	// The turn ends at the next prompt that has a snapshot (or now).
	var next *filehistory.Key
	for j := i + 1; j < len(points); j++ {
		k := filehistory.Key{Index: points[j].Index, Text: points[j].Text}
		if fh.CanRestore(k) {
			next = &k
			break
		}
	}
	st, _ := fh.TurnStats(key, next)
	switch n := len(st.FilesChanged); n {
	case 0:
		return "No code changes"
	case 1:
		return fmt.Sprintf("%s +%d -%d", filepath.Base(st.FilesChanged[0]), st.Insertions, st.Deletions)
	default:
		return fmt.Sprintf("%d files changed +%d -%d", n, st.Insertions, st.Deletions)
	}
}

// rewindCodeSummary is the confirm screen's code note.
func rewindCodeSummary(st filehistory.DiffStats) string {
	if len(st.FilesChanged) == 0 {
		return "The code has not changed (nothing will be restored)."
	}
	var where string
	switch n := len(st.FilesChanged); n {
	case 1:
		where = filepath.Base(st.FilesChanged[0])
	case 2:
		where = filepath.Base(st.FilesChanged[0]) + " and " + filepath.Base(st.FilesChanged[1])
	default:
		where = fmt.Sprintf("%s and %d other files", filepath.Base(st.FilesChanged[0]), n-1)
	}
	return fmt.Sprintf("The code will be restored +%d -%d in %s.", st.Insertions, st.Deletions, where)
}
