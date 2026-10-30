//go:build !windows

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
)

// peerCaptureCaller records the last model request.
type peerCaptureCaller struct {
	mu   sync.Mutex
	last agent.ModelRequest
}

func (c *peerCaptureCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.last = req
	c.mu.Unlock()
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("ok")), StopReason: agent.StopEndTurn}, nil
}

func peerSessionEnv(t *testing.T, cfgDir string, vars map[string]string) *Env {
	t.Helper()
	cwd := t.TempDir()
	vars["GOCODE_CONFIG_DIR"] = cfgDir
	return &Env{
		Getenv: func(k string) string { return vars[k] },
		Setenv: func(string, string) {},
		Stdout: &strings.Builder{},
		Stderr: &strings.Builder{},
		Cwd:    func() (string, error) { return cwd, nil },
	}
}

func schemaNames(rt *sessionRuntime) map[string]bool {
	out := map[string]bool{}
	for _, s := range rt.dispatcher.Schemas() {
		out[s.Name] = true
	}
	return out
}

// TestPeerMessagingSessionWiring builds two sessions through buildSession: the
// receiving session binds its inbox and registers itself; a second (bare)
// session finds it by name and sends; the message wakes the receiver and
// reaches its model framed as a cross-session message. Subagents get
// SendMessage but not ListAgents. Close removes the record and the socket.
func TestPeerMessagingSessionWiring(t *testing.T) {
	cfgDir := t.TempDir()
	capA := &peerCaptureCaller{}
	sA := &printSession{Mode: toolreg.ModeDefault}
	rtA := buildSession(context.Background(), peerSessionEnv(t, cfgDir, map[string]string{"GOCODE_CODE_SESSION_NAME": "alpha"}), sA, capA, sessionConfig{interactive: true})
	defer rtA.Close("other")
	woke := make(chan struct{}, 4)
	rtA.setResume(func() { woke <- struct{}{} })

	names := schemaNames(rtA)
	if !names["SendMessage"] || !names["ListAgents"] {
		t.Fatalf("main tools: %v", names)
	}
	if rtA.peer == nil || rtA.peer.listener == nil {
		t.Fatal("inbox not bound")
	}
	sock := rtA.peer.listener.Path()
	recPath := filepath.Join(cfgDir, "sessions", strconv.Itoa(os.Getpid())+".json")
	data, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("registry record: %v", err)
	}
	var rec map[string]any
	_ = json.Unmarshal(data, &rec)
	if rec["name"] != "alpha" || rec["messagingSocketPath"] != sock || rec["nameSource"] != "user" {
		t.Fatalf("record: %v", rec)
	}

	// Subagents: SendMessage yes, ListAgents no.
	child := rtA.childDispatcher(rtA.agentTool, 1, []string{"*"})
	cn := map[string]bool{}
	for _, s := range child.Schemas() {
		cn[s.Name] = true
	}
	if !cn["SendMessage"] || cn["ListAgents"] {
		t.Fatalf("subagent tools: %v", cn)
	}

	// A bare session (no inbox of its own) discovers alpha and sends to it.
	sB := &printSession{Mode: toolreg.ModeDefault}
	rtB := buildSession(context.Background(), peerSessionEnv(t, cfgDir, map[string]string{"GOCODE_CODE_SIMPLE": "1"}), sB, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rtB.Close("other")
	if rtB.peer.listener != nil {
		t.Fatal("a bare session must not bind an inbox")
	}
	listRes, _ := rtB.dispatcher.Dispatch(context.Background(), agent.ToolUse{ID: "1", Name: "ListAgents", Input: json.RawMessage(`{}`)})
	if listRes.IsError || !strings.Contains(listRes.Content, "Peer sessions (1):\n  alpha [") {
		t.Fatalf("B listing: %+v", listRes)
	}
	res, _ := rtB.dispatcher.Dispatch(context.Background(), agent.ToolUse{ID: "2", Name: "SendMessage", Input: json.RawMessage(`{"to":"alpha","message":"hello from beta"}`)})
	if res.IsError || !strings.Contains(res.Content, `"success":true`) || !strings.Contains(res.Content, "with no inbox bound here") {
		t.Fatalf("B send: %+v", res)
	}
	select {
	case <-woke:
	case <-time.After(3 * time.Second):
		t.Fatal("receiving session was not woken")
	}
	if _, ran, err := rtA.conv.ResumeWithQueuedContext(context.Background()); err != nil || !ran {
		t.Fatalf("resume: ran=%v err=%v", ran, err)
	}
	capA.mu.Lock()
	msgs := capA.last.Messages
	capA.mu.Unlock()
	last := msgs[len(msgs)-1]
	text := ""
	for _, b := range last.Content {
		text += b.Text
	}
	// Idle delivery: the turn-start peer framing around the
	// <cross-session-message> envelope.
	if !strings.HasPrefix(text, "Another gocode session sent a message:\n<cross-session-message") || !strings.Contains(text, "hello from beta\n</cross-session-message>\n\nThis came from another gocode session") || strings.Contains(text, "After completing") {
		t.Fatalf("model saw %q", text)
	}

	rtA.Close("other")
	if _, err := os.Stat(recPath); !os.IsNotExist(err) {
		t.Fatal("registry record not removed at exit")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("inbox socket not removed at exit")
	}
}

// With GOCODE_CODE_HARBOR_KITE=0 no inbox is bound and ListAgents is not
// offered; SendMessage stays (for "main" from subagents) without the
// cross-session section.
func TestPeerMessagingKillSwitch(t *testing.T) {
	rt := buildSession(context.Background(), peerSessionEnv(t, t.TempDir(), map[string]string{"GOCODE_CODE_HARBOR_KITE": "0"}), &printSession{Mode: toolreg.ModeDefault}, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")
	names := schemaNames(rt)
	if !names["SendMessage"] || names["ListAgents"] {
		t.Fatalf("tools: %v", names)
	}
	if rt.peer.listener != nil {
		t.Fatal("inbox bound despite the kill switch")
	}
	for _, s := range rt.dispatcher.Schemas() {
		if s.Name == "SendMessage" && strings.Contains(s.Description, "## Cross-session") {
			t.Fatal("cross-session section advertised while messaging is off")
		}
	}
}
