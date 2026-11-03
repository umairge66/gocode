package cli

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/toolrun"
)

// A background worker's permission prompts can be answered by a daemon
// client as well as in the attached terminal (daemon bgpermission.go): while
// the dialog is open the worker records the prompt in its job directory and
// watches for the decision the daemon's permission-response op writes there.
// Whichever answer comes first wins; the other is withdrawn.

// remotePermissionPoll is how often an open prompt checks for a decision.
const remotePermissionPoll = 200 * time.Millisecond

// openPermission records req as the job's open permission prompt and returns
// a channel that delivers a client's decision for it, and the func that
// withdraws the prompt (call it once the prompt is answered either way). A
// nil *jobStatus (not a background worker) returns a nil channel.
func (j *jobStatus) openPermission(ctx context.Context, req toolrun.PermissionRequest) (<-chan headless.PermissionResult, func()) {
	if j == nil {
		return nil, func() {}
	}
	p := daemon.PendingPermission{
		RequestID: "perm_" + ids.RandomUUID(),
		ToolName:  req.ToolName,
		Input:     toolInputOrEmpty(req.Input),
		ToolUseID: toolrun.ToolUseIDFrom(ctx),
		Reason:    req.Reason,
		At:        j.stamp(),
	}
	if !req.NoAlwaysAllow {
		p.Suggestions = req.Suggested
	}
	// An answer left over from an earlier prompt is not for this one.
	daemon.ClearPendingPermission(j.jobDir, "")
	if daemon.WritePendingPermission(j.jobDir, p) != nil {
		return nil, func() {}
	}
	decisions := make(chan headless.PermissionResult, 1)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(remotePermissionPoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			raw, ok := daemon.TakePermissionAnswer(j.jobDir, p.RequestID)
			if !ok {
				continue
			}
			var res headless.PermissionResult
			if json.Unmarshal(raw, &res) != nil {
				continue
			}
			decisions <- res
			return
		}
	}()
	var once sync.Once
	return decisions, func() {
		once.Do(func() {
			close(done)
			daemon.ClearPendingPermission(j.jobDir, p.RequestID)
		})
	}
}

// remotePermissionResponse maps a client's decision onto the prompt's answer
// as a stream-json host's is mapped (permissionFromResult): rules it adds
// are for this session only, and a decision that cannot be read denies.
func remotePermissionResponse(req toolrun.PermissionRequest, res headless.PermissionResult) toolrun.PermissionResponse {
	resp, err := permissionFromResult(req, res)
	if err != nil {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied, Feedback: "The permission answer could not be read: " + err.Error()}
	}
	return resp
}
