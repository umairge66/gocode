package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
)

// -----------------------------------------------------------------------------
// Permission prompts of a background session, answered by a daemon client.
//
// A background session runs the interactive UI in its pty. While one of its
// permission prompts is open, the worker records it in
// <job>/permission.json (PendingPermission) beside state.json, whose tempo is
// then "blocked" with the prompt as `needs`. `list` carries the open prompt on
// the job's record (JobRecord.Permission).
//
// A client answers with the authed permission-response op: {short,
// requestId, decision}, decision being the Agent SDK PermissionResult shape
// ({"behavior":"allow",…} or {"behavior":"deny","message":…}). The daemon
// checks that the request is still the open one and writes the decision to
// <job>/permission-response.json; the worker, which watches for it while the
// prompt is open, takes it as the answer and closes the dialog. Answering in
// the attached terminal first removes permission.json, so a late
// permission-response is refused (or, in a race, dropped by the worker as no
// longer matching its prompt).
//
// Only permission prompts take this path: AskUserQuestion and the plan
// approval are still answered in the terminal.
// -----------------------------------------------------------------------------

const (
	// PermissionRequestFile holds a job's open permission prompt.
	PermissionRequestFile = "permission.json"
	// PermissionResponseFile holds a client's answer to it.
	PermissionResponseFile = "permission-response.json"
)

// maxPermissionBytes bounds both files when read.
const maxPermissionBytes = 1 << 20

// PendingPermission is a background session's open permission prompt.
type PendingPermission struct {
	RequestID string          `json:"requestId"`
	ToolName  string          `json:"toolName"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"toolUseId,omitempty"`
	// Reason is why the prompt is shown (the permission decision's reason).
	Reason string `json:"reason,omitempty"`
	// Suggestions are the "don't ask again" rules the prompt offers, most
	// specific first (e.g. "Bash(make:*)").
	Suggestions []string `json:"suggestions,omitempty"`
	At          string   `json:"at"`
}

// PermissionAnswer is a client's decision for one request.
type PermissionAnswer struct {
	RequestID string          `json:"requestId"`
	Decision  json.RawMessage `json:"decision"`
	At        string          `json:"at"`
}

// WritePendingPermission records p as the job's open prompt.
func WritePendingPermission(jobDir string, p PendingPermission) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(jobDir, PermissionRequestFile), b, 0o600)
}

// ReadPendingPermission returns the job's open prompt, nil when none.
func ReadPendingPermission(jobDir string) (*PendingPermission, error) {
	var p PendingPermission
	ok, err := readPermissionFile(filepath.Join(jobDir, PermissionRequestFile), &p)
	if !ok || err != nil {
		return nil, err
	}
	return &p, nil
}

// ClearPendingPermission removes the open prompt requestID and any answer
// left for it (or for an earlier prompt).
func ClearPendingPermission(jobDir, requestID string) {
	if p, _ := ReadPendingPermission(jobDir); p != nil && p.RequestID == requestID {
		_ = os.Remove(filepath.Join(jobDir, PermissionRequestFile))
	}
	_ = os.Remove(filepath.Join(jobDir, PermissionResponseFile))
}

// WritePermissionAnswer records a client's answer.
func WritePermissionAnswer(jobDir string, a PermissionAnswer) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(jobDir, PermissionResponseFile), b, 0o600)
}

// TakePermissionAnswer returns the answer for requestID and removes it. An
// answer for another request is stale and removed too.
func TakePermissionAnswer(jobDir, requestID string) (json.RawMessage, bool) {
	path := filepath.Join(jobDir, PermissionResponseFile)
	var a PermissionAnswer
	if ok, err := readPermissionFile(path, &a); !ok {
		if err != nil {
			_ = os.Remove(path)
		}
		return nil, false
	}
	_ = os.Remove(path)
	if a.RequestID != requestID || len(a.Decision) == 0 {
		return nil, false
	}
	return a.Decision, true
}

// readPermissionFile decodes path into v; ok is false when it is missing.
func readPermissionFile(path string, v any) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxPermissionBytes {
		return false, fmt.Errorf("%s is not a regular file of at most %d bytes", filepath.Base(path), maxPermissionBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	return true, nil
}

// validPermissionDecision reports whether d is a PermissionResult object
// with behavior allow or deny.
func validPermissionDecision(d json.RawMessage) bool {
	var v struct {
		Behavior string `json:"behavior"`
	}
	return json.Unmarshal(d, &v) == nil && (v.Behavior == "allow" || v.Behavior == "deny")
}

// opPermissionResponse routes a client's decision to the job's open
// permission prompt.
func (m *Manager) opPermissionResponse(_ context.Context, req Request, rw ReplyWriter) {
	if !KeysMatch(req.Auth, m.cfg.ControlKey) {
		_ = rw.Send(FailReply(errutil.WorkerEAuth, "permission-response rejected: this client didn't present the daemon control key"))
		return
	}
	h := m.get(req.Short)
	if h == nil {
		_ = rw.Send(errNoJob())
		return
	}
	h.mu.Lock()
	live := h.live()
	h.mu.Unlock()
	if !live {
		_ = rw.Send(errNoJob())
		return
	}
	if !validPermissionDecision(req.Decision) {
		_ = rw.Send(FailReply(errutil.WorkerEProto, `permission-response needs a decision {"behavior":"allow"|"deny",…}`))
		return
	}
	p, err := ReadPendingPermission(h.jobDir)
	if err != nil || p == nil {
		_ = rw.Send(FailReply(errutil.WorkerENoReply, "the session has no open permission prompt"))
		return
	}
	if req.RequestID != "" && req.RequestID != p.RequestID {
		_ = rw.Send(FailReply(errutil.WorkerEStale, "permission request %s is no longer open (the open one is %s)", req.RequestID, p.RequestID))
		return
	}
	if err := WritePermissionAnswer(h.jobDir, PermissionAnswer{RequestID: p.RequestID, Decision: req.Decision, At: isoTime(time.Now())}); err != nil {
		_ = rw.Send(FailReply(errutil.WorkerEUnknown, "could not record the decision: %v", err))
		return
	}
	rep := OKReply(OpPermissionResponse)
	rep.Short = req.Short
	_ = rw.Send(rep)
}

// withPendingPermission adds each job's open permission prompt to its record.
func withPendingPermission(h *JobHandle, rec JobRecord) JobRecord {
	if rec.Outcome == "" && h.jobDir != "" {
		rec.Permission, _ = ReadPendingPermission(h.jobDir)
	}
	return rec
}
