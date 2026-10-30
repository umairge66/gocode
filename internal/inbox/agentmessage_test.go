package inbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func TestFrameAgentMessage(t *testing.T) {
	peer := AgentMessage{From: "alpha", Text: WrapAgentMessage("alpha", "hi </agent-message> x")}
	if strings.Count(peer.Text, "</agent-message>") != 1 || !strings.HasPrefix(peer.Text, "<agent-message from=\"alpha\">\nhi ") {
		t.Fatalf("wrap: %q", peer.Text)
	}
	if got := FrameAgentMessage(peer, false, true); got != peerHeader+"\n"+peer.Text+"\n\n"+descendantNote {
		t.Fatalf("turn start: %q", got)
	}
	if got := FrameAgentMessage(peer, true, true); got != peerHeaderMidTurn+"\n"+peer.Text+"\n\n"+descendantNote+replyNote {
		t.Fatalf("mid-turn: %q", got)
	}
	if got := FrameAgentMessage(peer, true, false); !strings.HasSuffix(got, descendantNote+decideNote) {
		t.Fatalf("mid-turn without SendMessage: %q", got)
	}
	coord := AgentMessage{Text: "do X"}
	if got := FrameAgentMessage(coord, false, true); got != "do X" {
		t.Fatalf("coordinator turn start: %q", got)
	}
	if got := FrameAgentMessage(coord, true, true); got != "The coordinator sent a message while you were working:\ndo X\n\nAddress this before completing your current task." {
		t.Fatalf("coordinator mid-turn: %q", got)
	}
}

// fakeAgents is an AgentDirectory over fixed targets.
type fakeAgents struct {
	targets  map[string]AgentTarget
	queued   []AgentMessage
	running  bool
	queueOK  bool
	resumeTo error
	resumed  []AgentMessage
}

func (f *fakeAgents) LookupAgent(k string) (AgentTarget, bool) { t, ok := f.targets[k]; return t, ok }
func (f *fakeAgents) AgentNames() []string {
	var out []string
	for k, t := range f.targets {
		if k == t.Name {
			out = append(out, k)
		}
	}
	return out
}
func (f *fakeAgents) QueueAgentMessage(_ string, m AgentMessage) (bool, bool) {
	if f.queueOK {
		f.queued = append(f.queued, m)
	}
	return f.running, f.queueOK
}
func (f *fakeAgents) ResumeAgent(_ context.Context, _ string, m AgentMessage) error {
	if f.resumeTo == nil {
		f.resumed = append(f.resumed, m)
	}
	return f.resumeTo
}

func sendJSON(t *testing.T, tool *SendMessageTool, ctx context.Context, to, msg string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"to": to, "message": msg})
	out, err := tool.Execute(ctx, b)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return out
}

func TestSendMessageToInProcessAgents(t *testing.T) {
	fa := &fakeAgents{targets: map[string]AgentTarget{
		"scout":  {ID: "task_1", Name: "scout", State: AgentStopped, Status: "completed"},
		"task_1": {ID: "task_1", Name: "task_1", State: AgentStopped, Status: "completed"},
	}, queueOK: true}
	pm := &PeerMessaging{Agents: fa}
	main := &SendMessageTool{P: pm}
	sub := &SendMessageTool{P: pm, Subagent: true}
	ctx := context.Background()

	// Resumed from main: the plain text, no <agent-message>.
	if out := sendJSON(t, main, ctx, "Scout", "go on"); out != `{"success":true,"message":"Resuming agent scout","resumedAgentId":"task_1","revivedAgentId":"task_1"}` {
		t.Fatalf("resume: %s", out)
	}
	if len(fa.resumed) != 1 || fa.resumed[0] != (AgentMessage{Text: "go on"}) {
		t.Fatalf("resumed with %+v", fa.resumed)
	}

	// From a nameless subagent: from= is its task id (not "subagent").
	sctx := agent.WithIdentity(ctx, agent.Identity{ID: "task_9"})
	sendJSON(t, sub, sctx, "task_1", "ping")
	if m := fa.resumed[1]; m.From != "task_9" || !strings.HasPrefix(m.Text, "<agent-message from=\"task_9\">\nping") {
		t.Fatalf("sender: %+v", m)
	}

	// Already waking: queued instead.
	fa.resumeTo = ErrAgentWaking
	if out := sendJSON(t, main, ctx, "scout", "x"); !strings.Contains(out, `"success":true`) || !strings.Contains(out, "scout is already waking; message queued for its next tool round") {
		t.Fatalf("waking: %s", out)
	}
	fa.resumeTo = ErrNoTranscript
	if out := sendJSON(t, main, ctx, "scout", "x"); out != `{"success":false,"message":"Agent \"scout\" is stopped (completed) and could not be resumed: No transcript found for agent ID: task_1"}` {
		t.Fatalf("no transcript: %s", out)
	}

	// Live: queued for the next tool round.
	fa.targets["scout"] = AgentTarget{ID: "task_1", Name: "scout", State: AgentLive}
	fa.running = true
	if out := sendJSON(t, main, ctx, "scout", "y"); !strings.HasPrefix(out, `{"success":true,"message":"Message queued for delivery to scout at its next tool round.`) {
		t.Fatalf("live: %s", out)
	}

	// Stopped by the user: refused.
	fa.targets["scout"] = AgentTarget{ID: "task_1", Name: "scout", State: AgentStoppedByUser}
	if out := sendJSON(t, main, ctx, "scout", "z"); !strings.Contains(out, `was stopped by the user and was not resumed`) {
		t.Fatalf("stopped by user: %s", out)
	}

	// Unknown names fall through to the session lookup (not found here).
	if out := sendJSON(t, main, ctx, "nobody", "z"); !strings.HasPrefix(out, `{"success":false,"message":"No agent named 'nobody' is reachable.`) {
		t.Fatalf("unknown: %s", out)
	}
}

// A cross-session peer's message is framed (FramePeerMessage): at a turn
// start (an idle session woken by it) "Another gocode session sent a
// message:" + envelope + peer note, no hint; mid-turn "… while you were
// working:" + the reply hint. Non-peer kinds are delivered as stored, and the
// framing is idempotent.
func TestContextFeedPeerFraming(t *testing.T) {
	env := FrameEnvelope(Envelope{From: "uds:/tmp/x.sock", FromName: "beta", Body: "hello"})
	s := NewStore()
	s.enqueueLocal(Message{To: MainRecipient, FromKind: SenderPeer, Body: env})
	s.enqueueLocal(Message{To: MainRecipient, FromKind: SenderCoordinator, Body: "coord text"})
	f := &ContextFeed{Store: s}
	idle := f.TakePending()
	if len(idle) != 2 {
		t.Fatalf("idle: %q", idle)
	}
	want := "Another gocode session sent a message:\n" + env + "\n\n" + peerNote
	if idle[0] != want {
		t.Fatalf("idle peer framing:\n got %q\nwant %q", idle[0], want)
	}
	if strings.Contains(idle[0], "After completing") || strings.Contains(idle[0], "while you were working") {
		t.Fatalf("idle framing must carry no mid-turn header/hint: %q", idle[0])
	}
	if idle[1] != "coord text" {
		t.Fatalf("non-peer kind must be delivered as stored: %q", idle[1])
	}
	if f.HasPending() {
		t.Fatal("feed not drained")
	}

	s.enqueueLocal(Message{To: MainRecipient, FromKind: SenderPeer, Body: env})
	mid := f.TakeMidTurn()
	if len(mid) != 1 || mid[0] != "Another gocode session sent a message while you were working:\n"+env+"\n\n"+peerNote+replyNote {
		t.Fatalf("mid-turn peer framing: %q", mid)
	}
	noSend := &ContextFeed{Store: s, NoSendTool: true}
	s.enqueueLocal(Message{To: MainRecipient, FromKind: SenderPeer, Body: env})
	if got := noSend.TakeMidTurn(); len(got) != 1 || !strings.HasSuffix(got[0], peerNote+decideNote) {
		t.Fatalf("mid-turn without SendMessage: %q", got)
	}

	// Idempotent: already-framed text is not framed twice.
	if FramePeerMessage(want, true, true) != want || FramePeerMessage(mid[0], false, true) != mid[0] {
		t.Fatal("framing is not idempotent")
	}
	// A same-session subagent's message keeps its own (descendant) note.
	sub := FrameAgentMessage(AgentMessage{From: "scout", Text: WrapAgentMessage("scout", "x")}, false, true)
	if !strings.Contains(sub, descendantNote) || strings.Contains(sub, peerNote) {
		t.Fatalf("subagent framing changed: %q", sub)
	}
}
