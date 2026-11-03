package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/chromehost"
)

// extTab is a tab of the fake extension. Opened: gocode opened the tab;
// Keep: the tab is marked keep_open.
type extTab struct {
	ID      int
	URL     string
	GroupID int
	Opened  bool
	Keep    bool
}

// fakeExt mimics the extension's session-scoped tabs_context and its
// group-pinned tabs_close (one tabId, or several tabIds with cleanup).
// groupSession maps a gocode group to its session.
type fakeExt struct {
	mu           sync.Mutex
	tabs         []extTab
	groupSession map[int]string
	failClose    map[int]bool
	failAll      bool
	notConnected bool
	raw          json.RawMessage // overrides the tabs_context reply
	block        chan struct{}   // when set, every call waits for it (or ctx)
	ignoreCtx    bool            // a blocked call ignores ctx
	delay        time.Duration   // every call takes this long
	calls        []string
	closed       []int
}

func (f *fakeExt) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if f.block != nil {
		if f.ignoreCtx {
			<-f.block
		} else {
			select {
			case <-f.block:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(params)
	f.calls = append(f.calls, method+" "+string(b))
	if f.notConnected {
		return nil, chromehost.ErrChromeNotConnected
	}
	var p struct {
		SessionID     string `json:"sessionId"`
		CreateIfEmpty bool   `json:"createIfEmpty"`
		TabID         int    `json:"tabId"`
		TabIDs        []int  `json:"tabIds"`
		GroupID       *int   `json:"groupId"`
		Cleanup       bool   `json:"cleanup"`
	}
	_ = json.Unmarshal(b, &p)
	switch method {
	case chromehost.MethodTabsContext:
		if f.raw != nil {
			return f.raw, nil
		}
		type g struct {
			ID int `json:"id"`
		}
		type t struct {
			ID             int    `json:"id"`
			URL            string `json:"url"`
			GroupID        int    `json:"groupId"`
			OpenedByGocode bool   `json:"openedByGocode"`
			KeepOpen       bool   `json:"keepOpen"`
		}
		var out struct {
			Groups []g `json:"groups"`
			Tabs   []t `json:"tabs"`
		}
		seen := map[int]bool{}
		for _, tb := range f.tabs {
			if tb.GroupID == -1 {
				out.Tabs = append(out.Tabs, t{tb.ID, tb.URL, tb.GroupID, tb.Opened, tb.Keep})
				continue
			}
			if f.groupSession[tb.GroupID] != p.SessionID {
				continue
			}
			if !seen[tb.GroupID] {
				seen[tb.GroupID] = true
				out.Groups = append(out.Groups, g{tb.GroupID})
			}
			out.Tabs = append(out.Tabs, t{tb.ID, tb.URL, tb.GroupID, tb.Opened, tb.Keep})
		}
		return json.Marshal(out)
	case chromehost.MethodTabsClose:
		if f.failAll {
			return nil, errors.New("close failed")
		}
		ids := p.TabIDs
		if ids == nil {
			ids = []int{p.TabID}
		}
		closed := []int{}
		var refused []int
		for _, id := range ids {
			if err := f.closeOne(id, p.GroupID, p.SessionID, p.Cleanup); err != nil {
				if p.TabIDs == nil {
					return nil, err
				}
				refused = append(refused, id)
				continue
			}
			closed = append(closed, id)
		}
		if p.TabIDs == nil {
			return json.RawMessage(fmt.Sprintf(`{"closed":%d}`, p.TabID)), nil
		}
		return json.Marshal(map[string]any{"closed": closed, "refused": refused})
	}
	return json.RawMessage(`{}`), nil
}

func (f *fakeExt) closeOne(id int, groupID *int, session string, cleanup bool) error {
	if f.failClose[id] {
		return errors.New("close failed")
	}
	for i, tb := range f.tabs {
		if tb.ID != id {
			continue
		}
		if groupID == nil || tb.GroupID != *groupID || f.groupSession[tb.GroupID] != session {
			return fmt.Errorf("tab %d is not in the group", id)
		}
		if cleanup && (!tb.Opened || tb.Keep) {
			return fmt.Errorf("tab %d may not be closed by a cleanup", id)
		}
		f.tabs = append(f.tabs[:i], f.tabs[i+1:]...)
		f.closed = append(f.closed, id)
		return nil
	}
	return fmt.Errorf("tab %d does not exist", id)
}

func (f *fakeExt) closedTabs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.closed...)
}

func (f *fakeExt) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExt) remaining() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for _, tb := range f.tabs {
		out = append(out, tb.ID)
	}
	return out
}

func twoSessions() *fakeExt {
	return &fakeExt{
		groupSession: map[int]string{10: "A", 20: "B"},
		tabs: []extTab{
			{1, "https://a.example/1", 10, true, false},
			{2, "https://a.example/2", 10, true, false},
			{3, "chrome://newtab/", 10, true, false},
			{4, "https://b.example/", 20, true, false},
			{5, "https://popup.example/", -1, true, false}, // ungroupable gocode tab
		},
	}
}

func runCleanup(t *testing.T, c *TabGroupCleanup, session string, onlyIfEmpty bool) CleanupResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, ok := c.Start(session, onlyIfEmpty).Wait(ctx)
	if !ok {
		t.Fatal("cleanup did not finish")
	}
	return res
}

func TestCleanupClosesOnlyTheSessionGroupInReverse(t *testing.T) {
	ext := twoSessions()
	res := runCleanup(t, &TabGroupCleanup{Client: ext}, "A", false)
	if res.Status != CleanupClosed || res.Closed != 3 || res.Failed != 0 || res.Tabs != 3 || res.Kept != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := fmt.Sprint(ext.closedTabs()); got != "[3 2 1]" {
		t.Fatalf("closed %s, want [3 2 1] (last first)", got)
	}
	// Session B's tab and the ungroupable tab are left alone.
	if got := fmt.Sprint(ext.remaining()); got != "[4 5]" {
		t.Fatalf("remaining tabs = %s", got)
	}
	// The listing asked for this session without creating anything; the close
	// is one command pinned to the group and the session, marked as a cleanup.
	if !strings.Contains(ext.calls[0], `"createIfEmpty":false`) || !strings.Contains(ext.calls[0], `"sessionId":"A"`) {
		t.Fatalf("listing call = %s", ext.calls[0])
	}
	if len(ext.calls) != 2 {
		t.Fatalf("calls = %v, want one listing and one close", ext.calls)
	}
	for _, want := range []string{`"groupId":10`, `"sessionId":"A"`, `"cleanup":true`, `"tabIds":[3,2,1]`} {
		if !strings.Contains(ext.calls[1], want) {
			t.Fatalf("close call %s lacks %s", ext.calls[1], want)
		}
	}
}

func TestCleanupClosesGroupWithRealPages(t *testing.T) {
	ext := &fakeExt{groupSession: map[int]string{10: "A"}, tabs: []extTab{
		{1, "https://docs.example/", 10, true, false},
		{2, "https://shop.example/cart", 10, true, false},
	}}
	res := runCleanup(t, &TabGroupCleanup{Client: ext}, "A", false)
	if res.Status != CleanupClosed || res.Closed != 2 || len(ext.remaining()) != 0 {
		t.Fatalf("result = %+v, remaining %v", res, ext.remaining())
	}
}

func TestCleanupSkipsUserAndKeptTabs(t *testing.T) {
	ext := &fakeExt{groupSession: map[int]string{10: "A"}, tabs: []extTab{
		{1, "https://a.example/", 10, true, false},
		{2, "https://mail.example/", 10, false, false}, // dragged in by the user
		{3, "https://report.example/", 10, true, true}, // keep_open
		{4, "https://b.example/", 10, true, false},
	}}
	res := runCleanup(t, &TabGroupCleanup{Client: ext}, "A", false)
	if res.Status != CleanupClosed || res.Closed != 2 || res.Kept != 2 || res.Tabs != 4 {
		t.Fatalf("result = %+v", res)
	}
	if got := fmt.Sprint(ext.remaining()); got != "[2 3]" {
		t.Fatalf("remaining = %s, want the user's tab and the kept tab", got)
	}
	if strings.Contains(ext.calls[1], `"tabIds":[4,3`) || !strings.Contains(ext.calls[1], `"tabIds":[4,1]`) {
		t.Fatalf("close call = %s", ext.calls[1])
	}

	// Only such tabs left: nothing is sent and the group stays.
	before := ext.callCount()
	res = runCleanup(t, &TabGroupCleanup{Client: ext}, "A", false)
	if res.Status != CleanupKept || res.Kept != 2 || ext.callCount() != before+1 {
		t.Fatalf("result = %+v, calls %v", res, ext.calls[before:])
	}

	// A listing without the openedByGocode flags closes nothing.
	old := &fakeExt{raw: json.RawMessage(`{"groups":[{"id":10}],"tabs":[{"id":1,"url":"https://a.example/","groupId":10}]}`)}
	if res := runCleanup(t, &TabGroupCleanup{Client: old}, "A", false); res.Status != CleanupKept || old.callCount() != 1 {
		t.Fatalf("listing without flags: %+v (%d calls)", res, old.callCount())
	}
}

func TestCleanupIsBestEffort(t *testing.T) {
	ext := twoSessions()
	ext.failClose = map[int]bool{2: true}
	res := runCleanup(t, &TabGroupCleanup{Client: ext}, "A", false)
	if res.Status != CleanupClosed || res.Closed != 2 || res.Failed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := fmt.Sprint(ext.closedTabs()); got != "[3 1]" {
		t.Fatalf("closed %s, want [3 1]", got)
	}

	ext = twoSessions()
	ext.failAll = true
	res = runCleanup(t, &TabGroupCleanup{Client: ext}, "A", false)
	if res.Status != CleanupClosed || res.Closed != 0 || res.Failed != 3 {
		t.Fatalf("failed command: result = %+v", res)
	}
}

func TestCleanupKeepsGroupOverTheCap(t *testing.T) {
	ext := &fakeExt{groupSession: map[int]string{10: "A"}}
	for i := 1; i <= TabGroupCloseCap+1; i++ {
		ext.tabs = append(ext.tabs, extTab{i, "https://e.example/", 10, true, false})
	}
	var notices []string
	c := &TabGroupCleanup{Client: ext, Notify: func(m string) { notices = append(notices, m) }}
	res := runCleanup(t, c, "A", false)
	if res.Status != CleanupKept || res.Tabs != TabGroupCloseCap+1 || len(ext.closedTabs()) != 0 {
		t.Fatalf("over the cap: result = %+v, closed %v", res, ext.closedTabs())
	}
	runCleanup(t, c, "A", false)
	if len(notices) != 1 || !strings.Contains(notices[0], "51 tabs") || !strings.Contains(notices[0], "50") {
		t.Fatalf("notices = %q, want exactly one", notices)
	}
	// Tabs the cleanup would not close do not count toward the cap.
	ext.tabs[0].Opened = false
	res = runCleanup(t, c, "A", false)
	if res.Status != CleanupClosed || res.Closed != TabGroupCloseCap || res.Kept != 1 {
		t.Fatalf("at the cap: result = %+v", res)
	}
	if len(notices) != 1 {
		t.Fatalf("a closed group produced a notice: %q", notices)
	}
}

func TestCleanupOnlyIfEmpty(t *testing.T) {
	ext := twoSessions()
	res := runCleanup(t, &TabGroupCleanup{Client: ext}, "A", true)
	if res.Status != CleanupKept || res.Tabs != 3 || len(ext.closedTabs()) != 0 {
		t.Fatalf("group with content: result = %+v, closed %v", res, ext.closedTabs())
	}
	blank := &fakeExt{groupSession: map[int]string{10: "A"}, tabs: []extTab{
		{1, "chrome://newtab/", 10, true, false},
		{2, "about:blank", 10, true, false},
		{3, "about:blank", 10, false, false}, // the user's blank tab
	}}
	res = runCleanup(t, &TabGroupCleanup{Client: blank}, "A", true)
	if res.Status != CleanupClosed || res.Closed != 2 || fmt.Sprint(blank.remaining()) != "[3]" {
		t.Fatalf("blank group: result = %+v, remaining %v", res, blank.remaining())
	}
}

func TestCleanupNoGroupNotConnectedUnreadable(t *testing.T) {
	ext := twoSessions()
	if res := runCleanup(t, &TabGroupCleanup{Client: ext}, "C", false); res.Status != CleanupNoGroup {
		t.Fatalf("session without a group: %+v", res)
	}
	if len(ext.closedTabs()) != 0 {
		t.Fatal("closed tabs of another session")
	}
	off := &fakeExt{notConnected: true}
	if res := runCleanup(t, &TabGroupCleanup{Client: off}, "A", false); res.Status != CleanupNotConnected {
		t.Fatalf("not connected: %+v", res)
	}
	if res := runCleanup(t, &TabGroupCleanup{}, "A", false); res.Status != CleanupNotConnected {
		t.Fatalf("no client: %+v", res)
	}
	bad := &fakeExt{raw: json.RawMessage(`{"groups":[{"id":10}],"tabs":[{"url":"x"}]}`)}
	if res := runCleanup(t, &TabGroupCleanup{Client: bad}, "A", false); res.Status != CleanupKept || bad.callCount() != 1 {
		t.Fatalf("unreadable listing: %+v (%d calls)", res, bad.callCount())
	}
	garbage := &fakeExt{raw: json.RawMessage(`[1,2]`)}
	if res := runCleanup(t, &TabGroupCleanup{Client: garbage}, "A", false); res.Status != CleanupKept {
		t.Fatalf("garbage listing: %+v", res)
	}
}

func TestCleanupReusesOrChainsRunningCleanup(t *testing.T) {
	ext := twoSessions()
	ext.block = make(chan struct{})
	c := &TabGroupCleanup{Client: ext}
	empty := c.Start("A", true)
	if again := c.Start("A", true); again != empty {
		t.Fatal("a second empty-only cleanup should reuse the running one")
	}
	full := c.Start("A", false)
	if full == empty {
		t.Fatal("a full cleanup must not be covered by an empty-only one")
	}
	if again := c.Start("A", true); again != full {
		t.Fatal("an empty-only cleanup should reuse the running full one")
	}
	if again := c.Start("A", false); again != full {
		t.Fatal("a second full cleanup should reuse the running one")
	}
	close(ext.block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r1, _ := empty.Wait(ctx)
	r2, ok := full.Wait(ctx)
	if !ok {
		t.Fatal("full cleanup did not finish")
	}
	if r1.Status != CleanupKept || r2.Status != CleanupClosed || r2.Closed != 3 {
		t.Fatalf("empty-only = %+v, full = %+v", r1, r2)
	}
	if len(c.InFlight()) != 0 {
		t.Fatal("finished cleanups still listed as running")
	}
}

func TestCleanupCallTimeoutAndExitBound(t *testing.T) {
	ext := twoSessions()
	ext.block = make(chan struct{})
	ext.ignoreCtx = true // a stalled extension that never answers
	defer close(ext.block)
	c := &TabGroupCleanup{Client: ext, CallTimeout: 50 * time.Millisecond}
	start := time.Now()
	res := runCleanup(t, c, "A", false)
	if res.Status != CleanupNoGroup || time.Since(start) > 2*time.Second {
		t.Fatalf("stalled listing: %+v after %v", res, time.Since(start))
	}

	slow := &TabGroupCleanup{Client: ext} // default 5s per call
	start = time.Now()
	if slow.AtExit("A", true, 100*time.Millisecond) {
		t.Fatal("AtExit reported success with a stalled extension")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("AtExit took %v; exit must not hang", d)
	}
}

func TestAtExitClosesTheGroupAndAwaitsRunning(t *testing.T) {
	ext := twoSessions()
	c := &TabGroupCleanup{Client: ext}
	if !c.AtExit("A", true, time.Second) {
		t.Fatal("AtExit timed out")
	}
	if got := fmt.Sprint(ext.closedTabs()); got != "[3 2 1]" {
		t.Fatalf("exit closed %s, want session A's group [3 2 1]", got)
	}

	// Without closeCurrent nothing new starts, but a running cleanup is awaited.
	ext = twoSessions()
	ext.block = make(chan struct{})
	c = &TabGroupCleanup{Client: ext}
	op := c.Start("B", false)
	go func() { time.Sleep(20 * time.Millisecond); close(ext.block) }()
	if !c.AtExit("A", false, 2*time.Second) {
		t.Fatal("AtExit timed out")
	}
	select {
	case <-op.Done():
	default:
		t.Fatal("AtExit returned before the running cleanup finished")
	}
	if fmt.Sprint(ext.closedTabs()) != "[4]" {
		t.Fatalf("closed %v, want session B's [4] only", ext.closedTabs())
	}
}

func TestAtExitFullGroupWithSlowExtensionFitsTheBound(t *testing.T) {
	// Every command takes 300ms: closing the tabs one by one would take
	// 15s, so the cleanup must close the group in a single command.
	ext := &fakeExt{groupSession: map[int]string{10: "A"}, delay: 300 * time.Millisecond}
	for i := 1; i <= TabGroupCloseCap; i++ {
		ext.tabs = append(ext.tabs, extTab{i, "https://e.example/", 10, true, false})
	}
	c := &TabGroupCleanup{Client: ext}
	start := time.Now()
	if !c.AtExit("A", true, TabGroupExitTimeout) {
		t.Fatalf("AtExit did not finish within the exit bound (%v)", time.Since(start))
	}
	if d := time.Since(start); d > TabGroupExitTimeout+200*time.Millisecond {
		t.Fatalf("AtExit took %v", d)
	}
	if n := len(ext.closedTabs()); n != TabGroupCloseCap {
		t.Fatalf("closed %d tabs, want %d", n, TabGroupCloseCap)
	}

	// An extension slower than the bound: exit returns on time anyway.
	slow := &fakeExt{groupSession: map[int]string{10: "A"}, delay: time.Second, tabs: []extTab{{1, "https://e.example/", 10, true, false}}}
	c = &TabGroupCleanup{Client: slow}
	start = time.Now()
	if c.AtExit("A", true, TabGroupExitTimeout) {
		t.Fatal("AtExit reported success with an extension slower than the bound")
	}
	if d := time.Since(start); d > TabGroupExitTimeout+300*time.Millisecond {
		t.Fatalf("AtExit took %v; exit must not hang", d)
	}
}

func TestSessionClientTagsCommands(t *testing.T) {
	inner := &fakeClient{byMethod: map[string]json.RawMessage{chromehost.MethodNavigate: json.RawMessage(`{"tabId":1}`)}}
	id := "S1"
	c := &SessionClient{Inner: inner, SessionID: func() string { return id }}
	if c.Used("S1") {
		t.Fatal("used before any command")
	}
	if _, err := c.Call(context.Background(), chromehost.MethodNavigate, NavigateInput{URL: "https://e.example"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inner.params), `"sessionId":"S1"`) || !strings.Contains(string(inner.params), `"url":"https://e.example"`) {
		t.Fatalf("params = %s", inner.params)
	}
	if !c.Used("S1") || c.Used("S2") {
		t.Fatal("use not tracked per session")
	}
	// A caller-supplied sessionId is replaced by the session's own.
	if _, err := c.Call(context.Background(), chromehost.MethodTabsClose, json.RawMessage(`{"tabId":3,"sessionId":"other"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inner.params), `"sessionId":"S1"`) || strings.Contains(string(inner.params), "other") {
		t.Fatalf("params = %s", inner.params)
	}
	// A command that never reached Chrome does not count as use.
	id = "S2"
	inner.err = chromehost.ErrChromeNotConnected
	_, _ = c.Call(context.Background(), chromehost.MethodTabsContext, nil)
	if c.Used("S2") {
		t.Fatal("a not-connected call counted as use")
	}
	// No session id: the params pass through untouched.
	id = ""
	inner.err = nil
	_, _ = c.Call(context.Background(), chromehost.MethodTabsContext, nil)
	if inner.params != nil {
		t.Fatalf("params = %s, want none", inner.params)
	}
}

func TestNavigateListsTabsWithCleanupNote(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodNavigate:    json.RawMessage(`{"tabId":12,"url":"https://go.dev"}`),
		chromehost.MethodTabsContext: json.RawMessage(`{"tabs":[{"id":12,"url":"https://go.dev","title":"Go"}]}`),
	}}
	res, err := NavigateTool{Client: c, ListTabs: true}.Execute(context.Background(), NavigateInput{URL: "https://go.dev"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "[12] Go") || !strings.Contains(res.Text, TabCleanupNote("tabs_close")) {
		t.Fatalf("text = %q", res.Text)
	}
	tab := Num(12)
	res, err = NavigateTool{Client: c, ListTabs: true}.Execute(context.Background(), NavigateInput{URL: "https://go.dev", TabID: &tab})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "yours to clean up") {
		t.Fatalf("explicit tabId should not append the listing: %q", res.Text)
	}
}
