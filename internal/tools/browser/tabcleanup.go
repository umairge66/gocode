package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/chromehost"
)

// Session-scoped tabs and the end-of-session tab group cleanup.
//
// Every browser command a session sends carries its session id
// (params.sessionId, see SessionClient), so the extension keeps one "gocode"
// tab group per session and tabs_context lists only the calling session's
// group. The model is told to close the tabs it opened before finishing and to
// mark a page the user wants to keep with keep_open (see TabCleanupNote and
// the tool descriptions); TabGroupCleanup is the safety net that closes a
// session's group when the session is cleared, switched away from, or ends.

// TabCleanupNote is the line that follows the tab listing navigate appends
// when it picks (or opens) the tab itself; closeTool names the tool that
// closes a tab.
func TabCleanupNote(closeTool string) string {
	return "Tabs in this group were opened for this task and are yours to clean up: close each with " + closeTool + " once you no longer need it and before finishing. If the user wants a page kept open, set keep_open on it instead of leaving it; the other tabs gocode opened are closed when the session ends."
}

// KeepOpenDescription is the schema description of the keep_open parameter
// of navigate and tabs_create.
const KeepOpenDescription = "Set true when the user asked to see this page or keep it open; it then stays open after the session ends."

// SessionClient tags every command with the current session's id
// (params.sessionId) and remembers which sessions sent browser commands, so
// the cleanup only runs for a session that could have opened tabs.
type SessionClient struct {
	Inner Client
	// SessionID returns the id of the session the next command belongs to.
	SessionID func() string

	mu   sync.Mutex
	used map[string]bool
}

// Call implements Client.
func (c *SessionClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.Inner == nil {
		return nil, ErrNoClient
	}
	id := ""
	if c.SessionID != nil {
		id = c.SessionID()
	}
	if id == "" {
		return c.Inner.Call(ctx, method, params)
	}
	tagged, err := attachParam(params, "sessionId", id)
	if err != nil {
		return nil, err
	}
	raw, err := c.Inner.Call(ctx, method, tagged)
	if !errors.Is(err, chromehost.ErrChromeNotConnected) {
		c.mu.Lock()
		if c.used == nil {
			c.used = map[string]bool{}
		}
		c.used[id] = true
		c.mu.Unlock()
	}
	return raw, err
}

// Used reports whether session id sent a command that reached Chrome.
func (c *SessionClient) Used(id string) bool {
	if c == nil || id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used[id]
}

// Cleanup limits.
const (
	// TabGroupCloseCap is the most tabs the cleanup closes in one session; a
	// session with more tabs to close keeps its group as it is.
	TabGroupCloseCap = 50
	// TabGroupCallTimeout bounds each tabs_context / tabs_close the cleanup
	// sends.
	TabGroupCallTimeout = 5 * time.Second
	// TabGroupExitTimeout bounds the whole cleanup when gocode exits.
	TabGroupExitTimeout = 1500 * time.Millisecond
)

// blankTabURLs are the pages a tab shows before anything was loaded in it.
var blankTabURLs = map[string]bool{"chrome://newtab/": true, "about:blank": true}

// CleanupStatus is the outcome of one tab group cleanup.
type CleanupStatus string

const (
	// CleanupNotConnected: Chrome (or the extension) is not connected.
	CleanupNotConnected CleanupStatus = "not_connected"
	// CleanupNoGroup: the session has no tab group (or the listing failed).
	CleanupNoGroup CleanupStatus = "no_group"
	// CleanupKept: the group was left open (only an empty group may be closed
	// and it has content, it holds no tab the cleanup may close, it is over
	// the close cap, or its tab list could not be read).
	CleanupKept CleanupStatus = "kept"
	// CleanupClosed: the tabs gocode opened in the group were closed (Failed >
	// 0 when some of them could not be).
	CleanupClosed CleanupStatus = "closed"
)

// CleanupResult reports what a cleanup did.
type CleanupResult struct {
	Status CleanupStatus
	// Tabs is the number of tabs in the group.
	Tabs int
	// Kept counts the tabs left open on purpose: tabs the user put in the
	// group and tabs marked keep_open.
	Kept int
	// Closed and Failed count the tab closes (closed outcome).
	Closed int
	Failed int
}

// TabGroupCleanup closes a session's "gocode" tab group.
//
// Only tabs gocode opened itself are closed (the extension flags them
// openedByGocode in its tab listing); a tab the user dragged into the group
// and a tab marked keep_open stay open, and when only such tabs remain the
// group stays as it is. The closes go to the extension as one tabs_close
// command per group (last tab first), pinned to the group and the session and
// marked as a cleanup, so the extension itself refuses any tab gocode did not
// open, a kept tab, and a tab that has left the group. The close is best
// effort: a tab that cannot be closed is counted in Failed and the others are
// still closed.
//
// With onlyIfEmpty the group is closed only when every tab in it is still
// blank (a new tab page or about:blank).
type TabGroupCleanup struct {
	// Client reaches the extension. It is called with an explicit sessionId,
	// so it must not be a SessionClient (which would tag the current session).
	Client Client
	// Debugf receives diagnostic lines; nil drops them.
	Debugf func(format string, args ...any)
	// Notify receives the one user-facing notice a cleanup may produce: the
	// group was kept because it is over the close cap. nil drops it.
	Notify func(msg string)
	// CallTimeout bounds each extension call (0 -> TabGroupCallTimeout).
	CallTimeout time.Duration
	// Cap is the close cap (0 -> TabGroupCloseCap).
	Cap int

	mu       sync.Mutex
	inFlight map[string]*CleanupOp
	notified map[string]bool
}

// CleanupOp is a cleanup in progress.
type CleanupOp struct {
	onlyIfEmpty bool
	done        chan struct{}
	res         CleanupResult
}

// Done is closed when the cleanup has finished.
func (op *CleanupOp) Done() <-chan struct{} { return op.done }

// Wait blocks until the cleanup finishes or ctx ends; ok is false when ctx
// ended first.
func (op *CleanupOp) Wait(ctx context.Context) (res CleanupResult, ok bool) {
	select {
	case <-op.done:
		return op.res, true
	case <-ctx.Done():
		return CleanupResult{}, false
	}
}

func (c *TabGroupCleanup) debugf(format string, args ...any) {
	if c.Debugf != nil {
		c.Debugf(format, args...)
	}
}

// notifyOnce delivers msg once per session.
func (c *TabGroupCleanup) notifyOnce(sessionID, msg string) {
	c.mu.Lock()
	if c.notified == nil {
		c.notified = map[string]bool{}
	}
	seen := c.notified[sessionID]
	c.notified[sessionID] = true
	c.mu.Unlock()
	if !seen && c.Notify != nil {
		c.Notify(msg)
	}
}

// Start begins closing session id's tab group in the background and returns
// the operation. A cleanup already running for the session is reused when it
// covers the request (the new one only closes an empty group, or the running
// one closes the group in any case); otherwise the new cleanup runs once the
// running one has finished.
func (c *TabGroupCleanup) Start(sessionID string, onlyIfEmpty bool) *CleanupOp {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight == nil {
		c.inFlight = map[string]*CleanupOp{}
	}
	prev := c.inFlight[sessionID]
	if prev != nil && (onlyIfEmpty || !prev.onlyIfEmpty) {
		return prev
	}
	op := &CleanupOp{onlyIfEmpty: onlyIfEmpty, done: make(chan struct{})}
	c.inFlight[sessionID] = op
	go func() {
		if prev != nil {
			<-prev.done
		}
		op.res = c.run(sessionID, onlyIfEmpty)
		c.mu.Lock()
		if c.inFlight[sessionID] == op {
			delete(c.inFlight, sessionID)
		}
		c.mu.Unlock()
		close(op.done)
	}()
	return op
}

// InFlight returns the cleanups still running.
func (c *TabGroupCleanup) InFlight() []*CleanupOp {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*CleanupOp, 0, len(c.inFlight))
	for _, op := range c.inFlight {
		out = append(out, op)
	}
	return out
}

// AtExit waits for the running cleanups and, when closeCurrent is set, closes
// session id's group — all bounded by timeout (0 -> TabGroupExitTimeout) so
// exit never hangs on Chrome. It reports whether everything finished in time.
func (c *TabGroupCleanup) AtExit(sessionID string, closeCurrent bool, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = TabGroupExitTimeout
	}
	ops := c.InFlight()
	if closeCurrent {
		ops = append(ops, c.Start(sessionID, false))
	}
	if len(ops) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, op := range ops {
		if _, ok := op.Wait(ctx); !ok {
			c.debugf("[tab group cleanup] chrome tab group close timed out at exit")
			return false
		}
	}
	return true
}

func (c *TabGroupCleanup) call(method string, params any) (json.RawMessage, error) {
	d := c.CallTimeout
	if d <= 0 {
		d = TabGroupCallTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	type reply struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		raw, err := c.Client.Call(ctx, method, params)
		ch <- reply{raw, err}
	}()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("%s timed out", method)
	}
}

// groupTab is one tab of the session's group.
type groupTab struct {
	ID      int
	URL     string
	GroupID int
	// Opened: gocode opened the tab itself (not a tab the user put in the
	// group). Keep: the tab is marked keep_open.
	Opened bool
	Keep   bool
}

func (c *TabGroupCleanup) run(sessionID string, onlyIfEmpty bool) CleanupResult {
	if c.Client == nil {
		return CleanupResult{Status: CleanupNotConnected}
	}
	raw, err := c.call(chromehost.MethodTabsContext, map[string]any{"createIfEmpty": false, "sessionId": sessionID})
	if errors.Is(err, chromehost.ErrChromeNotConnected) {
		c.debugf("[tab group cleanup] Chrome not connected, skipping")
		return CleanupResult{Status: CleanupNotConnected}
	}
	if err != nil {
		c.debugf("[tab group cleanup] tabs_context failed: %v", err)
		return CleanupResult{Status: CleanupNoGroup}
	}
	var listing struct {
		Groups []struct {
			ID *int `json:"id"`
		} `json:"groups"`
		Tabs []struct {
			ID             *int   `json:"id"`
			URL            string `json:"url"`
			GroupID        *int   `json:"groupId"`
			OpenedByGocode bool   `json:"openedByGocode"`
			KeepOpen       bool   `json:"keepOpen"`
		} `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		c.debugf("[tab group cleanup] unreadable tab list, keeping the group")
		return CleanupResult{Status: CleanupKept}
	}
	groups := map[int]bool{}
	var groupIDs []string
	var order []int
	for _, g := range listing.Groups {
		if g.ID == nil {
			c.debugf("[tab group cleanup] unreadable tab list, keeping the group")
			return CleanupResult{Status: CleanupKept}
		}
		if !groups[*g.ID] {
			order = append(order, *g.ID)
		}
		groups[*g.ID] = true
		groupIDs = append(groupIDs, fmt.Sprint(*g.ID))
	}
	if len(groups) == 0 {
		c.debugf("[tab group cleanup] no group for session")
		return CleanupResult{Status: CleanupNoGroup}
	}
	label := strings.Join(groupIDs, ",")
	var tabs []groupTab
	for _, t := range listing.Tabs {
		if t.ID == nil || t.GroupID == nil {
			c.debugf("[tab group cleanup] group %s: unreadable tab list, keeping it", label)
			return CleanupResult{Status: CleanupKept}
		}
		// Only the tabs in the session's groups; an ungroupable gocode tab
		// belongs to no session's group.
		if groups[*t.GroupID] {
			tabs = append(tabs, groupTab{ID: *t.ID, URL: t.URL, GroupID: *t.GroupID, Opened: t.OpenedByGocode, Keep: t.KeepOpen})
		}
	}
	if onlyIfEmpty {
		for _, t := range tabs {
			if !blankTabURLs[t.URL] {
				c.debugf("[tab group cleanup] group %s has content, keeping %d tabs", label, len(tabs))
				return CleanupResult{Status: CleanupKept, Tabs: len(tabs)}
			}
		}
	}
	// The tabs to close, per group, last first.
	byGroup := map[int][]int{}
	closable := 0
	for i := len(tabs) - 1; i >= 0; i-- {
		t := tabs[i]
		if !t.Opened || t.Keep {
			continue
		}
		byGroup[t.GroupID] = append(byGroup[t.GroupID], t.ID)
		closable++
	}
	kept := len(tabs) - closable
	if closable == 0 {
		c.debugf("[tab group cleanup] group %s: no tab to close, keeping %d tabs", label, len(tabs))
		return CleanupResult{Status: CleanupKept, Tabs: len(tabs), Kept: kept}
	}
	limit := c.Cap
	if limit <= 0 {
		limit = TabGroupCloseCap
	}
	if closable > limit {
		c.debugf("[tab group cleanup] group %s holds %d tabs to close, over the close cap; keeping it", label, closable)
		c.notifyOnce(sessionID, fmt.Sprintf("Left the gocode tab group in Chrome open: it holds %d tabs gocode opened, more than the %d it closes automatically.", closable, limit))
		return CleanupResult{Status: CleanupKept, Tabs: len(tabs), Kept: len(tabs)}
	}
	res := CleanupResult{Status: CleanupClosed, Tabs: len(tabs), Kept: kept}
	for _, gid := range order {
		ids := byGroup[gid]
		if len(ids) == 0 {
			continue
		}
		raw, err := c.call(chromehost.MethodTabsClose, map[string]any{"tabIds": ids, "groupId": gid, "sessionId": sessionID, "cleanup": true})
		if err != nil {
			c.debugf("[tab group cleanup] group %d: close failed: %v", gid, err)
			res.Failed += len(ids)
			continue
		}
		var reply struct {
			Closed []int `json:"closed"`
		}
		if json.Unmarshal(raw, &reply) != nil {
			res.Failed += len(ids)
			continue
		}
		requested := map[int]bool{}
		for _, id := range ids {
			requested[id] = true
		}
		n := 0
		for _, id := range reply.Closed {
			if requested[id] {
				requested[id] = false
				n++
			}
		}
		res.Closed += n
		res.Failed += len(ids) - n
	}
	suffix := ""
	if res.Failed > 0 {
		suffix = fmt.Sprintf(", %d could not be closed", res.Failed)
	}
	c.debugf("[tab group cleanup] group %s: closed %d/%d tabs, kept %d%s", label, res.Closed, closable, kept, suffix)
	return res
}
