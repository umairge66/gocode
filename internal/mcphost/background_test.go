package mcphost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// backgroundExcept connects every server in the background except those named.
func backgroundExcept(names ...string) func(string, mcp.ServerConfig) bool {
	return func(name string, cfg mcp.ServerConfig) bool {
		for _, n := range names {
			if n == name {
				return false
			}
		}
		return !cfg.AlwaysLoad
	}
}

func TestHostBackgroundConnect(t *testing.T) {
	fast, slow, pinned := echoServer(), echoServer(), echoServer()
	gate := make(chan struct{})
	slow.initGate = gate
	h := Start(context.Background(), map[string]mcp.ServerConfig{
		"fast":   {Command: "x"},
		"slow":   {Command: "x"},
		"pinned": {Command: "x", AlwaysLoad: true},
	}, Options{
		Transport:  multiFactory(map[string]*fakeServer{"fast": fast, "slow": slow, "pinned": pinned}),
		Getenv:     noEnv,
		Background: backgroundExcept(),
	})
	defer h.Close()

	// Start did not wait for the gated server, but did for the alwaysLoad
	// one.
	if !h.Has("mcp__pinned__echo") {
		t.Fatal("foreground server not connected when Start returned")
	}
	if h.Has("mcp__slow__echo") {
		t.Fatal("gated background server already connected")
	}
	if !h.Connecting() {
		t.Fatal("Connecting() = false with a background connect in flight")
	}
	states := map[string]State{}
	for _, st := range h.Status() {
		states[st.Name] = st.State
		if st.Name == "slow" && StateLabel(st) != "connecting…" {
			t.Errorf("slow label = %q", StateLabel(st))
		}
	}
	if states["slow"] != StatePending || states["pinned"] != StateConnected {
		t.Fatalf("states = %v", states)
	}

	var mu sync.Mutex
	var settled []ServerStatus
	h.OnBackgroundSettled(func(st ServerStatus) {
		mu.Lock()
		settled = append(settled, st)
		mu.Unlock()
	})

	waited := make(chan bool, 1)
	go func() { waited <- h.WaitConnecting(context.Background()) }()
	select {
	case <-waited:
		t.Fatal("WaitConnecting returned before the server connected")
	case <-time.After(30 * time.Millisecond):
	}
	close(gate)
	select {
	case ok := <-waited:
		if !ok {
			t.Fatal("WaitConnecting reported nothing to wait for")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitConnecting did not return")
	}
	if !h.Has("mcp__slow__echo") || h.Connecting() {
		t.Fatalf("after wait: tools = %+v, connecting = %v", h.Tools(), h.Connecting())
	}
	if h.WaitConnecting(context.Background()) {
		t.Error("WaitConnecting with nothing in flight should report false")
	}
	mu.Lock()
	defer mu.Unlock()
	// "fast" may have settled before the listener was set (replayed) or
	// after; both are reported exactly once.
	got := map[string]State{}
	for _, st := range settled {
		if _, dup := got[st.Name]; dup {
			t.Errorf("%s reported twice", st.Name)
		}
		got[st.Name] = st.State
	}
	if len(got) != 2 || got["fast"] != StateConnected || got["slow"] != StateConnected {
		t.Fatalf("settled = %+v", settled)
	}
}

// A background server that fails before anyone listens is still reported,
// and a hung one bounds WaitConnecting by the connect timeout.
func TestHostBackgroundFailureAndTimeout(t *testing.T) {
	bad := &fakeServer{startErr: errors.New("boom")}
	hung := &fakeServer{hangInit: true}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"bad": {}, "hung": {}}, Options{
		Transport:      multiFactory(map[string]*fakeServer{"bad": bad, "hung": hung}),
		Getenv:         noEnv,
		ConnectTimeout: 150 * time.Millisecond,
		Background:     backgroundExcept(),
	})
	defer h.Close()

	start := time.Now()
	if !h.WaitConnecting(context.Background()) {
		t.Fatal("nothing to wait for")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("WaitConnecting took %v", d)
	}
	reports := make(chan ServerStatus, 4)
	h.OnBackgroundSettled(func(st ServerStatus) { reports <- st })
	got := map[string]ServerStatus{}
	for len(got) < 2 {
		select {
		case st := <-reports:
			got[st.Name] = st
		case <-time.After(3 * time.Second):
			t.Fatalf("reports = %+v", got)
		}
	}
	if got["bad"].State != StateFailed || got["bad"].Error == "" {
		t.Errorf("bad = %+v", got["bad"])
	}
	if got["hung"].State != StateFailed {
		t.Errorf("hung = %+v", got["hung"])
	}
}

// Without Options.Background, Start connects everything in the foreground,
// and Connect / AddServers always wait.
func TestHostForegroundByDefault(t *testing.T) {
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {}}, Options{Transport: multiFactory(map[string]*fakeServer{"a": echoServer(), "b": echoServer()}), Getenv: noEnv})
	defer h.Close()
	if !h.Has("mcp__a__echo") || h.Connecting() {
		t.Fatal("foreground connect")
	}
	h.opts.Background = backgroundExcept()
	h.AddServers(context.Background(), map[string]mcp.ServerConfig{"b": {}})
	if !h.Has("mcp__b__echo") {
		t.Fatal("AddServers must wait for the new servers")
	}
}
