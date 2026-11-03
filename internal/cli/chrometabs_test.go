package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/chromehost"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// sessionChrome is a fake extension: tabs_context answers with the listing
// set for the session in the command, tabs_close succeeds.
type sessionChrome struct {
	mu       sync.Mutex
	listings map[string]string // session id -> tabs_context reply
	notConn  bool
	calls    []string
}

func (f *sessionChrome) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(params)
	if raw, ok := params.(json.RawMessage); ok {
		b = raw
	}
	f.calls = append(f.calls, method+" "+string(b))
	if f.notConn {
		return nil, chromehost.ErrChromeNotConnected
	}
	var p struct {
		SessionID string `json:"sessionId"`
		TabID     int    `json:"tabId"`
		TabIDs    []int  `json:"tabIds"`
	}
	_ = json.Unmarshal(b, &p)
	switch method {
	case chromehost.MethodTabsContext:
		if l, ok := f.listings[p.SessionID]; ok {
			return json.RawMessage(l), nil
		}
		return json.RawMessage(`{"groups":[],"tabs":[]}`), nil
	case chromehost.MethodTabsCreate:
		return json.RawMessage(`{"tabId":7}`), nil
	case chromehost.MethodTabsClose:
		if p.TabIDs != nil {
			out, _ := json.Marshal(map[string]any{"closed": p.TabIDs})
			return out, nil
		}
		return json.RawMessage(`{"closed":1}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func (f *sessionChrome) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// closes returns the tabs_close calls in order.
func (f *sessionChrome) closes() []string {
	var out []string
	for _, c := range f.log() {
		if strings.HasPrefix(c, chromehost.MethodTabsClose+" ") {
			out = append(out, c)
		}
	}
	return out
}

const contentGroup = `{"groups":[{"id":10}],"tabs":[{"id":1,"url":"https://a.example/","groupId":10,"openedByGocode":true},{"id":2,"url":"https://b.example/","groupId":10,"openedByGocode":true}]}`
const blankGroup = `{"groups":[{"id":10}],"tabs":[{"id":1,"url":"chrome://newtab/","groupId":10,"openedByGocode":true}]}`

// mixedGroup holds a tab gocode opened (1), a tab the user dragged in (2) and
// a tab marked keep_open (3).
const mixedGroup = `{"groups":[{"id":10}],"tabs":[` +
	`{"id":1,"url":"https://a.example/","groupId":10,"openedByGocode":true},` +
	`{"id":2,"url":"https://mail.example/","groupId":10,"openedByGocode":false},` +
	`{"id":3,"url":"https://report.example/","groupId":10,"openedByGocode":true,"keepOpen":true}]}`

// closedIDs returns the tab ids the tabs_close calls asked for, in order.
func (f *sessionChrome) closedIDs() string {
	var out []string
	for _, c := range f.closes() {
		var p struct {
			TabIDs []int `json:"tabIds"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(c, chromehost.MethodTabsClose+" ")), &p)
		for _, id := range p.TabIDs {
			out = append(out, strconv.Itoa(id))
		}
	}
	return strings.Join(out, ",")
}

func chromeRuntime(t *testing.T, fc *sessionChrome, session string) *sessionRuntime {
	t.Helper()
	r := &sessionRuntime{dispatcher: toolrun.New(toolreg.ModeBypassPermissions, nil, toolrun.WithBrowserClient(fc))}
	r.wireChromeTabs(nil, session, true)
	if r.chromeTabs == nil {
		t.Fatal("chrome tabs not wired")
	}
	return r
}

func openTab(t *testing.T, r *sessionRuntime) {
	t.Helper()
	res, err := r.dispatcher.Dispatch(context.Background(), agent.ToolUse{Name: string(protocol.ToolBrowserTabsCreate), Input: json.RawMessage(`{}`)})
	if err != nil || res.IsError {
		t.Fatalf("tabs_create: %v %+v", err, res)
	}
}

func waitOp(t *testing.T, op interface {
	Done() <-chan struct{}
}) {
	t.Helper()
	if op == nil {
		t.Fatal("no cleanup started")
	}
	select {
	case <-op.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish")
	}
}

func TestChromeTabsBrowserCommandsCarryTheSession(t *testing.T) {
	fc := &sessionChrome{}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	calls := fc.log()
	if len(calls) != 1 || !strings.Contains(calls[0], `"sessionId":"S1"`) {
		t.Fatalf("calls = %v", calls)
	}
	// Subagents share the session's client, so their commands carry it too.
	if r.dispatcher.BrowserClient != r.chromeTabs.client {
		t.Fatal("dispatcher does not use the session client")
	}
}

func TestChromeTabsExitNoOpWithoutBrowserUse(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": blankGroup}}
	r := chromeRuntime(t, fc, "S1")
	r.Close("prompt_input_exit")
	if calls := fc.log(); len(calls) != 0 {
		t.Fatalf("a session that never used the browser contacted Chrome: %v", calls)
	}
}

func TestChromeTabsExitClosesTheGroup(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	r.Close("prompt_input_exit")
	c := fc.closes()
	if len(c) != 1 || !strings.Contains(c[0], `"sessionId":"S1"`) || !strings.Contains(c[0], `"cleanup":true`) || fc.closedIDs() != "2,1" {
		t.Fatalf("closes = %v", c)
	}

	fc = &sessionChrome{listings: map[string]string{"S1": blankGroup}}
	r = chromeRuntime(t, fc, "S1")
	openTab(t, r)
	r.Close("other")
	if fc.closedIDs() != "1" {
		t.Fatalf("closes = %v", fc.closes())
	}
}

func TestChromeTabsExitKeepsUserAndKeptTabs(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": mixedGroup}}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	r.Close("other")
	if got := fc.closedIDs(); got != "1" {
		t.Fatalf("closed %q, want only the tab gocode opened (1); closes = %v", got, fc.closes())
	}
}

func TestChromeTabsOverCapNotice(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"groups":[{"id":10}],"tabs":[`)
	for i := 1; i <= 51; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"url":"https://e.example/%d","groupId":10,"openedByGocode":true}`, i, i)
	}
	b.WriteString("]}")
	big := b.String()

	// While the session runs the notice goes to the transcript.
	fc := &sessionChrome{listings: map[string]string{"S1": big}}
	r := chromeRuntime(t, fc, "S1")
	var shown []string
	var mu sync.Mutex
	r.setNotify(func(m string) { mu.Lock(); shown = append(shown, m); mu.Unlock() })
	openTab(t, r)
	waitOp(t, r.chromeTabs.cleared("S2"))
	mu.Lock()
	got := append([]string(nil), shown...)
	mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "51 tabs") || len(fc.closes()) != 0 {
		t.Fatalf("notices = %q, closes = %v", got, fc.closes())
	}

	// At exit it goes to stderr, once.
	var stderr bytes.Buffer
	fc = &sessionChrome{listings: map[string]string{"S1": big}}
	r = &sessionRuntime{dispatcher: toolrun.New(toolreg.ModeBypassPermissions, nil, toolrun.WithBrowserClient(fc))}
	r.wireChromeTabs(&Env{Stderr: &stderr, Getenv: func(string) string { return "" }}, "S1", false)
	openTab(t, r)
	r.Close("other")
	if n := strings.Count(stderr.String(), "51 tabs"); n != 1 || len(fc.closes()) != 0 {
		t.Fatalf("stderr = %q, closes = %v", stderr.String(), fc.closes())
	}
}

func TestChromeTabsSuccessIsSilent(t *testing.T) {
	var stderr bytes.Buffer
	fc := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r := &sessionRuntime{dispatcher: toolrun.New(toolreg.ModeBypassPermissions, nil, toolrun.WithBrowserClient(fc))}
	r.wireChromeTabs(&Env{Stderr: &stderr, Getenv: func(string) string { return "" }}, "S1", false)
	openTab(t, r)
	r.Close("other")
	if stderr.Len() != 0 || fc.closedIDs() != "2,1" {
		t.Fatalf("stderr = %q, closed %q", stderr.String(), fc.closedIDs())
	}

	stderr.Reset()
	fc = &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r = &sessionRuntime{dispatcher: toolrun.New(toolreg.ModeBypassPermissions, nil, toolrun.WithBrowserClient(fc))}
	r.wireChromeTabs(&Env{Stderr: &stderr, Getenv: func(k string) string {
		if k == "GOCODE_DEBUG" {
			return "1"
		}
		return ""
	}}, "S1", false)
	openTab(t, r)
	r.Close("other")
	if !strings.Contains(stderr.String(), "[tab group cleanup]") {
		t.Fatalf("GOCODE_DEBUG: stderr = %q", stderr.String())
	}
}

func TestChromeTabsExitNotConnectedDoesNotHang(t *testing.T) {
	fc := &sessionChrome{}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	fc.mu.Lock()
	fc.notConn = true
	fc.mu.Unlock()
	start := time.Now()
	r.Close("other")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("exit took %v", d)
	}
	if c := fc.closes(); len(c) != 0 {
		t.Fatalf("closes = %v", c)
	}
}

func TestChromeTabsClearClosesTheClearedSessionGroup(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	waitOp(t, r.chromeTabs.cleared("S2"))
	if c := fc.closes(); len(c) != 1 || fc.closedIDs() != "2,1" {
		t.Fatalf("closes = %v, want tab 2 then tab 1", c)
	}
	if r.chromeTabs.sessionID() != "S2" {
		t.Fatalf("current session = %q", r.chromeTabs.sessionID())
	}
	// The new session never used the browser: exit does nothing more.
	before := len(fc.log())
	r.Close("other")
	if len(fc.log()) != before {
		t.Fatalf("exit contacted Chrome for an unused session: %v", fc.log()[before:])
	}
}

func TestChromeTabsClearKeepsUserAndKeptTabs(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": mixedGroup}}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	waitOp(t, r.chromeTabs.cleared("S2"))
	if got := fc.closedIDs(); got != "1" {
		t.Fatalf("closed %q, want 1", got)
	}
}

func TestChromeTabsClearWithRunningAgentClosesOnlyAnEmptyGroup(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r := chromeRuntime(t, fc, "S1")
	r.chromeTabs.tasks = subagent.NewTaskRegistry()
	r.chromeTabs.tasks.Create(subagent.Task{Status: subagent.StatusRunning})
	openTab(t, r)
	waitOp(t, r.chromeTabs.cleared("S2"))
	if c := fc.closes(); len(c) != 0 {
		t.Fatalf("closed tabs while a subagent still runs: %v", c)
	}
}

func TestChromeTabsClearWithoutPersistenceClosesTheCurrentGroup(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	waitOp(t, r.chromeTabs.cleared("S1")) // the session id does not change
	if c := fc.closedIDs(); c != "2,1" {
		t.Fatalf("closes = %v", c)
	}
}

func TestChromeTabsSwitchClosesPreviousGroup(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r := chromeRuntime(t, fc, "S1")
	if op := r.chromeTabs.switched("S1"); op != nil {
		t.Fatal("no switch, yet a cleanup started")
	}
	if op := r.chromeTabs.switched("S0"); op != nil {
		t.Fatal("cleanup started for a session that never used the browser")
	}
	r.chromeTabs.switched("S1")
	openTab(t, r)
	waitOp(t, r.chromeTabs.switched("S2"))
	if c := fc.closes(); len(c) != 1 || !strings.Contains(c[0], `"sessionId":"S1"`) || fc.closedIDs() != "2,1" {
		t.Fatalf("closes = %v", c)
	}

	fc = &sessionChrome{listings: map[string]string{"S1": mixedGroup}}
	r = chromeRuntime(t, fc, "S1")
	openTab(t, r)
	waitOp(t, r.chromeTabs.switched("S2"))
	if got := fc.closedIDs(); got != "1" {
		t.Fatalf("switch closed %q, want 1", got)
	}
}

func TestChromeTabsSwitchWithRunningAgentClosesOnlyAnEmptyGroup(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r := chromeRuntime(t, fc, "S1")
	r.chromeTabs.tasks = subagent.NewTaskRegistry()
	r.chromeTabs.tasks.Create(subagent.Task{Status: subagent.StatusRunning})
	openTab(t, r)
	waitOp(t, r.chromeTabs.switched("S2"))
	if c := fc.closes(); len(c) != 0 {
		t.Fatalf("closed tabs while a subagent still runs: %v", c)
	}
}

func TestChromeTabsNilSafe(t *testing.T) {
	var ct *chromeTabs
	ct.atExit()
	if ct.switched("x") != nil || ct.cleared("x") != nil {
		t.Fatal("nil chromeTabs started a cleanup")
	}
	r := &sessionRuntime{}
	r.wireChromeTabs(nil, "S1", true)
	if r.chromeTabs != nil {
		t.Fatal("wired without a dispatcher")
	}
}

// /clear starts exactly one cleanup of the cleared session. It used to start
// two (one through switched, one of its own); when the first had already
// finished, the second closed the same tabs again, which is how
// TestChromeTabsClearKeepsUserAndKeptTabs closed tab 1 twice on slow machines.
func TestChromeTabsClearStartsOneCleanup(t *testing.T) {
	fc := &sessionChrome{listings: map[string]string{"S1": mixedGroup}}
	r := chromeRuntime(t, fc, "S1")
	openTab(t, r)
	waitOp(t, r.chromeTabs.cleared("S2"))
	if n := r.chromeTabs.started.Load(); n != 1 {
		t.Fatalf("/clear started %d cleanups of the cleared session, want 1", n)
	}
	// Without persistence the session id stays the same: still one cleanup.
	fc2 := &sessionChrome{listings: map[string]string{"S1": contentGroup}}
	r2 := chromeRuntime(t, fc2, "S1")
	openTab(t, r2)
	waitOp(t, r2.chromeTabs.cleared("S1"))
	if n := r2.chromeTabs.started.Load(); n != 1 {
		t.Fatalf("/clear without a new session started %d cleanups, want 1", n)
	}
}
