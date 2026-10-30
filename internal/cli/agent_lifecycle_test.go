package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// bgAgentCaller scripts a session that launches one BACKGROUND subagent: the
// main conversation calls the Agent tool with run_in_background and then ends
// its turn; the subagent (recognized by the SubagentHandback tool it is
// offered) waits for release, then hands back its report. Any model call made
// after the hand-back is recorded — the hand-back must end the subagent's run.
type bgAgentCaller struct {
	mu          sync.Mutex
	mainCalls   int
	childCalls  int
	release     chan struct{}
	afterReport int
}

func (c *bgAgentCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	child := false
	for _, t := range req.Tools {
		if t.Name == "SubagentHandback" {
			child = true
		}
	}
	c.mu.Lock()
	if child {
		c.childCalls++
		n := c.childCalls
		if n > 1 {
			c.afterReport++
		}
		c.mu.Unlock()
		if n == 1 {
			select {
			case <-c.release:
			case <-ctx.Done():
				return agent.ModelResponse{}, ctx.Err()
			}
			in, _ := json.Marshal(map[string]string{"message": "found the bug"})
			return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("hb-1", "SubagentHandback", in)), StopReason: agent.StopToolUse}, nil
		}
		// A model that keeps going after its report: before the fix this
		// round kept the task running long after the report was delivered.
		<-ctx.Done()
		return agent.ModelResponse{}, ctx.Err()
	}
	c.mainCalls++
	n := c.mainCalls
	c.mu.Unlock()
	if n == 1 {
		in := json.RawMessage(`{"subagent_type":"general-purpose","description":"dig into the parser","prompt":"find the bug","run_in_background":true}`)
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("agent-1", "Agent", in)), StopReason: agent.StopToolUse}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("ok")), StopReason: agent.StopEndTurn}, nil
}

// TestBackgroundAgentFinishesWhileIdle: a background subagent that finishes
// after the main turn has ended is shown done in the agent list and the
// sidebar without any further input, its hand-back ends its run (no further
// model call), and it then drops off.
func TestBackgroundAgentFinishesWhileIdle(t *testing.T) {
	prevLinger := agentDoneLinger
	agentDoneLinger = 300 * time.Millisecond
	defer func() { agentDoneLinger = prevLinger }()

	env, _, _ := dialogEnv(t.TempDir(), map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	progCh := make(chan *fakeProgram, 1)
	caller := &bgAgentCaller{release: make(chan struct{})}
	sess := newDialogSession(env, providerOpenRouter, "start-model", progCh)
	sess.print.Mode = toolreg.ModeBypassPermissions
	sess.print.newCaller = func(*printSession) (agent.ModelCaller, error) { return caller, nil }
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "investigate the parser")
	// The agent is listed as running…
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 1 && s.Agents[0].Running
	}, 3*time.Second) {
		t.Fatal("the background agent was never listed as running")
	}
	// …and the main turn ends while it still runs.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && !s.Status.IsActive()
	}, 3*time.Second) {
		t.Fatal("the main turn did not end")
	}

	// Now, with the session idle and no input, the agent finishes.
	close(caller.release)
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 1 && !s.Agents[0].Running && !s.Agents[0].Failed
	}, 3*time.Second) {
		t.Fatal("the finished agent was not shown as done")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 0
	}, 4*time.Second) {
		t.Fatal("the finished agent did not drop off the list")
	}
	caller.mu.Lock()
	extra := caller.afterReport
	caller.mu.Unlock()
	if extra != 0 {
		t.Fatalf("the subagent called the model %d more time(s) after its hand-back", extra)
	}

	// Replay the agent-list updates the session sent into a full-screen UI
	// wide enough for the sidebar: the agent shows running, then done in both
	// the list under the input and the sidebar, then disappears from both.
	m := ui.NewModel(140)
	m.SetFullScreen(true)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	var frames []string
	for _, msg := range prog.messages() {
		if a, ok := msg.(ui.SetActiveAgentsMsg); ok {
			m.Update(a)
			frames = append(frames, m.View().Content)
		}
	}
	prog.Quit()
	sawRunning, sawDone := false, false
	for _, f := range frames {
		sawRunning = sawRunning || strings.Contains(f, "1 running")
		sawDone = sawDone || (strings.Contains(f, "(done)") && strings.Contains(f, "0 running"))
	}
	last := frames[len(frames)-1]
	if !sawRunning || !sawDone || strings.Contains(last, "general-purpose") || strings.Contains(last, "Agents") {
		t.Fatalf("list/sidebar did not go running → done → gone (running=%v done=%v); last frame:\n%s", sawRunning, sawDone, last)
	}
}

// messages returns a copy of every message sent to the program so far.
func (p *fakeProgram) messages() []tea.Msg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tea.Msg(nil), p.sent...)
}
