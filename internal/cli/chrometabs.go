package cli

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/tools/browser"
)

// chromeTabs scopes the session's browser commands to its own "gocode" tab
// group and closes that group when the session is done with it:
//
//   - /clear closes the cleared session's group;
//   - switching to another session (/clear, /resume) closes the previous
//     session's group;
//   - when gocode exits, the cleanups still running are awaited and the
//     current session's group is closed, all within
//     browser.TabGroupExitTimeout.
//
// A close leaves alone the tabs the user put in the group and the tabs marked
// keep_open (see browser.TabGroupCleanup). While a subagent is still running
// it may still be using the group, so /clear and a switch then close the
// group only if it is still empty.
//
// Nothing runs for a session that never sent a browser command. A cleanup
// that succeeds shows nothing; a group kept because it is over the close cap
// gets one notice (in the transcript while the session runs, on stderr at
// exit). With GOCODE_DEBUG set, a non-interactive session writes the
// cleanup's notes to stderr.
type chromeTabs struct {
	client  *browser.SessionClient
	cleanup *browser.TabGroupCleanup
	tasks   *subagent.TaskRegistry

	mu      sync.Mutex
	current string
	exiting bool

	// started counts the cleanups switched and cleared start (tests).
	started atomic.Int32
}

// start begins a cleanup of session id's group.
func (ct *chromeTabs) start(id string) *browser.CleanupOp {
	ct.started.Add(1)
	return ct.cleanup.Start(id, ct.agentsRunning())
}

// wireChromeTabs wraps the dispatcher's browser client so every command
// carries the session id, and sets up the cleanup. A dispatcher without a
// browser client is left alone.
func (r *sessionRuntime) wireChromeTabs(env *Env, sessionID string, interactive bool) {
	if r.dispatcher == nil || r.dispatcher.BrowserClient == nil {
		return
	}
	raw := r.dispatcher.BrowserClient
	ct := &chromeTabs{current: sessionID}
	ct.client = &browser.SessionClient{Inner: raw, SessionID: ct.sessionID}
	ct.cleanup = &browser.TabGroupCleanup{Client: raw}
	var stderr io.Writer = os.Stderr
	if env != nil && env.Stderr != nil {
		stderr = env.Stderr
	}
	if !interactive && envGetenv(env)("GOCODE_DEBUG") != "" {
		ct.cleanup.Debugf = func(format string, args ...any) {
			fmt.Fprintf(stderr, format+"\n", args...)
		}
	}
	ct.cleanup.Notify = func(msg string) {
		r.notifyMu.Lock()
		live := r.notify
		r.notifyMu.Unlock()
		if live != nil && !ct.isExiting() {
			live(msg)
			return
		}
		fmt.Fprintln(stderr, msg)
	}
	r.dispatcher.SetBrowserClient(ct.client)
	r.chromeTabs = ct
}

func (ct *chromeTabs) isExiting() bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.exiting
}

// sessionID is the session the next browser command belongs to.
func (ct *chromeTabs) sessionID() string {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.current
}

// switchTo makes id the current session and returns the previous one.
func (ct *chromeTabs) switchTo(id string) string {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	prev := ct.current
	ct.current = id
	return prev
}

// agentsRunning reports whether a subagent is still running (it may still be
// using the session's tabs).
func (ct *chromeTabs) agentsRunning() bool {
	if ct.tasks == nil {
		return false
	}
	for _, t := range ct.tasks.List() {
		if !t.Status.IsTerminal() {
			return true
		}
	}
	return false
}

// switched records that the conversation now records to id and closes the
// previous session's group (only if it is still empty while a subagent runs).
func (ct *chromeTabs) switched(id string) *browser.CleanupOp {
	if ct == nil {
		return nil
	}
	prev := ct.switchTo(id)
	if prev == id || !ct.client.Used(prev) {
		return nil
	}
	return ct.start(prev)
}

// cleared handles /clear: the cleared session (the one current before the
// clear) has its group closed (only if it is still empty while a subagent
// runs) and the conversation moves on to id. It starts one cleanup: going
// through switched as well started a second one for the same session, which
// closed the same tabs again when the first had already finished.
func (ct *chromeTabs) cleared(id string) *browser.CleanupOp {
	if ct == nil {
		return nil
	}
	prev := ct.switchTo(id)
	if !ct.client.Used(prev) {
		return nil
	}
	return ct.start(prev)
}

// atExit waits for the running cleanups and closes the current session's
// group, bounded by browser.TabGroupExitTimeout.
func (ct *chromeTabs) atExit() {
	if ct == nil {
		return
	}
	ct.mu.Lock()
	ct.exiting = true
	id := ct.current
	ct.mu.Unlock()
	ct.cleanup.AtExit(id, ct.client.Used(id), browser.TabGroupExitTimeout)
}
